package intake

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
	MaxRedrafts      = MaxDrafts
	MaxReplyAttempts = MaxDraftAttempts
	ReplyFile        = "reply.json"
)

// answering names what the architect's turn answers: one round of the
// committee, or the owner's request for a redraft after it.
func (in RoundInput) Answering() string {
	if in.Redraft {
		return "redraft"
	}
	return "reply"
}

// action is the operation action that runs the turn.
func (in RoundInput) Action() string {
	if in.Redraft {
		return RedraftAction
	}
	return ReplyAction
}

// running is the shed state while the turn runs, and recorded the kind of the
// state it reaches once its record is committed.
func (in RoundInput) Running() string { return fmt.Sprintf("%s-%d", in.Answering(), in.Round) }

func (in RoundInput) Recorded() string {
	if in.Redraft {
		return "redrafted"
	}
	return "replied"
}

// ids are the transition and run-event IDs of the turn's operation.
func (in RoundInput) Ids() (transition, event string) {
	transition = fmt.Sprintf("shed-%s-%d", in.Answering(), in.Round)
	return transition, trace.EventID(transition, "run")
}

// path and documentID are where the architect's record of the turn goes.
func (in RoundInput) Path() string {
	if in.Redraft {
		return shed.RedraftedPath(in.Round)
	}
	return shed.ReplyPath(in.Round)
}

func (in RoundInput) DocumentID() string {
	if in.Redraft {
		return shed.RedraftedDocumentID(in.Round)
	}
	return shed.ReplyDocumentID(in.Round)
}

// about names the architect's answer for a message: its reply is to the round,
// and the redraft the owner asked for comes after it.
func (in RoundInput) About() string {
	if in.Redraft {
		return fmt.Sprintf("the redraft after round %d", in.Round)
	}
	return fmt.Sprintf("the reply to round %d", in.Round)
}

// turnPrefix is what every turn of the operation is named with, and turnID
// the name of its k-th turn.
func (in RoundInput) TurnPrefix() string { return fmt.Sprintf("%s-%d-", in.Answering(), in.Round) }

func (in RoundInput) TurnID(k int) string {
	return in.TurnPrefix() + strconv.Itoa(k)
}

// runs returns the transition and event of the operation that runs the
// turn: its own, or the resumption the input numbers.
func (in RoundInput) Runs() (transition, event string) {
	if in.Resume == 0 {
		return in.Ids()
	}
	id, _ := in.Ids()
	transition = fmt.Sprintf("%s-resume-%d", id, in.Resume)
	return transition, trace.EventID(transition, "run")
}

// parked is the transition that parks the architect's answer the operation
// runs: the k-th park follows the k-1-th resumption.
func (in RoundInput) Parked() string {
	id, _ := in.Ids()
	return fmt.Sprintf("%s-waiting-%d", id, in.Resume+1)
}

// chain maps the operation's turns of the architect thread to the attempt
// each continues.
func (in RoundInput) Chain(t trace.Thread, asked []trace.QuestionState) map[string]string {
	return AskChain(t, in.TurnPrefix(), asked)
}

func ReplyIDs(n int) (transition, event string) { return RoundInput{Round: n}.Ids() }

func ReplyTurnID(n, k int) string { return RoundInput{Round: n}.TurnID(k) }

// replier reconciles the architect's reply operations of the shed controller.
type Replier struct{ *Debate }

var _ coreadapter.Reconciler = Replier{}

func (d *Debate) Drafter() *Drafter {
	return &Drafter{S: d.S.DrafterDependencies(), Repository: d.Repository}
}

// requestReply publishes the architect's reply to a heard round as a durable
// operation pinned to the revision the round debated.
func (d *Debate) RequestReply(ctx context.Context, stream config.WorkstreamID, state trace.WorkflowState, in RoundInput, open int) error {
	round, _ := RoundIDs(in.Round)
	reason := fmt.Sprintf("%d objections stand after round %d; the architect is asked for its reply", open, in.Round)
	return d.RequestAnswer(ctx, stream, state, in, round+"-heard", reason, fmt.Sprintf("Architect's reply to committee round %d", in.Round))
}

// requestRedraft publishes the redraft the owner asked for after round n as a
// durable operation pinned to the latest recorded revisions, which are what
// the architect redrafts and what the owner may have edited since the request.
func (d *Debate) RequestRedraft(ctx context.Context, stream config.WorkstreamID, state trace.WorkflowState, in RoundInput, note string) error {
	reason := fmt.Sprintf("the owner asked for a redraft after round %d: %s", in.Round, note)
	return d.RequestAnswer(ctx, stream, state, in, shed.RedraftDocumentID(in.Round), reason, fmt.Sprintf("Architect's redraft after round %d, at the owner's request", in.Round))
}

