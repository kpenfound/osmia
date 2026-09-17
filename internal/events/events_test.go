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
	{"before-acknowledge", 1}, {"before-acknowledge", 2}, {"before-settle", 1},
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
	for _, first := range steps {
		for _, second := range append(steps, struct {
			name string
			n    int
		}{"none", 1}) {
			t.Run(fmt.Sprintf("%s%d_then_%s%d", first.name, first.n, second.name, second.n), func(t *testing.T) {
				ctx := context.Background()
				f, repo := setup(t)
				defer func() { repo.Close() }()
				f.notify(t, repo, "first")
				f.notify(t, repo, "second")
				f.clock.Advance(time.Minute)
				d := f.deliverer(t, repo, time.Second)
				d.boundary = crashAt(first.name, first.n)
				_ = d.Pass(ctx)

				// Each restart is a new repository session; a later event joins
				// whatever was not delivered.
				repo = f.reopen(t, repo)
				f.notify(t, repo, "third")
				f.clock.Advance(time.Minute)
				d = f.deliverer(t, repo, time.Second)
				d.boundary = crashAt(second.name, second.n)
				_ = d.Pass(ctx)

				repo = f.reopen(t, repo)
				f.clock.Advance(time.Minute)
				d = f.deliverer(t, repo, time.Second)
				must(t, d.Pass(ctx))
				must(t, d.Pass(ctx))

				if n := unacknowledged(t, repo); n != 0 {
					t.Fatalf("%d events left unacknowledged", n)
				}
				for _, id := range []string{"first", "second", "third"} {
					count := 0
					for _, q := range eventTurns(t, repo) {
						count += strings.Count(q.Request.Prompt, "Notice "+id+"\n")
					}
					if count != 1 {
						t.Fatalf("event %s delivered %d times", id, count)
					}
				}
			})
		}
	}
}

func TestExpiredLeaseIsAcknowledgedWithoutAnotherTurn(t *testing.T) {
	ctx := context.Background()
	f, repo := setup(t)
	defer repo.Close()
	f.notify(t, repo, "first")
	f.clock.Advance(time.Minute)
	d := f.deliverer(t, repo, time.Second)
	// The lease runs out between the queued turn and its acknowledgement.
	d.boundary = func(step string) error {
		if step == "before-acknowledge" {
			f.clock.Advance(2 * time.Minute)
		}
		return nil
	}
	must(t, d.Pass(ctx))
	if n := unacknowledged(t, repo); n != 1 {
		t.Fatalf("%d unacknowledged after the expired lease", n)
	}
	d.boundary = nil
	must(t, d.Pass(ctx))
	if n := unacknowledged(t, repo); n != 0 || len(eventTurns(t, repo)) != 1 {
		t.Fatalf("%d unacknowledged, turns %+v", n, eventTurns(t, repo))
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
