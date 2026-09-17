package trace

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
)

// The states of a question's workflow subject. A question is open from the
// moment it is asked until the chief of staff records its one choice.
const (
	QuestionOpen      = "open"
	QuestionEscalated = "escalated"
	QuestionAnswered  = "answered"
)

// DecisionAnswer is the Decision of a ruling the chief of staff recorded by
// answering from the record.
const DecisionAnswer = "answer"

// QuestionSubject is the workflow subject holding one question's state.
func QuestionSubject(id string) string { return "question_" + id }

func (e Escalation) valid(id string) bool {
	if !key(e.Batch) || !slices.Contains(e.Questions, id) || !present(e.Blocked) || !present(e.Recommendation) {
		return false
	}
	for _, q := range e.Questions {
		if !key(q) {
			return false
		}
	}
	for _, o := range e.Options {
		if !present(o) {
			return false
		}
	}
	return true
}

// QuestionRefused is returned when a question tool's request cannot be
// recorded. Nothing was written. Reason is written for the calling agent.
type QuestionRefused struct{ Reason string }

func (e *QuestionRefused) Error() string { return "question refused: " + e.Reason }

func refused(format string, args ...any) error {
	return &QuestionRefused{Reason: fmt.Sprintf(format, args...)}
}

// QuestionState is one question with the chief of staff's choice. Asked is
// revision 1, the question as asked; Latest is its newest revision, which
// carries the escalation once escalated. State is the value of the question's
// workflow subject, empty for a question recorded without one. Ruling is the
// newest revision of the question's ruling, nil until one is recorded.
type QuestionState struct {
	Asked  Question
	Latest Question
	State  string
	Ruling *Ruling
}

// Questions returns every question of the workstream, oldest first.
func (r *Repository) Questions(stream config.WorkstreamID) ([]QuestionState, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, v, err := r.loadWorkflow(stream)
	if err != nil {
		return nil, err
	}
	records, _, err := r.scan()
	if err != nil {
		return nil, err
	}
	return questions(records, v, stream), nil
}

func questions(records []Record, v *workflowView, stream config.WorkstreamID) []QuestionState {
	byID := map[string]*QuestionState{}
	var out []*QuestionState
	for _, rec := range records {
		if rec.header().Workstream != stream {
			continue
		}
		if q, ok := rec.(Question); ok {
			s := byID[q.ID]
			if s == nil {
				s = &QuestionState{Asked: q, State: v.states[QuestionSubject(q.ID)].Value}
				byID[q.ID] = s
				out = append(out, s)
			}
			s.Latest = q
		}
	}
	for _, rec := range records {
		if ruling, ok := rec.(Ruling); ok && ruling.Workstream == stream && byID[ruling.QuestionID] != nil {
			byID[ruling.QuestionID].Ruling = &ruling
		}
	}
	slices.SortStableFunc(out, func(x, y *QuestionState) int {
		return cmp.Or(x.Asked.At.Compare(y.Asked.At), cmp.Compare(len(x.Asked.ID), len(y.Asked.ID)), strings.Compare(x.Asked.ID, y.Asked.ID))
	})
	result := make([]QuestionState, 0, len(out))
	for _, s := range out {
		result = append(result, *s)
	}
	return result
}

// questionTurn requires r.mu. It checks that scope names this session's
// active turn of agent and returns the turn with the workstream's workflow
// and questions.
func (r *Repository) questionTurn(ctx context.Context, agent string, scope coreadapter.Scope, at time.Time) (QueuedTurn, workflowLog, *workflowView, []Record, error) {
	if err := ctx.Err(); err != nil {
		return QueuedTurn{}, workflowLog{}, nil, nil, err
	}
	if at.IsZero() {
		return QueuedTurn{}, workflowLog{}, nil, nil, fmt.Errorf("timestamp required")
	}
	_, q, err := r.turnScope(agent, scope, true)
	if err != nil {
		return QueuedTurn{}, workflowLog{}, nil, nil, err
	}
	log, v, err := r.loadWorkflow(config.WorkstreamID(scope.Workstream))
	if err != nil {
		return QueuedTurn{}, workflowLog{}, nil, nil, err
	}
	records, _, err := r.scan()
	return q, log, v, records, err
}

