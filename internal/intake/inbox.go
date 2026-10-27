package intake

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/events"
	"github.com/kpenfound/osmia/internal/readview"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
)

// inbox lists the open owner decisions of every active project, oldest
// first, or none when no project or trace is active. Decisions of abandoned
// workstreams are left out. A cross-repo workgroup's own consolidated
// escalations, recorded only in its own trace, are listed exactly once,
// never duplicated by each of its real members' own project, and never
// merged with either member's own unrelated escalations: each project's
// own inbox counter is independent of the workgroup's.
func (s *Controller) Inbox(ctx context.Context) (InboxResponse, *APIError) {
	cfg, projects := s.Runtimes()
	out := InboxResponse{Entries: []InboxEntry{}}
	seenWorkgroup := map[config.WorkgroupID]bool{}
	for _, active := range projects {
		if !cfg.Active(active.ID) {
			continue
		}
		// projectInbox makes many separate repository and runtime-store
		// reads per workstream (statuses, escalations, operations, outbox,
		// the loop guard's pause, ...). readview.Consistent retries the whole
		// project's inbox from scratch when a commit or runtime mutation
		// lands partway through, so the response never mixes entries read
		// from one trace or runtime generation with entries read from
		// another.
		entries, err := readview.Consistent(func() ([]InboxEntry, error) {
			es, api := s.ProjectInbox(ctx, active)
			if api != nil {
				return nil, ApiErrAsError{api}
			}
			return es, nil
		}, active.Repository.Generation, s.StoreGeneration)
		if err != nil {
			var wrapped ApiErrAsError
			if errors.As(err, &wrapped) {
				return InboxResponse{}, wrapped.Api
			}
			return InboxResponse{}, &APIError{Code: Internal, Message: err.Error()}
		}
		out.Entries = append(out.Entries, entries...)

		ids, err := s.CrossRepoWorkgroupIDs(ctx, active.Repository)
		if err != nil {
			return InboxResponse{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot resolve the cross-repo workgroups of project %s; check the trace repository", active.ID)}
		}
		for _, id := range ids {
			if seenWorkgroup[id] {
				continue
			}
			seenWorkgroup[id] = true
			entries, api := s.WorkgroupInbox(ctx, active.Repository, id)
			if api != nil {
				return InboxResponse{}, api
			}
			out.Entries = append(out.Entries, entries...)
		}
	}
	slices.SortStableFunc(out.Entries, func(a, b InboxEntry) int { return a.OpenedAt.Compare(b.OpenedAt) })
	return out, nil
}

// crossRepoWorkgroupIDs returns, in encounter order, the distinct
// fully joined cross-repo workgroup IDs repository's workstreams point to.
func (s *Controller) CrossRepoWorkgroupIDs(ctx context.Context, repository *trace.Repository) ([]config.WorkgroupID, error) {
	streams, err := repository.Workstreams()
	if err != nil {
		return nil, err
	}
	var ids []config.WorkgroupID
	seen := map[config.WorkgroupID]bool{}
	for _, a := range s.MemberArchives() {
		if a.Project != repository.Project() || a.Workgroup == "" || seen[a.Workgroup] {
			continue
		}
		seen[a.Workgroup] = true
		_, group, _, err := repository.CrossRepoProjectSpec(a.Workgroup)
		if err != nil {
			return nil, err
		}
		if _, err := s.CrossRepoMemberRepos(ctx, group); errors.Is(err, ErrWorkgroupJoining) {
			continue
		} else if err != nil {
			return nil, err
		}
		ids = append(ids, a.Workgroup)
	}
	for _, stream := range streams {
		id, err := repository.CrossRepoWorkgroupOf(stream)
		if err != nil {
			return nil, err
		}
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		_, group, _, err := repository.CrossRepoProjectSpec(id)
		if err != nil {
			return nil, err
		}
		if _, err := s.CrossRepoMemberRepos(ctx, group); errors.Is(err, ErrWorkgroupJoining) {
			continue
		} else if err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// workgroupInbox lists the open consolidated escalations of the cross-repo
// workgroup id, recorded only in its own trace, opened and closed for this
// call alone through known, one of its real members' own already open
// repository, used only to resolve the service root. Every entry is tagged
// with the workgroup's own synthetic project identity, which
// workgroupInboxRepository resolves back to this same trace when the owner
// answers it, so the entry is never confused with either real member's own,
// independently numbered inbox.
func (s *Controller) WorkgroupInbox(ctx context.Context, known *trace.Repository, id config.WorkgroupID) ([]InboxEntry, *APIError) {
	failed := &APIError{Code: Internal, Message: fmt.Sprintf("cannot read the inbox of workgroup %s; check the trace repository", id)}
	_, g, _, err := known.CrossRepoProjectSpec(id)
	if err != nil {
		return nil, failed
	}
	if len(g.Members) < 2 {
		return nil, nil
	}
	members, err := s.CrossRepoMemberRepos(ctx, g)
	if errors.Is(err, ErrWorkgroupJoining) {
		return nil, nil
	}
	if err != nil {
		return nil, failed
	}
	wt, err := trace.OpenWorkgroupTrace(s.Current().Root, id, members)
	if wt != nil && err != nil {
		wt.Close()
	}
	if err != nil {
		return nil, failed
	}
	defer wt.Close()
	escalations, err := wt.Inbox()
	if err != nil {
		return nil, failed
	}
	var out []InboxEntry
	for _, e := range escalations {
		if e.State != trace.QuestionEscalated && e.Hold == "" {
			continue
		}
		entry := InboxView(e)
		entry.Workgroup = id
		entry.Project = wt.Project()
		entry.Answer.Body["project"] = string(wt.Project())
		out = append(out, entry)
	}
	return out, nil
}

// projectInbox lists the open owner decisions of one project.
func (s *Controller) ProjectInbox(ctx context.Context, active *Project) ([]InboxEntry, *APIError) {
	out := InboxResponse{Entries: []InboxEntry{}}
	failed := &APIError{Code: Internal, Message: fmt.Sprintf("cannot read the inbox of project %s; check the trace repository", active.ID)}
	repository := active.Repository
	statuses, err := repository.Statuses()
	if err != nil {
		return nil, failed
	}
	escalations, err := repository.Inbox()
	if err != nil {
		return nil, failed
	}
	gone := map[config.WorkstreamID]bool{}
	for _, w := range statuses {
		gone[w.Workstream] = w.State == AbandonedState
	}
	for _, e := range escalations {
		if e.State == trace.QuestionEscalated && !gone[e.Workstream] {
			out.Entries = append(out.Entries, InboxView(e))
		}
	}
	for _, w := range statuses {
		if gone[w.Workstream] || w.Workstream == LibrarianWorkstream(active.ID) {
			continue
		}
		entries, err := s.OpenDecisions(ctx, repository, w)
		if err == nil {
			err = s.Step("inbox-decisions")
		}
		if err != nil {
			return nil, failed
		}
		out.Entries = append(out.Entries, entries...)
	}
	for i := range out.Entries {
		e := &out.Entries[i]
		e.Project = active.ID
		if e.Kind == InboxEscalation {
			e.Answer.Body["project"] = string(active.ID)
		}
	}
	return out.Entries, nil
}

func InboxView(e trace.InboxEntry) InboxEntry {
	if e.Hold != "" {
		e.Blocked += "; " + e.Hold
	}
	out := InboxEntry{Kind: InboxEscalation, Number: e.Number, Workstream: e.Workstream, Batch: e.Batch, Question: e.Rephrasing, Blocked: e.Blocked,
		Options: append([]string{}, e.Options...), Recommendation: e.Recommendation, OpenedAt: e.EscalatedAt, Asked: []InboxQuestion{},
		Answer: InboxAnswer{Method: http.MethodPost, Path: fmt.Sprintf("%s/inbox/%d", Prefix, e.Number), Body: map[string]any{}}}
	if e.State == trace.QuestionEscalated && len(e.Questions) > 0 && EligibleQuickReply(e.Recommendation) {
		out.QuickReply = e.Recommendation
	}
	for _, q := range e.Questions {
		out.Asked = append(out.Asked, InboxQuestion{ID: q.Asked.ID, AskedBy: q.Asked.AskedBy.ID, Unit: q.Asked.Unit, Question: q.Asked.Question})
	}
	return out
}

// decision starts an inbox entry of a kind other than an escalation.
func Decision(kind string, stream config.WorkstreamID, opened time.Time, path string, body map[string]any) InboxEntry {
	return InboxEntry{Kind: kind, Workstream: stream, OpenedAt: opened, Options: []string{}, Asked: []InboxQuestion{},
		Answer: InboxAnswer{Method: http.MethodPost, Path: Prefix + path, Body: body}}
}

// openDecisions lists a workstream's open decisions other than its
// escalations: its ratification packet, contested units, presented
// amendments, held drift rebase, delivery approval, failing publication,
// final checks that do not complete, parked base, notices held from the chief of staff and a loop guard pause.
func (s *Controller) OpenDecisions(ctx context.Context, repository *trace.Repository, w trace.WorkstreamStatus) ([]InboxEntry, error) {
	var out []InboxEntry
	add := func(e InboxEntry, open bool, err error) error {
		if open {
			out = append(out, e)
		}
		return err
	}
	if err := add(s.LegacyProjectsEntry(repository, w.Workstream)); err != nil {
		return nil, err
	}
	if w.State == InShedState || w.State == SketchedState {
		if err := add(RatificationEntry(repository, w.Workstream)); err != nil {
			return nil, err
		}
	}
	for _, gate := range w.Gates {
		if gate.Kind != UnitContested {
			continue
		}
		if err := add(s.ContestedEntry(repository, w.Workstream, gate.Reference)); err != nil {
			return nil, err
		}
	}
	if w.State == BuildingState || w.State == AssembledState {
		var ids []int
		for subject, state := range w.Subjects {
			id, ok := strings.CutPrefix(subject, AmendmentSubject(""))
			if n, err := strconv.Atoi(id); ok && err == nil && state.Value == AmendmentPresented {
				ids = append(ids, n)
			}
		}
		slices.Sort(ids)
		for _, id := range ids {
			if err := add(s.AmendmentEntry(repository, w.Workstream, strconv.Itoa(id))); err != nil {
				return nil, err
			}
		}
		if err := add(s.DriftEntry(repository, w)); err != nil {
			return nil, err
		}
	}
	if w.State == AssembledState || w.State == DeliveredState {
		if err := add(s.DeliveryEntry(ctx, repository, w.Workstream)); err != nil {
			return nil, err
		}
		if err := add(s.PublicationEntry(repository, w)); err != nil {
			return nil, err
		}
	}
	if w.State == AssembledState {
		if err := add(s.FinalChecksEntry(repository, w)); err != nil {
			return nil, err
		}
	}
	if w.State != DeliveredState {
		if err := add(s.BaseEntry(repository, w)); err != nil {
			return nil, err
		}
	}
	if err := add(HeldNoticesEntry(repository, w.Workstream)); err != nil {
		return nil, err
	}
	if e, open := s.LoopEntry(repository, w.Workstream); open {
		out = append(out, e)
	}
	return out, nil
}

// loopEntry is the loop guard's pause of the workstream while it holds,
// opened when it was set. Resuming the workstream answers it.
func (s *Controller) LoopEntry(repository *trace.Repository, stream config.WorkstreamID) (InboxEntry, bool) {
	if s.Store == nil || s.Store() == nil {
		return InboxEntry{}, false
	}
	target := runtime.Target{Scope: "workstream", Project: repository.Project(), Workstream: stream}
	state, _ := s.Store().Snapshot()
	i := slices.IndexFunc(state.Pauses, func(p runtime.Pause) bool { return p.Target == target && p.Source == runtime.PauseLoopGuard })
	if i < 0 {
		return InboxEntry{}, false
	}
	pause := state.Pauses[i]
	e := Decision(InboxLoop, stream, pause.SetAt, "/runtime/pause", map[string]any{"scope": target.Scope, "project": target.Project, "workstream": target.Workstream})
	e.Answer.Method = http.MethodDelete
	e.Question = pause.Reason + "."
	e.Blocked = fmt.Sprintf("Everything on the workstream but the chief of staff. Look at its status and feed, then resume it with osmia resume %s; the count of sessions without progress starts over.", stream)
	return e, true
}

// heldNoticesEntry is the workstream's notices the chief of staff no longer
// receives because events.MaxDeliveryFailures turns delivering them failed,
// opened when the first of them was recorded. A message to the chief of
// staff answers it: once the chief completes a turn answering the owner,
// they are delivered again.
func HeldNoticesEntry(repository *trace.Repository, stream config.WorkstreamID) (InboxEntry, bool, error) {
	chief, err := repository.ChiefOfStaffThread(stream)
	if errors.Is(err, os.ErrNotExist) {
		return InboxEntry{}, false, nil
	}
	if err != nil {
		return InboxEntry{}, false, err
	}
	outbox, err := repository.Outbox(stream)
	if err != nil {
		return InboxEntry{}, false, err
	}
	held := 0
	var opened time.Time
	var last trace.QueuedTurn
	for _, e := range outbox {
		if e.Event.Operation != nil || e.Acknowledged {
			continue
		}
		turn, ok := events.Held(e, chief)
		if !ok {
			continue
		}
		held++
		if opened.IsZero() || e.At.Before(opened) {
			opened = e.At
		}
		if turn.CompletedAt.After(last.CompletedAt) {
			last = turn
		}
	}
	if held == 0 {
		return InboxEntry{}, false, nil
	}
	failure := "the turn failed"
	if last.Response != nil && last.Response.Failure != "" {
		failure = last.Response.Failure
	}
	e := Decision(InboxNotices, stream, opened, "/conversation/"+string(stream), map[string]any{})
	e.Question = fmt.Sprintf("%d notices for the chief of staff are held: the chief of staff's turns delivering them failed %d times in a row. The latest failure: %s", held, events.MaxDeliveryFailures, failure)
	e.Blocked = fmt.Sprintf("The chief of staff hearing what happened on the workstream. Send it a message with osmia send %s once the cause is fixed; when its answer completes, the held notices are delivered again.", stream)
	return e, true, nil
}

// publicationEscalation is how many consecutive failed attempts raise a
// publication to the inbox.
const PublicationEscalation = 3

// ratificationEntry is the workstream's ratification packet while debate
// rests at the decision it presents and the owner has not ratified it. The
// chief of staff decides every committee objection that stands first, so the
// entry is listed only once none waits for its decision. It offers ratify only
// while no objection blocks it.
func RatificationEntry(repository *trace.Repository, stream config.WorkstreamID) (InboxEntry, bool, error) {
	packet, doc, found, err := shed.LatestPacket(repository, stream)
	if err != nil || !found {
		return InboxEntry{}, false, err
	}
	state, err := repository.Workflow(stream, ShedSubject)
	if err != nil {
		return InboxEntry{}, false, err
	}
	if kind, round, ok := ShedState(state.Value); !packet.Skipped && (!ok || kind != "concluded" || round != packet.Round) {
		return InboxEntry{}, false, nil
	}
	ratified, recorded, err := LatestRatification(repository, stream)
	if err != nil {
		return InboxEntry{}, false, err
	}
	if recorded && ratified.Round == packet.Round && ratified.Revision == packet.Revision {
		return InboxEntry{}, false, nil
	}
	entries, err := Dissent(repository, stream)
	if err != nil {
		return InboxEntry{}, false, err
	}
	if len(shed.Undecided(entries)) > 0 {
		return InboxEntry{}, false, nil
	}
	e := Decision(InboxRatification, stream, doc.At, "/ratify/"+string(stream), map[string]any{"spec": packet.Revision.Spec, "plan": packet.Revision.Plan})
	e.Revision = doc.Revision
	e.Question = fmt.Sprintf("Ratify %s? Debate ended after round %d: %s", packet.Revision, packet.Round, packet.Conclusion)
	if packet.Skipped {
		e.Question = fmt.Sprintf("Ratify %s? Debate was skipped: %s", packet.Revision, packet.Conclusion)
	}
	e.Blocked = "Sealing the spec and plan, and building the workstream."
	if len(shed.Blocked(entries)) == 0 {
		e.Options = []string{"ratify"}
	}
	e.Recommendation = packet.Recommendation
	return e, true, nil
}

// amendmentEntry is a presented amendment at its latest packet revision, with
// the decisions that revision takes.
func (s *Controller) AmendmentEntry(repository *trace.Repository, stream config.WorkstreamID, id string) (InboxEntry, bool, error) {
	c, found, err := ReadAmendment(repository, stream, id)
	if err != nil || !found || c.State.Value != AmendmentPresented || c.Packet.Revision == 0 {
		return InboxEntry{}, false, err
	}
	var packet struct {
		Recommendation string `json:"recommendation"`
	}
	if err := json.Unmarshal([]byte(c.Packet.Content), &packet); err != nil {
		return InboxEntry{}, false, fmt.Errorf("%s revision %d: %w", c.Packet.Path, c.Packet.Revision, err)
	}
	records, err := AmendmentRecords(repository, stream, id)
	if err != nil {
		return InboxEntry{}, false, err
	}
	current, _, sealed, err := seal.Latest(repository, stream)
	if err != nil {
		return InboxEntry{}, false, err
	}
	matching := false
	if sealed {
		if matching, err = AmendmentSealMatches(repository, stream, c.Request.SealRevision, current); err != nil {
			return InboxEntry{}, false, err
		}
	}
	entries := AmendmentDissent(records)
	if sealed && s.EngineeringRevision(repository, stream, c, current, entries, "Assess delegated authority") == nil {
		return InboxEntry{}, false, nil
	}
	var options []string
	if matching && len(shed.Blocked(entries)) == 0 {
		options = append(options, AmendmentApprove)
	}
	if matching && len(entries) > 0 {
		options = append(options, AmendmentOverrule)
	}
	options = append(options, AmendmentReject)
	if c.Round < s.Current().Shed.MaxRounds {
		options = append(options, AmendmentRound)
	}
	e := Decision(InboxAmendment, stream, c.Packet.At, "/amendment/"+string(stream)+"/"+id, map[string]any{"packet": c.Packet.Revision})
	e.Amendment, e.Unit, e.Revision = id, c.Request.Unit, c.Packet.Revision
	e.Question = fmt.Sprintf("Amend the sealed spec and plan after debate round %d? Change: %s Reason: %s", c.Round, c.Request.Change, c.Request.Reason)
	e.Blocked = "The sealed spec and plan stay in force until the amendment is decided."
	if c.Request.Unit != "" {
		e.Blocked = fmt.Sprintf("Unit %s waits for the decision.", c.Request.Unit)
	}
	e.Options = options
	e.Recommendation = packet.Recommendation
	return e, true, nil
}

var ProhibitedQuickReplyTokens = map[string]bool{
	"push": true, "pushes": true, "pushed": true, "pushing": true,
	"merge": true, "merges": true, "merged": true, "merging": true,
	"deliver": true, "delivers": true, "delivered": true, "delivering": true,
	"abandon": true, "abandons": true, "abandoned": true, "abandoning": true,
	"force": true, "forces": true, "forced": true, "forcing": true,
	"delete": true, "deletes": true, "deleted": true, "deleting": true,
	"rebase": true, "rebases": true, "rebased": true, "rebasing": true,
	"overrule": true, "overrules": true, "overruled": true, "overruling": true,
	"deploy": true, "deploys": true, "deployed": true, "deploying": true,
	"ratify": true, "ratifies": true, "ratified": true, "ratifying": true,
	"veto": true, "vetoes": true, "vetoed": true, "vetoing": true,
	"revert": true, "reverts": true, "reverted": true, "reverting": true,
	"reset": true, "resets": true, "resetting": true,
	"discard": true, "discards": true, "discarded": true, "discarding": true,
}

func EligibleQuickReply(recommendation string) bool {
	if strings.TrimSpace(recommendation) == "" {
		return false
	}
	for _, token := range strings.FieldsFunc(strings.ToLower(recommendation), func(r rune) bool { return !unicode.IsLetter(r) }) {
		if ProhibitedQuickReplyTokens[token] {
			return false
		}
	}
	return true
}

// answer records the owner's ruling on the inbox entry raw numbers. It returns
// once the ruling and the chief of staff's notice of it are durable; relaying
// the ruling to the askers is the chief of staff's next turn. For an entry
// workgroupInbox tagged with a cross-repo workgroup's own synthetic project
// identity, the ruling is recorded in that workgroup's own trace, which
// resumes all and only the affected askers in their own repos the next time
// the shared chief of staff relays it; an ordinary project's own entry is
// unaffected.
func (s *Controller) Answer(ctx context.Context, raw string, req AnswerRequest) (AnswerResponse, *APIError) {
	number, err := strconv.Atoi(raw)
	if err != nil || number < 1 || strconv.Itoa(number) != raw {
		return AnswerResponse{}, &APIError{Code: Validation, Message: "inbox entry must be a number from osmia inbox"}
	}
	unknown := &APIError{Code: Validation, Message: fmt.Sprintf("there is no inbox entry %d; list the entries with osmia inbox", number)}
	var repository *trace.Repository
	if target, closeTarget, matched, api := s.WorkgroupInboxRepository(ctx, req.Project); matched {
		if api != nil {
			return AnswerResponse{}, api
		}
		defer closeTarget()
		repository = target
	} else {
		project, api := s.ProjectFor(req.Project)
		if api != nil {
			return AnswerResponse{}, api
		}
		_, active := s.RuntimeOf(project)
		if active == nil {
			return AnswerResponse{}, unknown
		}
		repository = active.Repository
	}
	if strings.TrimSpace(req.Text) == "" {
		return AnswerResponse{}, &APIError{Code: Validation, Message: "text must not be empty"}
	}
	return s.RuleInbox(ctx, repository, number, req.Text, unknown)
}

// workgroupInboxRepository resolves req.Project as a cross-repo workgroup's
// own synthetic project identity, when it is one. matched reports whether
// req.Project matched that pattern and a workgroup's own trace was found at
// it; the caller falls back to its own project resolution unchanged when
// matched is false. The caller must call close once done with target.
func (s *Controller) WorkgroupInboxRepository(ctx context.Context, project config.ProjectID) (target *trace.Repository, close func(), matched bool, api *APIError) {
	noop := func() {}
	id, ok := trace.WorkgroupProjectID(project)
	if !ok {
		return nil, noop, false, nil
	}
	_, projects := s.Runtimes()
	if len(projects) == 0 {
		return nil, noop, false, nil
	}
	_, g, _, err := projects[0].Repository.CrossRepoProjectSpec(id)
	if err != nil {
		s.Log().Error("cannot resolve inbox workgroup", "workgroup", id, "error", err.Error())
		return nil, noop, true, &APIError{Code: Internal, Message: fmt.Sprintf("cannot resolve workgroup %s; check the trace repository", id)}
	}
	if len(g.Members) < 2 {
		return nil, noop, false, nil
	}
	members, err := s.CrossRepoMemberRepos(ctx, g)
	if err != nil {
		return nil, noop, true, &APIError{Code: Internal, Message: fmt.Sprintf("cannot resolve the members of workgroup %s: %v", id, err)}
	}
	wt, err := trace.OpenWorkgroupTrace(s.Current().Root, id, members)
	if wt != nil && err != nil {
		wt.Close()
	}
	if err != nil {
		s.Log().Error("cannot open inbox workgroup", "workgroup", id, "error", err.Error())
		return nil, noop, true, &APIError{Code: Internal, Message: fmt.Sprintf("cannot open workgroup %s; check the trace repository", id)}
	}
	return wt, func() { wt.Close() }, true, nil
}

// ruleInbox records the owner's ruling on repository's own inbox entry
// number.
func (s *Controller) RuleInbox(ctx context.Context, repository *trace.Repository, number int, text string, unknown *APIError) (AnswerResponse, *APIError) {
	failed := &APIError{Code: Internal, Message: fmt.Sprintf("cannot record the ruling on inbox entry %d; check the trace repository", number)}
	at := s.Now()
	entry, err := repository.Rule(ctx, number, text, OwnerActor, at, AbandonedState)
	switch {
	case errors.Is(err, trace.ErrMemberRetired):
		return AnswerResponse{}, &APIError{Code: Conflict, Message: err.Error()}
	case entry.Hold != "" && err != nil:
		return AnswerResponse{}, &APIError{Code: Conflict, Message: entry.Hold}
	case errors.Is(err, trace.ErrInboxEntry):
		return AnswerResponse{}, unknown
	case errors.Is(err, trace.ErrFeatureState):
		return AnswerResponse{}, &APIError{Code: Conflict, Message: fmt.Sprintf("inbox entry %d belongs to abandoned workstream %s and takes no ruling", number, entry.Workstream)}
	case errors.Is(err, trace.ErrRuled):
		return AnswerResponse{}, &APIError{Code: Conflict, Message: fmt.Sprintf("inbox entry %d is already answered", number)}
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		s.Log().Warn("inbox ruling interrupted", "project", repository.Project(), "number", number, "error", err.Error())
		return AnswerResponse{}, &APIError{Code: Unavailable, Message: fmt.Sprintf("the ruling on inbox entry %d was interrupted; refresh the inbox and retry the same answer if it is still open", number)}
	case err != nil:
		s.Log().Error("cannot record inbox ruling", "project", repository.Project(), "number", number, "error", err.Error())
		return AnswerResponse{}, failed
	}
	out := AnswerResponse{Number: number, Workstream: entry.Workstream, Batch: entry.Batch, Questions: []string{}, Ruling: text, At: at}
	for _, q := range entry.Questions {
		out.Questions = append(out.Questions, q.Asked.ID)
	}
	return out, nil
}
