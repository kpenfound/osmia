package service

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/bundle"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/pulls"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// resolutionPulls fails only the sink member's own PR lookup. Upstream edge
// observation keeps its existing provider behavior.
type resolutionPulls struct {
	*fakePulls
	fail atomic.Bool
	sink atomic.Pointer[string]
}

func (p *resolutionPulls) Find(ctx context.Context, repository, head, branch string) ([]pulls.PullRequest, error) {
	sink := p.sink.Load()
	if p.fail.Load() && sink != nil && *sink == branch {
		p.Mu.Lock()
		p.Finds++
		p.Mu.Unlock()
		return nil, errors.New("provider temporarily unavailable")
	}
	return p.fakePulls.Find(ctx, repository, head, branch)
}

func resolutionApprovedFixture(t *testing.T, configuration productionDesignConfiguration, edge string, host pulls.Client, prepare ...func(*productionDesignFixture)) *productionDesignFixture {
	t.Helper()
	if host == nil {
		host = &fakePulls{}
	}
	options := configuration.options
	configuration.options = func(opts *Options) {
		if options != nil {
			options(opts)
		}
		opts.PullRequests = host
	}
	f := newConfiguredProductionDesignFixture(t, configuration)
	for _, setup := range prepare {
		setup(f)
	}
	f.engine.Mu.Lock()
	f.engine.Turns["project-spec-draft-1"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		graph := []map[string]any{}
		if edge != "" {
			graph = append(graph, map[string]any{"from": string(f.stream1), "to": string(f.stream2), "type": edge})
		}
		_, err := callTool(ctx, tools, ProjectSpecDraftTool, map[string]any{
			"behavior":  "The server stores chunks with an acknowledgment; the client resumes from that offset.",
			"contracts": []map[string]any{{"name": "acknowledgment", "text": "Stored chunks are acknowledged with their offset."}},
			"graph":     graph,
			"slices":    []map[string]any{{"workstream": string(f.stream1), "contract_criteria": []int{2}}, {"workstream": string(f.stream2), "contract_criteria": []int{1}}},
		})
		return questionResult(req, "session-project-spec", "Project spec delivered."), err
	}
	if edge == "" {
		for i := 1; i <= 2; i++ {
			f.engine.Turns[roundTurnID(1, committeeAgent(i), 1)] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
				content, err := readTool(ctx, tools, "context.md")
				if err != nil {
					return nil, err
				}
				if !strings.Contains(content, "Draft project spec") || !strings.Contains(content, "acknowledgment") {
					return nil, errors.New("committee did not receive the shared draft and contract without merge edges")
				}
				return questionResult(req, "session-committee-"+req.Name, "The proposed contract and slice fit the charter."), nil
			}
		}
	}
	f.engine.Mu.Unlock()
	f.startOwner(t)
	packet := f.awaitPacket(t)
	_, err := f.c.Ratify(context.Background(), f.origin, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan, packet.ProjectSpecRevision)
	must(t, err)
	f.awaitApproved(t)
	return f
}

func resolutionMembers(f *productionDesignFixture) []struct {
	project config.ProjectID
	stream  config.WorkstreamID
} {
	return []struct {
		project config.ProjectID
		stream  config.WorkstreamID
	}{{f.project, f.stream1}, {f.project2, f.stream2}}
}

func deliverResolutionMembers(t *testing.T, f *productionDesignFixture) []DeliveryPublication {
	t.Helper()
	var out []DeliveryPublication
	for i, m := range resolutionMembers(f) {
		repo, err := f.s.repository(m.project)
		must(t, err)
		lifecycleAwait(t, f, "member build recorded before delivery", func() bool {
			transitions, err := trace.Read[trace.Transition](repo, m.stream)
			must(t, err)
			var state string
			for _, transition := range transitions {
				if transition.Subject == trace.FeatureSubject {
					state = transition.To
				}
			}
			return state == BuildingState
		})
		cfg := f.s.current().For(m.project)
		p := DeliveryPublication{Status: publicationOpened, Upstream: cfg.Project.Upstream, Fork: cfg.Project.PushRepository(), Base: "main", Branch: featureBranch(m.stream), Commit: "feature-tip", PullRequest: 101 + i, URL: fmt.Sprintf("https://example.invalid/pull/%d", 101+i)}
		resolutionDocument(t, repo, m.stream, publicationDocument, publicationPath, p)
		h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "delivered-for-resolution", Revision: 1, Project: m.project, Workstream: m.stream, At: f.clock.Now(), Actor: ownerActor, Cause: "fixture"}
		_, err = repo.SetFeatureState(context.Background(), h, DeliveredState, "Owner-approved ready PR published")
		must(t, err)
		out = append(out, p)
	}
	return out
}

