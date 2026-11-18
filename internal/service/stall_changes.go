package service

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/router"
	"github.com/kpenfound/osmia/internal/scheduler"
	"github.com/kpenfound/osmia/internal/trace"
)

// changeSubjectPrefix starts the subject a workgroup change's stall is raised
// on: the name of the change's journal document. The change itself moves in
// that document, never in a workflow subject, so the stall's subject stays at
// version zero and the occurrence is told apart by the change's revision.
const changeSubjectPrefix = "workgroup-change-"

// isChangeSubject reports whether a subject is the stall of a workgroup change.
func isChangeSubject(subject string) bool { return strings.HasPrefix(subject, changeSubjectPrefix) }

// changeOfSubject returns the ID of the workgroup change a stall subject names.
func changeOfSubject(subject string) (string, bool) {
	id, ok := strings.CutPrefix(subject, changeSubjectPrefix)
	return id, ok && id != ""
}

// changeStateTarget is the target of a recovery card that ends a workgroup
// change sitting in its draft or debate.
const changeStateTarget = "failed"

// watchedChange reports whether the watchdog watches a workgroup change in this
// state: one the committee is drafting, debating or answering, or the service is
// presenting or applying. A change presented to the owner waits on the owner's
// decision, and one that failed, was rejected or was applied has ended.
func watchedChange(state string) bool {
	switch state {
	case "drafting", "debating", "answering", "presenting", "approved", "applying", "releasing":
		return true
	}
	return false
}

// changeWatch is one workgroup change, or one project amendment request not yet
// drafted into a change, as the watchdog judges it.
type changeWatch struct {
	// id is the change's ID, or the ID the request's change will have.
	id      string
	what    string
	state   string
	version uint64
	// since is when the change last moved: its latest journal revision, or
	// when the request was filed.
	since   time.Time
	failure string
	// endable says the owner can end the change from its recovery card.
	endable bool
}

// subject is the subject the change's stall is raised on.
func (c changeWatch) subject() string { return lifecycle.WorkgroupChangeDocument(c.id) }

// changes raises the stalls of the workgroup changes the stream hosts that have
// not moved for the window with nothing to explain it, and of the first project
// amendment request of its workgroup that was not drafted into a change while
// no change is ahead of it.
func (w stallWatch) changes(ctx context.Context, p *stallPass, host config.WorkstreamID) error {
	changes, err := lifecycle.ReadChanges(w.repository, host)
	if err != nil {
		return err
	}
	moved, err := changeRevisionTimes(w.repository, host)
	if err != nil {
		return err
	}
	pending := false
	for _, c := range changes {
		if c.State != "applied" && c.State != "rejected" && c.State != "failed" {
			pending = true
		}
		if !watchedChange(c.State) {
			continue
		}
		watch := changeWatch{id: c.ID, what: "workgroup change " + c.ID, state: c.State, version: uint64(c.Revision), since: moved[lifecycle.WorkgroupChangeDocument(c.ID)], failure: c.Failure, endable: lifecycle.StalledChangeEndable(c.State)}
		if err := w.change(ctx, p, host, watch); err != nil {
			return err
		}
	}
	if pending {
		return nil
	}
	requests, _, err := trace.ProjectAmendments(w.repository)
	if err != nil || len(requests) == 0 {
		return err
	}
	groups, _, err := trace.Workgroups(w.repository)
	if err != nil {
		return err
	}
	g, ok := trace.WorkgroupOf(groups, host)
	if !ok || len(g.Members) == 0 || g.Members[0] != host {
		return nil
	}
	for _, r := range requests {
		id := "contract-" + r.ID
		if r.Workgroup != g.ID || slices.ContainsFunc(changes, func(c lifecycle.WorkgroupChange) bool { return c.ID == id }) {
			continue
		}
		watch := changeWatch{id: id, what: fmt.Sprintf("project amendment request %s of workgroup %s", r.ID, g.ID), state: "requested", since: r.At}
		return w.change(ctx, p, host, watch)
	}
	return nil
}

// changeRevisionTimes returns when each workgroup change journal of the host was
// last revised, by document ID.
func changeRevisionTimes(repository *trace.Repository, host config.WorkstreamID) (map[string]time.Time, error) {
	docs, err := trace.Read[trace.Document](repository, host)
	if err != nil {
		return nil, err
	}
	out, revisions := map[string]time.Time{}, map[string]int{}
	for _, d := range docs {
		if !strings.HasPrefix(d.ID, changeSubjectPrefix) || d.Revision < revisions[d.ID] {
			continue
		}
		out[d.ID], revisions[d.ID] = d.At, d.Revision
	}
	return out, nil
}

