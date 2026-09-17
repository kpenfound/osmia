package events

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/scheduler"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
)

const project config.ProjectID = "p_00000000000000000000000000000001"
const stream config.WorkstreamID = "w_00000000000000000000000000000001"

var (
	start   = time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	owner   = trace.Actor{Kind: "owner", ID: "local"}
	profile = coreadapter.Profile{Name: "default", Backend: "fake", Model: "test"}
)

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time          { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *clock) Advance(d time.Duration) { c.mu.Lock(); defer c.mu.Unlock(); c.now = c.now.Add(d) }

type fixture struct {
	root    config.Root
	project config.Project
	clock   *clock
	// failNext makes the next turn runAll completes fail.
	failNext bool
	// failed is the prompt of the turn runAll failed.
	failed string
	claims int
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func setup(t *testing.T) (*fixture, *trace.Repository) {
	t.Helper()
	base := t.TempDir()
	root, err := config.ResolveRoot(filepath.Join(base, "osmia"), "")
	must(t, err)
	f := &fixture{root: root, project: config.Project{ID: project, Clone: filepath.Join(base, "target")}, clock: &clock{now: start}}
	must(t, os.Mkdir(f.project.Clone, 0700))
	ctx := context.Background()
	repo, err := trace.Create(ctx, root, f.project, start, owner)
	must(t, err)
	must(t, repo.CreateWorkstream(ctx, stream, start, owner))
	return f, repo
}

func (f *fixture) reopen(t *testing.T, repo *trace.Repository) *trace.Repository {
	t.Helper()
	must(t, repo.Close())
	repo, err := trace.Open(f.root, f.project)
	must(t, err)
	return repo
}

func (f *fixture) deliverer(t *testing.T, repo *trace.Repository, window time.Duration) *Deliverer {
	t.Helper()
	d, err := New(repo, Options{Now: f.clock.Now, Window: window, Profile: func() (coreadapter.Profile, error) { return profile, nil }})
	must(t, err)
	return d
}

// notify commits a state transition of subject id with one notice.
func (f *fixture) notify(t *testing.T, repo *trace.Repository, id string) {
	t.Helper()
	h := trace.Header{Schema: "osmia.trace.transition", Version: 1, Revision: 1, ID: id, Project: project, Workstream: stream, At: f.clock.Now(), Actor: owner, Cause: "test"}
	_, err := repo.Transact(context.Background(), trace.Transaction{Transition: trace.Transition{Header: h, Subject: id, To: "changed", Reason: "Test change"},
		Events: []trace.Event{trace.Notice(id, "notice", "Notice "+id)}})
	must(t, err)
}

// eventTurns returns the chief-of-staff turns the deliverer queued.
func eventTurns(t *testing.T, repo *trace.Repository) []trace.QueuedTurn {
	t.Helper()
	th, err := repo.ChiefOfStaffThread(stream)
	must(t, err)
	var out []trace.QueuedTurn
	for _, q := range th.Turns {
		if q.Request.Actor == Actor {
			out = append(out, q)
		}
	}
	return out
}

// claimNext reserves the chief of staff's next queued turn.
func (f *fixture) claimNext(t *testing.T, repo *trace.Repository) trace.QueuedTurn {
	t.Helper()
	f.claims++
	token := fmt.Sprintf("run_%d", f.claims)
	q, err := repo.ClaimTurn(context.Background(), stream, trace.ChiefOfStaff, token, "/sessions/"+token, f.clock.Now())
	must(t, err)
	return q
}

// finish completes a turn claimed with claimNext, successfully or with a failure.
func (f *fixture) finish(t *testing.T, repo *trace.Repository, q trace.QueuedTurn, ok bool) {
	t.Helper()
	ctx := context.Background()
	h := q.Request.Header
	h.Schema, h.ID, h.At = "osmia.trace.turn-response", "response_"+q.Request.TurnID, f.clock.Now()
	response := trace.TurnResponse{Header: h, AgentID: trace.ChiefOfStaff, ThreadID: trace.ChiefOfStaff, TurnID: q.Request.TurnID, RequestID: q.Request.ID, RequestRevision: 1,
		Result: coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "fake", ID: "session"}, SessionDirectory: q.Claim.SessionDirectory, StartedAt: q.Claim.At, FinalResponse: "Noted"}}
	if !ok {
		response.Result.IsError, response.Failure = true, "the turn failed"
	}
	must(t, repo.CaptureTurn(ctx, q.Claim.Token, response))
	must(t, repo.CompleteTurn(ctx, stream, trace.ChiefOfStaff, q.Request.TurnID, q.Claim.Token, f.clock.Now()))
}