func resolutionGroupState(t *testing.T, f *productionDesignFixture, wantResolved bool) {
	t.Helper()
	ctx := context.Background()
	repo, err := f.s.repository(f.project)
	must(t, err)
	id, err := repo.CrossRepoWorkgroupOf(f.stream1)
	must(t, err)
	authority, err := trace.OpenWorkgroupAuthority(f.s.current().Root, id)
	must(t, err)
	defer authority.Close()
	ids, revision, err := trace.ResolvedWorkgroups(authority)
	must(t, err)
	active, err := trace.ProjectSpecActive(authority, id)
	must(t, err)
	if wantResolved {
		if !slices.Equal(ids, []config.WorkgroupID{id}) || revision != 1 || active {
			t.Fatalf("resolution=%v revision=%d active=%v", ids, revision, active)
		}
	} else if len(ids) != 0 || revision != 0 || !active {
		t.Fatalf("premature resolution=%v revision=%d active=%v", ids, revision, active)
	}
	for _, m := range resolutionMembers(f) {
		if _, archived := f.s.archivedWorkstream(string(m.stream)); archived {
			continue
		}
		// Both member roles and the one shared chief keep the project contract.
		for _, role := range []string{masonRole, trace.ChiefOfStaff} {
			b, err := f.s.Context().Assemble(ctx, m.project, bundle.Scope{Role: role, Workstream: m.stream})
			must(t, err)
			if (b.ProjectSpec != nil) == wantResolved {
				t.Fatalf("member %s role %s project spec presence=%v", m.stream, role, b.ProjectSpec != nil)
			}
		}
		r, err := f.s.repository(m.project)
		must(t, err)
		docs, err := trace.Read[trace.Document](r, m.stream)
		must(t, err)
		if len(docs) == 0 {
			t.Fatal("member history disappeared")
		}
	}
}

func awaitMemberMergeResolution(t *testing.T, f *productionDesignFixture) {
	t.Helper()
	lifecycleAwait(t, f, "group merge resolution", func() bool {
		repo, err := f.s.repository(f.project)
		must(t, err)
		id, err := repo.CrossRepoWorkgroupOf(f.stream1)
		must(t, err)
		authority, err := trace.OpenWorkgroupAuthority(f.s.current().Root, id)
		must(t, err)
		defer authority.Close()
		_, revision, err := trace.ResolvedWorkgroups(authority)
		must(t, err)
		return revision == 1
	})
	resolutionGroupState(t, f, true)
}

func TestDesignProductionProjectSpecWaitsForEveryMemberMerge(t *testing.T) {
	f := resolutionApprovedFixture(t, productionDesignConfiguration{wait: lifecycleAwait}, "contract", nil)
	defer f.stop(t)
	deliverResolutionMembers(t, f)
	resolutionGroupState(t, f, false)
	repo, err := f.s.repository(f.project)
	must(t, err)
	recordResolutionMerge(t, repo, f.stream1)
	resolutionGroupState(t, f, false)
	f.stop(t)
	f.start(t)
	resolutionGroupState(t, f, false)
	repo, err = f.s.repository(f.project2)
	must(t, err)
	recordResolutionMerge(t, repo, f.stream2)
	awaitMemberMergeResolution(t, f)
	f.stop(t)
	f.start(t)
	resolutionGroupState(t, f, true)
}

