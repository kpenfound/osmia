package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kpenfound/osmia/internal/bundle"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/isolation"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
)

// RoundAction is the runner-boundary operation action that runs the
// committee's turns of one shed round.
const RoundAction = "shed-round"

const (
	committeeRole = "committee"
	// InShedState is the feature state of a workstream whose spec and plan the
	// committee debates.
	InShedState = "in-shed"
	// shedSubject is the workflow subject that tracks a workstream's debate:
	// round-<n> while the committee runs, heard-<n> once its contributions
	// are recorded, reply-<n> while the architect answers, replied-<n> once
	// its reply is recorded, concluded-<n> once debate ended after round n,
	// and failed-<n> when round n ended without a record or a reply.
	shedSubject = "shed"
	// maxRoundAttempts bounds the turns one member may start in one round
	// after service stops interrupt earlier ones: the librarian's bound.
	maxRoundAttempts = maxExtractionAttempts
)

var shedActor = trace.Actor{Kind: "service", ID: "shed"}

// Committee supplies the execution boundary of the committee's shed turns:
// the core execution engine and the role-scoped MCP host. The service owns
// each member's view, tools, prompt and record.
type Committee struct {
	Engine coreadapter.Engine
	Hosts  coreadapter.MCPHosts
}

// roundInput pins a round to the revisions of spec.md and plan.json every
// member reads.
type roundInput struct {
	Round int `json:"round"`
	Spec  int `json:"spec"`
	Plan  int `json:"plan"`
}

func (in roundInput) pin() shed.Pin { return shed.Pin{Spec: in.Spec, Plan: in.Plan} }

func committeeAgent(i int) string  { return "agent_committee_" + strconv.Itoa(i) }
func committeeThread(i int) string { return "thread_committee_" + strconv.Itoa(i) }

func roundIDs(n int) (transition, event string) {
	transition = fmt.Sprintf("shed-round-%d", n)
	return transition, trace.EventID(transition, "run")
}
func roundTurnPrefix(n int, agent string) string { return fmt.Sprintf("shed-%d-%s-", n, agent) }
func roundTurnID(n int, agent string, attempt int) string {
	return roundTurnPrefix(n, agent) + strconv.Itoa(attempt)
}

// shedState splits a shed-subject value into its kind (round, heard, reply,
// replied, concluded or failed) and round number.
func shedState(value string) (kind string, n int, ok bool) {
	kind, number, found := strings.Cut(value, "-")
	n, err := strconv.Atoi(number)
	return kind, n, found && err == nil && n > 0 && slices.Contains([]string{"round", "heard", "reply", "replied", "concluded", "failed"}, kind)
}

// debate is the shed controller. Its pass moves every sketched workstream
// into the shed with its committee and derives the debate's next step from
// the trace: a round, the architect's reply to it, or the conclusion. Its
// reconciler runs each round operation: one turn per member, in parallel
// against the pinned revision, and the record of what each contributed.
type debate struct {
	s          *Service
	repository *trace.Repository
}

var _ coreadapter.Reconciler = (*debate)(nil)

