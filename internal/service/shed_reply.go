package service

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/kpenfound/osmia/internal/bundle"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/isolation"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
)

// ReplyAction is the runner-boundary operation action that runs the
// architect's one reply to a heard shed round, and RedraftAction the one that
// runs the redraft of the spec and the plan the owner asked for after debate
// concluded. Both are one architect turn on the same path, and differ in what
// the turn answers and where its record goes.
const (
	ReplyAction   = "shed-reply"
	RedraftAction = "shed-redraft"
)

const (
	// maxRedrafts bounds the redrafts the architect may deliver in one reply
	// before an invalid one is given up, and maxReplyAttempts the turns of one
	// reply that service stops may interrupt: the architect's drafting bounds.
	maxRedrafts      = maxDrafts
	maxReplyAttempts = maxDraftAttempts
	replyFile        = "reply.json"
)

// answering names what the architect's turn answers: one round of the
// committee, or the owner's request for a redraft after it.
func (in roundInput) answering() string {
	if in.Redraft {
		return "redraft"
	}
	return "reply"
}

// action is the operation action that runs the turn.
func (in roundInput) action() string {
	if in.Redraft {
		return RedraftAction
	}
	return ReplyAction
}

// running is the shed state while the turn runs, and recorded the kind of the
// state it reaches once its record is committed.
func (in roundInput) running() string { return fmt.Sprintf("%s-%d", in.answering(), in.Round) }
func (in roundInput) recorded() string {
	if in.Redraft {
		return "redrafted"
	}
	return "replied"
}

// ids are the transition and run-event IDs of the turn's operation.
func (in roundInput) ids() (transition, event string) {
	transition = fmt.Sprintf("shed-%s-%d", in.answering(), in.Round)
	return transition, trace.EventID(transition, "run")
}

// path and documentID are where the architect's record of the turn goes.
func (in roundInput) path() string {
	if in.Redraft {
		return shed.RedraftedPath(in.Round)
	}
	return shed.ReplyPath(in.Round)
}

func (in roundInput) documentID() string {
	if in.Redraft {
		return shed.RedraftedDocumentID(in.Round)
	}
	return shed.ReplyDocumentID(in.Round)
}

// about names the architect's answer for a message: its reply is to the round,
// and the redraft the owner asked for comes after it.
func (in roundInput) about() string {
	if in.Redraft {
		return fmt.Sprintf("the redraft after round %d", in.Round)
	}
	return fmt.Sprintf("the reply to round %d", in.Round)
}

// turnPrefix is what every turn of the operation is named with, and turnID
// the name of its k-th turn.
func (in roundInput) turnPrefix() string { return fmt.Sprintf("%s-%d-", in.answering(), in.Round) }
func (in roundInput) turnID(k int) string {
	return in.turnPrefix() + strconv.Itoa(k)
}

// runs returns the transition and event of the operation that runs the
// turn: its own, or the resumption the input numbers.
func (in roundInput) runs() (transition, event string) {
	if in.Resume == 0 {
		return in.ids()
	}
	id, _ := in.ids()
	transition = fmt.Sprintf("%s-resume-%d", id, in.Resume)
	return transition, trace.EventID(transition, "run")
}

// parked is the transition that parks the architect's answer the operation
// runs: the k-th park follows the k-1-th resumption.
func (in roundInput) parked() string {
	id, _ := in.ids()
	return fmt.Sprintf("%s-waiting-%d", id, in.Resume+1)
}

// chain maps the operation's turns of the architect thread to the attempt
// each continues.
func (in roundInput) chain(t trace.Thread, asked []trace.QuestionState) map[string]string {
	return askChain(t, in.turnPrefix(), asked)
}

func replyIDs(n int) (transition, event string) { return roundInput{Round: n}.ids() }
func replyTurnID(n, k int) string               { return roundInput{Round: n}.turnID(k) }

// replier reconciles the architect's reply operations of the shed controller.
type replier struct{ *debate }

var _ coreadapter.Reconciler = replier{}

func (d *debate) drafter() *drafter { return &drafter{s: d.s, repository: d.repository} }

// requestReply publishes the architect's reply to a heard round as a durable
// operation pinned to the revision the round debated.
func (d *debate) requestReply(ctx context.Context, stream config.WorkstreamID, state trace.WorkflowState, in roundInput, open int) error {
	round, _ := roundIDs(in.Round)
	reason := fmt.Sprintf("%d objections stand after round %d; the architect is asked for its reply", open, in.Round)
	return d.requestAnswer(ctx, stream, state, in, round+"-heard", reason, fmt.Sprintf("Architect's reply to committee round %d", in.Round))
}

// requestRedraft publishes the redraft the owner asked for after round n as a
// durable operation pinned to the latest recorded revisions, which are what
// the architect redrafts and what the owner may have edited since the request.
func (d *debate) requestRedraft(ctx context.Context, stream config.WorkstreamID, state trace.WorkflowState, in roundInput, note string) error {
	reason := fmt.Sprintf("the owner asked for a redraft after round %d: %s", in.Round, note)
	return d.requestAnswer(ctx, stream, state, in, shed.RedraftDocumentID(in.Round), reason, fmt.Sprintf("Architect's redraft after round %d, at the owner's request", in.Round))
}