// requestAnswer publishes one architect turn of the shed as a durable
// operation and moves the shed to the state that says it is running.
func (d *Debate) RequestAnswer(ctx context.Context, stream config.WorkstreamID, state trace.WorkflowState, in RoundInput, cause, reason, body string) error {
	// A paused workstream is asked for nothing until the pause is lifted.
	if _, paused := d.S.Pausing(d.Repository.Project(), stream); paused {
		return nil
	}
	if err := d.Drafter().EnsureThread(ctx, stream); err != nil {
		return err
	}
	transition, event := in.Runs()
	input, err := EncodeRound(in)
	if err != nil {
		return err
	}
	op := coreadapter.Operation{ID: trace.OperationID(d.Repository.Project(), stream, event), Boundary: coreadapter.RunnerBoundary, Action: in.Action(), Input: input}
	tx := trace.Transaction{ExpectedVersion: state.Version,
		Transition: trace.Transition{Header: d.Header(transition, stream, cause, d.S.Now()), Subject: ShedSubject, From: state.Value, To: in.Running(), Reason: reason},
		Events:     []trace.Event{{ID: event, Kind: in.Action(), Body: body, Operation: &op}}}
	_, err = d.Repository.Transact(ctx, tx)
	return err
}

// resumeAnswer requests the architect's parked reply or redraft after round
// n again once the answer to its question is queued on the architect's
// thread. The resumption is pinned to the revision the parked operation was,
// and caused by the transition that parked it.
func (d *Debate) ResumeAnswer(ctx context.Context, stream config.WorkstreamID, state trace.WorkflowState, n int) error {
	transitions, err := trace.Read[trace.Transition](d.Repository, stream)
	if err != nil {
		return err
	}
	var park trace.Transition
	for _, t := range transitions {
		if t.Subject == ShedSubject {
			park = t
		}
	}
	in := RoundInput{Round: n, Redraft: strings.HasPrefix(park.ID, "shed-redraft-")}
	id, event := in.Ids()
	if !strings.HasPrefix(park.ID, id+"-waiting-") {
		return fmt.Errorf("the shed is %s without a parking transition", state.Value)
	}
	t, err := d.Repository.Thread(stream, ArchitectAgent)
	if err != nil {
		return err
	}
	asked, err := d.Repository.Questions(stream)
	if err != nil {
		return err
	}
	if turns := ChainTurns(t, in.Chain(t, asked)); len(turns) > 0 && AskedBy(asked, ArchitectThread, turns[len(turns)-1].Request.TurnID) != "" {
		return nil
	}
	ops, err := d.Repository.Operations(stream)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(ops, func(o trace.OperationRecord) bool {
		return o.Operation.ID == trace.OperationID(d.Repository.Project(), stream, event)
	})
	if i < 0 {
		return fmt.Errorf("%s has no recorded operation", in.About())
	}
	requested, err := DecodeShed(ops[i].Operation, in.Action())
	if err != nil {
		return err
	}
	in.Spec, in.Plan = requested.Spec, requested.Plan
	for _, t := range transitions {
		if t.Subject == ShedSubject && strings.HasPrefix(t.ID, id+"-waiting-") {
			in.Resume++
		}
	}
	body := fmt.Sprintf("Architect's reply to committee round %d, resumed", n)
	if in.Redraft {
		body = fmt.Sprintf("Architect's redraft after round %d, resumed", n)
	}
	return d.RequestAnswer(ctx, stream, state, in, park.ID, fmt.Sprintf("%s resumes: the architect's question is answered", in.About()), body)
}

// replyOutcome returns the recorded terminal result of the operation that
// runs one architect turn of the shed: succeeded once its record is
// committed, failed when its failed transition is recorded, waiting when the
// operation parked it on the architect's question, nil before any of them.
func (d *Debate) ReplyOutcome(stream config.WorkstreamID, in RoundInput, operation string) (*coreadapter.OperationResult, error) {
	transitions, err := trace.Read[trace.Transition](d.Repository, stream)
	if err != nil {
		return nil, err
	}
	id, _ := in.Ids()
	for _, t := range transitions {
		switch {
		case t.Subject != ShedSubject:
		case t.ID == id+"-"+in.Recorded():
			return &coreadapter.OperationResult{Outcome: "succeeded", Evidence: t.Reason}, nil
		case t.ID == id+"-failed":
			return &coreadapter.OperationResult{Outcome: "failed", Evidence: t.Reason}, nil
		case strings.HasPrefix(t.ID, id+"-waiting-") && t.Cause == operation:
			return &coreadapter.OperationResult{Outcome: questions.Waiting, Evidence: t.Reason}, nil
		}
	}
	return nil, nil
}