// runAll completes every queued chief-of-staff turn, successfully unless
// failNext is set.
func (f *fixture) runAll(t *testing.T, repo *trace.Repository) {
	t.Helper()
	th, err := repo.ChiefOfStaffThread(stream)
	must(t, err)
	for _, q := range th.Turns {
		if q.Claim == nil {
			ok := !f.failNext
			if !ok {
				f.failNext, f.failed = false, q.Request.Prompt
			}
			f.finish(t, repo, f.claimNext(t, repo), ok)
		}
	}
}

// deliveries counts the event turns whose prompt carries the notice of id.
func deliveries(t *testing.T, repo *trace.Repository, id string) int {
	t.Helper()
	n := 0
	for _, q := range eventTurns(t, repo) {
		n += strings.Count(q.Request.Prompt, "Notice "+id+"\n")
	}
	return n
}

func unacknowledged(t *testing.T, repo *trace.Repository) int {
	t.Helper()
	entries, err := repo.Outbox(stream)
	must(t, err)
	n := 0
	for _, e := range entries {
		if !e.Acknowledged {
			n++
		}
	}
	return n
}

func TestEventsInsideTheWindowMakeOneTurn(t *testing.T) {
	ctx := context.Background()
	f, repo := setup(t)
	defer repo.Close()
	d := f.deliverer(t, repo, 5*time.Second)
	f.notify(t, repo, "first")
	f.clock.Advance(2 * time.Second)
	f.notify(t, repo, "second")
	f.clock.Advance(2 * time.Second)
	f.notify(t, repo, "third")
	must(t, d.Pass(ctx))
	if got := eventTurns(t, repo); len(got) != 0 {
		t.Fatalf("turn before the window closed: %+v", got)
	}
	f.clock.Advance(time.Second)
	must(t, d.Pass(ctx))
	must(t, d.Pass(ctx))
	turns := eventTurns(t, repo)
	if len(turns) != 1 {
		t.Fatalf("turns %+v", turns)
	}
	req := turns[0].Request
	want := Preamble + "\n\n- 2026-09-16T12:00:00Z (notice): Notice first\n- 2026-09-16T12:00:02Z (notice): Notice second\n- 2026-09-16T12:00:04Z (notice): Notice third\n"
	if req.Prompt != want {
		t.Fatalf("prompt %q", req.Prompt)
	}
	if req.AgentID != trace.ChiefOfStaff || req.ThreadID != trace.ChiefOfStaff || req.Profile != profile || req.Cause != "first" || !req.At.Equal(f.clock.Now()) || req.SystemPrompt != "" {
		t.Fatalf("request %+v", req)
	}
	// The events stay unacknowledged until their turn completes.
	if n := unacknowledged(t, repo); n != 3 {
		t.Fatalf("%d events unacknowledged while the turn is queued", n)
	}
	f.runAll(t, repo)
	must(t, d.Pass(ctx))
	if n := unacknowledged(t, repo); n != 0 {
		t.Fatalf("%d events left unacknowledged", n)
	}

	// A later event opens a new window of its own.
	f.notify(t, repo, "fourth")
	f.clock.Advance(4 * time.Second)
	must(t, d.Pass(ctx))
	if got := eventTurns(t, repo); len(got) != 1 {
		t.Fatalf("turns before second window closed: %d", len(got))
	}
	f.clock.Advance(time.Second)
	must(t, d.Pass(ctx))
	turns = eventTurns(t, repo)
	if len(turns) != 2 || !strings.Contains(turns[1].Request.Prompt, "Notice fourth") || strings.Contains(turns[1].Request.Prompt, "Notice first") {
		t.Fatalf("second delivery %+v", turns)
	}
}

