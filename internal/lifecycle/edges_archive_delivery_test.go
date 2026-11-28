package lifecycle_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/bundle"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/followup"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/pulls"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const archivedUpstreamMerged = "aabbccddeeff0011223344556677889900aabbccdd"

type archivedUpstream struct {
	f         *journey.ProductionDesignFixture
	host      *fixturekit.FakePulls
	cleanup   *journey.LifecycleCleanupGate
	fixture   atomic.Pointer[journey.ProductionDesignFixture]
	interrupt atomic.Bool
	observed  atomic.Bool
	passes    atomic.Int64
	upstream  api.DeliveryPublication
}

func newArchivedUpstream(t *testing.T) *archivedUpstream {
	t.Helper()
	a := &archivedUpstream{
		host:    &fixturekit.FakePulls{SyncHead: true},
		cleanup: &journey.LifecycleCleanupGate{Reached: make(chan struct{})},
	}
	t.Cleanup(a.cleanup.Release)
	a.interrupt.Store(true)
	hook := fixturekit.NewWebhook(t)
	configuration := journey.ProductionDesignConfiguration{
		Wait:                 journey.LifecycleAwait,
		OpenPullRequestsStay: true,
		Checkpoint: func(ctx context.Context, step string) error {
			if step == "edge-merge-observed" && a.interrupt.Load() {
				a.observed.Store(true)
				<-ctx.Done()
				return ctx.Err()
			}
			if step == "final-review-governed" && !a.interrupt.Load() {
				if err := awaitDependentEdgeMerged(ctx, a.fixture.Load()); err != nil {
					return err
				}
			}
			return a.cleanup.Checkpoint(ctx, step)
		},
		Options: func(opts *journey.Options) {
			path := filepath.Join(opts.Config.Root, "config.toml")
			data, err := os.ReadFile(path)
			fixturekit.Must(t, err)
			data = []byte(strings.Replace(string(data), "[capacity]\n", "[capacity]\nmasons = 2\n", 1) + "[roles.mason]\nsandbox = \"container\"\nimage = \"fixture-image\"\n[roles.reviewer]\nsandbox = \"container\"\nimage = \"fixture-image\"\n")
			fixturekit.Must(t, os.WriteFile(path, data, 0600))
			fixturekit.SetWebhook(t, opts.Config.Root, hook.Hook())
			hook.Records(opts.Config.Root)
			opts.SchedulePassed = func() { a.passes.Add(1) }
			opts.ReviewChecks = fixturekit.CheckFunc(func(_ context.Context, directory string) (api.CheckResult, error) {
				dependency, err := os.ReadFile(filepath.Join(directory, "internal/trace/dependency.txt"))
				if os.IsNotExist(err) {
					return api.CheckResult{Output: "initial checks pass"}, nil
				}
				if err != nil || strings.TrimSpace(string(dependency)) != archivedUpstreamMerged {
					return api.CheckResult{}, fmt.Errorf("wrong dependency: %q, %v", dependency, err)
				}
				return api.CheckResult{Output: "unit checks pass"}, nil
			})
		},
	}
	a.f = resolutionApprovedFixture(t, configuration, trace.EdgeMergeOrder, a.host, a.scriptTurns(t))
	up, err := a.f.S.Repository(a.f.Project)
	fixturekit.Must(t, err)
	a.upstream = api.DeliveryPublication{Status: delivery.PublicationOpened, Upstream: a.f.Repos[0].Upstream, Fork: a.f.S.Current().For(a.f.Project).Project.PushRepository(), Branch: intake.FeatureBranch(a.f.Stream1), Commit: "upstream-feature", PullRequest: 71, URL: "https://example.invalid/pull/71"}
	journey.RecordUpstreamOwnPullRequest(t, a.f.S, up, a.f.Stream1, a.upstream)
	a.host.Mu.Lock()
	a.host.Prs = []pulls.PullRequest{{Number: a.upstream.PullRequest, State: "open", HeadRepository: a.upstream.Fork, Head: a.upstream.Branch, URL: a.upstream.URL}}
	a.host.Mu.Unlock()
	return a
}

