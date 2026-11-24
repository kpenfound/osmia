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
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/pulls"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func currentChange(t *testing.T, s *Service, address config.WorkstreamID, id string) lifecycle.WorkgroupChange {
	t.Helper()
	wt, g, _, close, err := s.changeContext(context.Background(), string(address))
	fixturekit.Must(t, err)
	defer close()
	changes, err := lifecycle.ReadChanges(wt, g.Members[0])
	fixturekit.Must(t, err)
	for _, c := range changes {
		if c.ID == id {
			return c
		}
	}
	return lifecycle.WorkgroupChange{}
}
func TestAStalledChangeEndsOnlyAtThePinnedRevision(t *testing.T) {
	t.Parallel()
	f, stream, repo := newApprovedFixture(t, "stalled-change")
	defer repo.Close()
	f.s.setSole(runtimeFor(repo))
	ctx := context.Background()
	_, err := trace.EnsureWorkgroups(ctx, repo, []config.WorkstreamID{stream}, f.s.now(), intake.ServiceActor, "fixture")
	fixturekit.Must(t, err)
	_, err = repo.EnsureChiefOfStaff(ctx, stream, f.s.now(), intake.ServiceActor)
	fixturekit.Must(t, err)
	c, apiFault := f.s.requestChange(ctx, string(stream), lifecycle.ChangeRequest{Key: "stalled-delta", Kind: "amend", Scope: "workgroup", Change: "Resume from checkpoints"})
	if apiFault != nil {
		t.Fatal(apiFault)
	}
	c = currentChange(t, f.s, stream, c.ID)
	if c.State != "drafting" {
		t.Fatalf("the requested change is %s", c.State)
	}
	if apiFault := f.s.endStalledChange(ctx, stream, c.ID, "debating", uint64(c.Revision), "stalled"); apiFault == nil || apiFault.Code != api.Conflict {
		t.Fatalf("ending the change at another state = %+v", apiFault)
	}
	if apiFault := f.s.endStalledChange(ctx, stream, c.ID, "drafting", uint64(c.Revision)+1, "stalled"); apiFault == nil || apiFault.Code != api.Conflict {
		t.Fatalf("ending the change at another revision = %+v", apiFault)
	}
	if got := currentChange(t, f.s, stream, c.ID); got.State != "drafting" || got.Revision != c.Revision {
		t.Fatalf("a refused end changed the change: %+v", got)
	}
	if apiFault := f.s.endStalledChange(ctx, stream, c.ID, "drafting", uint64(c.Revision), "ended on its recovery card"); apiFault != nil {
		t.Fatal(apiFault)
	}
	if got := currentChange(t, f.s, stream, c.ID); got.State != "failed" || got.Failure != "ended on its recovery card" {
		t.Fatalf("the ended change is %+v", got)
	}
	if _, apiFault := f.s.requestChange(ctx, string(stream), lifecycle.ChangeRequest{Key: "after-stall", Kind: "amend", Scope: "workgroup", Change: "Resume from checkpoints"}); apiFault != nil {
		t.Fatalf("a new change after the ended one: %+v", apiFault)
	}
}

