package service

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

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/bundle"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/followup"
	"github.com/kpenfound/osmia/internal/pulls"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestArchivedUpstreamMergeOrderRepairAndFreshApprovalAcrossRestart(t *testing.T) {
	ctx := context.Background()
	const merged = "aabbccddeeff0011223344556677889900aabbccdd"
	cleanup := &lifecycleCleanupGate{reached: make(chan struct{}), released: make(chan struct{})}
	t.Cleanup(cleanup.release)
	var interrupt, observed atomic.Bool
	interrupt.Store(true)
	var passes atomic.Int64
	var failedChecks, repairedChecks atomic.Int32
	host := &fakePulls{syncHead: true}
	hook := newFakeWebhook(t)
	configuration := productionDesignConfiguration{
		wait: lifecycleAwait,
		checkpoint: func(ctx context.Context, step string) error {
			if step == "edge-merge-observed" && interrupt.Load() {
				observed.Store(true)
				<-ctx.Done()
				return ctx.Err()
			}
			return cleanup.checkpoint(ctx, step)
		},
		options: func(opts *Options) {
			path := filepath.Join(opts.Config.Root, "config.toml")
			data, err := os.ReadFile(path)
			must(t, err)
			data = []byte(strings.Replace(string(data), "[capacity]\n", "[capacity]\nmasons = 2\n", 1) + "[roles.mason]\nsandbox = \"container\"\nimage = \"fixture-image\"\n[roles.reviewer]\nsandbox = \"container\"\nimage = \"fixture-image\"\n")
			must(t, os.WriteFile(path, data, 0600))
			setWebhook(t, *opts, hook.hook())
			hook.records(*opts)
			opts.schedulePassed = func() { passes.Add(1) }
			opts.reviewChecks = checkFunc(func(_ context.Context, directory string) (CheckResult, error) {
				dependency, err := os.ReadFile(filepath.Join(directory, "internal/trace/dependency.txt"))
				if os.IsNotExist(err) {
					return CheckResult{Output: "initial checks pass"}, nil
				}
				if err != nil || strings.TrimSpace(string(dependency)) != merged {
					return CheckResult{}, fmt.Errorf("wrong dependency: %q, %v", dependency, err)
				}
				if !strings.HasPrefix(filepath.Base(directory), "final-") {
					return CheckResult{Output: "unit checks pass"}, nil
				}
				if _, err := os.Stat(filepath.Join(directory, "internal/trace/repaired.go")); os.IsNotExist(err) {
					failedChecks.Add(1)
					return CheckResult{ExitCode: 1, Output: failedReport("dag://?check=go/packages/tests/test&go-test=TestResume") + "TestResume fails with the merged dependency\n"}, nil
				}
				repairedChecks.Add(1)
				return CheckResult{Output: "TestResume passes with the merged dependency"}, nil
			})
		},
	}
	f := resolutionApprovedFixture(t, configuration, trace.EdgeMergeOrder, host, func(f *productionDesignFixture) {
		clone := f.repos[1].clone
		home := filepath.Dir(clone)
		// The configured upstream is a local bare remote, so publication never
		// reaches GitHub or a real repository.
		host.fork = func() string {
			return strings.TrimSpace(demoGit(t, home, "-C", f.repos[1].remote, "for-each-ref", "--format=%(objectname)", "refs/heads/"+featureBranch(f.stream2)))
		}
		configureDeliverySigner(t, clone)
		f.engine.mu.Lock()
		chief := f.engine.turns["*"]
		f.engine.turns["*"] = func(ctx context.Context, req agent.Request, turn *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
			var err error
			switch {
			case strings.HasPrefix(req.Name, "mason-") && strings.HasSuffix(req.Name, "-implement"):
				path, content := "internal/trace/built.go", "package trace\n"
				if strings.Contains(req.Name, "dependency-update-") {
					if !strings.Contains(req.Prompt, merged) {
						return nil, fmt.Errorf("update lacks merged dependency context: %s", req.Prompt)
					}
					path, content = "internal/trace/dependency.txt", merged+"\n"
				} else if strings.Contains(req.Name, "final-") {
					path = "internal/trace/repaired.go"
				}
				if err = os.WriteFile(filepath.Join(req.Workspace.Directory(), path), []byte(content), 0600); err == nil {
					var recorded bool
					var reason string
					recorded, reason, err = done(ctx, tools, map[string]any{"outcome": "Implement the slice"})
					if err == nil && !recorded {
						err = fmt.Errorf("done refused: %s", reason)
					}
				}
			case strings.HasPrefix(req.Name, "reviewer-") || strings.HasPrefix(req.Name, "reviewer_"):
				_, err = callTool(ctx, tools, verdictTool, map[string]any{"decision": "satisfactory", "summary": reviewSummary, "findings": []ReviewFinding{}})
			case strings.HasPrefix(req.Name, "final-"):
				findings := []any{}
				if !strings.Contains(req.Workspace.Directory(), string(f.stream1)) && !strings.HasPrefix(req.Name, "final-1-") {
					dependency, e := readTool(ctx, tools, "branch/internal/trace/dependency.txt")
					if e != nil || strings.TrimSpace(dependency) != merged {
						return nil, fmt.Errorf("final reader missed dependency update: %q, %v", dependency, e)
					}
					if strings.HasPrefix(req.Name, "final-2-") {
						findings = append(findings, map[string]any{"severity": "blocking", "criteria": []any{"spec#1"}, "finding": "TestResume fails with the merged dependency; repair the client within its slice"})
					} else if _, e := readTool(ctx, tools, "branch/internal/trace/repaired.go"); e != nil {
						return nil, e
					}
				}
				_, err = callTool(ctx, tools, FinalReportTool, map[string]any{"summary": "The client resumes uploads", "findings": findings})
			default:
				return chief(ctx, req, turn, tools)
			}
			if err != nil {
				t.Logf("engine turn %s: %v", req.Name, err)
			}
			return questionResult(req, "session-"+req.Name, "Implemented and reviewed"), err
		}
		f.engine.mu.Unlock()
	})
	defer f.stop(t)
	up, err := f.s.repository(f.project)
	must(t, err)
	down, err := f.s.repository(f.project2)
	must(t, err)
	upstream := DeliveryPublication{Status: publicationOpened, Upstream: f.repos[0].upstream, Fork: f.s.current().For(f.project).Project.PushRepository(), Branch: featureBranch(f.stream1), Commit: "upstream-feature", PullRequest: 71, URL: "https://example.invalid/pull/71"}
	recordUpstreamOwnPullRequest(t, f.s, up, f.stream1, upstream)
	host.mu.Lock()
	host.prs = []pulls.PullRequest{{Number: upstream.PullRequest, State: "open", HeadRepository: upstream.Fork, Head: upstream.Branch, URL: upstream.URL}}
	host.mu.Unlock()

	awaitReview := func(review int) DeliveryPresentation {
		t.Helper()
		var out DeliveryPresentation
		lifecycleAwait(t, f, fmt.Sprintf("dependent final review %d", review), func() bool {
			out, err = f.c.Delivery(ctx, f.stream2)
			threads, readErr := down.Threads(f.stream2)
			must(t, readErr)
			for _, thread := range threads {
				if thread.Identity.Role != masonRole && thread.Identity.Role != reviewerRole {
					continue
				}
				for _, turn := range thread.Turns {
					if turn.Response != nil && turn.Response.Failure != "" {
						t.Fatalf("role turn failed: %s: %s", turn.Request.TurnID, turn.Response.Failure)
					}
				}
			}
			return err == nil && out.Report.Review == review && out.Draft != ""
		})
		return out
	}
	approve := func(p DeliveryPresentation) DeliveryApproval {
		t.Helper()
		a, err := f.c.ApproveDelivery(ctx, f.stream2, DeliveryDecision{Review: p.Report.Review, ReviewRevision: p.ReviewRevision, Commit: p.Report.Commit, DraftHash: p.DraftHash})
		must(t, err)
		return a
	}
	first := awaitReview(1)
	if !strings.Contains(first.Draft, upstream.Upstream+"#71") {
		t.Fatalf("draft description lost upstream dependency: %s", first.Draft)
	}
	initialApproval := approve(first)
	var draft DeliveryPublication
	lifecycleAwait(t, f, "owner-approved dependent draft", func() bool {
		var found bool
		draft, found, err = currentOwnPublication(down, f.stream2)
		must(t, err)
		return found && draft.Draft && draft.Approval == 1 && draft.Reviewed == initialApproval.Commit
	})
	assertPrompts := func() {
		t.Helper()
		inbox, err := f.c.Inbox(ctx)
		must(t, err)
		for _, entry := range inbox.Entries {
			if entry.Kind != InboxDelivery {
				t.Fatalf("unexpected owner prompt: %+v", entry)
			}
		}
		ledger := readLedger(t, f.opts)
		for _, notification := range ledger.Notifications {
			if notification.Kind != InboxRatification && notification.Kind != InboxDelivery {
				t.Fatalf("unexpected notification for dependency wait: %+v", notification)
			}
		}
	}
	assertPrompts()
	if reference, waiting, err := unresolvedMergeOrderEdge(down, f.stream2); err != nil || !waiting || reference != upstream.Upstream+"#71" {
		t.Fatalf("dependent lacks truthful merge wait: %q %v %v", reference, waiting, err)
	}
	host.mu.Lock()
	host.prs[0].Merged, host.prs[0].State, host.prs[0].MergeCommit = true, "closed", merged
	host.mu.Unlock()
	// Edge observations are polled at most once per minute.
	f.clock.Advance(time.Minute)
	lifecycleAwait(t, f, "edge read interrupted before persistence", observed.Load)
	_, err = f.c.Archive(ctx, f.stream1)
	must(t, err)
	cleanup.await(t)
	archived, ok := archiveRecord(t, f.s.store, f.stream1)
	if !ok || archived.Publication.PullRequest != upstream.PullRequest || archived.Publication.Branch != upstream.Branch {
		t.Fatalf("archive lost current upstream publication: %+v", archived)
	}
	before, beforeRevision, _, err := edgeObservationAt(down, f.stream2)
	must(t, err)
	if len(before.Edges) != 1 || before.Edges[0].Merged {
		t.Fatalf("dependent persisted merge before cleanup: %+v", before)
	}
	cleanup.release()
	lifecycleAwait(t, f, "authorized upstream cleanup", func() bool {
		a, ok := archiveRecord(t, f.s.store, f.stream1)
		return ok && !a.CleanedAt.IsZero()
	})
	f.stop(t)
	interrupt.Store(false)
	// The live controller recovers verified merge pins, or polls the exact
	// current publication retained by the archive after history deletion.
	f.start(t)
	down, err = f.s.repository(f.project2)
	must(t, err)
	fresh := awaitReview(3)
	if fresh.Report.Commit == first.Report.Commit || fresh.Report.EdgeResolved == "" || failedChecks.Load() != 1 || repairedChecks.Load() != 1 {
		t.Fatalf("missing dependency repair, checks or re-review: report=%+v failed=%d repaired=%d", fresh.Report, failedChecks.Load(), repairedChecks.Load())
	}
	updates, err := followup.Read(down, f.stream2)
	must(t, err)
	if len(updates) != 2 || !strings.Contains(updates[0].Unit.ID, "dependency-update-") || !strings.Contains(updates[0].Unit.Task, merged) || updates[1].Criterion != "spec#1" {
		t.Fatalf("update or in-slice repair missing/duplicated: %+v", updates)
	}
	for _, unit := range updates {
		state, err := down.Workflow(f.stream2, trace.UnitSubject(unit.Unit.ID))
		must(t, err)
		if state.Value != UnitMerged {
			t.Fatalf("unit did not land through the controller: %+v", state)
		}
	}
	for _, review := range []int{2, 3} {
		doc, err := trace.Get[trace.Document](down, f.stream2, finalChecksDocument(review), 1)
		must(t, err)
		var checks FinalCheckRun
		must(t, json.Unmarshal([]byte(doc.Content), &checks))
		if checks.Review != review || (review == 2 && checks.ExitCode != 1) || (review == 3 && (checks.ExitCode != 0 || checks.Commit != fresh.Report.Commit)) {
			t.Fatalf("wrong post-merge checks: %+v", checks)
		}
	}
	assertPrompts()
	group, err := down.CrossRepoWorkgroupOf(f.stream2)
	must(t, err)
	authority, err := trace.OpenWorkgroupAuthority(f.s.current().Root, group)
	must(t, err)
	resolved, resolutionRevision, err := trace.ResolvedWorkgroups(authority)
	must(t, err)
	active, err := trace.ProjectSpecActive(authority, group)
	must(t, err)
	must(t, authority.Close())
	if len(resolved) != 0 || resolutionRevision != 0 || !active {
		t.Fatalf("project spec retired before dependent delivery: resolved=%v revision=%d active=%v", resolved, resolutionRevision, active)
	}
	for _, role := range []string{masonRole, trace.ChiefOfStaff} {
		view, err := f.s.Context().Assemble(ctx, f.project2, bundle.Scope{Role: role, Workstream: f.stream2})
		must(t, err)
		if view.ProjectSpec == nil || !strings.Contains(view.Render(), "Stored chunks are acknowledged with their offset.") {
			t.Fatalf("surviving %s lost the ratified project contract", role)
		}
	}
	pass := passes.Load()
	lifecycleAwait(t, f, "publication stays gated after re-review", func() bool { return passes.Load() > pass+1 })
	host.mu.Lock()
	readies, creates := host.readies, host.creates
	host.mu.Unlock()
	remoteTip := host.fork()
	if readies != 0 || creates != 1 || remoteTip != draft.Commit {
		t.Fatalf("publication escaped fresh approval: ready=%d create=%d pushed=%s draft=%s", readies, creates, remoteTip, draft.Commit)
	}
	if _, err := f.c.ApproveDelivery(ctx, f.stream2, DeliveryDecision{Review: first.Report.Review, ReviewRevision: first.ReviewRevision, Commit: first.Report.Commit, DraftHash: first.DraftHash}); err == nil {
		t.Fatal("stale pinned approval authorized the changed dependency")
	}
	freshApproval := approve(fresh)
	if freshApproval.Commit != fresh.Report.Commit || freshApproval.Review != fresh.Report.Review {
		t.Fatalf("approval lacks fresh review and commit pins: %+v", freshApproval)
	}
	var ready DeliveryPublication
	lifecycleAwait(t, f, "fresh approval pushes and marks same PR ready", func() bool {
		var found bool
		ready, found, err = currentOwnPublication(down, f.stream2)
		must(t, err)
		return found && !ready.Draft && ready.Approval == 2
	})
	if ready.PullRequest != draft.PullRequest || ready.Commit == draft.Commit {
		t.Fatalf("ready publication lost PR identity or content update: %+v", ready)
	}
	clone := f.repos[1].clone
	if got := strings.TrimSpace(demoGit(t, filepath.Dir(clone), "-C", clone, "show", ready.Commit+":internal/trace/dependency.txt")); got != merged {
		t.Fatalf("published dependency=%q", got)
	}
	if !strings.Contains(demoGit(t, filepath.Dir(clone), "-C", clone, "cat-file", "-p", ready.Commit), "gpgsig") {
		t.Fatal("ready publication lacks owner signing")
	}
	observation, revision, _, err := edgeObservationAt(down, f.stream2)
	must(t, err)
	if revision != beforeRevision+1 || len(observation.Edges) != 1 || !observation.Edges[0].Merged || observation.Edges[0].Commit != merged {
		t.Fatalf("merge release receipt: before=%d after=%d %+v", beforeRevision, revision, observation)
	}
	f.stop(t)
	f.start(t)
	down, err = f.s.repository(f.project2)
	must(t, err)
	pass = passes.Load()
	lifecycleAwait(t, f, "stable publication after second restart", func() bool { return passes.Load() > pass+1 })
	after, afterRevision, _, err := edgeObservationAt(down, f.stream2)
	must(t, err)
	if afterRevision != revision || !slices.Equal(after.Edges, observation.Edges) {
		t.Fatalf("restart duplicated merge release: %+v revision=%d", after, afterRevision)
	}
	units, err := followup.Read(down, f.stream2)
	must(t, err)
	if len(units) != len(updates) {
		t.Fatalf("restart duplicated dependency work: %+v", units)
	}
	host.mu.Lock()
	readies, creates = host.readies, host.creates
	host.mu.Unlock()
	if readies != 1 || creates != 1 {
		t.Fatalf("publication duplicated: readies=%d creates=%d", readies, creates)
	}
	gone, err := trace.WorkstreamTraceGone(f.s.current().Root, f.project, f.stream1)
	must(t, err)
	if !gone {
		t.Fatal("upstream history was recreated")
	}
	streams, err := down.Workstreams()
	must(t, err)
	if !slices.Equal(streams, []config.WorkstreamID{librarianWorkstream(f.project2), f.stream2}) && !slices.Equal(streams, []config.WorkstreamID{f.stream2, librarianWorkstream(f.project2)}) {
		t.Fatalf("repair created another workstream: %v", streams)
	}
	assertPrompts()
}
