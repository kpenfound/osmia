package service

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
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func haltGuardControllers(t *testing.T, f *crossRepoRatifyFixture) {
	t.Helper()
	_, projects := f.s.runtimes()
	for _, p := range projects {
		must(t, f.s.halt(p))
	}
}

func runGuardDraft(t *testing.T, f *crossRepoRatifyFixture) {
	t.Helper()
	for _, project := range []config.ProjectID{f.project, f.project2} {
		err := f.s.projectSpecDraftPass(context.Background(), mustRepo(t, f.s, project))
		must(t, err)
	}
	id, err := mustRepo(t, f.s, f.project).CrossRepoWorkgroupOf(f.stream1)
	must(t, err)
	wt, err := trace.OpenWorkgroupAuthority(f.s.current().Root, id)
	must(t, err)
	defer wt.Close()
	groups, _, err := trace.Workgroups(wt)
	must(t, err)
	state, err := wt.Workflow(groups[0].Members[0], projectSpecDraftSubject)
	must(t, err)
	if state.Value == "failed" {
		th, err := wt.Thread(groups[0].Members[0], architectAgent)
		must(t, err)
		t.Fatalf("draft failed: %+v", th.Turns[len(th.Turns)-1].Response)
	}
}

func assertGuardRefusal(t *testing.T, f *crossRepoRatifyFixture, id config.WorkgroupID, n int, failure string) {
	t.Helper()
	before := refreshDocument(t, f, id)
	packet, err := f.c.Packet(context.Background(), f.stream1)
	must(t, err)
	_, err = f.c.Ratify(context.Background(), f.stream1, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan, packet.ProjectSpecRevision)
	for _, text := range []string{string(id), string(f.stream1), fmt.Sprintf("criterion %d", n), "contracts are being redrafted", "read the packet again", failure} {
		if !failed(err, Conflict) || !containsMessage(err, text) {
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
		_, err = callTool(ctx, tools, ProjectSpecDraftTool, map[string]any{
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
	must(t, err)
	if packet.ProjectSpecRevision != revision {
		t.Fatalf("packet serves %d, want %d", packet.ProjectSpecRevision, revision)
	}
	slice, found := trace.SliceFor(*packet.ProjectSpec, f.stream1)
	if !found || !slices.Equal(slice.ContractCriteria, []int{criterion}) {
		t.Fatalf("architect's criteria lost: %+v", slice)
	}
	_, err = f.c.Ratify(context.Background(), f.stream1, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan, packet.ProjectSpecRevision)
	must(t, err)
	decision, found, err := mustRepo(t, f.s, f.project).GroupApprovalOf(f.stream1)
	must(t, err)
	if !found || decision.ProjectSpecRevision != revision || !reflect.DeepEqual(decision.ProjectSpec, *packet.ProjectSpec) || !trace.ContractCitation(decision.ProjectSpec, f.stream1, plan.Cite(criterion)) {
		t.Fatalf("approval lost displayed contract: %+v", decision)
	}
}

func TestProjectSpecContractGuardRedraftsChangedAndRemovedCriteria(t *testing.T) {
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
			must(t, err)
			history, err := trace.Get[trace.Document](wt, "", trace.ProjectSpecsDocument, old.Revision)
			must(t, err)
			must(t, wt.Close())
			if !reflect.DeepEqual(history, old) {
				t.Fatal("redraft rewrote history")
			}
			assertGuardRatifiable(t, f, id, criterion, old.Revision+1)
			// Approved contracts remain fixed even when their member text changes.
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
		eventually(t, "member decision packet", func() bool {
			state, err := mustRepo(t, f.s, member.project).Workflow(member.stream, trace.FeatureSubject)
			must(t, err)
			return state.Value == InShedState
		})
	}
	haltGuardControllers(t, f)
	id, api := f.s.DraftWorkgroup(ctx, []config.WorkstreamID{f.stream1, f.stream2})
	if api != nil {
		t.Fatal(api)
	}
	wt, err := trace.OpenWorkgroupAuthority(f.s.current().Root, id)
	must(t, err)
	groups, _, err := trace.Workgroups(wt)
	must(t, err)
	g := groups[0]
	host := g.Members[0]
	// The request has no snapshot document; its original member pins are unknown.
	_, err = wt.Transact(ctx, trace.Transaction{Transition: trace.Transition{
		Header:  trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "project-spec-draft-1", Revision: 1, Project: wt.Project(), Workstream: host, At: f.s.now(), Actor: projectSpecDraftActor, Cause: "project-spec-draft"},
		Subject: projectSpecDraftSubject, To: "drafting", Reason: "every member is sketched; the architect is asked for the project spec",
	}})
	must(t, err)
	must(t, wt.CreateThread(ctx, trace.Agent{Header: trace.Header{Schema: "osmia.trace.agent", Version: trace.Version, ID: architectAgent, Revision: 1, Project: wt.Project(), Workstream: host, At: f.s.now(), Actor: projectSpecDraftActor, Cause: "project-spec-draft"}, Role: architectRole, ThreadID: architectThread}))
	must(t, f.s.enqueueProjectSpecDraft(ctx, wt, g, host, []trace.CrossRepoMember{
		{Workstream: f.stream1, Project: mustRepo(t, f.s, f.project)},
		{Workstream: f.stream2, Project: mustRepo(t, f.s, f.project2)},
	}, "project-spec-draft-1"))
	must(t, wt.Close())
	return f, id, g
}

func captureUnpinnedProjectSpecTurn(t *testing.T, f *crossRepoRatifyFixture, wt *trace.Repository, host config.WorkstreamID, completed bool) {
	t.Helper()
	ctx := context.Background()
	q, err := wt.ClaimTurn(ctx, host, architectAgent, "unpinned-claim", "/unpinned-session", f.s.now())
	must(t, err)
	h := q.Request.Header
	h.Schema, h.ID, h.At, h.Actor = "osmia.trace.turn-response", trace.EventID(q.Request.ID, "response"), f.s.now(), projectSpecDraftActor
	must(t, wt.CaptureTurn(ctx, q.Claim.Token, trace.TurnResponse{Header: h, AgentID: architectAgent, ThreadID: architectThread, TurnID: q.Request.TurnID, RequestID: q.Request.ID, RequestRevision: 1,
		Result: coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: q.Request.Profile.Backend, ID: "unpinned-session"}, SessionDirectory: q.Claim.SessionDirectory, StartedAt: q.Claim.At, FinalResponse: "Unpinned project spec delivered"}}))
	if completed {
		must(t, wt.CompleteTurn(ctx, host, architectAgent, q.Request.TurnID, q.Claim.Token, f.s.now()))
	}
}

