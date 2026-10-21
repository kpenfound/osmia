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

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/amendment"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/pulls"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func currentChange(t *testing.T, s *Service, address config.WorkstreamID, id string) WorkgroupChange {
	t.Helper()
	wt, g, _, close, err := s.changeContext(context.Background(), string(address))
	must(t, err)
	defer close()
	changes, err := readChanges(wt, g.Members[0])
	must(t, err)
	for _, c := range changes {
		if c.ID == id {
			return c
		}
	}
	return WorkgroupChange{}
}
func TestDeltaSchedulingRecordsRunnerIntentBeforeExecution(t *testing.T) {
	t.Parallel()
	f, stream, repo := newApprovedFixture(t, "delta-runner")
	defer repo.Close()
	f.s.setSole(runtimeFor(repo))
	ctx := context.Background()
	_, err := trace.EnsureWorkgroups(ctx, repo, []config.WorkstreamID{stream}, f.s.now(), serviceActor, "fixture")
	must(t, err)
	_, err = repo.EnsureChiefOfStaff(ctx, stream, f.s.now(), serviceActor)
	must(t, err)
	c, api := f.s.requestChange(ctx, string(stream), ChangeRequest{Key: "queued-delta", Kind: "amend", Scope: "workgroup", Change: "Resume from checkpoints"})
	if api != nil {
		t.Fatal(api)
	}
	var calls atomic.Int32
	f.engine.Mu.Lock()
	f.engine.Turns["*"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		calls.Add(1)
		assertAcceptanceDraftInstructions(t, req.Prompt)
		text, err := readTool(ctx, tools, "before.json")
		if err != nil {
			return nil, err
		}
		var after trace.ProjectSpec
		if err := json.Unmarshal([]byte(text), &after); err != nil {
			return nil, err
		}
		after.Slices[0].Spec = amendedSpec
		_, err = callTool(ctx, tools, deltaDraftTool, toolMap(after))
		return questionResult(req, "session-delta-runner", "Checkpoint delta delivered"), err
	}
	f.engine.Mu.Unlock()
	wt, g, _, close, err := f.s.changeContext(ctx, string(stream))
	must(t, err)
	defer close()
	must(t, f.s.advanceChanges(ctx, wt, g))
	must(t, f.s.advanceChanges(ctx, wt, g))
	if calls.Load() != 0 {
		t.Fatal("scheduling ran a model")
	}
	ops, err := repo.Operations(stream)
	must(t, err)
	op := slices.DeleteFunc(ops, func(o trace.OperationRecord) bool { return o.Operation.Action != deltaTurnAction })
	if len(op) != 1 || op[0].Result != nil || !concurrentOperation(op[0].Operation) {
		t.Fatal("delta scheduling did not publish exactly one concurrent runner intent")
	}
	runner := deltaRunner{f.s}
	observed, err := runner.Inspect(ctx, op[0].Operation)
	must(t, err)
	if observed.State != "absent" {
		t.Fatalf("unstarted delta: %+v", observed)
	}
	_, err = runner.Apply(ctx, op[0].Operation)
	must(t, err)
	_, err = runner.Apply(ctx, op[0].Operation)
	must(t, err)
	if calls.Load() != 1 || currentChange(t, f.s, stream, c.ID).State != "debating" {
		t.Fatal("runner replay repeated a model or lost its recorded proposal")
	}
}
func TestDeltaContributionsReconcileAfterJournalAdvances(t *testing.T) {
	t.Parallel()
	f, stream, repo := newApprovedFixture(t, "delta-contributions")
	defer repo.Close()
	f.s.setSole(runtimeFor(repo))
	ctx := context.Background()
	_, err := trace.EnsureWorkgroups(ctx, repo, []config.WorkstreamID{stream}, f.s.now(), serviceActor, "fixture")
	must(t, err)
	c, api := f.s.requestChange(ctx, string(stream), ChangeRequest{Key: "replayed-delta", Kind: "amend", Scope: "workgroup", Change: "Resume from checkpoints"})
	if api != nil {
		t.Fatal(api)
	}
	c.State = "debating"
	must(t, f.s.saveChange(ctx, repo, &c))
	review := f.s.deltaTool(repo, c, committeeAgent(1), deltaReviewTool)
	_, err = review.Handle(ctx, json.RawMessage(`{"objections":[],"note":"The delta is consistent"}`))
	must(t, err)
	c = currentChange(t, f.s, stream, c.ID)
	c.State = "answering"
	must(t, f.s.saveChange(ctx, repo, &c))
	// Recover a model turn after its tool write, once the scheduler has
	// already advanced to the architect's answer. No second review is filed.
	_, err = review.Handle(ctx, json.RawMessage(`{"objections":[],"note":"The delta is consistent"}`))
	must(t, err)
	answer := f.s.deltaTool(repo, c, architectAgent, deltaReplyTool)
	_, err = answer.Handle(ctx, json.RawMessage(`{"reply":"Unchanged sections remain fixed"}`))
	must(t, err)
	c = currentChange(t, f.s, stream, c.ID)
	c.State = "presented"
	must(t, f.s.saveChange(ctx, repo, &c))
	_, err = answer.Handle(ctx, json.RawMessage(`{"reply":"Unchanged sections remain fixed"}`))
	must(t, err)
	latest := currentChange(t, f.s, stream, c.ID)
	if latest.Revision != c.Revision || len(latest.Debate) != 1 || latest.Reply != c.Reply || latest.State != "presented" {
		t.Fatal("turn reconciliation duplicated contributions or changed the presented decision")
	}
}