// Pass reconciles every workstream of the trace except the librarian's. A
// service without a committee runner moves nothing: sketched workstreams wait
// for a service that has one.
func (d *debate) Pass(ctx context.Context) error {
	if d.s.options.Committee == nil {
		return nil
	}
	streams, err := d.repository.Workstreams()
	if err != nil {
		return err
	}
	librarian := librarianWorkstream(d.repository.Project())
	for _, stream := range streams {
		if stream == librarian {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := d.reconcile(ctx, stream); err != nil {
			return fmt.Errorf("workstream %s shed: %w", stream, err)
		}
	}
	return nil
}

// reconcile gives a sketched workstream its committee and moves it to
// in-shed, then takes the next step of a workstream in the shed. Every other
// feature state needs nothing.
func (d *debate) reconcile(ctx context.Context, stream config.WorkstreamID) error {
	feature, err := d.repository.Workflow(stream, trace.FeatureSubject)
	if err != nil {
		return err
	}
	if feature.Value != SketchedState && feature.Value != InShedState {
		return nil
	}
	pin, err := d.latestPin(stream)
	if err != nil {
		return err
	}
	if feature.Value == SketchedState {
		if err := d.ensureCommittee(ctx, stream, d.s.current().Capacity.Committee); err != nil {
			return err
		}
		// The committee is the threads that exist, which is what every round
		// runs.
		members, err := d.committee(stream)
		if err != nil {
			return err
		}
		reason := fmt.Sprintf("%s enter the shed with a committee of %d", pin, len(members))
		_, err = d.repository.MoveFeatureState(ctx, d.header(InShedState, stream, SketchedState, d.s.now()), SketchedState, InShedState, reason)
		if errors.Is(err, trace.ErrConflict) {
			return nil
		}
		if err != nil {
			return err
		}
	}
	return d.step(ctx, stream, pin)
}

// step derives what the debate needs next from the shed state and the
// recorded rounds. No round yet: round 1. A heard round with no open dissent
// concludes the debate by consensus; one with open dissent gets the
// architect's reply. After the reply, the next round runs against the latest
// revision unless shed.max_rounds rounds have run, which concludes the debate
// with its dissent open. A round or a reply in progress, a concluded debate
// and a failed round need nothing.
func (d *debate) step(ctx context.Context, stream config.WorkstreamID, latest shed.Pin) error {
	state, err := d.repository.Workflow(stream, shedSubject)
	if err != nil {
		return err
	}
	if state.Value == "" {
		return d.request(ctx, stream, state, roundInput{Round: 1, Spec: latest.Spec, Plan: latest.Plan}, InShedState)
	}
	kind, n, ok := shedState(state.Value)
	if !ok || kind != "heard" && kind != "replied" {
		return nil
	}
	records, err := shed.Records(d.repository, stream)
	if err != nil {
		return err
	}
	open := shed.DissentRecord(records)
	round, _ := roundIDs(n)
	limit := d.s.current().Shed.MaxRounds
	switch {
	case len(open) == 0:
		return d.conclude(ctx, stream, state, n, round+"-heard", fmt.Sprintf("debate concluded by consensus after round %d: no objection stands", n), open)
	case kind == "heard":
		// The reply waits for a service that can run the architect.
		if d.s.options.Architect == nil {
			return nil
		}
		// Every record of a round carries the revision the round was pinned to.
		i := slices.IndexFunc(records, func(r shed.Record) bool { return r.Round == n })
		if i < 0 {
			return fmt.Errorf("round %d was heard and has no record", n)
		}
		return d.requestReply(ctx, stream, state, roundInput{Round: n, Spec: records[i].Revision.Spec, Plan: records[i].Revision.Plan}, len(open))
	case n >= limit:
		reply, _ := replyIDs(n)
		return d.conclude(ctx, stream, state, n, reply+"-replied", fmt.Sprintf("debate stopped after round %d, at the shed.max_rounds cap of %d, with %s; the cap approves nothing", n, limit, standing(open)), open)
	}
	reply, _ := replyIDs(n)
	return d.request(ctx, stream, state, roundInput{Round: n + 1, Spec: latest.Spec, Plan: latest.Plan}, reply+"-replied")
}

// Dissent returns the workstream's dissent record, computed from the recorded
// rounds alone: every objection that stands, with its kind, its member, the
// part it names and whether it blocks.
func Dissent(repository *trace.Repository, stream config.WorkstreamID) ([]shed.Entry, error) {
	records, err := shed.Records(repository, stream)
	if err != nil {
		return nil, err
	}
	return shed.DissentRecord(records), nil
}

// standing counts the open dissent and how much of it blocks.
func standing(open []shed.Entry) string {
	blocking := 0
	for _, e := range open {
		if e.Blocking {
			blocking++
		}
	}
	return fmt.Sprintf("%d objections standing, %d of them blocking", len(open), blocking)
}

// conclude ends the debate after round n. The workstream stays in the shed:
// the conclusion is a shed transition, and its notice gives the chief of
// staff the reason and the dissent that stands.
func (d *debate) conclude(ctx context.Context, stream config.WorkstreamID, state trace.WorkflowState, n int, cause, reason string, open []shed.Entry) error {
	id := fmt.Sprintf("shed-concluded-%d", n)
	var body strings.Builder
	fmt.Fprintf(&body, "Debate concluded: %s. The workstream stays %s until the owner rules.", reason, InShedState)
	if len(open) > 0 {
		body.WriteString("\nOpen dissent:")
	}
	for _, e := range open {
		weight := "advisory"
		if e.Blocking {
			weight = "blocking"
		}
		fmt.Fprintf(&body, "\n- %s (%s, %s, by %s in round %d on %s, against %s): %s", e.ID, e.Kind, weight, e.Member, e.Round, e.Part, e.Revision, e.Argument)
	}
	tx := trace.Transaction{ExpectedVersion: state.Version,
		Transition: trace.Transition{Header: d.header(id, stream, cause, d.s.now()), Subject: shedSubject, From: state.Value, To: fmt.Sprintf("concluded-%d", n), Reason: reason},
		Events:     []trace.Event{trace.Notice(id, "concluded", body.String())}}
	_, err := d.repository.Transact(ctx, tx)
	return err
}

func (d *debate) header(id string, stream config.WorkstreamID, cause string, at time.Time) trace.Header {
	return trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: d.repository.Project(), Workstream: stream, At: at, Actor: shedActor, Cause: cause}
}