// requestAnswer publishes one architect turn of the shed as a durable
// operation and moves the shed to the state that says it is running.
func (d *debate) requestAnswer(ctx context.Context, stream config.WorkstreamID, state trace.WorkflowState, in roundInput, cause, reason, body string) error {
	if err := d.drafter().ensureThread(ctx, stream); err != nil {
		return err
	}
	transition, event := in.runs()
	input, err := encodeRound(in)
	if err != nil {
		return err
	}
	op := coreadapter.Operation{ID: trace.OperationID(d.repository.Project(), stream, event), Boundary: coreadapter.RunnerBoundary, Action: in.action(), Input: input}
	tx := trace.Transaction{ExpectedVersion: state.Version,
		Transition: trace.Transition{Header: d.header(transition, stream, cause, d.s.now()), Subject: shedSubject, From: state.Value, To: in.running(), Reason: reason},
		Events:     []trace.Event{{ID: event, Kind: in.action(), Body: body, Operation: &op}}}
	_, err = d.repository.Transact(ctx, tx)
	return err
}

// resumeAnswer requests the architect's parked reply or redraft after round
// n again once the answer to its question is queued on the architect's
// thread. The resumption is pinned to the revision the parked operation was,
// and caused by the transition that parked it.
func (d *debate) resumeAnswer(ctx context.Context, stream config.WorkstreamID, state trace.WorkflowState, n int) error {
	transitions, err := trace.Read[trace.Transition](d.repository, stream)
	if err != nil {
		return err
	}
	var park trace.Transition
	for _, t := range transitions {
		if t.Subject == shedSubject {
			park = t
		}
	}
	in := roundInput{Round: n, Redraft: strings.HasPrefix(park.ID, "shed-redraft-")}
	id, event := in.ids()
	if !strings.HasPrefix(park.ID, id+"-waiting-") {
		return fmt.Errorf("the shed is %s without a parking transition", state.Value)
	}
	t, err := d.repository.Thread(stream, architectAgent)
	if err != nil {
		return err
	}
	asked, err := d.repository.Questions(stream)
	if err != nil {
		return err
	}
	if turns := chainTurns(t, in.chain(t, asked)); len(turns) > 0 && askedBy(asked, architectThread, turns[len(turns)-1].Request.TurnID) != "" {
		return nil
	}
	ops, err := d.repository.Operations(stream)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(ops, func(o trace.OperationRecord) bool {
		return o.Operation.ID == trace.OperationID(d.repository.Project(), stream, event)
	})
	if i < 0 {
		return fmt.Errorf("%s has no recorded operation", in.about())
	}
	requested, err := decodeShed(ops[i].Operation, in.action())
	if err != nil {
		return err
	}
	in.Spec, in.Plan = requested.Spec, requested.Plan
	for _, t := range transitions {
		if t.Subject == shedSubject && strings.HasPrefix(t.ID, id+"-waiting-") {
			in.Resume++
		}
	}
	body := fmt.Sprintf("Architect's reply to committee round %d, resumed", n)
	if in.Redraft {
		body = fmt.Sprintf("Architect's redraft after round %d, resumed", n)
	}
	return d.requestAnswer(ctx, stream, state, in, park.ID, fmt.Sprintf("%s resumes: the architect's question is answered", in.about()), body)
}

// replyOutcome returns the recorded terminal result of the operation that
// runs one architect turn of the shed: succeeded once its record is
// committed, failed when its failed transition is recorded, waiting when the
// operation parked it on the architect's question, nil before any of them.
func (d *debate) replyOutcome(stream config.WorkstreamID, in roundInput, operation string) (*coreadapter.OperationResult, error) {
	transitions, err := trace.Read[trace.Transition](d.repository, stream)
	if err != nil {
		return nil, err
	}
	id, _ := in.ids()
	for _, t := range transitions {
		switch {
		case t.Subject != shedSubject:
		case t.ID == id+"-"+in.recorded():
			return &coreadapter.OperationResult{Outcome: "succeeded", Evidence: t.Reason}, nil
		case t.ID == id+"-failed":
			return &coreadapter.OperationResult{Outcome: "failed", Evidence: t.Reason}, nil
		case strings.HasPrefix(t.ID, id+"-waiting-") && t.Cause == operation:
			return &coreadapter.OperationResult{Outcome: questions.Waiting, Evidence: t.Reason}, nil
		}
	}
	return nil, nil
}

func (r replier) decode(op coreadapter.Operation) (roundInput, config.WorkstreamID, error) {
	in, err := decodeShed(op, op.Action)
	if err != nil {
		return in, "", err
	}
	if op.Action != ReplyAction && op.Action != RedraftAction {
		return in, "", fmt.Errorf("unsupported runner operation %q", op.Action)
	}
	in.Redraft = op.Action == RedraftAction
	_, event := in.runs()
	stream, err := r.owner(op, event)
	return in, stream, err
}