func (a *archivedUpstream) scriptTurns(t *testing.T) func(*journey.ProductionDesignFixture) {
	return func(f *journey.ProductionDesignFixture) {
		a.fixture.Store(f)
		clone := f.Repos[1].Clone
		home := filepath.Dir(clone)
		a.host.Fork = func() string {
			return strings.TrimSpace(fixturekit.DemoGit(t, home, "-C", f.Repos[1].Remote, "for-each-ref", "--format=%(objectname)", "refs/heads/"+intake.FeatureBranch(f.Stream2)))
		}
		fixturekit.ConfigureDeliverySigner(t, clone)
		f.Engine.Mu.Lock()
		manager := f.Engine.Turns["*"]
		f.Engine.Turns["*"] = func(ctx context.Context, req agent.Request, turn *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
			var err error
			switch {
			case strings.HasPrefix(req.Name, "mason-") && strings.HasSuffix(req.Name, "-implement"):
				path, content := "internal/trace/built.go", "package trace\n"
				if strings.Contains(req.Name, "dependency-update-") {
					if !strings.Contains(req.Prompt, archivedUpstreamMerged) {
						return nil, fmt.Errorf("update lacks merged dependency context: %s", req.Prompt)
					}
					path, content = "internal/trace/dependency.txt", archivedUpstreamMerged+"\n"
				} else if strings.Contains(req.Name, "final-") {
					path = "internal/trace/repaired.go"
				}
				if err = os.WriteFile(filepath.Join(req.Workspace.Directory(), path), []byte(content), 0600); err == nil {
					var recorded bool
					var reason string
					recorded, reason, err = intakefixture.Done(ctx, tools, map[string]any{"outcome": "Implement the slice"})
					if err == nil && !recorded {
						err = fmt.Errorf("done refused: %s", reason)
					}
				}
			case strings.HasPrefix(req.Name, "reviewer-") || strings.HasPrefix(req.Name, "reviewer_"):
				_, err = fixturekit.CallTool(ctx, tools, execution.VerdictTool, map[string]any{"decision": "satisfactory", "summary": fixturekit.ReviewSummary, "findings": []api.ReviewFinding{}})
			case strings.HasPrefix(req.Name, "final-"):
				findings := []any{}
				if !strings.Contains(req.Workspace.Directory(), string(f.Stream1)) && !strings.HasPrefix(req.Name, "final-1-") {
					dependency, e := intakefixture.ReadTool(ctx, tools, "branch/internal/trace/dependency.txt")
					if e != nil || strings.TrimSpace(dependency) != archivedUpstreamMerged {
						return nil, fmt.Errorf("final reader missed dependency update: %q, %v", dependency, e)
					}
					if strings.HasPrefix(req.Name, "final-2-") {
						findings = append(findings, map[string]any{"severity": "blocking", "criteria": []any{"spec#1"}, "finding": "TestResume fails with the merged dependency; repair the client within its slice"})
					} else if _, e := intakefixture.ReadTool(ctx, tools, "branch/internal/trace/repaired.go"); e != nil {
						return nil, e
					}
				}
				_, err = fixturekit.CallTool(ctx, tools, service.FinalReportTool, map[string]any{"summary": "The client resumes uploads", "findings": findings})
			default:
				return manager(ctx, req, turn, tools)
			}
			if err != nil {
				t.Logf("engine turn %s: %v", req.Name, err)
			}
			return intakefixture.QuestionResult(req, "session-"+req.Name, "Implemented and reviewed"), err
		}
		f.Engine.Mu.Unlock()
	}
}

func (a *archivedUpstream) dependent(t *testing.T) *trace.Repository {
	t.Helper()
	down, err := a.f.S.Repository(a.f.Project2)
	fixturekit.Must(t, err)
	return down
}

