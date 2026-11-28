package journey

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/cli"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/jev"
	"github.com/kpenfound/osmia/internal/jev/jevfixture"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ShedFixture is an architect fixture whose service also runs the committee,
// in a container, with the given number of members.
type ShedFixture struct {
	*ArchitectFixture
	Members int
}

// NewDebateFixture debates for at most the given number of rounds. The
// architect's replies are silent unless a test scripts them.
func NewDebateFixture(t *testing.T, members, rounds int) *ShedFixture {
	t.Helper()
	return NewDebateFixtureWith(t, members, rounds, "", nil)
}

func (f *ShedFixture) StopAtSketch(t *testing.T, stream config.WorkstreamID) {
	t.Helper()
	f.Await(t, stream, intakefixture.Sketched)
	f.Stop(t)
	f.S.Options.Committee = f.Opts.Committee
}

// NewDebateFixtureWith is NewDebateFixture with extra configuration appended
// and prepare, when given, adjusting the options before the service starts.
func NewDebateFixtureWith(t *testing.T, members, rounds int, extra string, prepare func(*Options)) *ShedFixture {
	t.Helper()
	opts, clone, engine, sessions, clock := NewArchitectOptions(t)
	intakefixture.ConfigureDebate(t, opts.Config.Root, members, rounds, extra)
	opts.Committee = &intake.Committee{Engine: engine, Hosts: opts.Architect.Hosts}
	if prepare != nil {
		prepare(&opts)
	}
	f := &ArchitectFixture{Opts: opts, Clone: clone, Engine: engine, Sessions: sessions, Clock: clock}
	f.Start(t)
	f.Project, f.Trace = intakefixture.AddArchitectProject(t, f.C, clone, intakefixture.ShedCharter, func(t *testing.T, p config.ProjectID, text string) { intakefixture.WriteCharter(t, f.C, p, text) })
	f.S.Cfg = f.S.Current()
	f.Scenario().ScriptDebate(rounds)
	return &ShedFixture{ArchitectFixture: f, Members: members}
}

// Member installs the fake behaviour of one member's turn. Errors it returns
// fail the turn, so checks collect theirs for the test to read.
func (f *ShedFixture) Member(round, i, attempt int, run func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) error) string {
	return f.Scenario().Member(round, i, attempt, run)
}

// AwaitShed waits until the workstream's shed has reached the wanted state,
// whatever it moved to since. It fails at once when the shed reaches a state
// in never, after which the wanted one cannot come: by default, for a heard
// round, its failure, and for a conclusion, the next round.
func (f *ShedFixture) AwaitShed(t *testing.T, stream config.WorkstreamID, want string, never ...string) {
	t.Helper()
	f.Scenario().AwaitShed(t, stream, want, never...)
}

func (f *ShedFixture) RoundOperations(t *testing.T, stream config.WorkstreamID) []trace.OperationRecord {
	t.Helper()
	return f.Scenario().RoundOperations(t, stream)
}

// AcknowledgedRoundOperations returns the round operations once every one of
// them is acknowledged, for assertions reached through an awaited state.
func (f *ShedFixture) AcknowledgedRoundOperations(t *testing.T, stream config.WorkstreamID) []trace.OperationRecord {
	t.Helper()
	return f.Scenario().AcknowledgedRoundOperations(t, stream)
}

// SetCommittee rewrites capacity.committee in the fixture's config.toml.
func (f *ShedFixture) SetCommittee(t *testing.T, members int) {
	t.Helper()
	path := filepath.Join(f.Opts.Config.Root, "config.toml")
	data, err := os.ReadFile(path)
	fixturekit.Must(t, err)
	old := fmt.Sprintf("committee = %d\n", f.Members)
	if !strings.Contains(string(data), old) {
		t.Fatalf("config.toml lacks %q", old)
	}
	fixturekit.Must(t, os.WriteFile(path, []byte(strings.Replace(string(data), old, fmt.Sprintf("committee = %d\n", members), 1)), 0600))
	f.Members = members
}

