package service

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/telemetry"
	oteltrace "go.opentelemetry.io/otel/trace"
)

const (
	factoryRun       = "service"
	ledgerVersion    = 1
	factoryHeartbeat = 30 * time.Second
)

const (
	limitReloaded    = "reloaded"
	limitShutdown    = "shutdown"
	limitInterrupted = "interrupted"
	pauseReleased    = "released"
	pauseReplaced    = "replaced"
)

type poolSpec struct {
	ID        string   `json:"id"`
	Roles     []string `json:"roles"`
	Limit     int      `json:"limit,omitempty"`
	Unbounded bool     `json:"unbounded,omitempty"`
}

func poolsOf(c config.Capacity) []poolSpec {
	bounded := func(role string, limit int) poolSpec {
		id := telemetry.PoolID(role)
		return poolSpec{ID: id, Roles: telemetry.PoolRolesOf(id), Limit: limit}
	}
	unbounded := func(role string) poolSpec {
		id := telemetry.PoolID(role)
		return poolSpec{ID: id, Roles: telemetry.PoolRolesOf(id), Unbounded: true}
	}
	return []poolSpec{
		bounded(masonRole, c.Masons),
		bounded(reviewerRole, c.Reviewers),
		bounded(committeeRole, c.Committee),
		unbounded(architectRole),
		bounded(checksRole, c.Checks),
	}
}

func (p poolSpec) sameLimit(o poolSpec) bool {
	return p.ID == o.ID && p.Limit == o.Limit && p.Unbounded == o.Unbounded && slices.Equal(p.Roles, o.Roles)
}

type poolInterval struct {
	poolSpec
	Start time.Time `json:"start"`
}

type intervalLedger struct {
	Version int            `json:"version"`
	Started time.Time      `json:"started"`
	Alive   time.Time      `json:"alive"`
	Pools   []poolInterval `json:"pools"`
}

type factorySpans struct {
	sink   *telemetry.Sink
	now    func() time.Time
	log    *slog.Logger
	ledger string

	mu      sync.Mutex
	active  bool
	started time.Time
	pools   []poolInterval
	stop    chan struct{}
	stopped chan struct{}
	warned  bool
}

func newFactorySpans(sink *telemetry.Sink, ledger string, now func() time.Time, log *slog.Logger) *factorySpans {
	return &factorySpans{sink: sink, ledger: ledger, now: now, log: log}
}

func (f *factorySpans) enabled() bool { return f != nil && f.sink.Enabled() }

func (f *factorySpans) trace() oteltrace.TraceID { return telemetry.FactoryTrace(factoryRun) }

func (f *factorySpans) uptimeID(started time.Time) oteltrace.SpanID {
	return telemetry.FactorySpan(factoryRun, "uptime", strconv.FormatInt(started.UnixNano(), 10))
}

func (f *factorySpans) poolAttrs(p poolSpec) []telemetry.Attr {
	attrs := []telemetry.Attr{telemetry.Pool(p.ID), telemetry.PoolRoles(p.Roles)}
	if p.Unbounded {
		return append(attrs, telemetry.PoolUnbounded())
	}
	return append(attrs, telemetry.PoolLimit(p.Limit))
}