func (a *archivedUpstream) logDependentReviewState(t *testing.T, down *trace.Repository) {
	t.Helper()
	stream := a.f.Stream2
	docs, readErr := trace.Read[trace.Document](down, stream)
	t.Logf("final report documents: %v", readErr)
	for _, doc := range docs {
		if doc.ID != delivery.FinalReportDocument {
			continue
		}
		var report api.FinalReport
		decodeErr := json.Unmarshal([]byte(doc.Content), &report)
		if decodeErr != nil || (report.Review >= 1 && report.Review <= 3) {
			t.Logf("final report revision=%d decode-error=%v: %s", doc.Revision, decodeErr, doc.Content)
		}
	}
	units, readErr := followup.Read(down, stream)
	t.Logf("follow-ups: %+v error=%v", units, readErr)
	for _, unit := range units {
		state, stateErr := down.Workflow(stream, trace.UnitSubject(unit.Unit.ID))
		t.Logf("follow-up %s workflow=%+v error=%v", unit.Unit.ID, state, stateErr)
	}
	transitions, readErr := trace.Read[trace.Transition](down, stream)
	t.Logf("dependent transitions: %v", readErr)
	for _, tr := range transitions {
		if tr.Subject != "base-refresh" && !strings.HasPrefix(tr.ID, "route-") {
			t.Logf("transition %s at=%s %s: %s -> %s", tr.ID, tr.At.Format(time.RFC3339), tr.Subject, tr.From, tr.To)
		}
	}
	ops, readErr := down.Operations(stream)
	t.Logf("delivery review operations: %v", readErr)
	for _, op := range ops {
		if op.Operation.Action == lifecycle.DeliveryReviewAction || op.Operation.Action == delivery.FinalReviewAction {
			t.Logf("delivery review operation=%s input=%s result=%+v acknowledged=%v", op.Operation.ID, op.Operation.Input, op.Result, op.Acknowledged)
		}
	}
}

func (a *archivedUpstream) awaitReview(t *testing.T, review int) api.DeliveryPresentation {
	t.Helper()
	ctx := context.Background()
	f := a.f
	var out api.DeliveryPresentation
	ready := func() bool {
		down := a.dependent(t)
		var err error
		out, err = f.C.Delivery(ctx, f.Stream2)
		threads, readErr := down.Threads(f.Stream2)
		fixturekit.Must(t, readErr)
		for _, thread := range threads {
			if thread.Identity.Role != intake.MasonRole && thread.Identity.Role != intake.ReviewerRole {
				continue
			}
			for _, turn := range thread.Turns {
				if turn.Response != nil && turn.Response.Failure != "" {
					t.Fatalf("role turn failed: %s: %s", turn.Request.TurnID, turn.Response.Failure)
				}
			}
		}
		return err == nil && out.Report.Review == review && out.Draft != ""
	}
	changes, closeChanges := journey.ServiceChanges(t, f.S)
	defer closeChanges()
	deadline := time.Now().Add(fixturekit.DemoTimeout)
	for !ready() {
		if time.Now().After(deadline) {
			a.logDependentReviewState(t, a.dependent(t))
			f.Timeout(t, fmt.Sprintf("dependent final review %d", review))
		}
		journey.LifecycleAdvanceScheduled(t, f)
		changes()
	}
	return out
}

func (a *archivedUpstream) approve(t *testing.T, p api.DeliveryPresentation) api.DeliveryApproval {
	t.Helper()
	approval, err := a.f.C.ApproveDelivery(context.Background(), a.f.Stream2, api.DeliveryDecision{Review: p.Report.Review, ReviewRevision: p.ReviewRevision, Commit: p.Report.Commit, DraftHash: p.DraftHash})
	fixturekit.Must(t, err)
	return approval
}

