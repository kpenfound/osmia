package intakejourney

import (
	context "context"
	errors "errors"
	fmt "fmt"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/journey"
	os "os"
	filepath "path/filepath"
	slices "slices"
	strings "strings"
	sync "sync"
	testing "testing"
	time "time"

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

func TestHandInToRatifiedPlan(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	opts, clone, engine, sessions, clock := journey.NewArchitectOptions(t)
	home := filepath.Dir(clone)
	root := opts.Config.Root
	configFile, err := os.OpenFile(filepath.Join(root, "config.toml"), os.O_APPEND|os.O_WRONLY, 0)
	fixturekit.Must(t, err)
	_, err = configFile.WriteString(exitDemoRoles)
	fixturekit.Must(t, errors.Join(err, configFile.Close()))

	engine.Resume = func(coreadapter.Profile, coreadapter.Profile, coreadapter.BackendSession) error { return nil }

	hosts := &coreadapter.MCPHost{Transport: &fixturekit.DemoTransport{Sessions: sessions}}
	opts = journey.Enforce(opts, service.Enforcement{Engine: engine, Hosts: hosts})
	f := &journey.ShedFixture{ArchitectFixture: &journey.ArchitectFixture{Opts: opts, Clone: clone, Engine: engine, Sessions: sessions, Clock: clock}, Members: 2}

	d := &intakefixture.ExitDemo{Plays: map[string]map[string]fixturekit.DemoTurn{}, Ran: map[string][]string{}, Prompts: map[string]string{}}
	veto, size := shed.ObjectionID(1, intake.CommitteeAgent(1), 1), shed.ObjectionID(1, intake.CommitteeAgent(1), 2)
	cappedVeto := veto
	chargeVeto := intakefixture.Objection{Kind: shed.Charter, Part: "spec#2", Argument: "Criterion 2 is shown by judgement, not by a test.", Citation: "charter#2"}
	quiet := d.Debates(nil)
	d.Plays[intakefixture.ExitTitle(intakefixture.ExitDebated)] = map[string]fixturekit.DemoTurn{
		"draft-1-1": d.Drafts(map[string]string{plan.SpecPath: fixturekit.ValidSpec, plan.PlanPath: fixturekit.ValidPlan}, nil),
		intake.RoundTurnID(1, intake.CommitteeAgent(1), 1): d.Debates([]intakefixture.Objection{chargeVeto, {Kind: shed.Size, Part: "plan#resume", Argument: "Resuming is two changes, not one.", Citation: "kb/entities.json#internal.trace"}}),
		intake.RoundTurnID(1, intake.CommitteeAgent(2), 1): quiet,
		intake.ReplyTurnID(1, 1):                           d.Drafts(map[string]string{plan.PlanPath: splitPlan}, map[string]string{veto: "Criterion 2 now has TestNoChunkTwice.", size: "Resuming is split in two units."}),
		intake.RoundTurnID(2, intake.CommitteeAgent(1), 1): d.Debates(nil, veto, size),
		intake.RoundTurnID(2, intake.CommitteeAgent(2), 1): quiet,
	}
	d.Plays[intakefixture.ExitTitle(intakefixture.ExitCapped)] = map[string]fixturekit.DemoTurn{
		"draft-1-1": d.Drafts(map[string]string{plan.SpecPath: fixturekit.ValidSpec, plan.PlanPath: fixturekit.ValidPlan}, nil),
		intake.RoundTurnID(1, intake.CommitteeAgent(1), 1): d.Debates([]intakefixture.Objection{chargeVeto}),

		intake.RoundTurnID(1, intake.CommitteeAgent(2), 1): func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
			if stream, ok := d.StreamOf(intakefixture.ExitCapped); !ok {
				d.Problem("the member asked before its workstream was known")
			} else if state, err := f.Repository().Workflow(stream, trace.FeatureSubject); err != nil || state.Value != intake.InShedState {
				d.Problem("the member asked with the workstream %q %v, not in the shed", state.Value, err)
			}
			if out, err := fixturekit.CallTool(ctx, tools, questions.AskTool, map[string]any{"question": intakefixture.ExitQuestion}); err != nil || !strings.Contains(out, `"recorded":true`) {
				d.Problem("ask: %s %v", out, err)
			}
			return &agent.Result{ClaudeID: "session-" + req.Name, ResultText: "Asked", SessionDir: req.SessionDir, NumTurns: 2}, nil
		},
		questions.TurnID("1"):                              quiet,
		intake.ReplyTurnID(1, 1):                           d.Drafts(nil, map[string]string{cappedVeto: "The reviewer's judgement is the right proof here."}),
		intake.RoundTurnID(2, intake.CommitteeAgent(1), 1): quiet,
		intake.RoundTurnID(2, intake.CommitteeAgent(2), 1): quiet,
		intake.ReplyTurnID(2, 1):                           d.Drafts(nil, nil),
	}
	d.Plays[intakefixture.ExitTitle(intakefixture.ExitSmall)] = map[string]fixturekit.DemoTurn{"draft-1-1": d.Drafts(map[string]string{plan.SpecPath: intakefixture.ExitSmallSpec, plan.PlanPath: intakefixture.ExitSmallPlan}, nil)}
	d.Plays[intakefixture.ExitTitle(intakefixture.ExitAbandoned)] = map[string]fixturekit.DemoTurn{"draft-1-1": d.Drafts(map[string]string{plan.SpecPath: fixturekit.ValidSpec, plan.PlanPath: fixturekit.ValidPlan}, nil)}
	for _, plays := range d.Plays {
		for name := range plays {
			engine.Turns[name] = d.Route
		}
	}
	engine.Turns["*"] = d.Chief
	awaitAttention := func(stream config.WorkstreamID, want string) api.StatusView {
		t.Helper()
		var status api.StatusView
		f.S.Until(t, fmt.Sprintf("workstream %s to show the attention %q", stream, want), func() bool {
			st, err := f.C.Status(ctx, stream)
			fixturekit.Must(t, err)
			if st.Status == nil || st.Status.Attention != want {
				return false
			}
			status = *st.Status
			return true
		})
		return status
	}
	sealOf := func(stream config.WorkstreamID) seal.Seal {
		t.Helper()
		record, doc, found, err := seal.Latest(f.Repository(), stream)
		fixturekit.Must(t, err)
		if !found || doc.Revision != 1 || len(f.Documents(t, stream, seal.DocumentID)) != 1 {
			t.Fatalf("seal of workstream %s: %v %+v", stream, found, doc)
		}
		return record
	}
	branchAt := func(stream config.WorkstreamID) string {
		t.Helper()
		return strings.TrimSpace(fixturekit.DemoGit(t, home, "-C", clone, "rev-parse", "refs/heads/osmia/"+string(stream)))
	}

	f.Start(t)
	added, err := f.C.AddProject(ctx, fixturekit.Request(clone))
	fixturekit.Must(t, err)
	f.Project, f.Trace, d.Trace, d.Root, d.Project = added.Project.ID, added.Project.Trace, added.Project.Trace, f.S.Current().Root, added.Project.ID
	journey.WriteCharter(t, f.C, added.Project.ID, intakefixture.ExitCharter)
	onboarded, err := f.C.Configuration(ctx)
	fixturekit.Must(t, err)
	if cs := onboarded.Project.CharterState; cs == nil || cs.Rules != 2 {
		t.Fatalf("charter: %+v", cs)
	}
	entities, err := kb.Load(f.Repository())
	fixturekit.Must(t, err)
	footprint := entities.ResolveEntities([]string{"internal.trace"})
	if len(footprint.Entities) != 1 || len(footprint.Paths) == 0 || len(footprint.Unresolved) != 0 {
		t.Fatalf("the local entity map resolves internal.trace to %+v", footprint)
	}
	commit := f.Upstream(t)

	handIn := func(key, design string, skip bool) config.WorkstreamID {
		t.Helper()
		out, err := f.C.HandIn(ctx, api.HandInRequest{Project: f.Project, Key: key, Stdin: intakefixture.Ptr(design), SkipDebate: skip})
		fixturekit.Must(t, err)
		if out.State != intake.HandedState || out.SkipDebate != skip {
			t.Fatalf("hand-in %+v", out)
		}
		return out.Workstream
	}
	debated, capped := handIn("debated", intakefixture.ExitDebated, false), handIn("capped", intakefixture.ExitCapped, false)
	small, gone := handIn("small", intakefixture.ExitSmall, true), handIn("abandoned", intakefixture.ExitAbandoned, true)
	d.Mu.Lock()
	d.Streams = map[string]config.WorkstreamID{intakefixture.ExitTitle(intakefixture.ExitDebated): debated, intakefixture.ExitTitle(intakefixture.ExitCapped): capped, intakefixture.ExitTitle(intakefixture.ExitSmall): small, intakefixture.ExitTitle(intakefixture.ExitAbandoned): gone}
	d.Mu.Unlock()

	for _, stream := range []config.WorkstreamID{debated, capped, small, gone} {
		f.AwaitFeature(t, stream, intake.InShedState)
		if moved := f.Transition(t, stream, intake.SketchedState); moved.From != intake.HandedState || moved.Actor != intake.DraftingActor {
			t.Fatalf("the move to sketched %+v", moved)
		}
	}
	for stream, want := range map[config.WorkstreamID]string{
		debated: "spec.md revision 1 and plan.json revision 1 enter the shed with a committee of 2",
		small:   "spec.md revision 1 and plan.json revision 1 enter the shed without a committee: the owner skipped debate",
	} {
		if enter := f.Transition(t, stream, intake.InShedState); enter.From != intake.SketchedState || enter.Reason != want {
			t.Fatalf("workstream %s entered the shed %+v", stream, enter)
		}
	}
	list, err := f.C.Statuses(ctx)
	fixturekit.Must(t, err)
	if len(list.Workstreams) != 4 {
		t.Fatalf("statuses: %+v", list)
	}
	for _, st := range list.Workstreams {
		if st.State == nil || *st.State != intake.InShedState {
			t.Fatalf("status of %s: %+v", st.Workstream, st)
		}
	}

	abandoned, err := f.C.Abandon(ctx, gone, "Quotas wait for the billing work.")
	fixturekit.Must(t, err)
	if abandoned.State != intake.AbandonedState || abandoned.Reason != "Quotas wait for the billing work." {
		t.Fatalf("abandon %+v", abandoned)
	}

	packet := f.AwaitPacket(t, small, "ratify: no objection stands")
	if !packet.Skipped || packet.Revision != (shed.Pin{Spec: 1, Plan: 1}) || len(packet.Dissent) != 0 {
		t.Fatalf("the skipped debate's packet %+v", packet)
	}
	awaitAttention(small, intakefixture.ExitAttention(packet.Recommendation))
	if out, err := f.C.Ratify(ctx, small, 1, 1); err != nil || out.Sealing != "requested" {
		t.Fatalf("ratifying the skipped debate: %+v %v", out, err)
	}
	f.AwaitFeature(t, small, intake.BuildingState)
	if s := sealOf(small); s.Base.Commit != commit || s.SpecHash != seal.SpecHash(intakefixture.ExitSmallSpec) || len(s.Footprints) != 1 || s.Footprints[0].Unit != "message" || branchAt(small) != commit {
		t.Fatalf("the small workstream's seal %+v", s)
	}
	if ran := slices.DeleteFunc(d.Runs(intakefixture.ExitTitle(intakefixture.ExitSmall)), func(name string) bool { return name == "events" }); !slices.Equal(ran, []string{"draft-1-1"}) {
		t.Fatalf("the small workstream ran %v", ran)
	}

	escalations := func() []api.InboxEntry {
		inbox, err := f.C.Inbox(ctx)
		fixturekit.Must(t, err)
		var out []api.InboxEntry
		for _, e := range inbox.Entries {
			if e.Kind == api.InboxEscalation {
				out = append(out, e)
			}
		}
		return out
	}
	var entry api.InboxEntry
	f.S.Until(t, "exactly one escalation", func() bool {
		entries := escalations()
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
	d.Mu.Lock()
	answered := d.Prompts[intakefixture.ExitTitle(intakefixture.ExitCapped)+"/"+questions.TurnID("1")]
	d.Mu.Unlock()
	if !strings.Contains(answered, "| "+intakefixture.ExitQuestion) || !strings.Contains(answered, "<<< osmia:owner_response") || !strings.Contains(answered, "| "+intakefixture.ExitRelayed) {
		t.Fatalf("the member heard:\n%s", answered)
	}
	if entries := escalations(); len(entries) != 0 {
		t.Fatalf("escalations after the answer %+v", entries)
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
	packet = f.AwaitPacket(t, debated, "ratify: no objection stands")
	if packet.Revision != (shed.Pin{Spec: 1, Plan: 2}) || len(packet.Dissent) != 0 {
		t.Fatalf("the debated packet %+v", packet)
	}
	awaitAttention(debated, intakefixture.ExitAttention(packet.Recommendation))
	if out, err := f.C.Ratify(ctx, debated, 1, 2); err != nil || out.Sealing != "requested" {
		t.Fatalf("ratifying the debated workstream: %+v %v", out, err)
	}
	f.AwaitFeature(t, debated, intake.BuildingState)
	s := sealOf(debated)
	if s.Base != (seal.Base{Remote: "upstream", Branch: "main", Commit: commit}) || s.SpecHash != seal.SpecHash(fixturekit.ValidSpec) || s.Revision != (shed.Pin{Spec: 1, Plan: 2}) || s.Branch != "osmia/"+string(debated) {
		t.Fatalf("the debated seal %+v", s)
	}
	var units []string
	for _, fp := range s.Footprints {
		units = append(units, fp.Unit)
		if !slices.Equal(fp.Entities, footprint.Entities) || !slices.Equal(fp.Paths, footprint.Paths) {
			t.Fatalf("footprint %+v, want %+v", fp, footprint)
		}
	}
	if !slices.Equal(units, []string{"resume-read", "resume-write", "dedupe"}) || branchAt(debated) != commit {
		t.Fatalf("the debated seal's units %v and branch", units)
	}
	if on := strings.TrimSpace(fixturekit.DemoGit(t, home, "-C", s.Workspace, "rev-parse", "--abbrev-ref", "HEAD")); on != s.Branch {
		t.Fatalf("the workspace is on %s", on)
	}

	f.AwaitShed(t, capped, "concluded-2")
	if moves := f.ShedMoves(t, capped); len(moves) < 4 || !slices.Equal(moves[:4], []string{"round-1", "waiting-1", "round-1", "heard-1"}) {
		t.Fatalf("shed of the retention workstream went %v", moves)
	}
	if end := f.Transition(t, capped, "shed-concluded-2"); end.Reason != "debate stopped after round 2, at the shed.max_rounds cap of 2, with 1 objection standing, 1 of them blocking; the cap approves nothing" {
		t.Fatalf("the conclusion %+v", end)
	}
	blocked := fmt.Sprintf("do not ratify yet: ratification is blocked by 1 objection (%s); overrule or sustain each one, or ask for a redraft", cappedVeto)
	f.AwaitPacket(t, capped, blocked)
	awaitAttention(capped, intakefixture.ExitAttention(blocked))
	_, err = f.C.Ratify(ctx, capped, 1, 1)
	if !intakefixture.Failed(err, api.Conflict) || !strings.Contains(err.Error(), fmt.Sprintf("objection %s (charter, by %s in round 1 on spec#2) blocks and has no disposition", cappedVeto, intake.CommitteeAgent(1))) {
		t.Fatalf("ratifying over the veto: %v", err)
	}
	if _, err := f.C.ShedOverrule(ctx, capped, cappedVeto, intakefixture.ExitOverrule); err != nil {
		t.Fatal(err)
	}
	packet = f.AwaitPacket(t, capped, "ratify: nothing blocks, and 1 objection stands as advice on the record")
	if len(packet.Dissent) != 1 || packet.Dissent[0].Disposition != shed.Overruled || packet.Dissent[0].Revision != (shed.Pin{Spec: 1, Plan: 1}) {
		t.Fatalf("the packet after the overrule %+v", packet)
	}

	rulings := f.Documents(t, capped, shed.RulingsDocumentID(2))
	if len(rulings) != 1 || rulings[0].Actor != intake.OwnerActor {
		t.Fatalf("the owner's rulings %+v", rulings)
	}
	ruled, err := shed.ParseRulings([]byte(rulings[0].Content))
	fixturekit.Must(t, err)
	if ruled.Revision != (shed.Pin{Spec: 1, Plan: 1}) || !slices.Equal(ruled.Rulings, []shed.Ruling{{Objection: cappedVeto, Disposition: shed.Overruled, Note: intakefixture.ExitOverrule}}) {
		t.Fatalf("the overrule %+v", ruled)
	}

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
	if branchAt(capped) != commit {
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
	defer f.Stop(t)
	f.AwaitRecoveredSeal(t, capped)
	record := f.Ratification(t, capped, 2)
	if record.Revision != (shed.Pin{Spec: 1, Plan: 1}) || len(record.Dispositions) != 1 || record.Dispositions[0] != (shed.Ruling{Objection: cappedVeto, Disposition: shed.Overruled, Note: intakefixture.ExitOverrule}) {
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
	if s := sealOf(capped); s.Seal != 1 || s.Base != (seal.Base{Remote: "upstream", Branch: "main", Commit: commit}) || s.SpecHash != seal.SpecHash(fixturekit.ValidSpec) || s.Revision != (shed.Pin{Spec: 1, Plan: 1}) || s.Branch != intake.FeatureBranch(capped) || branchAt(capped) != commit {
		t.Fatalf("the capped seal %+v", s)
	}

	if out := fixturekit.DemoGit(t, home, "-C", clone, "worktree", "list", "--porcelain"); strings.Count(out, "worktree ")-strings.Count(out, "worktree "+filepath.Join(f.Opts.Config.Root, intake.UnitsDirectory)+string(filepath.Separator)) != 4 {
		t.Fatalf("worktrees:\n%s", out)
	}

	if refs := fixturekit.DemoGit(t, home, "-C", filepath.Join(home, "remotes", "dagger", "dagger.git"), "for-each-ref", "--format=%(refname)"); strings.TrimSpace(refs) != "refs/heads/main" {
		t.Fatalf("upstream's refs:\n%s", refs)
	}

	if state, err := f.Repository().Workflow(gone, trace.FeatureSubject); err != nil || state.Value != intake.AbandonedState {
		t.Fatalf("the abandoned workstream %+v %v", state, err)
	}
	if ran := slices.DeleteFunc(d.Runs(intakefixture.ExitTitle(intakefixture.ExitAbandoned)), func(name string) bool { return name == "events" }); !slices.Equal(ran, []string{"draft-1-1"}) {
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
	if refs := fixturekit.DemoGit(t, home, "-C", clone, "for-each-ref", "--format=%(refname)", "refs/heads/osmia/"+string(gone)); strings.TrimSpace(refs) != "" {
		t.Fatalf("the abandoned workstream has a branch: %s", refs)
	}
	if _, err := os.Stat(filepath.Join(root, "branches", string(f.Project), string(gone))); !os.IsNotExist(err) {
		t.Fatalf("the abandoned workstream has a workspace: %v", err)
	}
	d.Mu.Lock()
	defer d.Mu.Unlock()
	if len(d.Problems) != 0 {
		t.Fatalf("fake agents saw:\n%s", strings.Join(d.Problems, "\n"))
	}
}
