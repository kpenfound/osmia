// Package clientturns runs turns on clients that connected to the service
// over the tailnet. A turn's session runs on the client's machine, with the
// client's own view and skills; the service keeps everything else: the trace,
// the tool handlers and their audit, every git write, and the capture of the
// view the client returns.
//
// The Dispatcher offers a turn to a client through the fleet registry, hands
// the client its view (a commit to fetch, or an archive), relays the client's
// tool calls to the turn's own MCP endpoint, carries out the view and skill
// operations the tools make on the client's machine, takes the finished view
// and the result back, and answers each return definitively: accepted, already
// recorded or superseded.
package clientturns

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/kpenfound/osmia/internal/activity"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/filetransport"
	"github.com/kpenfound/osmia/internal/fleet"
	"github.com/kpenfound/osmia/internal/ghlog"
	"github.com/kpenfound/osmia/internal/isolation"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
)

// The waits of turn dispatch. They are engineering choices, not settings.
const (
	// DefaultOfferWait is how long a client has to claim an offered turn.
	DefaultOfferWait = time.Minute
	// DefaultLateWait is how long the service waits, after a client's lease
	// ends, for the view the client returns, before it starts the recovery of
	// the interrupted turn.
	DefaultLateWait = 3 * time.Minute
	// DefaultViewWait is how long the service waits for a view that a client
	// that sent its result, or was told to cancel, still has to send.
	DefaultViewWait = 30 * time.Second
	// DefaultHold is how long a turn a previous service session started on a
	// client is held for the client to reattach it.
	DefaultHold = fleet.DefaultGrace
)

// The limits on the session files a result returns.
const (
	maxSessionFiles = 64
	maxSessionBytes = 8 << 20
)

// Binding is what a project supplies for turns that finish without the
// goroutine that started them, as after a restart: its trace, its thread
// runner and its capture of a returned view.
type Binding struct {
	Repository *trace.Repository
	Runner     thread.Runner
	Capture    func(context.Context, coreadapter.Scope, *isolation.FileView, coreadapter.SessionResult) error
}

// Dispatcher dispatches turns to the clients of a fleet.
type Dispatcher struct {
	Fleet     *fleet.Registry
	Transfers *filetransport.Registry
	Relay     *Relay
	Activity  *activity.Registry
	// Directory holds a record of every turn offered to a client, which
	// outlives the process: the turn's client, the digest of its token and where
	// its trace is. Views is the directory finished views are kept under until
	// they are captured.
	Directory string
	Views     string
	Now       func() time.Time

	OfferWait, LateWait, ViewWait, Hold time.Duration

	mu       sync.Mutex
	bindings map[config.ProjectID]Binding
	states   map[string]*turnState
	choose   Chooser
}

// Chooser chooses the node a turn runs on. It reports false to run the turn on
// the built-in local client.
type Chooser func(ctx context.Context, scope coreadapter.Scope, profile coreadapter.Profile) (fleet.Identity, bool)

// SetChooser sets how turns are placed. Without one every turn runs on the
// built-in local client.
func (d *Dispatcher) SetChooser(c Chooser) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.choose = c
}

// Placer places each turn on the client the chooser picks. It returns no
// client for a turn that runs on the built-in local client, which the turn's
// own engine then runs in this process.
func (d *Dispatcher) Placer() isolation.Placer {
	return func(ctx context.Context, scope coreadapter.Scope, profile coreadapter.Profile) (isolation.Client, error) {
		d.mu.Lock()
		choose := d.choose
		d.mu.Unlock()
		if choose != nil {
			if node, remote := choose(ctx, scope, profile); remote {
				return d.Remote(node), nil
			}
		}
		return nil, nil
	}
}