func (r Replier) Decode(op coreadapter.Operation) (RoundInput, config.WorkstreamID, error) {
	in, err := DecodeShed(op, op.Action)
	if err != nil {
		return in, "", err
	}
	if op.Action != ReplyAction && op.Action != RedraftAction {
		return in, "", fmt.Errorf("unsupported runner operation %q", op.Action)
	}
	in.Redraft = op.Action == RedraftAction
	_, event := in.Runs()
	stream, err := r.Owner(op, event)
	return in, stream, err
}

// Inspect reads the recorded transitions and the architect thread. A recorded
// outcome completes the operation; a turn this session is running is unknown;
// everything else is absent, and Apply decides between running, retrying and
// recording.
func (r Replier) Inspect(_ context.Context, op coreadapter.Operation) (coreadapter.Observation, error) {
	in, stream, err := r.Decode(op)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	result, err := r.ReplyOutcome(stream, in, op.ID)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	if result != nil {
		return coreadapter.Observation{State: coreadapter.EffectCompleted, Evidence: in.Answering() + " " + result.Outcome, Result: result}, nil
	}
	t, err := r.Repository.Thread(stream, ArchitectAgent)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	asked, err := r.Repository.Questions(stream)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	if turns := ChainTurns(t, in.Chain(t, asked)); len(turns) > 0 {
		if last := turns[len(turns)-1]; last.Claim != nil && last.Response == nil && t.Status != "interrupted" {
			return coreadapter.Observation{State: coreadapter.EffectUnknown, Evidence: "architect turn " + last.Request.TurnID + " is running"}, nil
		}
	}
	return coreadapter.Observation{State: coreadapter.EffectAbsent, Evidence: in.About() + " is not recorded"}, nil
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
func (r Replier) Apply(ctx context.Context, op coreadapter.Operation) (coreadapter.OperationResult, error) {
	d := r.Debate
	none := coreadapter.OperationResult{}
	in, stream, err := r.Decode(op)
	if err != nil {
		return none, err
	}
	if result, err := d.ReplyOutcome(stream, in, op.ID); err != nil || result != nil {
		if err != nil {
			return none, err
		}
		return *result, nil
	}
	cfg := d.S.About(d.Repository)
	if !cfg.HasProject() || cfg.Project.ID != d.Repository.Project() {
		return none, errors.New("the project is not active")
	}
	n := in.Round
	// Abandoning the workstream cancels the running turn, not the recording
	// of what it left.
	running, cancel := context.WithCancel(ctx)
	defer cancel()
	defer d.S.TrackTurn(stream, cancel)()
	for {
		if err := ctx.Err(); err != nil {
			return none, err
		}
		// An answer whose file is committed was given, whatever happened to
		// the workstream since.
		if recorded, err := d.RecordedReply(stream, in); err != nil || recorded != nil {
			if err != nil {
				return none, err
			}
			return d.Replied(ctx, op.ID, stream, in, *recorded)
		}
		t, err := d.Repository.Thread(stream, ArchitectAgent)
		if err != nil {
			return none, err
		}
		asked, err := d.Repository.Questions(stream)
		if err != nil {
			return none, err
		}
		origins := in.Chain(t, asked)
		chain := ChainTurns(t, origins)
		// One turn per attempt, named after the attempt: the answer to a
		// question continues the attempt that asked it.
		turns := Attempts(chain, origins)
		// Turns run in sequence, so the oldest unfinished turn of the chain is
		// the one to drive.
		var last, pending *trace.QueuedTurn
		if len(chain) > 0 {
			last = &chain[len(chain)-1]
		}
		if i := slices.IndexFunc(chain, func(q trace.QueuedTurn) bool { return q.CompletedAt.IsZero() }); i >= 0 {
			pending = &chain[i]
		}
		reply := shed.Reply{Version: shed.Version, Round: n, Revision: in.Pin()}
		switch {
		case pending != nil && pending.Claim != nil && pending.Response == nil:
			if t.Status != "interrupted" {
				return none, errors.New("architect turn " + pending.Request.TurnID + " is still running")
			}
			if err := d.Repository.AbandonTurn(ctx, stream, ArchitectAgent, pending.Request.TurnID, d.S.Now()); err != nil {
				return none, err
			}
		case pending != nil:
			gone, err := Abandoned(d.Repository, stream)
			if err != nil {
				return none, err
			}
			if gone && pending.Response == nil {
				if _, err := d.Repository.CancelTurns(ctx, stream, d.S.Now(), AbandonActor, CancelReason); err != nil {
					return none, err
				}
				continue
			}
			// Completing a captured turn runs no architect.
			turnCtx := running
			if pending.Response != nil {
				turnCtx = ctx
			} else if d.S.Options.Architect == nil {
				return none, ErrNoArchitect
			} else if err := d.S.Held(d.Repository, stream); err != nil {
				return none, err
			}
			if _, err := d.DispatchReply(turnCtx, stream, in, pending.Request.TurnID); err != nil {
				return none, err
			}
			if err := os.RemoveAll(filepath.Join(d.Drafter().TurnDirectory(stream, pending.Request.TurnID), "workspace")); err != nil {
				return none, err
			}
		case last != nil && AskedBy(asked, ArchitectThread, last.Request.TurnID) != "":
			if gone, err := Abandoned(d.Repository, stream); err != nil || gone {
				if err != nil {
					return none, err
				}
				return d.ReplyFailed(ctx, op.ID, stream, in, AbandonedReply(in))
			}
			id := AskedBy(asked, ArchitectThread, last.Request.TurnID)
			return d.EndReply(ctx, op.ID, stream, in, questions.Waiting, fmt.Sprintf("%s is parked: the architect waits for the answer to question %s", in.About(), id))
		case last == nil || last.Status() == "interrupted":
			if gone, err := Abandoned(d.Repository, stream); err != nil || gone {
				if err != nil {
					return none, err
				}
				return d.ReplyFailed(ctx, op.ID, stream, in, AbandonedReply(in))
			}
			// A turn a hard pause stopped is continued within its attempt.
			interrupted := len(slices.DeleteFunc(slices.Clone(turns), func(q trace.QueuedTurn) bool { return q.Status() != "interrupted" }))
			if !StoppedTurn(last) && interrupted >= MaxReplyAttempts {
				reply.Failure = fmt.Sprintf("the architect's turn was interrupted %d times by service stops", interrupted)
				return d.RecordReply(ctx, op.ID, stream, in, chain, reply, nil)
			}
			if d.S.Options.Architect == nil {
				return none, ErrNoArchitect
			}
			if err := d.S.Held(d.Repository, stream); err != nil {
				return none, err
			}
			if StoppedTurn(last) {
				if _, err := d.S.ContinueStopped(ctx, d.Repository, cfg, ArchitectRole, *last); err != nil {
					return none, err
				}
				continue
			}
			if err := d.EnqueueReply(ctx, cfg, stream, in, turns, op.ID, Received(t, origins, asked)); err != nil {
				return none, err
			}
		case last.Status() == "idle":
			files, problems, err := d.Redraft(stream, origins[last.Request.TurnID])
			if err != nil {
				return none, err
			}
			if len(problems) == 0 {
				return d.RecordReply(ctx, op.ID, stream, in, chain, reply, files)
			}
			if Redrafts(turns) >= MaxRedrafts {
				reply.Problems = problems
				return d.RecordReply(ctx, op.ID, stream, in, chain, reply, nil)
			}
			if d.S.Options.Architect == nil {
				return none, ErrNoArchitect
			}
			if err := d.S.Held(d.Repository, stream); err != nil {
				return none, err
			}
			if err := d.EnqueueReply(ctx, cfg, stream, in, turns, op.ID, Received(t, origins, asked)); err != nil {
				return none, err
			}
		default:
			reply.Failure = "the turn ended with status " + last.Status()
			if last.Response != nil && last.Response.Failure != "" {
				reply.Failure = last.Response.Failure
			}
			return d.RecordReply(ctx, op.ID, stream, in, chain, reply, nil)
		}
	}
}

// redrafts counts the attempts of a reply that ended normally: each
// delivered what the architect wanted to, so each one after the first
// followed an invalid redraft.
func Redrafts(turns []trace.QueuedTurn) int {
	return len(slices.DeleteFunc(slices.Clone(turns), func(q trace.QueuedTurn) bool { return q.CompletedAt.IsZero() || q.Status() != "idle" }))
}

// recordedReply returns the architect's recorded answer of the operation, nil
// when there is none.
func (d *Debate) RecordedReply(stream config.WorkstreamID, in RoundInput) (*shed.Reply, error) {
	read := shed.Replies
	if in.Redraft {
		read = shed.Redrafted
	}
	replies, err := read(d.Repository, stream)
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
func (d *Debate) Redraft(stream config.WorkstreamID, attempt string) (map[string]string, []string, error) {
	dr := d.Drafter()
	latest, err := dr.Latest(stream)
	if err != nil {
		return nil, nil, err
	}
	specDoc, planDoc := latest[plan.SpecDocument], latest[plan.PlanDocument]
	files := map[string]string{}
	var problems []string
	for _, doc := range []struct {
		Path   string
		Latest *trace.Document
	}{{plan.SpecPath, &specDoc}, {plan.PlanPath, &planDoc}} {
		data, err := os.ReadFile(filepath.Join(dr.TurnDirectory(stream, attempt), "output", doc.Path))
		switch {
		case errors.Is(err, fs.ErrNotExist):
		case err != nil:
			return nil, nil, err
		case !utf8.Valid(data):
			problems = append(problems, doc.Path+" is not UTF-8 text")
		case string(data) != doc.Latest.Content:
			files[doc.Path], doc.Latest.Content = string(data), string(data)
		}
	}
	if len(problems) == 0 && len(files) > 0 {
		if problems, err = dr.Validate(specDoc, planDoc); err != nil {
			return nil, nil, err
		}
		problems = append(problems, plan.CheckAcceptanceProblems(specDoc.Content, planDoc.Content, "", "")...)
	}
	return files, problems, nil
}

// judge is the draftJudge of a redraft delivered with a reply.
func (d *Debate) Judge(stream config.WorkstreamID, attempt string) (string, []string, error) {
	files, problems, err := d.Redraft(stream, attempt)
	switch {
	case err != nil:
		return "", nil, err
	case len(problems) > 0:
		return DraftInvalid, problems, nil
	case len(files) == 0:
		return DraftUnchanged, nil, nil
	}
	return DraftValid, nil, nil
}

// answers merges what the reply's turns answered, a later answer replacing an
// earlier one to the same objection, and names the last turn that ended. An
// interrupted turn contributes nothing.
func (d *Debate) Answers(stream config.WorkstreamID, turns []trace.QueuedTurn) ([]shed.Answer, string, error) {
	var answers []shed.Answer
	turn := ""
	for _, q := range turns {
		// A turn a hard pause stopped kept what it answered; its continuation
		// adds to it.
		if q.CompletedAt.IsZero() || q.Status() == "interrupted" && !StoppedTurn(&q) {
			continue
		}
		turn = q.Request.TurnID
		data, err := os.ReadFile(filepath.Join(d.Drafter().TurnDirectory(stream, turn), "output", ReplyFile))
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
func (d *Debate) RecordReply(ctx context.Context, operation string, stream config.WorkstreamID, in RoundInput, turns []trace.QueuedTurn, reply shed.Reply, files map[string]string) (coreadapter.OperationResult, error) {
	// An answer that is not committed yet records nothing for a workstream the
	// owner abandoned while the turn ran.
	if gone, err := Abandoned(d.Repository, stream); err != nil || gone {
		if err != nil {
			return coreadapter.OperationResult{}, err
		}
		return d.ReplyFailed(ctx, operation, stream, in, AbandonedReply(in))
	}
	var err error
	if reply.Answers, reply.Turn, err = d.Answers(stream, turns); err != nil {
		return coreadapter.OperationResult{}, err
	}
	latest, err := d.Drafter().Latest(stream)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	at := d.S.Now()
	header := func(id string, revision int) trace.Header {
		return trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: id, Revision: revision, Project: d.Repository.Project(), Workstream: stream, At: at, Actor: ArchitectActor, Cause: operation, Depth: 1}
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
	replyDoc := trace.Document{Header: header(in.DocumentID(), 1), Path: in.Path(), Content: string(data)}
	if err := d.Repository.RecordDocuments(ctx, append(docs, replyDoc)); err != nil {
		return coreadapter.OperationResult{}, err
	}
	return d.Replied(ctx, operation, stream, in, reply)
}

// replied moves the shed to replied-<n> or redrafted-<n> for a recorded
// answer, with a reason that says what it holds.
func (d *Debate) Replied(ctx context.Context, operation string, stream config.WorkstreamID, in RoundInput, reply shed.Reply) (coreadapter.OperationResult, error) {
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
	return d.EndReply(ctx, operation, stream, in, in.Recorded(), reason)
}

// abandonedReply is why the architect's answer of an abandoned workstream
// failed.
func AbandonedReply(in RoundInput) string {
	return fmt.Sprintf("%s failed: the workstream was abandoned, so the architect's %s is not recorded", in.About(), in.Answering())
}

func (d *Debate) ReplyFailed(ctx context.Context, operation string, stream config.WorkstreamID, in RoundInput, reason string) (coreadapter.OperationResult, error) {
	return d.EndReply(ctx, operation, stream, in, "failed", reason)
}

// endReply ends the architect's answer with a shed transition, replied-<n>,
// redrafted-<n> or failed-<n>, or parks it at asked-<n> when the kind is
// waiting, and returns the matching result. The transition already recorded
// is returned as it is.
func (d *Debate) EndReply(ctx context.Context, operation string, stream config.WorkstreamID, in RoundInput, kind, reason string) (coreadapter.OperationResult, error) {
	outcome := coreadapter.OperationResult{Outcome: "failed", Evidence: reason}
	id, _ := in.Ids()
	id, to := id+"-"+kind, fmt.Sprintf("%s-%d", kind, in.Round)
	switch kind {
	case in.Recorded():
		outcome.Outcome = "succeeded"
	case questions.Waiting:
		outcome.Outcome, id, to = questions.Waiting, in.Parked(), fmt.Sprintf("asked-%d", in.Round)
	}
	state, err := d.Repository.Workflow(stream, ShedSubject)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	if state.Value != in.Running() {
		result, err := d.ReplyOutcome(stream, in, operation)
		if err != nil || result == nil {
			return coreadapter.OperationResult{}, errors.Join(err, fmt.Errorf("%s is %q, not in progress", in.About(), state.Value))
		}
		return *result, nil
	}
	tx := trace.Transaction{ExpectedVersion: state.Version,
		Transition: trace.Transition{Header: d.Header(id, stream, operation, d.S.Now()), Subject: ShedSubject, From: state.Value, To: to, Reason: reason}}
	if _, err := d.Repository.Transact(ctx, tx); err != nil {
		return coreadapter.OperationResult{}, err
	}
	return outcome, nil
}

// standingAfter returns the dissent that stood once round n was heard, and
// that the owner has not disposed of: what the architect answers.
func (d *Debate) StandingAfter(stream config.WorkstreamID, n int) ([]shed.Entry, error) {
	records, err := d.Earlier(stream, n+1)
	if err != nil {
		return nil, err
	}
	open, err := DissentRecord(d.Repository, stream, records)
	if err != nil {
		return nil, err
	}
	return shed.Standing(open), nil
}

// enqueueReply accepts the next architect turn of the reply, or of the
// redraft the owner asked for, fixing its profile and prompts. turns holds
// one turn per earlier attempt. A turn that follows an invalid redraft lists
// why it was not accepted. Every new attempt, whether it follows an
// interrupted turn or an invalid redraft, repeats the answers the architect
// received in the operation.
func (d *Debate) EnqueueReply(ctx context.Context, cfg *config.Config, stream config.WorkstreamID, in RoundInput, turns []trace.QueuedTurn, operation string, answers []string) error {
	profile, _, err := d.S.RoleExecution(cfg, ArchitectRole)
	if err != nil {
		return err
	}
	open, err := d.StandingAfter(stream, in.Round)
	if err != nil {
		return err
	}
	latest, err := LatestPin(d.Repository, stream)
	if err != nil {
		return err
	}
	var problems []string
	if previous := d.Returned(turns); previous != "" {
		if _, problems, err = d.Redraft(stream, previous); err != nil {
			return err
		}
	}
	prompt := ReplyPrompt(in, latest, open, problems)
	if in.Redraft {
		asked, err := d.Asked(stream, in.Round)
		if err != nil {
			return err
		}
		prompt = RedraftPrompt(in, latest, open, problems, asked.Note)
	}
	if len(answers) > 0 {
		prompt += "\n" + Answered(in.Answering(), answers)
	}
	turn := in.TurnID(len(turns) + 1)
	req := trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_" + turn, Revision: 1, Project: d.Repository.Project(), Workstream: stream, At: d.S.Now(), Actor: ShedActor, Cause: operation, Depth: 1},
		AgentID: ArchitectAgent, ThreadID: ArchitectThread, TurnID: turn, Profile: profile, SystemPrompt: ArchitectSystemPrompt(cfg.Project), Prompt: prompt}
	_, err = d.Repository.EnqueueTurn(ctx, req)
	return err
}

// returned names the reply's latest attempt that ended normally: the one
// whose redraft the next turn is returned, empty when no attempt has.
func (d *Debate) Returned(turns []trace.QueuedTurn) string {
	for i := len(turns) - 1; i >= 0; i-- {
		if !turns[i].CompletedAt.IsZero() && turns[i].Status() == "idle" {
			return turns[i].Request.TurnID
		}
	}
	return ""
}

// dispatchReply runs the turn through the thread dispatcher and runner,
// stopped by a hard pause covering the workstream.
func (d *Debate) DispatchReply(ctx context.Context, stream config.WorkstreamID, in RoundInput, turn string) (coreadapter.OperationResult, error) {
	ctx, release := d.S.Stoppable(ctx, d.Repository.Project(), stream)
	defer release()
	dr := d.Drafter()
	dispatcher := thread.Dispatcher{Runner: d.S.ThreadRunner(d.S.About(d.Repository), d.Repository, &questions.Turns{Turns: d.ReplyPath(stream, in), Repository: d.Repository}, d.S.Now), Prepare: func(_ context.Context, input thread.TurnInput) (coreadapter.PreparedTurn, error) {
		directory := filepath.Join(dr.TurnDirectory(input.Workstream, input.Turn), "session")
		return coreadapter.PreparedTurn{SessionDirectory: directory}, os.MkdirAll(directory, 0700)
	}}
	op, err := thread.TurnOperation(d.Repository.Project(), "reply-turn", thread.TurnInput{Workstream: stream, Agent: ArchitectAgent, Turn: turn})
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
func (d *Debate) ReplyPath(stream config.WorkstreamID, in RoundInput) *isolation.Turns {
	var engine coreadapter.Engine
	var hosts coreadapter.MCPHosts
	if a := d.S.Options.Architect; a != nil {
		engine, hosts = a.Engine, a.Hosts
	}
	denied := errors.New("turn scope denied")
	turns := &isolation.Turns{
		Workspaces: StagedWorkspaces{},
		Views:      isolation.Views{Directory: filepath.Join(d.S.Current().Root.String(), "views")},
		Select: func(ctx context.Context, scope coreadapter.Scope) (isolation.Selection, error) {
			return d.SelectReplyView(ctx, scope, in)
		},
		Grants: map[string]coreadapter.Capabilities{ArchitectRole: {Tools: []string{"file_read", shed.ReplyTool, DraftTool, questions.AskTool}}},
		Scoped: func(_ context.Context, scope coreadapter.Scope) ([]coreadapter.Tool, error) {
			if scope.Workstream != string(stream) || scope.Role != ArchitectRole || scope.Project != string(d.Repository.Project()) || scope.Thread != ArchitectThread {
				return nil, denied
			}
			// The turn that delivers the answer to the architect's question
			// continues the attempt that asked it.
			attempt, err := d.Drafter().Continued(stream, scope.Turn)
			if err != nil {
				return nil, err
			}
			if !strings.HasPrefix(attempt, in.TurnPrefix()) {
				return nil, denied
			}
			open, err := d.StandingAfter(stream, in.Round)
			if err != nil {
				return nil, err
			}
			standing := make([]shed.Dissent, len(open))
			for i, e := range open {
				standing[i] = e.Dissent
			}
			file := filepath.Join(d.Drafter().TurnDirectory(stream, scope.Turn), "output", ReplyFile)
			tools, err := shed.ReplyTools(shed.ReplyTurn{Reply: shed.Reply{Round: in.Round, Revision: in.Pin(), Turn: scope.Turn}, Standing: standing,
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
			ask, err := questions.Tools(d.Repository, ArchitectAgent, scope, d.S.Now)
			if err != nil {
				return nil, err
			}
			return append(append(tools, d.Drafter().DraftTool(scope, attempt, d.Judge)), ask...), nil
		},
		Hosts:  hosts,
		Engine: engine,
	}
	return d.S.MemoryTurns(turns, d.S.About(d.Repository), d.Repository)
}

// selectReplyView stages the architect's view for the claimed turn and
// selects all of it, read-only.
func (d *Debate) SelectReplyView(ctx context.Context, scope coreadapter.Scope, in RoundInput) (isolation.Selection, error) {
	cfg := d.S.About(d.Repository)
	if scope.Role != ArchitectRole || scope.Project != string(d.Repository.Project()) || !cfg.HasProject() || cfg.Project.ID != d.Repository.Project() {
		return isolation.Selection{}, errors.New("view selection denied")
	}
	stream, err := config.ParseWorkstreamID(scope.Workstream)
	if err != nil {
		return isolation.Selection{}, err
	}
	_, settings, err := d.S.RoleExecution(cfg, ArchitectRole)
	if err != nil {
		return isolation.Selection{}, err
	}
	if err := os.MkdirAll(filepath.Join(cfg.Root.String(), "views"), 0700); err != nil {
		return isolation.Selection{}, err
	}
	workspace := filepath.Join(d.Drafter().TurnDirectory(stream, scope.Turn), "workspace")
	paths, err := d.StageReply(ctx, stream, in, scope.Turn, workspace)
	if err != nil {
		return isolation.Selection{}, err
	}
	return isolation.Selection{Workspace: coreadapter.WorkspaceRequest{SourceDirectory: workspace, Directory: workspace}, Paths: paths, Execution: settings}, nil
}

// stageReply builds the view: the latest spec.md and plan.json, the handed
// input under handed/, charter.md, the rendered context bundle as context.md,
// the shed's records under shed/ and, after an invalid redraft, what the
// architect delivered under redraft/. It returns the paths to select.
func (d *Debate) StageReply(ctx context.Context, stream config.WorkstreamID, in RoundInput, turn, workspace string) ([]string, error) {
	if err := os.RemoveAll(workspace); err != nil {
		return nil, err
	}
	dr := d.Drafter()
	latest, err := dr.Latest(stream)
	if err != nil {
		return nil, err
	}
	charter, err := d.Repository.Charter(ctx)
	if err != nil {
		return nil, err
	}
	b, err := d.S.ContextFor(d.Repository).Assemble(ctx, d.Repository.Project(), bundle.Scope{Role: "architect", Workstream: stream})
	if err != nil {
		return nil, err
	}
	files := map[string]string{plan.SpecPath: latest[plan.SpecDocument].Content, plan.PlanPath: latest[plan.PlanDocument].Content, "charter.md": charter.Content, "context.md": b.Brief()}
	paths := []string{plan.SpecPath, plan.PlanPath, "charter.md", "context.md"}
	for _, doc := range latest {
		if directory, _, _ := strings.Cut(doc.Path, "/"); directory == "handed" || directory == "shed" {
			files[doc.Path] = doc.Content
			if !slices.Contains(paths, directory) {
				paths = append(paths, directory)
			}
		}
	}
	t, err := d.Repository.Thread(stream, ArchitectAgent)
	if err != nil {
		return nil, err
	}
	asked, err := d.Repository.Questions(stream)
	if err != nil {
		return nil, err
	}
	origins := in.Chain(t, asked)
	earlier := slices.DeleteFunc(Attempts(ChainTurns(t, origins), origins), func(q trace.QueuedTurn) bool { return q.Request.TurnID == origins[turn] })
	if previous := d.Returned(earlier); previous != "" {
		delivered, _, err := d.Redraft(stream, previous)
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

func ReplyPrompt(in RoundInput, latest shed.Pin, open []shed.Entry, problems []string) string {
	var b strings.Builder
	returned, redraft := InvalidRedraft(problems)
	b.WriteString(returned)
	fmt.Fprintf(&b, "Round %d of the shed is heard. The committee debated %s. %d objections stand:\n", in.Round, in.Pin(), len(open))
	b.WriteString(DissentList(open))
	fmt.Fprintf(&b, `
What each kind asks of you:
- charter: a veto on the part it names. It blocks until its member concedes it after a redraft, or the owner disposes of it.
- size: assess whether splitting improves execution; broad coherent outcomes can remain one unit. This is engineering advice.
- acceptance: clarify observable outcomes and ownership. Keep proposed evidence adaptable. This is engineering advice.
- fit: advice to the owner. It never blocks; answer it.
- owner: the owner's own objection. It blocks until the owner disposes of it; answer it as you would a member's.

%s
This is your one reply to this round. Answer each objection with %s, giving its ID and your answer. When the objections call for a change, also deliver the changed spec.md, plan.json or both with %s: the redraft is validated by the rules your draft was, and a valid one is the revision the committee debates next. An invalid one comes back to you. Deliver nothing to leave the revision as it is.`, View(latest, redraft), shed.ReplyTool, DraftTool)
	return b.String()
}

// redraftPrompt is the architect's turn for the redraft the owner asked for
// after debate concluded: the owner's note is what to change, and the dissent
// that stands is what the committee will read the redraft against.
func RedraftPrompt(in RoundInput, latest shed.Pin, open []shed.Entry, problems []string, note string) string {
	var b strings.Builder
	returned, redraft := InvalidRedraft(problems)
	b.WriteString(returned)
	fmt.Fprintf(&b, "Debate concluded after round %d of the shed, and the owner read the packet and asked you to redraft %s. The owner's note:\n\n%s\n\n", in.Round, in.Pin(), note)
	fmt.Fprintf(&b, "%d objections stand:\n", len(open))
	b.WriteString(DissentList(open))
	fmt.Fprintf(&b, `
%s
Deliver the redrafted spec.md, plan.json or both with %s: the redraft is validated by the rules your draft was, and a valid one is the revision the committee debates in the round that follows. An invalid one comes back to you. Answer with %s an objection your redraft settles, giving its ID and your answer. Deliver nothing to leave the revision as it is; the owner's note is then unanswered.`, View(latest, redraft), DraftTool, shed.ReplyTool)
	return b.String()
}

// invalidRedraft is what the turn is told about a redraft that was not
// accepted, and the line naming where the turn can read it.
func InvalidRedraft(problems []string) (returned, view string) {
	if len(problems) == 0 {
		return "", ""
	}
	return fmt.Sprintf("Your redraft was not accepted, and the committee will not read it:\n- %s\n\nWhat you delivered is in redraft/. Deliver every file of the redraft again with %s, corrected: only what this turn delivers counts. Deliver nothing to leave the revision as it is. Your answers so far are kept; answer an objection again only to replace what you said.\n\n", strings.Join(problems, "\n- "), DraftTool),
		"- redraft/: the files of your redraft that was not accepted.\n"
}

// view describes what the architect's staged view holds.
func View(latest shed.Pin, redraft string) string {
	return fmt.Sprintf(`Your view holds:
- spec.md and plan.json: the latest recorded revision, %s.
- handed/, charter.md and context.md: what you drafted from.
- shed/round-<n>/: what every member contributed to each round, your earlier replies as reply.json, and the owner's own files of the round.
%s`, latest, redraft)
}

// dissentList is one line per objection that stands, as the architect reads
// them.
func DissentList(open []shed.Entry) string {
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
func (d *Debate) Asked(stream config.WorkstreamID, n int) (shed.Redraft, error) {
	redrafts, err := shed.Redrafts(d.Repository, stream)
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
