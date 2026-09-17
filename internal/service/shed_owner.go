package service

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/kb"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
)

// ownerEdits records the owner's edits to spec.md and plan.json as new owner
// revisions before any turn reads them, and returns the latest recorded
// revisions of both. The two are one draft: an edit that leaves them invalid
// is not recorded, neither file included, the latest recorded revisions stay
// the ones under debate, and the problems are reported once to the
// workstream's chief of staff.
func (d *debate) ownerEdits(ctx context.Context, stream config.WorkstreamID) (shed.Pin, error) {
	// The check runs while the trace is held, so the entity map it validates
	// against is loaded before the read.
	entities, err := kb.Load(d.repository)
	if err != nil {
		return shed.Pin{}, err
	}
	docs, err := d.repository.OwnerDocuments(ctx, stream, d.s.now(), func(content map[string]string) error {
		if problems := validateDraft(content[plan.SpecDocument], content[plan.PlanDocument], entities); len(problems) > 0 {
			return errors.New(strings.Join(problems, "; "))
		}
		return nil
	})
	if err != nil && !errors.Is(err, trace.ErrOwnerEdit) {
		return shed.Pin{}, err
	}
	if err != nil {
		if err := d.reportInvalidEdit(ctx, stream, err.Error()); err != nil {
			return shed.Pin{}, err
		}
	}
	return shed.Pin{Spec: docs[plan.SpecDocument].Revision, Plan: docs[plan.PlanDocument].Revision}, nil
}

// reportInvalidEdit tells the chief of staff why an owner edit is not
// debated. The transition is identified by the problems it reports, so each
// distinct problem is reported once however many passes read the same edit.
func (d *debate) reportInvalidEdit(ctx context.Context, stream config.WorkstreamID, problems string) error {
	id := fmt.Sprintf("shed-owner-invalid-%x", sha256.Sum256([]byte(problems)))[:32]
	transitions, err := trace.Read[trace.Transition](d.repository, stream)
	if err != nil {
		return err
	}
	if slices.ContainsFunc(transitions, func(t trace.Transition) bool { return t.ID == id }) {
		return nil
	}
	state, err := d.repository.Workflow(stream, ownerSubject)
	if err != nil {
		return err
	}
	reason := "an owner edit is not recorded and not debated: " + problems
	tx := trace.Transaction{ExpectedVersion: state.Version,
		Transition: trace.Transition{Header: ownerHeader(id, d.repository.Project(), stream, "owner-edit", d.s.now()), Subject: ownerSubject, From: state.Value, To: invalidEditValue, Reason: reason},
		Events: []trace.Event{trace.Notice(id, invalidEditValue, "Your edit of the spec or the plan is not recorded, and the committee keeps debating the revisions that are: "+problems+
			"\nCorrect the file to have it debated. Until then the architect gives up a redraft of what you edited rather than write over it.")}}
	// Another writer of the subject moved it; the next pass reports the edit.
	if _, err := d.repository.Transact(ctx, tx); err != nil && !errors.Is(err, trace.ErrConflict) {
		return err
	}
	return nil
}

func ownerHeader(id string, project config.ProjectID, stream config.WorkstreamID, cause string, at time.Time) trace.Header {
	return trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: project, Workstream: stream, At: at, Actor: ownerActor, Cause: cause}
}

// shedOwner is one owner action on one workstream's shed: the workstream, its
// trace, the state the action found and the revision it is recorded against.
type shedOwner struct {
	project    config.ProjectID
	stream     config.WorkstreamID
	repository *trace.Repository
	feature    string
	shed       trace.WorkflowState
	owner      trace.WorkflowState
	skipped    bool
	round      int
	pin        shed.Pin
	now        func() time.Time
}

