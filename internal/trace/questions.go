package trace

import (
	"cmp"
	"context"
	"errors"
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
	QuestionRuled     = "ruled"
	QuestionAnswered  = "answered"
	QuestionRouted    = "routed"
)

// The Decision of a ruling: DecisionAnswer when the chief of staff answered
// from the record, DecisionRuling when the owner ruled on an escalation.
const (
	DecisionAnswer = "answer"
	DecisionRuling = "ruling"
)

// The Scope of a relayed owner ruling. A local ruling reaches only the askers;
// a notify ruling is also a notice to the whole project.
const (
	ScopeLocal  = "local"
	ScopeNotify = "notify"
)

// ErrInboxEntry reports an inbox number no escalation of the project carries.
var ErrInboxEntry = errors.New("no such inbox entry")

// ErrRuled reports an inbox entry that already has the owner's ruling.
var ErrRuled = errors.New("inbox entry already has the owner's ruling")

func validRulingScope(r Ruling) bool {
	if r.Scope == "" {
		return true
	}
	return (r.Scope == ScopeLocal || r.Scope == ScopeNotify) && present(r.ReturnedAnswer)
}

// QuestionSubject is the workflow subject holding one question's state.
func QuestionSubject(id string) string { return "question_" + id }

func (e Escalation) valid(id string) bool {
	if !key(e.Batch) || e.Inbox < 1 || !slices.Contains(e.Questions, id) || !present(e.Recommendation) {
		return false
	}
	if len(e.Workstreams) != 0 && len(e.Workstreams) != len(e.Questions) {
		return false
	}
	for _, w := range e.Workstreams {
		if w != "" && config.CheckWorkstreamIDs(w) != nil {
			return false
		}
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
	questions, _, err := r.QuestionActions(stream)
	return questions, err
}

// QuestionActions reads questions and filed amendment requests from one trace
// snapshot, so a turn can determine its durable waiting outcome in one scan.
func (r *Repository) QuestionActions(stream config.WorkstreamID) ([]QuestionState, []Amendment, error) {
	target, err := r.lockQuestionRead(stream)
	if err != nil {
		return nil, nil, err
	}
	defer target.mu.Unlock()
	_, v, err := target.loadWorkflow(stream)
	if err != nil {
		return nil, nil, err
	}
	records, _, err := target.scan()
	if err != nil {
		return nil, nil, err
	}
	var amendments []Amendment
	for _, record := range records {
		if a, ok := record.(Amendment); ok && a.Workstream == stream {
			amendments = append(amendments, a)
		}
	}
	return questions(records, v, stream), amendments, nil
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
	if r.parent != nil {
		group := config.WorkgroupID(v.states[CrossRepoWorkgroupSubject].Value)
		retired, err := r.parent.workgroupMemberRetired(group, r.stream)
		if err != nil {
			return QueuedTurn{}, workflowLog{}, nil, nil, err
		}
		if retired {
			return QueuedTurn{}, workflowLog{}, nil, nil, &RetiredMemberError{Project: r.project, Workstream: r.stream}
		}
	}
	records, _, err := r.scan()
	return q, log, v, records, err
}

func questionTransition(v *workflowView, h Header, id, from, to, reason string) Transaction {
	h.Schema, h.ID, h.Revision = "osmia.trace.transition", QuestionSubject(id)+"_"+to, 1
	return Transaction{ExpectedVersion: v.states[QuestionSubject(id)].Version, Transition: Transition{Header: h, Subject: QuestionSubject(id), From: from, To: to, Reason: reason}}
}

// Ask records text as the workstream's next question, numbered from 1, asked
// by the scope's active turn, and raises one notice for the chief of staff in
// the same commit. A chief-of-staff turn fails. An empty question and a second
// question from the same turn are refused with *QuestionRefused.
func (r *Repository) Ask(ctx context.Context, agent string, scope coreadapter.Scope, text string, at time.Time) (Question, error) {
	return r.AskCharterConflict(ctx, agent, scope, text, nil, at)
}

// AskCharterConflict records a question and sends a reported conflict between
// member charters directly to the owner in the same question transaction.
func (r *Repository) AskCharterConflict(ctx context.Context, agent string, scope coreadapter.Scope, text string, citations []string, at time.Time) (Question, error) {
	if citations != nil && len(citations) < 2 {
		return Question{}, refused("a charter conflict must cite rules from two member projects")
	}
	var inbox int
	if len(citations) > 0 {
		if scope.Role == ChiefOfStaff {
			return Question{}, fmt.Errorf("the chief of staff answers questions and cannot ask one")
		}
		if !present(text) {
			return Question{}, refused("question is required: say what you need decided and what it blocks")
		}
		if err := r.ActiveTurn(scope); err != nil {
			return Question{}, err
		}

		if err := r.CheckCharterConflict(ctx, config.WorkstreamID(scope.Workstream), citations); err != nil {
			return Question{}, refused("%s", err)
		}
		authority := r
		if r.memberBindings() == nil {
			id, err := r.CrossRepoWorkgroupOf(config.WorkstreamID(scope.Workstream))
			if err != nil {
				return Question{}, err
			}
			if id != "" {
				authority, err = OpenWorkgroupAuthority(r.root, id)
				if err != nil {
					return Question{}, err
				}
				defer authority.Close()
			}
		}
		var err error
		inbox, err = authority.reserveInboxNumber(ctx)
		if err != nil {
			return Question{}, err
		}
	}

	r, err := r.lockQuestionRead(config.WorkstreamID(scope.Workstream))
	if err != nil {
		return Question{}, err
	}
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
	for _, record := range records {
		if a, ok := record.(Amendment); ok && a.Workstream == stream && a.Actor.ID == agent && a.Thread == scope.Thread && a.Turn == scope.Turn {
			return Question{}, refused("this turn already filed amendment %s; end the turn", a.ID)
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
	tx := questionTransition(v, h, id, "", QuestionOpen, fmt.Sprintf("The %s asked question %s", scope.Role, id))
	tx.Events = []Event{Notice(tx.Transition.ID, "question", fmt.Sprintf("Question %s is open, asked by the %s: %s", id, scope.Role, strings.TrimSpace(text)))}
	if len(citations) > 0 {
		question.CharterConflict = slices.Clone(citations)
		question.SentToOwner = text
		question.Escalation = &Escalation{Batch: "charter_conflict_" + id, Inbox: inbox, Questions: []string{id}, Section: strings.Join(citations, ", "), Conflict: text, Blocked: "The asking member waits for the owner's charter ruling.", Recommendation: "Resolve the conflict between member charters before continuing."}
		tx = questionTransition(v, h, id, "", QuestionEscalated, "A conflict between member charters requires the owner")
		tx.Events = []Event{Notice(tx.Transition.ID, "question", "A conflict between member charters is escalated to the owner: "+text)}
	}

	files, _, err := r.stage(ctx, stream, log, v, []Record{question}, tx)
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
		case QuestionRuled:
			return q, refused("question %s has the owner's ruling; relay it with relay_ruling", id)
		case QuestionAnswered:
			return q, refused("question %s is already answered", id)
		case QuestionRouted:
			return q, refused("question %s was routed to an amendment", id)
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

// chiefTurnOn requires that scope is the active chief-of-staff turn of its
// own workstream, then returns a repository instance scoped and locked for
// member, ready for the caller to read or write its questions. The caller
// must unlock the returned repository's mu once done, whether or not this
// returns an error. r must be the project's own repository.
func (r *Repository) chiefTurnOn(ctx context.Context, agent string, scope coreadapter.Scope, member config.WorkstreamID, at time.Time) (*Repository, QueuedTurn, error) {
	host := config.WorkstreamID(scope.Workstream)
	hr, err := r.lockQuestionRead(host)
	if err != nil {
		return nil, QueuedTurn{}, err
	}
	turn, _, _, _, err := hr.chiefTurn(ctx, agent, scope, at)
	if member == host {
		if err != nil {
			hr.mu.Unlock()
			return nil, QueuedTurn{}, err
		}
		return hr, turn, nil
	}
	hr.mu.Unlock()
	if err != nil {
		return nil, QueuedTurn{}, err
	}
	tr, err := r.lockQuestionRead(member)
	if err != nil {
		return nil, QueuedTurn{}, err
	}
	return tr, turn, nil
}

// chiefQuestionScope resolves id to the member workstream a chief-of-staff
// tool names, with id's own local form, validates that scope is its active
// turn, and returns a repository instance scoped and locked for that
// member, ready for the caller to read or write its questions. The caller
// must unlock target.mu once done, whether or not it returns an error.
// r must be the project's own repository.
func (r *Repository) chiefQuestionScope(ctx context.Context, agent string, scope coreadapter.Scope, id string, at time.Time) (target *Repository, localID string, turn QueuedTurn, err error) {
	member, localID, err := r.ChiefQuestionTarget(config.WorkstreamID(scope.Workstream), id)
	if err != nil {
		return nil, "", QueuedTurn{}, err
	}
	target, turn, err = r.chiefTurnOn(ctx, agent, scope, member, at)
	return target, localID, turn, err
}

// AnswerQuestion records the chief of staff's answer to an open question as
// the question's ruling, with the citations it rests on, and moves the
// question to answered in the same commit. id names a question of the
// turn's own workstream when unqualified, or "<workstream>:<id>" one of any
// other member of the shared workgroup; the ruling is recorded in that
// question's own workstream. The caller has checked that every citation
// resolves. A turn of another role fails, and so does, with ErrConflict, an
// open question that already has a ruling record. A question that is not
// open, an empty answer and an answer without a citation are refused with
// *QuestionRefused.
func (r *Repository) AnswerQuestion(ctx context.Context, agent string, scope coreadapter.Scope, id, text string, citations []string, at time.Time) (Ruling, error) {
	target, localID, turn, err := r.chiefQuestionScope(ctx, agent, scope, id, at)
	if err != nil {
		return Ruling{}, err
	}
	defer target.mu.Unlock()
	stream := target.stream
	log, v, err := target.loadWorkflow(stream)
	if err != nil {
		return Ruling{}, err
	}
	records, _, err := target.scan()
	if err != nil {
		return Ruling{}, err
	}
	q, err := openQuestion(questions(records, v, stream), localID)
	if err != nil {
		return Ruling{}, err
	}
	if !present(text) {
		return Ruling{}, refused("text is required: the answer the asker receives")
	}
	if len(citations) == 0 {
		return Ruling{}, refused("an answer needs at least one citation; escalate a question the record does not settle")
	}
	h := Header{Schema: "osmia.trace.ruling", Version: Version, ID: localID, Revision: 1, Project: target.project, Workstream: stream, Unit: q.Asked.Unit,
		At: at, Actor: Actor{Kind: "agent", ID: agent}, Cause: turn.Request.ID, Depth: turn.Request.Depth + 1}
	ruling := Ruling{Header: h, QuestionID: localID, QuestionRevision: q.Latest.Revision, Decision: DecisionAnswer, ReturnedAnswer: text, Citations: slices.Clone(citations)}
	if err := validate(ruling); err != nil {
		return Ruling{}, err
	}
	for _, rec := range records {
		if recordKey(rec) == recordKey(ruling) {
			return Ruling{}, fmt.Errorf("%w: ruling %s already exists", ErrConflict, localID)
		}
	}
	tx := questionTransition(v, h, localID, QuestionOpen, QuestionAnswered, fmt.Sprintf("The chief of staff answered question %s citing %s", localID, strings.Join(citations, ", ")))
	files, _, err := target.stage(ctx, stream, log, v, []Record{ruling}, tx)
	if err != nil {
		return Ruling{}, err
	}
	if err := target.publish(ctx, files); err != nil {
		return Ruling{}, err
	}
	_ = target.wake.Notify(context.Background())
	return ruling, nil
}

// EscalationRequest is what the chief of staff sends the owner for one or
// several open questions. Section cites the spec or plan passage in
// tension and Conflict states the tension itself: a worker's escalated
// question is structured with both, plus Options and a Recommendation
// (spec#17).
type EscalationRequest struct {
	Questions      []string
	Rephrasing     string
	Section        string
	Conflict       string
	Blocked        string
	Options        []string
	Recommendation string
}

// EscalateQuestions marks every listed open question escalated in one
// consolidated batch: each gets a revision carrying the rephrasing and the
// escalation, whose batch ID it returns, and the batch takes the project's
// next inbox number, once, however many workstreams it spans. Every id
// names a question of the turn's own workstream when unqualified, or
// "<workstream>:<id>" one of any other member of the shared workgroup;
// ChiefQuestionTarget already refuses a workstream outside it, so a batch
// can span only members of one workgroup. Recording spans one commit per
// distinct workstream the batch touches: every question is checked eligible,
// and the inbox number reserved, before any of them is written, so a
// refused batch wastes no number and writes nothing; a crash after some of
// those commits is not retried by this call, but its own questions keep
// their escalation, and run() with the same idempotent action, as every
// caller of this tool already does, must not call it a second time for
// questions it already escalated. A turn of another role fails. Unless
// every question is open, none is escalated and the request is refused
// with *QuestionRefused.
func (r *Repository) EscalateQuestions(ctx context.Context, agent string, scope coreadapter.Scope, req EscalationRequest, at time.Time) (string, error) {
	project := r
	if len(req.Questions) == 0 {
		return "", refused("questions is required: list at least one open question")
	}
	host := config.WorkstreamID(scope.Workstream)
	members := make([]config.WorkstreamID, len(req.Questions))
	locals := make([]string, len(req.Questions))
	for i, raw := range req.Questions {
		member, local, err := project.ChiefQuestionTarget(host, raw)
		if err != nil {
			return "", err
		}
		for j := 0; j < i; j++ {
			if members[j] == member && locals[j] == local {
				return "", refused("question %s is listed twice", raw)
			}
		}
		members[i], locals[i] = member, local
	}
	switch {
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
	var distinct []config.WorkstreamID
	for _, m := range members {
		if !slices.Contains(distinct, m) {
			distinct = append(distinct, m)
		}
	}
	// Every question of every workstream the batch touches is checked
	// eligible before the inbox number is reserved, so a refused batch
	// never wastes one.
	for _, member := range distinct {
		if err := func() error {
			mr, _, err := project.chiefTurnOn(ctx, agent, scope, member, at)
			if err != nil {
				return err
			}
			defer mr.mu.Unlock()
			_, v, err := mr.loadWorkflow(member)
			if err != nil {
				return err
			}
			records, _, err := mr.scan()
			if err != nil {
				return err
			}
			existing := questions(records, v, member)
			for i, m := range members {
				if m != member {
					continue
				}
				if _, err := openQuestion(existing, locals[i]); err != nil {
					return err
				}
			}
			return nil
		}(); err != nil {
			return "", err
		}
	}
	// The project's inbox number is reserved before any workstream's own
	// commit, in the project's own trace: a crash between the
	// two leaves the number unused, never reused, and the escalation is
	// retried with a freshly reserved one.
	inbox, err := project.reserveInboxNumber(ctx)
	if err != nil {
		return "", err
	}
	batch := "escalation_" + locals[0]
	escalation := &Escalation{Batch: batch, Inbox: inbox, Questions: slices.Clone(locals), Section: req.Section, Conflict: req.Conflict, Blocked: req.Blocked, Options: slices.Clone(req.Options), Recommendation: req.Recommendation}
	if len(distinct) > 1 {
		// Workstreams is recorded only for a batch that genuinely spans more
		// than one of the workgroup's members, so a single-workstream
		// escalation's record is byte-identical to one from before
		// cross-workstream batches existed.
		escalation.Workstreams = slices.Clone(members)
	}
	for _, member := range distinct {
		if err := func() error {
			mr, turn, err := project.chiefTurnOn(ctx, agent, scope, member, at)
			if err != nil {
				return err
			}
			defer mr.mu.Unlock()
			log, v, err := mr.loadWorkflow(member)
			if err != nil {
				return err
			}
			records, _, err := mr.scan()
			if err != nil {
				return err
			}
			existing := questions(records, v, member)
			var revisions []Record
			var txs []Transaction
			for i, m := range members {
				if m != member {
					continue
				}
				id := locals[i]
				q, err := openQuestion(existing, id)
				if err != nil {
					return err
				}
				next := q.Latest
				next.Revision++
				next.At, next.Actor, next.Cause, next.Depth = at, Actor{Kind: "agent", ID: agent}, turn.Request.ID, turn.Request.Depth+1
				next.SentToOwner, next.Escalation = req.Rephrasing, escalation
				if err := validate(next); err != nil {
					return err
				}
				revisions = append(revisions, next)
				txs = append(txs, questionTransition(v, next.Header, id, QuestionOpen, QuestionEscalated, fmt.Sprintf("The chief of staff escalated question %s to the owner in %s", id, batch)))
			}
			files, _, err := mr.stage(ctx, member, log, v, revisions, txs...)
			if err != nil {
				return err
			}
			if err := mr.publish(ctx, files); err != nil {
				return err
			}
			_ = mr.wake.Notify(context.Background())
			return nil
		}(); err != nil {
			return "", err
		}
	}
	return batch, nil
}