func questionTransition(h Header, id, from, to, reason string) Transaction {
	h.Schema, h.ID, h.Revision = "osmia.trace.transition", QuestionSubject(id)+"_"+to, 1
	version := uint64(0)
	if from != "" {
		version = 1
	}
	return Transaction{ExpectedVersion: version, Transition: Transition{Header: h, Subject: QuestionSubject(id), From: from, To: to, Reason: reason}}
}

// Ask records text as the workstream's next question, numbered from 1, asked
// by the scope's active turn, and raises one notice for the chief of staff in
// the same commit. A chief-of-staff turn, an empty question and a second
// question from the same turn are refused with *QuestionRefused.
func (r *Repository) Ask(ctx context.Context, agent string, scope coreadapter.Scope, text string, at time.Time) (Question, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if scope.Role == ChiefOfStaff {
		return Question{}, fmt.Errorf("the chief of staff answers questions and cannot ask one")
	}
	turn, log, v, records, err := r.questionTurn(ctx, agent, scope, at)
	if err != nil {
		return Question{}, err
	}
	if !present(text) {
		return Question{}, refused("question is required: say what you need decided and what it blocks")
	}
	stream := config.WorkstreamID(scope.Workstream)
	existing := questions(records, v, stream)
	ids := map[string]bool{}
	for _, q := range existing {
		ids[q.Asked.ID] = true
		if q.Asked.AskedBy.ID == agent && q.Asked.Thread == scope.Thread && q.Asked.Turn == scope.Turn {
			return Question{}, refused("this turn already asked question %s; end the turn, the answer arrives as your next turn", q.Asked.ID)
		}
	}
	n := len(existing) + 1
	for ids[strconv.Itoa(n)] {
		n++
	}
	id := strconv.Itoa(n)
	h := Header{Schema: "osmia.trace.question", Version: Version, ID: id, Revision: 1, Project: r.project, Workstream: stream, Unit: scope.Unit,
		At: at, Actor: Actor{Kind: "agent", ID: agent}, Cause: turn.Request.ID, Depth: turn.Request.Depth + 1}
	question := Question{Header: h, AskedBy: h.Actor, Thread: scope.Thread, Turn: scope.Turn, Question: text}
	if err := validate(question); err != nil {
		return Question{}, err
	}
	tx := questionTransition(h, id, "", QuestionOpen, fmt.Sprintf("The %s asked question %s", scope.Role, id))
	tx.Events = []Event{Notice(tx.Transition.ID, "question", fmt.Sprintf("Question %s is open, asked by the %s: %s", id, scope.Role, strings.TrimSpace(text)))}
	files, _, err := r.stage(stream, log, v, []Record{question}, tx)
	if err != nil {
		return Question{}, err
	}
	if err := r.publish(ctx, files); err != nil {
		return Question{}, err
	}
	_ = r.wake.Notify(context.Background())
	return question, nil
}

// openQuestion returns the open question id, or why the chief of staff can
// no longer choose for it.
func openQuestion(existing []QuestionState, id string) (QuestionState, error) {
	for _, q := range existing {
		if q.Asked.ID != id {
			continue
		}
		switch q.State {
		case QuestionOpen:
			return q, nil
		case QuestionEscalated:
			return q, refused("question %s is escalated to the owner; only the owner's ruling answers it", id)
		case QuestionAnswered:
			return q, refused("question %s is already answered", id)
		default:
			return q, refused("question %s was not asked through ask and cannot be chosen for", id)
		}
	}
	return QuestionState{}, refused("there is no question %s in this workstream", id)
}

// chiefTurn requires r.mu. It is questionTurn for a chief-of-staff tool.
func (r *Repository) chiefTurn(ctx context.Context, agent string, scope coreadapter.Scope, at time.Time) (QueuedTurn, workflowLog, *workflowView, []Record, error) {
	if scope.Role != ChiefOfStaff {
		return QueuedTurn{}, workflowLog{}, nil, nil, fmt.Errorf("only a chief-of-staff turn may choose what happens to a question")
	}
	return r.questionTurn(ctx, agent, scope, at)
}

