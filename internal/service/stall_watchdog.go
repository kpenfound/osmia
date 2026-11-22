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
	p := &stallPass{statuses: statuses, escalations: escalations, after: after, now: w.s.now(), limit: w.s.GiveUpLimit().Failures, upstreams: map[config.WorkstreamID]*bool{}}
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
		if err := w.workstream(ctx, p, statuses[i]); err != nil {
			failed = append(failed, fmt.Errorf("workstream %s: %w", stream, err))
		}
		if err := w.changes(ctx, p, stream); err != nil {
			failed = append(failed, fmt.Errorf("workgroup changes of %s: %w", stream, err))
		}
	}
	return errors.Join(failed...)
}

// stallPass is what one pass of the watchdog shares between the workstreams it
// judges: the records it read once, its clock and limits, and the judgment of
// each upstream a dependency wait names.
type stallPass struct {
	statuses    []trace.WorkstreamStatus
	escalations []trace.InboxEntry
	after       time.Duration
	now         time.Time
	limit       int
	// upstreams says, for each upstream judged in the pass, whether it explains
	// the quiet of the workstreams that wait on it; nil while it is being judged.
	upstreams map[config.WorkstreamID]*bool
}

// stallKey names a subject in the watchdog's clocks: a unit, or the workstream
// when unit is empty.
func stallKey(project config.ProjectID, stream config.WorkstreamID, unit string) string {
	return fmt.Sprintf("%s/%s/%s", project, stream, unit)
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
	// live is the latest time a non-chief turn running or queued was queued,
	// claimed or attempted, by unit; the empty unit holds all of them. done is
	// when the latest finished.
	live map[string]time.Time
	done map[string]time.Time
	// card is whether an owner card is open for the workstream. unitsHeld is
	// whether one of those holds its units too, and unitCards whether each unit
	// has a card of its own.
	card      bool
	unitsHeld bool
	unitCards map[string]bool
	// groupCard is whether the owner's approval of the shared workgroup explains
	// the quiet: the owner is asked for it, with every member's ratification
	// card in the inbox, and the workstream is not ratified yet.
	groupCard bool
	// paused is whether a pause covers the workstream.
	paused bool
	// waiting is whether the workstream waits on an upstream that is itself
	// progressing or explained.
	waiting bool
	// operations are the operations without a result, by unit; the empty unit
	// holds all of them.
	operations map[string][]scheduler.Operation
}

// workstreamHold is when the router last held a subject of the workstream
// itself, not one of its units or the stall of one of its workgroup changes.
func (r stallReading) workstreamHold() time.Time {
	var out time.Time
	for subject, at := range r.holds {
		if !strings.HasPrefix(subject, "unit-") && !strings.HasPrefix(subject, "unit_") && !isChangeSubject(subject) && at.After(out) {
			out = at
		}
	}
	return out
}

// streamQuiet is the quiet of the workstream itself: quiet since the later of
// its own progress, a hold of the router and the end of the latest turn of its
// workers.
func (r stallReading) streamQuiet() scheduler.Quiet {
	since := latest(r.created, r.progress.stream, r.workstreamHold(), latestOf(r.done))
	return scheduler.Quiet{Since: since, Card: r.card, Paused: r.paused, Turn: r.live[""], Waiting: r.waiting, Operations: r.operations[""]}
}

// read reads everything the watchdog judges a workstream and its units by.
func (w stallWatch) read(ctx context.Context, p *stallPass, status trace.WorkstreamStatus, feature trace.WorkflowState) (stallReading, error) {
	stream := status.Workstream
	r := stallReading{feature: feature, created: status.CreatedAt}
	var err error
	if r.progress, err = readStallProgress(w.repository, stream); err != nil {
		return r, err
	}
	if err := w.readHolds(&r, stream); err != nil {
		return r, err
	}
	if err := w.readTurns(&r, stream); err != nil {
		return r, err
	}
	if err := w.readCards(ctx, &r, status, p.escalations); err != nil {
		return r, err
	}
	if err := w.readWaits(ctx, &r, p, stream); err != nil {
		return r, err
	}
	if err := w.readOperations(&r, stream); err != nil {
		return r, err
	}
	if w.s.store != nil {
		st, _ := w.s.effective()
		_, r.paused = scheduler.Pausing(st.Pauses, w.repository.Project(), stream)
	}
	return r, nil
}