func TestAMoveRefusedByAChangeHoldNamesTheChange(t *testing.T) {
	t.Parallel()
	f, stream, repo := newApprovedFixture(t, "change-hold-move")
	defer repo.Close()
	f.s.setSole(runtimeFor(repo))
	ctx := context.Background()
	_, err := trace.EnsureWorkgroups(ctx, repo, []config.WorkstreamID{stream}, f.s.now(), intake.ServiceActor, "fixture")
	fixturekit.Must(t, err)
	c, apiFault := f.s.requestChange(ctx, string(stream), lifecycle.ChangeRequest{Key: "held-delta", Kind: "amend", Scope: "workgroup", Change: "Resume from checkpoints"})
	if apiFault != nil {
		t.Fatal(apiFault)
	}
	c = currentChange(t, f.s, stream, c.ID)
	c.State, c.Failure = "approved", fmt.Sprintf("%s: workstream %s", trace.ErrMemberRetired, stream)
	fixturekit.Must(t, f.s.saveChange(ctx, repo, &c))
	st, err := repo.Workflow(stream, lifecycle.ChangeBoundarySubject)
	fixturekit.Must(t, err)
	_, err = repo.Transact(ctx, trace.Transaction{ExpectedVersion: st.Version, Transition: trace.Transition{
		Header:  trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "change-" + c.ID + "-boundary-false", Revision: 1, Project: repo.Project(), Workstream: stream, At: f.s.now(), Actor: intake.ServiceActor, Cause: "fixture"},
		Subject: lifecycle.ChangeBoundarySubject, From: st.Value, To: c.ID, Reason: "owner-approved change stops at a unit boundary",
	}})
	fixturekit.Must(t, err)
	_, apiFault = f.s.ownerMoveWorkstream(ctx, string(stream), api.WorkstreamMoveRequest{To: intake.InShedState, Note: "Debate it again."})
	for _, text := range []string{"workgroup change " + c.ID, "is approved", trace.ErrMemberRetired.Error(), "does not resume by itself", "the owner"} {
		if apiFault == nil || apiFault.Code != api.Conflict || !strings.Contains(apiFault.Message, text) {
			t.Fatalf("change-hold refusal lacks %q: %v", text, apiFault)
		}
	}
	for state, lever := range map[string]string{"drafting": "retry_change with id " + c.ID, "presented": "decide_change with id " + c.ID, "applying": "applies it on its next pass"} {
		held := c
		held.State = state
		if state != "drafting" {
			held.Failure = ""
		}
		if message := lifecycle.ChangeHoldMessage(held); !strings.Contains(message, "is "+state) || !strings.Contains(message, lever) {
			t.Fatalf("a %s change's hold reads %q", state, message)
		}
	}
}