func (a *archivedUpstream) publishDependentDraft(t *testing.T) (api.DeliveryPresentation, api.DeliveryPublication) {
	t.Helper()
	first := a.awaitReview(t, 1)
	initialApproval := a.approve(t, first)
	var draft api.DeliveryPublication
	journey.LifecycleAwait(t, a.f, "owner-approved dependent draft", func() bool {
		down := a.dependent(t)
		state, err := down.Workflow(a.f.Stream2, trace.FeatureSubject)
		fixturekit.Must(t, err)
		var found bool
		draft, found, err = delivery.CurrentOwnPublication(down, a.f.Stream2)
		fixturekit.Must(t, err)
		return state.Value == intake.DeliveredState && found && draft.Draft && draft.Reviewed == initialApproval.Commit
	})
	return first, draft
}

func (a *archivedUpstream) requireOnlyDeliveryPrompts(t *testing.T) {
	t.Helper()
	inbox, err := a.f.C.Inbox(context.Background())
	fixturekit.Must(t, err)
	for _, entry := range inbox.Entries {
		if entry.Kind != api.InboxDelivery {
			t.Fatalf("unexpected owner prompt: %+v", entry)
		}
	}
	ledger := fixturekit.ReadNotificationLedger[service.TestNotifyLedger](t, a.f.Opts.Config.Root)
	for _, notification := range ledger.Notifications {
		if notification.Kind != api.InboxRatification && notification.Kind != api.InboxDelivery {
			t.Fatalf("unexpected notification for dependency wait: %+v", notification)
		}
	}
}

func (a *archivedUpstream) mergeUpstream(t *testing.T) {
	t.Helper()
	a.host.Mu.Lock()
	a.host.Prs[0].Merged, a.host.Prs[0].State, a.host.Prs[0].MergeCommit = true, "closed", archivedUpstreamMerged
	a.host.Mu.Unlock()
	a.f.Clock.Advance(10 * time.Minute)
	journey.LifecycleAwait(t, a.f, "edge read interrupted before persistence", a.observed.Load)
}

func (a *archivedUpstream) archiveUpstream(t *testing.T) {
	t.Helper()
	_, err := a.f.C.Archive(context.Background(), a.f.Stream1)
	fixturekit.Must(t, err)
	a.cleanup.Await(t)
}

func (a *archivedUpstream) releaseCleanup(t *testing.T) {
	t.Helper()
	a.cleanup.Release()
	journey.LifecycleAwait(t, a.f, "authorized upstream cleanup", func() bool {
		archived, ok := intakefixture.ArchiveRecord(t, a.f.S.Store, a.f.Stream1)
		return ok && !archived.CleanedAt.IsZero()
	})
}

func (a *archivedUpstream) restartWithTheEdgeReadResumed(t *testing.T) {
	t.Helper()
	a.f.Stop(t)
	a.interrupt.Store(false)
	a.f.Start(t)
}

func (a *archivedUpstream) repairAfterTheUpstreamMerge(t *testing.T) (first api.DeliveryPresentation, draft api.DeliveryPublication, beforeRevision int, fresh api.DeliveryPresentation) {
	t.Helper()
	first, draft = a.publishDependentDraft(t)
	a.mergeUpstream(t)
	a.archiveUpstream(t)
	_, beforeRevision, _, err := lifecycle.EdgeObservationAt(a.dependent(t), a.f.Stream2)
	fixturekit.Must(t, err)
	a.releaseCleanup(t)
	a.restartWithTheEdgeReadResumed(t)
	fresh = a.awaitReview(t, 3)
	return first, draft, beforeRevision, fresh
}