func TestProjectSpecContractGuardReconcilesDraftWithoutInput(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, id, g := unpinnedProjectSpecDraftFixture(t)
	host := g.Members[0]
	wt, err := trace.OpenWorkgroupAuthority(f.s.current().Root, id)
	must(t, err)
	captureUnpinnedProjectSpecTurn(t, f, wt, host, true)
	must(t, trace.RecordProjectSpec(ctx, wt, g, trace.ProjectSpec{
		Behavior:  "Chunks are acknowledged and resumed.",
		Contracts: []trace.Contract{{Name: "acknowledgment", Text: "The acknowledgment names the stored offset."}},
		Slices: []trace.Slice{
			{Workstream: f.stream1, Spec: crossSpec1, Plan: crossPlan1, ContractCriteria: []int{2}},
			{Workstream: f.stream2, Spec: crossSpec2, Plan: crossPlan2, ContractCriteria: []int{1}},
		},
	}, f.s.now(), projectSpecDraftActor, "project-spec-draft-1"))
	old := projectSpecDocument(t, wt)
	must(t, wt.Close())
	spec := strings.Replace(crossSpec1, "chunk's offset.", "chunk's byte offset.", 1)
	reviseRefreshMember(t, f, f.stream1, plan.SpecDocument, spec)
	runRefresh(t, f)
	assertGuardRefusal(t, f, id, 2, "")
	if got := refreshDocument(t, f, id); !reflect.DeepEqual(got, old) {
		t.Fatal("guard mechanically refreshed the contract")
	}

	// Finish only the recorded turn's bookkeeping, without guessing input pins.
	must(t, f.s.draftProjectSpecIfReady(ctx, mustRepo(t, f.s, f.project), id))
	wt, err = trace.OpenWorkgroupAuthority(f.s.current().Root, id)
	must(t, err)
	state, err := wt.Workflow(host, projectSpecDraftSubject)
	must(t, err)
	_, inputRevision, err := readProjectSpecDraftInput(wt, host)
	must(t, err)
	must(t, wt.Close())
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
	f.s.boundary = nil
	runGuardDraft(t, f)
	runGuardDraft(t, f)
	runRefresh(t, f)
	if calls.Load() != 1 || refreshDocument(t, f, id).Revision != old.Revision+1 {
		t.Fatalf("redraft duplicated: calls=%d revision=%d", calls.Load(), refreshDocument(t, f, id).Revision)
	}
	wt, err = trace.OpenWorkgroupAuthority(f.s.current().Root, id)
	must(t, err)
	history, err := trace.Get[trace.Document](wt, "", trace.ProjectSpecsDocument, old.Revision)
	must(t, err)
	_, inputRevision, err = readProjectSpecDraftInput(wt, host)
	must(t, err)
	must(t, wt.Close())
	if !reflect.DeepEqual(history, old) || inputRevision != 1 {
		t.Fatalf("redraft rewrote history or duplicated its request: input=%d", inputRevision)
	}
	assertGuardRatifiable(t, f, id, 2, old.Revision+1)
}