// Inspect reads the recorded transitions and the architect thread. A recorded
// outcome completes the operation; a turn this session is running is unknown;
// everything else is absent, and Apply decides between running, retrying and
// recording.
func (r replier) Inspect(_ context.Context, op coreadapter.Operation) (coreadapter.Observation, error) {
	in, stream, err := r.decode(op)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	result, err := r.replyOutcome(stream, in, op.ID)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	if result != nil {
		return coreadapter.Observation{State: coreadapter.EffectCompleted, Evidence: in.answering() + " " + result.Outcome, Result: result}, nil
	}
	t, err := r.repository.Thread(stream, architectAgent)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	asked, err := r.repository.Questions(stream)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	if turns := chainTurns(t, in.chain(t, asked)); len(turns) > 0 {
		if last := turns[len(turns)-1]; last.Claim != nil && last.Response == nil && t.Status != "interrupted" {
			return coreadapter.Observation{State: coreadapter.EffectUnknown, Evidence: "architect turn " + last.Request.TurnID + " is running"}, nil
		}
	}
	return coreadapter.Observation{State: coreadapter.EffectAbsent, Evidence: in.about() + " is not recorded"}, nil
}

// Apply drives the architect's reply to a round, or the redraft the owner
// asked for after one, to a terminal result. The architect gets one turn; a
// turn that ends normally with an invalid redraft is followed by another that
// returns the problems, while redrafts remain, and an invalid redraft is never
// recorded. The record and a valid redraft are recorded in one commit and the
// shed moves to replied-<n> or redrafted-<n>. A turn that asked a question,
// whatever it ended with, parks the answer instead: the shed moves to
// asked-<n>, the operation ends waiting, and the controller requests it again
// once the answer is queued; the turn that delivers the answer continues the
// attempt that asked. A failed turn and a redraft given up are recorded, so
// the debate goes on without them. Abandoning the
// workstream cancels the running turn and fails an answer that has no record;
// one whose file is committed is recorded all the same. Storage errors and a
// missing architect runner leave the operation pending for another attempt.
func (r replier) Apply(ctx context.Context, op coreadapter.Operation) (coreadapter.OperationResult, error) {
	d := r.debate
	none := coreadapter.OperationResult{}
	in, stream, err := r.decode(op)
	if err != nil {
		return none, err
	}
	if result, err := d.replyOutcome(stream, in, op.ID); err != nil || result != nil {
		if err != nil {
			return none, err
		}
		return *result, nil
	}
	cfg := d.s.current()
	if !cfg.HasProject() || cfg.Project.ID != d.repository.Project() {
		return none, errors.New("the project is not active")
	}
	n := in.Round
	// Abandoning the workstream cancels the running turn, not the recording
	// of what it left.
	running, cancel := context.WithCancel(ctx)
	defer cancel()
	defer d.s.turns.add(stream, cancel)()
	for {
		if err := ctx.Err(); err != nil {
			return none, err
		}
		// An answer whose file is committed was given, whatever happened to
		// the workstream since.
		if recorded, err := d.recordedReply(stream, in); err != nil || recorded != nil {
			if err != nil {
				return none, err
			}
			return d.replied(ctx, op.ID, stream, in, *recorded)
		}
		t, err := d.repository.Thread(stream, architectAgent)
		if err != nil {
			return none, err
		}
		asked, err := d.repository.Questions(stream)
		if err != nil {
			return none, err
		}
		origins := in.chain(t, asked)
		chain := chainTurns(t, origins)
		// One turn per attempt, named after the attempt: the answer to a
		// question continues the attempt that asked it.
		turns := attempts(chain, origins)
		// Turns run in sequence, so the oldest unfinished turn of the chain is
		// the one to drive.
		var last, pending *trace.QueuedTurn
		if len(chain) > 0 {
			last = &chain[len(chain)-1]
		}
		if i := slices.IndexFunc(chain, func(q trace.QueuedTurn) bool { return q.CompletedAt.IsZero() }); i >= 0 {
			pending = &chain[i]
		}
		reply := shed.Reply{Version: shed.Version, Round: n, Revision: in.pin()}
		switch {
		case pending != nil && pending.Claim != nil && pending.Response == nil:
			if t.Status != "interrupted" {
				return none, errors.New("architect turn " + pending.Request.TurnID + " is still running")
			}
			if err := d.repository.AbandonTurn(ctx, stream, architectAgent, pending.Request.TurnID, d.s.now()); err != nil {
				return none, err
			}
		case pending != nil:
			gone, err := abandoned(d.repository, stream)
			if err != nil {
				return none, err
			}
			if gone && pending.Response == nil {
				if _, err := d.repository.CancelTurns(ctx, stream, d.s.now(), abandonActor, cancelReason); err != nil {
					return none, err
				}
				continue
			}
			// Completing a captured turn runs no architect.
			turnCtx := running
			if pending.Response != nil {
				turnCtx = ctx
			} else if d.s.options.Architect == nil {
				return none, errNoArchitect
			}
			if _, err := d.dispatchReply(turnCtx, stream, in, pending.Request.TurnID); err != nil {
				return none, err
			}
			if err := os.RemoveAll(filepath.Join(d.drafter().turnDirectory(stream, pending.Request.TurnID), "workspace")); err != nil {
				return none, err
			}
		case last != nil && askedBy(asked, architectThread, last.Request.TurnID) != "":
			if gone, err := abandoned(d.repository, stream); err != nil || gone {
				if err != nil {
					return none, err
				}
				return d.replyFailed(ctx, op.ID, stream, in, abandonedReply(in))
			}
			id := askedBy(asked, architectThread, last.Request.TurnID)
			return d.endReply(ctx, op.ID, stream, in, questions.Waiting, fmt.Sprintf("%s is parked: the architect waits for the answer to question %s", in.about(), id))
		case last == nil || last.Status() == "interrupted":
			if gone, err := abandoned(d.repository, stream); err != nil || gone {
				if err != nil {
					return none, err
				}
				return d.replyFailed(ctx, op.ID, stream, in, abandonedReply(in))
			}
			interrupted := len(slices.DeleteFunc(slices.Clone(turns), func(q trace.QueuedTurn) bool { return q.Status() != "interrupted" }))
			if interrupted >= maxReplyAttempts {
				reply.Failure = fmt.Sprintf("the architect's turn was interrupted %d times by service stops", interrupted)
				return d.recordReply(ctx, op.ID, stream, in, chain, reply, nil)
			}
			if d.s.options.Architect == nil {
				return none, errNoArchitect
			}
			if err := d.enqueueReply(ctx, cfg, stream, in, turns, op.ID, received(t, origins, asked)); err != nil {
				return none, err
			}
		case last.Status() == "idle":
			files, problems, err := d.redraft(stream, origins[last.Request.TurnID])
			if err != nil {
				return none, err
			}
			if len(problems) == 0 {
				return d.recordReply(ctx, op.ID, stream, in, chain, reply, files)
			}
			if redrafts(turns) >= maxRedrafts {
				reply.Problems = problems
				return d.recordReply(ctx, op.ID, stream, in, chain, reply, nil)
			}
			if d.s.options.Architect == nil {
				return none, errNoArchitect
			}
			if err := d.enqueueReply(ctx, cfg, stream, in, turns, op.ID, received(t, origins, asked)); err != nil {
				return none, err
			}
		default:
			reply.Failure = "the turn ended with status " + last.Status()
			if last.Response != nil && last.Response.Failure != "" {
				reply.Failure = last.Response.Failure
			}
			return d.recordReply(ctx, op.ID, stream, in, chain, reply, nil)
		}
	}
}

