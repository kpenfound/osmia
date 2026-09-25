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
	"time"
	"unicode/utf8"

	"github.com/kpenfound/osmia/internal/bundle"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/isolation"
	"github.com/kpenfound/osmia/internal/kb"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
)

// DraftAction is the runner-boundary operation action that runs one draft of
// a handed workstream's spec and plan by the architect.
const DraftAction = "architect-draft"

const (
	architectRole   = "architect"
	architectAgent  = "agent_architect"
	architectThread = "thread_architect"
	// SketchedState is the feature state of a workstream whose spec and plan
	// passed validation.
	SketchedState = "sketched"
	// DraftTool is the tool the architect delivers spec.md and plan.json with.
	DraftTool = "draft_write"
	// draftSubject is the workflow subject that tracks the architect's drafts
	// of one workstream: drafting-<n>, waiting-<n> while draft n waits for the
	// answer to the architect's question, invalid-<n>, failed-<n> or
	// exhausted.
	draftSubject = "draft"
	// maxDrafts bounds the drafts the service asks the architect for on one
	// workstream, and maxDraftAttempts the turns one draft may start after
	// service stops interrupt earlier ones: the librarian's bounds.
	maxDrafts        = maxExtractionAttempts
	maxDraftAttempts = maxExtractionAttempts
)

var (
	architectActor = trace.Actor{Kind: "agent", ID: architectAgent}
	draftingActor  = trace.Actor{Kind: "service", ID: "architect-drafting"}
)

// Architect supplies the execution boundary of the architect's drafting
// turns and shed replies: the core execution engine and the role-scoped MCP host. The service
// owns the architect's view, tools, prompt and output.
type Architect struct {
	Engine coreadapter.Engine
	Hosts  coreadapter.MCPHosts
}

// draftInput numbers the draft an operation runs. Resume numbers the
// operation that runs the draft again after it parked on the architect's
// question, from 1; the draft's own operation has none.
type draftInput struct {
	Draft  int `json:"draft"`
	Resume int `json:"resume,omitempty"`
}

// runIDs returns the transition and event of the operation that runs the
// draft: the draft's own, or the resumption the input numbers.
func (in draftInput) runIDs() (transition, event string) {
	if in.Resume == 0 {
		return draftIDs(in.Draft)
	}
	transition = fmt.Sprintf("draft-%d-resume-%d", in.Draft, in.Resume)
	return transition, trace.EventID(transition, "run")
}

// parkedID is the transition that parks the draft the operation runs: the
// k-th park of draft n follows its k-1-th resumption.
func (in draftInput) parkedID() string {
	return fmt.Sprintf("draft-%d-waiting-%d", in.Draft, in.Resume+1)
}

func draftIDs(n int) (transition, event string) {
	transition = fmt.Sprintf("draft-%d", n)
	return transition, trace.EventID(transition, "run")
}
func draftTurnPrefix(n int) string      { return fmt.Sprintf("draft-%d-", n) }
func draftTurnID(n, attempt int) string { return draftTurnPrefix(n) + strconv.Itoa(attempt) }

// draftState splits a draft-subject value into its kind (drafting, waiting,
// invalid or failed) and draft number.
func draftState(value string) (kind string, n int, ok bool) {
	i := strings.LastIndexByte(value, '-')
	if i < 0 {
		return "", 0, false
	}
	n, err := strconv.Atoi(value[i+1:])
	kind = value[:i]
	return kind, n, err == nil && n > 0 && (kind == "drafting" || kind == "waiting" || kind == "invalid" || kind == "failed")
}

// drafter is the architect controller. Its pass asks the architect for a
// draft of every handed workstream's spec and plan, and its reconciler runs
// each draft operation: one architect turn, the recording of what it
// delivered, validation, and the move to sketched or the record of why not.
type drafter struct {
	s          *Service
	repository *trace.Repository
}

var _ coreadapter.Reconciler = (*drafter)(nil)

// Pass reconciles every workstream of the trace except the librarian's. A
// service without an architect runner requests nothing: handed workstreams
// wait for a service that has one.
func (d *drafter) Pass(ctx context.Context) error {
	if d.s.options.Architect == nil {
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
			return fmt.Errorf("workstream %s draft: %w", stream, err)
		}
	}
	return nil
}

// reconcile requests the first draft of a handed workstream, the next draft
// after an invalid or failed one while drafts remain, and tells the chief of
// staff once they are exhausted. A draft parked on the architect's question
// resumes once the answer is queued on the architect's thread. A draft in
// progress, a parked draft whose answer is not queued and a workstream in any
// other feature state need nothing.
func (d *drafter) reconcile(ctx context.Context, stream config.WorkstreamID) error {
	feature, err := d.repository.Workflow(stream, trace.FeatureSubject)
	if err != nil {
		return err
	}
	if feature.Value != HandedState {
		return nil
	}
	state, err := d.repository.Workflow(stream, draftSubject)
	if err != nil {
		return err
	}
	if state.Value == "" {
		if err := d.ensureThread(ctx, stream); err != nil {
			return err
		}
		return d.request(ctx, stream, state, draftInput{Draft: 1}, handInTransition, "the workstream was handed in; the architect is asked for draft 1 of the spec and plan")
	}
	kind, n, ok := draftState(state.Value)
	switch {
	case !ok || kind == "drafting":
		return nil
	case kind == "waiting":
		return d.resume(ctx, stream, state, n)
	case n < maxDrafts:
		return d.request(ctx, stream, state, draftInput{Draft: n + 1}, fmt.Sprintf("draft-%d-%s", n, kind), fmt.Sprintf("draft %d was %s; the architect is asked for draft %d", n, kind, n+1))
	}
	last, err := d.transition(stream, fmt.Sprintf("draft-%d-%s", n, kind))
	if err != nil {
		return err
	}
	reason := fmt.Sprintf("none of the architect's %d drafts of the spec and plan was accepted; drafting stops and the workstream stays %s", n, HandedState)
	body := fmt.Sprintf("None of the architect's %d drafts of the spec and plan was accepted and drafting has stopped; the workstream stays %s. Last draft: %s", n, HandedState, last.Reason)
	tx := trace.Transaction{ExpectedVersion: state.Version,
		Transition: trace.Transition{Header: d.header("draft-exhausted", stream, last.ID, d.s.now()), Subject: draftSubject, From: state.Value, To: "exhausted", Reason: reason},
		Events:     []trace.Event{trace.Notice("draft-exhausted", "chief", body)}}
	_, err = d.repository.Transact(ctx, tx)
	return err
}