// Answer installs the fake behaviour of the turn that delivers the answer to
// question id to its asker.
func (f *ShedFixture) Answer(id string, run fixturekit.FakeTurn) string {
	return intakefixture.Answer(f.Engine, id, run)
}

// Rule has the owner rule on the inbox entry that escalated question id, once
// it is there.
func (f *ShedFixture) Rule(t *testing.T, id string) {
	t.Helper()
	intakefixture.Rule(t, f.C, id)
}

func (f *ShedFixture) Thread(t *testing.T, stream config.WorkstreamID, member string) trace.Thread {
	t.Helper()
	return intakefixture.Thread(t, f.Repository(), stream, member)
}

func (f *ShedFixture) Question(t *testing.T, stream config.WorkstreamID, id string) trace.QuestionState {
	t.Helper()
	return intakefixture.Question(t, f.Repository(), stream, id)
}

// AwaitQuestion waits until question id is in the given state.
func (f *ShedFixture) AwaitQuestion(t *testing.T, stream config.WorkstreamID, id, state string) {
	t.Helper()
	intakefixture.AwaitQuestion(t, f.Repository(), stream, id, state)
}

// AwaitTurn waits until the member's thread holds the given turn.
func (f *ShedFixture) AwaitTurn(t *testing.T, stream config.WorkstreamID, member, turn string) trace.QueuedTurn {
	t.Helper()
	return intakefixture.AwaitNamedTurn(t, f.Repository(), stream, member, turn)
}

// RoundOperation returns the acknowledged round operation with the given
// resumption number.
func (f *ShedFixture) RoundOperation(t *testing.T, stream config.WorkstreamID, resume int) trace.OperationRecord {
	t.Helper()
	ops := f.AcknowledgedRoundOperations(t, stream)
	for _, o := range ops {
		if in, err := intake.DecodeRound(o.Operation); err == nil && in.Resume == resume {
			return o
		}
	}
	t.Fatalf("no round operation with resumption %d: %v", resume, intakefixture.Results(ops))
	return trace.OperationRecord{}
}

func (f *ShedFixture) Record(t *testing.T, stream config.WorkstreamID, member string) shed.Record {
	t.Helper()
	records, err := shed.Records(f.Repository(), stream)
	fixturekit.Must(t, err)
	for _, r := range records {
		if r.Member == member {
			return r
		}
	}
	t.Fatalf("no record of %s: %+v", member, records)
	return shed.Record{}
}

// OwnerRecord returns the owner's objections recorded in a round.
func (f *ShedFixture) OwnerRecord(t *testing.T, stream config.WorkstreamID, round int) shed.Record {
	t.Helper()
	docs := f.Documents(t, stream, shed.DocumentID(round, shed.OwnerMember))
	if len(docs) == 0 {
		t.Fatalf("round %d holds no record of the owner", round)
	}
	latest := docs[len(docs)-1]
	if latest.Path != shed.Path(round, shed.OwnerMember) || latest.Actor != intake.OwnerActor || latest.Cause != "owner-shed" {
		t.Fatalf("owner record %+v", latest)
	}
	record, err := shed.Parse([]byte(latest.Content))
	fixturekit.Must(t, err)
	return record
}

// OwnerMoves lists the states the workstream's owner subject went through.
func (f *ShedFixture) OwnerMoves(t *testing.T, stream config.WorkstreamID) []string {
	t.Helper()
	return intakefixture.OwnerMoves(t, f.Transitions(t, stream))
}

func (f *ShedFixture) Dissent(t *testing.T, stream config.WorkstreamID) []shed.Entry {
	t.Helper()
	entries, err := intake.Dissent(f.Repository(), stream)
	fixturekit.Must(t, err)
	return entries
}

// AwaitDocument waits until the workstream records that revision of a
// document.
func (f *ShedFixture) AwaitDocument(t *testing.T, stream config.WorkstreamID, id string, revision int) {
	t.Helper()
	f.S.Until(t, fmt.Sprintf("workstream %s to record revision %d of %s", stream, revision, id), func() bool {
		docs := f.Documents(t, stream, id)
		return len(docs) > 0 && docs[len(docs)-1].Revision >= revision
	})
}