// redrafts counts the attempts of a reply that ended normally: each
// delivered what the architect wanted to, so each one after the first
// followed an invalid redraft.
func redrafts(turns []trace.QueuedTurn) int {
	return len(slices.DeleteFunc(slices.Clone(turns), func(q trace.QueuedTurn) bool { return q.CompletedAt.IsZero() || q.Status() != "idle" }))
}

// recordedReply returns the architect's recorded answer of the operation, nil
// when there is none.
func (d *debate) recordedReply(stream config.WorkstreamID, in roundInput) (*shed.Reply, error) {
	read := shed.Replies
	if in.Redraft {
		read = shed.Redrafted
	}
	replies, err := read(d.repository, stream)
	if err != nil {
		return nil, err
	}
	for _, r := range replies {
		if r.Round == in.Round {
			return &r, nil
		}
	}
	return nil, nil
}

// redraft reads the spec.md and plan.json the attempt delivered and validates
// them with the latest recorded revision of whichever was not delivered. It
// returns the delivered files that differ from the latest revision, none when
// the architect left the revision as it is, and every problem of an invalid
// redraft.
func (d *debate) redraft(stream config.WorkstreamID, attempt string) (map[string]string, []string, error) {
	dr := d.drafter()
	latest, err := dr.latest(stream)
	if err != nil {
		return nil, nil, err
	}
	specDoc, planDoc := latest[plan.SpecDocument], latest[plan.PlanDocument]
	files := map[string]string{}
	var problems []string
	for _, doc := range []struct {
		path   string
		latest *trace.Document
	}{{plan.SpecPath, &specDoc}, {plan.PlanPath, &planDoc}} {
		data, err := os.ReadFile(filepath.Join(dr.turnDirectory(stream, attempt), "output", doc.path))
		switch {
		case errors.Is(err, fs.ErrNotExist):
		case err != nil:
			return nil, nil, err
		case !utf8.Valid(data):
			problems = append(problems, doc.path+" is not UTF-8 text")
		case string(data) != doc.latest.Content:
			files[doc.path], doc.latest.Content = string(data), string(data)
		}
	}
	if len(problems) == 0 && len(files) > 0 {
		if problems, err = dr.validate(specDoc, planDoc); err != nil {
			return nil, nil, err
		}
	}
	return files, problems, nil
}

// answers merges what the reply's turns answered, a later answer replacing an
// earlier one to the same objection, and names the last turn that ended. An
// interrupted turn contributes nothing.
func (d *debate) answers(stream config.WorkstreamID, turns []trace.QueuedTurn) ([]shed.Answer, string, error) {
	var answers []shed.Answer
	turn := ""
	for _, q := range turns {
		if q.CompletedAt.IsZero() || q.Status() == "interrupted" {
			continue
		}
		turn = q.Request.TurnID
		data, err := os.ReadFile(filepath.Join(d.drafter().turnDirectory(stream, turn), "output", replyFile))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, "", err
		}
		kept, err := shed.ParseReply(data)
		if err != nil {
			return nil, "", err
		}
		if kept.Turn != turn {
			return nil, "", fmt.Errorf("answers of turn %s belong to another turn", turn)
		}
		for _, a := range kept.Answers {
			answers = append(slices.DeleteFunc(answers, func(old shed.Answer) bool { return old.Objection == a.Objection }), a)
		}
	}
	return answers, turn, nil
}

