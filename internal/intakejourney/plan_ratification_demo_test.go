package intakejourney

import (
	context "context"
	errors "errors"
	fmt "fmt"
	os "os"
	filepath "path/filepath"
	slices "slices"
	strings "strings"
	sync "sync"
	testing "testing"
	time "time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	agent "github.com/kpenfound/agentcore/agent"
	config "github.com/kpenfound/osmia/internal/config"
	coreadapter "github.com/kpenfound/osmia/internal/coreadapter"
	kb "github.com/kpenfound/osmia/internal/kb"
	plan "github.com/kpenfound/osmia/internal/plan"
	questions "github.com/kpenfound/osmia/internal/questions"
	seal "github.com/kpenfound/osmia/internal/seal"
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	shed "github.com/kpenfound/osmia/internal/shed"
	trace "github.com/kpenfound/osmia/internal/trace"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

const exitDemoRoles = `[roles.committee]
sandbox = "container"
image = "fixture-image"
[roles.chief_of_staff]
sandbox = "container"
image = "fixture-image"
[capacity]
committee = 2
[shed]
max_rounds = 2
`

var exitChargeVeto = intakefixture.Objection{Kind: shed.Charter, Part: "spec#2", Argument: "Criterion 2 is shown by judgement, not by a test.", Citation: "charter#2"}

type handInDemo struct {
	ctx    context.Context
	f      *journey.ShedFixture
	d      *intakefixture.ExitDemo
	engine *fixturekit.DemoEngine
	home   string
	clone  string
	commit string
}

func newHandInDemo(t *testing.T) *handInDemo {
	t.Helper()
	opts, clone, engine, sessions, clock := journey.NewArchitectOptions(t)
	configFile, err := os.OpenFile(filepath.Join(opts.Config.Root, "config.toml"), os.O_APPEND|os.O_WRONLY, 0)
	fixturekit.Must(t, err)
	_, err = configFile.WriteString(exitDemoRoles)
	fixturekit.Must(t, errors.Join(err, configFile.Close()))
	engine.Resume = func(coreadapter.Profile, coreadapter.Profile, coreadapter.BackendSession) error { return nil }
	hosts := &coreadapter.MCPHost{Transport: &fixturekit.DemoTransport{Sessions: sessions}}
	opts = journey.Enforce(opts, service.Enforcement{Engine: engine, Hosts: hosts})
	f := &journey.ShedFixture{ArchitectFixture: &journey.ArchitectFixture{Opts: opts, Clone: clone, Engine: engine, Sessions: sessions, Clock: clock}, Members: 2}
	d := &intakefixture.ExitDemo{Plays: map[string]map[string]fixturekit.DemoTurn{}, Ran: map[string][]string{}, Prompts: map[string]string{}}
	return &handInDemo{ctx: context.Background(), f: f, d: d, engine: engine, home: filepath.Dir(clone), clone: clone}
}

func (h *handInDemo) start(t *testing.T) {
	t.Helper()
	for _, plays := range h.d.Plays {
		for name := range plays {
			h.engine.Turns[name] = h.d.Route
		}
	}
	h.engine.Turns["*"] = h.d.Chief
	h.f.Start(t)
	added, err := h.f.C.AddProject(h.ctx, fixturekit.Request(h.clone))
	fixturekit.Must(t, err)
	h.f.Project, h.f.Trace, h.d.Trace, h.d.Root, h.d.Project = added.Project.ID, added.Project.Trace, added.Project.Trace, h.f.S.Current().Root, added.Project.ID
	intakefixture.WriteCharter(t, h.f.C, added.Project.ID, intakefixture.ShedCharter)
	onboarded, err := h.f.C.Configuration(h.ctx)
	fixturekit.Must(t, err)
	if cs := onboarded.Project.CharterState; cs == nil || cs.Rules != 2 {
		t.Fatalf("charter: %+v", cs)
	}
	h.commit = h.f.Upstream(t)
}

func (h *handInDemo) handIn(t *testing.T, key, design string, skip bool) config.WorkstreamID {
	t.Helper()
	out, err := h.f.C.HandIn(h.ctx, api.HandInRequest{Project: h.f.Project, Key: key, Stdin: intakefixture.Ptr(design), SkipDebate: skip})
	fixturekit.Must(t, err)
	if out.State != intake.HandedState || out.SkipDebate != skip {
		t.Fatalf("hand-in %+v", out)
	}
	h.d.Mu.Lock()
	if h.d.Streams == nil {
		h.d.Streams = map[string]config.WorkstreamID{}
	}
	h.d.Streams[intakefixture.ExitTitle(design)] = out.Workstream
	h.d.Mu.Unlock()
	return out.Workstream
}

func (h *handInDemo) awaitInShed(t *testing.T, stream config.WorkstreamID) trace.Transition {
	t.Helper()
	h.f.AwaitFeature(t, stream, intake.InShedState)
	moved := h.f.Transition(t, stream, intake.SketchedState)
	if moved.From != intake.HandedState || moved.Actor != intake.DraftingActor {
		t.Fatalf("the move to sketched %+v", moved)
	}
	return moved
}

func (h *handInDemo) awaitAttention(t *testing.T, stream config.WorkstreamID, want string) {
	t.Helper()
	h.f.S.Until(t, fmt.Sprintf("workstream %s to show the attention %q", stream, want), func() bool {
		st, err := h.f.C.Status(h.ctx, stream)
		fixturekit.Must(t, err)
		return st.Status != nil && st.Status.Attention == want
	})
}

func (h *handInDemo) sealOf(t *testing.T, stream config.WorkstreamID) seal.Seal {
	t.Helper()
	record, doc, found, err := seal.Latest(h.f.Repository(), stream)
	fixturekit.Must(t, err)
	if !found || doc.Revision != 1 || len(h.f.Documents(t, stream, seal.DocumentID)) != 1 {
		t.Fatalf("seal of workstream %s: %v %+v", stream, found, doc)
	}
	return record
}

func (h *handInDemo) branchAt(t *testing.T, stream config.WorkstreamID) string {
	t.Helper()
	return strings.TrimSpace(fixturekit.DemoGit(t, h.home, "-C", h.clone, "rev-parse", "refs/heads/osmia/"+string(stream)))
}

func (h *handInDemo) escalations(t *testing.T) []api.InboxEntry {
	t.Helper()
	inbox, err := h.f.C.Inbox(h.ctx)
	fixturekit.Must(t, err)
	var out []api.InboxEntry
	for _, e := range inbox.Entries {
		if e.Kind == api.InboxEscalation {
			out = append(out, e)
		}
	}
	return out
}

func (h *handInDemo) checkSealedWorktrees(t *testing.T, sealed int) {
	t.Helper()
	out := fixturekit.DemoGit(t, h.home, "-C", h.clone, "worktree", "list", "--porcelain")
	units := "worktree " + filepath.Join(h.f.Opts.Config.Root, intake.UnitsDirectory) + string(filepath.Separator)
	if got := strings.Count(out, "worktree ") - strings.Count(out, units); got != 1+sealed {
		t.Fatalf("worktrees, want the clone and %d sealed:\n%s", sealed, out)
	}
}

func (h *handInDemo) checkNothingPushedUpstream(t *testing.T) {
	t.Helper()
	if refs := fixturekit.DemoGit(t, h.home, "-C", filepath.Join(h.home, "remotes", "dagger", "dagger.git"), "for-each-ref", "--format=%(refname)"); strings.TrimSpace(refs) != "refs/heads/main" {
		t.Fatalf("upstream's refs:\n%s", refs)
	}
}

func (h *handInDemo) checkFakeAgents(t *testing.T) {
	t.Helper()
	h.d.Mu.Lock()
	defer h.d.Mu.Unlock()
	if len(h.d.Problems) != 0 {
		t.Fatalf("fake agents saw:\n%s", strings.Join(h.d.Problems, "\n"))
	}
}

func (h *handInDemo) playDebated() {
	veto, size := shed.ObjectionID(1, intake.CommitteeAgent(1), 1), shed.ObjectionID(1, intake.CommitteeAgent(1), 2)
	quiet := h.d.Debates(nil)
	h.d.Plays[intakefixture.ExitTitle(fixturekit.HandedDesign)] = map[string]fixturekit.DemoTurn{
		"draft-1-1": h.d.Drafts(map[string]string{plan.SpecPath: fixturekit.ValidSpec, plan.PlanPath: fixturekit.ValidPlan}, nil),
		intake.RoundTurnID(1, intake.CommitteeAgent(1), 1): h.d.Debates([]intakefixture.Objection{exitChargeVeto, {Kind: shed.Size, Part: "plan#resume", Argument: "Resuming is two changes, not one.", Citation: "kb/entities.json#internal.trace"}}),
		intake.RoundTurnID(1, intake.CommitteeAgent(2), 1): quiet,
		intake.ReplyTurnID(1, 1):                           h.d.Drafts(map[string]string{plan.PlanPath: splitPlan}, map[string]string{veto: "Criterion 2 now has TestNoChunkTwice.", size: "Resuming is split in two units."}),
		intake.RoundTurnID(2, intake.CommitteeAgent(1), 1): h.d.Debates(nil, veto, size),
		intake.RoundTurnID(2, intake.CommitteeAgent(2), 1): quiet,
	}
}

func (h *handInDemo) playCapped(asks bool) {
	d, f := h.d, h.f
	cappedVeto := shed.ObjectionID(1, intake.CommitteeAgent(1), 1)
	quiet := d.Debates(nil)
	second := quiet
	if asks {
		second = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
			if stream, ok := d.StreamOf(intakefixture.ExitCapped); !ok {
				d.Problem("the member asked before its workstream was known")
			} else if state, err := f.Repository().Workflow(stream, trace.FeatureSubject); err != nil || state.Value != intake.InShedState {
				d.Problem("the member asked with the workstream %q %v, not in the shed", state.Value, err)
			}
			if out, err := fixturekit.CallTool(ctx, tools, questions.AskTool, map[string]any{"question": intakefixture.ExitQuestion}); err != nil || !strings.Contains(out, `"recorded":true`) {
				d.Problem("ask: %s %v", out, err)
			}
			return &agent.Result{ClaudeID: "session-" + req.Name, ResultText: "Asked", SessionDir: req.SessionDir, NumTurns: 2}, nil
		}
	}
	plays := map[string]fixturekit.DemoTurn{
		"draft-1-1": d.Drafts(map[string]string{plan.SpecPath: fixturekit.ValidSpec, plan.PlanPath: fixturekit.ValidPlan}, nil),
		intake.RoundTurnID(1, intake.CommitteeAgent(1), 1): d.Debates([]intakefixture.Objection{exitChargeVeto}),
		intake.RoundTurnID(1, intake.CommitteeAgent(2), 1): second,
		intake.ReplyTurnID(1, 1):                           d.Drafts(nil, map[string]string{cappedVeto: "The reviewer's judgement is the right proof here."}),
		intake.RoundTurnID(2, intake.CommitteeAgent(1), 1): quiet,
		intake.RoundTurnID(2, intake.CommitteeAgent(2), 1): quiet,
		intake.ReplyTurnID(2, 1):                           d.Drafts(nil, nil),
	}
	if asks {
		plays[questions.TurnID("1")] = quiet
	}
	d.Plays[intakefixture.ExitTitle(intakefixture.ExitCapped)] = plays
}