func (f *factorySpans) begin(pools []poolSpec) {
	if !f.enabled() {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recoverPrevious()
	at := f.now()
	f.active, f.started = true, at
	f.pools = f.pools[:0]
	for _, p := range pools {
		f.pools = append(f.pools, poolInterval{poolSpec: p, Start: at})
	}
	f.sink.Emit(f.instant("start", telemetry.SpanName("service", "start"), at, nil))
	f.save(at)
	f.stop, f.stopped = make(chan struct{}), make(chan struct{})
	go f.heartbeat(f.stop, f.stopped)
}

func (f *factorySpans) instant(id string, name telemetry.Name, at time.Time, attrs []telemetry.Attr, ids ...string) telemetry.Span {
	return telemetry.Span{Name: name, Trace: f.trace(), ID: telemetry.FactorySpan(factoryRun, append([]string{id, strconv.FormatInt(f.started.UnixNano(), 10)}, ids...)...),
		Parent: f.uptimeID(f.started), Start: at, End: at, Attrs: attrs, Events: []telemetry.Event{{Name: name, At: at}}}
}

func (f *factorySpans) recoverPrevious() {
	if f.ledger == "" {
		return
	}
	data, err := os.ReadFile(f.ledger)
	if err != nil {
		return
	}
	os.Remove(f.ledger)
	var prev intervalLedger
	if json.Unmarshal(data, &prev) != nil || prev.Version != ledgerVersion || prev.Started.IsZero() {
		return
	}
	end := prev.Alive
	if end.Before(prev.Started) {
		end = prev.Started
	}
	for _, p := range prev.Pools {
		f.emitPool(p, end, limitInterrupted, prev.Started)
	}
	f.sink.Emit(f.uptime(prev.Started, end, limitInterrupted))
}

func (f *factorySpans) uptime(started, end time.Time, outcome string) telemetry.Span {
	return telemetry.Span{Name: telemetry.SpanName("service", "uptime"), Trace: f.trace(), ID: f.uptimeID(started), Start: started, End: end,
		Attrs: []telemetry.Attr{telemetry.Outcome(outcome)}}
}

func (f *factorySpans) emitPool(p poolInterval, end time.Time, outcome string, run time.Time) {
	if end.Before(p.Start) {
		end = p.Start
	}
	f.sink.Emit(telemetry.Span{Name: telemetry.SpanName("pool", "limit"), Trace: f.trace(),
		ID:     telemetry.FactorySpan(factoryRun, "pool-limit", p.ID, strconv.FormatInt(p.Start.UnixNano(), 10)),
		Parent: f.uptimeID(run), Start: p.Start, End: end,
		Attrs: append(f.poolAttrs(p.poolSpec), telemetry.Outcome(outcome))})
}

func (f *factorySpans) reload(at time.Time, pools []poolSpec) {
	if !f.enabled() {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.active {
		return
	}
	changed := false
	next := make([]poolInterval, 0, len(pools))
	for _, p := range pools {
		i := slices.IndexFunc(f.pools, func(o poolInterval) bool { return o.ID == p.ID })
		if i >= 0 && f.pools[i].sameLimit(p) {
			next = append(next, f.pools[i])
			continue
		}
		changed = true
		if i >= 0 {
			f.emitPool(f.pools[i], at, limitReloaded, f.started)
		}
		next = append(next, poolInterval{poolSpec: p, Start: later(at, f.started)})
	}
	for _, o := range f.pools {
		if !slices.ContainsFunc(pools, func(p poolSpec) bool { return p.ID == o.ID }) {
			changed = true
			f.emitPool(o, at, limitReloaded, f.started)
		}
	}
	f.pools = next
	if changed {
		f.save(at)
	}
}

func later(a, b time.Time) time.Time {
	if a.Before(b) {
		return b
	}
	return a
}

func (f *factorySpans) end() {
	if !f.enabled() {
		return
	}
	f.mu.Lock()
	if !f.active {
		f.mu.Unlock()
		return
	}
	f.active = false
	stop, stopped := f.stop, f.stopped
	at := later(f.now(), f.started)
	for _, p := range f.pools {
		f.emitPool(p, at, limitShutdown, f.started)
	}
	f.sink.Emit(f.uptime(f.started, at, limitShutdown))
	f.pools = nil
	if f.ledger != "" {
		os.Remove(f.ledger)
	}
	f.mu.Unlock()
	close(stop)
	<-stopped
}

func (f *factorySpans) heartbeat(stop <-chan struct{}, stopped chan<- struct{}) {
	defer close(stopped)
	ticker := time.NewTicker(factoryHeartbeat)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			f.mu.Lock()
			if f.active {
				f.save(f.now())
			}
			f.mu.Unlock()
		}
	}
}

func (f *factorySpans) save(alive time.Time) {
	if f.ledger == "" {
		return
	}
	data, err := json.Marshal(intervalLedger{Version: ledgerVersion, Started: f.started, Alive: alive, Pools: f.pools})
	if err == nil {
		tmp := f.ledger + ".tmp"
		if err = os.WriteFile(tmp, data, 0o600); err == nil {
			err = os.Rename(tmp, f.ledger)
		}
	}
	if err != nil && !f.warned {
		f.warned = true
		if f.log != nil {
			f.log.Warn("telemetry cannot record the open pool limit intervals; a stop without shutdown leaves them unsent", "component", "telemetry", "error", err.Error())
		}
	}
}

func (f *factorySpans) pauseEnded(p runtime.Pause, at time.Time, replaced bool) {
	if !f.enabled() || p.SetAt.IsZero() {
		return
	}
	outcome := pauseReleased
	if replaced {
		outcome = pauseReplaced
	}
	attrs := []telemetry.Attr{telemetry.PauseScope(p.Target.Scope), telemetry.PauseSource(p.Source), telemetry.Outcome(outcome)}
	if p.Target.Project != "" {
		attrs = append(attrs, telemetry.Project(p.Target.Project))
	}
	if p.Target.Workstream != "" {
		attrs = append(attrs, telemetry.Workstream(p.Target.Workstream))
	}
	if p.Target.Role != "" {
		attrs = append(attrs, telemetry.Role(p.Target.Role))
	}
	if at.Before(p.SetAt) {
		at = p.SetAt
	}
	f.sink.Emit(telemetry.Span{Name: telemetry.SpanName("pause"), Trace: f.trace(),
		ID:    telemetry.FactorySpan(factoryRun, "pause", p.Target.Scope, string(p.Target.Project), string(p.Target.Workstream), p.Target.Role, p.Source, strconv.FormatInt(p.SetAt.UnixNano(), 10)),
		Start: p.SetAt, End: at, Attrs: attrs})
}

func (f *factorySpans) loopGuardTrip(project config.ProjectID, stream config.WorkstreamID, record string, at time.Time) {
	if !f.enabled() {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.active {
		return
	}
	f.sink.Emit(f.instant("loop-guard-trip", telemetry.SpanName("loop_guard", "trip"), later(at, f.started), []telemetry.Attr{
		telemetry.Project(project), telemetry.Workstream(stream), telemetry.Record(record),
		telemetry.PauseScope("workstream"), telemetry.PauseSource(runtime.PauseLoopGuard)}, string(project), string(stream), record))
}

func ledgerPath(root config.Root) string {
	p, err := root.ServiceIntervals()
	if err != nil {
		return ""
	}
	return filepath.Clean(p)
}
