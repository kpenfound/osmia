package trace

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
)

// InboxEntry is one escalation: the batch of questions the chief of staff
// sent the owner as one ask. Number is the escalation's inbox number, unique
// in the project and never reused. Questions holds the batch in the order it
// was escalated, and every one of them is in State.
type InboxEntry struct {
	Number         int
	Workstream     config.WorkstreamID
	Batch          string
	Rephrasing     string
	Blocked        string
	Options        []string
	Recommendation string
	EscalatedAt    time.Time
	State          string
	Questions      []QuestionState
}

// inboxCounterFile is the project's own durable record of the next inbox
// number to hand out. It is not a versioned trace record: only its current
// value matters, and nothing but ReserveInboxNumber ever writes it.
const inboxCounterFile = "inbox-counter.json"

type inboxCounter struct {
	Next int `json:"next"`
}

// reserveInboxNumber durably advances the project's own inbox counter and
// returns the number it reserves, unique across every workstream of the
// project and never reused. It must run on the project's own trace instance,
// not a workstream's: inbox numbering is project-level bookkeeping, held in
// the project's trace and reserved there before the escalation that uses the
// number is recorded in its workstream's own trace. A crash between the two
// leaves the reserved number unused rather than ever handing it out again.
func (r *Repository) reserveInboxNumber(ctx context.Context) (int, error) {
	if r.stream != "" {
		return 0, fmt.Errorf("inbox numbering belongs to the project's own trace")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	r.forget()
	if _, err := r.checkHistory(ctx); err != nil {
		return 0, err
	}
	data, err := r.readFile(inboxCounterFile)
	if err != nil && !os.IsNotExist(err) {
		return 0, err
	}
	var c inboxCounter
	if err == nil {
		if err := decode(data, &c); err != nil {
			return 0, err
		}
	}
	next := c.Next + 1
	out, err := json.Marshal(inboxCounter{Next: next})
	if err != nil {
		return 0, err
	}
	if err := r.writeFile(inboxCounterFile, append(out, '\n')); err != nil {
		return 0, err
	}
	if err := r.commit(ctx, []string{inboxCounterFile}, "Reserve inbox number"); err != nil {
		return 0, err
	}
	return next, nil
}

// Inbox returns every escalation of the project by inbox number, whatever
// became of it since.
func (r *Repository) Inbox() ([]InboxEntry, error) {
	return r.inbox()
}

func (r *Repository) inbox() ([]InboxEntry, error) {
	ids, err := r.diskStreams()
	if err != nil {
		return nil, err
	}
	var out []InboxEntry
	for _, id := range ids {
		s, err := r.scoped(id)
		if err != nil {
			return nil, err
		}
		s.mu.Lock()
		records, _, err := s.scan()
		if err == nil {
			var v *workflowView
			_, v, err = s.loadWorkflow(id)
			if err == nil {
				out = append(out, escalations(questions(records, v, id))...)
			}
		}
		s.mu.Unlock()
		if err != nil {
			return nil, fmt.Errorf("workstream %s: %w", id, err)
		}
	}
	slices.SortFunc(out, func(x, y InboxEntry) int { return cmp.Compare(x.Number, y.Number) })
	return out, nil
}

// escalations groups a workstream's escalated questions by batch.
func escalations(states []QuestionState) []InboxEntry {
	byID := map[string]QuestionState{}
	for _, q := range states {
		byID[q.Asked.ID] = q
	}
	var out []InboxEntry
	for _, q := range states {
		e := q.Latest.Escalation
		if e == nil || e.Questions[0] != q.Asked.ID {
			continue
		}
		entry := InboxEntry{Number: e.Inbox, Workstream: q.Asked.Workstream, Batch: e.Batch, Rephrasing: q.Latest.SentToOwner, Blocked: e.Blocked,
			Options: slices.Clone(e.Options), Recommendation: e.Recommendation, EscalatedAt: q.Latest.At, State: q.State}
		for _, id := range e.Questions {
			entry.Questions = append(entry.Questions, byID[id])
		}
		out = append(out, entry)
	}
	return out
}

// Rule records text as the owner's ruling on inbox entry number: revision 1 of
// the ruling of every question in the batch, each question's move from
// escalated to ruled and one notice for the chief of staff, in one commit. It
// returns the entry as it was ruled on. An unknown number fails with
// ErrInboxEntry, an entry whose workstream's feature state is one of refused
// with ErrFeatureState and the entry, and an entry that is no longer escalated
// with ErrRuled; all three write nothing. The checks and the write hold the
// same lock. Text must not be blank.
func (r *Repository) Rule(ctx context.Context, number int, text string, owner Actor, at time.Time, refused ...string) (InboxEntry, error) {
	if err := ctx.Err(); err != nil {
		return InboxEntry{}, err
	}
	if !present(text) || at.IsZero() {
		return InboxEntry{}, fmt.Errorf("a ruling needs text and a timestamp")
	}
	entries, err := r.inbox()
	if err != nil {
		return InboxEntry{}, err
	}
	i := slices.IndexFunc(entries, func(e InboxEntry) bool { return e.Number == number })
	if i < 0 {
		return InboxEntry{}, fmt.Errorf("%w: %d", ErrInboxEntry, number)
	}
	entry := entries[i]
	r, err = r.scoped(entry.Workstream)
	if err != nil {
		return InboxEntry{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	log, v, err := r.loadWorkflow(entry.Workstream)
	if err != nil {
		return InboxEntry{}, err
	}
	if slices.Contains(refused, v.states[FeatureSubject].Value) {
		return entry, ErrFeatureState
	}
	if entry.State != QuestionEscalated {
		return InboxEntry{}, fmt.Errorf("%w: %d", ErrRuled, number)
	}
	var ids []string
	var rulings []Record
	var txs []Transaction
	for _, q := range entry.Questions {
		id := q.Asked.ID
		h := Header{Schema: "osmia.trace.ruling", Version: Version, ID: id, Revision: 1, Project: r.project, Workstream: entry.Workstream, Unit: q.Asked.Unit,
			At: at, Actor: owner, Cause: QuestionSubject(id) + "_" + QuestionEscalated, Depth: q.Latest.Depth + 1}
		ruling := Ruling{Header: h, QuestionID: id, QuestionRevision: q.Latest.Revision, Decision: DecisionRuling, OwnerResponse: text}
		if err := validate(ruling); err != nil {
			return InboxEntry{}, err
		}
		ids = append(ids, id)
		rulings = append(rulings, ruling)
		txs = append(txs, questionTransition(v, h, id, QuestionEscalated, QuestionRuled, fmt.Sprintf("The owner ruled on question %s, inbox entry %d", id, number)))
	}
	txs[0].Events = []Event{Notice(txs[0].Transition.ID, "ruling", fmt.Sprintf("The owner ruled on inbox entry %d, %s (questions %s): %s", number, entry.Batch, strings.Join(ids, ", "), strings.TrimSpace(text)))}
	files, _, err := r.stage(entry.Workstream, log, v, rulings, txs...)
	if err != nil {
		return InboxEntry{}, err
	}
	if err := r.publish(ctx, files); err != nil {
		return InboxEntry{}, err
	}
	_ = r.wake.Notify(context.Background())
	return entry, nil
}

// RelayRuling records what goes back to the askers of a ruled question's
// batch: revision 2 of every ruling in it, with text as the returned answer and
// the scope, and each question's move from ruled to answered, in one commit.
// It returns the batch's questions. A turn of another role fails. A question
// that is not ruled, an empty text and a scope other than ScopeLocal and
// ScopeNotify are refused with *QuestionRefused.
func (r *Repository) RelayRuling(ctx context.Context, agent string, scope coreadapter.Scope, id, text, reach string, at time.Time) ([]string, error) {
	r, err := r.scoped(config.WorkstreamID(scope.Workstream))
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	turn, log, v, records, err := r.chiefTurn(ctx, agent, scope, at)
	if err != nil {
		return nil, err
	}
	stream := config.WorkstreamID(scope.Workstream)
	existing := questions(records, v, stream)
	i := slices.IndexFunc(existing, func(q QuestionState) bool { return q.Asked.ID == id })
	switch {
	case i < 0:
		return nil, refused("there is no question %s in this workstream", id)
	case existing[i].State == QuestionAnswered:
		return nil, refused("question %s is already answered", id)
	case existing[i].State != QuestionRuled:
		return nil, refused("question %s has no ruling from the owner to relay", id)
	case !present(text):
		return nil, refused("text is required: the owner's ruling as the askers should read it")
	case reach != ScopeLocal && reach != ScopeNotify:
		return nil, refused("scope must be %s, for the askers only, or %s, for a notice to the whole project", ScopeLocal, ScopeNotify)
	}
	batch := existing[i].Latest.Escalation.Questions
	var relayed []Record
	var txs []Transaction
	for _, q := range existing {
		if !slices.Contains(batch, q.Asked.ID) {
			continue
		}
		next := *q.Ruling
		next.Revision++
		next.At, next.Actor, next.Cause, next.Depth = at, Actor{Kind: "agent", ID: agent}, turn.Request.ID, turn.Request.Depth+1
		next.ReturnedAnswer, next.Scope = text, reach
		if err := validate(next); err != nil {
			return nil, err
		}
		relayed = append(relayed, next)
		txs = append(txs, questionTransition(v, next.Header, q.Asked.ID, QuestionRuled, QuestionAnswered, fmt.Sprintf("The chief of staff relayed the owner's ruling on question %s with scope %s", q.Asked.ID, reach)))
	}
	files, _, err := r.stage(stream, log, v, relayed, txs...)
	if err != nil {
		return nil, err
	}
	if err := r.publish(ctx, files); err != nil {
		return nil, err
	}
	_ = r.wake.Notify(context.Background())
	return slices.Clone(batch), nil
}