func TestOperationEventsAreNotDelivered(t *testing.T) {
	ctx := context.Background()
	f, repo := setup(t)
	defer repo.Close()
	event := trace.EventID("dispatch", "turn")
	op, err := thread.TurnOperation(project, event, thread.TurnInput{Workstream: stream, Agent: trace.ChiefOfStaff, Turn: "turn"})
	must(t, err)
	h := trace.Header{Schema: "osmia.trace.transition", Version: 1, Revision: 1, ID: "dispatch", Project: project, Workstream: stream, At: start, Actor: owner, Cause: "test"}
	_, err = repo.Transact(ctx, trace.Transaction{Transition: trace.Transition{Header: h, Subject: "dispatch", To: "turn", Reason: "Dispatch"},
		Events: []trace.Event{{ID: event, Kind: "turn", Body: "Run turn", Operation: &op}}})
	must(t, err)
	f.clock.Advance(time.Hour)
	must(t, f.deliverer(t, repo, time.Second).Pass(ctx))
	if got := eventTurns(t, repo); len(got) != 0 {
		t.Fatalf("operation delivered as event: %+v", got)
	}
}

func TestOnlyTheChiefOfStaffReceivesEvents(t *testing.T) {
	ctx := context.Background()
	f, repo := setup(t)
	defer repo.Close()
	for _, role := range []string{"mason", "reviewer", "architect"} {
		h := trace.Header{Schema: "osmia.trace.agent", Version: 1, Revision: 1, ID: role, Project: project, Workstream: stream, At: start, Actor: owner, Cause: "created"}
		must(t, repo.CreateThread(ctx, trace.Agent{Header: h, Role: role, ThreadID: "thread_" + role}))
	}
	f.notify(t, repo, "first")
	f.clock.Advance(time.Minute)
	must(t, f.deliverer(t, repo, time.Second).Pass(ctx))
	threads, err := repo.Threads(stream)
	must(t, err)
	for _, th := range threads {
		want := 0
		if th.Identity.ID == trace.ChiefOfStaff {
			want = 1
		}
		if len(th.Turns) != want {
			t.Fatalf("thread %s has %d turns", th.Identity.ID, len(th.Turns))
		}
	}
}

func TestEventTurnQueuesBehindTheTurnInFlight(t *testing.T) {
	ctx := context.Background()
	f, repo := setup(t)
	defer repo.Close()
	req := trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: 1, Revision: 1, ID: "request_owner", Project: project, Workstream: stream, At: start, Actor: owner, Cause: "message"},
		AgentID: trace.ChiefOfStaff, ThreadID: trace.ChiefOfStaff, TurnID: "owner", Profile: profile, Prompt: "Owner message"}
	_, err := repo.EnqueueTurn(ctx, req)
	must(t, err)
	s, err := scheduler.New(repo, scheduler.Options{Now: f.clock.Now})
	must(t, err)
	must(t, s.Pass(ctx))
	_, err = repo.ClaimTurn(ctx, stream, trace.ChiefOfStaff, "claim", filepath.Join(t.TempDir(), "session"), start)
	must(t, err)

	f.notify(t, repo, "first")
	f.clock.Advance(time.Minute)
	must(t, f.deliverer(t, repo, time.Second).Pass(ctx))
	must(t, s.Pass(ctx))
	th, err := repo.ChiefOfStaffThread(stream)
	must(t, err)
	if th.Active != "owner" || len(th.Turns) != 2 || th.Turns[1].Request.Actor != Actor || th.Turns[1].Claim != nil {
		t.Fatalf("thread %+v", th)
	}
	ops, err := repo.Operations(stream)
	must(t, err)
	for _, op := range ops {
		in, err := thread.DecodeTurn(op.Operation)
		must(t, err)
		if in.Turn != "owner" {
			t.Fatalf("event turn dispatched during the owner turn: %+v", in)
		}
	}
}