func (h *handInDemo) playSmall() {
	h.d.Plays[intakefixture.ExitTitle(intakefixture.ExitSmall)] = map[string]fixturekit.DemoTurn{"draft-1-1": h.d.Drafts(map[string]string{plan.SpecPath: intakefixture.ExitSmallSpec, plan.PlanPath: intakefixture.ExitSmallPlan}, nil)}
}

func (h *handInDemo) playAbandoned() {
	h.d.Plays[intakefixture.ExitTitle(intakefixture.ExitAbandoned)] = map[string]fixturekit.DemoTurn{"draft-1-1": h.d.Drafts(map[string]string{plan.SpecPath: fixturekit.ValidSpec, plan.PlanPath: fixturekit.ValidPlan}, nil)}
}

func (h *handInDemo) awaitCapped(t *testing.T, stream config.WorkstreamID) {
	t.Helper()
	h.f.AwaitShed(t, stream, "concluded-2")
	if end := h.f.Transition(t, stream, "shed-concluded-2"); end.Reason != "debate stopped after round 2, at the shed.max_rounds cap of 2, with 1 objection standing, 1 of them blocking; the cap approves nothing" {
		t.Fatalf("the conclusion %+v", end)
	}
}

func cappedBlocked() string {
	return fmt.Sprintf("do not ratify yet: ratification is blocked by 1 objection (%s); overrule or sustain each one, or ask for a redraft", shed.ObjectionID(1, intake.CommitteeAgent(1), 1))
}