func TestDependentDraftNamesItsUnmergedUpstreamAndWaitsOnTheMergeWithoutPrompting(t *testing.T) {
	t.Parallel()
	a := newArchivedUpstream(t)
	defer a.f.Stop(t)
	down := a.dependent(t)

	first, _ := a.publishDependentDraft(t)
	if !strings.Contains(first.Draft, a.upstream.Upstream+"#71") {
		t.Fatalf("draft description lost upstream dependency: %s", first.Draft)
	}
	if state, err := down.Workflow(a.f.Stream2, "publication"); err != nil || state.Value != "published-1" {
		t.Fatalf("the approval's outcome %+v: %v", state, err)
	}
	a.requireOnlyDeliveryPrompts(t)
	if reference, waiting, err := lifecycle.UnresolvedMergeOrderEdge(down, a.f.Stream2); err != nil || !waiting || reference != a.upstream.Upstream+"#71" {
		t.Fatalf("dependent lacks truthful merge wait: %q %v %v", reference, waiting, err)
	}
}

func TestArchivingAMergedUpstreamKeepsItsPublicationAndHoldsTheDependentsMergeUntilCleanup(t *testing.T) {
	t.Parallel()
	a := newArchivedUpstream(t)
	defer a.f.Stop(t)

	a.publishDependentDraft(t)
	a.mergeUpstream(t)
	a.archiveUpstream(t)
	archived, ok := intakefixture.ArchiveRecord(t, a.f.S.Store, a.f.Stream1)
	if !ok || archived.Publication.PullRequest != a.upstream.PullRequest || archived.Publication.Branch != a.upstream.Branch {
		t.Fatalf("archive lost current upstream publication: %+v", archived)
	}
	before, _, _, err := lifecycle.EdgeObservationAt(a.dependent(t), a.f.Stream2)
	fixturekit.Must(t, err)
	if len(before.Edges) != 1 || before.Edges[0].Merged {
		t.Fatalf("dependent persisted merge before cleanup: %+v", before)
	}
	a.releaseCleanup(t)
}

func TestMergedUpstreamRepairKeepsTheProjectSpecActiveForTheDependent(t *testing.T) {
	t.Parallel()
	a := newArchivedUpstream(t)
	defer a.f.Stop(t)
	f := a.f
	ctx := context.Background()

	a.repairAfterTheUpstreamMerge(t)
	down := a.dependent(t)
	group, err := down.CrossRepoWorkgroupOf(f.Stream2)
	fixturekit.Must(t, err)
	authority, err := trace.OpenWorkgroupAuthority(f.S.Current().Root, group)
	fixturekit.Must(t, err)
	resolved, resolutionRevision, err := trace.ResolvedWorkgroups(authority)
	fixturekit.Must(t, err)
	active, err := trace.ProjectSpecActive(authority, group)
	fixturekit.Must(t, err)
	fixturekit.Must(t, authority.Close())
	if len(resolved) != 0 || resolutionRevision != 0 || !active {
		t.Fatalf("project spec retired before dependent delivery: resolved=%v revision=%d active=%v", resolved, resolutionRevision, active)
	}
	for _, role := range []string{intake.MasonRole, trace.Manager} {
		view, err := f.S.Context().Assemble(ctx, f.Project2, bundle.Scope{Role: role, Workstream: f.Stream2})
		fixturekit.Must(t, err)
		if view.ProjectSpec == nil || !strings.Contains(view.Render(), "Stored chunks are acknowledged with their offset.") {
			t.Fatalf("surviving %s lost the ratified project contract", role)
		}
	}
}