// This fixture starts from real design turns, exactly one production start,
// the architect's actual delivery tools, and one group ratification. No test
// helper creates the members, shared spec or conversation to be amended.
func TestAmendOnProductionContinuedDesignThread(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newProductionDesignFixture(t)
	defer f.stop(t)
	// Include a repo-local criterion in each slice so the actual service
	// amendment must distinguish contract rework from unaffected landed units.
	f.engine.Mu.Lock()
	f.engine.Turns["draft-1-1"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		spec, graph := crossSpec1, crossPlan1
		number := 3
		if strings.Contains(req.Workspace.Directory(), string(f.stream2)) {
			spec, graph, number = crossSpec2, crossPlan2, 2
		}
		spec += fmt.Sprintf("%d. Local diagnostics remain available.\n", number)
		p, err := plan.Parse([]byte(graph))
		if err != nil {
			return nil, err
		}
		p.Units = append(p.Units, plan.Unit{ID: "local", Task: "Keep local diagnostics", Acceptance: []string{"Local diagnostics remain available"}, Criteria: []string{plan.Cite(number)}, DependsOn: []string{}, Footprint: []string{"internal.trace"}})
		p.Units = append(p.Units, plan.Unit{ID: "idle", Task: "Complete unrelated diagnostics", Acceptance: []string{"Additional local diagnostics available"}, Criteria: []string{plan.Cite(number)}, DependsOn: []string{}, Footprint: []string{"internal.trace"}})
		data, err := plan.Encode(p)
		if err != nil {
			return nil, err
		}
		for path, text := range map[string]string{"spec.md": spec, "plan.json": string(data)} {
			if _, err := callTool(ctx, tools, DraftTool, map[string]any{"path": path, "content": text}); err != nil {
				return nil, err
			}
		}
		return questionResult(req, "session-draft", "Draft delivered"), nil
	}
	f.engine.Mu.Unlock()
	f.startOwner(t)
	packet := f.awaitPacket(t)
	_, err := f.c.Ratify(ctx, f.origin, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan, packet.ProjectSpecRevision)
	must(t, err)
	f.awaitApproved(t)
	f.wait(t, "members initialized", func() bool {
		for i, id := range []config.WorkstreamID{f.stream1, f.stream2} {
			r, err := f.s.repository(f.repos[i].ID)
			must(t, err)
			st, err := r.Workflow(id, trace.UnitSubject("local"))
			must(t, err)
			if st.Value == "" {
				return false
			}
		}
		return true
	})
	keptStates := map[config.WorkstreamID]trace.WorkflowState{}
	keptTips := map[config.WorkstreamID]string{}
	for i, id := range []config.WorkstreamID{f.stream1, f.stream2} {
		r, err := f.s.repository(f.repos[i].ID)
		must(t, err)
		sealed, _, _, err := seal.Latest(r, id)
		must(t, err)
		for _, unit := range []string{"local", "contract"} {
			must(t, os.WriteFile(filepath.Join(sealed.Workspace, unit+".go"), []byte("package demo\n// "+unit+" is complete.\n"), 0600))
			demoGit(t, sealed.Workspace, "-C", sealed.Workspace, "add", unit+".go")
			demoGit(t, sealed.Workspace, "-C", sealed.Workspace, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-qm", "Complete "+unit)
		}
		recordUnitMerged(t, r, id, "local")
		contract := "accept"
		if i == 1 {
			contract = "resume"
		}
		recordUnitMerged(t, r, id, contract)
		keptStates[id], err = r.Workflow(id, trace.UnitSubject("local"))
		must(t, err)
		keptTips[id] = strings.TrimSpace(demoGit(t, f.repos[i].Clone, "-C", f.repos[i].Clone, "rev-parse", featureBranch(id)))
	}
	const key = "offset-delta"
	var approvedRevision atomic.Int64
	var approvals atomic.Int32
	f.engine.Mu.Lock()
	f.engine.Turns["*"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		names, err := toolNames(ctx, tools)
		if err != nil {
			return nil, err
		}
		if slices.Contains(names, deltaDraftTool) {
			text, err := readTool(ctx, tools, "before.json")
			if err != nil {
				return nil, err
			}
			var proposed trace.ProjectSpec
			if err := json.Unmarshal([]byte(text), &proposed); err != nil {
				return nil, err
			}
			if len(proposed.Slices) != 2 || len(proposed.Contracts) == 0 {
				return nil, os.ErrInvalid
			}
			proposed.Contracts[0].Text += " Offset is encoded as a string."
			proposed.Slices[0].Spec = strings.Replace(proposed.Slices[0].Spec, "names the chunk's offset", "names the chunk's offset as a string", 1)
			proposed.Slices[0].Spec += "4. Offset strings are validated.\n"
			p, err := plan.Parse([]byte(proposed.Slices[0].Plan))
			if err != nil {
				return nil, err
			}
			p.Units = append(p.Units, plan.Unit{ID: "validate", Task: "Validate offset strings", Acceptance: []string{"Invalid strings refused"}, Criteria: []string{"spec#4"}, DependsOn: []string{"accept"}, Footprint: []string{"internal.trace"}})
			data, err := plan.Encode(p)
			if err != nil {
				return nil, err
			}
			proposed.Slices[0].Plan = string(data)
			_, err = callTool(ctx, tools, deltaDraftTool, toolMap(proposed))
			return questionResult(req, "session-delta", "Delivered only the offset delta."), err
		}
		if slices.Contains(names, deltaReviewTool) {
			text, err := readTool(ctx, tools, "change.json")
			if err != nil {
				return nil, err
			}
			if !strings.Contains(text, "Offset is encoded as a string") || !strings.Contains(req.Prompt, "only the requested delta") {
				return nil, os.ErrInvalid
			}
			_, err = callTool(ctx, tools, deltaReviewTool, map[string]any{"objections": []string{}, "note": "The offset delta is consistent; local sections stay fixed."})
			return questionResult(req, "session-delta-review", "Reviewed the delta."), err
		}
		if slices.Contains(names, deltaReplyTool) {
			_, err = callTool(ctx, tools, deltaReplyTool, map[string]any{"reply": "The unchanged local sections retain their implementation."})
			return questionResult(req, "session-delta-answer", "Answered the delta debate."), err
		}
		if strings.Contains(req.Prompt, "Approve the offset impact") {
			approvals.Add(1)
			_, err = callTool(ctx, tools, decideChangeTool, map[string]any{"id": key, "revision": int(approvedRevision.Load()), "decision": "approve"})
			return questionResult(req, "session-owner-approval", "The approved impact will be applied."), err
		}
		if strings.Contains(req.Prompt, "Amend the offset contract to string") {
			_, err = callTool(ctx, tools, requestChangeTool, toolMap(ChangeRequest{Key: key, Kind: "amend", Scope: "workgroup", Change: "Offset is encoded as a string."}))
			return questionResult(req, "session-owner-amend", "The committee will debate the delta."), err
		}
		return questionResult(req, "session-chief", "Following the approved workgroup."), nil
	}
	f.engine.Mu.Unlock()
	_, err = f.c.Send(ctx, f.origin, "Amend the offset contract to string.")
	must(t, err)
	f.wait(t, "one impact summary in continued thread", func() bool { return currentChange(t, f.s, f.origin, key).State == "presented" })
	c := currentChange(t, f.s, f.origin, key)
	if len(c.Debate) != 2 || c.Reply == "" || !c.Impact.Streams[1].Cascade {
		t.Fatalf("delta debate or cascade missing: %+v", c)
	}
	for _, member := range []config.WorkstreamID{f.origin, f.stream1, f.stream2} {
		conversation, err := f.c.Conversation(ctx, member)
		must(t, err)
		count := 0
		for _, entry := range conversation.Entries {
			if entry.Turn == "change-"+key+"-impact" {
				count++
				if !strings.Contains(entry.Text, "Impact summary") {
					t.Fatal("impact absent from chat")
				}
			}
		}
		if count != 1 {
			t.Fatalf("member %s received %d prompts", member, count)
		}
	}
	f.checkContinuation(t)
	f.stop(t)
	f.start(t)
	c = currentChange(t, f.s, f.origin, key)
	if c.State != "presented" {
		t.Fatal("restart lost presented impact")
	}
	approvedRevision.Store(int64(c.Revision))
	_, err = f.c.Send(ctx, f.origin, "Approve the offset impact summary.")
	must(t, err)
	f.wait(t, "approved delta applied", func() bool { return currentChange(t, f.s, f.origin, key).State == "applied" })
	c = currentChange(t, f.s, f.origin, key)
	for i, member := range []config.WorkstreamID{f.stream1, f.stream2} {
		repo, err := f.s.repository(f.repos[i].ID)
		must(t, err)
		spec, _, found, err := repo.ProjectSpecOf(member)
		must(t, err)
		if !found || !strings.Contains(spec.Contracts[0].Text, "encoded as a string") {
			t.Fatal("amended contract did not become authoritative")
		}
		st, err := repo.Workflow(member, trace.UnitSubject("local"))
		must(t, err)
		if st != keptStates[member] {
			t.Fatal("unaffected unit lost its state/version")
		}
		tip := strings.TrimSpace(demoGit(t, f.repos[i].Clone, "-C", f.repos[i].Clone, "rev-parse", featureBranch(member)))
		if tip != keptTips[member] {
			t.Fatal("amendment changed landed commits")
		}
		latest, _, _, err := seal.Latest(repo, member)
		must(t, err)
		graph, err := sealedPlan(repo, member, latest.Revision.Plan)
		must(t, err)
		p, err := plan.Parse([]byte(graph.Content))
		must(t, err)
		contract := "accept"
		if i == 1 {
			contract = "resume"
		}
		if _, ok := p.Unit(amendment.RevisionID(key, contract)); !ok {
			t.Fatal("affected completed unit did not reopen")
		}
		if i == 0 {
			u, ok := p.Unit("validate")
			if !ok || !slices.Contains(u.DependsOn, amendment.RevisionID(key, "accept")) {
				t.Fatal("new unit bypasses reopened dependency")
			}
		}
		if held, err := changeHeld(repo, member); err != nil || held {
			t.Fatalf("member barrier remained: %v", err)
		}
	}
	if c.Decision == nil || c.Decision.Decision != "approve" {
		t.Fatal("owner decision missing")
	}
	if approvals.Load() != 1 {
		t.Fatal("amendment required repeated owner approval")
	}
	f.checkRepos(t)
}
func approvedCancellation(t *testing.T, f *shedFixture, repo *trace.Repository, stream config.WorkstreamID, scope, choice string) WorkgroupChange {
	t.Helper()
	ctx := context.Background()
	f.s.setSole(&activeProject{id: repo.Project(), repository: repo})
	// SeedBuild fixtures model landed branches, so establish their ordinary
	// one-member grouping just as startup does for non-session workstreams.
	_, err := trace.EnsureWorkgroups(ctx, repo, []config.WorkstreamID{stream}, f.s.now(), serviceActor, "fixture")
	must(t, err)
	_, err = repo.EnsureChiefOfStaff(ctx, stream, f.s.now(), serviceActor)
	must(t, err)
	c, api := f.s.requestChange(ctx, string(stream), ChangeRequest{Key: "cancel", Kind: "cancel", Scope: scope, Change: "Superseded by owner"})
	if api != nil {
		t.Fatal(api)
	}
	wt, g, _, close, err := f.s.changeContext(ctx, string(stream))
	must(t, err)
	defer close()
	must(t, f.s.advanceChanges(ctx, wt, g))
	c = currentChange(t, f.s, stream, c.ID)
	if c.State != "presented" {
		t.Fatalf("cancel not presented: %+v", c)
	}
	if _, api := f.s.decideChange(ctx, string(stream), c.ID, ChangeDecision{Revision: c.Revision, Decision: "approve"}); api == nil {
		t.Fatal("cancel accepted without PR disposition")
	}
	c, api = f.s.decideChange(ctx, string(stream), c.ID, ChangeDecision{Revision: c.Revision, Decision: "approve", PullRequests: choice})
	if api != nil {
		t.Fatal(api)
	}
	return c
}
func TestCancelPreservesUnlandedUnitBranches(t *testing.T) {
	t.Parallel()
	for _, backend := range []string{config.WorkspacesGit, config.WorkspacesJujutsu} {
		for _, scope := range []string{"workstream", "workgroup"} {
			for _, state := range []string{UnitImplementing, UnitApproved} {
				t.Run(backend+scope+state, func(t *testing.T) {
					f, stream, repo := newCleanupFixture(t, backend)
					defer repo.Close()
					ctx := context.Background()
					for _, id := range []string{"upload", "audit"} {
						completeMasonTurn(t, f, repo, stream, id, "built "+id)
						if state == UnitApproved {
							st, err := repo.Workflow(stream, trace.UnitSubject(id))
							must(t, err)
							_, err = repo.Transact(ctx, trace.Transaction{ExpectedVersion: st.Version, Transition: trace.Transition{Header: trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id + "-approve", Revision: 1, Project: repo.Project(), Workstream: stream, Unit: id, At: f.s.now(), Actor: reviewerActor, Cause: "fixture"}, Subject: trace.UnitSubject(id), From: st.Value, To: UnitApproved, Reason: "approved but unlanded"}})
							must(t, err)
						}
					}
					tip := strings.TrimSpace(demoGit(t, f.clone, "-C", f.clone, "rev-parse", featureBranch(stream)))
					approvedCancellation(t, f, repo, stream, scope, "draft")
					wt, g, _, close, err := f.s.changeContext(ctx, string(stream))
					must(t, err)
					must(t, f.s.advanceChanges(ctx, wt, g))
					close()
					if c := currentChange(t, f.s, stream, "cancel"); c.State != "applied" {
						t.Fatalf("cancel %+v", c)
					}
					cleanup := newCleanup(f, repo)
					must(t, cleanup.Pass(ctx))
					if len(cleanup.failures) > 0 {
						t.Fatalf("cleanup failures %v", cleanup.failures)
					}
					for _, id := range []string{"upload", "audit"} {
						if got := branchFile(t, f, repo, stream, unitBranch(stream, id), id+".txt"); got != id+"\n" {
							t.Fatalf("lost unit work: %q", got)
						}
					}
					after := strings.TrimSpace(demoGit(t, f.clone, "-C", f.clone, "rev-parse", featureBranch(stream)))
					if after != tip {
						t.Fatal("cancel landed unapproved or partial unit code")
					}
				})
			}
		}
	}
}