func (d *drafter) header(id string, stream config.WorkstreamID, cause string, at time.Time) trace.Header {
	return trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: d.repository.Project(), Workstream: stream, At: at, Actor: draftingActor, Cause: cause}
}

// transition returns the workstream's transition with the given ID.
func (d *drafter) transition(stream config.WorkstreamID, id string) (trace.Transition, error) {
	transitions, err := trace.Read[trace.Transition](d.repository, stream)
	if err != nil {
		return trace.Transition{}, err
	}
	for _, t := range transitions {
		if t.ID == id {
			return t, nil
		}
	}
	return trace.Transition{}, fmt.Errorf("transition %s is missing", id)
}

// ensureThread creates the workstream's architect thread when it is missing.
func (d *drafter) ensureThread(ctx context.Context, stream config.WorkstreamID) error {
	_, err := d.repository.Thread(stream, architectAgent)
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	identity := trace.Agent{Header: trace.Header{Schema: "osmia.trace.agent", Version: trace.Version, ID: architectAgent, Revision: 1, Project: d.repository.Project(), Workstream: stream, At: d.s.now(), Actor: draftingActor, Cause: "architect-thread"}, Role: architectRole, ThreadID: architectThread}
	return d.repository.CreateThread(ctx, identity)
}

// request publishes the draft the input numbers as a durable operation.
func (d *drafter) request(ctx context.Context, stream config.WorkstreamID, state trace.WorkflowState, in draftInput, cause, reason string) error {
	transition, event := in.runIDs()
	input, err := json.Marshal(in)
	if err != nil {
		return err
	}
	body := fmt.Sprintf("Draft %d of the spec and plan", in.Draft)
	if in.Resume > 0 {
		body += ", resumed"
	}
	op := coreadapter.Operation{ID: trace.OperationID(d.repository.Project(), stream, event), Boundary: coreadapter.RunnerBoundary, Action: DraftAction, Input: input}
	tx := trace.Transaction{ExpectedVersion: state.Version,
		Transition: trace.Transition{Header: d.header(transition, stream, cause, d.s.now()), Subject: draftSubject, From: state.Value, To: fmt.Sprintf("drafting-%d", in.Draft), Reason: reason},
		Events:     []trace.Event{{ID: event, Kind: "draft", Body: body, Operation: &op}}}
	_, err = d.repository.Transact(ctx, tx)
	return err
}

// resume requests draft n again once the answer to the architect's question
// is queued on its thread, caused by the transition that parked the draft.
func (d *drafter) resume(ctx context.Context, stream config.WorkstreamID, state trace.WorkflowState, n int) error {
	chain, asked, err := d.chain(stream, n)
	if err != nil {
		return err
	}
	if len(chain) > 0 && askedBy(asked, architectThread, chain[len(chain)-1].Request.TurnID) != "" {
		return nil
	}
	transitions, err := trace.Read[trace.Transition](d.repository, stream)
	if err != nil {
		return err
	}
	var parks []trace.Transition
	for _, t := range transitions {
		if t.Subject == draftSubject && strings.HasPrefix(t.ID, fmt.Sprintf("draft-%d-waiting-", n)) {
			parks = append(parks, t)
		}
	}
	if len(parks) == 0 {
		return fmt.Errorf("draft %d is parked without a parking transition", n)
	}
	return d.request(ctx, stream, state, draftInput{Draft: n, Resume: len(parks)}, parks[len(parks)-1].ID, fmt.Sprintf("draft %d resumes: the architect's question is answered", n))
}

// chain returns the architect's turns of draft n in thread order, its
// attempts and the turns that answered their questions, with the
// workstream's questions.
func (d *drafter) chain(stream config.WorkstreamID, n int) ([]trace.QueuedTurn, []trace.QuestionState, error) {
	t, err := d.repository.Thread(stream, architectAgent)
	if err != nil {
		return nil, nil, err
	}
	asked, err := d.repository.Questions(stream)
	if err != nil {
		return nil, nil, err
	}
	return chainTurns(t, askChain(t, draftTurnPrefix(n), asked)), asked, nil
}