func TestArchivedUpstreamMergeOrderRepairAndFreshApprovalAcrossRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	a := newArchivedUpstream(t)
	f := a.f
	defer f.Stop(t)

	first, draft, beforeRevision, fresh := a.repairAfterTheUpstreamMerge(t)
	down := a.dependent(t)
	assertBaseRefreshesBounded(t, down, f.Stream2)
	assertMergedEdgeReviewsFollowTheUpdate(t, down, f.Stream2)
	if fresh.Report.Commit == first.Report.Commit || fresh.Report.EdgeResolved == "" {
		t.Fatalf("missing dependency repair or re-review: report=%+v", fresh.Report)
	}
	updates, err := followup.Read(down, f.Stream2)
	fixturekit.Must(t, err)
	if len(updates) != 2 || !strings.Contains(updates[0].Unit.ID, "dependency-update-") || !strings.Contains(updates[0].Unit.Task, archivedUpstreamMerged) || updates[1].Criterion != "spec#1" {
		t.Fatalf("update or in-slice repair missing/duplicated: %+v", updates)
	}
	for _, unit := range updates {
		state, err := down.Workflow(f.Stream2, trace.UnitSubject(unit.Unit.ID))
		fixturekit.Must(t, err)
		if state.Value != intake.UnitMerged {
			t.Fatalf("unit did not land through the controller: %+v", state)
		}
	}
	a.requireOnlyDeliveryPrompts(t)

	pass := a.passes.Load()
	journey.LifecycleAwait(t, f, "publication stays gated after re-review", func() bool { return a.passes.Load() > pass+1 })
	a.host.Mu.Lock()
	readies, creates := a.host.Readies, a.host.Creates
	a.host.Mu.Unlock()
	remoteTip := a.host.Fork()
	if readies != 0 || creates != 1 || remoteTip != draft.Commit {
		t.Fatalf("publication escaped fresh approval: ready=%d create=%d pushed=%s draft=%s", readies, creates, remoteTip, draft.Commit)
	}
	if _, err := f.C.ApproveDelivery(ctx, f.Stream2, api.DeliveryDecision{Review: first.Report.Review, ReviewRevision: first.ReviewRevision, Commit: first.Report.Commit, DraftHash: first.DraftHash}); err == nil {
		t.Fatal("stale pinned approval authorized the changed dependency")
	}
	freshApproval := a.approve(t, fresh)
	if freshApproval.Commit != fresh.Report.Commit || freshApproval.Review != fresh.Report.Review {
		t.Fatalf("approval lacks fresh review and commit pins: %+v", freshApproval)
	}
	var ready api.DeliveryPublication
	journey.LifecycleAwait(t, f, "fresh approval pushes and marks same PR ready", func() bool {
		var found bool
		ready, found, err = delivery.CurrentOwnPublication(down, f.Stream2)
		fixturekit.Must(t, err)
		return found && !ready.Draft && ready.Approval == 2
	})
	if ready.PullRequest != draft.PullRequest || ready.Commit == draft.Commit {
		t.Fatalf("ready publication lost PR identity or content update: %+v", ready)
	}
	a.host.Mu.Lock()
	for _, pr := range a.host.Prs {
		if pr.Number == ready.PullRequest && (pr.Draft || pr.Body != freshApproval.Description) {
			t.Fatalf("the pull request is not ready with the approved text: %+v", pr)
		}
	}
	a.host.Mu.Unlock()
	clone := f.Repos[1].Clone
	if got := strings.TrimSpace(fixturekit.DemoGit(t, filepath.Dir(clone), "-C", clone, "show", ready.Commit+":internal/trace/dependency.txt")); got != archivedUpstreamMerged {
		t.Fatalf("published dependency=%q", got)
	}
	if !strings.Contains(fixturekit.DemoGit(t, filepath.Dir(clone), "-C", clone, "cat-file", "-p", ready.Commit), "gpgsig") {
		t.Fatal("ready publication lacks owner signing")
	}
	observation, revision, _, err := lifecycle.EdgeObservationAt(down, f.Stream2)
	fixturekit.Must(t, err)
	if revision != beforeRevision+1 || len(observation.Edges) != 1 || !observation.Edges[0].Merged || observation.Edges[0].Commit != archivedUpstreamMerged {
		t.Fatalf("merge release receipt: before=%d after=%d %+v", beforeRevision, revision, observation)
	}

	f.Stop(t)
	f.Start(t)
	down = a.dependent(t)
	pass = a.passes.Load()
	journey.LifecycleAwait(t, f, "stable publication after second restart", func() bool { return a.passes.Load() > pass+1 })
	after, afterRevision, _, err := lifecycle.EdgeObservationAt(down, f.Stream2)
	fixturekit.Must(t, err)
	if afterRevision != revision || !slices.Equal(after.Edges, observation.Edges) {
		t.Fatalf("restart duplicated merge release: %+v revision=%d", after, afterRevision)
	}
	units, err := followup.Read(down, f.Stream2)
	fixturekit.Must(t, err)
	if len(units) != len(updates) {
		t.Fatalf("restart duplicated dependency work: %+v", units)
	}
	a.host.Mu.Lock()
	readies, creates = a.host.Readies, a.host.Creates
	a.host.Mu.Unlock()
	if readies != 1 || creates != 1 {
		t.Fatalf("publication duplicated: readies=%d creates=%d", readies, creates)
	}
	gone, err := trace.WorkstreamTraceGone(f.S.Current().Root, f.Project, f.Stream1)
	fixturekit.Must(t, err)
	if !gone {
		t.Fatal("upstream history was recreated")
	}
	streams, err := down.Workstreams()
	fixturekit.Must(t, err)
	if !slices.Equal(streams, []config.WorkstreamID{f.Stream2}) {
		t.Fatalf("repair created another workstream: %v", streams)
	}
	a.requireOnlyDeliveryPrompts(t)
}