// latestPin returns the latest recorded revisions of the spec and the plan.
func (d *debate) latestPin(stream config.WorkstreamID) (shed.Pin, error) {
	docs, err := trace.Read[trace.Document](d.repository, stream)
	if err != nil {
		return shed.Pin{}, err
	}
	var pin shed.Pin
	for _, doc := range docs {
		switch doc.ID {
		case plan.SpecDocument:
			pin.Spec = doc.Revision
		case plan.PlanDocument:
			pin.Plan = doc.Revision
		}
	}
	if pin.Spec == 0 || pin.Plan == 0 {
		return pin, fmt.Errorf("workstream %s has no recorded spec and plan", stream)
	}
	return pin, nil
}

// ensureCommittee creates the committee threads the workstream is missing, up
// to the given number of members.
func (d *debate) ensureCommittee(ctx context.Context, stream config.WorkstreamID, members int) error {
	for i := 1; i <= members; i++ {
		_, err := d.repository.Thread(stream, committeeAgent(i))
		if err == nil {
			continue
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		identity := trace.Agent{Header: trace.Header{Schema: "osmia.trace.agent", Version: trace.Version, ID: committeeAgent(i), Revision: 1, Project: d.repository.Project(), Workstream: stream, At: d.s.now(), Actor: shedActor, Cause: "committee-thread"}, Role: committeeRole, ThreadID: committeeThread(i)}
		if err := d.repository.CreateThread(ctx, identity); err != nil {
			return err
		}
	}
	return nil
}

// committee returns the workstream's committee members, the agents of its
// committee threads, in order.
func (d *debate) committee(stream config.WorkstreamID) ([]string, error) {
	threads, err := d.repository.Threads(stream)
	if err != nil {
		return nil, err
	}
	var members []string
	for _, t := range threads {
		if t.Identity.Role == committeeRole {
			members = append(members, t.Identity.ID)
		}
	}
	slices.SortFunc(members, func(a, b string) int {
		if len(a) != len(b) {
			return len(a) - len(b)
		}
		return strings.Compare(a, b)
	})
	if len(members) == 0 {
		return nil, fmt.Errorf("workstream %s has no committee", stream)
	}
	return members, nil
}

// request publishes a round as a durable operation.
func (d *debate) request(ctx context.Context, stream config.WorkstreamID, state trace.WorkflowState, in roundInput, cause string) error {
	transition, event := roundIDs(in.Round)
	input, err := encodeRound(in)
	if err != nil {
		return err
	}
	op := coreadapter.Operation{ID: trace.OperationID(d.repository.Project(), stream, event), Boundary: coreadapter.RunnerBoundary, Action: RoundAction, Input: input}
	reason := fmt.Sprintf("the committee is asked for round %d against %s", in.Round, in.pin())
	tx := trace.Transaction{ExpectedVersion: state.Version,
		Transition: trace.Transition{Header: d.header(transition, stream, cause, d.s.now()), Subject: shedSubject, From: state.Value, To: fmt.Sprintf("round-%d", in.Round), Reason: reason},
		Events:     []trace.Event{{ID: event, Kind: "shed-round", Body: fmt.Sprintf("Committee round %d against %s", in.Round, in.pin()), Operation: &op}}}
	_, err = d.repository.Transact(ctx, tx)
	return err
}

func encodeRound(in roundInput) (json.RawMessage, error) { return json.Marshal(in) }

func decodeRound(op coreadapter.Operation) (roundInput, error) { return decodeShed(op, RoundAction) }

// decodeShed reads the input of a shed operation of the given action: a round
// and the revision it is pinned to.
func decodeShed(op coreadapter.Operation, action string) (roundInput, error) {
	var in roundInput
	if op.Boundary != coreadapter.RunnerBoundary || op.Action != action {
		return in, fmt.Errorf("unsupported runner operation %q", op.Action)
	}
	dec := json.NewDecoder(bytes.NewReader(op.Input))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return in, fmt.Errorf("invalid %s operation input: %w", action, err)
	}
	if in.Round < 1 || in.Spec < 1 || in.Plan < 1 {
		return in, fmt.Errorf("%s operation requires a positive round and pinned revisions", action)
	}
	return in, nil
}

// stream returns the workstream a round operation belongs to: the one whose
// run event derives the operation ID.
func (d *debate) stream(op coreadapter.Operation, n int) (config.WorkstreamID, error) {
	_, event := roundIDs(n)
	return d.owner(op, event)
}

// owner returns the workstream whose event derives the operation ID.
func (d *debate) owner(op coreadapter.Operation, event string) (config.WorkstreamID, error) {
	streams, err := d.repository.Workstreams()
	if err != nil {
		return "", err
	}
	for _, stream := range streams {
		if trace.OperationID(d.repository.Project(), stream, event) == op.ID {
			return stream, nil
		}
	}
	return "", fmt.Errorf("%s operation %s belongs to no workstream", op.Action, op.ID)
}