// recordReply commits the architect's answer as shed/round-<n>/reply.json or,
// for a redraft the owner asked for, shed/round-<n>/redrafted.json, and the
// files of a valid redraft as revisions of spec.md and plan.json, all authored
// by the architect and caused by the operation, in one commit, then moves the
// shed to replied-<n> or redrafted-<n>.
func (d *debate) recordReply(ctx context.Context, operation string, stream config.WorkstreamID, in roundInput, turns []trace.QueuedTurn, reply shed.Reply, files map[string]string) (coreadapter.OperationResult, error) {
	// An answer that is not committed yet records nothing for a workstream the
	// owner abandoned while the turn ran.
	if gone, err := abandoned(d.repository, stream); err != nil || gone {
		if err != nil {
			return coreadapter.OperationResult{}, err
		}
		return d.replyFailed(ctx, operation, stream, in, abandonedReply(in))
	}
	var err error
	if reply.Answers, reply.Turn, err = d.answers(stream, turns); err != nil {
		return coreadapter.OperationResult{}, err
	}
	latest, err := d.drafter().latest(stream)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	at := d.s.now()
	header := func(id string, revision int) trace.Header {
		return trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: id, Revision: revision, Project: d.repository.Project(), Workstream: stream, At: at, Actor: architectActor, Cause: operation, Depth: 1}
	}
	var docs []trace.Document
	redraft := shed.Pin{Spec: latest[plan.SpecDocument].Revision, Plan: latest[plan.PlanDocument].Revision}
	if content, ok := files[plan.SpecPath]; ok {
		redraft.Spec++
		docs = append(docs, trace.Document{Header: header(plan.SpecDocument, redraft.Spec), Path: plan.SpecPath, Content: content})
	}
	if content, ok := files[plan.PlanPath]; ok {
		redraft.Plan++
		docs = append(docs, trace.Document{Header: header(plan.PlanDocument, redraft.Plan), Path: plan.PlanPath, Content: content})
	}
	if len(docs) > 0 {
		reply.Redraft = &redraft
	}
	data, err := shed.EncodeReply(reply)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	replyDoc := trace.Document{Header: header(in.documentID(), 1), Path: in.path(), Content: string(data)}
	err = d.repository.RecordDocuments(ctx, append(docs, replyDoc))
	if errors.Is(err, trace.ErrOwnerEdit) && reply.Redraft != nil {
		// The owner has edited a file the redraft changes and the edit is not
		// recorded yet. The redraft must not write over it, so it is given up
		// like an invalid one and the answer is recorded on its own.
		if reply, replyDoc, err = d.withoutRedraft(stream, in, reply, header); err != nil {
			return coreadapter.OperationResult{}, err
		}
		err = d.repository.RecordDocuments(ctx, []trace.Document{replyDoc})
	}
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	return d.replied(ctx, operation, stream, in, reply)
}

// withoutRedraft gives up a redraft the owner's own unrecorded edit stands in
// the way of: the reply keeps its answers, records why the redraft was given
// up, and the revision the round debated stays.
func (d *debate) withoutRedraft(stream config.WorkstreamID, in roundInput, reply shed.Reply, header func(string, int) trace.Header) (shed.Reply, trace.Document, error) {
	edited, err := d.repository.OwnerEdits(stream)
	if err != nil {
		return reply, trace.Document{}, err
	}
	reply.Redraft = nil
	reply.Problems = append(reply.Problems, fmt.Sprintf("the redraft is not recorded: the owner has edited %s and the edit is not recorded yet, so the redraft would write over it", strings.Join(edited, " and ")))
	data, err := shed.EncodeReply(reply)
	if err != nil {
		return reply, trace.Document{}, err
	}
	return reply, trace.Document{Header: header(in.documentID(), 1), Path: in.path(), Content: string(data)}, nil
}

// replied moves the shed to replied-<n> or redrafted-<n> for a recorded
// answer, with a reason that says what it holds.
func (d *debate) replied(ctx context.Context, operation string, stream config.WorkstreamID, in roundInput, reply shed.Reply) (coreadapter.OperationResult, error) {
	reason := fmt.Sprintf("the architect answered %d objections after round %d", len(reply.Answers), reply.Round)
	if in.Redraft {
		reason = fmt.Sprintf("the architect redrafted at the owner's request after round %d and answered %d objections", reply.Round, len(reply.Answers))
	}
	switch {
	case reply.Failure != "":
		reason += "; its turn failed: " + reply.Failure
	case reply.Redraft != nil:
		reason += " and redrafted: " + reply.Redraft.String()
	case len(reply.Problems) > 0:
		reason += fmt.Sprintf("; its redraft was given up and %s stays:\n- %s", reply.Revision, strings.Join(reply.Problems, "\n- "))
	default:
		reason += " and left " + reply.Revision.String() + " as it is"
	}
	return d.endReply(ctx, operation, stream, in, in.recorded(), reason)
}

// abandonedReply is why the architect's answer of an abandoned workstream
// failed.
func abandonedReply(in roundInput) string {
	return fmt.Sprintf("%s failed: the workstream was abandoned, so the architect's %s is not recorded", in.about(), in.answering())
}