func (h *handInDemo) overruleCapped(t *testing.T, stream config.WorkstreamID) {
	t.Helper()
	h.f.AwaitPacket(t, stream, cappedBlocked())
	if _, err := h.f.C.ShedOverrule(h.ctx, stream, shed.ObjectionID(1, intake.CommitteeAgent(1), 1), intakefixture.ExitOverrule); err != nil {
		t.Fatal(err)
	}
	h.f.AwaitPacket(t, stream, "ratify: nothing blocks, and 1 objection stands as advice on the record")
}

func TestHandInToRatifiedPlan(t *testing.T) {
	t.Parallel()
	h := newHandInDemo(t)
	h.playDebated()
	h.start(t)
	f, ctx := h.f, h.ctx
	defer f.Stop(t)

	entities, err := kb.Load(f.Repository())
	fixturekit.Must(t, err)
	footprint := entities.ResolveEntities([]string{"internal.trace"})
	if len(footprint.Entities) != 1 || len(footprint.Paths) == 0 || len(footprint.Unresolved) != 0 {
		t.Fatalf("the local entity map resolves internal.trace to %+v", footprint)
	}

	debated := h.handIn(t, "debated", fixturekit.HandedDesign, false)
	h.awaitInShed(t, debated)
	if enter := f.Transition(t, debated, intake.InShedState); enter.From != intake.SketchedState || enter.Reason != "spec.md revision 1 and plan.json revision 1 enter the shed with a committee of 2" {
		t.Fatalf("workstream %s entered the shed %+v", debated, enter)
	}
	list, err := f.C.Statuses(ctx)
	fixturekit.Must(t, err)
	if len(list.Workstreams) != 1 || list.Workstreams[0].State == nil || *list.Workstreams[0].State != intake.InShedState {
		t.Fatalf("statuses: %+v", list)
	}

	f.AwaitShed(t, debated, "concluded-2")
	if moves, want := f.ShedMoves(t, debated), []string{"round-1", "heard-1", "reply-1", "replied-1", "round-2", "heard-2", "concluded-2"}; !slices.Equal(moves, want) {
		t.Fatalf("shed of the debated workstream went %v, want %v", moves, want)
	}
	if reply := f.Reply(t, debated, 1); reply.Redraft == nil || *reply.Redraft != (shed.Pin{Spec: 1, Plan: 2}) || len(reply.Answers) != 2 {
		t.Fatalf("the architect's reply %+v", reply)
	}
	if end := f.Transition(t, debated, "shed-concluded-2"); end.Reason != "debate concluded by consensus after round 2: no objection stands" {
		t.Fatalf("the conclusion %+v", end)
	}
	packet := f.AwaitPacket(t, debated, "ratify: no objection stands")
	if packet.Revision != (shed.Pin{Spec: 1, Plan: 2}) || len(packet.Dissent) != 0 {
		t.Fatalf("the debated packet %+v", packet)
	}
	h.awaitAttention(t, debated, intakefixture.ExitAttention(packet.Recommendation))
	if out, err := f.C.Ratify(ctx, debated, 1, 2); err != nil || out.Sealing != "requested" {
		t.Fatalf("ratifying the debated workstream: %+v %v", out, err)
	}
	f.AwaitFeature(t, debated, intake.BuildingState)
	s := h.sealOf(t, debated)
	if s.Base != (seal.Base{Remote: "upstream", Branch: "main", Commit: h.commit}) || s.SpecHash != seal.SpecHash(fixturekit.ValidSpec) || s.Revision != (shed.Pin{Spec: 1, Plan: 2}) || s.Branch != "osmia/"+string(debated) {
		t.Fatalf("the debated seal %+v", s)
	}
	var units []string
	for _, fp := range s.Footprints {
		units = append(units, fp.Unit)
		if !slices.Equal(fp.Entities, footprint.Entities) || !slices.Equal(fp.Paths, footprint.Paths) {
			t.Fatalf("footprint %+v, want %+v", fp, footprint)
		}
	}
	if !slices.Equal(units, []string{"resume-read", "resume-write", "dedupe"}) || h.branchAt(t, debated) != h.commit {
		t.Fatalf("the debated seal's units %v and branch", units)
	}
	if on := strings.TrimSpace(fixturekit.DemoGit(t, h.home, "-C", s.Workspace, "rev-parse", "--abbrev-ref", "HEAD")); on != s.Branch {
		t.Fatalf("the workspace is on %s", on)
	}
	h.checkSealedWorktrees(t, 1)
	h.checkNothingPushedUpstream(t)
	h.checkFakeAgents(t)
}