// change judges one workgroup change and raises its stalled exit once for the
// stall, identified by the change's revision and the time it last moved. A
// stall the router decided that still stands a further window later, on a
// change that can still be ended, has the owner's recovery card.
func (w stallWatch) change(ctx context.Context, p *stallPass, host config.WorkstreamID, c changeWatch) error {
	key := stallKey(w.repository.Project(), host, c.subject())
	turn, done, err := w.changeTurns(host, c.id)
	if err != nil {
		return err
	}
	q := scheduler.Quiet{Since: latest(c.since, done), Turn: turn}
	if w.s.store != nil {
		st, _ := w.s.effective()
		_, q.Paused = scheduler.Pausing(st.Pauses, w.repository.Project(), host)
	}
	identity := q.Since
	// The operations of the change's turns are read only when nothing else
	// explains the quiet.
	if scheduler.Explain(q, p.now, p.after, p.limit) == scheduler.NotExplained && p.now.Sub(latest(q.Since, w.s.stalls.last(key))) >= p.after {
		if q.Operations, err = w.changeOperations(p, c.id); err != nil {
			return err
		}
	}
	if scheduler.Explain(q, p.now, p.after, p.limit) != scheduler.NotExplained {
		w.s.stalls.note(key, p.now)
	}
	q.Since = latest(q.Since, w.s.stalls.last(key))
	verdict := scheduler.Assess(q, p.now, p.after, p.limit)
	if !verdict.Stalled {
		return nil
	}
	x := w.changeExit(host, c, verdict, identity, p.now)
	if rec, err := router.Lookup(w.repository, host, x); err != nil {
		return err
	} else if rec != nil && rec.Finished() {
		return w.openChangeRecovery(ctx, host, c, *rec, p.now, p.after)
	}
	submit := w.submit
	if submit == nil {
		submit = func(x router.Exit) bool { return w.s.SubmitExit(x, nil) }
	}
	submit(x)
	return nil
}

// changeTurns returns the latest time a delta turn of the change that is
// running or queued was queued, claimed or attempted, and when the latest of
// them finished.
func (w stallWatch) changeTurns(host config.WorkstreamID, id string) (time.Time, time.Time, error) {
	threads, err := w.repository.Threads(host)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	cause := lifecycle.WorkgroupChangeDocument(id)
	var live, done time.Time
	for _, t := range threads {
		for _, q := range t.Turns {
			if q.Request.Cause != cause {
				continue
			}
			if q.CompletedAt.IsZero() {
				live = latest(live, turnActivity(q))
			} else {
				done = latest(done, q.CompletedAt)
			}
		}
	}
	return live, done, nil
}

// changeOperations returns the delta turn operations of the change that have
// no result, in any workstream of the project: a change's turns run on its
// workgroup's leading member.
func (w stallWatch) changeOperations(p *stallPass, id string) ([]scheduler.Operation, error) {
	cause := w.s.holdingCause(w.repository)
	var out []scheduler.Operation
	for _, status := range p.statuses {
		records, err := w.repository.Operations(status.Workstream)
		if err != nil {
			return nil, err
		}
		for _, rec := range records {
			if rec.Result != nil || rec.Acknowledged || rec.Operation.Action != deltaTurnAction {
				continue
			}
			if in, err := lifecycle.DecodeDeltaTurn(rec.Operation); err != nil || in.Change != id {
				continue
			}
			out = append(out, operationFacts(rec, cause(status.Workstream, rec.Operation)))
		}
	}
	return out, nil
}