func (d *debate) replyFailed(ctx context.Context, operation string, stream config.WorkstreamID, in roundInput, reason string) (coreadapter.OperationResult, error) {
	return d.endReply(ctx, operation, stream, in, "failed", reason)
}

// endReply ends the architect's answer with a shed transition, replied-<n>,
// redrafted-<n> or failed-<n>, or parks it at asked-<n> when the kind is
// waiting, and returns the matching result. The transition already recorded
// is returned as it is.
func (d *debate) endReply(ctx context.Context, operation string, stream config.WorkstreamID, in roundInput, kind, reason string) (coreadapter.OperationResult, error) {
	outcome := coreadapter.OperationResult{Outcome: "failed", Evidence: reason}
	id, _ := in.ids()
	id, to := id+"-"+kind, fmt.Sprintf("%s-%d", kind, in.Round)
	switch kind {
	case in.recorded():
		outcome.Outcome = "succeeded"
	case questions.Waiting:
		outcome.Outcome, id, to = questions.Waiting, in.parked(), fmt.Sprintf("asked-%d", in.Round)
	}
	state, err := d.repository.Workflow(stream, shedSubject)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	if state.Value != in.running() {
		result, err := d.replyOutcome(stream, in, operation)
		if err != nil || result == nil {
			return coreadapter.OperationResult{}, errors.Join(err, fmt.Errorf("%s is %q, not in progress", in.about(), state.Value))
		}
		return *result, nil
	}
	tx := trace.Transaction{ExpectedVersion: state.Version,
		Transition: trace.Transition{Header: d.header(id, stream, operation, d.s.now()), Subject: shedSubject, From: state.Value, To: to, Reason: reason}}
	if _, err := d.repository.Transact(ctx, tx); err != nil {
		return coreadapter.OperationResult{}, err
	}
	return outcome, nil
}

// standingAfter returns the dissent that stood once round n was heard, and
// that the owner has not disposed of: what the architect answers.
func (d *debate) standingAfter(stream config.WorkstreamID, n int) ([]shed.Entry, error) {
	records, err := d.earlier(stream, n+1)
	if err != nil {
		return nil, err
	}
	rulings, err := shed.AllRulings(d.repository, stream)
	if err != nil {
		return nil, err
	}
	return shed.Standing(shed.DissentRecord(records, rulings)), nil
}

// enqueueReply accepts the next architect turn of the reply, or of the
// redraft the owner asked for, fixing its profile and prompts. turns holds
// one turn per earlier attempt. A turn that follows an invalid redraft lists
// why it was not accepted. Every new attempt, whether it follows an
// interrupted turn or an invalid redraft, repeats the answers the architect
// received in the operation.
func (d *debate) enqueueReply(ctx context.Context, cfg *config.Config, stream config.WorkstreamID, in roundInput, turns []trace.QueuedTurn, operation string, answers []string) error {
	profile, _, err := d.s.roleExecution(cfg, architectRole)
	if err != nil {
		return err
	}
	open, err := d.standingAfter(stream, in.Round)
	if err != nil {
		return err
	}
	latest, err := latestPin(d.repository, stream)
	if err != nil {
		return err
	}
	var problems []string
	if previous := d.returned(turns); previous != "" {
		if _, problems, err = d.redraft(stream, previous); err != nil {
			return err
		}
	}
	prompt := replyPrompt(in, latest, open, problems)
	if in.Redraft {
		asked, err := d.asked(stream, in.Round)
		if err != nil {
			return err
		}
		prompt = redraftPrompt(in, latest, open, problems, asked.Note)
	}
	if len(answers) > 0 {
		prompt += "\n" + answered(in.answering(), answers)
	}
	turn := in.turnID(len(turns) + 1)
	req := trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_" + turn, Revision: 1, Project: d.repository.Project(), Workstream: stream, At: d.s.now(), Actor: shedActor, Cause: operation, Depth: 1},
		AgentID: architectAgent, ThreadID: architectThread, TurnID: turn, Profile: profile, SystemPrompt: architectSystemPrompt(cfg.Project), Prompt: prompt}
	_, err = d.repository.EnqueueTurn(ctx, req)
	return err
}

// returned names the reply's latest attempt that ended normally: the one
// whose redraft the next turn is returned, empty when no attempt has.
func (d *debate) returned(turns []trace.QueuedTurn) string {
	for i := len(turns) - 1; i >= 0; i-- {
		if !turns[i].CompletedAt.IsZero() && turns[i].Status() == "idle" {
			return turns[i].Request.TurnID
		}
	}
	return ""
}

// dispatchReply runs the turn through the thread dispatcher and runner.
func (d *debate) dispatchReply(ctx context.Context, stream config.WorkstreamID, in roundInput, turn string) (coreadapter.OperationResult, error) {
	dr := d.drafter()
	dispatcher := thread.Dispatcher{Runner: threadRunner(d.s.current(), d.repository, &questions.Turns{Turns: d.replyPath(stream, in), Repository: d.repository}, d.s.now), Prepare: func(_ context.Context, input thread.TurnInput) (coreadapter.PreparedTurn, error) {
		directory := filepath.Join(dr.turnDirectory(input.Workstream, input.Turn), "session")
		return coreadapter.PreparedTurn{SessionDirectory: directory}, os.MkdirAll(directory, 0700)
	}}
	op, err := thread.TurnOperation(d.repository.Project(), "reply-turn", thread.TurnInput{Workstream: stream, Agent: architectAgent, Turn: turn})
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	return dispatcher.Apply(ctx, op)
}