func TestSkippedDebateRatifiesWithoutACommittee(t *testing.T) {
	t.Parallel()
	h := newHandInDemo(t)
	h.playSmall()
	h.start(t)
	f, ctx := h.f, h.ctx
	defer f.Stop(t)

	small := h.handIn(t, "small", intakefixture.ExitSmall, true)
	h.awaitInShed(t, small)
	if enter := f.Transition(t, small, intake.InShedState); enter.From != intake.SketchedState || enter.Reason != "spec.md revision 1 and plan.json revision 1 enter the shed without a committee: the owner skipped debate" {
		t.Fatalf("workstream %s entered the shed %+v", small, enter)
	}
	packet := f.AwaitPacket(t, small, "ratify: no objection stands")
	if !packet.Skipped || packet.Revision != (shed.Pin{Spec: 1, Plan: 1}) || len(packet.Dissent) != 0 {
		t.Fatalf("the skipped debate's packet %+v", packet)
	}
	h.awaitAttention(t, small, intakefixture.ExitAttention(packet.Recommendation))
	if out, err := f.C.Ratify(ctx, small, 1, 1); err != nil || out.Sealing != "requested" {
		t.Fatalf("ratifying the skipped debate: %+v %v", out, err)
	}
	f.AwaitFeature(t, small, intake.BuildingState)
	if s := h.sealOf(t, small); s.Base.Commit != h.commit || s.SpecHash != seal.SpecHash(intakefixture.ExitSmallSpec) || len(s.Footprints) != 1 || s.Footprints[0].Unit != "message" || h.branchAt(t, small) != h.commit {
		t.Fatalf("the small workstream's seal %+v", s)
	}
	if ran := slices.DeleteFunc(h.d.Runs(intakefixture.ExitTitle(intakefixture.ExitSmall)), func(name string) bool { return name == "events" }); !slices.Equal(ran, []string{"draft-1-1"}) {
		t.Fatalf("the small workstream ran %v", ran)
	}
	h.checkSealedWorktrees(t, 1)
	h.checkNothingPushedUpstream(t)
	h.checkFakeAgents(t)
}