// shedAction reads what every owner action of the shed needs and refuses a
// workstream that is not in the shed. The current round is the round the
// debate has reached, or the first round, which is what the owner's action is
// recorded under.
func (s *Service) shedAction(raw string, states ...string) (*shedOwner, *APIError) {
	project, stream, repository, api := s.conversationTrace(raw)
	if api != nil {
		return nil, api
	}
	failed := &APIError{Internal, fmt.Sprintf("cannot read the shed of workstream %s; check the trace repository", stream)}
	feature, err := repository.Workflow(stream, trace.FeatureSubject)
	if err != nil {
		return nil, failed
	}
	if !slices.Contains(states, feature.Value) {
		return nil, &APIError{Conflict, fmt.Sprintf("workstream %s is %s; the owner takes part in the shed while it is %s", stream, featureState(feature.Value), strings.Join(states, " or "))}
	}
	state, err := repository.Workflow(stream, shedSubject)
	if err != nil {
		return nil, failed
	}
	owner, err := repository.Workflow(stream, ownerSubject)
	if err != nil {
		return nil, failed
	}
	skipped, err := skippedDebate(repository, stream)
	if err != nil {
		return nil, failed
	}
	round := 1
	if _, n, ok := shedState(state.Value); ok {
		round = n
	}
	pin, err := latestPin(repository, stream)
	if err != nil {
		return nil, &APIError{Conflict, fmt.Sprintf("workstream %s has no recorded spec and plan yet", stream)}
	}
	return &shedOwner{project: project, stream: stream, repository: repository, feature: feature.Value, shed: state, owner: owner, skipped: skipped, round: round, pin: pin, now: s.now}, nil
}

// skippedDebate reports whether the owner has skipped debate on the
// workstream. The skip is the recorded transition, not the owner subject's
// current value: the subject shows the owner's latest action, and a later
// objection, ruling or reported edit moves it without un-skipping anything.
func skippedDebate(repository *trace.Repository, stream config.WorkstreamID) (bool, error) {
	transitions, err := trace.Read[trace.Transition](repository, stream)
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(transitions, func(t trace.Transition) bool { return t.ID == skipTransition }), nil
}

// featureState names a feature state for a message, including the state of a
// workstream that has none.
func featureState(value string) string {
	if value == "" {
		return "not started"
	}
	return value
}

// document returns the latest recorded revision of one of the workstream's
// shed files, and whether it is recorded.
func (o *shedOwner) document(path string) (trace.Document, bool, error) {
	docs, err := trace.Read[trace.Document](o.repository, o.stream)
	if err != nil {
		return trace.Document{}, false, err
	}
	var latest trace.Document
	found := false
	for _, d := range docs {
		if d.Path == path {
			latest, found = d, true
		}
	}
	return latest, found, nil
}

// record writes one revision of an owner file of the shed and then records
// the transition of the owner subject that names it. The file is the debate's
// input and the transition its provenance, so the file is committed first: an
// interruption between the two leaves the owner's action recorded and
// readable, never a transition that names a file that is not there.
func (o *shedOwner) record(ctx context.Context, id, path, content, value, reason string) *APIError {
	latest, found, err := o.document(path)
	failed := &APIError{Internal, fmt.Sprintf("cannot record the owner's shed action on workstream %s; check the trace repository", o.stream)}
	if err != nil {
		return failed
	}
	revision := 1
	if found {
		revision = latest.Revision + 1
	}
	doc := trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: id, Revision: revision, Project: o.project, Workstream: o.stream, At: o.now(), Actor: ownerActor, Cause: "owner-shed"}, Path: path, Content: content}
	if err := o.repository.RecordDocuments(ctx, []trace.Document{doc}); err != nil {
		return failed
	}
	transition := fmt.Sprintf("%s-%d", id, revision)
	tx := trace.Transaction{ExpectedVersion: o.owner.Version,
		Transition: trace.Transition{Header: ownerHeader(transition, o.project, o.stream, "owner-shed", o.now()), Subject: ownerSubject, From: o.owner.Value, To: value, Reason: reason},
		Events:     []trace.Event{trace.Notice(transition, "owner", reason)}}
	if _, err := o.repository.Transact(ctx, tx); err != nil {
		if errors.Is(err, trace.ErrConflict) {
			return &APIError{Conflict, fmt.Sprintf("workstream %s changed while recording the action; check osmia status and retry", o.stream)}
		}
		return failed
	}
	return nil
}