// replyPath is the architect's isolated turn path for a reply: a read-only
// private view of the latest spec and plan, the handed input, the charter,
// the context bundle and the shed's records, the file reading tool, the reply
// tool, the draft delivery tool and the question tool, and no notes, write,
// execute, network or VCS capability. A turn that asks ends waiting, so its
// thread parks and its slot is free while the answer is found.
func (d *debate) replyPath(stream config.WorkstreamID, in roundInput) *isolation.Turns {
	var engine coreadapter.Engine
	var hosts coreadapter.MCPHosts
	if a := d.s.options.Architect; a != nil {
		engine, hosts = a.Engine, a.Hosts
	}
	denied := errors.New("turn scope denied")
	return &isolation.Turns{
		Workspaces: stagedWorkspaces{},
		Views:      isolation.Views{Directory: filepath.Join(d.s.current().Root.String(), "views")},
		Select: func(ctx context.Context, scope coreadapter.Scope) (isolation.Selection, error) {
			return d.selectReplyView(ctx, scope, in)
		},
		Grants: map[string]coreadapter.Capabilities{architectRole: {Tools: []string{"file_read", shed.ReplyTool, DraftTool, questions.AskTool}}},
		Scoped: func(_ context.Context, scope coreadapter.Scope) ([]coreadapter.Tool, error) {
			if scope.Workstream != string(stream) || scope.Role != architectRole || scope.Project != string(d.repository.Project()) || scope.Thread != architectThread {
				return nil, denied
			}
			// The turn that delivers the answer to the architect's question
			// continues the attempt that asked it.
			attempt, err := d.drafter().continued(stream, scope.Turn)
			if err != nil {
				return nil, err
			}
			if !strings.HasPrefix(attempt, in.turnPrefix()) {
				return nil, denied
			}
			open, err := d.standingAfter(stream, in.Round)
			if err != nil {
				return nil, err
			}
			standing := make([]shed.Dissent, len(open))
			for i, e := range open {
				standing[i] = e.Dissent
			}
			file := filepath.Join(d.drafter().turnDirectory(stream, scope.Turn), "output", replyFile)
			tools, err := shed.ReplyTools(shed.ReplyTurn{Reply: shed.Reply{Round: in.Round, Revision: in.pin(), Turn: scope.Turn}, Standing: standing,
				Save: func(r shed.Reply) error {
					data, err := shed.EncodeReply(r)
					if err != nil {
						return err
					}
					if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
						return err
					}
					// A rename keeps the previous answers whole if the write stops.
					if err := os.WriteFile(file+".tmp", data, 0600); err != nil {
						return err
					}
					return os.Rename(file+".tmp", file)
				}})
			if err != nil {
				return nil, err
			}
			ask, err := questions.Tools(d.repository, architectAgent, scope, d.s.now)
			if err != nil {
				return nil, err
			}
			return append(append(tools, d.drafter().draftTool(scope, attempt)), ask...), nil
		},
		Hosts:  hosts,
		Engine: engine,
	}
}

// selectReplyView stages the architect's view for the claimed turn and
// selects all of it, read-only.
func (d *debate) selectReplyView(ctx context.Context, scope coreadapter.Scope, in roundInput) (isolation.Selection, error) {
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
	workspace := filepath.Join(d.drafter().turnDirectory(stream, scope.Turn), "workspace")
	paths, err := d.stageReply(ctx, stream, in, scope.Turn, workspace)
	if err != nil {
		return isolation.Selection{}, err
	}
	return isolation.Selection{Workspace: coreadapter.WorkspaceRequest{SourceDirectory: workspace, Directory: workspace}, Paths: paths, Execution: settings}, nil
}

