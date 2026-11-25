// Package slotspans sends one span per slot held and one per slot waited for
// in the pools that grant capacity: the scheduler's turn pools, the shared
// workflow reservations and the service's check pool. It tracks each open
// interval until its owner closes it, sends the span then with the interval's
// recorded start and end, and keeps the open intervals in a small ledger so a
// restart sends an interval that was open across it once, with its original
// start and the same IDs.
//
// Telemetry observes capacity and never decides it: every method is
// non-blocking, never fails its caller and does nothing when telemetry is off.
package slotspans

import (
	"cmp"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/telemetry"
	oteltrace "go.opentelemetry.io/otel/trace"
)

// Kind tells a held-slot interval from a wait interval.
type Kind string

const (
	// Held spans run from the moment a pool counts a slot as in use to the
	// moment it stops counting it.
	Held Kind = "held"
	// Wait spans run from the first capacity refusal to the end of the wait.
	Wait Kind = "wait"
)

// Where an interval comes from.
const (
	// SourceTurn: a scheduled turn of a thread.
	SourceTurn = "turn"
	// SourceReservation: a shared workflow reservation and the runner turns it covers.
	SourceReservation = "reservation"
	// SourceCheck: a run in the service's check pool.
	SourceCheck = "check"
)

// The outcomes an interval ends with.
const (
	// Released: a held slot was given back.
	Released = "released"
	// Granted: a wait ended because the work got its slot.
	Granted = "granted"
	// Canceled: the work was canceled before it got its slot.
	Canceled = "canceled"
	// Removed: the work disappeared before it got its slot.
	Removed = "removed"
	// Blocked: the work stopped waiting for capacity for another reason, such
	// as a pause, and is not waiting for a slot any more.
	Blocked = "blocked"
	// Abandoned: a wait stopped being refreshed, so its owner gave up on it.
	Abandoned = "abandoned"
	// Completed: the work finished without this wait being granted, such as a
	// workflow operation another run completed.
	Completed = "completed"
	// Interrupted: the service stopped while the interval was open and nothing
	// carried it over a restart.
	Interrupted = "interrupted"
)

// The wait reasons slot spans carry.
const (
	ReasonCapacity      = "capacity"
	ReasonWorkstreamCap = "workstream-cap"
	// ReasonClient: the role has room but the client the turn waits for has
	// no free slot.
	ReasonClient = "client"
	// ReasonNoClient: no connected client can take the turn.
	ReasonNoClient = "no-client"
)

// Interval is one held or wait interval of a pool, open until its owner ends it.
type Interval struct {
	Kind   Kind   `json:"kind"`
	Source string `json:"source"`
	Pool   string `json:"pool"`
	Role   string `json:"role"`
	// Project and Workstream place the span: a workstream's slot spans belong
	// to that workstream's trace.
	Project    config.ProjectID    `json:"project,omitempty"`
	Workstream config.WorkstreamID `json:"workstream,omitempty"`
	Unit       string              `json:"unit,omitempty"`
	// Agent is the thread of a turn or the runner thread a reservation covers.
	Agent string `json:"agent,omitempty"`
	// Client is the client whose slot the interval covers: the tailnet node's
	// stable ID, or local for the server's own machine. It is empty for work
	// no client runs and for a wait that no client is chosen for yet.
	Client string `json:"client,omitempty"`
	// Work names the work within its pool and source, so one piece of work has
	// one open interval of a kind.
	Work  string    `json:"work"`
	Start time.Time `json:"start"`
	// Limit is the pool's limit when the interval started; it is nil for a pool
	// without a limit and for one whose limit was not known.
	Limit     *int `json:"limit,omitempty"`
	Unbounded bool `json:"unbounded,omitempty"`
	// Reason is why work waited.
	Reason string `json:"reason,omitempty"`
	// Seen is when a reservation wait was last refused in this run. It is not
	// kept for a restart.
	Seen time.Time `json:"-"`
	// Recovered marks an interval read from the ledger that no owner has taken
	// up in this run.
	Recovered bool `json:"-"`
}