// New returns a dispatcher and connects it to its fleet and file transport.
func New(d Dispatcher) *Dispatcher {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.OfferWait <= 0 {
		d.OfferWait = DefaultOfferWait
	}
	if d.LateWait <= 0 {
		d.LateWait = DefaultLateWait
	}
	if d.ViewWait <= 0 {
		d.ViewWait = DefaultViewWait
	}
	if d.Hold <= 0 {
		d.Hold = DefaultHold
	}
	if d.Relay == nil {
		d.Relay = NewRelay()
	}
	out := &Dispatcher{Fleet: d.Fleet, Transfers: d.Transfers, Relay: d.Relay, Activity: d.Activity, Directory: d.Directory, Views: d.Views, Now: d.Now,
		OfferWait: d.OfferWait, LateWait: d.LateWait, ViewWait: d.ViewWait, Hold: d.Hold,
		bindings: map[config.ProjectID]Binding{}, states: map[string]*turnState{}}
	if out.Fleet != nil {
		out.Fleet.SetHooks(out.Hooks())
	}
	if out.Transfers != nil {
		out.Transfers.SetLookup(out.lookup)
	}
	return out
}

// Hooks are the fleet hooks that connect the protocol to turn dispatch.
func (d *Dispatcher) Hooks() fleet.Hooks {
	return fleet.Hooks{Progress: d.progress, Reattach: d.reattach, Result: d.result, LeaseEnded: d.leaseEnded}
}

// Bind supplies what a project's turns need to finish without their
// goroutine.
func (d *Dispatcher) Bind(project config.ProjectID, b Binding) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.bindings[project] = b
}

func (d *Dispatcher) binding(project config.ProjectID) (Binding, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	b, ok := d.bindings[project]
	return b, ok
}

// Remote returns the client of a tailnet node: turns placed on it run on that
// node's machine.
func (d *Dispatcher) Remote(node fleet.Identity) isolation.Client {
	return &remote{d: d, node: node}
}

type turnRecord struct {
	Turn       string    `json:"turn"`
	Project    string    `json:"project"`
	Workstream string    `json:"workstream"`
	Unit       string    `json:"unit,omitempty"`
	Thread     string    `json:"thread"`
	Agent      string    `json:"agent"`
	Role       string    `json:"role"`
	Node       string    `json:"node"`
	Name       string    `json:"name,omitempty"`
	TokenSum   string    `json:"token_sum"`
	ReturnView bool      `json:"return_view,omitempty"`
	Directory  string    `json:"session_directory,omitempty"`
	At         time.Time `json:"at"`
}

func (r turnRecord) scope() coreadapter.Scope {
	return coreadapter.Scope{Project: r.Project, Workstream: r.Workstream, Unit: r.Unit, Thread: r.Thread, Turn: r.Turn, Role: r.Role}
}

func (d *Dispatcher) recordPath(turn string) string {
	return filepath.Join(d.Directory, "turns", turn+".json")
}

func (d *Dispatcher) saveRecord(r turnRecord) error {
	if d.Directory == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(d.recordPath(r.Turn)), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	path := d.recordPath(r.Turn)
	if err := os.WriteFile(path+".tmp", data, 0o600); err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
}

func (d *Dispatcher) loadRecord(turn string) (turnRecord, bool) {
	if d.Directory == "" || turn == "" || strings.ContainsAny(turn, "/\\") {
		return turnRecord{}, false
	}
	data, err := os.ReadFile(d.recordPath(turn))
	if err != nil {
		return turnRecord{}, false
	}
	var r turnRecord
	if json.Unmarshal(data, &r) != nil || r.Turn != turn {
		return turnRecord{}, false
	}
	return r, true
}

type turnState struct {
	d   *Dispatcher
	rec turnRecord

	mu          sync.Mutex
	viewReady   chan struct{}
	resultReady chan struct{}
	lost        chan struct{}
	lostOnce    sync.Once
	result      *api.TurnResult
	viewDir     string
	viewTaken   bool
	resultTaken bool
	finished    bool
	adopted     bool
	deadline    time.Time
	endedAt     time.Time
	release     []func()
	activity    *activity.Handle
	lastPhase   string
	lastFrac    *float64
	startedAt   time.Time
}

func newState(d *Dispatcher, rec turnRecord) *turnState {
	return &turnState{d: d, rec: rec, viewReady: make(chan struct{}), resultReady: make(chan struct{}), lost: make(chan struct{})}
}

func (d *Dispatcher) liveState(turn string) *turnState {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.states[turn]
}