func TestCommitteeQuestionReachesTheOwnerAndTheAnswerIsRelayedToTheMember(t *testing.T) {
	t.Parallel()
	h := newHandInDemo(t)
	f, ctx := h.f, h.ctx
	h.playCapped(true)
	h.start(t)
	defer f.Stop(t)

	capped := h.handIn(t, "capped", intakefixture.ExitCapped, false)
	h.awaitInShed(t, capped)
	var entry api.InboxEntry
	f.S.Until(t, "exactly one escalation", func() bool {
		entries := h.escalations(t)
		if len(entries) != 1 {
			return false
		}
		entry = entries[0]
		return true
	})
	asker := intake.CommitteeAgent(2)
	if entry.Number != 1 || entry.Workstream != capped || entry.Batch != "escalation_1" || entry.Question != "Does deletion end an upload's retention period?" ||
		len(entry.Asked) != 1 || entry.Asked[0] != (api.InboxQuestion{ID: "1", AskedBy: asker, Question: intakefixture.ExitQuestion}) {
		t.Fatalf("inbox entry %+v", entry)
	}
	if state, err := f.Repository().Workflow(capped, intake.ShedSubject); err != nil || state.Value != "waiting-1" {
		t.Fatalf("the retention shed while the question is open: %+v %v", state, err)
	}
	if _, err := f.C.Answer(ctx, entry.Number, intakefixture.ExitRuling, ""); err != nil {
		t.Fatal(err)
	}

	f.AwaitShed(t, capped, "heard-1", "concluded-2")
	th, err := f.Repository().Thread(capped, asker)
	fixturekit.Must(t, err)
	if len(th.Turns) < 2 || th.Turns[1].Request.TurnID != questions.TurnID("1") || th.Turns[1].Status() != "idle" {
		t.Fatalf("the member's thread %+v", th.Turns)
	}
	h.d.Mu.Lock()
	answered := h.d.Prompts[intakefixture.ExitTitle(intakefixture.ExitCapped)+"/"+questions.TurnID("1")]
	h.d.Mu.Unlock()
	if !strings.Contains(answered, "| "+intakefixture.ExitQuestion) || !strings.Contains(answered, "<<< osmia:owner_response") || !strings.Contains(answered, "| "+intakefixture.ExitRelayed) {
		t.Fatalf("the member heard:\n%s", answered)
	}
	if entries := h.escalations(t); len(entries) != 0 {
		t.Fatalf("escalations after the answer %+v", entries)
	}

	f.AwaitShed(t, capped, "concluded-2")
	if moves := f.ShedMoves(t, capped); len(moves) < 4 || !slices.Equal(moves[:4], []string{"round-1", "waiting-1", "round-1", "heard-1"}) {
		t.Fatalf("shed of the retention workstream went %v", moves)
	}
	h.checkFakeAgents(t)
}

