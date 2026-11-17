package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/reconcile"
	"github.com/kpenfound/osmia/internal/router"
	"github.com/kpenfound/osmia/internal/scheduler"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
)

// stallActor is who the stall watchdog's exits and notices are attributed to.
var stallActor = trace.Actor{Kind: "service", ID: "stall-watchdog"}

// stallClocks keeps, in memory, when each subject was last explained. A subject
// whose pause was lifted or whose card was answered is quiet from then, not
// from its last progress, which may be long before. Nothing here is a record: a
// restart loses it, which only delays a stall by one window.
type stallClocks struct {
	mu        sync.Mutex
	explained map[string]time.Time
	ran       map[config.ProjectID]time.Time
}

// due reports whether the project's watchdog should read the records at now,
// and records the run. It looks at most once per every, and not at all in the
// first one after the project's first pass: a service that has just started has
// not yet resumed the turns and operations that explain a quiet.
func (c *stallClocks) due(project config.ProjectID, now time.Time, every time.Duration) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ran == nil {
		c.ran = map[config.ProjectID]time.Time{}
	}
	last, ok := c.ran[project]
	if !ok {
		c.ran[project] = now
		return false
	}
	if now.Sub(last) < every && !now.Before(last) {
		return false
	}
	c.ran[project] = now
	return true
}

// stallCheckEvery is how often the watchdog reads a project's records. A stall
// is measured in tens of minutes, so a pass of the scheduler does not need to
// look every time.
const stallCheckEvery = time.Minute

// note records that the subject was explained at the time.
func (c *stallClocks) note(key string, at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.explained == nil {
		c.explained = map[string]time.Time{}
	}
	if at.After(c.explained[key]) {
		c.explained[key] = at
	}
}

// last returns when the subject was last explained, zero when it never was.
func (c *stallClocks) last(key string) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.explained[key]
}

// stallWatch is the stall watchdog of one project: code only. A pass reads the
// records and raises one stalled exit to the transition router for each unit
// and workstream that has not progressed for [loop] stall_after with nothing
// legitimate explaining the quiet. Jev is never asked here: the exits are handed
// to the router's worker, and Jev judges only what to do about a stall.
type stallWatch struct {
	s          *Service
	cfg        *config.Config
	repository *trace.Repository
	// submit hands an exit to the router's worker; nil uses the service's.
	submit func(router.Exit) bool
}

// Run is the watchdog as a pass of the scheduler runs it: it reads the records
// at most once per stallCheckEvery of wall-clock time, whatever the service's
// own clock reads.
func (w stallWatch) Run(ctx context.Context) error {
	if w.cfg.StallWindow() <= 0 || !w.s.stalls.due(w.repository.Project(), time.Now(), stallCheckEvery) {
		return nil
	}
	// The watchdog is no reason to stop a pass of the scheduler: a project it
	// cannot read is tried again at the next one.
	if err := w.Pass(ctx); err != nil && ctx.Err() == nil {
		w.s.log().Warn("the stall watchdog could not read the project", "project", string(w.repository.Project()), "error", err.Error())
	}
	return nil
}