// Bound sets the interval's limit from a pool's limit: bounded pools carry the
// number, unbounded ones the marker.
func (iv *Interval) Bound(limit int, bounded bool) {
	if bounded {
		iv.Limit, iv.Unbounded = &limit, false
		return
	}
	iv.Limit, iv.Unbounded = nil, true
}

// Key identifies the interval among the open ones.
func (iv Interval) Key() string {
	return strings.Join([]string{string(iv.Kind), iv.Source, iv.Pool, string(iv.Project), string(iv.Workstream), iv.Work}, "\x00")
}

// SpanID is the span ID of the interval: derived from the workstream, the pool,
// the work and the recorded start, so it is the same in every service run.
func (iv Interval) SpanID() oteltrace.SpanID {
	start := strconv.FormatInt(iv.Start.UnixNano(), 10)
	if iv.Workstream == "" {
		return telemetry.FactorySpan(factoryRun, "slot", string(iv.Kind), iv.Source, iv.Pool, string(iv.Project), iv.Work, start)
	}
	return telemetry.WorkstreamSpan(iv.Workstream, "slot", string(iv.Kind), iv.Source, iv.Pool, iv.Work, start)
}

const factoryRun = "slots"

// Recorder tracks open intervals and sends their spans. A nil Recorder does
// nothing.
type Recorder struct {
	sink    *telemetry.Sink
	now     func() time.Time
	logger  *slog.Logger
	path    string
	started time.Time

	mu        sync.Mutex
	open      map[string]Interval
	seq       uint64
	recovered int

	dirty  chan struct{}
	stop   chan struct{}
	exited chan struct{}
	once   sync.Once
}

// Options tune a Recorder.
type Options struct {
	// Ledger is the file that keeps the open intervals across a restart. Empty
	// keeps them in memory only.
	Ledger string
	Logger *slog.Logger
	// Now defaults to time.Now.
	Now func() time.Time
}

// New returns the recorder that sends spans to sink, or nil when sink is off:
// with telemetry off nothing is tracked and no ledger is written. Intervals the
// ledger holds from an earlier run are open again, with their recorded starts.
func New(sink *telemetry.Sink, opts Options) *Recorder {
	if !sink.Enabled() {
		return nil
	}
	r := &Recorder{sink: sink, now: opts.Now, logger: opts.Logger, path: opts.Ledger, open: map[string]Interval{},
		dirty: make(chan struct{}, 1), stop: make(chan struct{}), exited: make(chan struct{})}
	if r.now == nil {
		r.now = time.Now
	}
	r.started = r.now()
	if r.path != "" {
		r.load()
		go r.write()
	} else {
		close(r.exited)
	}
	return r
}

type ledgerFile struct {
	Version   int        `json:"version"`
	Intervals []Interval `json:"intervals"`
}

func (r *Recorder) load() {
	data, err := os.ReadFile(r.path)
	if err != nil {
		if !os.IsNotExist(err) {
			r.warn("slot intervals ledger could not be read; open intervals are lost", err)
		}
		return
	}
	var f ledgerFile
	if err := json.Unmarshal(data, &f); err != nil {
		r.warn("slot intervals ledger is malformed; open intervals are lost", err)
		return
	}
	for _, iv := range f.Intervals {
		if iv.Start.IsZero() || iv.Work == "" || (iv.Kind != Held && iv.Kind != Wait) {
			continue
		}
		iv.Recovered = true
		r.open[iv.Key()] = iv
		r.recovered++
	}
}

func (r *Recorder) warn(msg string, err error) {
	if r.logger != nil {
		r.logger.Warn(msg, "component", "telemetry", "error", err.Error())
	}
}