// stageReply builds the view: the latest spec.md and plan.json, the handed
// input under handed/, charter.md, the rendered context bundle as context.md,
// the shed's records under shed/ and, after an invalid redraft, what the
// architect delivered under redraft/. It returns the paths to select.
func (d *debate) stageReply(ctx context.Context, stream config.WorkstreamID, in roundInput, turn, workspace string) ([]string, error) {
	if err := os.RemoveAll(workspace); err != nil {
		return nil, err
	}
	dr := d.drafter()
	latest, err := dr.latest(stream)
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
	files := map[string]string{plan.SpecPath: latest[plan.SpecDocument].Content, plan.PlanPath: latest[plan.PlanDocument].Content, "charter.md": charter.Content, "context.md": b.Render()}
	paths := []string{plan.SpecPath, plan.PlanPath, "charter.md", "context.md"}
	for _, doc := range latest {
		if directory, _, _ := strings.Cut(doc.Path, "/"); directory == "handed" || directory == "shed" {
			files[doc.Path] = doc.Content
			if !slices.Contains(paths, directory) {
				paths = append(paths, directory)
			}
		}
	}
	t, err := d.repository.Thread(stream, architectAgent)
	if err != nil {
		return nil, err
	}
	asked, err := d.repository.Questions(stream)
	if err != nil {
		return nil, err
	}
	origins := in.chain(t, asked)
	earlier := slices.DeleteFunc(attempts(chainTurns(t, origins), origins), func(q trace.QueuedTurn) bool { return q.Request.TurnID == origins[turn] })
	if previous := d.returned(earlier); previous != "" {
		delivered, _, err := d.redraft(stream, previous)
		if err != nil {
			return nil, err
		}
		for path, content := range delivered {
			files["redraft/"+path] = content
		}
		if len(delivered) > 0 {
			paths = append(paths, "redraft")
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

func replyPrompt(in roundInput, latest shed.Pin, open []shed.Entry, problems []string) string {
	var b strings.Builder
	returned, redraft := invalidRedraft(problems)
	b.WriteString(returned)
	fmt.Fprintf(&b, "Round %d of the shed is heard. The committee debated %s. %d objections stand:\n", in.Round, in.pin(), len(open))
	b.WriteString(dissentList(open))
	fmt.Fprintf(&b, `
What each kind asks of you:
- charter: a veto on the part it names. It blocks until its member concedes it after a redraft, or the owner disposes of it.
- size: split the unit by what it addresses. It blocks until its member concedes it.
- proof: name a proof that can show the criterion. It blocks until its member concedes it.
- fit: advice to the owner. It never blocks; answer it.
- owner: the owner's own objection. It blocks until the owner disposes of it; answer it as you would a member's.

%s
This is your one reply to this round. Answer each objection with %s, giving its ID and your answer. When the objections call for a change, also deliver the changed spec.md, plan.json or both with %s: the redraft is validated by the rules your draft was, and a valid one is the revision the committee debates next. An invalid one comes back to you. Deliver nothing to leave the revision as it is.`, view(latest, redraft), shed.ReplyTool, DraftTool)
	return b.String()
}

// redraftPrompt is the architect's turn for the redraft the owner asked for
// after debate concluded: the owner's note is what to change, and the dissent
// that stands is what the committee will read the redraft against.
func redraftPrompt(in roundInput, latest shed.Pin, open []shed.Entry, problems []string, note string) string {
	var b strings.Builder
	returned, redraft := invalidRedraft(problems)
	b.WriteString(returned)
	fmt.Fprintf(&b, "Debate concluded after round %d of the shed, and the owner read the packet and asked you to redraft %s. The owner's note:\n\n%s\n\n", in.Round, in.pin(), note)
	fmt.Fprintf(&b, "%d objections stand:\n", len(open))
	b.WriteString(dissentList(open))
	fmt.Fprintf(&b, `
%s
Deliver the redrafted spec.md, plan.json or both with %s: the redraft is validated by the rules your draft was, and a valid one is the revision the committee debates in the round that follows. An invalid one comes back to you. Answer with %s an objection your redraft settles, giving its ID and your answer. Deliver nothing to leave the revision as it is; the owner's note is then unanswered.`, view(latest, redraft), DraftTool, shed.ReplyTool)
	return b.String()
}

// invalidRedraft is what the turn is told about a redraft that was not
// accepted, and the line naming where the turn can read it.
func invalidRedraft(problems []string) (returned, view string) {
	if len(problems) == 0 {
		return "", ""
	}
	return fmt.Sprintf("Your redraft was not accepted, and the committee will not read it:\n- %s\n\nWhat you delivered is in redraft/. Deliver every file of the redraft again with %s, corrected: only what this turn delivers counts. Deliver nothing to leave the revision as it is. Your answers so far are kept; answer an objection again only to replace what you said.\n\n", strings.Join(problems, "\n- "), DraftTool),
		"- redraft/: the files of your redraft that was not accepted.\n"
}

// view describes what the architect's staged view holds.
func view(latest shed.Pin, redraft string) string {
	return fmt.Sprintf(`Your view holds:
- spec.md and plan.json: the latest recorded revision, %s.
- handed/, charter.md and context.md: what you drafted from.
- shed/round-<n>/: what every member contributed to each round, your earlier replies as reply.json, and the owner's own files of the round.
%s`, latest, redraft)
}

// dissentList is one line per objection that stands, as the architect reads
// them.
func dissentList(open []shed.Entry) string {
	var b strings.Builder
	for _, e := range open {
		weight := "advisory"
		if e.Blocking {
			weight = "blocking"
		}
		// The owner's own objection names no part and cites nothing.
		about := " on " + e.Part
		if e.Part == "" {
			about = ""
		}
		citing := ", citing " + strings.Join(e.Citations, ", ")
		if len(e.Citations) == 0 {
			citing = ""
		}
		fmt.Fprintf(&b, "- %s (%s, %s, by %s in round %d%s%s): %s\n", e.ID, e.Kind, weight, e.Member, e.Round, about, citing, e.Argument)
	}
	return b.String()
}

// asked returns the owner's request for the redraft after round n.
func (d *debate) asked(stream config.WorkstreamID, n int) (shed.Redraft, error) {
	redrafts, err := shed.Redrafts(d.repository, stream)
	if err != nil {
		return shed.Redraft{}, err
	}
	for _, r := range redrafts {
		if r.Round == n {
			return r, nil
		}
	}
	return shed.Redraft{}, fmt.Errorf("no redraft is asked for after round %d", n)
}