func TestDeltaSchedulingRecordsRunnerIntentBeforeExecution(t *testing.T) {
	t.Parallel()
	f, stream, repo := newApprovedFixture(t, "delta-runner")
	defer repo.Close()
	f.s.setSole(runtimeFor(repo))
	ctx := context.Background()
	_, err := trace.EnsureWorkgroups(ctx, repo, []config.WorkstreamID{stream}, f.s.now(), intake.ServiceActor, "fixture")
	fixturekit.Must(t, err)
	_, err = repo.EnsureChiefOfStaff(ctx, stream, f.s.now(), intake.ServiceActor)
	fixturekit.Must(t, err)
	c, apiFault := f.s.requestChange(ctx, string(stream), lifecycle.ChangeRequest{Key: "queued-delta", Kind: "amend", Scope: "workgroup", Change: "Resume from checkpoints"})
	if apiFault != nil {
		t.Fatal(apiFault)
	}
	var calls atomic.Int32
	f.engine.Mu.Lock()
	f.engine.Turns["*"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		calls.Add(1)
		assertAcceptanceDraftInstructions(t, req.Prompt)
		text, err := intakefixture.ReadTool(ctx, tools, "before.json")
		if err != nil {
			return nil, err
		}
		var after trace.ProjectSpec
		if err := json.Unmarshal([]byte(text), &after); err != nil {
			return nil, err
		}
		after.Slices[0].Spec = amendedSpec
		_, err = fixturekit.CallTool(ctx, tools, lifecycle.DeltaDraftTool, toolMap(after))
		return intakefixture.QuestionResult(req, "session-delta-runner", "Checkpoint delta delivered"), err
	}
	f.engine.Mu.Unlock()
	wt, g, _, close, err := f.s.changeContext(ctx, string(stream))
	fixturekit.Must(t, err)
	defer close()
	fixturekit.Must(t, f.s.advanceChanges(ctx, wt, g))
	fixturekit.Must(t, f.s.advanceChanges(ctx, wt, g))
	if calls.Load() != 0 {
		t.Fatal("scheduling ran a model")
	}
	ops, err := repo.Operations(stream)
	fixturekit.Must(t, err)
	op := slices.DeleteFunc(ops, func(o trace.OperationRecord) bool { return o.Operation.Action != lifecycle.DeltaTurnAction })
	if len(op) != 1 || op[0].Result != nil || !service.TestConcurrentOperation(op[0].Operation) {
		t.Fatal("delta scheduling did not publish exactly one concurrent runner intent")
	}
	runner := deltaRunner{f.s}
	observed, err := runner.Inspect(ctx, op[0].Operation)
	fixturekit.Must(t, err)
	if observed.State != "absent" {
		t.Fatalf("unstarted delta: %+v", observed)
	}
	_, err = runner.Apply(ctx, op[0].Operation)
	fixturekit.Must(t, err)
	_, err = runner.Apply(ctx, op[0].Operation)
	fixturekit.Must(t, err)
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
	_, err := trace.EnsureWorkgroups(ctx, repo, []config.WorkstreamID{stream}, f.s.now(), intake.ServiceActor, "fixture")
	fixturekit.Must(t, err)
	c, apiFault := f.s.requestChange(ctx, string(stream), lifecycle.ChangeRequest{Key: "replayed-delta", Kind: "amend", Scope: "workgroup", Change: "Resume from checkpoints"})
	if apiFault != nil {
		t.Fatal(apiFault)
	}
	c.State = "debating"
	fixturekit.Must(t, f.s.saveChange(ctx, repo, &c))
	review := f.s.deltaTool(repo, c, intake.CommitteeAgent(1), lifecycle.DeltaReviewTool)
	_, err = review.Handle(ctx, json.RawMessage(`{"objections":[],"note":"The delta is consistent"}`))
	fixturekit.Must(t, err)
	c = currentChange(t, f.s, stream, c.ID)
	c.State = "answering"
	fixturekit.Must(t, f.s.saveChange(ctx, repo, &c))
	_, err = review.Handle(ctx, json.RawMessage(`{"objections":[],"note":"The delta is consistent"}`))
	fixturekit.Must(t, err)
	answer := f.s.deltaTool(repo, c, intake.ArchitectAgent, lifecycle.DeltaReplyTool)
	_, err = answer.Handle(ctx, json.RawMessage(`{"reply":"Unchanged sections remain fixed"}`))
	fixturekit.Must(t, err)
	c = currentChange(t, f.s, stream, c.ID)
	c.State = "presented"
	fixturekit.Must(t, f.s.saveChange(ctx, repo, &c))
	_, err = answer.Handle(ctx, json.RawMessage(`{"reply":"Unchanged sections remain fixed"}`))
	fixturekit.Must(t, err)
	latest := currentChange(t, f.s, stream, c.ID)
	if latest.Revision != c.Revision || len(latest.Debate) != 1 || latest.Reply != c.Reply || latest.State != "presented" {
		t.Fatal("turn reconciliation duplicated contributions or changed the presented decision")
	}
}