// shedObject records the owner's own objection to the current round. It is
// the owner's, blocks like a member's, and the architect answers it in its
// reply to the round.
func (s *Service) shedObject(ctx context.Context, raw string, req ShedObjectRequest) (ShedResponse, *APIError) {
	o, api := s.shedAction(raw, InShedState)
	if api != nil {
		return ShedResponse{}, api
	}
	argument := strings.TrimSpace(req.Argument)
	if argument == "" {
		return ShedResponse{}, &APIError{Validation, "an objection requires an argument"}
	}
	if o.skipped {
		return ShedResponse{}, &APIError{Conflict, fmt.Sprintf("the owner skipped debate on workstream %s; nothing answers a new objection", o.stream)}
	}
	records, err := shed.Records(o.repository, o.stream)
	if err != nil {
		return ShedResponse{}, &APIError{Internal, fmt.Sprintf("cannot read the shed records of workstream %s; check the trace repository", o.stream)}
	}
	// A round's file keeps the revision it was opened against, as a member's
	// record keeps the revision its round was pinned to.
	record := shed.Record{Version: shed.Version, Round: o.round, Member: shed.OwnerMember, Revision: o.pin}
	for _, r := range records {
		if r.Owned() && r.Round == o.round {
			record = r
		}
	}
	objection := shed.Objection{ID: shed.ObjectionID(o.round, shed.OwnerMember, len(record.Objections)+1), Kind: shed.Owner, Argument: argument}
	record.Objections = append(slices.Clone(record.Objections), objection)
	content, err := shed.Encode(record)
	if err != nil {
		return ShedResponse{}, &APIError{Validation, "the objection cannot be recorded: " + err.Error()}
	}
	reason := fmt.Sprintf("the owner objected to round %d against %s: %s", o.round, record.Revision, argument)
	if api := o.record(ctx, shed.DocumentID(o.round, shed.OwnerMember), shed.Path(o.round, shed.OwnerMember), string(content), fmt.Sprintf("objected-%d", o.round), reason); api != nil {
		return ShedResponse{}, api
	}
	return ShedResponse{Project: o.project, Workstream: o.stream, Round: o.round, Objection: objection.ID, Action: "objected", Detail: reason}, nil
}

// shedRule records the owner's ruling on one objection that stands: sustained,
// which blocks until it is conceded, or dismissed, which is kept as the
// owner's disposition and blocks no longer.
func (s *Service) shedRule(ctx context.Context, raw string, req ShedRuleRequest) (ShedResponse, *APIError) {
	o, api := s.shedAction(raw, InShedState)
	if api != nil {
		return ShedResponse{}, api
	}
	disposition, ok := shed.ParseDisposition(req.Disposition)
	if !ok {
		return ShedResponse{}, &APIError{Validation, "a ruling is sustain or dismiss"}
	}
	return s.dispose(ctx, o, req.Objection, disposition, req.Note, "ruled")
}

// shedOverrule records that the owner decided to proceed in spite of one
// objection that stands, charter vetoes included. It is a disposition of the
// same kind as a ruling, recorded against the revision the documents are at,
// and the objection blocks no longer.
func (s *Service) shedOverrule(ctx context.Context, raw string, req ShedOverruleRequest) (ShedResponse, *APIError) {
	o, api := s.shedAction(raw, InShedState)
	if api != nil {
		return ShedResponse{}, api
	}
	return s.dispose(ctx, o, req.Objection, shed.Overruled, req.Reason, "overruled")
}