func decodeDraft(op coreadapter.Operation) (draftInput, error) {
	var in draftInput
	if op.Boundary != coreadapter.RunnerBoundary || op.Action != DraftAction {
		return in, fmt.Errorf("unsupported runner operation %q", op.Action)
	}
	dec := json.NewDecoder(bytes.NewReader(op.Input))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return in, fmt.Errorf("invalid draft operation input: %w", err)
	}
	if in.Draft < 1 {
		return in, errors.New("draft operation requires a positive draft number")
	}
	if in.Resume < 0 {
		return in, errors.New("draft operation input carries an invalid resumption number")
	}
	return in, nil
}

// stream returns the workstream a draft operation belongs to: the one whose
// run event derives the operation ID.
func (d *drafter) stream(op coreadapter.Operation, in draftInput) (config.WorkstreamID, error) {
	streams, err := d.repository.Workstreams()
	if err != nil {
		return "", err
	}
	_, event := in.runIDs()
	for _, stream := range streams {
		if trace.OperationID(d.repository.Project(), stream, event) == op.ID {
			return stream, nil
		}
	}
	return "", fmt.Errorf("draft operation %s belongs to no workstream", op.ID)
}

// outcome returns the recorded terminal result of the operation that runs
// draft n: succeeded when the operation moved the workstream to sketched,
// failed when the draft's invalid or failed transition is recorded, waiting
// when the operation parked the draft on the architect's question, nil before
// any of them.
func (d *drafter) outcome(stream config.WorkstreamID, n int, operation string) (*coreadapter.OperationResult, error) {
	transitions, err := trace.Read[trace.Transition](d.repository, stream)
	if err != nil {
		return nil, err
	}
	invalid, failed, parked := fmt.Sprintf("draft-%d-invalid", n), fmt.Sprintf("draft-%d-failed", n), fmt.Sprintf("draft-%d-waiting-", n)
	for _, t := range transitions {
		switch {
		case t.Subject == draftSubject && strings.HasPrefix(t.ID, parked) && t.Cause == operation:
			return &coreadapter.OperationResult{Outcome: questions.Waiting, Evidence: t.Reason}, nil
		case t.Subject == trace.FeatureSubject && t.To == SketchedState && t.Cause == operation:
			return &coreadapter.OperationResult{Outcome: "succeeded", Evidence: t.Reason}, nil
		case t.Subject == draftSubject && (t.ID == invalid || t.ID == failed):
			return &coreadapter.OperationResult{Outcome: "failed", Evidence: t.Reason}, nil
		}
	}
	return nil, nil
}

// Inspect reads the recorded transitions and the architect thread. A recorded
// outcome completes the operation; a turn this session is running is unknown;
// everything else is absent, and Apply decides between running, retrying and
// recording.
func (d *drafter) Inspect(_ context.Context, op coreadapter.Operation) (coreadapter.Observation, error) {
	in, err := decodeDraft(op)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	stream, err := d.stream(op, in)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	result, err := d.outcome(stream, in.Draft, op.ID)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	if result != nil {
		return coreadapter.Observation{State: coreadapter.EffectCompleted, Evidence: "draft " + result.Outcome, Result: result}, nil
	}
	t, err := d.repository.Thread(stream, architectAgent)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	turns, _, err := d.chain(stream, in.Draft)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	if len(turns) == 0 {
		return coreadapter.Observation{State: coreadapter.EffectAbsent, Evidence: "no architect turn has run"}, nil
	}
	last := turns[len(turns)-1]
	switch {
	case last.Claim != nil && last.Response == nil && t.Status != "interrupted":
		return coreadapter.Observation{State: coreadapter.EffectUnknown, Evidence: "architect turn " + last.Request.TurnID + " is running"}, nil
	case last.Claim != nil && last.Response == nil:
		return coreadapter.Observation{State: coreadapter.EffectAbsent, Evidence: "architect turn " + last.Request.TurnID + " was interrupted by a service stop"}, nil
	case last.Claim == nil:
		return coreadapter.Observation{State: coreadapter.EffectAbsent, Evidence: "architect turn " + last.Request.TurnID + " is queued and unclaimed"}, nil
	case last.CompletedAt.IsZero():
		return coreadapter.Observation{State: coreadapter.EffectAbsent, Evidence: "architect turn " + last.Request.TurnID + " is captured and not completed"}, nil
	}
	return coreadapter.Observation{State: coreadapter.EffectAbsent, Evidence: "architect turn " + last.Request.TurnID + " is " + last.Status() + "; draft not recorded"}, nil
}

// errNoArchitect leaves a draft pending in a service that cannot run the
// architect's turn, so the draft is not spent on the missing runner.
var errNoArchitect = errors.New("this service has no agent runner for the architect")