// outcome returns the recorded terminal result of round n: succeeded once
// the committee was heard, failed when the round's failed transition is
// recorded, nil before either.
func (d *debate) outcome(stream config.WorkstreamID, n int) (*coreadapter.OperationResult, error) {
	transitions, err := trace.Read[trace.Transition](d.repository, stream)
	if err != nil {
		return nil, err
	}
	round, _ := roundIDs(n)
	for _, t := range transitions {
		switch {
		case t.Subject != shedSubject:
		case t.ID == round+"-heard":
			return &coreadapter.OperationResult{Outcome: "succeeded", Evidence: t.Reason}, nil
		case t.ID == round+"-failed":
			return &coreadapter.OperationResult{Outcome: "failed", Evidence: t.Reason}, nil
		}
	}
	return nil, nil
}

// roundTurns returns the member's turns of round n in attempt order.
func roundTurns(t trace.Thread, n int) []trace.QueuedTurn {
	prefix := roundTurnPrefix(n, t.Identity.ID)
	var out []trace.QueuedTurn
	for _, q := range t.Turns {
		if strings.HasPrefix(q.Request.TurnID, prefix) {
			out = append(out, q)
		}
	}
	return out
}

// Inspect reads the recorded transitions and the committee threads. A
// recorded outcome completes the operation; a member turn this session is
// running is unknown; everything else is absent, and Apply decides between
// running, retrying and recording.
func (d *debate) Inspect(_ context.Context, op coreadapter.Operation) (coreadapter.Observation, error) {
	in, err := decodeRound(op)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	stream, err := d.stream(op, in.Round)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	result, err := d.outcome(stream, in.Round)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	if result != nil {
		return coreadapter.Observation{State: coreadapter.EffectCompleted, Evidence: "round " + result.Outcome, Result: result}, nil
	}
	members, err := d.committee(stream)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	for _, member := range members {
		t, err := d.repository.Thread(stream, member)
		if err != nil {
			return coreadapter.Observation{}, err
		}
		turns := roundTurns(t, in.Round)
		if len(turns) == 0 {
			continue
		}
		if last := turns[len(turns)-1]; last.Claim != nil && last.Response == nil && t.Status != "interrupted" {
			return coreadapter.Observation{State: coreadapter.EffectUnknown, Evidence: "committee turn " + last.Request.TurnID + " is running"}, nil
		}
	}
	return coreadapter.Observation{State: coreadapter.EffectAbsent, Evidence: fmt.Sprintf("round %d is not recorded", in.Round)}, nil
}

// errNoCommittee leaves a round pending in a service that cannot run the
// committee's turns, so no member's attempts are spent on the missing runner.
var errNoCommittee = errors.New("this service has no agent runner for the committee")

// Apply drives the round to a terminal result. Every member's turn runs at
// the same time against the pinned revision; once all have ended, each
// member's contributions are recorded as one file of the round, in one
// commit, and the shed moves to heard-<n>. A member whose turn failed is
// recorded with the failure and what it contributed before it. Abandoning
// the workstream cancels the running turns and fails a round that has no
// record; a round whose files are committed is heard all the same. Storage
// errors and a missing committee runner leave the operation pending for
// another attempt, which finds the turns that already ended in the threads.
func (d *debate) Apply(ctx context.Context, op coreadapter.Operation) (coreadapter.OperationResult, error) {
	in, err := decodeRound(op)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	stream, err := d.stream(op, in.Round)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	if result, err := d.outcome(stream, in.Round); err != nil || result != nil {
		if err != nil {
			return coreadapter.OperationResult{}, err
		}
		return *result, nil
	}
	cfg := d.s.current()
	if !cfg.HasProject() || cfg.Project.ID != d.repository.Project() {
		return coreadapter.OperationResult{}, errors.New("the project is not active")
	}
	members, err := d.committee(stream)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	// Abandoning the workstream cancels the running turns, not the recording
	// of what they left.
	running, cancel := context.WithCancel(ctx)
	defer cancel()
	defer d.s.turns.add(stream, cancel)()
	records := make([]shed.Record, len(members))
	failures := make([]error, len(members))
	var wg sync.WaitGroup
	for i, member := range members {
		wg.Add(1)
		go func() {
			defer wg.Done()
			records[i], failures[i] = d.member(ctx, running, cfg, stream, op.ID, in, member)
		}()
	}
	wg.Wait()
	if err := errors.Join(failures...); err != nil {
		return coreadapter.OperationResult{}, err
	}
	recorded, err := shed.Records(d.repository, stream)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	// A round whose files are committed was heard, whatever happened to the
	// workstream since: only a round without a record fails on abandonment.
	heard := slices.ContainsFunc(recorded, func(r shed.Record) bool { return r.Round == in.Round })
	if !heard {
		gone, err := abandoned(d.repository, stream)
		if err != nil {
			return coreadapter.OperationResult{}, err
		}
		if gone {
			return d.terminal(ctx, op.ID, stream, in.Round, "failed", fmt.Sprintf("round %d failed: the workstream was abandoned, so the committee is not heard", in.Round))
		}
	}
	return d.record(ctx, op.ID, stream, in, records, recorded)
}