// dispose records one disposition of one objection that stands in the owner's
// rulings of the current round. Disposing of the same objection again replaces
// the earlier disposition.
func (s *Service) dispose(ctx context.Context, o *shedOwner, objection string, disposition shed.Disposition, note, action string) (ShedResponse, *APIError) {
	req := ShedRuleRequest{Objection: objection, Note: note}
	entries, err := Dissent(o.repository, o.stream)
	if err != nil {
		return ShedResponse{}, &APIError{Internal, fmt.Sprintf("cannot read the dissent record of workstream %s; check the trace repository", o.stream)}
	}
	i := slices.IndexFunc(entries, func(e shed.Entry) bool { return e.ID == req.Objection })
	if i < 0 {
		return ShedResponse{}, &APIError{NotFound, fmt.Sprintf("no objection %s stands in workstream %s; read the dissent record with osmia status", req.Objection, o.stream)}
	}
	rounds, err := shed.AllRulings(o.repository, o.stream)
	if err != nil {
		return ShedResponse{}, &APIError{Internal, fmt.Sprintf("cannot read the owner's rulings of workstream %s; check the trace repository", o.stream)}
	}
	rulings := shed.Rulings{Version: shed.Version, Round: o.round, Revision: o.pin}
	for _, r := range rounds {
		if r.Round == o.round {
			rulings = r
		}
	}
	ruling := shed.Ruling{Objection: req.Objection, Disposition: disposition, Note: strings.TrimSpace(req.Note)}
	rulings.Rulings = append(slices.DeleteFunc(slices.Clone(rulings.Rulings), func(r shed.Ruling) bool { return r.Objection == ruling.Objection }), ruling)
	content, err := shed.EncodeRulings(rulings)
	if err != nil {
		return ShedResponse{}, &APIError{Validation, "the ruling cannot be recorded: " + err.Error()}
	}
	reason := fmt.Sprintf("the owner %s objection %s of %s in round %d, against %s", disposition, ruling.Objection, entries[i].Member, o.round, rulings.Revision)
	if ruling.Note != "" {
		reason += ": " + ruling.Note
	}
	if api := o.record(ctx, shed.RulingsDocumentID(o.round), shed.RulingsPath(o.round), string(content), fmt.Sprintf("%s-%d", action, o.round), reason); api != nil {
		return ShedResponse{}, api
	}
	return ShedResponse{Project: o.project, Workstream: o.stream, Round: o.round, Objection: ruling.Objection, Action: string(disposition), Detail: reason}, nil
}

// shedRedraft records the owner's request for the architect to redraft the
// spec and the plan after debate concluded, with the note that says what to
// change. The controller asks the architect for the redraft and debate
// resumes with the round that reads it.
func (s *Service) shedRedraft(ctx context.Context, raw string, req ShedRedraftRequest) (ShedResponse, *APIError) {
	o, api := s.shedAction(raw, InShedState)
	if api != nil {
		return ShedResponse{}, api
	}
	note := strings.TrimSpace(req.Note)
	if note == "" {
		return ShedResponse{}, &APIError{Validation, "a redraft requires a note saying what to change"}
	}
	if o.skipped {
		return ShedResponse{}, &APIError{Conflict, fmt.Sprintf("the owner skipped debate on workstream %s; no redraft is asked for and debated", o.stream)}
	}
	kind, n, ok := shedState(o.shed.Value)
	if !ok || kind != "concluded" {
		return ShedResponse{}, &APIError{Conflict, fmt.Sprintf("debate on workstream %s has not concluded; ask for a redraft once it has", o.stream)}
	}
	asked, err := shed.Redrafts(o.repository, o.stream)
	if err != nil {
		return ShedResponse{}, &APIError{Internal, fmt.Sprintf("cannot read the owner's requests of workstream %s; check the trace repository", o.stream)}
	}
	if slices.ContainsFunc(asked, func(r shed.Redraft) bool { return r.Round == n }) {
		return ShedResponse{}, &APIError{Conflict, fmt.Sprintf("a redraft after round %d of workstream %s is already asked for", n, o.stream)}
	}
	content, err := shed.EncodeRedraft(shed.Redraft{Version: shed.Version, Round: n, Revision: o.pin, Note: note})
	if err != nil {
		return ShedResponse{}, &APIError{Validation, "the request cannot be recorded: " + err.Error()}
	}
	reason := fmt.Sprintf("the owner asked the architect for a redraft of %s after round %d: %s", o.pin, n, note)
	if api := o.record(ctx, shed.RedraftDocumentID(n), shed.RedraftPath(n), string(content), fmt.Sprintf("redraft-%d", n), reason); api != nil {
		return ShedResponse{}, api
	}
	return ShedResponse{Project: o.project, Workstream: o.stream, Round: n, Action: "redraft", Detail: reason}, nil
}