func TestDebateStoppedAtTheCapNeedsTheOwnersRulingBeforeRatification(t *testing.T) {
	t.Parallel()
	h := newHandInDemo(t)
	f, ctx := h.f, h.ctx
	h.playCapped(false)
	h.start(t)
	defer f.Stop(t)

	capped := h.handIn(t, "capped", intakefixture.ExitCapped, false)
	h.awaitCapped(t, capped)
	blocked := cappedBlocked()
	f.AwaitPacket(t, capped, blocked)
	h.awaitAttention(t, capped, intakefixture.ExitAttention(blocked))
	veto := shed.ObjectionID(1, intake.CommitteeAgent(1), 1)
	_, err := f.C.Ratify(ctx, capped, 1, 1)
	if !intakefixture.Failed(err, api.Conflict) || !strings.Contains(err.Error(), fmt.Sprintf("objection %s (charter, by %s in round 1 on spec#2) blocks and has no disposition", veto, intake.CommitteeAgent(1))) {
		t.Fatalf("ratifying over the veto: %v", err)
	}
	if _, err := f.C.ShedOverrule(ctx, capped, veto, intakefixture.ExitOverrule); err != nil {
		t.Fatal(err)
	}
	packet := f.AwaitPacket(t, capped, "ratify: nothing blocks, and 1 objection stands as advice on the record")
	if len(packet.Dissent) != 1 || packet.Dissent[0].Disposition != shed.Overruled || packet.Dissent[0].Revision != (shed.Pin{Spec: 1, Plan: 1}) {
		t.Fatalf("the packet after the overrule %+v", packet)
	}

	rulings := f.Documents(t, capped, shed.RulingsDocumentID(2))
	if len(rulings) != 1 || rulings[0].Actor != intake.OwnerActor {
		t.Fatalf("the owner's rulings %+v", rulings)
	}
	ruled, err := shed.ParseRulings([]byte(rulings[0].Content))
	fixturekit.Must(t, err)
	if ruled.Revision != (shed.Pin{Spec: 1, Plan: 1}) || !slices.Equal(ruled.Rulings, []shed.Ruling{{Objection: veto, Disposition: shed.Overruled, Note: intakefixture.ExitOverrule}}) {
		t.Fatalf("the overrule %+v", ruled)
	}
	h.checkFakeAgents(t)
}