func assertBaseRefreshesBounded(t *testing.T, r *trace.Repository, w config.WorkstreamID) {
	t.Helper()
	const bound = 200
	transitions, err := trace.Read[trace.Transition](r, w)
	fixturekit.Must(t, err)
	n := 0
	for _, tr := range transitions {
		if tr.Subject == "base-refresh" {
			n++
		}
	}
	if n > bound {
		t.Fatalf("%d base-refresh transitions recorded; want at most %d", n, bound)
	}
}

func awaitDependentEdgeMerged(ctx context.Context, f *journey.ProductionDesignFixture) error {
	if f == nil {
		return nil
	}
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		if down, err := f.S.Repository(f.Project2); err == nil {
			if observed, _, _, err := lifecycle.EdgeObservationAt(down, f.Stream2); err == nil && len(observed.Edges) == 1 && observed.Edges[0].Merged {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func assertMergedEdgeReviewsFollowTheUpdate(t *testing.T, r *trace.Repository, w config.WorkstreamID) {
	t.Helper()
	transitions, err := trace.Read[trace.Transition](r, w)
	fixturekit.Must(t, err)
	units, err := followup.Read(r, w)
	fixturekit.Must(t, err)
	var update string
	for _, unit := range units {
		if strings.HasPrefix(unit.Unit.ID, "dependency-update-") {
			update = unit.Unit.ID
		}
	}
	if update == "" {
		t.Fatalf("the dependent recorded no dependency update: %+v", units)
	}
	updated := -1
	for i, tr := range transitions {
		if tr.Subject == trace.UnitSubject(update) && tr.To == intake.UnitMerged {
			updated = i
		}
	}
	if updated < 0 {
		t.Fatalf("the dependency update %s never merged", update)
	}
	operations, err := r.Operations(w)
	fixturekit.Must(t, err)
	pinned := 0
	for _, op := range operations {
		if op.Operation.Action != delivery.FinalReviewAction && op.Operation.Action != lifecycle.DeliveryReviewAction {
			continue
		}
		var input struct {
			Review       int    `json:"review"`
			EdgeResolved string `json:"edge_resolved"`
		}
		fixturekit.Must(t, json.Unmarshal(op.Operation.Input, &input))
		if input.EdgeResolved == "" {
			continue
		}
		pinned++
		for i, tr := range transitions {
			if tr.Subject == "final-review" && tr.To == fmt.Sprintf("requested-%d", input.Review) && i < updated {
				t.Fatalf("final review %d pins the merged dependency but was requested before the update %s merged", input.Review, update)
			}
		}
	}
	if pinned == 0 {
		t.Fatal("no final review pinned the merged dependency")
	}
}
