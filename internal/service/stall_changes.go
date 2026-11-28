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

const changeSubjectPrefix = "workgroup-change-"

func isChangeSubject(subject string) bool { return strings.HasPrefix(subject, changeSubjectPrefix) }

func changeOfSubject(subject string) (string, bool) {
	id, ok := strings.CutPrefix(subject, changeSubjectPrefix)
	return id, ok && id != ""
}

const changeStateTarget = "failed"

func watchedChange(state string) bool {
	switch state {
	case "drafting", "debating", "answering", "presenting", "approved", "applying", "releasing":
		return true
	}
	return false
}

type changeWatch struct {
	id      string
	what    string
	state   string
	version uint64
	since   time.Time
	failure string
	endable bool
}

func (c changeWatch) subject() string { return lifecycle.WorkgroupChangeDocument(c.id) }

type changeJournal struct {
	trace *trace.Repository
	host  config.WorkstreamID
	at    config.WorkstreamID
}

func (w stallWatch) changes(ctx context.Context, p *stallPass, host config.WorkstreamID) error {
	return w.journal(ctx, p, changeJournal{trace: w.repository, host: host, at: host}, func() (config.WorkgroupID, error) {
		groups, _, err := trace.Workgroups(w.repository)
		if err != nil {
			return "", err
		}
		g, ok := trace.WorkgroupOf(groups, host)
		if !ok || len(g.Members) == 0 || g.Members[0] != host {
			return "", nil
		}
		return g.ID, nil
	})
}

func (w stallWatch) crossRepoChanges(ctx context.Context, p *stallPass, streams []config.WorkstreamID) error {
	seen := map[config.WorkgroupID]bool{}
	var failed []error
	for _, stream := range streams {
		if err := ctx.Err(); err != nil {
			return err
		}
		id, err := w.repository.CrossRepoWorkgroupOf(stream)
		if err != nil {
			failed = append(failed, fmt.Errorf("workgroup of %s: %w", stream, err))
			continue
		}
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		if err := w.crossRepoGroupChanges(ctx, p, id, streams); err != nil && !errors.Is(err, trace.ErrLocked) {
			failed = append(failed, fmt.Errorf("workgroup changes of %s: %w", id, err))
		}
	}
	return errors.Join(failed...)
}

func (w stallWatch) crossRepoGroupChanges(ctx context.Context, p *stallPass, id config.WorkgroupID, streams []config.WorkstreamID) error {
	g, _, err := w.repository.CrossRepoWorkgroup(id)
	if err != nil || len(g.Members) < 2 {
		return err
	}
	leader := w.s.lifecycle().ChangeLeader(g.Members[1:])
	if leader == "" || !slices.Contains(streams, leader) {
		return nil
	}
	wt, err := trace.OpenWorkgroupAuthority(w.s.current().Root, id)
	if err != nil {
		if wt != nil {
			wt.Close()
		}
		return err
	}
	defer wt.Close()
	return w.journal(ctx, p, changeJournal{trace: wt, host: g.Members[0], at: leader}, func() (config.WorkgroupID, error) { return id, nil })
}

func (w stallWatch) journal(ctx context.Context, p *stallPass, j changeJournal, workgroup func() (config.WorkgroupID, error)) error {
	changes, err := lifecycle.ReadChanges(j.trace, j.host)
	if err != nil {
		return err
	}
	moved, err := changeRevisionTimes(j.trace, j.host)
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
		if err := w.change(ctx, p, j, watch); err != nil {
			return err
		}
	}
	if pending {
		return nil
	}
	requests, _, err := trace.ProjectAmendments(j.trace)
	if err != nil || len(requests) == 0 {
		return err
	}
	group, err := workgroup()
	if err != nil || group == "" {
		return err
	}
	for _, r := range requests {
		id := "contract-" + r.ID
		if r.Workgroup != group || slices.ContainsFunc(changes, func(c lifecycle.WorkgroupChange) bool { return c.ID == id }) {
			continue
		}
		watch := changeWatch{id: id, what: fmt.Sprintf("project amendment request %s of workgroup %s", r.ID, group), state: "requested", since: r.At}
		return w.change(ctx, p, j, watch)
	}
	return nil
}

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

func (w stallWatch) change(ctx context.Context, p *stallPass, j changeJournal, c changeWatch) error {
	host := j.at
	key := stallKey(w.repository.Project(), host, c.subject())
	turn, done, err := changeTurns(j, c.id)
	if err != nil {
		return err
	}
	q := scheduler.Quiet{Since: latest(c.since, done), Turn: turn}
	if w.s.store != nil {
		st, _ := w.s.effective()
		_, q.Paused = scheduler.Pausing(st.Pauses, w.repository.Project(), host)
	}
	identity := q.Since
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

func changeTurns(j changeJournal, id string) (time.Time, time.Time, error) {
	threads, err := j.trace.Threads(j.host)
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
			return router.Effect{Events: []trace.Event{trace.Notice(id+"-"+cause, trace.ManagerKey, body)}, Reason: reason}, nil
		},
	}
}

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
	reason := fmt.Sprintf("%s has sat %s and neither Jev nor the manager moved it within a further %d minutes, so ending it as failed, which lets a new change be requested, is yours to approve", c.what, c.state, int(after/time.Minute))
	move := api.RecoveryMove{Occurrence: rec.Occurrence, Machine: rec.Machine, Subject: rec.Subject, From: c.state, Version: c.version, Target: changeStateTarget, Origin: api.RecoveryStall, Reason: reason}
	err = writeRecoveryRecord(ctx, w.repository, host, recoveryRecord{RecoveryMove: move, State: recoveryOpen, Opened: now}, revisions[rec.Occurrence], stallActor, now)
	if errors.Is(err, trace.ErrConflict) {
		return nil
	}
	return err
}

func (s *Service) changeState(repo *trace.Repository, stream config.WorkstreamID, id string) (trace.WorkflowState, error) {
	journal, host := repo, stream
	group, err := repo.CrossRepoWorkgroupOf(stream)
	if err != nil {
		return trace.WorkflowState{}, err
	}
	if group != "" {
		g, _, err := repo.CrossRepoWorkgroup(group)
		if err != nil {
			return trace.WorkflowState{}, err
		}
		if len(g.Members) == 0 {
			return trace.WorkflowState{}, nil
		}
		wt, err := trace.OpenWorkgroupAuthority(s.current().Root, group)
		if err != nil {
			if wt != nil {
				wt.Close()
			}
			return trace.WorkflowState{}, err
		}
		defer wt.Close()
		journal, host = wt, g.Members[0]
	}
	changes, err := lifecycle.ReadChanges(journal, host)
	if err != nil {
		return trace.WorkflowState{}, err
	}
	if i := slices.IndexFunc(changes, func(c lifecycle.WorkgroupChange) bool { return c.ID == id }); i >= 0 {
		return trace.WorkflowState{Value: changes[i].State, Version: uint64(changes[i].Revision)}, nil
	}
	return trace.WorkflowState{}, nil
}