func TestProjectSpecDraftSupersedesUnpinnedOutput(t *testing.T) {
	for _, stage := range []string{"queued", "reserved", "captured", "completed"} {
		t.Run(stage, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			f, id, g := unpinnedProjectSpecDraftFixture(t)
			host := g.Members[0]
			wt, err := trace.OpenWorkgroupAuthority(f.s.current().Root, id)
			must(t, err)
			switch stage {
			case "reserved":
				_, err := wt.ClaimTurn(ctx, host, architectAgent, "unpinned-claim", "/unpinned-session", f.s.now())
				must(t, err)
			case "captured", "completed":
				captureUnpinnedProjectSpecTurn(t, f, wt, host, stage == "completed")
			}
			must(t, wt.Close())
			// Even delivered output cannot be bound to today's member text.
			data, err := json.Marshal(projectSpecDraft{Behavior: "Unpinned contract", Slices: []projectSpecDraftSlice{
				{Workstream: f.stream1, ContractCriteria: []int{1}},
				{Workstream: f.stream2, ContractCriteria: []int{1}},
			}})
			must(t, err)
			output := filepath.Join(f.s.projectSpecTurnDirectoryFor(host), "project-spec-draft-1", "output")
			must(t, os.MkdirAll(output, 0700))
			must(t, os.WriteFile(filepath.Join(output, "project-spec.json"), data, 0600))
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
			must(t, err)
			_, found, err := trace.ProjectSpecFor(wt, id)
			must(t, err)
			input, revision, err := readProjectSpecDraftInput(wt, host)
			must(t, err)
			th, err := wt.Thread(host, architectAgent)
			must(t, err)
			must(t, wt.Close())
			if found || revision != 1 || input.Turn != "project-spec-draft-2" || len(th.Turns) != 1 || th.Turns[0].CompletedAt.IsZero() {
				t.Fatalf("unpinned turn not superseded once: found=%v revision=%d input=%+v thread=%+v", found, revision, input, th)
			}
			f.s.boundary = nil
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
	must(t, err)
	first, _ := trace.SliceFor(*packet.ProjectSpec, f.stream1)
	second, _ := trace.SliceFor(*packet.ProjectSpec, f.stream2)
	if first.Spec != crossSpec1 || second.Spec != crossSpec2+"\nOther behavior.\n" || packet.ProjectSpecRevision != packets[0].ProjectSpecRevision+1 {
		t.Fatalf("per-member refresh: %+v", packet.ProjectSpec)
	}
	assertGuardRefusal(t, f, id, 2, "")
}

func TestProjectSpecContractGuardPinsMidRedraftEdits(t *testing.T) {
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
				must(t, err)
			} else if !errors.Is(err, stop) {
				t.Fatalf("interruption: %v", err)
			}
			f.s.boundary = nil
			if !injected {
				t.Fatal("draft boundary not reached")
			}
			if interruption != "none" {
				// Keep background passes from delivering until this test explicitly
				// resumes; the persisted request and turn survive a real service restart.
				f.stop(t)
				f.opts.boundary = func(_ context.Context, step string) error {
					if step == "project-spec-draft-requested" {
						return stop
					}
					return nil
				}
				f.start(t)
				haltGuardControllers(t, f)
				f.s.boundary = nil
				if interruption != "recorded" {
					must(t, f.s.draftProjectSpecIfReady(context.Background(), mustRepo(t, f.s, f.project), id))
				}
			}
			packet, err := f.c.Packet(context.Background(), f.stream1)
			must(t, err)
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
			// The other member can refresh mechanically before this turn lands.
			runRefresh(t, f)
		}
		return nil
	}
	runGuardDraft(t, f)
	f.s.boundary = nil
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
	must(t, f.s.draftProjectSpecIfReady(context.Background(), mustRepo(t, f.s, f.project), id))
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
		// The member restores the reviewed contract while the architect runs.
		// The displayed project spec is now current and can be approved.
		reviseRefreshMember(t, f, f.stream1, plan.SpecDocument, crossSpec1)
		packet, err := f.c.Packet(context.Background(), f.stream1)
		must(t, err)
		if packet.ProjectSpecRevision != packets[0].ProjectSpecRevision {
			t.Fatal("unexpected revision before approval")
		}
		_, err = f.c.Ratify(context.Background(), f.stream1, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan, packet.ProjectSpecRevision)
		must(t, err)
		approved = true
		return nil
	}
	runGuardDraft(t, f)
	f.s.boundary = nil
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
	must(t, err)
	if packet.ProjectSpecRevision != 2 || packet.ProjectSpec.Behavior != packets[0].ProjectSpec.Behavior {
		t.Fatal("ordinary edit ran the architect")
	}
	wt, err := trace.OpenWorkgroupAuthority(f.s.current().Root, id)
	must(t, err)
	groups, _, err := trace.Workgroups(wt)
	must(t, err)
	th, err := wt.Thread(groups[0].Members[0], architectAgent)
	must(t, err)
	must(t, wt.Close())
	if len(th.Turns) != 1 {
		t.Fatalf("ordinary edit queued %d turns", len(th.Turns))
	}
	assertGuardRatifiable(t, f, id, 2, 2)
}