func (r *Recorder) write() {
	defer close(r.exited)
	for {
		select {
		case <-r.dirty:
			r.save()
		case <-r.stop:
			select {
			case <-r.dirty:
			default:
			}
			r.save()
			return
		}
	}
}

func (r *Recorder) save() {
	r.mu.Lock()
	f := ledgerFile{Version: 1}
	for _, iv := range r.open {
		f.Intervals = append(f.Intervals, iv)
	}
	r.mu.Unlock()
	slices.SortFunc(f.Intervals, func(a, b Interval) int { return cmp.Compare(a.Key(), b.Key()) })
	data, err := json.Marshal(f)
	if err == nil {
		err = writeAtomic(r.path, data)
	}
	if err != nil {
		r.warn("slot intervals ledger could not be written", err)
	}
}

func writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".slot-intervals-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	_, err = tmp.Write(data)
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(name, path)
	}
	if err != nil {
		os.Remove(name)
	}
	return err
}

func (r *Recorder) changed(iv Interval) {
	if r.path == "" {
		return
	}
	select {
	case r.dirty <- struct{}{}:
	default:
	}
}

// Close writes the ledger and stops its writer. The recorder keeps tracking
// afterwards but no longer saves.
func (r *Recorder) Close() {
	if r == nil {
		return
	}
	r.once.Do(func() { close(r.stop) })
	<-r.exited
}

// Sequence returns a number that is unique within this run, for work that has
// no identity of its own.
func (r *Recorder) Sequence() uint64 {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	return r.seq
}

// Begin opens the interval iv unless one is already open for the same work, and
// returns the open interval and whether it is new. An interval recovered from
// the ledger is kept as it was recorded, original start and limit included.
func (r *Recorder) Begin(iv Interval) (Interval, bool) {
	if r == nil {
		return iv, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if have, ok := r.open[iv.Key()]; ok {
		if refusalTracked(have) {
			have.Seen = iv.Start
		}
		if have.Recovered {
			have.Recovered = false
			r.recovered--
		}
		r.open[have.Key()] = have
		return have, false
	}
	if refusalTracked(iv) {
		iv.Seen = iv.Start
	}
	r.open[iv.Key()] = iv
	r.changed(iv)
	return iv, true
}

// Find returns the open intervals of kind and source for which match is true.
// A nil match selects them all.
func (r *Recorder) Find(kind Kind, source string, match func(Interval) bool) []Interval {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Interval
	for _, iv := range r.open {
		if iv.Kind == kind && iv.Source == source && (match == nil || match(iv)) {
			out = append(out, iv)
		}
	}
	slices.SortFunc(out, func(a, b Interval) int { return cmp.Compare(a.Key(), b.Key()) })
	return out
}

// End closes the interval for the same work as iv at end, with outcome, and
// sends its span. An end before the start is clamped to the start. It reports
// whether an interval was open.
func (r *Recorder) End(iv Interval, end time.Time, outcome string) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	have, ok := r.open[iv.Key()]
	if ok {
		delete(r.open, iv.Key())
		if have.Recovered {
			r.recovered--
		}
		r.changed(have)
	}
	r.mu.Unlock()
	if !ok {
		return false
	}
	r.send(have, end, outcome)
	return true
}

func refusalTracked(iv Interval) bool { return iv.Kind == Wait && iv.Source == SourceReservation }

// EndRecovered closes the interval for the same work as iv like End, but only
// while it is still one that no owner has taken up in this run: an owner that
// asked for the slot again keeps the interval to itself.
func (r *Recorder) EndRecovered(iv Interval, end time.Time, outcome string) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	have, ok := r.open[iv.Key()]
	if ok && have.Recovered {
		delete(r.open, iv.Key())
		r.recovered--
		r.changed(have)
	} else {
		ok = false
	}
	r.mu.Unlock()
	if !ok {
		return false
	}
	r.send(have, end, outcome)
	return true
}