// Apply drives the draft to a terminal result: it abandons a turn a previous
// service stop interrupted, starts a new turn while attempts remain, runs the
// pending turn, an attempt or the answer to the architect's question, through
// the dispatcher, and records a completed turn's draft. A turn that asked a
// question, whatever it ended with, parks the draft instead: the draft moves
// to waiting-<n>, the operation ends waiting, and the controller requests the
// draft again once the answer is queued. Abandoning the workstream cancels
// the running turn, and the draft of an abandoned workstream fails without
// starting another.
// A failed turn and an invalid draft are terminal failures recorded as draft
// transitions; storage errors and a missing architect runner leave the
// operation pending for another attempt.
func (d *drafter) Apply(ctx context.Context, op coreadapter.Operation) (coreadapter.OperationResult, error) {
	in, err := decodeDraft(op)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	stream, err := d.stream(op, in)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	if result, err := d.outcome(stream, in.Draft, op.ID); err != nil || result != nil {
		if err != nil {
			return coreadapter.OperationResult{}, err
		}
		return *result, nil
	}
	cfg := d.s.current()
	if !cfg.HasProject() || cfg.Project.ID != d.repository.Project() {
		return coreadapter.OperationResult{}, errors.New("the project is not active")
	}
	n := in.Draft
	// Abandoning the workstream cancels the running turn, not the recording
	// of what it left.
	running, cancel := context.WithCancel(ctx)
	defer cancel()
	defer d.s.turns.add(stream, cancel)()
	for {
		if err := ctx.Err(); err != nil {
			return coreadapter.OperationResult{}, err
		}
		t, err := d.repository.Thread(stream, architectAgent)
		if err != nil {
			return coreadapter.OperationResult{}, err
		}
		asked, err := d.repository.Questions(stream)
		if err != nil {
			return coreadapter.OperationResult{}, err
		}
		origins := askChain(t, draftTurnPrefix(n), asked)
		chain := chainTurns(t, origins)
		tries := attempts(chain, origins)
		// Turns run in sequence, so the oldest unfinished turn of the chain is
		// the one to drive: the answer to a question may be queued behind the
		// turn that asked it while a service stop left that turn unfinished.
		var last, pending *trace.QueuedTurn
		if len(chain) > 0 {
			last = &chain[len(chain)-1]
		}
		if i := slices.IndexFunc(chain, func(q trace.QueuedTurn) bool { return q.CompletedAt.IsZero() }); i >= 0 {
			pending = &chain[i]
		}
		switch {
		case pending != nil && pending.Claim != nil && pending.Response == nil:
			if t.Status != "interrupted" {
				return coreadapter.OperationResult{}, errors.New("architect turn " + pending.Request.TurnID + " is still running")
			}
			if err := d.repository.AbandonTurn(ctx, stream, architectAgent, pending.Request.TurnID, d.s.now()); err != nil {
				return coreadapter.OperationResult{}, err
			}
		case pending != nil:
			gone, err := abandoned(d.repository, stream)
			if err != nil {
				return coreadapter.OperationResult{}, err
			}
			if gone && pending.Response == nil {
				if _, err := d.repository.CancelTurns(ctx, stream, d.s.now(), abandonActor, cancelReason); err != nil {
					return coreadapter.OperationResult{}, err
				}
				continue
			}
			// Completing a captured turn runs no architect.
			turnCtx := running
			if pending.Response != nil {
				turnCtx = ctx
			} else if d.s.options.Architect == nil {
				return coreadapter.OperationResult{}, errNoArchitect
			}
			if _, err := d.dispatch(turnCtx, stream, pending.Request.TurnID); err != nil {
				return coreadapter.OperationResult{}, err
			}
			if err := os.RemoveAll(filepath.Join(d.turnDirectory(stream, pending.Request.TurnID), "workspace")); err != nil {
				return coreadapter.OperationResult{}, err
			}
		case last != nil && askedBy(asked, architectThread, last.Request.TurnID) != "":
			if gone, err := abandoned(d.repository, stream); err != nil || gone {
				if err != nil {
					return coreadapter.OperationResult{}, err
				}
				return d.terminal(ctx, op.ID, stream, n, "failed", fmt.Sprintf("draft %d failed: the workstream was abandoned, so the architect runs no turn for it", n))
			}
			id := askedBy(asked, architectThread, last.Request.TurnID)
			return d.park(ctx, op.ID, stream, in, fmt.Sprintf("draft %d is parked: the architect waits for the answer to question %s", n, id))
		case last == nil || last.Status() == "interrupted":
			if gone, err := abandoned(d.repository, stream); err != nil || gone {
				if err != nil {
					return coreadapter.OperationResult{}, err
				}
				return d.terminal(ctx, op.ID, stream, n, "failed", fmt.Sprintf("draft %d failed: the workstream was abandoned, so the architect runs no turn for it", n))
			}
			if len(tries) >= maxDraftAttempts {
				return d.terminal(ctx, op.ID, stream, n, "failed", fmt.Sprintf("draft %d failed: the architect turn was interrupted %d times by service stops", n, len(tries)))
			}
			if d.s.options.Architect == nil {
				return coreadapter.OperationResult{}, errNoArchitect
			}
			if err := d.enqueue(ctx, cfg, stream, n, len(tries)+1, op.ID, received(t, origins, asked)); err != nil {
				return coreadapter.OperationResult{}, err
			}
		case last.Status() == "idle":
			return d.record(ctx, op.ID, stream, n, origins[last.Request.TurnID])
		default:
			reason := last.Response.Failure
			if reason == "" {
				reason = "the turn ended with status " + last.Status()
			}
			return d.terminal(ctx, op.ID, stream, n, "failed", fmt.Sprintf("draft %d failed: architect turn %s failed: %s", n, last.Request.TurnID, reason))
		}
	}
}

// terminal records why draft n ended without a sketched workstream as a draft
// transition, invalid-<n> or failed-<n>, and returns the failed result. The
// transition already recorded is returned as it is.
func (d *drafter) terminal(ctx context.Context, operation string, stream config.WorkstreamID, n int, kind, reason string) (coreadapter.OperationResult, error) {
	return d.end(ctx, operation, stream, n, fmt.Sprintf("draft-%d-%s", n, kind), fmt.Sprintf("%s-%d", kind, n), coreadapter.OperationResult{Outcome: "failed", Evidence: reason})
}

