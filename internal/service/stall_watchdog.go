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

var stallActor = trace.Actor{Kind: "service", ID: "stall-watchdog"}

type stallClocks struct {
	mu        sync.Mutex
	explained map[string]time.Time
	ran       map[config.ProjectID]time.Time
}

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

const stallCheckEvery = time.Minute

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

func (c *stallClocks) last(key string) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.explained[key]
}

type stallWatch struct {
	s          *Service
	cfg        *config.Config
	repository *trace.Repository
	submit     func(router.Exit) bool
}

func (w stallWatch) Run(ctx context.Context) error {
	if w.cfg.StallWindow() <= 0 || !w.s.stalls.due(w.repository.Project(), time.Now(), stallCheckEvery) {
		return nil
	}
	if err := w.Pass(ctx); err != nil && ctx.Err() == nil {
		w.s.log().Warn("the stall watchdog could not read the project", "project", string(w.repository.Project()), "error", err.Error())
	}
	return nil
}

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
	p := &stallPass{statuses: statuses, escalations: escalations, after: after, now: w.s.now(), limit: w.s.GiveUpLimit().Failures, upstreams: map[config.WorkstreamID]*bool{}}
	var failed []error
	for _, stream := range streams {
		if err := ctx.Err(); err != nil {
			return err
		}
		i := slices.IndexFunc(statuses, func(s trace.WorkstreamStatus) bool { return s.Workstream == stream })
		if i < 0 {
			continue
		}
		if err := w.workstream(ctx, p, statuses[i]); err != nil {
			failed = append(failed, fmt.Errorf("workstream %s: %w", stream, err))
		}
		if err := w.changes(ctx, p, stream); err != nil {
			failed = append(failed, fmt.Errorf("workgroup changes of %s: %w", stream, err))
		}
	}
	if err := w.crossRepoChanges(ctx, p, streams); err != nil {
		failed = append(failed, err)
	}
	return errors.Join(failed...)
}

type stallPass struct {
	statuses    []trace.WorkstreamStatus
	escalations []trace.InboxEntry
	after       time.Duration
	now         time.Time
	limit       int
	upstreams   map[config.WorkstreamID]*bool
}

func stallKey(project config.ProjectID, stream config.WorkstreamID, unit string) string {
	return fmt.Sprintf("%s/%s/%s", project, stream, unit)
}

type stallReading struct {
	feature    trace.WorkflowState
	created    time.Time
	progress   stallProgress
	holds      map[string]time.Time
	live       map[string]time.Time
	done       map[string]time.Time
	card       bool
	unitsHeld  bool
	unitCards  map[string]bool
	groupCard  bool
	paused     bool
	waiting    bool
	operations map[string][]scheduler.Operation
}

func (r stallReading) workstreamHold() time.Time {
	var out time.Time
	for subject, at := range r.holds {
		if !strings.HasPrefix(subject, "unit-") && !strings.HasPrefix(subject, "unit_") && !isChangeSubject(subject) && at.After(out) {
			out = at
		}
	}
	return out
}

func (r stallReading) streamQuiet() scheduler.Quiet {
	since := latest(r.created, r.progress.stream, r.workstreamHold(), latestOf(r.done))
	return scheduler.Quiet{Since: since, Card: r.card, Paused: r.paused, Turn: r.live[""], Waiting: r.waiting, Operations: r.operations[""]}
}

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

	if feature.Value != BuildingState && feature.Value != AssembledState {
		return nil
	}
	states, err := w.repository.WorkflowStates(stream)
	if err != nil {
		return err
	}
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
		waiting := r.waiting
		if state.Value == UnitPlanned {
			waiting = waiting || slices.ContainsFunc(deps[unit], func(id string) bool { return !merged(id) })
		}
		turn := r.live[unit]
		if state.Value == UnitReady {
			turn = latest(turn, r.live[""])
		}
		q := scheduler.Quiet{Since: since, Card: r.unitsHeld || r.unitCards[unit], Paused: r.paused, Turn: turn, Waiting: waiting, Operations: r.operations[unit]}
		if err := w.assess(ctx, stream, unit, state, q, since, r, now, after, limit); err != nil {
			return err
		}
	}
	return nil
}

func (w stallWatch) assess(ctx context.Context, stream config.WorkstreamID, unit string, state trace.WorkflowState, q scheduler.Quiet, identity time.Time, r stallReading, now time.Time, after time.Duration, limit int) error {
	key := stallKey(w.repository.Project(), stream, unit)
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
		return w.openRecovery(ctx, stream, unit, state, *rec, now, after)
	}
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

const maxStallHolds = 2

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

type stallProgress struct {
	stream     time.Time
	streamAt   time.Time
	streamWhat string
	units      map[string]time.Time
	unitWhat   map[string]string
	names      map[string]string
	owner      time.Time
}

func (p stallProgress) unitOf(subject string) (string, bool) {
	if unit, ok := p.names[subject]; ok {
		return unit, true
	}
	if unit, ok := strings.CutPrefix(subject, "unit-"); ok {
		return unit, true
	}
	return "", false
}

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
	for unit, at := range r.done {
		if unit != "" && at.After(r.done[""]) {
			r.done[""] = at
		}
	}
	return nil
}

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

func cardHoldsUnits(kind string) bool {
	switch kind {
	case InboxRatification, InboxDrift, InboxBase, InboxLoop:
		return true
	}
	return false
}

func ratifiedOrLater(state string) bool {
	switch state {
	case RatifiedState, BuildingState, AssembledState, DeliveredState:
		return true
	}
	return false
}

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

func liveDependencyWait(repository *trace.Repository, stream config.WorkstreamID) (bool, error) {
	upstreams, err := liveDependencyUpstreams(repository, stream)
	return len(upstreams) > 0, err
}

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
		if _, err := thread.DecodeTurn(rec.Operation); err == nil {
			continue
		}
		held := cause(stream, rec.Operation)
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

func operationFacts(rec trace.OperationRecord, held scheduler.HoldCause) scheduler.Operation {
	status := reconcile.StatusOf(rec)
	return scheduler.Operation{
		Action: rec.Operation.Action, Failures: status.Failures, LastFailure: status.LastFailure,
		LastAttempt: latest(status.LastAttempt, rec.Transition.At), Held: held,
	}
}

func operationUnitOf(op coreadapter.Operation) string {
	var in struct {
		Unit string `json:"unit"`
	}
	if json.Unmarshal(op.Input, &in) != nil {
		return ""
	}
	return in.Unit
}
