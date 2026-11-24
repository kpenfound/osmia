package lifecycle_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func haltGuardControllers(t *testing.T, f *crossRepoRatifyFixture) {
	t.Helper()
	_, projects := f.s.runtimes()
	for _, p := range projects {
		intakefixture.Must(t, f.s.halt(p))
	}
}
func runGuardDraft(t *testing.T, f *crossRepoRatifyFixture) {
	t.Helper()
	for _, project := range []config.ProjectID{f.project, f.project2} {
		err := f.s.projectSpecDraftPass(context.Background(), mustRepo(t, f.s, project))
		intakefixture.Must(t, err)
	}
	id, err := mustRepo(t, f.s, f.project).CrossRepoWorkgroupOf(f.stream1)
	intakefixture.Must(t, err)
	wt, err := trace.OpenWorkgroupAuthority(f.s.current().Root, id)
	intakefixture.Must(t, err)
	defer wt.Close()
	groups, _, err := trace.Workgroups(wt)
	intakefixture.Must(t, err)
	state, err := wt.Workflow(groups[0].Members[0], lifecycle.ProjectSpecDraftSubject)
	intakefixture.Must(t, err)
	if state.Value == "failed" {
		th, err := wt.Thread(groups[0].Members[0], intake.ArchitectAgent)
		intakefixture.Must(t, err)
		t.Fatalf("draft failed: %+v", th.Turns[len(th.Turns)-1].Response)
	}
}
func assertGuardRefusal(t *testing.T, f *crossRepoRatifyFixture, id config.WorkgroupID, n int, failure string) {
	t.Helper()
	before := refreshDocument(t, f, id)
	packet, err := f.c.Packet(context.Background(), f.stream1)
	intakefixture.Must(t, err)
	_, err = f.c.Ratify(context.Background(), f.stream1, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan, packet.ProjectSpecRevision)
	for _, text := range []string{string(id), string(f.stream1), fmt.Sprintf("criterion %d", n), "contracts are being redrafted", "read the packet again", failure} {
		if !intakefixture.Failed(err, api.Conflict) || !containsMessage(err, text) {
			t.Fatalf("guard refusal missing %q: %v", text, err)
		}
	}
	f.assertApprovalRefused(t, id, before)
}
func scriptGuardDraft(t *testing.T, f *crossRepoRatifyFixture, turn string, criterion int, spec string, calls *atomic.Int32, graph ...string) {
	t.Helper()
	f.engine.Mu.Lock()
	f.engine.Turns[turn] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		calls.Add(1)
		data, err := os.ReadFile(filepath.Join(req.Grants.Mounts[0].Path, "members", string(f.stream1), plan.SpecPath))
		if err != nil {
			return nil, err
		}
		if string(data) != spec {
			return nil, fmt.Errorf("architect saw wrong member revision: %s", data)
		}

		if len(graph) > 0 {
			data, err := os.ReadFile(filepath.Join(req.Grants.Mounts[0].Path, "members", string(f.stream1), plan.PlanPath))
			if err != nil {
				return nil, err
			}
			if string(data) != graph[0] {
				return nil, fmt.Errorf("architect saw wrong member plan revision: %s", data)
			}
		}
		_, err = intakefixture.CallTool(ctx, tools, lifecycle.ProjectSpecDraftTool, map[string]any{
			"behavior":  "Redrafted upload contract.",
			"contracts": []map[string]any{{"name": "acknowledgment", "text": "The reviewed acknowledgment."}},
			"slices": []map[string]any{
				{"workstream": string(f.stream1), "contract_criteria": []int{criterion}},
				{"workstream": string(f.stream2), "contract_criteria": []int{1}},
			},
		})
		return &agent.Result{ClaudeID: "session-" + req.Name, ResultText: "Redraft delivered", SessionDir: req.SessionDir, NumTurns: 1}, err
	}
	f.engine.Mu.Unlock()
}
func assertGuardRatifiable(t *testing.T, f *crossRepoRatifyFixture, id config.WorkgroupID, criterion int, revision int) {
	t.Helper()
	packet, err := f.c.Packet(context.Background(), f.stream1)
	intakefixture.Must(t, err)
	if packet.ProjectSpecRevision != revision {
		t.Fatalf("packet serves %d, want %d", packet.ProjectSpecRevision, revision)
	}
	slice, found := trace.SliceFor(*packet.ProjectSpec, f.stream1)
	if !found || !slices.Equal(slice.ContractCriteria, []int{criterion}) {
		t.Fatalf("architect's criteria lost: %+v", slice)
	}
	_, err = f.c.Ratify(context.Background(), f.stream1, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan, packet.ProjectSpecRevision)
	intakefixture.Must(t, err)
	decision, found, err := mustRepo(t, f.s, f.project).GroupApprovalOf(f.stream1)
	intakefixture.Must(t, err)
	if !found || decision.ProjectSpecRevision != revision || !reflect.DeepEqual(decision.ProjectSpec, *packet.ProjectSpec) || !trace.ContractCitation(decision.ProjectSpec, f.stream1, plan.Cite(criterion)) {
		t.Fatalf("approval lost displayed contract: %+v", decision)
	}
}
func TestProjectSpecContractGuardRedraftsChangedAndRemovedCriteria(t *testing.T) {
	t.Parallel()
	for _, removed := range []bool{false, true} {
		t.Run(fmt.Sprint("removed=", removed), func(t *testing.T) {
			t.Parallel()
			f, id, _ := projectSliceRefreshFixture(t)
			old := refreshDocument(t, f, id)
			spec := strings.Replace(crossSpec1, "2. The acknowledgment names the chunk's offset.", "2. The acknowledgment names the chunk's byte offset.", 1)
			criterion := 2
			if removed {
				spec = strings.Replace(crossSpec1, "2. The acknowledgment names the chunk's offset.\n", "", 1)
				reviseRefreshMember(t, f, f.stream1, plan.PlanDocument, strings.Replace(crossPlan1, `"spec#1", "spec#2"`, `"spec#1"`, 1))
				criterion = 1
			}
			reviseRefreshMember(t, f, f.stream1, plan.SpecDocument, spec)
			runRefresh(t, f)
			if got := refreshDocument(t, f, id); !reflect.DeepEqual(got, old) {
				t.Fatal("guard mechanically refreshed criterion")
			}
			assertGuardRefusal(t, f, id, 2, "")
			var calls atomic.Int32
			scriptGuardDraft(t, f, "project-spec-draft-3", criterion, spec, &calls)
			runGuardDraft(t, f)
			runGuardDraft(t, f)
			runRefresh(t, f)
			if calls.Load() != 1 {
				t.Fatalf("architect ran %d times", calls.Load())
			}
			if got := refreshDocument(t, f, id); got.Revision != old.Revision+1 {
				t.Fatalf("redraft did not append once: %d", got.Revision)
			}
			wt, err := trace.OpenWorkgroupAuthority(f.s.current().Root, id)
			intakefixture.Must(t, err)
			history, err := trace.Get[trace.Document](wt, "", trace.ProjectSpecsDocument, old.Revision)
			intakefixture.Must(t, err)
			intakefixture.Must(t, wt.Close())
			if !reflect.DeepEqual(history, old) {
				t.Fatal("redraft rewrote history")
			}
			assertGuardRatifiable(t, f, id, criterion, old.Revision+1)
			frozen := refreshDocument(t, f, id)
			reviseRefreshMember(t, f, f.stream1, plan.SpecDocument, crossSpec1)
			runRefresh(t, f)
			runGuardDraft(t, f)
			if calls.Load() != 1 || !reflect.DeepEqual(refreshDocument(t, f, id), frozen) {
				t.Fatal("approved project redrafted")
			}
		})
	}
}
func unpinnedProjectSpecDraftFixture(t *testing.T) (*crossRepoRatifyFixture, config.WorkgroupID, trace.Workgroup) {
	t.Helper()
	ctx := context.Background()
	f := newCrossRepoRatifyFixture(t)
	t.Cleanup(func() { f.stop(t) })
	for _, member := range []struct {
		project config.ProjectID
		stream  config.WorkstreamID
	}{{f.project, f.stream1}, {f.project2, f.stream2}} {
		intakefixture.Eventually(t, "member decision packet", func() bool {
			state, err := mustRepo(t, f.s, member.project).Workflow(member.stream, trace.FeatureSubject)
			intakefixture.Must(t, err)
			return state.Value == intake.InShedState
		})
	}
	haltGuardControllers(t, f)
	id, apiFault := f.s.DraftWorkgroup(ctx, []config.WorkstreamID{f.stream1, f.stream2})
	if apiFault != nil {
		t.Fatal(apiFault)
	}
	wt, err := trace.OpenWorkgroupAuthority(f.s.current().Root, id)
	intakefixture.Must(t, err)
	groups, _, err := trace.Workgroups(wt)
	intakefixture.Must(t, err)
	g := groups[0]
	host := g.Members[0]
	_, err = wt.Transact(ctx, trace.Transaction{Transition: trace.Transition{
		Header:  trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "project-spec-draft-1", Revision: 1, Project: wt.Project(), Workstream: host, At: f.s.now(), Actor: lifecycle.ProjectSpecDraftActor, Cause: "project-spec-draft"},
		Subject: lifecycle.ProjectSpecDraftSubject, To: "drafting", Reason: "every member is sketched; the architect is asked for the project spec",
	}})
	intakefixture.Must(t, err)
	intakefixture.Must(t, wt.CreateThread(ctx, trace.Agent{Header: trace.Header{Schema: "osmia.trace.agent", Version: trace.Version, ID: intake.ArchitectAgent, Revision: 1, Project: wt.Project(), Workstream: host, At: f.s.now(), Actor: lifecycle.ProjectSpecDraftActor, Cause: "project-spec-draft"}, Role: intake.ArchitectRole, ThreadID: intake.ArchitectThread}))
	intakefixture.Must(t, f.s.enqueueProjectSpecDraft(ctx, wt, g, host, []trace.CrossRepoMember{
		{Workstream: f.stream1, Project: mustRepo(t, f.s, f.project)},
		{Workstream: f.stream2, Project: mustRepo(t, f.s, f.project2)},
	}, "project-spec-draft-1"))
	intakefixture.Must(t, wt.Close())
	return f, id, g
}
func captureUnpinnedProjectSpecTurn(t *testing.T, f *crossRepoRatifyFixture, wt *trace.Repository, host config.WorkstreamID, completed bool) {
	t.Helper()
	ctx := context.Background()
	q, err := wt.ClaimTurn(ctx, host, intake.ArchitectAgent, "unpinned-claim", "/unpinned-session", f.s.now())
	intakefixture.Must(t, err)
	h := q.Request.Header
	h.Schema, h.ID, h.At, h.Actor = "osmia.trace.turn-response", trace.EventID(q.Request.ID, "response"), f.s.now(), lifecycle.ProjectSpecDraftActor
	intakefixture.Must(t, wt.CaptureTurn(ctx, q.Claim.Token, trace.TurnResponse{Header: h, AgentID: intake.ArchitectAgent, ThreadID: intake.ArchitectThread, TurnID: q.Request.TurnID, RequestID: q.Request.ID, RequestRevision: 1,
		Result: coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: q.Request.Profile.Backend, ID: "unpinned-session"}, SessionDirectory: q.Claim.SessionDirectory, StartedAt: q.Claim.At, FinalResponse: "Unpinned project spec delivered"}}))
	if completed {
		intakefixture.Must(t, wt.CompleteTurn(ctx, host, intake.ArchitectAgent, q.Request.TurnID, q.Claim.Token, f.s.now()))
	}
}
func TestProjectSpecContractGuardReconcilesDraftWithoutInput(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, id, g := unpinnedProjectSpecDraftFixture(t)
	host := g.Members[0]
	wt, err := trace.OpenWorkgroupAuthority(f.s.current().Root, id)
	intakefixture.Must(t, err)
	captureUnpinnedProjectSpecTurn(t, f, wt, host, true)
	intakefixture.Must(t, trace.RecordProjectSpec(ctx, wt, g, trace.ProjectSpec{
		Behavior:  "Chunks are acknowledged and resumed.",
		Contracts: []trace.Contract{{Name: "acknowledgment", Text: "The acknowledgment names the stored offset."}},
		Slices: []trace.Slice{
			{Workstream: f.stream1, Spec: crossSpec1, Plan: crossPlan1, ContractCriteria: []int{2}},
			{Workstream: f.stream2, Spec: crossSpec2, Plan: crossPlan2, ContractCriteria: []int{1}},
		},
	}, f.s.now(), lifecycle.ProjectSpecDraftActor, "project-spec-draft-1"))
	old := projectSpecDocument(t, wt)
	intakefixture.Must(t, wt.Close())
	spec := strings.Replace(crossSpec1, "chunk's offset.", "chunk's byte offset.", 1)
	reviseRefreshMember(t, f, f.stream1, plan.SpecDocument, spec)
	runRefresh(t, f)
	assertGuardRefusal(t, f, id, 2, "")
	if got := refreshDocument(t, f, id); !reflect.DeepEqual(got, old) {
		t.Fatal("guard mechanically refreshed the contract")
	}

	intakefixture.Must(t, f.s.draftProjectSpecIfReady(ctx, mustRepo(t, f.s, f.project), id))
	wt, err = trace.OpenWorkgroupAuthority(f.s.current().Root, id)
	intakefixture.Must(t, err)
	state, err := wt.Workflow(host, lifecycle.ProjectSpecDraftSubject)
	intakefixture.Must(t, err)
	_, inputRevision, err := lifecycle.ReadProjectSpecDraftInput(wt, host)
	intakefixture.Must(t, err)
	intakefixture.Must(t, wt.Close())
	if state.Value != "drafted" || inputRevision != 0 || !reflect.DeepEqual(refreshDocument(t, f, id), old) {
		t.Fatalf("recorded draft reconciliation changed output or invented pins: state=%+v input=%d", state, inputRevision)
	}
	var calls atomic.Int32
	scriptGuardDraft(t, f, "project-spec-draft-3", 2, spec, &calls)
	stop := errors.New("redraft interrupted after request")
	f.s.boundary = func(step string) error {
		if step == "project-spec-draft-requested" {
			return stop
		}
		return nil
	}
	for range 2 {
		if err := f.s.draftProjectSpecIfReady(ctx, mustRepo(t, f.s, f.project), id); !errors.Is(err, stop) {
			t.Fatalf("redraft request: %v", err)
		}
	}
	assertGuardRefusal(t, f, id, 2, "")
	f.stop(t)
	f.opts.boundary = func(_ context.Context, step string) error {
		if step == "project-spec-draft-requested" {
			return stop
		}
		return nil
	}
	f.start(t)
	haltGuardControllers(t, f)
	f.s.clearBoundary()
	runGuardDraft(t, f)
	runGuardDraft(t, f)
	runRefresh(t, f)
	if calls.Load() != 1 || refreshDocument(t, f, id).Revision != old.Revision+1 {
		t.Fatalf("redraft duplicated: calls=%d revision=%d", calls.Load(), refreshDocument(t, f, id).Revision)
	}
	wt, err = trace.OpenWorkgroupAuthority(f.s.current().Root, id)
	intakefixture.Must(t, err)
	history, err := trace.Get[trace.Document](wt, "", trace.ProjectSpecsDocument, old.Revision)
	intakefixture.Must(t, err)
	_, inputRevision, err = lifecycle.ReadProjectSpecDraftInput(wt, host)
	intakefixture.Must(t, err)
	intakefixture.Must(t, wt.Close())
	if !reflect.DeepEqual(history, old) || inputRevision != 1 {
		t.Fatalf("redraft rewrote history or duplicated its request: input=%d", inputRevision)
	}
	assertGuardRatifiable(t, f, id, 2, old.Revision+1)
}
func TestProjectSpecDraftSupersedesUnpinnedOutput(t *testing.T) {
	t.Parallel()
	for _, stage := range []string{"queued", "reserved", "captured", "completed"} {
		t.Run(stage, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			f, id, g := unpinnedProjectSpecDraftFixture(t)
			host := g.Members[0]
			wt, err := trace.OpenWorkgroupAuthority(f.s.current().Root, id)
			intakefixture.Must(t, err)
			switch stage {
			case "reserved":
				_, err := wt.ClaimTurn(ctx, host, intake.ArchitectAgent, "unpinned-claim", "/unpinned-session", f.s.now())
				intakefixture.Must(t, err)
			case "captured", "completed":
				captureUnpinnedProjectSpecTurn(t, f, wt, host, stage == "completed")
			}
			intakefixture.Must(t, wt.Close())
			data, err := json.Marshal(lifecycle.ProjectSpecDraft{Behavior: "Unpinned contract", Slices: []lifecycle.ProjectSpecDraftSlice{
				{Workstream: f.stream1, ContractCriteria: []int{1}},
				{Workstream: f.stream2, ContractCriteria: []int{1}},
			}})
			intakefixture.Must(t, err)
			output := filepath.Join(f.s.projectSpecTurnDirectoryFor(host), "project-spec-draft-1", "output")
			intakefixture.Must(t, os.MkdirAll(output, 0700))
			intakefixture.Must(t, os.WriteFile(filepath.Join(output, "project-spec.json"), data, 0600))
			spec := strings.Replace(crossSpec1, "chunk's offset.", "chunk's byte offset.", 1)
			reviseRefreshMember(t, f, f.stream1, plan.SpecDocument, spec)
			var calls atomic.Int32
			scriptGuardDraft(t, f, "project-spec-draft-2", 2, spec, &calls)
			stop := errors.New("pinned replacement requested")
			f.stop(t)
			f.opts.boundary = func(_ context.Context, step string) error {
				if step == "project-spec-draft-requested" {
					return stop
				}
				return nil
			}
			f.start(t)
			haltGuardControllers(t, f)
			for range 2 {
				if err := f.s.draftProjectSpecIfReady(ctx, mustRepo(t, f.s, f.project), id); !errors.Is(err, stop) {
					t.Fatalf("pinned replacement request: %v", err)
				}
			}
			wt, err = trace.OpenWorkgroupAuthority(f.s.current().Root, id)
			intakefixture.Must(t, err)
			_, found, err := trace.ProjectSpecFor(wt, id)
			intakefixture.Must(t, err)
			input, revision, err := lifecycle.ReadProjectSpecDraftInput(wt, host)
			intakefixture.Must(t, err)
			th, err := wt.Thread(host, intake.ArchitectAgent)
			intakefixture.Must(t, err)
			intakefixture.Must(t, wt.Close())
			if found || revision != 1 || input.Turn != "project-spec-draft-2" || len(th.Turns) != 1 || th.Turns[0].CompletedAt.IsZero() {
				t.Fatalf("unpinned turn not superseded once: found=%v revision=%d input=%+v thread=%+v", found, revision, input, th)
			}
			f.s.clearBoundary()
			runGuardDraft(t, f)
			runGuardDraft(t, f)
			if calls.Load() != 1 {
				t.Fatalf("replacement ran %d times", calls.Load())
			}
			assertGuardRatifiable(t, f, id, 2, 1)
		})
	}
}
func TestProjectSpecContractGuardAllowsOtherMembersToRefresh(t *testing.T) {
	t.Parallel()
	f, id, packets := projectSliceRefreshFixture(t)
	spec := strings.Replace(crossSpec1, "2. The acknowledgment names the chunk's offset.", "2. The acknowledgment names the byte offset.", 1)
	reviseRefreshMember(t, f, f.stream1, plan.SpecDocument, spec)
	reviseRefreshMember(t, f, f.stream2, plan.SpecDocument, crossSpec2+"\nOther behavior.\n")
	runRefresh(t, f)
	packet, err := f.c.Packet(context.Background(), f.stream1)
	intakefixture.Must(t, err)
	first, _ := trace.SliceFor(*packet.ProjectSpec, f.stream1)
	second, _ := trace.SliceFor(*packet.ProjectSpec, f.stream2)
	if first.Spec != crossSpec1 || second.Spec != crossSpec2+"\nOther behavior.\n" || packet.ProjectSpecRevision != packets[0].ProjectSpecRevision+1 {
		t.Fatalf("per-member refresh: %+v", packet.ProjectSpec)
	}
	assertGuardRefusal(t, f, id, 2, "")
}
func TestProjectSpecContractGuardPinsMidRedraftEdits(t *testing.T) {
	t.Parallel()
	for _, interruption := range []string{"none", "requested", "staged", "delivered", "recorded"} {
		t.Run(interruption, func(t *testing.T) {
			t.Parallel()
			f, id, _ := projectSliceRefreshFixture(t)
			old := refreshDocument(t, f, id)
			s2 := strings.Replace(crossSpec1, "chunk's offset.", "chunk's byte offset.", 1)
			s3 := strings.Replace(crossSpec1, "chunk's offset.", "chunk's persisted byte offset.", 1)
			reviseRefreshMember(t, f, f.stream1, plan.SpecDocument, s2)
			var calls atomic.Int32
			scriptGuardDraft(t, f, "project-spec-draft-3", 2, s2, &calls, crossPlan1)
			stop := errors.New("redraft interrupted")
			injected := false
			f.s.boundary = func(step string) error {
				target := "project-spec-draft-delivered"
				if interruption != "none" {
					target = "project-spec-draft-" + interruption
				}
				if step == target && !injected {
					injected = true
					reviseRefreshMember(t, f, f.stream1, plan.SpecDocument, s3)
					reviseRefreshMember(t, f, f.stream1, plan.PlanDocument, crossPlan1+"\n")
					if interruption != "none" {
						return stop
					}
				}
				return nil
			}
			repo := mustRepo(t, f.s, f.project)
			err := f.s.draftProjectSpecIfReady(context.Background(), repo, id)
			if interruption == "none" {
				intakefixture.Must(t, err)
			} else if !errors.Is(err, stop) {
				t.Fatalf("interruption: %v", err)
			}
			f.s.clearBoundary()
			if !injected {
				t.Fatal("draft boundary not reached")
			}
			if interruption != "none" {
				f.stop(t)
				f.opts.boundary = func(_ context.Context, step string) error {
					if step == "project-spec-draft-requested" {
						return stop
					}
					return nil
				}
				f.start(t)
				haltGuardControllers(t, f)
				f.s.clearBoundary()
				if interruption != "recorded" {
					intakefixture.Must(t, f.s.draftProjectSpecIfReady(context.Background(), mustRepo(t, f.s, f.project), id))
				}
			}
			packet, err := f.c.Packet(context.Background(), f.stream1)
			intakefixture.Must(t, err)
			slice, _ := trace.SliceFor(*packet.ProjectSpec, f.stream1)
			if packet.ProjectSpecRevision != old.Revision+1 || slice.Spec != s2 || slice.Plan != crossPlan1 || calls.Load() != 1 {
				t.Fatalf("redraft attached unseen text or duplicated: rev=%d calls=%d slice=%+v", packet.ProjectSpecRevision, calls.Load(), slice)
			}
			runRefresh(t, f)
			assertGuardRefusal(t, f, id, 2, "")
			scriptGuardDraft(t, f, "project-spec-draft-5", 2, s3, &calls, crossPlan1+"\n")
			runGuardDraft(t, f)
			runGuardDraft(t, f)
			if calls.Load() != 2 {
				t.Fatalf("next guarded revision ran %d turns", calls.Load())
			}
			assertGuardRatifiable(t, f, id, 2, old.Revision+2)
		})
	}
}
func TestProjectSpecContractGuardRefreshesOrdinaryMidRedraftEdits(t *testing.T) {
	t.Parallel()
	f, id, _ := projectSliceRefreshFixture(t)
	s2 := strings.Replace(crossSpec1, "chunk's offset.", "chunk's byte offset.", 1)
	reviseRefreshMember(t, f, f.stream1, plan.SpecDocument, s2)
	var calls atomic.Int32
	scriptGuardDraft(t, f, "project-spec-draft-3", 2, s2, &calls, crossPlan1)
	f.s.boundary = func(step string) error {
		if step == "project-spec-draft-delivered" {
			assertGuardRefusal(t, f, id, 2, "")
			reviseRefreshMember(t, f, f.stream1, plan.SpecDocument, s2+"\nAdditional behavior.\n")
			reviseRefreshMember(t, f, f.stream2, plan.PlanDocument, crossPlan2+"\n")
			runRefresh(t, f)
		}
		return nil
	}
	runGuardDraft(t, f)
	f.s.clearBoundary()
	runRefresh(t, f)
	runGuardDraft(t, f)
	if calls.Load() != 1 {
		t.Fatal("ordinary edits caused another architect turn")
	}
	assertGuardRatifiable(t, f, id, 2, 4)
}
func TestProjectSpecContractGuardFailureAndRetry(t *testing.T) {
	t.Parallel()
	f, id, _ := projectSliceRefreshFixture(t)
	old := refreshDocument(t, f, id)
	spec := strings.Replace(crossSpec1, "chunk's offset.", "chunk's byte offset.", 1)
	reviseRefreshMember(t, f, f.stream1, plan.SpecDocument, spec)
	var calls atomic.Int32
	f.engine.Mu.Lock()
	f.engine.Turns["project-spec-draft-3"] = func(_ context.Context, req agent.Request, _ *agent.Turn, _ *mcp.ClientSession) (*agent.Result, error) {
		calls.Add(1)
		return &agent.Result{ClaudeID: "empty-redraft", ResultText: "No delivery", SessionDir: req.SessionDir, NumTurns: 1}, nil
	}
	f.engine.Mu.Unlock()
	intakefixture.Must(t, f.s.draftProjectSpecIfReady(context.Background(), mustRepo(t, f.s, f.project), id))
	assertGuardRefusal(t, f, id, 2, "ended without delivering")
	runRefresh(t, f)
	if !reflect.DeepEqual(refreshDocument(t, f, id), old) {
		t.Fatal("failed draft changed project spec")
	}
	scriptGuardDraft(t, f, "project-spec-draft-5", 2, spec, &calls)
	runGuardDraft(t, f)
	if calls.Load() != 2 {
		t.Fatalf("failed redraft did not retry: %d", calls.Load())
	}
	assertGuardRatifiable(t, f, id, 2, old.Revision+1)
}
func TestProjectSpecContractGuardRedraftStopsRetryingAndTheChiefAsksAgain(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, id, _ := projectSliceRefreshFixture(t)
	old := refreshDocument(t, f, id)
	spec := strings.Replace(crossSpec1, "chunk's offset.", "chunk's byte offset.", 1)
	reviseRefreshMember(t, f, f.stream1, plan.SpecDocument, spec)
	var refused atomic.Int32
	var retryPrompt atomic.Value
	for _, turn := range []string{"project-spec-draft-3", "project-spec-draft-5", "project-spec-draft-7"} {
		scriptProjectSpecTurn(f, turn, func(_ context.Context, req agent.Request, _ *mcp.ClientSession) error {
			if refused.Add(1) == 2 {
				retryPrompt.Store(req.Prompt)
			}
			return nil
		})
	}
	repo := mustRepo(t, f.s, f.project)
	for range lifecycle.ProjectSpecDraftAttempts + 2 {
		intakefixture.Must(t, f.s.draftProjectSpecIfReady(ctx, repo, id))
	}
	if refused.Load() != lifecycle.ProjectSpecDraftAttempts || !reflect.DeepEqual(refreshDocument(t, f, id), old) {
		t.Fatalf("guarded redraft did not stop after %d refusals: calls=%d", lifecycle.ProjectSpecDraftAttempts, refused.Load())
	}
	if p, _ := retryPrompt.Load().(string); !strings.Contains(p, "Your previous draft was refused") || !strings.Contains(p, "ended without delivering") {
		t.Fatalf("guarded retry prompt lacks the refusal: %s", p)
	}
	notices := leaderNotices(t, f, "contracts could not be redrafted")
	if len(notices) != 1 || !strings.Contains(notices[0], "last 3 redrafts were refused") || !strings.Contains(notices[0], "move_workstream to sketched") {
		t.Fatalf("chief notice of the stopped redraft: %v", notices)
	}
	assertGuardRefusal(t, f, id, 2, "ended without delivering")

	note := "Keep the acknowledgment contract on criterion 2."
	var prompt atomic.Value
	var calls atomic.Int32
	scriptProjectSpecTurn(f, "project-spec-draft-9", func(ctx context.Context, req agent.Request, tools *mcp.ClientSession) error {
		calls.Add(1)
		prompt.Store(req.Prompt)
		_, err := intakefixture.CallTool(ctx, tools, lifecycle.ProjectSpecDraftTool, map[string]any{
			"behavior":  "Redrafted upload contract.",
			"contracts": []map[string]any{{"name": "acknowledgment", "text": "The reviewed acknowledgment."}},
			"slices": []map[string]any{
				{"workstream": string(f.stream1), "contract_criteria": []int{2}},
				{"workstream": string(f.stream2), "contract_criteria": []int{1}},
			},
		})
		return err
	})
	chief := trace.Actor{Kind: "agent", ID: trace.ChiefOfStaff}
	out, apiFault := f.s.lifecycle().MoveWorkstream(ctx, mustRepo(t, f.s, f.project2), f.stream2, api.WorkstreamMoveRequest{To: intake.SketchedState, Note: note}, chief, "chief-turn-1")
	if apiFault != nil {
		t.Fatal(apiFault)
	}
	if !strings.Contains(out.Detail, "redrafts the workgroup spec's contracts again") {
		t.Fatalf("chief's request not taken: %+v", out)
	}
	for range 2 {
		intakefixture.Must(t, f.s.draftProjectSpecIfReady(ctx, repo, id))
	}
	if calls.Load() != 1 {
		t.Fatalf("requested redraft ran %d times", calls.Load())
	}
	if p, _ := prompt.Load().(string); !strings.Contains(p, note) {
		t.Fatalf("requested redraft prompt lacks the note: %s", p)
	}
	if got := leaderNotices(t, f, "contract redraft is recorded"); len(got) != 1 {
		t.Fatalf("recorded redraft reported %d times", len(got))
	}
	runRefresh(t, f)
	assertGuardRatifiable(t, f, id, 2, old.Revision+1)
}
func TestProjectSpecContractGuardHonorsApprovalDuringRedraft(t *testing.T) {
	t.Parallel()
	f, id, packets := projectSliceRefreshFixture(t)
	old := refreshDocument(t, f, id)
	s2 := strings.Replace(crossSpec1, "chunk's offset.", "chunk's byte offset.", 1)
	reviseRefreshMember(t, f, f.stream1, plan.SpecDocument, s2)
	var calls atomic.Int32
	scriptGuardDraft(t, f, "project-spec-draft-3", 2, s2, &calls, crossPlan1)
	approved := false
	f.s.boundary = func(step string) error {
		if step != "project-spec-draft-delivered" {
			return nil
		}
		reviseRefreshMember(t, f, f.stream1, plan.SpecDocument, crossSpec1)
		packet, err := f.c.Packet(context.Background(), f.stream1)
		intakefixture.Must(t, err)
		if packet.ProjectSpecRevision != packets[0].ProjectSpecRevision {
			t.Fatal("unexpected revision before approval")
		}
		_, err = f.c.Ratify(context.Background(), f.stream1, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan, packet.ProjectSpecRevision)
		intakefixture.Must(t, err)
		approved = true
		return nil
	}
	runGuardDraft(t, f)
	f.s.clearBoundary()
	if !approved {
		t.Fatal("approval boundary not reached")
	}
	runGuardDraft(t, f)
	runRefresh(t, f)
	if calls.Load() != 1 || !reflect.DeepEqual(refreshDocument(t, f, id), old) {
		t.Fatal("redraft recorded over an approval")
	}
}
func TestProjectSpecContractGuardLeavesOrdinaryEditsMechanical(t *testing.T) {
	t.Parallel()
	f, id, packets := projectSliceRefreshFixture(t)
	reviseRefreshMember(t, f, f.stream1, plan.SpecDocument, crossSpec1+"\nAdditional behavior.\n")
	reviseRefreshMember(t, f, f.stream1, plan.PlanDocument, crossPlan1+"\n")
	runGuardDraft(t, f)
	runRefresh(t, f)
	runGuardDraft(t, f)
	packet, err := f.c.Packet(context.Background(), f.stream1)
	intakefixture.Must(t, err)
	if packet.ProjectSpecRevision != 2 || packet.ProjectSpec.Behavior != packets[0].ProjectSpec.Behavior {
		t.Fatal("ordinary edit ran the architect")
	}
	wt, err := trace.OpenWorkgroupAuthority(f.s.current().Root, id)
	intakefixture.Must(t, err)
	groups, _, err := trace.Workgroups(wt)
	intakefixture.Must(t, err)
	th, err := wt.Thread(groups[0].Members[0], intake.ArchitectAgent)
	intakefixture.Must(t, err)
	intakefixture.Must(t, wt.Close())
	if len(th.Turns) != 1 {
		t.Fatalf("ordinary edit queued %d turns", len(th.Turns))
	}
	assertGuardRatifiable(t, f, id, 2, 2)
}
func TestProjectSpecContractGuardDefersHeldMembers(t *testing.T) {
	t.Parallel()
	for _, hold := range []string{"joining", "retired"} {
		t.Run(hold, func(t *testing.T) {
			t.Parallel()
			f, id, _ := projectSliceRefreshFixture(t)
			old := refreshDocument(t, f, id)
			reviseRefreshMember(t, f, f.stream1, plan.SpecDocument, strings.Replace(crossSpec1, "chunk's offset.", "chunk's byte offset.", 1))
			if hold == "joining" {
				_, err := trace.CreateWorkgroup(context.Background(), mustRepo(t, f.s, f.project2), []config.WorkstreamID{f.stream2}, f.s.now(), intake.OwnerActor, "joining")
				intakefixture.Must(t, err)
			} else {
				intakefixture.Must(t, f.s.store.SetArchived(runtime.Archive{Project: f.project2, Workstream: f.stream2, Workgroup: id, ArchivedAt: f.s.now()}))
			}
			runGuardDraft(t, f)
			if !reflect.DeepEqual(refreshDocument(t, f, id), old) {
				t.Fatal("held group redrafted")
			}
		})
	}
}