// steps are the deliverer's crash points, each with the occurrence to fail.
var steps = []struct {
	name string
	n    int
}{
	{"before-claim", 1}, {"before-claim", 2}, {"before-enqueue", 1},
	{"before-settle", 1}, {"before-settle", 2}, {"before-release", 1}, {"before-release", 2},
}

func crashAt(name string, n int) func(string) error {
	seen := 0
	return func(step string) error {
		if step == name {
			seen++
			if seen == n {
				return errors.New("crash at " + step)
			}
		}
		return nil
	}
}

func TestCrashAndRestartNeitherLoseNorRepeatEvents(t *testing.T) {
	for _, failed := range []bool{false, true} {
		for _, first := range steps {
			for _, second := range append(steps, struct {
				name string
				n    int
			}{"none", 1}) {
				t.Run(fmt.Sprintf("failed=%v/%s%d_then_%s%d", failed, first.name, first.n, second.name, second.n), func(t *testing.T) {
					t.Parallel()
					crashAndRestart(t, failed, first.name, first.n, second.name, second.n)
				})
			}
		}
	}
}

// crashAndRestart delivers three events across two crashing passes and a
// clean one, restarting after each and running the queued turns between
// passes. When failed is set, the first turn to run fails.
func crashAndRestart(t *testing.T, failed bool, first string, firstN int, second string, secondN int) {
	ctx := context.Background()
	f, repo := setup(t)
	defer func() { repo.Close() }()
	f.failNext = failed
	f.notify(t, repo, "first")
	f.notify(t, repo, "second")
	f.clock.Advance(time.Minute)
	d := f.deliverer(t, repo, time.Second)
	d.boundary = crashAt(first, firstN)
	for range 2 {
		_ = d.Pass(ctx)
		f.runAll(t, repo)
	}

	// Each restart is a new repository session; a later event joins
	// whatever was not delivered.
	repo = f.reopen(t, repo)
	f.notify(t, repo, "third")
	f.clock.Advance(time.Minute)
	d = f.deliverer(t, repo, time.Second)
	d.boundary = crashAt(second, secondN)
	for range 2 {
		_ = d.Pass(ctx)
		f.runAll(t, repo)
	}

	repo = f.reopen(t, repo)
	f.clock.Advance(time.Minute)
	d = f.deliverer(t, repo, time.Second)
	for range 3 {
		must(t, d.Pass(ctx))
		f.runAll(t, repo)
	}
	must(t, d.Pass(ctx))

	if n := unacknowledged(t, repo); n != 0 {
		t.Fatalf("%d events left unacknowledged", n)
	}
	if failed && f.failed == "" {
		t.Fatal("no turn failed")
	}
	for _, id := range []string{"first", "second", "third"} {
		// An event of the failed turn goes out in exactly one more.
		want := 1 + strings.Count(f.failed, "Notice "+id+"\n")
		if n := deliveries(t, repo, id); n != want {
			t.Fatalf("event %s delivered %d times, want %d", id, n, want)
		}
	}
}

func TestEventOfACompletedTurnIsNeverDeliveredAgain(t *testing.T) {
	ctx := context.Background()
	f, repo := setup(t)
	defer func() { repo.Close() }()
	f.notify(t, repo, "first")
	f.clock.Advance(time.Minute)
	d := f.deliverer(t, repo, time.Second)
	must(t, d.Pass(ctx))
	f.runAll(t, repo)
	// A restart before the acknowledgement settles the event without a turn.
	repo = f.reopen(t, repo)
	f.clock.Advance(time.Hour)
	d = f.deliverer(t, repo, time.Second)
	must(t, d.Pass(ctx))
	must(t, d.Pass(ctx))
	repo = f.reopen(t, repo)
	must(t, f.deliverer(t, repo, time.Second).Pass(ctx))
	if n := unacknowledged(t, repo); n != 0 || len(eventTurns(t, repo)) != 1 {
		t.Fatalf("%d unacknowledged, turns %+v", n, eventTurns(t, repo))
	}
}