func (w stallWatch) workstream(ctx context.Context, p *stallPass, status trace.WorkstreamStatus) error {
	stream := status.Workstream
	feature, err := w.repository.Workflow(stream, trace.FeatureSubject)
	if err != nil {
		return err
	}
	if feature.Value == "" || feature.Value == DeliveredState || feature.Value == AbandonedState {
		return nil
	}
	r, err := w.read(ctx, p, status, feature)
	if err != nil {
		return err
	}
	now, after, limit := p.now, p.after, p.limit

	q := r.streamQuiet()
	raw, err := w.repository.RawWorkflow(stream, trace.FeatureSubject)
	if err != nil {
		return err
	}
	if err := w.assess(ctx, stream, "", raw, q, q.Since, r, now, after, limit); err != nil {
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
			turn = latest(turn, r.live[""])
		}
		// Only a card about the unit, or one of the workstream's that holds its
		// units, explains the unit's quiet.
		q := scheduler.Quiet{Since: since, Card: r.unitsHeld || r.unitCards[unit], Paused: r.paused, Turn: turn, Waiting: waiting, Operations: r.operations[unit]}
		if err := w.assess(ctx, stream, unit, state, q, since, r, now, after, limit); err != nil {
			return err
		}
	}
	return nil
}

// assess judges one subject and raises its stalled exit. A subject that is
// explained notes the time; a subject that is stalled is raised once for the
// stall, which is identified by the progress it has been quiet since.
func (w stallWatch) assess(ctx context.Context, stream config.WorkstreamID, unit string, state trace.WorkflowState, q scheduler.Quiet, identity time.Time, r stallReading, now time.Time, after time.Duration, limit int) error {
	key := stallKey(w.repository.Project(), stream, unit)
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
		// The router decided this stall and the chief of staff was told. A stall
		// still standing a further window later has the owner's recovery card.
		return w.openRecovery(ctx, stream, unit, state, *rec, now, after)
	}
	// A hold that stood for the window raises the stall again. A stall that Jev
	// held and held again is one nobody clears: it ends on the recovery card
	// instead of being asked a third time.
	held, err := w.heldStalls(stream, *x)
	if err != nil {
		return err
	}
	if len(held) >= maxStallHolds {
		return w.openRecovery(ctx, stream, unit, state, held[len(held)-1], now, after)
	}
	submit := w.submit
	if submit == nil {
		submit = func(x router.Exit) bool { return w.s.SubmitExit(x, nil) }
	}
	submit(*x)
	return nil
}

// maxStallHolds is how many times the router holds the stalled exit of a
// subject in one state before the stall ends on the owner's recovery card.
const maxStallHolds = 2