// member drives one member's turn of the round to its end and returns what
// the member contributed. It abandons a turn a previous service stop
// interrupted, starts a new turn while attempts remain and runs the pending
// turn through the dispatcher.
func (d *debate) member(ctx, running context.Context, cfg *config.Config, stream config.WorkstreamID, operation string, in roundInput, member string) (shed.Record, error) {
	empty := shed.Record{Version: shed.Version, Round: in.Round, Member: member, Revision: in.pin()}
	for {
		if err := ctx.Err(); err != nil {
			return empty, err
		}
		t, err := d.repository.Thread(stream, member)
		if err != nil {
			return empty, err
		}
		turns := roundTurns(t, in.Round)
		var last *trace.QueuedTurn
		if len(turns) > 0 {
			last = &turns[len(turns)-1]
		}
		switch {
		case last != nil && last.Claim != nil && last.Response == nil:
			if t.Status != "interrupted" {
				return empty, errors.New("committee turn " + last.Request.TurnID + " is still running")
			}
			if err := d.repository.AbandonTurn(ctx, stream, member, last.Request.TurnID, d.s.now()); err != nil {
				return empty, err
			}
		case last == nil || last.Status() == "interrupted":
			if gone, err := abandoned(d.repository, stream); err != nil || gone {
				empty.Failure = "the workstream was abandoned, so the member ran no turn"
				return empty, err
			}
			if len(turns) >= maxRoundAttempts {
				empty.Failure = fmt.Sprintf("the member's turn was interrupted %d times by service stops", len(turns))
				return empty, nil
			}
			if d.s.options.Committee == nil {
				return empty, errNoCommittee
			}
			if err := d.enqueue(ctx, cfg, stream, in, member, len(turns)+1, operation); err != nil {
				return empty, err
			}
		case last.CompletedAt.IsZero():
			gone, err := abandoned(d.repository, stream)
			if err != nil {
				return empty, err
			}
			if gone && last.Response == nil {
				if _, err := d.repository.CancelTurns(ctx, stream, d.s.now(), abandonActor, cancelReason); err != nil {
					return empty, err
				}
				continue
			}
			// Completing a captured turn runs no member.
			turnCtx := running
			if last.Response != nil {
				turnCtx = ctx
			} else if d.s.options.Committee == nil {
				return empty, errNoCommittee
			}
			if _, err := d.dispatch(turnCtx, stream, in, member, last.Request.TurnID); err != nil {
				return empty, err
			}
			if err := os.RemoveAll(filepath.Join(d.turnDirectory(stream, last.Request.TurnID), "workspace")); err != nil {
				return empty, err
			}
		default:
			record, err := d.contributed(stream, last.Request.TurnID, empty)
			if err != nil {
				return empty, err
			}
			if last.Status() != "idle" {
				record.Failure = "the turn ended with status " + last.Status()
				if last.Response != nil && last.Response.Failure != "" {
					record.Failure = last.Response.Failure
				}
			}
			return record, nil
		}
	}
}

// terminal ends round n with a shed transition, heard-<n> once the committee
// is recorded or failed-<n> with why it was not, and returns the matching
// result. The transition already recorded is returned as it is.
func (d *debate) terminal(ctx context.Context, operation string, stream config.WorkstreamID, n int, kind, reason string) (coreadapter.OperationResult, error) {
	outcome := coreadapter.OperationResult{Outcome: "failed", Evidence: reason}
	if kind == "heard" {
		outcome.Outcome = "succeeded"
	}
	state, err := d.repository.Workflow(stream, shedSubject)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	if state.Value != fmt.Sprintf("round-%d", n) {
		result, err := d.outcome(stream, n)
		if err != nil || result == nil {
			return coreadapter.OperationResult{}, errors.Join(err, fmt.Errorf("round %d is %q, not in progress", n, state.Value))
		}
		return *result, nil
	}
	round, _ := roundIDs(n)
	tx := trace.Transaction{ExpectedVersion: state.Version,
		Transition: trace.Transition{Header: d.header(round+"-"+kind, stream, operation, d.s.now()), Subject: shedSubject, From: state.Value, To: fmt.Sprintf("%s-%d", kind, n), Reason: reason}}
	if _, err := d.repository.Transact(ctx, tx); err != nil {
		return coreadapter.OperationResult{}, err
	}
	return outcome, nil
}