// Pass raises the stalled exits of the project's units and workstreams.
func (w stallWatch) Pass(ctx context.Context) error {
	after := w.cfg.StallWindow()
	if after <= 0 {
		return nil
	}
	streams, err := intake.NonSessionWorkstreams(w.repository)
	if err != nil {
		return err
	}
	statuses, err := w.repository.Statuses()
	if err != nil {
		return err
	}
	escalations, err := w.repository.Inbox()
	if err != nil {
		return err
	}
	librarian := librarianWorkstream(w.repository.Project())
	var failed []error
	for _, stream := range streams {
		if stream == librarian {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		i := slices.IndexFunc(statuses, func(s trace.WorkstreamStatus) bool { return s.Workstream == stream })
		if i < 0 {
			continue
		}
		// One workstream that cannot be read leaves the others watched.
		if err := w.workstream(ctx, statuses[i], escalations, after); err != nil {
			failed = append(failed, fmt.Errorf("workstream %s: %w", stream, err))
		}
	}
	return errors.Join(failed...)
}

// stallReading is everything a pass reads about one workstream and its units.
type stallReading struct {
	feature trace.WorkflowState
	// created is when the workstream was created.
	created time.Time
	// progress is when the workstream and each unit last progressed.
	progress stallProgress
	// holds are when the router last held each subject.
	holds map[string]time.Time
	// turns are the non-chief turns running or queued, by unit; the empty unit
	// holds all of them. done is when the latest finished.
	live map[string]bool
	done map[string]time.Time
	// card is whether an owner card is open for the workstream, and for each unit
	// that has its own.
	card      bool
	unitCards map[string]bool
	// paused is whether a pause covers the workstream.
	paused bool
	// waiting is whether the workstream waits on a live upstream.
	waiting bool
	// operations are the operations without a result, by unit; the empty unit
	// holds all of them.
	operations map[string][]scheduler.Operation
}

// workstreamHold is when the router last held a subject of the workstream
// itself, not one of its units.
func (r stallReading) workstreamHold() time.Time {
	var out time.Time
	for subject, at := range r.holds {
		if !strings.HasPrefix(subject, "unit-") && !strings.HasPrefix(subject, "unit_") && at.After(out) {
			out = at
		}
	}
	return out
}

func (w stallWatch) workstream(ctx context.Context, status trace.WorkstreamStatus, escalations []trace.InboxEntry, after time.Duration) error {
	stream := status.Workstream
	feature, err := w.repository.Workflow(stream, trace.FeatureSubject)
	if err != nil {
		return err
	}
	if feature.Value == "" || feature.Value == DeliveredState || feature.Value == AbandonedState {
		return nil
	}
	r := stallReading{feature: feature, created: status.CreatedAt}
	if r.progress, err = readStallProgress(w.repository, stream); err != nil {
		return err
	}
	if err := w.readHolds(&r, stream); err != nil {
		return err
	}
	if err := w.readTurns(&r, stream); err != nil {
		return err
	}
	if err := w.readCards(ctx, &r, status, escalations); err != nil {
		return err
	}
	if err := w.readWaits(&r, stream); err != nil {
		return err
	}
	if err := w.readOperations(&r, stream); err != nil {
		return err
	}
	if w.s.store != nil {
		st, _ := w.s.effective()
		_, r.paused = scheduler.Pausing(st.Pauses, w.repository.Project(), stream)
	}
	now := w.s.now()
	limit := w.s.GiveUpLimit().Failures

	// The workstream: quiet since the later of its own progress, a hold of the
	// router and the end of the latest turn of its workers.
	since := latest(r.created, r.progress.stream, r.workstreamHold(), latestOf(r.done))
	q := scheduler.Quiet{Since: since, Card: r.card, Paused: r.paused, Turn: len(r.live) > 0, Waiting: r.waiting, Operations: r.operations[""]}
	raw, err := w.repository.RawWorkflow(stream, trace.FeatureSubject)
	if err != nil {
		return err
	}
	if err := w.assess(stream, "", raw, q, since, r, now, after, limit); err != nil {
		return err
	}

	// Its units, while it builds or is assembled.
	if feature.Value != BuildingState && feature.Value != AssembledState {
		return nil
	}
	states, err := w.repository.WorkflowStates(stream)
	if err != nil {
		return err
	}
	// The dependencies of a unit that waits on others are in the sealed plan.
	deps := map[string][]string{}
	if b, ok, err := (&masons{s: w.s, cfg: w.cfg, repository: w.repository}).read(stream); err == nil && ok {
		for _, u := range b.Plan().Units {
			deps[u.ID] = u.DependsOn
		}
	}
	merged := func(id string) bool { return states[trace.UnitSubject(id)].Value == UnitMerged }
	units := make([]string, 0, len(states))
	for subject := range states {
		if unit, ok := r.progress.unitOf(subject); ok {
			units = append(units, unit)
		}
	}
	slices.Sort(units)
	for _, unit := range units {
		state := states[trace.UnitSubject(unit)]
		if state.Value == "" || state.Value == UnitMerged {
			continue
		}
		since := latest(r.created, r.progress.units[unit], r.progress.owner, r.holds[trace.UnitSubject(unit)], r.done[unit])
		// A planned unit waits on the units it depends on; a ready one waits
		// its turn behind the work that is going on.
		waiting := r.waiting
		if state.Value == UnitPlanned {
			waiting = waiting || slices.ContainsFunc(deps[unit], func(id string) bool { return !merged(id) })
		}
		turn := r.live[unit]
		if state.Value == UnitReady {
			turn = turn || len(r.live) > 0
		}
		q := scheduler.Quiet{Since: since, Card: r.card || r.unitCards[unit], Paused: r.paused, Turn: turn, Waiting: waiting, Operations: r.operations[unit]}
		if err := w.assess(stream, unit, state, q, since, r, now, after, limit); err != nil {
			return err
		}
	}
	return nil
}

// assess judges one subject and raises its stalled exit. A subject that is
// explained notes the time; a subject that is stalled is raised once for the
// stall, which is identified by the progress it has been quiet since.
func (w stallWatch) assess(stream config.WorkstreamID, unit string, state trace.WorkflowState, q scheduler.Quiet, identity time.Time, r stallReading, now time.Time, after time.Duration, limit int) error {
	key := fmt.Sprintf("%s/%s/%s", w.repository.Project(), stream, unit)
	// The quiet is counted from the last time something explained it too, so a
	// lifted pause or an answered card starts the window over.
	if scheduler.Explain(q, now, after, limit) != scheduler.NotExplained {
		w.s.stalls.note(key, now)
	}
	q.Since = latest(q.Since, w.s.stalls.last(key))
	verdict := scheduler.Assess(q, now, after, limit)
	if !verdict.Stalled {
		return nil
	}
	cause := "stalled-" + identity.UTC().Format("20060102T150405Z")
	x, err := w.exit(stream, unit, state, verdict, identity, cause, r, now)
	if err != nil || x == nil {
		return err
	}
	if rec, err := router.Lookup(w.repository, stream, *x); err != nil {
		return err
	} else if rec != nil && rec.Finished() {
		return nil
	}
	submit := w.submit
	if submit == nil {
		submit = func(x router.Exit) bool { return w.s.SubmitExit(x, nil) }
	}
	submit(*x)
	return nil
}

// exit builds the stalled exit of a unit, or of the workstream when unit is
// empty, with the evidence the watchdog computed: how long the subject has been
// quiet, since when and why, and the last failure of each operation that
// explains nothing.
func (w stallWatch) exit(stream config.WorkstreamID, unit string, state trace.WorkflowState, v scheduler.Stall, since time.Time, cause string, r stallReading, now time.Time) (*router.Exit, error) {
	name := "workstream " + string(stream)
	last, what := r.progress.streamAt, r.progress.streamWhat
	if unit != "" {
		name = "unit " + unit
		last, what = r.progress.units[unit], r.progress.unitWhat[unit]
	}
	minutes := int(v.Quiet / time.Minute)
	if what == "" {
		what = "its creation"
	}
	if last.IsZero() {
		last = r.created
	}
	facts := map[string]any{"minutes_quiet": minutes, "state": state.Value, "last_progress": last.UTC().Format(time.RFC3339), "last_progress_was": what, "operations_without_result": len(v.Unresolved)}
	var records []router.Entry
	var failures []string
	for _, o := range v.Unresolved {
		text := fmt.Sprintf("operation %s has no result: %d consecutive failures, latest attempt at %s", o.Action, o.Failures, o.LastAttempt.UTC().Format(time.RFC3339))
		if o.LastFailure != "" {
			text += "; last failure: " + o.LastFailure
			failures = append(failures, fmt.Sprintf("%s: %s", o.Action, o.LastFailure))
		}
		if o.Held == scheduler.HeldByOther {
			text += "; it is held for a reason that is no pause, owner card or dependency wait"
		}
		records = append(records, router.Entry{Kind: "operation", Text: text})
	}
	body := fmt.Sprintf("%s has made no progress for %d minutes (since %s, %s) and nothing explains the quiet: no owner card, pause, running or queued turn, operation in flight or live dependency wait.", strings.ToUpper(name[:1])+name[1:], minutes, last.UTC().Format(time.RFC3339), what)
	if len(failures) > 0 {
		body += " Last failure: " + strings.Join(failures, "; ") + "."
	}
	evidence := router.Evidence{Facts: facts, Records: records}
	reason := fmt.Sprintf("Stalled: %s made no progress for %d minutes with nothing to explain it", name, minutes)
	if unit == "" {
		evidence.Intent = "The workstream's sealed spec and plan are what the owner approved."
		x := w.s.lifecycle().StalledExit(w.repository, stream, state, lifecycle.StalledInput{Cause: cause, Actor: stallActor, Notice: body, Reason: reason, Evidence: evidence, At: now})
		return &x, nil
	}
	id := fmt.Sprintf("%s-stalled-%d-%s", trace.UnitSubject(unit), state.Version, since.UTC().Format("20060102T150405Z"))
	if len(id) > 120 {
		id = fmt.Sprintf("unit-stalled-%d-%s", state.Version, since.UTC().Format("20060102T150405Z"))
	}
	x, err := w.s.execution().UnitExit(execution.UnitExitInput{
		Repo: w.repository, Stream: stream, Unit: unit, Trigger: execution.TriggerStalled, Cause: cause, State: state,
		Default: execution.TargetWait, Targets: execution.StalledTargets(state.Value), Actor: stallActor, Note: reason,
		Evidence: evidence, ID: id, HeaderCause: cause, Reason: reason, Active: true,
		Events: []trace.Event{trace.Notice(id, "chief", body)},
	})
	if err != nil {
		return nil, err
	}
	return &x, nil
}

func latest(times ...time.Time) time.Time {
	var out time.Time
	for _, t := range times {
		if t.After(out) {
			out = t
		}
	}
	return out
}

func latestOf[K comparable](m map[K]time.Time) time.Time {
	var out time.Time
	for _, t := range m {
		if t.After(out) {
			out = t
		}
	}
	return out
}

// stallProgress is when a workstream and its units last progressed. A turn of
// the chief of staff is not progress, nor is the router holding a subject.
type stallProgress struct {
	stream     time.Time
	streamAt   time.Time
	streamWhat string
	units      map[string]time.Time
	unitWhat   map[string]string
	// names maps the workflow subject of each unit the records name to its ID.
	names map[string]string
	// owner is when the owner last acted on the workstream.
	owner time.Time
}

// unitOf returns the ID of the unit whose workflow subject is subject.
func (p stallProgress) unitOf(subject string) (string, bool) {
	if unit, ok := p.names[subject]; ok {
		return unit, true
	}
	if unit, ok := strings.CutPrefix(subject, "unit-"); ok {
		return unit, true
	}
	return "", false
}

// readStallProgress reads when the workstream and its units last progressed, by
// the rules of the loop guard's progress, except that nothing the chief of staff
// did counts.
func readStallProgress(repository *trace.Repository, stream config.WorkstreamID) (stallProgress, error) {
	records, err := trace.Read[trace.Record](repository, stream)
	if err != nil {
		return stallProgress{}, err
	}
	out := stallProgress{units: map[string]time.Time{}, unitWhat: map[string]string{}, names: map[string]string{}}
	mark := func(at time.Time, why string) {
		if !at.Before(out.stream) {
			out.stream, out.streamAt, out.streamWhat = at, at, why
		}
	}
	for _, record := range records {
		switch r := record.(type) {
		case trace.Transition:
			if r.Unit != "" && (strings.HasPrefix(r.Subject, "unit-") || strings.HasPrefix(r.Subject, "unit_")) {
				out.names[r.Subject] = r.Unit
			}
			if r.Actor == chiefActor || router.IsJournal(r.Subject) {
				continue
			}
			switch {
			case r.Actor.Kind == ownerActor.Kind:
				mark(r.At, "an owner action")
				if r.At.After(out.owner) {
					out.owner = r.At
				}
			case r.From == r.To:
			case r.Subject == trace.FeatureSubject:
				mark(r.At, "the workstream moving to "+featureState(r.To))
			case strings.HasPrefix(r.Subject, "unit-") || strings.HasPrefix(r.Subject, "unit_"):
				if !slices.Contains(unitProgress, r.To) {
					continue
				}
				unit, _ := out.unitOf(r.Subject)
				why := fmt.Sprintf("unit %s moving to %s", unit, r.To)
				mark(r.At, why)
				if !r.At.Before(out.units[unit]) {
					out.units[unit], out.unitWhat[unit] = r.At, why
				}
			case r.Subject == shedSubject, strings.HasPrefix(r.Subject, amendmentSubject("")), r.Subject == finalReviewSubject, r.Subject == publicationSubject:
				mark(r.At, fmt.Sprintf("%s moving to %s", r.Subject, r.To))
			case r.Subject == driftSubject && strings.HasPrefix(r.To, driftRebased+"-"):
				mark(r.At, "a drift rebase")
			case r.Subject == loopGuardSubject && strings.HasPrefix(r.To, "released-"):
				mark(r.At, "the owner resuming it")
			}
		case trace.Document:
			if r.Actor.Kind == ownerActor.Kind {
				mark(r.At, "an owner action")
				if r.At.After(out.owner) {
					out.owner = r.At
				}
			}
		case trace.TurnRequest:
			if r.Actor.Kind == ownerActor.Kind {
				mark(r.At, "an owner message")
				if r.At.After(out.owner) {
					out.owner = r.At
				}
			}
		}
	}
	if out.streamWhat == "" {
		out.streamWhat = "its creation"
	}
	return out, nil
}

// readHolds reads when the router last held each subject of the workstream, so
// a hold that stays unresolved becomes a stall once the window passes it.
func (w stallWatch) readHolds(r *stallReading, stream config.WorkstreamID) error {
	records, err := router.Unfinished(w.repository, stream)
	if err != nil {
		return err
	}
	r.holds = map[string]time.Time{}
	for _, rec := range records {
		at := rec.Decided
		if at.IsZero() {
			at = rec.Raised
		}
		if at.After(r.holds[rec.Subject]) {
			r.holds[rec.Subject] = at
		}
	}
	return nil
}

// readTurns reads the turns of the workstream's workers that are running or
// queued, and when the latest of them finished. A turn of the chief of staff
// explains nothing and is no progress.
func (w stallWatch) readTurns(r *stallReading, stream config.WorkstreamID) error {
	threads, err := w.repository.Threads(stream)
	if err != nil {
		return err
	}
	r.live, r.done = map[string]bool{}, map[string]time.Time{}
	for _, t := range threads {
		if t.Identity.Role == trace.ChiefOfStaff {
			continue
		}
		for _, q := range t.Turns {
			if q.CompletedAt.IsZero() {
				r.live[q.Request.Unit] = true
				if q.Request.Unit != "" {
					r.live[""] = true
				}
			} else if q.CompletedAt.After(r.done[q.Request.Unit]) {
				r.done[q.Request.Unit] = q.CompletedAt
			}
		}
	}
	// A unit's turns also end the workstream's quiet.
	for unit, at := range r.done {
		if unit != "" && at.After(r.done[""]) {
			r.done[""] = at
		}
	}
	return nil
}

// readCards reads the owner cards open for the workstream and its units.
func (w stallWatch) readCards(ctx context.Context, r *stallReading, status trace.WorkstreamStatus, escalations []trace.InboxEntry) error {
	r.unitCards = map[string]bool{}
	entries, err := w.s.openDecisions(ctx, w.repository, status)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Unit != "" {
			r.unitCards[e.Unit] = true
		} else {
			r.card = true
		}
	}
	for _, e := range escalations {
		if e.State != trace.QuestionEscalated || e.Workstream != status.Workstream {
			continue
		}
		unit := false
		for _, q := range e.Questions {
			if q.Asked.Unit != "" {
				r.unitCards[q.Asked.Unit] = true
				unit = true
			}
		}
		if !unit {
			r.card = true
		}
	}
	// A shared workgroup that the owner has not approved is the owner's to
	// decide.
	if approved, err := w.repository.GroupApproved(status.Workstream); err != nil {
		return err
	} else if !approved {
		r.card = true
	}
	return nil
}