// park records that the architect's question parks the draft the operation
// runs, moving it to waiting-<n>, and returns the waiting result. The
// transition already recorded is returned as it is.
func (d *drafter) park(ctx context.Context, operation string, stream config.WorkstreamID, in draftInput, reason string) (coreadapter.OperationResult, error) {
	return d.end(ctx, operation, stream, in.Draft, in.parkedID(), fmt.Sprintf("waiting-%d", in.Draft), coreadapter.OperationResult{Outcome: questions.Waiting, Evidence: reason})
}

// end moves draft n from drafting-<n> to the given state with the given
// transition and returns result. A draft no longer in progress returns what
// the operation recorded.
func (d *drafter) end(ctx context.Context, operation string, stream config.WorkstreamID, n int, id, to string, result coreadapter.OperationResult) (coreadapter.OperationResult, error) {
	state, err := d.repository.Workflow(stream, draftSubject)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	if state.Value != fmt.Sprintf("drafting-%d", n) {
		result, err := d.outcome(stream, n, operation)
		if err != nil || result == nil {
			return coreadapter.OperationResult{}, errors.Join(err, fmt.Errorf("draft %d is %q, not in progress", n, state.Value))
		}
		return *result, nil
	}
	tx := trace.Transaction{ExpectedVersion: state.Version,
		Transition: trace.Transition{Header: d.header(id, stream, operation, d.s.now()), Subject: draftSubject, From: state.Value, To: to, Reason: result.Evidence}}
	if _, err := d.repository.Transact(ctx, tx); err != nil {
		return coreadapter.OperationResult{}, err
	}
	return result, nil
}

// enqueue accepts the architect turn of one draft attempt, fixing its profile
// and prompts. The turn of a later draft lists why the previous one was not
// accepted, and a later attempt repeats the answers the architect received
// in the draft, whose turns a service stop interrupted.
func (d *drafter) enqueue(ctx context.Context, cfg *config.Config, stream config.WorkstreamID, n, attempt int, operation string, answers []string) error {
	profile, _, err := d.s.roleExecution(cfg, architectRole)
	if err != nil {
		return err
	}
	handed, err := d.handed(stream)
	if err != nil {
		return err
	}
	previous, drafted := "", false
	if n > 1 {
		last, err := d.previous(stream, n-1)
		if err != nil {
			return err
		}
		previous = last.Reason
		latest, err := d.latest(stream)
		if err != nil {
			return err
		}
		_, spec := latest[plan.SpecDocument]
		_, graph := latest[plan.PlanDocument]
		drafted = spec || graph
	}
	turn := draftTurnID(n, attempt)
	req := trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_" + turn, Revision: 1, Project: d.repository.Project(), Workstream: stream, At: d.s.now(), Actor: draftingActor, Cause: operation, Depth: 1},
		AgentID: architectAgent, ThreadID: architectThread, TurnID: turn, Profile: profile, SystemPrompt: architectSystemPrompt(cfg.Project), Prompt: architectPrompt(handed.Path, n, previous, drafted, answers)}
	_, err = d.repository.EnqueueTurn(ctx, req)
	return err
}

// previous returns the transition that ended draft n.
func (d *drafter) previous(stream config.WorkstreamID, n int) (trace.Transition, error) {
	for _, kind := range []string{"invalid", "failed"} {
		if t, err := d.transition(stream, fmt.Sprintf("draft-%d-%s", n, kind)); err == nil {
			return t, nil
		}
	}
	return trace.Transition{}, fmt.Errorf("draft %d has no recorded outcome", n)
}

// latest returns the latest revision of each of the workstream's documents.
func (d *drafter) latest(stream config.WorkstreamID) (map[string]trace.Document, error) {
	docs, err := trace.Read[trace.Document](d.repository, stream)
	if err != nil {
		return nil, err
	}
	latest := map[string]trace.Document{}
	for _, doc := range docs {
		latest[doc.ID] = doc
	}
	return latest, nil
}

// handed returns the workstream's handed document.
func (d *drafter) handed(stream config.WorkstreamID) (trace.Document, error) {
	latest, err := d.latest(stream)
	if err != nil {
		return trace.Document{}, err
	}
	doc, ok := latest[handedDocument]
	if !ok {
		return trace.Document{}, fmt.Errorf("workstream %s has no handed input", stream)
	}
	return doc, nil
}

// turnDirectory is the service-owned directory of one architect turn: its
// staged view, its backend session and the delivered draft.
func (d *drafter) turnDirectory(stream config.WorkstreamID, turn string) string {
	return filepath.Join(d.s.current().Root.String(), "architect", string(d.repository.Project()), string(stream), turn)
}

// dispatch runs the turn through the thread dispatcher and runner.
func (d *drafter) dispatch(ctx context.Context, stream config.WorkstreamID, turn string) (coreadapter.OperationResult, error) {
	dispatcher := thread.Dispatcher{Runner: threadRunner(d.s.current(), d.repository, &questions.Turns{Turns: d.turns(stream), Repository: d.repository}, d.s.now), Prepare: func(_ context.Context, in thread.TurnInput) (coreadapter.PreparedTurn, error) {
		directory := filepath.Join(d.turnDirectory(in.Workstream, in.Turn), "session")
		return coreadapter.PreparedTurn{SessionDirectory: directory}, os.MkdirAll(directory, 0700)
	}}
	op, err := thread.TurnOperation(d.repository.Project(), "draft-turn", thread.TurnInput{Workstream: stream, Agent: architectAgent, Turn: turn})
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	return dispatcher.Apply(ctx, op)
}