type cancelPulls struct {
	*fakePulls
	closes, drafts     int
	mergeOnDisposition bool
}

func (p *cancelPulls) Close(_ context.Context, _ string, n int) (pulls.PullRequest, error) {
	p.Mu.Lock()
	defer p.Mu.Unlock()
	for i := range p.Prs {
		if p.Prs[i].Number == n {
			if p.mergeOnDisposition {
				p.Prs[i].Merged = true
				return p.Prs[i], nil
			}
			p.closes++
			if !p.Prs[i].Merged {
				p.Prs[i].State = "closed"
			}
			return p.Prs[i], nil
		}
	}
	return pulls.PullRequest{}, os.ErrNotExist
}
func (p *cancelPulls) Draft(_ context.Context, _ string, n int) (pulls.PullRequest, error) {
	p.Mu.Lock()
	defer p.Mu.Unlock()
	for i := range p.Prs {
		if p.Prs[i].Number == n {
			if p.mergeOnDisposition {
				p.Prs[i].Merged = true
				return p.Prs[i], nil
			}
			p.drafts++
			if !p.Prs[i].Merged {
				p.Prs[i].Draft = true
			}
			return p.Prs[i], nil
		}
	}
	return pulls.PullRequest{}, os.ErrNotExist
}
func TestCancelKeepsPullRequestThatMergesDuringDisposition(t *testing.T) {
	t.Parallel()
	for _, choice := range []string{"close", "draft"} {
		t.Run(choice, func(t *testing.T) {
			f, stream, repo := newApprovedFixture(t, "cancel-merge-race")
			defer repo.Close()
			ctx := context.Background()
			cfg := f.s.about(repo)
			client := &cancelPulls{fakePulls: &fakePulls{Prs: []pulls.PullRequest{{Number: 7, State: "open", Head: featureBranch(stream), HeadRepository: cfg.Project.PushRepository(), URL: "https://example.invalid/pull/7"}}}, mergeOnDisposition: true}
			f.s.options.PullRequests = client
			must(t, (&publisher{s: f.s, repository: repo}).recordPublication(ctx, stream, DeliveryPublication{Operation: "fixture-publication", Status: publicationOpened, PullRequest: 7, URL: client.Prs[0].URL, Upstream: cfg.Project.Upstream, Fork: cfg.Project.PushRepository(), Branch: featureBranch(stream)}))
			before, err := repo.Workflow(stream, trace.FeatureSubject)
			must(t, err)
			c := WorkgroupChange{ID: "cancel-merge-race", Request: ChangeRequest{Kind: "cancel", Change: "Cancel"}, Decision: &ChangeDecision{Decision: "approve", PullRequests: choice}}
			must(t, f.s.cancelChangeMember(ctx, repo, stream, c))
			after, err := repo.Workflow(stream, trace.FeatureSubject)
			must(t, err)
			if before != after || client.Prs[0].State != "open" || client.Prs[0].Draft {
				t.Fatal("a merge observed during PR disposition was cancelled")
			}
		})
	}
}
func TestCancelOpenPullRequestDispositionAndMergedProtection(t *testing.T) {
	t.Parallel()
	for _, choice := range []string{"close", "draft"} {
		for _, merged := range []bool{false, true} {
			t.Run(choice+map[bool]string{true: "merged", false: "open"}[merged], func(t *testing.T) {
				f, stream, repo := newApprovedFixture(t, "cancel-pr")
				defer repo.Close()
				ctx := context.Background()
				cfg := f.s.about(repo)
				branch := featureBranch(stream)
				client := &cancelPulls{fakePulls: &fakePulls{Prs: []pulls.PullRequest{{Number: 7, State: "open", Merged: merged, Head: branch, HeadRepository: cfg.Project.PushRepository(), URL: "https://example.invalid/pull/7"}}}}
				f.s.options.PullRequests = client
				p := &publisher{s: f.s, repository: repo}
				must(t, p.recordPublication(ctx, stream, DeliveryPublication{Operation: "fixture-publication", Status: publicationOpened, PullRequest: 7, URL: client.Prs[0].URL, Upstream: cfg.Project.Upstream, Fork: cfg.Project.PushRepository(), Branch: branch}))
				c := WorkgroupChange{ID: "cancel-pr", Request: ChangeRequest{Kind: "cancel", Change: "Owner cancels"}, Decision: &ChangeDecision{Decision: "approve", PullRequests: choice}}
				before, err := repo.Workflow(stream, trace.FeatureSubject)
				must(t, err)
				must(t, f.s.cancelChangeMember(ctx, repo, stream, c))
				must(t, f.s.cancelChangeMember(ctx, repo, stream, c))
				after, err := repo.Workflow(stream, trace.FeatureSubject)
				must(t, err)
				if merged {
					if after != before || client.closes != 0 || client.drafts != 0 {
						t.Fatal("merged work touched")
					}
				} else {
					if after.Value != AbandonedState {
						t.Fatal("PR-open member not cancelled")
					}
					if choice == "close" && client.Prs[0].State != "closed" || choice == "draft" && !client.Prs[0].Draft {
						t.Fatal("owner choice ignored")
					}
				}
			})
		}
	}
}
func TestDeltaRevisionsPreserveUnchangedStateAndBranch(t *testing.T) {
	t.Parallel()
	f, stream, repo := newApprovedFixture(t, "delta-state")
	defer repo.Close()
	ctx := context.Background()
	old, graph, err := memberSpecAndPlan(repo, stream)
	must(t, err)
	before := trace.ProjectSpec{Behavior: "Resume", Slices: []trace.Slice{{Workstream: stream, Spec: old, Plan: graph}}}
	after := before
	after.Slices = slices.Clone(before.Slices)
	after.Slices[0].Spec = amendedSpec
	// Establish one completed unit and leave the unaffected approved unit's
	// exact transition/version and existing feature tip intact.
	st, err := repo.Workflow(stream, trace.UnitSubject("resume"))
	must(t, err)
	_, err = repo.Transact(ctx, trace.Transaction{ExpectedVersion: st.Version, Transition: trace.Transition{Header: trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "resume-completed", Revision: 1, Project: repo.Project(), Workstream: stream, Unit: "resume", At: f.s.now(), Actor: foremanActor, Cause: "fixture"}, Subject: trace.UnitSubject("resume"), From: st.Value, To: UnitMerged, Reason: "whole unit landed"}})
	must(t, err)
	unaffected, err := repo.Workflow(stream, trace.UnitSubject("dedupe"))
	must(t, err)
	tip := strings.TrimSpace(demoGit(t, f.clone, "-C", f.clone, "rev-parse", featureBranch(stream)))
	members := []amendment.Member{{Workstream: stream, State: BuildingState, Units: map[string]string{"resume": UnitMerged, "dedupe": UnitApproved}}}
	impact, err := amendment.ClassifyImpact("delta", before, after, members)
	must(t, err)
	c := WorkgroupChange{ID: "delta", Before: before, After: after, Result: &after, Impact: impact, Request: ChangeRequest{Change: "Use checkpoints"}}
	must(t, f.s.reworkChangeMember(ctx, repo, impact.Streams[0], &c))
	must(t, f.s.reworkChangeMember(ctx, repo, impact.Streams[0], &c))
	kept, err := repo.Workflow(stream, trace.UnitSubject("dedupe"))
	must(t, err)
	if kept != unaffected {
		t.Fatal("unaffected state changed")
	}
	if got := strings.TrimSpace(demoGit(t, f.clone, "-C", f.clone, "rev-parse", featureBranch(stream))); got != tip {
		t.Fatal("amendment rewrote landed commits")
	}
	sealed, _, _, err := seal.Latest(repo, stream)
	must(t, err)
	graphDoc, err := sealedPlan(repo, stream, sealed.Revision.Plan)
	must(t, err)
	p, err := plan.Parse([]byte(graphDoc.Content))
	must(t, err)
	if _, ok := p.Unit(amendment.RevisionID("delta", "resume")); !ok {
		t.Fatal("completed changed unit did not get revision")
	}
}