func (d *Dispatcher) state(turn string) *turnState {
	if st := d.liveState(turn); st != nil {
		return st
	}
	rec, ok := d.loadRecord(turn)
	if !ok {
		return nil
	}
	st := newState(d, rec)
	st.finished = true
	if b, ok := d.binding(config.ProjectID(rec.Project)); ok {
		events, err := b.Repository.ClientEvents(config.WorkstreamID(rec.Workstream), turn)
		if err == nil {
			for _, e := range events {
				if e.Kind != trace.ClientReturned {
					continue
				}
				switch e.What {
				case "view":
					st.viewTaken = true
				case "result":
					st.resultTaken = true
				}
			}
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if live := d.states[turn]; live != nil {
		return live
	}
	d.states[turn] = st
	return st
}

func randomToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (st *turnState) recordEvent(ctx context.Context, kind, what string, n int, answer api.ViewAnswer) {
	b, ok := st.d.binding(config.ProjectID(st.rec.Project))
	if !ok || b.Repository == nil {
		return
	}
	stream := config.WorkstreamID(st.rec.Workstream)
	if kind == trace.ClientLate {
		events, _ := b.Repository.ClientEvents(stream, st.rec.Turn)
		for _, e := range events {
			if e.Kind == trace.ClientLate {
				n++
			}
		}
	}
	_, _ = b.Repository.RecordClientEvent(context.WithoutCancel(ctx), stream, st.rec.Unit, n, trace.ClientEvent{Kind: kind, Turn: st.rec.Turn, Thread: st.rec.Thread,
		Client: st.rec.Node, Name: st.rec.Name, What: what, Answer: string(answer), At: st.d.Now().UTC()})
}

func (st *turnState) late(ctx context.Context, what string) api.ViewAnswer {
	st.recordEvent(ctx, trace.ClientLate, what, 1, api.ViewSuperseded)
	return api.ViewSuperseded
}

func (st *turnState) takeView(ctx context.Context, staging string) (api.ViewAnswer, error) {
	st.mu.Lock()
	switch {
	case st.viewTaken:
		st.mu.Unlock()
		return api.ViewAlreadyRecorded, nil
	case st.finished:
		st.mu.Unlock()
		return st.late(ctx, "view"), nil
	}
	dir, err := st.store(staging)
	if err != nil {
		st.mu.Unlock()
		return "", err
	}
	st.viewDir, st.viewTaken = dir, true
	close(st.viewReady)
	st.mu.Unlock()
	st.recordEvent(ctx, trace.ClientReturned, "view", 2, api.ViewAccepted)
	st.d.maybeFinish(ctx, st)
	return api.ViewAccepted, nil
}

func (st *turnState) store(staging string) (string, error) {
	parent := filepath.Join(st.d.Views, st.rec.Project, st.rec.Workstream, st.rec.Thread, st.rec.Turn)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp(parent, "turn-")
	if err != nil {
		return "", err
	}
	if err := os.Remove(dir); err != nil {
		return "", err
	}
	if err := os.Rename(staging, dir); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(parent, "ready"), []byte(filepath.Base(dir)), 0o600); err != nil {
		return "", err
	}
	return dir, nil
}

func (st *turnState) takeResult(ctx context.Context, res api.TurnResult) (api.ViewAnswer, error) {
	st.mu.Lock()
	switch {
	case st.resultTaken:
		st.mu.Unlock()
		return api.ViewAlreadyRecorded, nil
	case st.finished:
		st.mu.Unlock()
		return st.late(ctx, "result"), nil
	}
	if err := st.keepSessionFiles(res.SessionFiles); err != nil {
		st.mu.Unlock()
		return "", &api.APIError{Code: api.Malformed, Message: err.Error()}
	}
	st.result, st.resultTaken = &res, true
	close(st.resultReady)
	st.mu.Unlock()
	st.recordEvent(ctx, trace.ClientReturned, "result", 1, api.ViewAccepted)
	st.d.maybeFinish(ctx, st)
	return api.ViewAccepted, nil
}

func (st *turnState) keepSessionFiles(files []api.SessionFile) error {
	if len(files) == 0 || st.rec.Directory == "" {
		return nil
	}
	if len(files) > maxSessionFiles {
		return fmt.Errorf("a result returns at most %d session files", maxSessionFiles)
	}
	total := 0
	for _, f := range files {
		total += len(f.Data)
		if f.Name == "" || f.Name != filepath.Base(f.Name) || strings.HasPrefix(f.Name, ".") || strings.ContainsAny(f.Name, "\x00\\") {
			return fmt.Errorf("session file %q is not a plain file name", f.Name)
		}
	}
	if total > maxSessionBytes {
		return fmt.Errorf("a result returns at most %d bytes of session files", maxSessionBytes)
	}
	if err := os.MkdirAll(st.rec.Directory, 0o700); err != nil {
		return err
	}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(st.rec.Directory, f.Name), f.Data, 0o600); err != nil {
			return err
		}
	}
	return nil
}