// heldStalls returns the earlier exits with the trigger of x, on its subject in
// its state, that the router held, oldest first.
func (w stallWatch) heldStalls(stream config.WorkstreamID, x router.Exit) ([]router.Record, error) {
	records, err := router.Records(w.repository, stream)
	if err != nil {
		return nil, err
	}
	key := router.OccurrenceKey(x)
	var out []router.Record
	for _, rec := range records {
		if rec.Subject == x.Subject && rec.Trigger == x.Trigger && rec.Version == x.Version && rec.State == router.StateHeld && rec.Occurrence != key {
			out = append(out, rec)
		}
	}
	slices.SortStableFunc(out, func(a, b router.Record) int { return a.Decided.Compare(b.Decided) })
	return out, nil
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
	body := fmt.Sprintf("%s has made no progress for %d minutes (since %s, %s) and nothing explains the quiet: no owner card about it, pause, running or queued turn younger than the window, operation in flight or wait on an upstream that is progressing.", strings.ToUpper(name[:1])+name[1:], minutes, last.UTC().Format(time.RFC3339), what)
	if len(failures) > 0 {
		body += " Last failure: " + strings.Join(failures, "; ") + "."
	}
	body += " " + stallTurnRule + " A turn on this notice that ends with none of them gets a check-back from the service."
	evidence := router.Evidence{Facts: facts, Records: records}
	reason := fmt.Sprintf("Stalled: %s made no progress for %d minutes with nothing to explain it", name, minutes)
	if unit == "" {
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
// did counts and neither does a drift rebase: a branch rebased onto upstream on
// its cadence moves no work of the workstream on.
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
// queued, with the latest time each was queued, claimed or attempted, and when
// the latest of them finished. A turn of the chief of staff explains nothing
// and is no progress.
func (w stallWatch) readTurns(r *stallReading, stream config.WorkstreamID) error {
	threads, err := w.repository.Threads(stream)
	if err != nil {
		return err
	}
	r.live, r.done = map[string]time.Time{}, map[string]time.Time{}
	for _, t := range threads {
		if t.Identity.Role == trace.ChiefOfStaff {
			continue
		}
		for _, q := range t.Turns {
			if q.CompletedAt.IsZero() {
				at := turnActivity(q)
				for _, unit := range []string{q.Request.Unit, ""} {
					if at.After(r.live[unit]) {
						r.live[unit] = at
					}
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

// turnActivity is the latest time a turn was queued, claimed or attempted.
func turnActivity(q trace.QueuedTurn) time.Time {
	at := q.Request.At
	if q.Claim != nil {
		at = latest(at, q.Claim.At)
	}
	for _, a := range q.Attempts {
		at = latest(at, a.At)
	}
	return at
}

// cardHoldsUnits reports whether a workstream's card of this kind holds its
// units as well: a card that stops the work of the whole workstream until the
// owner answers it. Any other card of the workstream explains only the
// workstream's own quiet.
func cardHoldsUnits(kind string) bool {
	switch kind {
	case InboxRatification, InboxDrift, InboxBase, InboxLoop, "project-selection":
		return true
	}
	return false
}

// ratifiedOrLater reports whether a workstream in this state has been ratified.
func ratifiedOrLater(state string) bool {
	switch state {
	case RatifiedState, BuildingState, AssembledState, DeliveredState:
		return true
	}
	return false
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
			continue
		}
		r.card = true
		if cardHoldsUnits(e.Kind) {
			r.unitsHeld = true
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
	// decide only while the owner is asked for it, with every member's
	// ratification card in the inbox, and the workstream is not ratified yet.
	if !ratifiedOrLater(r.feature.Value) {
		asked, err := w.s.intake().GroupApprovalAsked(ctx, w.repository, status.Workstream)
		if err != nil {
			return err
		}
		if asked {
			r.card, r.groupCard = true, true
		}
	}
	return nil
}

// readWaits reads whether the workstream waits on an upstream that explains its
// quiet: a live upstream (its same-repo base, or the source of an incoming
// dependency edge, that has not ended and is not already integrated) that has
// itself progressed within the window or is explained.
func (w stallWatch) readWaits(ctx context.Context, r *stallReading, p *stallPass, stream config.WorkstreamID) error {
	upstreams, err := liveDependencyUpstreams(w.repository, stream)
	if err != nil {
		return err
	}
	for _, upstream := range upstreams {
		explains, err := w.upstreamExplains(ctx, p, upstream)
		if err != nil {
			return err
		}
		if explains {
			r.waiting = true
			return nil
		}
	}
	return nil
}

// upstreamExplains reports whether an upstream a workstream waits on explains
// that workstream's quiet: it has progressed within the window, or something
// explains its own quiet. An upstream that is alive but stalled explains
// nothing, so the workstreams that wait on it stall too, and so does one that
// waits, through a cycle, on itself. A delivered upstream waits on its pull
// request, and one this project's records do not hold is not judged; both
// explain it.
func (w stallWatch) upstreamExplains(ctx context.Context, p *stallPass, upstream config.WorkstreamID) (bool, error) {
	if v, ok := p.upstreams[upstream]; ok {
		return v != nil && *v, nil
	}
	p.upstreams[upstream] = nil
	explains, err := w.judgeUpstream(ctx, p, upstream)
	if err != nil {
		delete(p.upstreams, upstream)
		return false, err
	}
	p.upstreams[upstream] = &explains
	return explains, nil
}

func (w stallWatch) judgeUpstream(ctx context.Context, p *stallPass, upstream config.WorkstreamID) (bool, error) {
	i := slices.IndexFunc(p.statuses, func(s trace.WorkstreamStatus) bool { return s.Workstream == upstream })
	if i < 0 {
		return true, nil
	}
	feature, err := w.repository.Workflow(upstream, trace.FeatureSubject)
	if err != nil {
		return false, err
	}
	switch feature.Value {
	case "", DeliveredState:
		return true, nil
	case AbandonedState:
		return false, nil
	}
	r, err := w.read(ctx, p, p.statuses[i], feature)
	if err != nil {
		return false, err
	}
	q := r.streamQuiet()
	q.Since = latest(q.Since, w.s.stalls.last(stallKey(w.repository.Project(), upstream, "")))
	return !scheduler.Assess(q, p.now, p.after, p.limit).Stalled, nil
}

// liveDependencyWait reports whether stream is held by a dependency wait whose
// upstreams are all alive. An upstream that is abandoned, closed or already in
// its project's base branch no longer explains the wait: the dependency exit
// decides it.
func liveDependencyWait(repository *trace.Repository, stream config.WorkstreamID) (bool, error) {
	upstreams, err := liveDependencyUpstreams(repository, stream)
	return len(upstreams) > 0, err
}

// liveDependencyUpstreams returns the upstreams of the dependency wait stream is
// held by, while they are alive: its same-repo base while the workstream is
// parked on it and it is not abandoned, and the sources of its unmerged
// hard-block edges while none of them has ended or is already integrated.
func liveDependencyUpstreams(repository *trace.Repository, stream config.WorkstreamID) ([]config.WorkstreamID, error) {
	var out []config.WorkstreamID
	if waiting, err := baseWaiting(repository, stream); err != nil {
		return nil, err
	} else if waiting {
		dependency, err := repository.WorkstreamBase(stream)
		if err != nil {
			return nil, err
		}
		if dependency.Base != "" {
			parent, err := repository.Workflow(dependency.Base, trace.FeatureSubject)
			if err != nil {
				return nil, err
			}
			if parent.Value != AbandonedState {
				out = append(out, dependency.Base)
			}
		}
	}
	waiting, err := edgeWaiting(repository, stream)
	if err != nil || !waiting {
		return out, err
	}
	edges, err := lifecycle.IncomingMergeEdges(repository, stream)
	if err != nil {
		return nil, err
	}
	observed, _, _, err := lifecycle.EdgeObservationAt(repository, stream)
	if err != nil {
		return nil, err
	}
	var live []config.WorkstreamID
	for _, e := range lifecycle.EdgesOfType(edges, trace.EdgeHardBlock) {
		i := slices.IndexFunc(observed.Edges, func(m lifecycle.EdgeMergeStatus) bool { return m.Upstream == e.From && m.Type == e.Type })
		if i >= 0 && observed.Edges[i].Merged {
			continue
		}
		if i >= 0 && (observed.Edges[i].Ended != "" || observed.Edges[i].Integrated != "") {
			return out, nil
		}
		live = append(live, e.From)
	}
	return append(out, live...), nil
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
		held := cause(stream, rec.Operation)
		// A hold explains the quiet only as far as its cause does here: a
		// dependency wait while its upstream explains it, and the owner's
		// approval of the workgroup while that explains it.
		switch {
		case held == scheduler.HeldByDependency && !r.waiting, held == scheduler.HeldByCard && !r.groupCard:
			held = scheduler.HeldByOther
		}
		op := operationFacts(rec, held)
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