// Touch records that the reservation wait for the same work as iv was refused again at at.
func (r *Recorder) Touch(iv Interval, at time.Time) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if have, ok := r.open[iv.Key()]; ok && refusalTracked(have) {
		have.Seen = at
		r.open[iv.Key()] = have
	}
}

// Abandon closes the reservation waits last refused before cutoff, at that last
// refusal, so a wait whose owner stopped asking does not stay open. A wait
// recovered from the ledger has no refusal in this run: its operation's fate
// decides how it ends.
func (r *Recorder) Abandon(cutoff time.Time) {
	if r == nil {
		return
	}
	for _, iv := range r.Find(Wait, SourceReservation, func(iv Interval) bool { return !iv.Recovered && iv.Seen.Before(cutoff) }) {
		r.End(iv, iv.Seen, Abandoned)
	}
}

// RecoveryGrace is how long after a start the intervals a restart left open
// wait for their owners to take them up before they are sent as interrupted.
const RecoveryGrace = 10 * time.Minute

// Expire sends as interrupted, ending where the earlier run stopped, the
// intervals recovered from the ledger that no owner took up within the
// RecoveryGrace of the run's start; at is the current time.
func (r *Recorder) Expire(at time.Time) {
	if r == nil {
		return
	}
	r.mu.Lock()
	pending := r.recovered > 0 && at.After(r.started.Add(RecoveryGrace))
	var stale []Interval
	if pending {
		for _, iv := range r.open {
			if iv.Recovered {
				stale = append(stale, iv)
			}
		}
	}
	r.mu.Unlock()
	for _, iv := range stale {
		r.End(iv, r.started, Interrupted)
	}
}

// Now is the recorder's clock.
func (r *Recorder) Now() time.Time {
	if r == nil {
		return time.Time{}
	}
	return r.now()
}

// Interrupt closes every open interval of pool from before this run at the
// start of the run, for a pool whose holders and waiters do not survive a
// restart.
func (r *Recorder) Interrupt(pool string) {
	if r == nil {
		return
	}
	for _, kind := range []Kind{Held, Wait} {
		for _, iv := range r.Find(kind, SourceCheck, func(iv Interval) bool { return iv.Pool == pool && iv.Start.Before(r.started) }) {
			r.End(iv, r.started, Interrupted)
		}
	}
}

func (r *Recorder) send(iv Interval, end time.Time, outcome string) {
	if end.Before(iv.Start) {
		end = iv.Start
	}
	attrs := []telemetry.Attr{telemetry.Role(iv.Role), telemetry.Pool(iv.Pool), telemetry.PoolRoles(telemetry.PoolRolesOf(iv.Pool)), telemetry.Outcome(outcome)}
	if iv.Project != "" {
		attrs = append(attrs, telemetry.Project(iv.Project))
	}
	if iv.Workstream != "" {
		attrs = append(attrs, telemetry.Workstream(iv.Workstream))
	}
	if iv.Unit != "" {
		attrs = append(attrs, telemetry.Unit(iv.Unit))
	}
	if iv.Client != "" {
		attrs = append(attrs, telemetry.Client(iv.Client))
	}
	switch {
	case iv.Unbounded:
		attrs = append(attrs, telemetry.PoolUnbounded())
	case iv.Limit != nil:
		attrs = append(attrs, telemetry.PoolLimit(*iv.Limit))
	}
	if iv.Kind == Wait && iv.Reason != "" {
		attrs = append(attrs, telemetry.WaitReason(iv.Reason))
	}
	span := telemetry.Span{Name: telemetry.SpanName("slot", string(iv.Kind)), ID: iv.SpanID(), Start: iv.Start, End: end, Attrs: attrs}
	if iv.Workstream == "" {
		span.Trace = telemetry.FactoryTrace(factoryRun)
	} else {
		span.Trace, span.Parent = telemetry.WorkstreamTrace(iv.Workstream), telemetry.WorkstreamRoot(iv.Workstream)
	}
	r.sink.Emit(span)
}