// ShedMoves lists the states the workstream's shed went through.
func (f *ShedFixture) ShedMoves(t *testing.T, stream config.WorkstreamID) []string {
	t.Helper()
	var moves []string
	for _, tr := range f.Transitions(t, stream) {
		if tr.Subject == intake.ShedSubject {
			moves = append(moves, tr.To)
		}
	}
	return moves
}

func (f *ShedFixture) Transition(t *testing.T, stream config.WorkstreamID, id string) trace.Transition {
	t.Helper()
	for _, tr := range f.Transitions(t, stream) {
		if tr.ID == id {
			return tr
		}
	}
	t.Fatalf("transition %s is not recorded", id)
	return trace.Transition{}
}

func (f *ShedFixture) ReplyOperations(t *testing.T, stream config.WorkstreamID) []trace.OperationRecord {
	t.Helper()
	ops, err := f.Repository().Operations(stream)
	fixturekit.Must(t, err)
	return slices.DeleteFunc(ops, func(o trace.OperationRecord) bool { return o.Operation.Action != intake.ReplyAction })
}

// AcknowledgedReplies returns the reply operations once every one of them is
// acknowledged, for assertions reached through an awaited shed state.
func (f *ShedFixture) AcknowledgedReplies(t *testing.T, stream config.WorkstreamID) []trace.OperationRecord {
	t.Helper()
	return fixturekit.AwaitAcknowledged(t, func(t *testing.T) []trace.OperationRecord { return f.ReplyOperations(t, stream) })
}

// SettledReplies returns the reply operations once each has its result: the
// shed moves before the operation that moved it records what it did.
func (f *ShedFixture) SettledReplies(t *testing.T, stream config.WorkstreamID) []trace.OperationRecord {
	t.Helper()
	var ops []trace.OperationRecord
	f.S.Until(t, "every reply operation to have a result", func() bool {
		ops = f.ReplyOperations(t, stream)
		return !slices.ContainsFunc(ops, func(o trace.OperationRecord) bool { return o.Result == nil })
	})
	return ops
}

// Ran counts how often the backend ran each turn.
func (f *ShedFixture) Ran() map[string]int {
	out := map[string]int{}
	for _, turn := range f.Runs() {
		out[turn]++
	}
	return out
}

func (f *ShedFixture) Reply(t *testing.T, stream config.WorkstreamID, round int) shed.Reply {
	t.Helper()
	docs := f.Documents(t, stream, shed.ReplyDocumentID(round))
	if len(docs) != 1 || docs[0].Path != shed.ReplyPath(round) || docs[0].Actor != intake.ArchitectActor {
		t.Fatalf("reply documents of round %d: %+v", round, docs)
	}
	reply, err := shed.ParseReply([]byte(docs[0].Content))
	fixturekit.Must(t, err)
	return reply
}

// StillInShed asserts that the debate's end moved the workstream nowhere.
func (f *ShedFixture) StillInShed(t *testing.T, stream config.WorkstreamID) {
	t.Helper()
	if state, err := f.Repository().Workflow(stream, trace.FeatureSubject); err != nil || state.Value != intake.InShedState {
		t.Fatalf("feature %+v %v, want %s", state, err, intake.InShedState)
	}
	if status, err := f.C.Status(context.Background(), stream); err != nil || status.State == nil || *status.State != intake.InShedState {
		t.Fatalf("status: %+v %v", status, err)
	}
	for _, tr := range f.Transitions(t, stream) {
		if tr.Subject == trace.FeatureSubject && tr.From == intake.InShedState {
			t.Fatalf("the workstream left the shed: %+v", tr)
		}
	}
}

// Concluded returns the notice the conclusion after round n told the manager of
// staff with.
func (f *ShedFixture) Concluded(t *testing.T, stream config.WorkstreamID, n int) trace.OutboxEntry {
	t.Helper()
	id := fmt.Sprintf("shed-concluded-%d", n)
	outbox, err := f.Repository().Outbox(stream)
	fixturekit.Must(t, err)
	for _, entry := range outbox {
		if entry.Event.ID == trace.EventID(id, "concluded") {
			if entry.TransitionID != id || entry.Event.Kind != trace.NoticeKind {
				t.Fatalf("conclusion notice: %+v", entry)
			}
			return entry
		}
	}
	t.Fatalf("the outbox holds no conclusion of round %d: %+v", n, outbox)
	return trace.OutboxEntry{}
}