func TestDesignProductionObservesAllOwnPullRequestsAcrossRestart(t *testing.T) {
	for _, edge := range []string{trace.EdgeHardBlock, trace.EdgeContract, ""} {
		t.Run("edge-"+edge, func(t *testing.T) {
			host := &resolutionPulls{fakePulls: &fakePulls{}}
			host.fail.Store(true)
			var hold atomic.Bool
			reached := make(chan struct{}, 1)
			f := resolutionApprovedFixture(t, productionDesignConfiguration{wait: lifecycleAwait, checkpoint: func(ctx context.Context, step string) error {
				if step == "workgroup-resolution-ready" && hold.Load() {
					select {
					case reached <- struct{}{}:
					default:
					}
					<-ctx.Done()
					return ctx.Err()
				}
				return nil
			}}, edge, host)
			defer func() { hold.Store(false); f.stop(t) }()
			sink := featureBranch(f.stream2)
			host.sink.Store(&sink)
			publications := deliverResolutionMembers(t, f)
			awaitChecks := func(phase string, minimum int) {
				lifecycleAwait(t, f, phase, func() bool {
					for _, m := range resolutionMembers(f) {
						repo, err := f.s.repository(m.project)
						must(t, err)
						transitions, err := trace.Read[trace.Transition](repo, m.stream)
						must(t, err)
						n := 0
						for _, tr := range transitions {
							if tr.Subject == baseRefreshAction && tr.To == "checked" {
								n++
							}
						}
						if n < minimum {
							return false
						}
					}
					return true
				})
			}
			assertNoOutcomes := func() {
				for _, m := range resolutionMembers(f) {
					repo, err := f.s.repository(m.project)
					must(t, err)
					_, rev, err := ownPullRequestOutcomeAt(repo, m.stream)
					must(t, err)
					if rev != 0 {
						t.Fatal("open or failed lookup fabricated an outcome")
					}
				}
				resolutionGroupState(t, f, false)
			}
			awaitChecks("failed provider lookups", 1)
			assertNoOutcomes()
			host.Mu.Lock()
			host.fail.Store(false)
			for _, p := range publications {
				host.Prs = append(host.Prs, pulls.PullRequest{Number: p.PullRequest, State: "open", Head: p.Branch, HeadRepository: p.Fork})
			}
			host.Mu.Unlock()
			f.clock.Advance(10 * time.Minute)
			awaitChecks("ready open provider lookups", 2)
			assertNoOutcomes()
			// Hold final resolution after every durable observation, then perform a real restart.
			hold.Store(true)
			host.Mu.Lock()
			for i := range host.Prs {
				host.Prs[i].Merged = true
				host.Prs[i].State = "closed"
				host.Prs[i].MergeCommit = fmt.Sprintf("merged-%d", i)
			}
			host.Mu.Unlock()
			f.clock.Advance(10 * time.Minute)
			select {
			case <-reached:
			case <-time.After(demoTimeout):
				f.timeout(t, "durable outcomes before resolution")
			}
			for i, m := range resolutionMembers(f) {
				repo, err := f.s.repository(m.project)
				must(t, err)
				outcome, rev, err := ownPullRequestOutcomeAt(repo, m.stream)
				must(t, err)
				if rev != 1 || outcome.PullRequest != publications[i].PullRequest || outcome.Outcome != "merged" || outcome.MergeCommit != fmt.Sprintf("merged-%d", i) {
					t.Fatalf("observed evidence=%+v revision=%d", outcome, rev)
				}
			}
			resolutionGroupState(t, f, false)
			f.stop(t)
			hold.Store(false)
			host.Mu.Lock()
			calls := host.Finds
			host.Mu.Unlock()
			f.start(t)
			awaitMemberMergeResolution(t, f)
			host.Mu.Lock()
			after := host.Finds
			host.Mu.Unlock()
			if after != calls {
				t.Fatalf("finished own PRs polled after restart: %d -> %d", calls, after)
			}
			f.stop(t)
			f.start(t)
			resolutionGroupState(t, f, true)
		})
	}
}
