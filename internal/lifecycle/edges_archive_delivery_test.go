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

func TestArchivedUpstreamMergeOrderRepairAndFreshApprovalAcrossRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const merged = "aabbccddeeff0011223344556677889900aabbccdd"
	cleanup := &lifecycleCleanupGate{reached: make(chan struct{}), released: make(chan struct{})}
	t.Cleanup(cleanup.release)
	var interrupt, observed atomic.Bool
	interrupt.Store(true)
	var fixture atomic.Pointer[productionDesignFixture]
	var passes atomic.Int64
	host := &fixturekit.FakePulls{SyncHead: true}
	hook := newFakeWebhook(t)
	configuration := productionDesignConfiguration{
		wait:                 lifecycleAwait,
		openPullRequestsStay: true,
		checkpoint: func(ctx context.Context, step string) error {
			if step == "edge-merge-observed" && interrupt.Load() {
				observed.Store(true)
				<-ctx.Done()
				return ctx.Err()
			}
			if step == "final-review-governed" && !interrupt.Load() {
				if err := awaitDependentEdgeMerged(ctx, fixture.Load()); err != nil {
					return err
				}
			}
			return cleanup.checkpoint(ctx, step)
		},
		options: func(opts *Options) {
			path := filepath.Join(opts.Config.Root, "config.toml")
			data, err := os.ReadFile(path)
			fixturekit.Must(t, err)
			data = []byte(strings.Replace(string(data), "[capacity]\n", "[capacity]\nmasons = 2\n", 1) + "[roles.mason]\nsandbox = \"container\"\nimage = \"fixture-image\"\n[roles.reviewer]\nsandbox = \"container\"\nimage = \"fixture-image\"\n")
			fixturekit.Must(t, os.WriteFile(path, data, 0600))
			setWebhook(t, *opts, hook.hook())
			hook.records(*opts)
			opts.schedulePassed = func() { passes.Add(1) }
			opts.reviewChecks = fixturekit.CheckFunc(func(_ context.Context, directory string) (api.CheckResult, error) {
				dependency, err := os.ReadFile(filepath.Join(directory, "internal/trace/dependency.txt"))
				if os.IsNotExist(err) {
					return api.CheckResult{Output: "initial checks pass"}, nil
				}
				if err != nil || strings.TrimSpace(string(dependency)) != merged {
					return api.CheckResult{}, fmt.Errorf("wrong dependency: %q, %v", dependency, err)
				}
				return api.CheckResult{Output: "unit checks pass"}, nil
			})
		},
	}
	f := resolutionApprovedFixture(t, configuration, trace.EdgeMergeOrder, host, func(f *productionDesignFixture) {
		fixture.Store(f)
		clone := f.repos[1].Clone
		home := filepath.Dir(clone)
		host.Fork = func() string {
			return strings.TrimSpace(fixturekit.DemoGit(t, home, "-C", f.repos[1].Remote, "for-each-ref", "--format=%(objectname)", "refs/heads/"+intake.FeatureBranch(f.stream2)))
		}
		fixturekit.ConfigureDeliverySigner(t, clone)
		f.engine.Mu.Lock()
		chief := f.engine.Turns["*"]
		f.engine.Turns["*"] = func(ctx context.Context, req agent.Request, turn *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
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
					recorded, reason, err = intakefixture.Done(ctx, tools, map[string]any{"outcome": "Implement the slice"})
					if err == nil && !recorded {
						err = fmt.Errorf("done refused: %s", reason)
					}
				}
			case strings.HasPrefix(req.Name, "reviewer-") || strings.HasPrefix(req.Name, "reviewer_"):
				_, err = fixturekit.CallTool(ctx, tools, execution.VerdictTool, map[string]any{"decision": "satisfactory", "summary": reviewSummary, "findings": []api.ReviewFinding{}})
			case strings.HasPrefix(req.Name, "final-"):
				findings := []any{}
				if !strings.Contains(req.Workspace.Directory(), string(f.stream1)) && !strings.HasPrefix(req.Name, "final-1-") {
					dependency, e := intakefixture.ReadTool(ctx, tools, "branch/internal/trace/dependency.txt")
					if e != nil || strings.TrimSpace(dependency) != merged {
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
				return chief(ctx, req, turn, tools)
			}
			if err != nil {
				t.Logf("engine turn %s: %v", req.Name, err)
			}
			return intakefixture.QuestionResult(req, "session-"+req.Name, "Implemented and reviewed"), err
		}
		f.engine.Mu.Unlock()
	})
	defer f.stop(t)
	up, err := f.s.repository(f.project)
	fixturekit.Must(t, err)
	down, err := f.s.repository(f.project2)
	fixturekit.Must(t, err)
	upstream := api.DeliveryPublication{Status: delivery.PublicationOpened, Upstream: f.repos[0].Upstream, Fork: f.s.current().For(f.project).Project.PushRepository(), Branch: intake.FeatureBranch(f.stream1), Commit: "upstream-feature", PullRequest: 71, URL: "https://example.invalid/pull/71"}
	recordUpstreamOwnPullRequest(t, f.s, up, f.stream1, upstream)
	host.Mu.Lock()
	host.Prs = []pulls.PullRequest{{Number: upstream.PullRequest, State: "open", HeadRepository: upstream.Fork, Head: upstream.Branch, URL: upstream.URL}}
	host.Mu.Unlock()

	awaitReview := func(review int) api.DeliveryPresentation {
		t.Helper()
		var out api.DeliveryPresentation
		ready := func() bool {
			out, err = f.c.Delivery(ctx, f.stream2)
			threads, readErr := down.Threads(f.stream2)
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
		changes, closeChanges := serviceChanges(t, f.s)
		defer closeChanges()
		deadline := time.Now().Add(fixturekit.DemoTimeout)
		for !ready() {
			if time.Now().After(deadline) {
				docs, readErr := trace.Read[trace.Document](down, f.stream2)
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
				units, readErr := followup.Read(down, f.stream2)
				t.Logf("follow-ups: %+v error=%v", units, readErr)
				for _, unit := range units {
					state, stateErr := down.Workflow(f.stream2, trace.UnitSubject(unit.Unit.ID))
					t.Logf("follow-up %s workflow=%+v error=%v", unit.Unit.ID, state, stateErr)
				}
				transitions, readErr := trace.Read[trace.Transition](down, f.stream2)
				t.Logf("dependent transitions: %v", readErr)
				for _, tr := range transitions {
					if tr.Subject != "base-refresh" && !strings.HasPrefix(tr.ID, "route-") {
						t.Logf("transition %s at=%s %s: %s -> %s", tr.ID, tr.At.Format(time.RFC3339), tr.Subject, tr.From, tr.To)
					}
				}
				ops, readErr := down.Operations(f.stream2)
				t.Logf("delivery review operations: %v", readErr)
				for _, op := range ops {
					if op.Operation.Action == lifecycle.DeliveryReviewAction || op.Operation.Action == delivery.FinalReviewAction {
						t.Logf("delivery review operation=%s input=%s result=%+v acknowledged=%v", op.Operation.ID, op.Operation.Input, op.Result, op.Acknowledged)
					}
				}
				f.timeout(t, fmt.Sprintf("dependent final review %d", review))
			}
			lifecycleAdvanceScheduled(t, f)
			changes()
		}
		return out
	}
	approve := func(p api.DeliveryPresentation) api.DeliveryApproval {
		t.Helper()
		a, err := f.c.ApproveDelivery(ctx, f.stream2, api.DeliveryDecision{Review: p.Report.Review, ReviewRevision: p.ReviewRevision, Commit: p.Report.Commit, DraftHash: p.DraftHash})
		fixturekit.Must(t, err)
		return a
	}
	first := awaitReview(1)
	if !strings.Contains(first.Draft, upstream.Upstream+"#71") {
		t.Fatalf("draft description lost upstream dependency: %s", first.Draft)
	}
	initialApproval := approve(first)
	var draft api.DeliveryPublication
	lifecycleAwait(t, f, "owner-approved dependent draft", func() bool {
		state, err := down.Workflow(f.stream2, trace.FeatureSubject)
		fixturekit.Must(t, err)
		var found bool
		draft, found, err = delivery.CurrentOwnPublication(down, f.stream2)
		fixturekit.Must(t, err)
		return state.Value == intake.DeliveredState && found && draft.Draft && draft.Reviewed == initialApproval.Commit
	})
	if state, err := down.Workflow(f.stream2, "publication"); err != nil || state.Value != "published-1" {
		t.Fatalf("the approval's outcome %+v: %v", state, err)
	}
	assertPrompts := func() {
		t.Helper()
		inbox, err := f.c.Inbox(ctx)
		fixturekit.Must(t, err)
		for _, entry := range inbox.Entries {
			if entry.Kind != api.InboxDelivery {
				t.Fatalf("unexpected owner prompt: %+v", entry)
			}
		}
		ledger := readLedger(t, f.opts)
		for _, notification := range ledger.Notifications {
			if notification.Kind != api.InboxRatification && notification.Kind != api.InboxDelivery {
				t.Fatalf("unexpected notification for dependency wait: %+v", notification)
			}
		}
	}
	assertPrompts()
	if reference, waiting, err := lifecycle.UnresolvedMergeOrderEdge(down, f.stream2); err != nil || !waiting || reference != upstream.Upstream+"#71" {
		t.Fatalf("dependent lacks truthful merge wait: %q %v %v", reference, waiting, err)
	}
	host.Mu.Lock()
	host.Prs[0].Merged, host.Prs[0].State, host.Prs[0].MergeCommit = true, "closed", merged
	host.Mu.Unlock()
	f.clock.Advance(10 * time.Minute)
	lifecycleAwait(t, f, "edge read interrupted before persistence", observed.Load)
	_, err = f.c.Archive(ctx, f.stream1)
	fixturekit.Must(t, err)
	cleanup.await(t)
	archived, ok := intakefixture.ArchiveRecord(t, f.s.store, f.stream1)
	if !ok || archived.Publication.PullRequest != upstream.PullRequest || archived.Publication.Branch != upstream.Branch {
		t.Fatalf("archive lost current upstream publication: %+v", archived)
	}
	before, beforeRevision, _, err := lifecycle.EdgeObservationAt(down, f.stream2)
	fixturekit.Must(t, err)
	if len(before.Edges) != 1 || before.Edges[0].Merged {
		t.Fatalf("dependent persisted merge before cleanup: %+v", before)
	}
	cleanup.release()
	lifecycleAwait(t, f, "authorized upstream cleanup", func() bool {
		a, ok := intakefixture.ArchiveRecord(t, f.s.store, f.stream1)
		return ok && !a.CleanedAt.IsZero()
	})
	f.stop(t)
	interrupt.Store(false)
	f.start(t)
	down, err = f.s.repository(f.project2)
	fixturekit.Must(t, err)
	fresh := awaitReview(3)
	assertBaseRefreshesBounded(t, down, f.stream2)
	assertMergedEdgeReviewsFollowTheUpdate(t, down, f.stream2)
	if fresh.Report.Commit == first.Report.Commit || fresh.Report.EdgeResolved == "" {
		t.Fatalf("missing dependency repair or re-review: report=%+v", fresh.Report)
	}
	updates, err := followup.Read(down, f.stream2)
	fixturekit.Must(t, err)
	if len(updates) != 2 || !strings.Contains(updates[0].Unit.ID, "dependency-update-") || !strings.Contains(updates[0].Unit.Task, merged) || updates[1].Criterion != "spec#1" {
		t.Fatalf("update or in-slice repair missing/duplicated: %+v", updates)
	}
	for _, unit := range updates {
		state, err := down.Workflow(f.stream2, trace.UnitSubject(unit.Unit.ID))
		fixturekit.Must(t, err)
		if state.Value != intake.UnitMerged {
			t.Fatalf("unit did not land through the controller: %+v", state)
		}
	}
	assertPrompts()
	group, err := down.CrossRepoWorkgroupOf(f.stream2)
	fixturekit.Must(t, err)
	authority, err := trace.OpenWorkgroupAuthority(f.s.current().Root, group)
	fixturekit.Must(t, err)
	resolved, resolutionRevision, err := trace.ResolvedWorkgroups(authority)
	fixturekit.Must(t, err)
	active, err := trace.ProjectSpecActive(authority, group)
	fixturekit.Must(t, err)
	fixturekit.Must(t, authority.Close())
	if len(resolved) != 0 || resolutionRevision != 0 || !active {
		t.Fatalf("project spec retired before dependent delivery: resolved=%v revision=%d active=%v", resolved, resolutionRevision, active)
	}
	for _, role := range []string{intake.MasonRole, trace.ChiefOfStaff} {
		view, err := f.s.Context().Assemble(ctx, f.project2, bundle.Scope{Role: role, Workstream: f.stream2})
		fixturekit.Must(t, err)
		if view.ProjectSpec == nil || !strings.Contains(view.Render(), "Stored chunks are acknowledged with their offset.") {
			t.Fatalf("surviving %s lost the ratified project contract", role)
		}
	}
	pass := passes.Load()
	lifecycleAwait(t, f, "publication stays gated after re-review", func() bool { return passes.Load() > pass+1 })
	host.Mu.Lock()
	readies, creates := host.Readies, host.Creates
	host.Mu.Unlock()
	remoteTip := host.Fork()
	if readies != 0 || creates != 1 || remoteTip != draft.Commit {
		t.Fatalf("publication escaped fresh approval: ready=%d create=%d pushed=%s draft=%s", readies, creates, remoteTip, draft.Commit)
	}
	if _, err := f.c.ApproveDelivery(ctx, f.stream2, api.DeliveryDecision{Review: first.Report.Review, ReviewRevision: first.ReviewRevision, Commit: first.Report.Commit, DraftHash: first.DraftHash}); err == nil {
		t.Fatal("stale pinned approval authorized the changed dependency")
	}
	freshApproval := approve(fresh)
	if freshApproval.Commit != fresh.Report.Commit || freshApproval.Review != fresh.Report.Review {
		t.Fatalf("approval lacks fresh review and commit pins: %+v", freshApproval)
	}
	var ready api.DeliveryPublication
	lifecycleAwait(t, f, "fresh approval pushes and marks same PR ready", func() bool {
		var found bool
		ready, found, err = delivery.CurrentOwnPublication(down, f.stream2)
		fixturekit.Must(t, err)
		return found && !ready.Draft && ready.Approval == 2
	})
	if ready.PullRequest != draft.PullRequest || ready.Commit == draft.Commit {
		t.Fatalf("ready publication lost PR identity or content update: %+v", ready)
	}
	host.Mu.Lock()
	for _, pr := range host.Prs {
		if pr.Number == ready.PullRequest && (pr.Draft || pr.Body != freshApproval.Description) {
			t.Fatalf("the pull request is not ready with the approved text: %+v", pr)
		}
	}
	host.Mu.Unlock()
	clone := f.repos[1].Clone
	if got := strings.TrimSpace(fixturekit.DemoGit(t, filepath.Dir(clone), "-C", clone, "show", ready.Commit+":internal/trace/dependency.txt")); got != merged {
		t.Fatalf("published dependency=%q", got)
	}
	if !strings.Contains(fixturekit.DemoGit(t, filepath.Dir(clone), "-C", clone, "cat-file", "-p", ready.Commit), "gpgsig") {
		t.Fatal("ready publication lacks owner signing")
	}
	observation, revision, _, err := lifecycle.EdgeObservationAt(down, f.stream2)
	fixturekit.Must(t, err)
	if revision != beforeRevision+1 || len(observation.Edges) != 1 || !observation.Edges[0].Merged || observation.Edges[0].Commit != merged {
		t.Fatalf("merge release receipt: before=%d after=%d %+v", beforeRevision, revision, observation)
	}
	f.stop(t)
	f.start(t)
	down, err = f.s.repository(f.project2)
	fixturekit.Must(t, err)
	pass = passes.Load()
	lifecycleAwait(t, f, "stable publication after second restart", func() bool { return passes.Load() > pass+1 })
	after, afterRevision, _, err := lifecycle.EdgeObservationAt(down, f.stream2)
	fixturekit.Must(t, err)
	if afterRevision != revision || !slices.Equal(after.Edges, observation.Edges) {
		t.Fatalf("restart duplicated merge release: %+v revision=%d", after, afterRevision)
	}
	units, err := followup.Read(down, f.stream2)
	fixturekit.Must(t, err)
	if len(units) != len(updates) {
		t.Fatalf("restart duplicated dependency work: %+v", units)
	}
	host.Mu.Lock()
	readies, creates = host.Readies, host.Creates
	host.Mu.Unlock()
	if readies != 1 || creates != 1 {
		t.Fatalf("publication duplicated: readies=%d creates=%d", readies, creates)
	}
	gone, err := trace.WorkstreamTraceGone(f.s.current().Root, f.project, f.stream1)
	fixturekit.Must(t, err)
	if !gone {
		t.Fatal("upstream history was recreated")
	}
	streams, err := down.Workstreams()
	fixturekit.Must(t, err)
	if !slices.Equal(streams, []config.WorkstreamID{f.stream2}) {
		t.Fatalf("repair created another workstream: %v", streams)
	}
	assertPrompts()
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

func awaitDependentEdgeMerged(ctx context.Context, f *productionDesignFixture) error {
	if f == nil {
		return nil
	}
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		if down, err := f.s.repository(f.project2); err == nil {
			if observed, _, _, err := lifecycle.EdgeObservationAt(down, f.stream2); err == nil && len(observed.Edges) == 1 && observed.Edges[0].Merged {
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