func TestEventOfATurnCompletedInThisSessionIsAcknowledgedUnderItsClaim(t *testing.T) {
	ctx := context.Background()
	f, repo := setup(t)
	defer repo.Close()
	f.notify(t, repo, "first")
	f.clock.Advance(time.Minute)
	d := f.deliverer(t, repo, time.Second)
	must(t, d.Pass(ctx))
	f.runAll(t, repo)
	must(t, d.Pass(ctx))
	entries, err := repo.Outbox(stream)
	must(t, err)
	if len(entries) != 1 || !entries[0].Acknowledged || len(entries[0].History) != 2 || entries[0].History[1].Token != entries[0].History[0].Token {
		t.Fatalf("outbox %+v", entries)
	}
}

func TestEventOfAQueuedOrRunningTurnIsNotDeliveredTwice(t *testing.T) {
	for _, c := range []struct {
		name             string
		running, restart bool
	}{{"queued", false, false}, {"queued across a restart", false, true}, {"running", true, false}} {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			f, repo := setup(t)
			defer func() { repo.Close() }()
			f.notify(t, repo, "first")
			f.clock.Advance(time.Minute)
			d := f.deliverer(t, repo, time.Second)
			must(t, d.Pass(ctx))
			var q trace.QueuedTurn
			if c.running {
				q = f.claimNext(t, repo)
			}
			if c.restart {
				repo = f.reopen(t, repo)
				d = f.deliverer(t, repo, time.Second)
			}
			// The claim's lease runs out while the turn is in flight.
			f.clock.Advance(time.Hour)
			must(t, d.Pass(ctx))
			must(t, d.Pass(ctx))
			if n := unacknowledged(t, repo); n != 1 || len(eventTurns(t, repo)) != 1 {
				t.Fatalf("%d unacknowledged, turns %+v", n, eventTurns(t, repo))
			}
			if c.running {
				f.finish(t, repo, q, true)
			} else {
				f.runAll(t, repo)
			}
			must(t, d.Pass(ctx))
			if n := unacknowledged(t, repo); n != 0 || len(eventTurns(t, repo)) != 1 {
				t.Fatalf("%d unacknowledged, turns %+v", n, eventTurns(t, repo))
			}
		})
	}
}

func TestEventOfAFailedTurnIsDeliveredInOneNewTurn(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(fmt.Sprintf("expired=%v", expired), func(t *testing.T) {
			ctx := context.Background()
			f, repo := setup(t)
			defer repo.Close()
			f.notify(t, repo, "first")
			f.clock.Advance(time.Minute)
			d := f.deliverer(t, repo, time.Second)
			must(t, d.Pass(ctx))
			f.finish(t, repo, f.claimNext(t, repo), false)
			if expired {
				f.clock.Advance(time.Hour)
			}
			must(t, d.Pass(ctx))
			must(t, d.Pass(ctx))
			turns := eventTurns(t, repo)
			if len(turns) != 2 || deliveries(t, repo, "first") != 2 || turns[1].Request.TurnID == turns[0].Request.TurnID || turns[1].Request.Prompt != turns[0].Request.Prompt {
				t.Fatalf("turns %+v", turns)
			}
			entries, err := repo.Outbox(stream)
			must(t, err)
			var kinds []string
			for _, a := range entries[0].History {
				kinds = append(kinds, a.Kind)
			}
			want := []string{"claim", "release", "claim"}
			if expired {
				want = []string{"claim", "claim"}
			}
			if !slices.Equal(kinds, want) {
				t.Fatalf("history %v, want %v", kinds, want)
			}
			f.runAll(t, repo)
			must(t, d.Pass(ctx))
			must(t, d.Pass(ctx))
			if n := unacknowledged(t, repo); n != 0 || len(eventTurns(t, repo)) != 2 {
				t.Fatalf("%d unacknowledged, turns %+v", n, eventTurns(t, repo))
			}
		})
	}
}