func TestSealingInterruptedAfterRatifyingOverAnOverruledObjectionResumesAfterRestart(t *testing.T) {
	t.Parallel()
	h := newHandInDemo(t)
	f, ctx := h.f, h.ctx
	h.playCapped(false)
	h.start(t)
	defer func() { f.Stop(t) }()

	capped := h.handIn(t, "capped", intakefixture.ExitCapped, false)
	h.awaitCapped(t, capped)
	h.overruleCapped(t, capped)

	reached := make(chan struct{})
	lifetime := f.S
	var once sync.Once
	f.S.Boundary = func(name string) error {
		if name != "seal-branch-created" {
			return nil
		}
		stop := false
		once.Do(func() { stop = true; close(reached) })
		if !stop {
			return nil
		}
		<-lifetime.Lifetime.Done()
		return lifetime.Lifetime.Err()
	}
	ratified, err := f.C.Ratify(ctx, capped, 1, 1)
	fixturekit.Must(t, err)
	if ratified.Sealing != "requested" || ratified.Detail != "the owner ratified spec.md revision 1 and plan.json revision 1 after round 2, over 1 objection the owner disposed of; the sealing is asked for" {
		t.Fatalf("ratification %+v", ratified)
	}
	select {
	case <-reached:
	case <-time.After(fixturekit.DemoTimeout):
		t.Fatal("the sealing never created the feature branch")
	}
	f.Stop(t)
	if h.branchAt(t, capped) != h.commit {
		t.Fatal("the feature branch is not where the stopped sealing left it")
	}
	repo, err := trace.Open(f.S.Current().Root, f.S.Current().Project)
	fixturekit.Must(t, err)
	if _, _, found, err := seal.Latest(repo, capped); err != nil || found {
		t.Fatalf("a seal before the restart: %v %v", found, err)
	}
	interrupted, err := repo.Operations(capped)
	fixturekit.Must(t, err)
	interrupted = slices.DeleteFunc(interrupted, func(o trace.OperationRecord) bool { return o.Operation.Action != intake.SealAction })
	if len(interrupted) != 1 || interrupted[0].Result != nil || interrupted[0].Acknowledged || !interrupted[0].EffectStarted {
		t.Fatalf("sealing at the restart boundary: %+v", interrupted)
	}
	fixturekit.Must(t, repo.Close())

	f.Start(t)
	f.AwaitRecoveredSeal(t, capped)
	veto := shed.ObjectionID(1, intake.CommitteeAgent(1), 1)
	record := f.Ratification(t, capped, 2)
	if record.Revision != (shed.Pin{Spec: 1, Plan: 1}) || len(record.Dispositions) != 1 || record.Dispositions[0] != (shed.Ruling{Objection: veto, Disposition: shed.Overruled, Note: intakefixture.ExitOverrule}) {
		t.Fatalf("the ratification %+v", record)
	}
	ops := fixturekit.AwaitAcknowledged(t, func(t *testing.T) []trace.OperationRecord { return f.SealOperations(t, capped) })
	if len(ops) != 1 || ops[0].Result == nil || ops[0].Result.Outcome != "succeeded" {
		t.Fatalf("seal operations %+v", ops)
	}
	if !slices.ContainsFunc(ops[0].History, func(a trace.OperationAction) bool {
		return a.Kind == "observe" && a.Observation.Evidence == fmt.Sprintf("the clone has feature branch osmia/%s; the sealing resumes from it", capped)
	}) {
		t.Fatalf("the sealing did not resume from the branch: %+v", ops[0].History)
	}
	if s := h.sealOf(t, capped); s.Seal != 1 || s.Base != (seal.Base{Remote: "upstream", Branch: "main", Commit: h.commit}) || s.SpecHash != seal.SpecHash(fixturekit.ValidSpec) || s.Revision != (shed.Pin{Spec: 1, Plan: 1}) || s.Branch != intake.FeatureBranch(capped) || h.branchAt(t, capped) != h.commit {
		t.Fatalf("the capped seal %+v", s)
	}
	h.checkSealedWorktrees(t, 1)
	h.checkNothingPushedUpstream(t)
	h.checkFakeAgents(t)
}