// enqueue accepts one member's turn of one round attempt, fixing its profile
// and prompts.
func (d *debate) enqueue(ctx context.Context, cfg *config.Config, stream config.WorkstreamID, in roundInput, member string, attempt int, operation string) error {
	profile, _, err := d.s.roleExecution(cfg, committeeRole)
	if err != nil {
		return err
	}
	t, err := d.repository.Thread(stream, member)
	if err != nil {
		return err
	}
	earlier, err := d.earlier(stream, in.Round)
	if err != nil {
		return err
	}
	var standing []shed.Dissent
	for _, dissent := range shed.OpenDissent(earlier) {
		if dissent.Member == member {
			standing = append(standing, dissent)
		}
	}
	turn := roundTurnID(in.Round, member, attempt)
	req := trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_" + turn, Revision: 1, Project: d.repository.Project(), Workstream: stream, At: d.s.now(), Actor: shedActor, Cause: operation, Depth: 1},
		AgentID: member, ThreadID: t.Identity.ThreadID, TurnID: turn, Profile: profile, SystemPrompt: committeeSystemPrompt(cfg.Project), Prompt: committeePrompt(in, standing)}
	_, err = d.repository.EnqueueTurn(ctx, req)
	return err
}

// earlier returns the workstream's records of the rounds before round n.
func (d *debate) earlier(stream config.WorkstreamID, n int) ([]shed.Record, error) {
	records, err := shed.Records(d.repository, stream)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(records, func(r shed.Record) bool { return r.Round >= n }), nil
}

// turnDirectory is the service-owned directory of one committee turn: its
// staged view, its backend session and what the member contributed.
func (d *debate) turnDirectory(stream config.WorkstreamID, turn string) string {
	return filepath.Join(d.s.current().Root.String(), "shed", string(d.repository.Project()), string(stream), turn)
}

func (d *debate) contributions(stream config.WorkstreamID, turn string) string {
	return filepath.Join(d.turnDirectory(stream, turn), "output", "contributions.json")
}

// contributed reads what the turn's tools kept. A turn that kept nothing was
// silent.
func (d *debate) contributed(stream config.WorkstreamID, turn string, empty shed.Record) (shed.Record, error) {
	empty.Turn = turn
	data, err := os.ReadFile(d.contributions(stream, turn))
	if errors.Is(err, fs.ErrNotExist) {
		return empty, nil
	}
	if err != nil {
		return empty, err
	}
	record, err := shed.Parse(data)
	if err != nil {
		return empty, err
	}
	if record.Round != empty.Round || record.Member != empty.Member || record.Revision != empty.Revision || record.Turn != turn {
		return empty, fmt.Errorf("contributions of turn %s belong to another turn", turn)
	}
	return record, nil
}

// dispatch runs the turn through the thread dispatcher and runner.
func (d *debate) dispatch(ctx context.Context, stream config.WorkstreamID, in roundInput, member, turn string) (coreadapter.OperationResult, error) {
	dispatcher := thread.Dispatcher{Runner: thread.Runner{Store: d.repository, Turns: d.turns(stream, in), Now: d.s.now}, Prepare: func(_ context.Context, input thread.TurnInput) (coreadapter.PreparedTurn, error) {
		directory := filepath.Join(d.turnDirectory(input.Workstream, input.Turn), "session")
		return coreadapter.PreparedTurn{SessionDirectory: directory}, os.MkdirAll(directory, 0700)
	}}
	op, err := thread.TurnOperation(d.repository.Project(), "shed-turn", thread.TurnInput{Workstream: stream, Agent: member, Turn: turn})
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	return dispatcher.Apply(ctx, op)
}

// turns is a committee member's isolated turn path: a read-only private view
// of the owner's clone, the pinned spec and plan, the handed input, the
// charter, the context bundle and the earlier rounds, the file reading tool
// and the two contribution tools, and no notes, write, execute, network or
// VCS capability.
func (d *debate) turns(stream config.WorkstreamID, in roundInput) *isolation.Turns {
	var engine coreadapter.Engine
	var hosts coreadapter.MCPHosts
	if c := d.s.options.Committee; c != nil {
		engine, hosts = c.Engine, c.Hosts
	}
	return &isolation.Turns{
		Workspaces: stagedWorkspaces{},
		Views:      isolation.Views{Directory: filepath.Join(d.s.current().Root.String(), "views")},
		Select: func(ctx context.Context, scope coreadapter.Scope) (isolation.Selection, error) {
			return d.selectView(ctx, scope, in)
		},
		Grants: map[string]coreadapter.Capabilities{committeeRole: {Tools: []string{"file_read", shed.ObjectTool, shed.ConcedeTool}}},
		Scoped: func(_ context.Context, scope coreadapter.Scope) ([]coreadapter.Tool, error) {
			if scope.Workstream != string(stream) {
				return nil, errors.New("turn scope denied")
			}
			return d.tools(scope, in)
		},
		Hosts:  hosts,
		Engine: engine,
	}
}