// turns is the architect's isolated turn path: a read-only private view of
// the handed input, the charter and the context bundle, the file reading
// tool, the delivery tool and the question tool, and no notes, write,
// execute, network or VCS capability: notes are the project's, and would
// carry one workstream's drafting into another's. A turn that asks ends
// waiting, so its thread parks and its slot is free while the answer is
// found.
func (d *drafter) turns(stream config.WorkstreamID) *isolation.Turns {
	var engine coreadapter.Engine
	var hosts coreadapter.MCPHosts
	if a := d.s.options.Architect; a != nil {
		engine, hosts = a.Engine, a.Hosts
	}
	return &isolation.Turns{
		Workspaces: stagedWorkspaces{},
		Views:      isolation.Views{Directory: filepath.Join(d.s.current().Root.String(), "views")},
		Select:     d.selectView,
		Grants:     map[string]coreadapter.Capabilities{architectRole: {Tools: []string{"file_read", DraftTool, questions.AskTool}}},
		Scoped: func(_ context.Context, scope coreadapter.Scope) ([]coreadapter.Tool, error) {
			if scope.Workstream != string(stream) {
				return nil, errors.New("turn scope denied")
			}
			origin, err := d.continued(stream, scope.Turn)
			if err != nil {
				return nil, err
			}
			ask, err := questions.Tools(d.repository, architectAgent, scope, d.s.now)
			if err != nil {
				return nil, err
			}
			return append([]coreadapter.Tool{d.draftTool(scope, origin)}, ask...), nil
		},
		Hosts:  hosts,
		Engine: engine,
	}
}

// continued returns the architect's turn the given turn continues: the turn
// itself, or, for the turn that delivers the answer to a question, the turn
// that asked it, followed back to a turn that answers none.
func (d *drafter) continued(stream config.WorkstreamID, turn string) (string, error) {
	asked, err := d.repository.Questions(stream)
	if err != nil {
		return "", err
	}
	for {
		i := slices.IndexFunc(asked, func(q trace.QuestionState) bool {
			return q.Asked.Thread == architectThread && questions.TurnID(q.Asked.ID) == turn
		})
		if i < 0 {
			return turn, nil
		}
		turn = asked[i].Asked.Turn
	}
}

// draftTool delivers one of the two draft files into the output directory of
// the attempt the claimed turn continues, which the service records once the
// attempt completes: the turn that delivers the answer to the architect's
// question adds to what the turn that asked it delivered.
func (d *drafter) draftTool(scope coreadapter.Scope, attempt string) coreadapter.Tool {
	return coreadapter.Tool{Name: DraftTool, Description: "Deliver spec.md, plan.json, or an amendment decline.txt. The service records and validates delivered files.", Effect: coreadapter.ToolMemory,
		InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","enum":["spec.md","plan.json","decline.txt"]},"content":{"type":"string"}},"required":["path","content"],"additionalProperties":false}`),
		Handle: func(_ context.Context, raw json.RawMessage) (json.RawMessage, error) {
			var input struct{ Path, Content string }
			dec := json.NewDecoder(bytes.NewReader(raw))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&input); err != nil {
				return nil, err
			}
			if input.Path != plan.SpecPath && input.Path != plan.PlanPath && !(input.Path == "decline.txt" && strings.HasPrefix(scope.Turn, "amend-")) {
				return nil, fmt.Errorf("path must be %s or %s", plan.SpecPath, plan.PlanPath)
			}
			if len(input.Content) > MaxHandedBytes {
				return nil, fmt.Errorf("%s is larger than %d bytes", input.Path, MaxHandedBytes)
			}
			if scope.Role != architectRole || scope.Project != string(d.repository.Project()) {
				return nil, errors.New("turn scope denied")
			}
			directory := filepath.Join(d.turnDirectory(config.WorkstreamID(scope.Workstream), attempt), "output")
			if err := os.MkdirAll(directory, 0700); err != nil {
				return nil, err
			}
			if err := os.WriteFile(filepath.Join(directory, input.Path), []byte(input.Content), 0600); err != nil {
				return nil, err
			}
			return json.RawMessage(`{}`), nil
		}}
}

// selectView stages the architect's view for the claimed turn and selects all
// of it, read-only.
func (d *drafter) selectView(ctx context.Context, scope coreadapter.Scope) (isolation.Selection, error) {
	cfg := d.s.current()
	if scope.Role != architectRole || scope.Project != string(d.repository.Project()) || !cfg.HasProject() || cfg.Project.ID != d.repository.Project() {
		return isolation.Selection{}, errors.New("view selection denied")
	}
	stream, err := config.ParseWorkstreamID(scope.Workstream)
	if err != nil {
		return isolation.Selection{}, err
	}
	_, settings, err := d.s.roleExecution(cfg, architectRole)
	if err != nil {
		return isolation.Selection{}, err
	}
	if err := os.MkdirAll(filepath.Join(cfg.Root.String(), "views"), 0700); err != nil {
		return isolation.Selection{}, err
	}
	workspace := filepath.Join(d.turnDirectory(stream, scope.Turn), "workspace")
	paths, err := d.stage(ctx, stream, scope.Turn, workspace)
	if err != nil {
		return isolation.Selection{}, err
	}
	return isolation.Selection{Workspace: coreadapter.WorkspaceRequest{SourceDirectory: workspace, Directory: workspace}, Paths: paths, Execution: settings}, nil
}