func TestAbandonedHandInKeepsItsDraftAndStopsBeforeAnyDebate(t *testing.T) {
	t.Parallel()
	h := newHandInDemo(t)
	h.playAbandoned()
	h.start(t)
	f, ctx := h.f, h.ctx
	defer f.Stop(t)

	gone := h.handIn(t, "abandoned", intakefixture.ExitAbandoned, true)
	h.awaitInShed(t, gone)
	abandoned, err := f.C.Abandon(ctx, gone, "Quotas wait for the billing work.")
	fixturekit.Must(t, err)
	if abandoned.State != intake.AbandonedState || abandoned.Reason != "Quotas wait for the billing work." {
		t.Fatalf("abandon %+v", abandoned)
	}

	if state, err := f.Repository().Workflow(gone, trace.FeatureSubject); err != nil || state.Value != intake.AbandonedState {
		t.Fatalf("the abandoned workstream %+v %v", state, err)
	}
	if ran := slices.DeleteFunc(h.d.Runs(intakefixture.ExitTitle(intakefixture.ExitAbandoned)), func(name string) bool { return name == "events" }); !slices.Equal(ran, []string{"draft-1-1"}) {
		t.Fatalf("the abandoned workstream ran %v", ran)
	}
	if moves := f.ShedMoves(t, gone); len(moves) != 0 {
		t.Fatalf("the abandoned workstream's shed went %v", moves)
	}
	if threads, err := f.Repository().Threads(gone); err != nil || slices.ContainsFunc(threads, func(th trace.Thread) bool { return th.Identity.Role == intake.CommitteeRole }) {
		t.Fatalf("the abandoned workstream has a committee: %+v %v", threads, err)
	}
	for _, path := range []string{"handed/stdin", plan.SpecPath, plan.PlanPath} {
		_, err := fixturekit.WorkstreamDocument(t, f.S.Current().Root, f.Project, gone, path)
		fixturekit.Must(t, err)
	}
	if refs := fixturekit.DemoGit(t, h.home, "-C", h.clone, "for-each-ref", "--format=%(refname)", "refs/heads/osmia/"+string(gone)); strings.TrimSpace(refs) != "" {
		t.Fatalf("the abandoned workstream has a branch: %s", refs)
	}
	if _, err := os.Stat(filepath.Join(f.Opts.Config.Root, "branches", string(f.Project), string(gone))); !os.IsNotExist(err) {
		t.Fatalf("the abandoned workstream has a workspace: %v", err)
	}
	h.checkNothingPushedUpstream(t)
	h.checkFakeAgents(t)
}