// tools binds the object and concede tools to the claimed turn: they validate
// against the pinned revision and keep what the member contributed in the
// turn's output directory, which the service records once the round ends.
func (d *debate) tools(scope coreadapter.Scope, in roundInput) ([]coreadapter.Tool, error) {
	if scope.Role != committeeRole || scope.Project != string(d.repository.Project()) {
		return nil, errors.New("turn scope denied")
	}
	stream := config.WorkstreamID(scope.Workstream)
	threads, err := d.repository.Threads(stream)
	if err != nil {
		return nil, err
	}
	i := slices.IndexFunc(threads, func(t trace.Thread) bool {
		return t.Identity.Role == committeeRole && t.Identity.ThreadID == scope.Thread
	})
	if i < 0 || !strings.HasPrefix(scope.Turn, roundTurnPrefix(in.Round, threads[i].Identity.ID)) {
		return nil, errors.New("turn scope denied")
	}
	member := threads[i].Identity.ID
	specDoc, planDoc, err := d.pinned(stream, in)
	if err != nil {
		return nil, err
	}
	graph, err := plan.Parse([]byte(planDoc.Content))
	if err != nil {
		return nil, err
	}
	earlier, err := d.earlier(stream, in.Round)
	if err != nil {
		return nil, err
	}
	file := d.contributions(stream, scope.Turn)
	return shed.Tools(shed.Turn{Repository: d.repository, Stream: stream, Spec: plan.ParseSpec(specDoc.Content), Plan: graph, Earlier: earlier, Now: d.s.now,
		Record: shed.Record{Round: in.Round, Member: member, Revision: in.pin(), Turn: scope.Turn},
		Save: func(r shed.Record) error {
			data, err := shed.Encode(r)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
				return err
			}
			// A rename keeps the previous contributions whole if the write stops.
			if err := os.WriteFile(file+".tmp", data, 0600); err != nil {
				return err
			}
			return os.Rename(file+".tmp", file)
		}})
}

// pinned returns the revisions of spec.md and plan.json the round is pinned to.
func (d *debate) pinned(stream config.WorkstreamID, in roundInput) (spec, graph trace.Document, err error) {
	docs, err := trace.Read[trace.Document](d.repository, stream)
	if err != nil {
		return spec, graph, err
	}
	for _, doc := range docs {
		switch {
		case doc.ID == plan.SpecDocument && doc.Revision == in.Spec:
			spec = doc
		case doc.ID == plan.PlanDocument && doc.Revision == in.Plan:
			graph = doc
		}
	}
	if spec.Revision == 0 || graph.Revision == 0 {
		return spec, graph, fmt.Errorf("workstream %s does not record %s", stream, in.pin())
	}
	return spec, graph, nil
}

// selectView stages the member's view for the claimed turn and selects all of
// it, read-only.
func (d *debate) selectView(ctx context.Context, scope coreadapter.Scope, in roundInput) (isolation.Selection, error) {
	cfg := d.s.current()
	if scope.Role != committeeRole || scope.Project != string(d.repository.Project()) || !cfg.HasProject() || cfg.Project.ID != d.repository.Project() {
		return isolation.Selection{}, errors.New("view selection denied")
	}
	stream, err := config.ParseWorkstreamID(scope.Workstream)
	if err != nil {
		return isolation.Selection{}, err
	}
	_, settings, err := d.s.roleExecution(cfg, committeeRole)
	if err != nil {
		return isolation.Selection{}, err
	}
	if err := os.MkdirAll(filepath.Join(cfg.Root.String(), "views"), 0700); err != nil {
		return isolation.Selection{}, err
	}
	workspace := filepath.Join(d.turnDirectory(stream, scope.Turn), "workspace")
	paths, err := d.stage(ctx, cfg.Project.Clone, stream, in, workspace)
	if err != nil {
		return isolation.Selection{}, err
	}
	return isolation.Selection{Workspace: coreadapter.WorkspaceRequest{SourceDirectory: workspace, Directory: workspace}, Paths: paths, Execution: settings}, nil
}

// stage builds the view: the clone's tracked files under repo/, the pinned
// spec.md and plan.json, the handed input under handed/, charter.md, the
// rendered context bundle as context.md and the records of the earlier rounds
// under shed/. It returns the paths to select.
func (d *debate) stage(ctx context.Context, clone string, stream config.WorkstreamID, in roundInput, workspace string) ([]string, error) {
	if err := os.RemoveAll(workspace); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(workspace, "repo"), 0700); err != nil {
		return nil, err
	}
	if err := copyTracked(ctx, clone, filepath.Join(workspace, "repo")); err != nil {
		return nil, err
	}
	specDoc, planDoc, err := d.pinned(stream, in)
	if err != nil {
		return nil, err
	}
	charter, err := d.repository.Charter(ctx, d.s.now())
	if err != nil {
		return nil, err
	}
	b, err := d.s.Context().Assemble(ctx, d.repository.Project(), bundle.Scope{Workstream: stream})
	if err != nil {
		return nil, err
	}
	files := map[string]string{plan.SpecPath: specDoc.Content, plan.PlanPath: planDoc.Content, "charter.md": charter.Content, "context.md": b.Render()}
	paths := []string{"repo", plan.SpecPath, plan.PlanPath, "charter.md", "context.md"}
	docs, err := trace.Read[trace.Document](d.repository, stream)
	if err != nil {
		return nil, err
	}
	for _, doc := range docs {
		if directory, _, _ := strings.Cut(doc.Path, "/"); directory == "handed" || directory == "shed" {
			files[doc.Path] = doc.Content
			if !slices.Contains(paths, directory) {
				paths = append(paths, directory)
			}
		}
	}
	for name, content := range files {
		path := filepath.Join(workspace, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			return nil, err
		}
	}
	return paths, nil
}