// stage builds the view: the handed input under handed/, charter.md, the
// rendered context bundle as context.md and, once a draft is recorded, its
// latest spec.md and plan.json under draft/. It returns the paths to select.
func (d *drafter) stage(ctx context.Context, stream config.WorkstreamID, turn, workspace string) ([]string, error) {
	if err := os.RemoveAll(workspace); err != nil {
		return nil, err
	}
	latest, err := d.latest(stream)
	if err != nil {
		return nil, err
	}
	handed, ok := latest[handedDocument]
	if !ok {
		return nil, fmt.Errorf("workstream %s has no handed input", stream)
	}
	charter, err := d.repository.Charter(ctx, d.s.now())
	if err != nil {
		return nil, err
	}
	b, err := (bundle.Files{Repository: func(config.ProjectID) (*trace.Repository, error) { return d.repository, nil }, Now: d.s.now}).Assemble(ctx, d.repository.Project(), bundle.Scope{Workstream: stream})
	if err != nil {
		return nil, err
	}
	files := map[string]string{handed.Path: handed.Content, "charter.md": charter.Content, "context.md": b.Render()}
	paths := []string{"handed", "charter.md", "context.md"}
	if id, ok := amendmentTurnID(turn); ok {
		requests, err := trace.Read[trace.Amendment](d.repository, stream)
		if err != nil {
			return nil, err
		}
		for _, request := range requests {
			if request.ID == id {
				data, err := json.MarshalIndent(request, "", "  ")
				if err != nil {
					return nil, err
				}
				files["request.json"] = string(data)
				paths = append(paths, "request.json")
				break
			}
		}
		if _, ok := files["request.json"]; !ok {
			return nil, fmt.Errorf("amendment %s is missing", id)
		}
	}
	drafted := false
	for _, id := range []string{plan.SpecDocument, plan.PlanDocument} {
		if doc, ok := latest[id]; ok {
			files["draft/"+doc.Path] = doc.Content
			drafted = true
		}
	}
	if drafted {
		paths = append(paths, "draft")
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

// delivered reads the files the turn delivered. A file that is missing or
// not UTF-8 text is a problem with the draft, not a storage error.
func (d *drafter) delivered(stream config.WorkstreamID, turn string) (map[string]string, []string, error) {
	directory := filepath.Join(d.turnDirectory(stream, turn), "output")
	files := map[string]string{}
	var problems []string
	for _, path := range []string{plan.SpecPath, plan.PlanPath} {
		data, err := os.ReadFile(filepath.Join(directory, path))
		switch {
		case errors.Is(err, fs.ErrNotExist):
			problems = append(problems, path+" was not delivered")
		case err != nil:
			return nil, nil, err
		case !utf8.Valid(data):
			problems = append(problems, path+" is not UTF-8 text")
		default:
			files[path] = string(data)
		}
	}
	return files, problems, nil
}

// record ingests the completed attempt's draft as architect-authored revisions
// of spec.md and plan.json in one commit, keyed by the operation, then
// validates the recorded draft: a valid one moves the workstream to sketched,
// an invalid one is recorded with its problems.
func (d *drafter) record(ctx context.Context, operation string, stream config.WorkstreamID, n int, attempt string) (coreadapter.OperationResult, error) {
	latest, err := d.latest(stream)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	ingested := latest[plan.SpecDocument].Cause == operation || latest[plan.PlanDocument].Cause == operation
	var problems []string
	if !ingested {
		files, undelivered, err := d.delivered(stream, attempt)
		if err != nil {
			return coreadapter.OperationResult{}, err
		}
		problems = undelivered
		at := d.s.now()
		var records []trace.Document
		for _, doc := range []struct{ id, path string }{{plan.SpecDocument, plan.SpecPath}, {plan.PlanDocument, plan.PlanPath}} {
			content, ok := files[doc.path]
			if !ok {
				continue
			}
			records = append(records, trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: doc.id, Revision: latest[doc.id].Revision + 1, Project: d.repository.Project(), Workstream: stream, At: at, Actor: architectActor, Cause: operation, Depth: 1}, Path: doc.path, Content: content})
		}
		if len(records) > 0 {
			if err := d.repository.RecordDocuments(ctx, records); err != nil {
				return coreadapter.OperationResult{}, err
			}
			if latest, err = d.latest(stream); err != nil {
				return coreadapter.OperationResult{}, err
			}
		}
	}
	specDoc, planDoc := latest[plan.SpecDocument], latest[plan.PlanDocument]
	if ingested {
		for _, doc := range []struct {
			path string
			doc  trace.Document
		}{{plan.SpecPath, specDoc}, {plan.PlanPath, planDoc}} {
			if doc.doc.Cause != operation {
				problems = append(problems, doc.path+" was not delivered")
			}
		}
	}
	if len(problems) == 0 {
		found, err := d.validate(specDoc, planDoc)
		if err != nil {
			return coreadapter.OperationResult{}, err
		}
		problems = found
	}
	if len(problems) > 0 {
		return d.terminal(ctx, operation, stream, n, "invalid", fmt.Sprintf("draft %d of the spec and plan is invalid:\n- %s", n, strings.Join(problems, "\n- ")))
	}
	return d.sketch(ctx, operation, stream, n, specDoc, planDoc)
}