// shedSkip records that the owner skips debate. No further committee turn
// starts; the workstream stays in the shed and still needs the owner's
// ratification of both documents.
func (s *Service) shedSkip(ctx context.Context, raw string) (ShedResponse, *APIError) {
	o, api := s.shedAction(raw, SketchedState, InShedState)
	if api != nil {
		return ShedResponse{}, api
	}
	if o.skipped {
		return ShedResponse{}, &APIError{Conflict, fmt.Sprintf("debate on workstream %s is already skipped", o.stream)}
	}
	if kind, n, ok := shedState(o.shed.Value); ok && (kind == "round" || kind == "reply") {
		return ShedResponse{}, &APIError{Conflict, fmt.Sprintf("workstream %s is running round %d; skip debate once the round is recorded", o.stream, n)}
	}
	entries, err := Dissent(o.repository, o.stream)
	if err != nil {
		return ShedResponse{}, &APIError{Internal, fmt.Sprintf("cannot read the dissent record of workstream %s; check the trace repository", o.stream)}
	}
	reason := "the owner skipped debate; the workstream still needs the owner's ratification of the spec and the plan"
	tx := trace.Transaction{ExpectedVersion: o.owner.Version,
		Transition: trace.Transition{Header: ownerHeader(skipTransition, o.project, o.stream, "owner-shed", o.now()), Subject: ownerSubject, From: o.owner.Value, To: skippedValue, Reason: reason},
		Events:     []trace.Event{trace.Notice(skipTransition, "owner", reason+".\n"+presentation(shed.Recommend(entries)))}}
	if _, err := o.repository.Transact(ctx, tx); err != nil {
		if errors.Is(err, trace.ErrConflict) {
			return ShedResponse{}, &APIError{Conflict, fmt.Sprintf("workstream %s changed while skipping debate; check osmia status and retry", o.stream)}
		}
		return ShedResponse{}, &APIError{Internal, fmt.Sprintf("cannot record the skipped debate of workstream %s; check the trace repository", o.stream)}
	}
	// A sketched workstream enters the shed without a committee: a skipped
	// debate runs no round, and ratification happens in the shed.
	if o.feature == SketchedState {
		h := ownerHeader(skipTransition+"-"+InShedState, o.project, o.stream, "owner-shed", o.now())
		if _, err := o.repository.MoveFeatureState(ctx, h, SketchedState, InShedState, reason); err != nil && !errors.Is(err, trace.ErrConflict) {
			return ShedResponse{}, &APIError{Internal, fmt.Sprintf("debate on workstream %s is skipped but it did not enter the shed; check the trace repository", o.stream)}
		}
	}
	return ShedResponse{Project: o.project, Workstream: o.stream, Round: o.round, Action: skippedValue, Detail: reason}, nil
}

// shedMore records the owner's request for further rounds after debate
// concluded. The controller resumes from the conclusion and runs them.
func (s *Service) shedMore(ctx context.Context, raw string, req ShedMoreRequest) (ShedResponse, *APIError) {
	o, api := s.shedAction(raw, InShedState)
	if api != nil {
		return ShedResponse{}, api
	}
	limit := s.current().Shed.MaxRounds
	if req.Rounds < 1 || req.Rounds > limit {
		return ShedResponse{}, &APIError{Validation, fmt.Sprintf("ask for between 1 and shed.max_rounds (%d) further rounds", limit)}
	}
	if o.skipped {
		return ShedResponse{}, &APIError{Conflict, fmt.Sprintf("the owner skipped debate on workstream %s; no further round runs", o.stream)}
	}
	kind, n, ok := shedState(o.shed.Value)
	if !ok || kind != "concluded" {
		return ShedResponse{}, &APIError{Conflict, fmt.Sprintf("debate on workstream %s has not concluded; ask for further rounds once it has", o.stream)}
	}
	content, err := shed.EncodeMore(shed.More{Version: shed.Version, Round: n, Rounds: req.Rounds})
	if err != nil {
		return ShedResponse{}, &APIError{Validation, "the request cannot be recorded: " + err.Error()}
	}
	reason := fmt.Sprintf("the owner asked for %s of debate after round %d", rounds(req.Rounds), n)
	if api := o.record(ctx, shed.MoreDocumentID(n), shed.MorePath(n), string(content), fmt.Sprintf("more-%d", n), reason); api != nil {
		return ShedResponse{}, api
	}
	return ShedResponse{Project: o.project, Workstream: o.stream, Round: n, Rounds: req.Rounds, Action: "more", Detail: reason}, nil
}

// rounds counts rounds for a message.
func rounds(n int) string {
	if n == 1 {
		return "1 more round"
	}
	return strconv.Itoa(n) + " more rounds"
}