// SetRounds rewrites shed.max_rounds in the fixture's config.toml.
func (f *ShedFixture) SetRounds(t *testing.T, from, to int) {
	t.Helper()
	path := filepath.Join(f.Opts.Config.Root, "config.toml")
	data, err := os.ReadFile(path)
	fixturekit.Must(t, err)
	old := fmt.Sprintf("max_rounds = %d\n", from)
	if !strings.Contains(string(data), old) {
		t.Fatalf("config.toml lacks %q", old)
	}
	fixturekit.Must(t, os.WriteFile(path, []byte(strings.Replace(string(data), old, fmt.Sprintf("max_rounds = %d\n", to), 1)), 0600))
}

// HandInSkipping hands the design in with debate skipped.
func (f *ShedFixture) HandInSkipping(t *testing.T, key string) config.WorkstreamID {
	t.Helper()
	return f.Scenario().HandInSkipping(t, key)
}

// DebatedNothing checks that the workstream has no committee, and that no
// round, reply or committee turn was ever asked for or run.
func (f *ShedFixture) DebatedNothing(t *testing.T, stream config.WorkstreamID) {
	t.Helper()
	if ops := f.RoundOperations(t, stream); len(ops) != 0 {
		t.Fatalf("round operations %+v", ops)
	}
	if ops := f.ReplyOperations(t, stream); len(ops) != 0 {
		t.Fatalf("reply operations %+v", ops)
	}
	if moves := f.ShedMoves(t, stream); len(moves) != 0 {
		t.Fatalf("shed moves %v", moves)
	}
	threads, err := f.Repository().Threads(stream)
	fixturekit.Must(t, err)
	for _, th := range threads {
		if th.Identity.Role == intake.CommitteeRole {
			t.Fatalf("committee thread %+v", th.Identity)
		}
	}
	for turn := range f.Ran() {
		if strings.HasPrefix(turn, "shed-") {
			t.Fatalf("turn %s ran", turn)
		}
	}
}

// FeatureMoves lists the feature transitions of the workstream as ID:state.
func (f *ShedFixture) FeatureMoves(t *testing.T, stream config.WorkstreamID) []string {
	t.Helper()
	var moves []string
	for _, tr := range f.Transitions(t, stream) {
		if tr.Subject == trace.FeatureSubject {
			moves = append(moves, tr.ID+":"+tr.To)
		}
	}
	return moves
}

// ReachJevPacket hands in a design whose debate concludes by consensus and
// returns once the shed has concluded. Nobody ratifies it.
func (f *ShedFixture) ReachJevPacket(t *testing.T) config.WorkstreamID {
	t.Helper()
	f.Upstream(t)
	faults := &intakefixture.Faults{}
	f.Member(1, 1, 1, intakefixture.Concedes(faults))
	stream := f.HandIn(t, "design", fixturekit.HandedDesign)
	f.AwaitShed(t, stream, "concluded-1")
	faults.Check(t)
	return stream
}

// RunCLI runs an osmia command against the service of the fixture.
func (f *ShedFixture) RunCLI(t *testing.T, args ...string) string {
	t.Helper()
	root, err := config.ResolveRoot(f.Opts.Config.Root, "")
	fixturekit.Must(t, err)
	var out, diag bytes.Buffer
	code := cli.Run(context.Background(), append([]string{"--root", root.String()}, args...), strings.NewReader(""), &out, &diag)
	if code != 0 || diag.Len() != 0 {
		t.Fatalf("%v: code=%d stdout=%s stderr=%s", args, code, out.String(), diag.String())
	}
	return out.String()
}

// ManagerDecides records the manager's decision on one committee
// objection, as its resolve_objection tool records it.
func (f *ShedFixture) ManagerDecides(t *testing.T, stream config.WorkstreamID, objection string, decision shed.Decide, note string) shed.Entry {
	t.Helper()
	entry, apiErr := f.S.DecideObjection(context.Background(), f.Repository(), stream, intake.ObjectionDecisionRequest{Objection: objection, Decision: decision, Note: note, Turn: "events_1"})
	if apiErr != nil {
		t.Fatal(apiErr.Message)
	}
	return entry
}