// changeExit is the stalled exit of a workgroup change. Its one target leaves
// the change where it is and tells the chief of staff, with the evidence: the
// state the change sits in, for how long, and why it is held when it is.
func (w stallWatch) changeExit(host config.WorkstreamID, c changeWatch, v scheduler.Stall, since, now time.Time) router.Exit {
	minutes := int(v.Quiet / time.Minute)
	cause := fmt.Sprintf("stalled-%d-%s", c.version, since.UTC().Format("20060102T150405Z"))
	facts := map[string]any{"minutes_quiet": minutes, "state": c.state, "revision": c.version, "last_progress": since.UTC().Format(time.RFC3339), "operations_without_result": len(v.Unresolved)}
	body := fmt.Sprintf("%s has sat %s for %d minutes (since %s) and nothing explains the quiet: no pause, running or queued delta turn younger than the window or delta turn operation in flight.", strings.ToUpper(c.what[:1])+c.what[1:], c.state, minutes, since.UTC().Format(time.RFC3339))
	if c.failure != "" {
		facts["held"] = c.failure
		body += " It is held: " + c.failure + "."
	}
	var records []router.Entry
	for _, o := range v.Unresolved {
		text := fmt.Sprintf("operation %s has no result: %d consecutive failures, latest attempt at %s", o.Action, o.Failures, o.LastAttempt.UTC().Format(time.RFC3339))
		if o.LastFailure != "" {
			text += "; last failure: " + o.LastFailure
			body += fmt.Sprintf(" Last failure of %s: %s.", o.Action, o.LastFailure)
		}
		records = append(records, router.Entry{Kind: "operation", Text: text})
	}
	reason := fmt.Sprintf("Stalled: %s sat %s for %d minutes with nothing to explain it", c.what, c.state, minutes)
	id := fmt.Sprintf("%s-stalled-%d", c.subject(), c.version)
	return router.Exit{
		Repo: w.repository, Project: w.repository.Project(), Workstream: host, Machine: intake.WorkstreamMachine,
		Subject: c.subject(), Trigger: intake.TriggerStalled, Cause: cause, HeaderCause: cause, Actor: stallActor, At: now,
		Default: intake.TargetWait, Targets: []router.Target{intake.ExitTarget(intake.TriggerStalled, router.Target{Name: intake.TargetWait, Stay: true, NoMove: true})},
		Questions: intake.ExitQuestions(intake.TriggerStalled), Evidence: router.Evidence{Facts: facts, Records: records},
		TransitionID: func(router.Target) string { return id },
		Enter: func(context.Context, router.Plan) (router.Effect, error) {
			return router.Effect{Events: []trace.Event{trace.Notice(id+"-"+cause, "chief", body)}, Reason: reason}, nil
		},
	}
}

// openChangeRecovery opens the recovery card of a workgroup change's stall that
// nobody cleared: the router decided it, the chief of staff was told, and the
// change has not moved for a further window. A change still being drafted,
// debated or answered is ended as failed when the owner approves, so a new one
// can be requested; a change being presented or applied has no such move.
func (w stallWatch) openChangeRecovery(ctx context.Context, host config.WorkstreamID, c changeWatch, rec router.Record, now time.Time, after time.Duration) error {
	if !c.endable || rec.State == router.StateOwner || rec.State == router.StateStale || rec.State == router.StateDecided {
		return nil
	}
	decided := rec.Decided
	if decided.IsZero() {
		decided = rec.Raised
	}
	if now.Sub(decided) < after {
		return nil
	}
	marks, revisions, _, err := readRecoveryRecords(w.repository, host)
	if err != nil {
		return err
	}
	if _, opened := marks[rec.Occurrence]; opened {
		return nil
	}
	reason := fmt.Sprintf("%s has sat %s and neither Jev nor the chief of staff moved it within a further %d minutes, so ending it as failed, which lets a new change be requested, is yours to approve", c.what, c.state, int(after/time.Minute))
	move := api.RecoveryMove{Occurrence: rec.Occurrence, Machine: rec.Machine, Subject: rec.Subject, From: c.state, Version: c.version, Target: changeStateTarget, Origin: api.RecoveryStall, Reason: reason}
	err = writeRecoveryRecord(ctx, w.repository, host, recoveryRecord{RecoveryMove: move, State: recoveryOpen, Opened: now}, revisions[rec.Occurrence], stallActor, now)
	if errors.Is(err, trace.ErrConflict) {
		return nil
	}
	return err
}

// changeState is the state and revision of the workgroup change the host's
// journal holds, as a recovery card pins it; zero when the journal holds none.
func changeState(repo *trace.Repository, host config.WorkstreamID, id string) (trace.WorkflowState, error) {
	changes, err := lifecycle.ReadChanges(repo, host)
	if err != nil {
		return trace.WorkflowState{}, err
	}
	if i := slices.IndexFunc(changes, func(c lifecycle.WorkgroupChange) bool { return c.ID == id }); i >= 0 {
		return trace.WorkflowState{Value: changes[i].State, Version: uint64(changes[i].Revision)}, nil
	}
	return trace.WorkflowState{}, nil
}