// AnswerQuestion records the chief of staff's answer to an open question as
// the question's ruling, with the citations it rests on, and moves the
// question to answered in the same commit. The caller has checked that every
// citation resolves. A question that is not open, an empty answer and an
// answer without a citation are refused with *QuestionRefused.
func (r *Repository) AnswerQuestion(ctx context.Context, agent string, scope coreadapter.Scope, id, text string, citations []string, at time.Time) (Ruling, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	turn, log, v, records, err := r.chiefTurn(ctx, agent, scope, at)
	if err != nil {
		return Ruling{}, err
	}
	stream := config.WorkstreamID(scope.Workstream)
	q, err := openQuestion(questions(records, v, stream), id)
	if err != nil {
		return Ruling{}, err
	}
	if !present(text) {
		return Ruling{}, refused("text is required: the answer the asker receives")
	}
	if len(citations) == 0 {
		return Ruling{}, refused("an answer needs at least one citation; escalate a question the record does not settle")
	}
	h := Header{Schema: "osmia.trace.ruling", Version: Version, ID: id, Revision: 1, Project: r.project, Workstream: stream, Unit: q.Asked.Unit,
		At: at, Actor: Actor{Kind: "agent", ID: agent}, Cause: turn.Request.ID, Depth: turn.Request.Depth + 1}
	ruling := Ruling{Header: h, QuestionID: id, QuestionRevision: q.Latest.Revision, Decision: DecisionAnswer, ReturnedAnswer: text, Citations: slices.Clone(citations)}
	if err := validate(ruling); err != nil {
		return Ruling{}, err
	}
	for _, rec := range records {
		if recordKey(rec) == recordKey(ruling) {
			return Ruling{}, fmt.Errorf("%w: ruling %s already exists", ErrConflict, id)
		}
	}
	tx := questionTransition(h, id, QuestionOpen, QuestionAnswered, fmt.Sprintf("The chief of staff answered question %s citing %s", id, strings.Join(citations, ", ")))
	files, _, err := r.stage(stream, log, v, []Record{ruling}, tx)
	if err != nil {
		return Ruling{}, err
	}
	if err := r.publish(ctx, files); err != nil {
		return Ruling{}, err
	}
	_ = r.wake.Notify(context.Background())
	return ruling, nil
}

// EscalationRequest is what the chief of staff sends the owner for one or
// several open questions.
type EscalationRequest struct {
	Questions      []string
	Rephrasing     string
	Blocked        string
	Options        []string
	Recommendation string
}

// EscalateQuestions marks every listed open question escalated in one commit:
// each gets a revision carrying the rephrasing and the escalation, whose
// batch ID it returns. Unless every question is open, none is escalated and
// the request is refused with *QuestionRefused.
func (r *Repository) EscalateQuestions(ctx context.Context, agent string, scope coreadapter.Scope, req EscalationRequest, at time.Time) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	turn, log, v, records, err := r.chiefTurn(ctx, agent, scope, at)
	if err != nil {
		return "", err
	}
	switch {
	case len(req.Questions) == 0:
		return "", refused("questions is required: list at least one open question")
	case !present(req.Rephrasing):
		return "", refused("rephrasing is required: the question as the owner should read it")
	case !present(req.Blocked):
		return "", refused("blocked is required: what waits on the owner's answer")
	case !present(req.Recommendation):
		return "", refused("recommendation is required: what you would decide")
	}
	for _, o := range req.Options {
		if !present(o) {
			return "", refused("options must not contain an empty entry")
		}
	}
	stream := config.WorkstreamID(scope.Workstream)
	existing := questions(records, v, stream)
	escalation := &Escalation{Batch: "escalation_" + req.Questions[0], Questions: slices.Clone(req.Questions), Blocked: req.Blocked, Options: slices.Clone(req.Options), Recommendation: req.Recommendation}
	var revisions []Record
	var txs []Transaction
	for i, id := range req.Questions {
		if slices.Contains(req.Questions[:i], id) {
			return "", refused("question %s is listed twice", id)
		}
		q, err := openQuestion(existing, id)
		if err != nil {
			return "", err
		}
		next := q.Latest
		next.Revision++
		next.At, next.Actor, next.Cause, next.Depth = at, Actor{Kind: "agent", ID: agent}, turn.Request.ID, turn.Request.Depth+1
		next.SentToOwner, next.Escalation = req.Rephrasing, escalation
		if err := validate(next); err != nil {
			return "", err
		}
		revisions = append(revisions, next)
		txs = append(txs, questionTransition(next.Header, id, QuestionOpen, QuestionEscalated, fmt.Sprintf("The chief of staff escalated question %s to the owner in %s", id, escalation.Batch)))
	}
	files, _, err := r.stage(stream, log, v, revisions, txs...)
	if err != nil {
		return "", err
	}
	if err := r.publish(ctx, files); err != nil {
		return "", err
	}
	_ = r.wake.Notify(context.Background())
	return escalation.Batch, nil
}