// AwaitDecided waits until the packet the API serves carries a decision of
// the manager or a disposition on every committee objection of its
// dissent record, and returns it with its revision.
func (f *ShedFixture) AwaitDecided(t *testing.T, stream config.WorkstreamID) (shed.Packet, int) {
	t.Helper()
	wait, close := ServiceChanges(t, f.S)
	defer close()
	deadline := time.Now().Add(fixturekit.DemoTimeout)
	for {
		out, err := f.C.Packet(context.Background(), stream)
		if err != nil && !intakefixture.Failed(err, api.NotFound) {
			t.Fatal(err)
		}
		if err == nil && len(shed.Undecided(out.Packet.Dissent)) == 0 {
			return out.Packet, out.Revision
		}
		if time.Now().After(deadline) {
			t.Fatalf("the packet of workstream %s is %+v %v, want every committee objection decided", stream, out.Packet, err)
		}
		wait()
	}
}

// ManagerObjectionTool claims a manager turn of the workstream and
// returns its resolve_objection tool.
func (f *ShedFixture) ManagerObjectionTool(t *testing.T, stream config.WorkstreamID) coreadapter.Tool {
	t.Helper()
	repo := f.Repository()
	scope := intakefixture.ClaimManagerTurn(t, repo, f.Project, stream, f.Clock.Now, intakefixture.ManagerTurnInput{
		Turn: "events_1", Prompt: "Debate concluded.", Actor: intake.ServiceActor, Cause: "fixture", Token: "token-events", Directory: "events",
	})
	controls := &RuntimeControls{}
	controls.Service.Store(f.S)
	return controls.ResolveObjection(repo, scope)
}

// ObjectionDecided leaves the decide result of the objection as a fallback, as
// the decide tool does while Jev is unconfigured.
func (f *ShedFixture) ObjectionDecided(t *testing.T, stream config.WorkstreamID, objection string) {
	t.Helper()
	jevfixture.Fallback(f.Repository(), f.Project, stream, jev.KindObjection, jev.ObjectionKey(objection))
}

// ManagerDecideTool claims a manager turn of the workstream and returns
// the service's decide tool.
func (f *ShedFixture) ManagerDecideTool(t *testing.T, stream config.WorkstreamID, turn string) coreadapter.Tool {
	t.Helper()
	repo := f.Repository()
	scope := intakefixture.ClaimManagerTurn(t, repo, f.Project, stream, f.Clock.Now, intakefixture.ManagerTurnInput{
		Turn: turn, Prompt: "Debate concluded.", Actor: intake.ServiceActor, Cause: "fixture", Token: "token-" + turn, Directory: turn,
	})
	return service.InspectForTest(f.S.Service).DecideTool(repo, scope)
}

// ObjectionTransition returns the latest transition of the objection's subject.
func (f *ShedFixture) ObjectionTransition(t *testing.T, stream config.WorkstreamID, objection string) trace.Transition {
	t.Helper()
	transitions, err := trace.Read[trace.Transition](f.Repository(), stream)
	fixturekit.Must(t, err)
	var out trace.Transition
	for _, tr := range transitions {
		if tr.Subject == "objection-"+objection {
			out = tr
		}
	}
	return out
}