func TestEventOfATurnARestartInterruptedIsDeliveredInOneNewTurn(t *testing.T) {
	ctx := context.Background()
	f, repo := setup(t)
	defer func() { repo.Close() }()
	f.notify(t, repo, "first")
	f.clock.Advance(time.Minute)
	must(t, f.deliverer(t, repo, time.Second).Pass(ctx))
	f.claimNext(t, repo)
	// The service stops before the turn captures a result.
	repo = f.reopen(t, repo)
	d := f.deliverer(t, repo, time.Second)
	must(t, d.Pass(ctx))
	must(t, d.Pass(ctx))
	repo = f.reopen(t, repo)
	d = f.deliverer(t, repo, time.Second)
	f.clock.Advance(time.Hour)
	must(t, d.Pass(ctx))
	turns := eventTurns(t, repo)
	if len(turns) != 2 || deliveries(t, repo, "first") != 2 || turns[1].Claim != nil {
		t.Fatalf("turns %+v", turns)
	}
	if n := unacknowledged(t, repo); n != 1 {
		t.Fatalf("%d unacknowledged", n)
	}
}

func TestProfileFailureKeepsEvents(t *testing.T) {
	ctx := context.Background()
	f, repo := setup(t)
	defer repo.Close()
	d, err := New(repo, Options{Now: f.clock.Now, Profile: func() (coreadapter.Profile, error) { return coreadapter.Profile{}, errors.New("unused") }})
	must(t, err)
	// Without events, no profile is needed.
	must(t, d.Pass(ctx))
	f.notify(t, repo, "first")
	if err := d.Pass(ctx); err == nil || !strings.Contains(err.Error(), "unused") {
		t.Fatalf("profile error not reported: %v", err)
	}
	if n := unacknowledged(t, repo); n != 1 {
		t.Fatalf("%d unacknowledged", n)
	}
}

func TestSystemPromptIsBuiltPerWorkstreamWhenATurnIsQueued(t *testing.T) {
	ctx := context.Background()
	f, repo := setup(t)
	defer repo.Close()
	var asked []config.WorkstreamID
	fail := errors.New("no context")
	var failing error
	d, err := New(repo, Options{Now: f.clock.Now, Window: time.Second, Profile: func() (coreadapter.Profile, error) { return profile, nil },
		System: func(_ context.Context, s config.WorkstreamID) (string, error) {
			asked = append(asked, s)
			return "Context of " + string(s), failing
		}})
	must(t, err)
	// No system prompt is built without events, or inside the window.
	must(t, d.Pass(ctx))
	f.notify(t, repo, "first")
	must(t, d.Pass(ctx))
	if len(asked) != 0 {
		t.Fatalf("system prompt built early: %v", asked)
	}
	f.clock.Advance(time.Second)
	// A failing system prompt keeps the events for a later pass.
	failing = fail
	if err := d.Pass(ctx); !errors.Is(err, fail) {
		t.Fatalf("system prompt error not reported: %v", err)
	}
	if n, turns := unacknowledged(t, repo), eventTurns(t, repo); n != 1 || len(turns) != 0 {
		t.Fatalf("%d unacknowledged, turns %+v", n, turns)
	}
	failing = nil
	must(t, d.Pass(ctx))
	turns := eventTurns(t, repo)
	if len(turns) != 1 || turns[0].Request.SystemPrompt != "Context of "+string(stream) || !slices.Equal(asked, []config.WorkstreamID{stream, stream}) {
		t.Fatalf("turns %+v, asked %v", turns, asked)
	}
}

func TestNewValidatesOptions(t *testing.T) {
	_, repo := setup(t)
	defer repo.Close()
	p := func() (coreadapter.Profile, error) { return profile, nil }
	for name, o := range map[string]Options{"no profile": {}, "negative window": {Profile: p, Window: -1}, "negative lease": {Profile: p, Lease: -1}} {
		if _, err := New(repo, o); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	if _, err := New(nil, Options{Profile: p}); err == nil {
		t.Fatal("nil repository accepted")
	}
}

func TestTurnIDIsAValidKey(t *testing.T) {
	token, err := newToken()
	must(t, err)
	id := TurnID(token)
	if id != TurnID(token) || id == TurnID(token+"x") || !strings.HasPrefix(id, "events_") || len(id) != 47 {
		t.Fatalf("turn id %q", id)
	}
}