func TestProjectSpecInitialDraftFailureRemainsTerminal(t *testing.T) {
	t.Parallel()
	f := newCrossRepoRatifyFixture(t)
	defer f.stop(t)
	var calls atomic.Int32
	f.engine.Mu.Lock()
	f.engine.Turns["project-spec-draft-1"] = func(_ context.Context, req agent.Request, _ *agent.Turn, _ *mcp.ClientSession) (*agent.Result, error) {
		calls.Add(1)
		return &agent.Result{ClaudeID: "no-project-spec", ResultText: "No delivery", SessionDir: req.SessionDir, NumTurns: 1}, nil
	}
	f.engine.Mu.Unlock()
	id, api := f.s.DraftWorkgroup(context.Background(), []config.WorkstreamID{f.stream1, f.stream2})
	if api != nil {
		t.Fatal(api)
	}
	eventually(t, "the project spec draft failure", func() bool {
		wt, err := trace.OpenWorkgroupAuthority(f.s.current().Root, id)
		must(t, err)
		defer wt.Close()
		groups, _, err := trace.Workgroups(wt)
		must(t, err)
		state, err := wt.Workflow(groups[0].Members[0], projectSpecDraftSubject)
		must(t, err)
		return state.Value == "failed"
	})
	haltGuardControllers(t, f)
	for range 3 {
		must(t, f.s.draftProjectSpecIfReady(context.Background(), mustRepo(t, f.s, f.project), id))
	}
	wt, err := trace.OpenWorkgroupAuthority(f.s.current().Root, id)
	must(t, err)
	defer wt.Close()
	_, found, err := trace.ProjectSpecFor(wt, id)
	must(t, err)
	if calls.Load() != 1 || found {
		t.Fatalf("initial failure retried or recorded: calls=%d spec=%v", calls.Load(), found)
	}
}

func TestProjectSpecContractGuardDefersHeldMembers(t *testing.T) {
	for _, hold := range []string{"joining", "retired"} {
		t.Run(hold, func(t *testing.T) {
			t.Parallel()
			f, id, _ := projectSliceRefreshFixture(t)
			old := refreshDocument(t, f, id)
			reviseRefreshMember(t, f, f.stream1, plan.SpecDocument, strings.Replace(crossSpec1, "chunk's offset.", "chunk's byte offset.", 1))
			if hold == "joining" {
				_, err := trace.CreateWorkgroup(context.Background(), mustRepo(t, f.s, f.project2), []config.WorkstreamID{f.stream2}, f.s.now(), ownerActor, "joining")
				must(t, err)
			} else {
				must(t, f.s.store.SetArchived(runtime.Archive{Project: f.project2, Workstream: f.stream2, Workgroup: id, ArchivedAt: f.s.now()}))
			}
			runGuardDraft(t, f)
			if !reflect.DeepEqual(refreshDocument(t, f, id), old) {
				t.Fatal("held group redrafted")
			}
		})
	}
}