// AwaitRecoveredSeal watches the operation that must finish before the
// workstream can start building. Reads run separately so a trace read waiting
// behind publication cannot hide the test deadline.
func (f *ShedFixture) AwaitRecoveredSeal(t *testing.T, stream config.WorkstreamID) {
	t.Helper()
	type snapshot struct {
		feature, sealing trace.WorkflowState
		ops              []trace.OperationRecord
		err              error
	}
	repo := f.Repository()
	read := func() snapshot {
		feature, err := repo.Workflow(stream, trace.FeatureSubject)
		if err != nil {
			return snapshot{err: err}
		}
		sealing, err := repo.Workflow(stream, intake.SealSubject)
		if err != nil {
			return snapshot{feature: feature, err: err}
		}
		ops, err := repo.Operations(stream)
		if err != nil {
			return snapshot{feature: feature, sealing: sealing, err: err}
		}
		ops = slices.DeleteFunc(ops, func(o trace.OperationRecord) bool { return o.Operation.Action != intake.SealAction })
		return snapshot{feature: feature, sealing: sealing, ops: ops}
	}
	deadline := time.NewTimer(fixturekit.DemoTimeout)
	defer deadline.Stop()
	last := "no trace read completed"
	for {
		result := make(chan snapshot, 1)
		go func() { result <- read() }()
		select {
		case <-f.ServiceDone:
			t.Fatalf("service stopped before sealing recovery completed: %v; %s", f.S.Wait(), last)
		case <-deadline.C:
			t.Fatalf("sealing recovery did not complete: %s", last)
		case got := <-result:
			var operations []string
			for _, op := range got.ops {
				result := "pending"
				if op.Result != nil {
					result = op.Result.Outcome + ": " + op.Result.Evidence
				}
				var history []string
				for _, action := range op.History {
					step := action.Kind
					if action.Observation != nil {
						step += fmt.Sprintf("(%s: %s)", action.Observation.State, action.Observation.Evidence)
					}
					if action.Failure != "" {
						step += "(" + action.Failure + ")"
					}
					history = append(history, step)
				}
				operations = append(operations, fmt.Sprintf("%s result=%q acknowledged=%t retry-at=%s history=[%s]", op.Operation.ID, result, op.Acknowledged, op.RetryAt, strings.Join(history, ", ")))
			}
			last = fmt.Sprintf("feature=%q seal-subject=%q operations=[%s] read-error=%v", got.feature.Value, got.sealing.Value, strings.Join(operations, "; "), got.err)
			if got.err != nil {
				t.Fatalf("reading sealing recovery: %s", last)
			}
			if len(got.ops) > 1 {
				t.Fatalf("sealing was requested more than once: %s", last)
			}
			if len(got.ops) == 1 && got.ops[0].Result != nil && got.ops[0].Result.Outcome != "succeeded" {
				t.Fatalf("sealing recovery failed: %s", last)
			}
			if got.feature.Value == intake.BuildingState && len(got.ops) == 1 && got.ops[0].Result != nil && got.ops[0].Result.Outcome == "succeeded" && got.ops[0].Acknowledged {
				return
			}
		}
		select {
		case <-deadline.C:
			t.Fatalf("sealing recovery did not complete: %s", last)
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// AwaitPacket waits until the packet the API serves recommends what the
// dissent record now calls for, and returns it. A workstream at its decision
// point has no packet until the pass that presents it records one.
func (f *ShedFixture) AwaitPacket(t *testing.T, stream config.WorkstreamID, recommendation string) shed.Packet {
	t.Helper()
	return f.Scenario().AwaitPacket(t, stream, recommendation)
}

// InboxEntries returns the inbox entries of one kind and workstream.
func (f *ShedFixture) InboxEntries(t *testing.T, stream config.WorkstreamID, kind string) []api.InboxEntry {
	t.Helper()
	return f.Scenario().InboxEntries(t, stream, kind)
}

func (f *ShedFixture) Ratification(t *testing.T, stream config.WorkstreamID, round int) shed.Ratification {
	t.Helper()
	return f.Scenario().Ratification(t, stream, round)
}

// AwaitTransition waits until the running service records the transition,
// and checks the states it moves between.
func (f *ShedFixture) AwaitTransition(t *testing.T, stream config.WorkstreamID, id, from, to string) {
	t.Helper()
	var found trace.Transition
	f.S.Until(t, fmt.Sprintf("transition %s to be recorded", id), func() bool {
		i := slices.IndexFunc(f.Transitions(t, stream), func(tr trace.Transition) bool { return tr.ID == id })
		if i >= 0 {
			found = f.Transitions(t, stream)[i]
		}
		return i >= 0
	})
	if found.From != from || found.To != to {
		t.Fatalf("transition %s %+v, want %s to %s", id, found, from, to)
	}
}

// LandingReason returns why the unit's latest approval cannot land, or "".
func LandingReason(t *testing.T, lands *Foreman, repository *trace.Repository, stream config.WorkstreamID, unit string) string {
	t.Helper()
	approval := &Foreman{Masons: &Masons{Repository: repository}}
	return intakefixture.LandingReason(t, stream, unit, approval.Approval,
		func(result service.UnitReviewResult) service.UnitReviewIdentity { return result.Identity },
		func(ctx context.Context, stream config.WorkstreamID, in intakefixture.LandingApproval, result service.UnitReviewResult) (string, error) {
			return lands.Current(ctx, stream, execution.LandInput{Unit: in.Unit, Review: in.Review, Candidate: in.Candidate, Base: in.Base}, result)
		})
}

// BuiltForAmendment returns a fixture with a building workstream on seal 1,
// a committee of one and the given shed.max_rounds.
func BuiltForAmendment(t *testing.T, rounds int) (*ShedFixture, config.WorkstreamID) {
	t.Helper()
	f := NewDebateFixture(t, 1, rounds)
	return f, f.BuildFixture().Amendment(t)
}

// PresentAmendment plants amendment 1 as the manager presents it: the
// requester of the given role asked for it from its turn filing on unit
// resume, which waits for it, the architect proposed spec and graph, the
// given committee records make its first round, and packet.json revision 1
// is presented. It returns the requester's agent.
func PresentAmendment(t *testing.T, f *ShedFixture, stream config.WorkstreamID, role, spec, graph string, records ...shed.Record) string {
	t.Helper()
	return intakefixture.PresentAmendment(t, f.Repository(), f.Project, f.Clock.Now(), stream, role, spec, graph, records...)
}

// AwaitAmendment waits until amendment 1 reaches the wanted state.
func (f *ShedFixture) AwaitAmendment(t *testing.T, stream config.WorkstreamID, want string) {
	t.Helper()
	intakefixture.AwaitAmendment(t, f.Repository(), stream, want)
}

// Revisions lists the recorded revisions of one document.
func (f *ShedFixture) Revisions(t *testing.T, stream config.WorkstreamID, id string) []int {
	t.Helper()
	return f.Scenario().DocumentRevisions(t, stream, id)
}

// Ruling returns the turns of the requester's thread that deliver the
// ruling on amendment 1.
func (f *ShedFixture) Ruling(t *testing.T, stream config.WorkstreamID, requester string) []trace.QueuedTurn {
	t.Helper()
	return intakefixture.AmendmentRuling(t, f.Repository(), stream, requester)
}

// Resumed checks that unit resume left its amendment wait for stage through
// the recorded transition, and is there now.
func (f *ShedFixture) Resumed(t *testing.T, stream config.WorkstreamID, stage string) {
	t.Helper()
	moved := f.Transition(t, stream, trace.UnitSubject("resume")+"_resumed_amendment_1")
	if moved.From != intake.UnitWaiting || moved.To != stage || moved.Actor != intake.AmendmentActor {
		t.Fatalf("resumption %+v", moved)
	}
	if state, err := f.UnitState(stream, "resume"); err != nil || state != stage {
		t.Fatalf("unit resume is %q: %v", state, err)
	}
}

// NewShedFixture starts a one-round debate fixture with the given members.
func NewShedFixture(t *testing.T, members int) *ShedFixture {
	t.Helper()
	return NewDebateFixture(t, members, 1)
}

// HostBranches hosts the fixture's upstream pull requests on a fork whose
// branch tips the pull requests read, and returns the fork and the host.
func (f *ShedFixture) HostBranches(t *testing.T) (string, *fixturekit.FakePulls) {
	t.Helper()
	fork, pulls := fixturekit.PublicationFork(t, f.Clone, func() string { return "" })
	pulls.Branches = func(branch string) string {
		tip, _ := fixturekit.PublicationForkBranch(t, fork, branch)
		return tip
	}
	pulls.SyncHead = true
	f.S.Options.PullRequests = pulls
	f.Opts.PullRequests = pulls
	return fork, pulls
}