func TestAmendOnProductionContinuedDesignThread(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newProductionDesignFixture(t)
	defer f.stop(t)
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
			if _, err := fixturekit.CallTool(ctx, tools, intake.DraftTool, map[string]any{"path": path, "content": text}); err != nil {
				return nil, err
			}
		}
		return intakefixture.QuestionResult(req, "session-draft", "Draft delivered"), nil
	}
	f.engine.Mu.Unlock()
	f.startOwner(t)
	packet := f.awaitPacket(t)
	_, err := f.c.Ratify(ctx, f.origin, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan, packet.ProjectSpecRevision)
	fixturekit.Must(t, err)
	f.awaitApproved(t)
	f.wait(t, "members initialized", func() bool {
		for i, id := range []config.WorkstreamID{f.stream1, f.stream2} {
			r, err := f.s.repository(f.repos[i].ID)
			fixturekit.Must(t, err)
			st, err := r.Workflow(id, trace.UnitSubject("local"))
			fixturekit.Must(t, err)
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
		fixturekit.Must(t, err)
		sealed, _, _, err := seal.Latest(r, id)
		fixturekit.Must(t, err)
		for _, unit := range []string{"local", "contract"} {
			fixturekit.Must(t, os.WriteFile(filepath.Join(sealed.Workspace, unit+".go"), []byte("package demo\n// "+unit+" is complete.\n"), 0600))
			fixturekit.DemoGit(t, sealed.Workspace, "-C", sealed.Workspace, "add", unit+".go")
			fixturekit.DemoGit(t, sealed.Workspace, "-C", sealed.Workspace, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-qm", "Complete "+unit)
		}
		recordUnitMerged(t, r, id, "local")
		contract := "accept"
		if i == 1 {
			contract = "resume"
		}
		recordUnitMerged(t, r, id, contract)
		keptStates[id], err = r.Workflow(id, trace.UnitSubject("local"))
		fixturekit.Must(t, err)
		keptTips[id] = strings.TrimSpace(fixturekit.DemoGit(t, f.repos[i].Clone, "-C", f.repos[i].Clone, "rev-parse", intake.FeatureBranch(id)))
	}
	const key = "offset-delta"
	var approvedRevision atomic.Int64
	var approvals atomic.Int32
	f.engine.Mu.Lock()
	f.engine.Turns["*"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		names, err := intakefixture.ToolNames(ctx, tools)
		if err != nil {
			return nil, err
		}
		if slices.Contains(names, lifecycle.DeltaDraftTool) {
			text, err := intakefixture.ReadTool(ctx, tools, "before.json")
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
			_, err = fixturekit.CallTool(ctx, tools, lifecycle.DeltaDraftTool, toolMap(proposed))
			return intakefixture.QuestionResult(req, "session-delta", "Delivered only the offset delta."), err
		}
		if slices.Contains(names, lifecycle.DeltaReviewTool) {
			text, err := intakefixture.ReadTool(ctx, tools, "change.json")
			if err != nil {
				return nil, err
			}
			if !strings.Contains(text, "Offset is encoded as a string") || !strings.Contains(req.Prompt, "only the requested delta") {
				return nil, os.ErrInvalid
			}
			_, err = fixturekit.CallTool(ctx, tools, lifecycle.DeltaReviewTool, map[string]any{"objections": []string{}, "note": "The offset delta is consistent; local sections stay fixed."})
			return intakefixture.QuestionResult(req, "session-delta-review", "Reviewed the delta."), err
		}
		if slices.Contains(names, lifecycle.DeltaReplyTool) {
			_, err = fixturekit.CallTool(ctx, tools, lifecycle.DeltaReplyTool, map[string]any{"reply": "The unchanged local sections retain their implementation."})
			return intakefixture.QuestionResult(req, "session-delta-answer", "Answered the delta debate."), err
		}
		ownerSaid := func(message string) bool {
			return strings.HasPrefix(req.Name, "message_") && strings.HasSuffix(strings.TrimSpace(req.Prompt), message)
		}
		if ownerSaid("Approve the offset impact summary.") {
			approvals.Add(1)
			_, err = fixturekit.CallTool(ctx, tools, lifecycle.DecideChangeTool, map[string]any{"id": key, "revision": int(approvedRevision.Load()), "decision": "approve"})
			return intakefixture.QuestionResult(req, "session-owner-approval", "The approved impact will be applied."), err
		}
		if ownerSaid("Amend the offset contract to string.") {
			_, err = fixturekit.CallTool(ctx, tools, lifecycle.RequestChangeTool, toolMap(lifecycle.ChangeRequest{Key: key, Kind: "amend", Scope: "workgroup", Change: "Offset is encoded as a string."}))
			return intakefixture.QuestionResult(req, "session-owner-amend", "The committee will debate the delta."), err
		}
		return intakefixture.QuestionResult(req, "session-chief", "Following the approved workgroup."), nil
	}
	f.engine.Mu.Unlock()
	_, err = f.c.Send(ctx, f.origin, "Amend the offset contract to string.")
	fixturekit.Must(t, err)
	f.wait(t, "one impact summary in continued thread", func() bool { return currentChange(t, f.s, f.origin, key).State == "presented" })
	c := currentChange(t, f.s, f.origin, key)
	if len(c.Debate) != 2 || c.Reply == "" || !c.Impact.Streams[1].Cascade {
		t.Fatalf("delta debate or cascade missing: %+v", c)
	}
	for _, member := range []config.WorkstreamID{f.origin, f.stream1, f.stream2} {
		conversation, err := f.c.Conversation(ctx, member)
		fixturekit.Must(t, err)
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
	fixturekit.Must(t, err)
	f.wait(t, "approved delta applied", func() bool { return currentChange(t, f.s, f.origin, key).State == "applied" })
	c = currentChange(t, f.s, f.origin, key)
	for i, member := range []config.WorkstreamID{f.stream1, f.stream2} {
		repo, err := f.s.repository(f.repos[i].ID)
		fixturekit.Must(t, err)
		spec, _, found, err := repo.ProjectSpecOf(member)
		fixturekit.Must(t, err)
		if !found || !strings.Contains(spec.Contracts[0].Text, "encoded as a string") {
			t.Fatal("amended contract did not become authoritative")
		}
		st, err := repo.Workflow(member, trace.UnitSubject("local"))
		fixturekit.Must(t, err)
		if st != keptStates[member] {
			t.Fatal("unaffected unit lost its state/version")
		}
		tip := strings.TrimSpace(fixturekit.DemoGit(t, f.repos[i].Clone, "-C", f.repos[i].Clone, "rev-parse", intake.FeatureBranch(member)))
		if tip != keptTips[member] {
			t.Fatal("amendment changed landed commits")
		}
		latest, _, _, err := seal.Latest(repo, member)
		fixturekit.Must(t, err)
		graph, err := intake.SealedPlan(repo, member, latest.Revision.Plan)
		fixturekit.Must(t, err)
		p, err := plan.Parse([]byte(graph.Content))
		fixturekit.Must(t, err)
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
		if held, err := lifecycle.ChangeHeld(repo, member); err != nil || held {
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
func approvedCancellation(t *testing.T, f *shedFixture, repo *trace.Repository, stream config.WorkstreamID, scope, choice string) lifecycle.WorkgroupChange {
	t.Helper()
	ctx := context.Background()
	f.s.setSole(&activeProject{id: repo.Project(), repository: repo})
	_, err := trace.EnsureWorkgroups(ctx, repo, []config.WorkstreamID{stream}, f.s.now(), intake.ServiceActor, "fixture")
	fixturekit.Must(t, err)
	_, err = repo.EnsureChiefOfStaff(ctx, stream, f.s.now(), intake.ServiceActor)
	fixturekit.Must(t, err)
	c, apiFault := f.s.requestChange(ctx, string(stream), lifecycle.ChangeRequest{Key: "cancel", Kind: "cancel", Scope: scope, Change: "Superseded by owner"})
	if apiFault != nil {
		t.Fatal(apiFault)
	}
	wt, g, _, close, err := f.s.changeContext(ctx, string(stream))
	fixturekit.Must(t, err)
	defer close()
	fixturekit.Must(t, f.s.advanceChanges(ctx, wt, g))
	c = currentChange(t, f.s, stream, c.ID)
	if c.State != "presented" {
		t.Fatalf("cancel not presented: %+v", c)
	}
	if _, apiFault := f.s.decideChange(ctx, string(stream), c.ID, lifecycle.ChangeDecision{Revision: c.Revision, Decision: "approve"}); apiFault == nil {
		t.Fatal("cancel accepted without PR disposition")
	}
	c, apiFault = f.s.decideChange(ctx, string(stream), c.ID, lifecycle.ChangeDecision{Revision: c.Revision, Decision: "approve", PullRequests: choice})
	if apiFault != nil {
		t.Fatal(apiFault)
	}
	return c
}
func TestCancelPreservesUnlandedUnitBranches(t *testing.T) {
	t.Parallel()
	for _, backend := range []string{config.WorkspacesGit, config.WorkspacesJujutsu} {
		for _, scope := range []string{"workstream", "workgroup"} {
			for _, state := range []string{intake.UnitImplementing, intake.UnitApproved} {
				t.Run(backend+scope+state, func(t *testing.T) {
					f, stream, repo := newCleanupFixture(t, backend)
					defer repo.Close()
					ctx := context.Background()
					for _, id := range []string{"upload", "audit"} {
						completeMasonTurn(t, f, repo, stream, id, "built "+id)
						if state == intake.UnitApproved {
							st, err := repo.Workflow(stream, trace.UnitSubject(id))
							fixturekit.Must(t, err)
							_, err = repo.Transact(ctx, trace.Transaction{ExpectedVersion: st.Version, Transition: trace.Transition{Header: trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id + "-approve", Revision: 1, Project: repo.Project(), Workstream: stream, Unit: id, At: f.s.now(), Actor: execution.ReviewerActor, Cause: "fixture"}, Subject: trace.UnitSubject(id), From: st.Value, To: intake.UnitApproved, Reason: "approved but unlanded"}})
							fixturekit.Must(t, err)
						}
					}
					tip := strings.TrimSpace(fixturekit.DemoGit(t, f.clone, "-C", f.clone, "rev-parse", intake.FeatureBranch(stream)))
					approvedCancellation(t, f, repo, stream, scope, "draft")
					wt, g, _, close, err := f.s.changeContext(ctx, string(stream))
					fixturekit.Must(t, err)
					fixturekit.Must(t, f.s.advanceChanges(ctx, wt, g))
					close()
					if c := currentChange(t, f.s, stream, "cancel"); c.State != "applied" {
						t.Fatalf("cancel %+v", c)
					}
					cleanup := newCleanup(f, repo)
					fixturekit.Must(t, cleanup.Pass(ctx))
					if len(cleanup.failures) > 0 {
						t.Fatalf("cleanup failures %v", cleanup.failures)
					}
					for _, id := range []string{"upload", "audit"} {
						if got := branchFile(t, f, repo, stream, execution.UnitBranch(stream, id), id+".txt"); got != id+"\n" {
							t.Fatalf("lost unit work: %q", got)
						}
					}
					after := strings.TrimSpace(fixturekit.DemoGit(t, f.clone, "-C", f.clone, "rev-parse", intake.FeatureBranch(stream)))
					if after != tip {
						t.Fatal("cancel landed unapproved or partial unit code")
					}
				})
			}
		}
	}
}

type cancelPulls struct {
	*fixturekit.FakePulls
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
			client := &cancelPulls{FakePulls: &fixturekit.FakePulls{Prs: []pulls.PullRequest{{Number: 7, State: "open", Head: intake.FeatureBranch(stream), HeadRepository: cfg.Project.PushRepository(), URL: "https://example.invalid/pull/7"}}}, mergeOnDisposition: true}
			f.s.options.PullRequests = client
			fixturekit.Must(t, (&publisher{s: f.s, repository: repo}).recordPublication(ctx, stream, api.DeliveryPublication{Operation: "fixture-publication", Status: delivery.PublicationOpened, PullRequest: 7, URL: client.Prs[0].URL, Upstream: cfg.Project.Upstream, Fork: cfg.Project.PushRepository(), Branch: intake.FeatureBranch(stream)}))
			before, err := repo.Workflow(stream, trace.FeatureSubject)
			fixturekit.Must(t, err)
			c := lifecycle.WorkgroupChange{ID: "cancel-merge-race", Request: lifecycle.ChangeRequest{Kind: "cancel", Change: "Cancel"}, Decision: &lifecycle.ChangeDecision{Decision: "approve", PullRequests: choice}}
			fixturekit.Must(t, f.s.cancelChangeMember(ctx, repo, stream, c))
			after, err := repo.Workflow(stream, trace.FeatureSubject)
			fixturekit.Must(t, err)
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
				branch := intake.FeatureBranch(stream)
				client := &cancelPulls{FakePulls: &fixturekit.FakePulls{Prs: []pulls.PullRequest{{Number: 7, State: "open", Merged: merged, Head: branch, HeadRepository: cfg.Project.PushRepository(), URL: "https://example.invalid/pull/7"}}}}
				f.s.options.PullRequests = client
				p := &publisher{s: f.s, repository: repo}
				fixturekit.Must(t, p.recordPublication(ctx, stream, api.DeliveryPublication{Operation: "fixture-publication", Status: delivery.PublicationOpened, PullRequest: 7, URL: client.Prs[0].URL, Upstream: cfg.Project.Upstream, Fork: cfg.Project.PushRepository(), Branch: branch}))
				c := lifecycle.WorkgroupChange{ID: "cancel-pr", Request: lifecycle.ChangeRequest{Kind: "cancel", Change: "Owner cancels"}, Decision: &lifecycle.ChangeDecision{Decision: "approve", PullRequests: choice}}
				before, err := repo.Workflow(stream, trace.FeatureSubject)
				fixturekit.Must(t, err)
				fixturekit.Must(t, f.s.cancelChangeMember(ctx, repo, stream, c))
				fixturekit.Must(t, f.s.cancelChangeMember(ctx, repo, stream, c))
				after, err := repo.Workflow(stream, trace.FeatureSubject)
				fixturekit.Must(t, err)
				if merged {
					if after != before || client.closes != 0 || client.drafts != 0 {
						t.Fatal("merged work touched")
					}
				} else {
					if after.Value != intake.AbandonedState {
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
	old, graph, err := lifecycle.MemberSpecAndPlan(repo, stream)
	fixturekit.Must(t, err)
	before := trace.ProjectSpec{Behavior: "Resume", Slices: []trace.Slice{{Workstream: stream, Spec: old, Plan: graph}}}
	after := before
	after.Slices = slices.Clone(before.Slices)
	after.Slices[0].Spec = amendedSpec
	st, err := repo.Workflow(stream, trace.UnitSubject("resume"))
	fixturekit.Must(t, err)
	_, err = repo.Transact(ctx, trace.Transaction{ExpectedVersion: st.Version, Transition: trace.Transition{Header: trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "resume-completed", Revision: 1, Project: repo.Project(), Workstream: stream, Unit: "resume", At: f.s.now(), Actor: service.TestForemanActor, Cause: "fixture"}, Subject: trace.UnitSubject("resume"), From: st.Value, To: intake.UnitMerged, Reason: "whole unit landed"}})
	fixturekit.Must(t, err)
	unaffected, err := repo.Workflow(stream, trace.UnitSubject("dedupe"))
	fixturekit.Must(t, err)
	tip := strings.TrimSpace(fixturekit.DemoGit(t, f.clone, "-C", f.clone, "rev-parse", intake.FeatureBranch(stream)))
	members := []amendment.Member{{Workstream: stream, State: intake.BuildingState, Units: map[string]string{"resume": intake.UnitMerged, "dedupe": intake.UnitApproved}}}
	impact, err := amendment.ClassifyImpact("delta", before, after, members)
	fixturekit.Must(t, err)
	c := lifecycle.WorkgroupChange{ID: "delta", Before: before, After: after, Result: &after, Impact: impact, Request: lifecycle.ChangeRequest{Change: "Use checkpoints"}}
	fixturekit.Must(t, f.s.reworkChangeMember(ctx, repo, impact.Streams[0], &c))
	fixturekit.Must(t, f.s.reworkChangeMember(ctx, repo, impact.Streams[0], &c))
	kept, err := repo.Workflow(stream, trace.UnitSubject("dedupe"))
	fixturekit.Must(t, err)
	if kept != unaffected {
		t.Fatal("unaffected state changed")
	}
	if got := strings.TrimSpace(fixturekit.DemoGit(t, f.clone, "-C", f.clone, "rev-parse", intake.FeatureBranch(stream))); got != tip {
		t.Fatal("amendment rewrote landed commits")
	}
	sealed, _, _, err := seal.Latest(repo, stream)
	fixturekit.Must(t, err)
	graphDoc, err := intake.SealedPlan(repo, stream, sealed.Revision.Plan)
	fixturekit.Must(t, err)
	p, err := plan.Parse([]byte(graphDoc.Content))
	fixturekit.Must(t, err)
	if _, ok := p.Unit(amendment.RevisionID("delta", "resume")); !ok {
		t.Fatal("completed changed unit did not get revision")
	}
}

func TestCancelWaitsForRunningUnitBoundary(t *testing.T) {
	t.Parallel()
	f, stream, repo := newCleanupFixture(t, config.WorkspacesGit)
	defer repo.Close()
	ctx := context.Background()
	c := approvedCancellation(t, f, repo, stream, "workstream", "draft")
	wt, g, _, close, err := f.s.changeContext(ctx, string(stream))
	fixturekit.Must(t, err)
	defer close()
	th, err := repo.Thread(stream, intake.MasonAgent("upload"))
	fixturekit.Must(t, err)
	claimed, err := repo.ClaimTurn(ctx, stream, intake.MasonAgent("upload"), "done-"+th.Turns[0].Request.TurnID, filepath.Join(f.s.cfg.Root.String(), "threads", string(f.project), string(stream), intake.MasonAgent("upload"), "done-"+th.Turns[0].Request.TurnID), f.clock.Now())
	fixturekit.Must(t, err)
	fixturekit.Must(t, f.s.advanceChanges(ctx, wt, g))
	st, err := repo.Workflow(stream, trace.FeatureSubject)
	fixturekit.Must(t, err)
	if st.Value == intake.AbandonedState {
		t.Fatal("cancel crossed a running unit boundary")
	}
	held, err := lifecycle.ChangeHeld(repo, stream)
	fixturekit.Must(t, err)
	if !held {
		t.Fatal("no admission barrier")
	}
	content, err := json.Marshal(service.MasonReport{Outcome: "built upload"})
	fixturekit.Must(t, err)
	h := claimed.Request.Header
	h.Schema, h.ID, h.At = "osmia.trace.turn-response", trace.EventID(claimed.Request.ID, "response"), f.clock.Now()
	response := trace.TurnResponse{Header: h, AgentID: intake.MasonAgent("upload"), ThreadID: intake.MasonAgent("upload"), TurnID: claimed.Request.TurnID, RequestID: claimed.Request.ID, RequestRevision: claimed.Request.Revision,
		Result: coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: claimed.Request.Profile.Backend, ID: claimed.Claim.Token}, SessionDirectory: claimed.Claim.SessionDirectory, StartedAt: claimed.Claim.At, FinalResponse: "Built", Outcome: &coreadapter.Outcome{Status: execution.MasonDone, Report: string(content), Card: &intakefixture.ExampleCard}}}
	fixturekit.Must(t, repo.CaptureTurn(ctx, claimed.Claim.Token, response))
	fixturekit.Must(t, repo.CompleteTurn(ctx, stream, intake.MasonAgent("upload"), claimed.Request.TurnID, claimed.Claim.Token, f.clock.Now()))
	fixturekit.Must(t, f.s.advanceChanges(ctx, wt, g))
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