// record commits one file per member under shed/round-<n>/, each authored by
// its member and caused by the operation, then moves the shed to heard-<n>.
// recorded is what the workstream's shed already holds: a round with files in
// it only moves.
func (d *debate) record(ctx context.Context, operation string, stream config.WorkstreamID, in roundInput, records, recorded []shed.Record) (coreadapter.OperationResult, error) {
	if !slices.ContainsFunc(recorded, func(r shed.Record) bool { return r.Round == in.Round }) {
		at := d.s.now()
		var docs []trace.Document
		for _, r := range records {
			data, err := shed.Encode(r)
			if err != nil {
				return coreadapter.OperationResult{}, err
			}
			docs = append(docs, trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: shed.DocumentID(r.Round, r.Member), Revision: 1, Project: d.repository.Project(), Workstream: stream, At: at, Actor: trace.Actor{Kind: "agent", ID: r.Member}, Cause: operation, Depth: 1},
				Path: shed.Path(r.Round, r.Member), Content: string(data)})
		}
		if err := d.repository.RecordDocuments(ctx, docs); err != nil {
			return coreadapter.OperationResult{}, err
		}
		var err error
		if recorded, err = shed.Records(d.repository, stream); err != nil {
			return coreadapter.OperationResult{}, err
		}
	}
	var objections, concessions, failed int
	for _, r := range recorded {
		if r.Round == in.Round {
			objections, concessions = objections+len(r.Objections), concessions+len(r.Concessions)
			if r.Failure != "" {
				failed++
			}
		}
	}
	reason := fmt.Sprintf("round %d against %s: %d members heard, %d objections, %d concessions, %d failed turns; %d objections stand", in.Round, in.pin(), len(records), objections, concessions, failed, len(shed.OpenDissent(recorded)))
	return d.terminal(ctx, operation, stream, in.Round, "heard", reason)
}

func committeeSystemPrompt(p config.Project) string {
	return fmt.Sprintf("You are a member of the committee that debates one workstream's feature spec and plan for the %s project (%s) before anything is built. You test the draft against the project's charter, the owner's handed design and the decisions the knowledge base holds. You read; you hold no tool that writes, runs or fetches. You contribute only through %s and %s, and every objection cites what it rests on.", p.Name, p.Upstream, shed.ObjectTool, shed.ConcedeTool)
}

func committeePrompt(in roundInput, standing []shed.Dissent) string {
	var b strings.Builder
	fmt.Fprintf(&b, `Round %d of the shed. Every member reads the same revision: %s.

Your view holds:
- spec.md and plan.json: the revision under debate. Cite a criterion as spec#<n> and a unit as plan#<unit>.
- handed/: what the owner handed in, unchanged. It says what to build.
- charter.md: the owner's rules for contributing to this project. Cite them as charter#<n>.
- context.md: the project context bundle: the charter's rules, the knowledge-base prose per subsystem, the entity map and the recorded decisions. Cite a knowledge-base file as kb/<subsystem>.md and an entity as %s<entity>.
- repo/: the tracked files of the owner's clone, read-only.
- shed/round-<n>/: what every member contributed in earlier rounds, and the architect's reply to each as reply.json, when there are any.

Apply two tests and one judgement, and call %s once for each thing you find:
- charter: a part that violates a charter rule. This is a veto on that part; cite the rule.
- fit: the plan does not realise the handed design, or works against a decision the knowledge base holds. This is advice to the owner.
- size: a unit that addresses too many criteria or touches too much of the code and must be split by what it addresses. Name the unit as the part.
- proof: a criterion whose proof the plan cannot name, or names a proof that cannot show it. Name the criterion as the part.

An objection that is refused comes back with the reason; correct it and call again. Ending your turn without objecting or conceding says you have no new dissent on this revision, and that you accept it in place of any earlier revision you objected to.
`, in.Round, in.pin(), shed.EntityCitation, shed.ObjectTool)
	if len(standing) > 0 {
		fmt.Fprintf(&b, "\nYour objections that still stand. Call %s for each one that the revision or the debate has settled:\n", shed.ConcedeTool)
		for _, s := range standing {
			fmt.Fprintf(&b, "- %s (%s, %s, made against %s): %s\n", s.ID, s.Kind, s.Part, s.Revision, s.Argument)
		}
	}
	return b.String()
}