// validate returns every problem with the recorded draft.
func (d *drafter) validate(specDoc, planDoc trace.Document) ([]string, error) {
	entities, err := kb.Load(d.repository)
	if err != nil {
		return nil, err
	}
	return validateDraft(specDoc.Content, planDoc.Content, entities), nil
}

// validateDraft returns every problem with a spec and a plan against the
// entity map. It reads nothing, so a caller that already holds the trace can
// use it.
func validateDraft(spec, graph string, entities kb.Map) []string {
	parsed, parseErr := plan.Parse([]byte(graph))
	if parseErr != nil {
		parsed = plan.Plan{Version: plan.Version}
	}
	var out []string
	for _, p := range plan.Validate(plan.ParseSpec(spec), parsed, entities) {
		if parseErr == nil || p.Kind == plan.SpecProblem {
			out = append(out, p.Error())
		}
	}
	if parseErr != nil {
		out = append(out, plan.PlanPath+": "+parseErr.Error())
	}
	return out
}

// sketch moves the workstream from handed to sketched for a valid draft. The
// transition is timestamped with the draft's revisions, so a retry repeats
// the committed transition exactly.
func (d *drafter) sketch(ctx context.Context, operation string, stream config.WorkstreamID, n int, specDoc, planDoc trace.Document) (coreadapter.OperationResult, error) {
	spec := plan.ParseSpec(specDoc.Content)
	graph, err := plan.Parse([]byte(planDoc.Content))
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	reason := fmt.Sprintf("the architect's draft %d passed validation: %s revision %d with %d acceptance criteria and %s revision %d with %d units", n, specDoc.Path, specDoc.Revision, len(spec.Criteria), planDoc.Path, planDoc.Revision, len(graph.Units))
	h := d.header(SketchedState, stream, operation, specDoc.At)
	_, err = d.repository.MoveFeatureState(ctx, h, HandedState, SketchedState, reason)
	if errors.Is(err, trace.ErrConflict) {
		feature, readErr := d.repository.Workflow(stream, trace.FeatureSubject)
		if readErr != nil {
			return coreadapter.OperationResult{}, readErr
		}
		return d.terminal(ctx, operation, stream, n, "failed", fmt.Sprintf("draft %d failed: the workstream is %s, not %s, so the valid draft was recorded and not presented", n, feature.Value, HandedState))
	}
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	return coreadapter.OperationResult{Outcome: "succeeded", Evidence: reason}, nil
}

func architectSystemPrompt(p config.Project) string {
	return fmt.Sprintf("You are the architect of the %s project (%s). You draft one workstream's feature spec and plan from what the owner handed in, the project's charter and its knowledge base, in the vocabulary the knowledge base gives the project. You hold no version control tool: you deliver spec.md and plan.json with %s, and the service records and validates them. When your view does not settle something you must know, call %s: your work waits for the answer, which arrives as your next turn, and what you delivered so far is kept.", p.Name, p.Upstream, DraftTool, questions.AskTool)
}

func architectPrompt(handedPath string, n int, previous string, drafted bool, answers []string) string {
	var b strings.Builder
	draft := ""
	switch {
	case n > 1 && drafted:
		fmt.Fprintf(&b, "Draft %d was not accepted:\n%s\n\nYour previous spec.md and plan.json are in draft/. Deliver corrected files.\n\n", n-1, previous)
		draft = "- draft/spec.md and draft/plan.json: your previous draft.\n"
	case n > 1:
		fmt.Fprintf(&b, "Draft %d was not accepted:\n%s\n\nNo file of that draft was recorded. Deliver both files.\n\n", n-1, previous)
	}
	fmt.Fprintf(&b, `Draft the feature spec and the plan for this workstream.

Your view holds:
- %s: what the owner handed in, unchanged. It says what to build.
- charter.md: the owner's rules for contributing to this project. Cite them as charter#<n>.
- context.md: the project context bundle: the charter's rules, the knowledge-base prose per subsystem, the entity map and the recorded decisions. Its Entities section lists every entity a footprint may name.
%s
Deliver two files with %s; only what you deliver is kept.

1. spec.md, in Markdown: the intended behaviour, what the feature must not do, and a section headed "## Acceptance criteria" holding the acceptance criteria as a numbered list ("1. ...", "2. ..."), numbered from 1 without gaps or repeats. Each criterion is one statement that can be shown to hold; the plan cites it as spec#<n>.
2. plan.json: the directed graph of units, as {"version": 1, "units": [...]}. Each unit is {"id": "...", "title": "...", "addresses": [{"criterion": "spec#<n>", "proof": {"kind": "...", "name": "..."}}], "depends_on": ["..."], "footprint": ["..."]}. An id is 1 to 128 letters, digits, '_' or '-', starting with a letter or digit. addresses names every criterion the unit will show to hold and how: kind is new-test, existing-test, scripted-check or reviewer-judgement, and name is the test, the check or what the reviewer will judge. depends_on lists the ids of the units it must wait for, and the dependencies form no cycle. footprint lists the entities the unit will touch, by ID or alias from context.md, at least one per unit.

The draft is accepted only when every criterion in spec.md is addressed by at least one unit with a named proof, every citation names a criterion the spec has, every dependency names a unit in the plan without forming a cycle, and every footprint resolves against the entity map. How finely the work is cut into units is your call.

Call %s when something you must know to draft is not in your view: the draft waits for the answer, which arrives as your next turn.
`, handedPath, draft, DraftTool, questions.AskTool)
	b.WriteString(answered("draft", answers))
	return b.String()
}