func (st *turnState) waitView(ctx context.Context, wait time.Duration) {
	if !st.rec.ReturnView {
		return
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-st.viewReady:
	case <-timer.C:
	case <-ctx.Done():
	}
}

func (st *turnState) keep(release func()) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.release = append(st.release, release)
}

func (st *turnState) begin(h *activity.Handle) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.activity = h
}

func (st *turnState) end() {
	st.mu.Lock()
	st.finished = true
	if st.endedAt.IsZero() {
		st.endedAt = st.d.Now()
	}
	release, handle := st.release, st.activity
	st.release = nil
	st.mu.Unlock()
	for _, f := range release {
		f()
	}
	handle.Finish(nil)
	st.d.Fleet.Release(st.rec.Node, st.rec.Turn)
}

func (d *Dispatcher) progress(client fleet.Client, p api.TurnProgress) {
	st := d.liveState(p.Turn)
	if st == nil || st.rec.Node != client.Identity.ID {
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.finished && !st.adopted {
		return
	}
	if p.Phase == st.lastPhase && sameProgress(p.Progress, st.lastFrac) {
		return
	}
	st.lastPhase, st.lastFrac = p.Phase, p.Progress
	phase := p.Phase
	if phase == "" {
		phase = "running on " + st.name()
	}
	st.activity.Progress(phase, p.Progress)
}

func sameProgress(a, b *float64) bool {
	switch {
	case a == nil || b == nil:
		return a == b
	}
	return *a == *b
}

func (st *turnState) name() string {
	if st.rec.Name != "" {
		return st.rec.Name
	}
	return st.rec.Node
}

func (d *Dispatcher) leaseEnded(_ fleet.Client, turns []string) {
	for _, turn := range turns {
		if st := d.liveState(turn); st != nil {
			st.lostOnce.Do(func() { close(st.lost) })
		}
	}
}

func (d *Dispatcher) result(ctx context.Context, client fleet.Client, turn string, res api.TurnResult) (api.ViewAnswer, error) {
	st := d.state(turn)
	if st == nil {
		return api.ViewSuperseded, nil
	}
	if st.rec.Node != client.Identity.ID {
		return "", &api.APIError{Code: api.Forbidden, Message: "the turn was offered to another client"}
	}
	return st.takeResult(ctx, res)
}

func (d *Dispatcher) lookup(turn string) (filetransport.Turn, bool) {
	rec, ok := d.loadRecord(turn)
	if !ok || rec.TokenSum == "" {
		return filetransport.Turn{}, false
	}
	t := filetransport.Turn{TokenSum: rec.TokenSum}
	if rec.ReturnView {
		t.Upload = d.upload
	}
	return t, true
}

func (d *Dispatcher) upload(ctx context.Context, turn, staging string) (api.ViewAnswer, error) {
	st := d.state(turn)
	if st == nil {
		return api.ViewSuperseded, nil
	}
	return st.takeView(ctx, staging)
}

func (d *Dispatcher) maybeFinish(ctx context.Context, st *turnState) {
	st.mu.Lock()
	if !st.adopted || st.finished || !st.resultTaken || st.rec.ReturnView && !st.viewTaken {
		st.mu.Unlock()
		return
	}
	st.finished = true
	res, dir := *st.result, st.viewDir
	st.mu.Unlock()
	defer st.end()
	ctx = context.WithoutCancel(ctx)
	b, ok := d.binding(config.ProjectID(st.rec.Project))
	if !ok {
		return
	}
	result := res.Result
	result.Client = st.rec.Node
	var runErr error
	if dir != "" {
		view, err := isolation.AdoptView(dir, coreadapter.ReadWrite)
		if err == nil {
			if b.Capture != nil {
				err = b.Capture(ctx, st.rec.scope(), view, result)
			}
			err = errors.Join(err, view.Release(ctx))
		}
		runErr = err
	}
	_, err := b.Runner.Adopt(ctx, config.WorkstreamID(st.rec.Workstream), st.rec.Agent, st.rec.Turn, result, runErr)
	if err != nil {
		ghlog.Logger(ctx).Warn("cannot record the result of a turn that reattached after a restart", "turn", st.rec.Turn, "error", err.Error())
	}
}

// Held reports whether a turn that a previous service session started is still
// held for its client to reattach it.
func (d *Dispatcher) Held(turn string) bool {
	st := d.liveState(turn)
	if st == nil {
		return false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.adopted && !st.finished && d.Now().Before(st.deadline)
}

// HoldFromTrace holds the turns that a previous service session started on a
// remote client and never captured a result for, so the clients that run them
// can reattach within the hold window. It must run before the service
// recovers its interrupted turns.
func (d *Dispatcher) HoldFromTrace(project config.ProjectID, repo *trace.Repository) error {
	streams, err := repo.Workstreams()
	if err != nil {
		return err
	}
	for _, stream := range streams {
		threads, err := repo.Threads(stream)
		if err != nil {
			return err
		}
		for _, th := range threads {
			for _, q := range th.Turns {
				if q.Claim == nil || q.Response != nil || th.Active != q.Request.TurnID {
					continue
				}
				rec, ok := d.loadRecord(q.Request.TurnID)
				if !ok || rec.Project != string(project) || rec.Workstream != string(stream) {
					continue
				}
				rec.Agent = th.Identity.ID
				st := newState(d, rec)
				st.adopted, st.deadline = true, d.Now().Add(d.Hold)
				st.startedAt = q.Claim.At
				d.mu.Lock()
				d.states[rec.Turn] = st
				d.mu.Unlock()
			}
		}
	}
	return nil
}

// Expired is a held turn whose client did not reattach it in time.
type Expired struct {
	Project    config.ProjectID
	Workstream config.WorkstreamID
	// Agent is the ID of the turn's thread in the trace.
	Agent string
	Turn  string
}

// Sweep ends the hold of every turn whose window passed, and returns them so
// the service recovers them as it does any turn a restart interrupted.
func (d *Dispatcher) Sweep() []Expired {
	d.mu.Lock()
	var states []*turnState
	for _, st := range d.states {
		states = append(states, st)
	}
	d.mu.Unlock()
	var out []Expired
	for _, st := range states {
		st.mu.Lock()
		if st.finished && !st.endedAt.IsZero() && d.Now().Sub(st.endedAt) > time.Hour {
			d.mu.Lock()
			delete(d.states, st.rec.Turn)
			d.mu.Unlock()
		}
		expired := st.adopted && !st.finished && !d.Now().Before(st.deadline)
		if expired {
			st.finished = true
		}
		st.mu.Unlock()
		if expired {
			st.end()
			out = append(out, Expired{Project: config.ProjectID(st.rec.Project), Workstream: config.WorkstreamID(st.rec.Workstream), Agent: st.rec.Agent, Turn: st.rec.Turn})
		}
	}
	slices.SortFunc(out, func(a, b Expired) int { return strings.Compare(a.Turn, b.Turn) })
	return out
}

func (d *Dispatcher) reattach(_ context.Context, ident fleet.Identity, turn string) (string, bool) {
	st := d.liveState(turn)
	if st == nil {
		return "", false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if !st.adopted || st.finished || !d.Now().Before(st.deadline) || st.rec.Node != ident.ID {
		return "", false
	}
	token := randomToken()
	st.rec.TokenSum = filetransport.TokenSum(token)
	_ = d.saveRecord(st.rec)
	if st.rec.ReturnView {
		st.release = append(st.release, d.Transfers.Register(turn, filetransport.Turn{Token: token, Upload: d.upload}))
	}
	if st.activity == nil {
		st.activity = d.Activity.Start("client-turn-"+turn, api.ActivityClientTurn, config.ProjectID(st.rec.Project), config.WorkstreamID(st.rec.Workstream), "reattached to "+st.name())
	}
	return token, true
}