// Keep an active turn's dirty files on its unit branch, never on the feature.
func TestCancelWaitsForRunningUnitBoundary(t *testing.T) {
	t.Parallel()
	f, stream, repo := newCleanupFixture(t, config.WorkspacesGit)
	defer repo.Close()
	ctx := context.Background()
	c := approvedCancellation(t, f, repo, stream, "workstream", "draft")
	wt, g, _, close, err := f.s.changeContext(ctx, string(stream))
	must(t, err)
	defer close()
	// The unit has an unfinished queued turn but no running process. A durable
	// claim models its currently executing turn, which must finish first.
	th, err := repo.Thread(stream, masonAgent("upload"))
	must(t, err)
	claimed, err := repo.ClaimTurn(ctx, stream, masonAgent("upload"), "done-"+th.Turns[0].Request.TurnID, filepath.Join(f.s.cfg.Root.String(), "threads", string(f.project), string(stream), masonAgent("upload"), "done-"+th.Turns[0].Request.TurnID), f.clock.Now())
	must(t, err)
	must(t, f.s.advanceChanges(ctx, wt, g))
	st, err := repo.Workflow(stream, trace.FeatureSubject)
	must(t, err)
	if st.Value == AbandonedState {
		t.Fatal("cancel crossed a running unit boundary")
	}
	held, err := changeHeld(repo, stream)
	must(t, err)
	if !held {
		t.Fatal("no admission barrier")
	}
	content, err := json.Marshal(MasonReport{Outcome: "built upload"})
	must(t, err)
	h := claimed.Request.Header
	h.Schema, h.ID, h.At = "osmia.trace.turn-response", trace.EventID(claimed.Request.ID, "response"), f.clock.Now()
	response := trace.TurnResponse{Header: h, AgentID: masonAgent("upload"), ThreadID: masonAgent("upload"), TurnID: claimed.Request.TurnID, RequestID: claimed.Request.ID, RequestRevision: claimed.Request.Revision,
		Result: coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: claimed.Request.Profile.Backend, ID: claimed.Claim.Token}, SessionDirectory: claimed.Claim.SessionDirectory, StartedAt: claimed.Claim.At, FinalResponse: "Built", Outcome: &coreadapter.Outcome{Status: masonDone, Report: string(content), Card: &exampleCard}}}
	must(t, repo.CaptureTurn(ctx, claimed.Claim.Token, response))
	must(t, repo.CompleteTurn(ctx, stream, masonAgent("upload"), claimed.Request.TurnID, claimed.Claim.Token, f.clock.Now()))
	// Other unit branches may be queued; cancellation preserves those too.
	must(t, f.s.advanceChanges(ctx, wt, g))
	if currentChange(t, f.s, stream, c.ID).State != "applied" {
		t.Fatal("boundary completion did not resume cancellation")
	}
	if _, err := os.Stat(filepath.Join(f.clone, "upload.txt")); !os.IsNotExist(err) {
		t.Fatal("partial code reached clone checkout")
	}
}
func toolMap(v any) map[string]any {
	data, _ := json.Marshal(v)
	var out map[string]any
	_ = json.Unmarshal(data, &out)
	return out
}
