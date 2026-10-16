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
// sent the owner as one ask, which may span every member of a shared
// workgroup a genuine gap affects, consolidated into this one entry.
// Number is the escalation's inbox number, unique in the project and never
// reused. Questions holds the batch in the order it was escalated, and
// every one of them is in State. Workstream is the first question's own
// workstream.
type InboxEntry struct {
	Hold           string
	Number         int
	Workstream     config.WorkstreamID
	Batch          string
	Rephrasing     string
	Section        string
	Conflict       string
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
	// Past this write the file is already ahead of committed history; see
	// append's commitContent call for why the commit that reconciles it runs
	// without the caller's cancellation.
	if err := r.commit(context.WithoutCancel(ctx), []string{inboxCounterFile}, "Reserve inbox number"); err != nil {
		return 0, err
	}
	return next, nil
}

// Inbox returns every escalation of the project by inbox number, whatever
// became of it since.
func (r *Repository) Inbox() ([]InboxEntry, error) {
	return r.inbox()
}

// inbox aggregates every workstream's own escalated questions into one
// entry per escalation, by inbox number: a consolidated escalation spans
// every member of a shared workgroup a genuine gap affects, so its
// questions are recorded, one commit apiece, across each of their own
// workstreams, but the owner reads it, and answers it, as the one entry it
// is. The inbox number, not the batch ID, is the aggregation key: batch IDs
// are reserved per workstream, so two workstreams' own first escalations
// both carry batch "escalation_1", while the inbox number is reserved once
// per escalation from the project's own counter and is unique across the
// whole project.
func (r *Repository) inbox() ([]InboxEntry, error) {
	ids, err := r.diskStreams()
	if err != nil {
		return nil, err
	}
	byNumber := map[int]*InboxEntry{}
	var order []int
	for id := range r.memberBindings() {
		retired, err := r.MemberRetired(id)
		if err != nil {
			return nil, err
		}
		if retired {
			if !slices.Contains(ids, id) {
				ids = append(ids, id)
			}
		}
	}
	for _, id := range ids {
		if r.memberBindings() == nil {
			// A member of a cross-repo workgroup reserves its escalation's
			// inbox number from that workgroup's own counter, never this
			// project's own, even though the escalated question's revision
			// is committed into this project's own trace (scoped(member)
			// on the workgroup's own trace resolves there directly). Read
			// on the workgroup's own trace instance (r.members != nil),
			// the same number correctly aggregates that member's own
			// question with its fellow members'; read here, on this
			// project's own instance, the same raw number can collide
			// with an escalation this project's own counter issued, so a
			// member's own cross-repo escalation is left for
			// workgroupInbox to report instead.
			workgroup, err := r.CrossRepoWorkgroupOf(id)
			if err != nil {
				return nil, err
			}
			if workgroup != "" {
				continue
			}
		}
		states, _, err := r.questionHistory(id)
		if err != nil {
			return nil, fmt.Errorf("workstream %s: %w", id, err)
		}
		for _, q := range states {
			e := q.Latest.Escalation
			if e == nil {
				continue
			}
			entry, ok := byNumber[e.Inbox]
			if !ok {
				entry = &InboxEntry{Number: e.Inbox, Workstream: e.workstreamOf(0, id), Batch: e.Batch, Section: e.Section, Conflict: e.Conflict, Blocked: e.Blocked,
					Options: slices.Clone(e.Options), Recommendation: e.Recommendation, Questions: make([]QuestionState, len(e.Questions))}
				byNumber[e.Inbox] = entry
				order = append(order, e.Inbox)
			}
			for i := range e.Questions {
				if e.Questions[i] != q.Asked.ID || e.workstreamOf(i, id) != id {
					continue
				}
				entry.Questions[i] = q
				if i == 0 {
					entry.Rephrasing, entry.EscalatedAt, entry.State = q.Latest.SentToOwner, q.Latest.At, q.State
				}
			}
		}
	}
	out := make([]InboxEntry, 0, len(order))
	for _, n := range order {
		entry := byNumber[n]
		// A consolidated escalation's members each commit their own ruling
		// separately (Rule, one commit per workstream); a crash between two
		// of those commits must not make the batch read as "ruled" while one
		// of its own questions is still escalated; see Rule's own retry of
		// exactly such a member. Questions[0]'s own state, set above, is
		// right whenever every member moved together, so it stands unless
		// some other question of the same batch is still escalated.
		anyEscalated, anyRuled := false, false
		for _, q := range entry.Questions {
			if q.Asked.Workstream != "" {
				retired, err := r.MemberRetired(q.Asked.Workstream)
				if err != nil {
					return nil, err
				}
				if retired && q.State != QuestionAnswered {
					entry.Hold = ErrMemberRetired.Error()
				}
			}
			if q.Asked.ID == "" {
				entry.Hold = "original question history is unavailable; operation remains held"
				anyEscalated = true
			}
			anyEscalated = anyEscalated || q.State == QuestionEscalated
			anyRuled = anyRuled || q.State == QuestionRuled
		}
		if anyEscalated {
			entry.State = QuestionEscalated
		} else if anyRuled {
			entry.State = QuestionRuled
		}
		out = append(out, *entry)
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

// ruleInterrupt lets a test simulate the service stopping between two
// members' own ruling commits inside Rule, the same way
// ensureWorkgroupsInterrupt (internal/service) tests a crash mid-pass: it is
// called, when set, right after each member workstream's own commit, with
// the workstream just committed.
var ruleInterrupt func(config.WorkstreamID) error

// Rule records text as the owner's ruling on inbox entry number: revision 1 of
// the ruling of every not-yet-ruled question in the batch, each such
// question's move from escalated to ruled, in one commit per member
// workstream the batch touches, with one notice for the chief of staff
// overall. It returns the entry as it was ruled on. An unknown number fails
// with ErrInboxEntry, an entry whose workstream's feature state is one of
// refused with ErrFeatureState and the entry, and an entry that is no longer
// escalated (every member already ruled) with ErrRuled; all three write
// nothing.
//
// A consolidated escalation's members each commit their own ruling
// separately; if an earlier call stopped after ruling some members but
// before others, the entry still reads escalated (inbox, above) and a
// retry lands here again. The ruling notice is attached to whichever
// member's commit is the last to clear its own pending questions in a
// given call, never an earlier one, so a call that stops partway through
// sends no notice at all: the batch is not yet whole. Called again with the
// same text, it completes only the members not yet ruled, leaves the ones
// already ruled untouched, and sends the one notice exactly once, on the
// commit that finishes the batch. Called again with a different text while
// some members are already ruled, it is refused with ErrRuled rather than
// recording two different owner rulings on the one batch. The checks and
// the write hold the same lock. Text must not be blank.
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
	if entry.Hold != "" && entry.Hold != ErrMemberRetired.Error() {
		return entry, fmt.Errorf("%s", entry.Hold)
	}
	// A consolidated escalation's questions may belong to several of the
	// workgroup's own workstreams; each is its own trace repository, so the
	// ruling is recorded as one commit per distinct workstream the batch
	// touches. Every one of them is checked, in the same order the single-
	// workstream check always ran in (feature state before "already
	// ruled"), before any of them is written. A workstream already ruled by
	// an earlier, interrupted call to this same batch (its own questions no
	// longer escalated) is recorded here too, so a different retried text
	// can be caught, but it is left out of pending and never rewritten.
	byStream := map[config.WorkstreamID][]QuestionState{}
	var order []config.WorkstreamID
	var ids []string
	alreadyRuled := false
	priorText := ""
	for _, q := range entry.Questions {
		ws := q.Asked.Workstream
		if _, ok := byStream[ws]; !ok {
			order = append(order, ws)
		}
		byStream[ws] = append(byStream[ws], q)
		ids = append(ids, q.Asked.ID)
		if q.State != QuestionEscalated && q.Ruling != nil {
			if alreadyRuled && priorText != q.Ruling.OwnerResponse {
				return entry, fmt.Errorf("%w: batch contains different owner rulings", ErrRuled)
			}
			alreadyRuled, priorText = true, q.Ruling.OwnerResponse
		}
	}
	for _, ws := range order {
		retired, err := r.MemberRetired(ws)
		if err != nil {
			return entry, err
		}
		if retired {
			for _, q := range byStream[ws] {
				if q.State == QuestionEscalated || q.Ruling == nil {
					m, _ := r.memberBinding(ws)
					return entry, &RetiredMemberError{Project: m.Project.Project(), Workstream: ws}
				}
			}
			continue
		}
		sr, err := r.lockQuestionRead(ws)
		if err != nil {
			retired, checkedErr := r.retiredRead(ws, err)
			if !retired {
				return entry, checkedErr
			}
			states, snapshotErr := r.retiredQuestions(ws)
			if snapshotErr != nil {
				return entry, snapshotErr
			}
			for _, q := range byStream[ws] {
				found := slices.ContainsFunc(states, func(saved QuestionState) bool {
					return saved.Asked.ID == q.Asked.ID && saved.State != QuestionEscalated && saved.Ruling != nil && saved.Ruling.OwnerResponse == text
				})
				if !found || q.State == QuestionEscalated || q.Ruling == nil {
					return entry, err
				}
			}
			continue
		}
		_, v, err := sr.loadWorkflow(ws)
		sr.mu.Unlock()
		if err != nil {
			return InboxEntry{}, err
		}
		if slices.Contains(refused, v.states[FeatureSubject].Value) {
			return entry, ErrFeatureState
		}
	}
	if entry.State != QuestionEscalated {
		return InboxEntry{}, fmt.Errorf("%w: %d", ErrRuled, number)
	}
	if alreadyRuled && priorText != text {
		return InboxEntry{}, fmt.Errorf("%w: %d", ErrRuled, number)
	}
	// inbox() keeps an entry escalated while any of its own questions still
	// is, so reaching here means at least one member has a pending question.
	// Whichever member's commit is the last to clear its own pending
	// questions in this call is the one that moves the whole entry out of
	// "escalated"; the owner's one ruling notice belongs there; attaching it
	// to an earlier member's commit, as a crash between two members' own
	// commits can show, would announce the ruling before the batch is
	// actually whole. A retry that completes the batch always has a last
	// pending member of its own, so the notice always goes out exactly once,
	// on the call that actually finishes the batch.
	pendingByStream := map[config.WorkstreamID][]QuestionState{}
	var lastPending config.WorkstreamID
	for _, ws := range order {
		pending := slices.DeleteFunc(slices.Clone(byStream[ws]), func(q QuestionState) bool { return q.State != QuestionEscalated })
		if len(pending) == 0 {
			continue
		}
		pendingByStream[ws], lastPending = pending, ws
	}
	for _, ws := range order {
		pending, ok := pendingByStream[ws]
		if !ok {
			continue
		}
		sr, err := r.scoped(ws)
		if err != nil {
			return InboxEntry{}, err
		}
		if err := func() error {
			sr.mu.Lock()
			defer sr.mu.Unlock()
			retired, err := r.MemberRetired(ws)
			if err != nil {
				return err
			}
			if retired {
				m, _ := r.memberBinding(ws)
				return &RetiredMemberError{Project: m.Project.Project(), Workstream: ws}
			}
			log, v, err := sr.loadWorkflow(ws)
			if err != nil {
				return err
			}
			var rulings []Record
			var txs []Transaction
			for _, q := range pending {
				id := q.Asked.ID
				h := Header{Schema: "osmia.trace.ruling", Version: Version, ID: id, Revision: 1, Project: sr.project, Workstream: ws, Unit: q.Asked.Unit,
					At: at, Actor: owner, Cause: QuestionSubject(id) + "_" + QuestionEscalated, Depth: q.Latest.Depth + 1}
				ruling := Ruling{Header: h, QuestionID: id, QuestionRevision: q.Latest.Revision, Decision: DecisionRuling, OwnerResponse: text}
				if err := validate(ruling); err != nil {
					return err
				}
				rulings = append(rulings, ruling)
				txs = append(txs, questionTransition(v, h, id, QuestionEscalated, QuestionRuled, fmt.Sprintf("The owner ruled on question %s, inbox entry %d", id, number)))
			}
			if ws == lastPending {
				txs[0].Events = []Event{Notice(txs[0].Transition.ID, "ruling", fmt.Sprintf("The owner ruled on inbox entry %d, %s (questions %s): %s", number, entry.Batch, strings.Join(ids, ", "), strings.TrimSpace(text)))}
			}
			files, _, err := sr.stage(ws, log, v, rulings, txs...)
			if err != nil {
				return err
			}
			return sr.publish(ctx, files)
		}(); err != nil {
			return InboxEntry{}, err
		}
		_ = sr.wake.Notify(context.Background())
		if err := r.afterQuestionCommit(ws, "owner-ruling-member"); err != nil {
			return entry, err
		}
		if ruleInterrupt != nil {
			if err := ruleInterrupt(ws); err != nil {
				return InboxEntry{}, err
			}
		}
	}
	return entry, nil
}

// RelayRuling records what goes back to the askers of a ruled question's
// batch: revision 2 of every ruling in it, with text as the returned answer and
// the scope, and each question's move from ruled to answered, in one commit
// per distinct member workstream the batch touches. id names a question of
// the turn's own workstream when unqualified, or "<workstream>:<id>" one of
// any other member of the shared workgroup; the relay is recorded in that
// question's own workstream. It returns the batch's questions. A turn of
// another role fails. A question that is neither ruled nor already
// answered as part of this same batch's own relay, an empty text and a
// scope other than ScopeLocal and ScopeNotify are refused with
// *QuestionRefused.
//
// A crash between two of those commits leaves some members answered and
// others still ruled; a retry lands here again. It skips a member already
// answered with the same text, leaving it untouched, and completes the
// rest. A different text on an already-answered member, while some other
// member is still ruled, is refused: a resume in progress must finish with
// the one text the owner's ruling was relayed as. Once every member of the
// batch is already answered, calling this again is refused with "already
// answered" regardless of text, matching a single-workstream relay's
// always having had no second attempt to resume.
func (r *Repository) RelayRuling(ctx context.Context, agent string, scope coreadapter.Scope, id, text, reach string, at time.Time) ([]string, error) {
	project := r
	member, localID, err := r.ChiefQuestionTarget(config.WorkstreamID(scope.Workstream), id)
	if err != nil {
		return nil, err
	}
	hr, turn, err := project.chiefTurnOn(ctx, agent, scope, config.WorkstreamID(scope.Workstream), at)
	if err != nil {
		return nil, err
	}
	hr.mu.Unlock()
	stream := member
	existing, _, err := project.questionHistory(member)
	if err != nil {
		return nil, err
	}
	i := slices.IndexFunc(existing, func(q QuestionState) bool { return q.Asked.ID == localID })
	switch {
	case i < 0:
		return nil, refused("there is no question %s in this workstream", localID)
	case existing[i].State != QuestionRuled && existing[i].State != QuestionAnswered:
		return nil, refused("question %s has no ruling from the owner to relay", localID)
	case !present(text):
		return nil, refused("text is required: the owner's ruling as the askers should read it")
	case reach != ScopeLocal && reach != ScopeNotify:
		return nil, refused("scope must be %s, for the askers only, or %s, for a notice to the whole project", ScopeLocal, ScopeNotify)
	}
	escalation := existing[i].Latest.Escalation
	if escalation == nil {
		// Answered directly, through answer_question, with no ruling batch
		// at all: already fully resolved, the same as a batch every one of
		// whose members is already answered.
		return nil, refused("question %s is already answered", localID)
	}
	batch := slices.Clone(escalation.Questions)
	byStream := map[config.WorkstreamID][]string{}
	var order []config.WorkstreamID
	for idx, qid := range batch {
		ws := escalation.workstreamOf(idx, stream)
		if _, ok := byStream[ws]; !ok {
			order = append(order, ws)
		}
		byStream[ws] = append(byStream[ws], qid)
	}
	// The batch's questions may belong to several of the workgroup's own
	// workstreams, each its own trace repository. A first pass across every
	// member the batch touches, none of its own locks held while another
	// member's is, finds which of them are still ruled (pending this call's
	// own relay) and which are already answered from an earlier, interrupted
	// call to this same batch; an already-answered member with a different
	// text, while some other member is still pending, is caught here rather
	// than attempting to redo a ruled-to-answered transition that already
	// happened and cannot run twice.
	pendingByStream := map[config.WorkstreamID][]string{}
	allAnswered := true
	var held error
	mismatch := ""
	for _, ws := range order {
		states, retired, err := project.questionHistory(ws)
		if err != nil {
			return nil, err
		}
		var pending []string
		if err == nil {
			for _, qid := range byStream[ws] {
				qi := slices.IndexFunc(states, func(q QuestionState) bool { return q.Asked.ID == qid })
				if qi < 0 || states[qi].Ruling == nil {
					if retired {
						m, _ := project.memberBinding(ws)
						return nil, &RetiredMemberError{Project: m.Project.Project(), Workstream: ws}
					}
					err = refused("question %s has no ruling from the owner to relay", qid)
					break
				}
				if states[qi].State == QuestionAnswered {
					if (states[qi].Ruling.ReturnedAnswer != text || states[qi].Ruling.Scope != reach) && mismatch == "" {
						mismatch = qid
					}
					continue
				}
				if retired {
					if states[qi].State != QuestionRuled {
						return nil, ErrConflict
					}
					m, _ := project.memberBinding(ws)
					held = &RetiredMemberError{Project: m.Project.Project(), Workstream: ws}
					continue
				}
				allAnswered, pending = false, append(pending, qid)
			}
		}
		if err != nil {
			return nil, err
		}
		if len(pending) > 0 {
			pendingByStream[ws] = pending
		}
	}
	if allAnswered && held != nil {
		return nil, held
	}
	if allAnswered {
		return nil, refused("question %s is already answered", localID)
	}
	if mismatch != "" {
		return nil, refused("question %s is already answered with a different ruling", mismatch)
	}
	// The relay is recorded as one commit per distinct workstream the batch
	// still has pending; a member already answered above is left untouched.
	for _, ws := range order {
		pending, ok := pendingByStream[ws]
		if !ok {
			continue
		}
		if err := func() error {
			tr, err := project.scoped(ws)
			if err != nil {
				return err
			}
			tr.mu.Lock()
			defer tr.mu.Unlock()
			retired, err := project.MemberRetired(ws)
			if err != nil {
				return err
			}
			if retired {
				m, _ := project.memberBinding(ws)
				return &RetiredMemberError{Project: m.Project.Project(), Workstream: ws}
			}
			log, v, err := tr.loadWorkflow(ws)
			if err != nil {
				return err
			}
			records, _, err := tr.scan()
			if err != nil {
				return err
			}
			existing := questions(records, v, ws)
			var relayed []Record
			var txs []Transaction
			for _, qid := range pending {
				qi := slices.IndexFunc(existing, func(q QuestionState) bool { return q.Asked.ID == qid })
				if qi < 0 || existing[qi].Ruling == nil {
					return refused("question %s has no ruling from the owner to relay", qid)
				}
				next := *existing[qi].Ruling
				next.Revision++
				next.At, next.Actor, next.Cause, next.Depth = at, Actor{Kind: "agent", ID: agent}, turn.Request.ID, turn.Request.Depth+1
				next.ReturnedAnswer, next.Scope = text, reach
				if err := validate(next); err != nil {
					return err
				}
				relayed = append(relayed, next)
				txs = append(txs, questionTransition(v, next.Header, qid, QuestionRuled, QuestionAnswered, fmt.Sprintf("The chief of staff relayed the owner's ruling on question %s with scope %s", qid, reach)))
			}
			files, _, err := tr.stage(ws, log, v, relayed, txs...)
			if err != nil {
				return err
			}
			if err := tr.publish(ctx, files); err != nil {
				return err
			}
			_ = tr.wake.Notify(context.Background())
			return nil
		}(); err != nil {
			return nil, err
		}
		if err := project.afterQuestionCommit(ws, "chief-relay-member"); err != nil {
			return nil, err
		}
		if relayInterrupt != nil {
			if err := relayInterrupt(ws); err != nil {
				return nil, err
			}
		}
	}
	return batch, held
}

// relayInterrupt lets a test simulate the service stopping between two
// members' own relay commits inside RelayRuling, the same way ruleInterrupt
// tests a crash inside Rule: it is called, when set, right after each
// member workstream's own commit, with the workstream just committed.
var relayInterrupt func(config.WorkstreamID) error