// readWaits reads whether the workstream waits on an upstream that is alive: its
// same-repo base, or an incoming dependency edge, neither of which has ended or
// is already integrated.
func (w stallWatch) readWaits(r *stallReading, stream config.WorkstreamID) error {
	waiting, err := liveDependencyWait(w.repository, stream)
	r.waiting = waiting
	return err
}

// liveDependencyWait reports whether stream is held by a dependency wait whose
// upstreams are all alive. An upstream that is abandoned, closed or already in
// its project's base branch no longer explains the wait: the dependency exit
// decides it.
func liveDependencyWait(repository *trace.Repository, stream config.WorkstreamID) (bool, error) {
	if waiting, err := baseWaiting(repository, stream); err != nil {
		return false, err
	} else if waiting {
		dependency, err := repository.WorkstreamBase(stream)
		if err != nil {
			return false, err
		}
		if dependency.Base != "" {
			parent, err := repository.Workflow(dependency.Base, trace.FeatureSubject)
			if err != nil {
				return false, err
			}
			if parent.Value != AbandonedState {
				return true, nil
			}
		}
	}
	waiting, err := edgeWaiting(repository, stream)
	if err != nil || !waiting {
		return false, err
	}
	edges, err := lifecycle.IncomingMergeEdges(repository, stream)
	if err != nil {
		return false, err
	}
	observed, _, _, err := lifecycle.EdgeObservationAt(repository, stream)
	if err != nil {
		return false, err
	}
	live := false
	for _, e := range lifecycle.EdgesOfType(edges, trace.EdgeHardBlock) {
		i := slices.IndexFunc(observed.Edges, func(m lifecycle.EdgeMergeStatus) bool { return m.Upstream == e.From && m.Type == e.Type })
		if i >= 0 && observed.Edges[i].Merged {
			continue
		}
		if i >= 0 && (observed.Edges[i].Ended != "" || observed.Edges[i].Integrated != "") {
			return false, nil
		}
		live = true
	}
	return live, nil
}

// readOperations reads the operations of the workstream that have no result,
// each with its failure count, latest attempt, last failure and the cause of the
// hold that keeps it from running, if one does.
func (w stallWatch) readOperations(r *stallReading, stream config.WorkstreamID) error {
	records, err := w.repository.Operations(stream)
	if err != nil {
		return err
	}
	r.operations = map[string][]scheduler.Operation{}
	cause := w.s.holdingCause(w.repository)
	for _, rec := range records {
		if rec.Result != nil || rec.Acknowledged {
			continue
		}
		// A turn is read from its thread.
		if _, err := thread.DecodeTurn(rec.Operation); err == nil {
			continue
		}
		op := operationFacts(rec, cause(stream, rec.Operation))
		r.operations[""] = append(r.operations[""], op)
		if unit := operationUnitOf(rec.Operation); unit != "" {
			r.operations[unit] = append(r.operations[unit], op)
		}
	}
	return nil
}

// operationFacts reads what the watchdog needs of an operation without a
// result: its consecutive failures, the time of its latest claim, observation,
// effect or retry (the time it was queued while it has had none) and the last
// failure it recorded, which names a recovery failure when that was the cause.
func operationFacts(rec trace.OperationRecord, held scheduler.HoldCause) scheduler.Operation {
	status := reconcile.StatusOf(rec)
	return scheduler.Operation{
		Action: rec.Operation.Action, Failures: status.Failures, LastFailure: status.LastFailure,
		LastAttempt: latest(status.LastAttempt, rec.Transition.At), Held: held,
	}
}

// operationUnitOf returns the unit an operation works on, empty when it works
// on none.
func operationUnitOf(op coreadapter.Operation) string {
	var in struct {
		Unit string `json:"unit"`
	}
	if json.Unmarshal(op.Input, &in) != nil {
		return ""
	}
	return in.Unit
}
