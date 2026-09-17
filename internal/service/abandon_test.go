package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/coreadapter/adaptertest"
	"github.com/kpenfound/osmia/internal/reconcile"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
)

func ptr(s string) *string { return &s }

// abandonCall posts an abandon request to the service's handler.
func abandonCall(t *testing.T, s *Service, id config.WorkstreamID, body string) (int, string) {
	t.Helper()
	w := httptest.NewRecorder()
	s.handle(w, httptest.NewRequest(http.MethodPost, Prefix+"/abandon/"+string(id), strings.NewReader(body)))
	return w.Code, w.Body.String()
}

func TestAbandonHandedWorkstream(t *testing.T) {
	f := newHandInFixture(t)
	ctx := context.Background()
	out := f.handIn(t, HandInRequest{Key: "k", Stdin: ptr("# Design\n")})
	stream := out.Workstream
	r := f.repository()

	got, err := f.c.Abandon(ctx, stream, "  Superseded.  ")
	must(t, err)
	if got != (AbandonResponse{Project: f.project, Workstream: stream, State: AbandonedState, Reason: "Superseded."}) {
		t.Fatalf("response %+v", got)
	}
	state, err := r.Workflow(stream, trace.FeatureSubject)
	must(t, err)
	if state != (trace.WorkflowState{Version: 2, Value: AbandonedState}) {
		t.Fatalf("state %+v", state)
	}
	transitions, err := trace.Read[trace.Transition](r, stream)
	must(t, err)
	last := transitions[len(transitions)-1]
	if len(transitions) != 2 || last.From != HandedState || last.To != AbandonedState || last.Actor != ownerActor || last.Reason != "Superseded." || last.Subject != trace.FeatureSubject {
		t.Fatalf("transitions %+v", transitions)
	}
	outbox, err := r.Outbox(stream)
	must(t, err)
	if !slices.ContainsFunc(outbox, func(e trace.OutboxEntry) bool {
		return e.TransitionID == last.ID && strings.Contains(e.Event.Body, "from handed to abandoned: Superseded.")
	}) {
		t.Fatalf("no notice for the abandonment: %+v", outbox)
	}
	// Nothing is deleted.
	if data, err := os.ReadFile(out.Handed); err != nil || string(data) != "# Design\n" {
		t.Fatalf("handed copy: %q %v", data, err)
	}
	if docs, err := trace.Read[trace.Document](r, stream); err != nil || len(docs) != 1 {
		t.Fatalf("documents %+v %v", docs, err)
	}
	st, err := f.c.Status(ctx, stream)
	must(t, err)
	if st.State == nil || *st.State != AbandonedState {
		t.Fatalf("status %+v", st)
	}

	head := f.head(t)
	code, body := abandonCall(t, f.s, stream, `{"reason":"again"}`)
	if code != http.StatusConflict || !strings.Contains(body, "workstream "+string(stream)+" is abandoned and cannot be abandoned") {
		t.Fatalf("abandoned again: %d %s", code, body)
	}
	if !f.unchanged(t, head) {
		t.Fatal("a refused abandonment wrote to the trace")
	}
}

func TestAbandonRefusals(t *testing.T) {
	f := newHandInFixture(t)
	ctx := context.Background()
	delivered := f.handIn(t, HandInRequest{Key: "d", Stdin: ptr("delivered")}).Workstream
	handed := f.handIn(t, HandInRequest{Key: "h", Stdin: ptr("handed")}).Workstream
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "deliver", Revision: 1, Project: f.project, Workstream: delivered, At: time.Now().UTC(), Actor: ownerActor, Cause: "deliver"}
	_, err := f.repository().SetFeatureState(ctx, h, DeliveredState, "pull request opened")
	must(t, err)
	head := f.head(t)
	unknown := config.WorkstreamID("w_00000000000000000000000000000000")
	for _, c := range []struct {
		name   string
		stream config.WorkstreamID
		body   string
		code   int
		text   string
	}{
		{"delivered", delivered, `{"reason":"r"}`, http.StatusConflict, "is delivered and cannot be abandoned"},
		{"empty reason", handed, `{"reason":" "}`, 422, "reason must not be empty"},
		{"unknown", unknown, `{"reason":"r"}`, 422, "is not in the active project"},
		{"librarian", librarianWorkstream(f.project), `{"reason":"r"}`, 422, "is not in the active project"},
		{"malformed ID", "w_x", `{"reason":"r"}`, 422, "workstream must be a workstream ID"},
		{"unknown field", handed, `{"reason":"r","force":true}`, http.StatusBadRequest, ""},
	} {
		code, body := abandonCall(t, f.s, c.stream, c.body)
		if code != c.code || !strings.Contains(body, c.text) {
			t.Errorf("%s: %d %s", c.name, code, body)
		}
	}
	if !f.unchanged(t, head) {
		t.Fatal("a refused abandonment wrote to the trace")
	}
	if state, err := f.repository().Workflow(handed, trace.FeatureSubject); err != nil || state.Value != HandedState {
		t.Fatalf("handed state %+v %v", state, err)
	}
}

// TestAbandonCancelsTurnsAcrossRestart abandons a workstream while one of its
// turns runs and another waits: the running turn stops with its result kept,
// the waiting one never runs, and neither do turns queued later, before or
// after a restart.
func TestAbandonCancelsTurnsAcrossRestart(t *testing.T) {
	ctx := context.Background()
	home, err := os.MkdirTemp("", "ab-")
	must(t, err)
	t.Cleanup(func() { os.RemoveAll(home) })
	opts := fixtureAt(t, home)
	// The demo clock advances on every read, so a short event window would
	// close after a timing-dependent number of passes and run a chief-of-staff
	// turn for the hand-in notice. This test covers the owner's thread only.
	global := filepath.Join(opts.Config.Root, "config.toml")
	data, err := os.ReadFile(global)
	must(t, err)
	must(t, os.WriteFile(global, append(data, []byte("[events]\nwindow = \"1000h\"\n")...), 0600))
	cfg, err := config.Load(opts.Config)
	must(t, err)
	root := cfg.Root.String()

	clock := &demoClock{now: demoStart}
	owner := trace.Actor{Kind: "owner", ID: "local"}
	repo, err := trace.Create(ctx, cfg.Root, cfg.Project, clock.Now(), owner)
	must(t, err)
	must(t, repo.CreateWorkstream(ctx, stream, clock.Now(), owner))
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "handin", Revision: 1, Project: project, Workstream: stream, At: clock.Now(), Actor: owner, Cause: "handin"}
	_, err = repo.SetFeatureState(ctx, h, HandedState, "handed in")
	must(t, err)
	identity := trace.Agent{Header: trace.Header{Schema: "osmia.trace.agent", Version: 1, Revision: 1, ID: demoAgent, Project: project, Workstream: stream, At: clock.Now(), Actor: owner, Cause: "workstream_created"}, Role: demoRole, ThreadID: demoThread}
	must(t, repo.CreateThread(ctx, identity))
	queueTurn(t, repo, "first", clock.Now())
	queueTurn(t, repo, "second", clock.Now())
	must(t, repo.Close())

	reply := adaptertest.Reply[coreadapter.SessionResult]{Value: coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "fake", ID: "session"}, FinalResponse: "Partial answer", Cancelled: true}}
	turns := &blockingTurns{fake: &adaptertest.Turns{Script: *adaptertest.NewScript[coreadapter.PreparedTurn](reply, reply, reply)}, hooks: map[string]func(context.Context){}}
	var bound sync.Mutex
	var live *trace.Repository
	opts.Threads = func(r *trace.Repository) (coreadapter.Reconciler, error) {
		bound.Lock()
		live = r
		bound.Unlock()
		return thread.Dispatcher{Runner: thread.Runner{Store: r, Turns: turns, Now: clock.Now},
			Prepare: func(_ context.Context, in thread.TurnInput) (coreadapter.PreparedTurn, error) {
				return coreadapter.PreparedTurn{SessionDirectory: filepath.Join(root, "sessions", in.Agent, in.Turn)}, nil
			}}, nil
	}
	ticks := make(chan time.Time)
	opts.Reconciliation.Now, opts.Reconciliation.Ticks = clock.Now, ticks

	entered, stopped := make(chan struct{}), make(chan struct{})
	turns.hooks["first"] = func(ctx context.Context) {
		close(entered)
		<-ctx.Done()
		close(stopped)
	}
	s, err := Start(ctx, opts)
	must(t, err)
	defer s.Close()
	select {
	case <-entered:
	case <-time.After(demoTimeout):
		t.Fatal("queued turn did not start")
	}
	bound.Lock()
	repo = live
	bound.Unlock()

	code, body := abandonCall(t, s, stream, `{"reason":"Superseded."}`)
	if code != http.StatusOK {
		t.Fatalf("abandon: %d %s", code, body)
	}
	select {
	case <-stopped:
	case <-time.After(demoTimeout):
		t.Fatal("abandoning did not cancel the running turn")
	}
	completed := func() trace.Thread {
		t.Helper()
		deadline := time.Now().Add(demoTimeout)
		for {
			th, err := repo.Thread(stream, demoAgent)
			must(t, err)
			if th.Active == "" && !slices.ContainsFunc(th.Turns, func(q trace.QueuedTurn) bool { return q.CompletedAt.IsZero() }) {
				return th
			}
			if time.Now().After(deadline) {
				t.Fatalf("turns did not complete: %+v", th)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	th := completed()
	first, second := th.Turns[0], th.Turns[1]
	if first.Response.Result.FinalResponse != "Partial answer" || first.Status() != "interrupted" || len(first.Attempts) != 1 {
		t.Fatalf("running turn lost its recorded work: %+v", first.Response)
	}
	if second.Status() != "interrupted" || second.Response.Failure != cancelReason || second.Response.Actor != abandonActor {
		t.Fatalf("waiting turn: %+v", second.Response)
	}

	// A turn queued after abandonment is never dispatched.
	queueTurn(t, repo, "third", clock.Now())
	ticks <- clock.Now()
	ticks <- clock.Now()
	if _, ok := turnOperations(t, repo)["third"]; ok {
		t.Fatal("a turn of an abandoned workstream was dispatched")
	}
	must(t, s.Close())

	// A restart completes the leftover turn as cancelled and runs nothing.
	s, err = Start(ctx, opts)
	must(t, err)
	defer s.Close()
	ticks <- clock.Now()
	ticks <- clock.Now()
	bound.Lock()
	repo = live
	bound.Unlock()
	th = completed()
	if third := th.Turns[2]; third.Response.Failure != cancelReason {
		t.Fatalf("queued turn after restart: %+v", third)
	}
	var ran []string
	for _, call := range turns.fake.Calls() {
		ran = append(ran, call.Scope.Turn)
	}
	if !slices.Equal(ran, []string{"first"}) {
		t.Fatalf("backend runs %v", ran)
	}
	st, api := s.workstreamStatus(string(stream))
	if api != nil || st.State == nil || *st.State != AbandonedState {
		t.Fatalf("status %+v %v", st, api)
	}
}

// TestAbandonedTurnOperationDoesNotRun applies a turn operation of an
// abandoned workstream: the turn completes as cancelled without a backend call.
func TestAbandonedTurnOperationDoesNotRun(t *testing.T) {
	ctx := context.Background()
	home, err := os.MkdirTemp("", "ao-")
	must(t, err)
	t.Cleanup(func() { os.RemoveAll(home) })
	cfg, err := config.Load(fixtureAt(t, home).Config)
	must(t, err)
	clock := &demoClock{now: demoStart}
	owner := trace.Actor{Kind: "owner", ID: "local"}
	repo, err := trace.Create(ctx, cfg.Root, cfg.Project, clock.Now(), owner)
	must(t, err)
	defer repo.Close()
	must(t, repo.CreateWorkstream(ctx, stream, clock.Now(), owner))
	identity := trace.Agent{Header: trace.Header{Schema: "osmia.trace.agent", Version: 1, Revision: 1, ID: demoAgent, Project: project, Workstream: stream, At: clock.Now(), Actor: owner, Cause: "workstream_created"}, Role: demoRole, ThreadID: demoThread}
	must(t, repo.CreateThread(ctx, identity))
	queueTurn(t, repo, "first", clock.Now())
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: abandonTransition, Revision: 1, Project: project, Workstream: stream, At: clock.Now(), Actor: owner, Cause: abandonTransition}
	_, err = repo.SetFeatureState(ctx, h, AbandonedState, "gone")
	must(t, err)

	turns := &adaptertest.Turns{}
	s := &Service{options: Options{Reconciliation: reconcile.Options{Now: clock.Now}}}
	a := abandonable{Reconciler: thread.Dispatcher{Runner: thread.Runner{Store: repo, Turns: turns, Now: clock.Now},
		Prepare: func(context.Context, thread.TurnInput) (coreadapter.PreparedTurn, error) {
			return coreadapter.PreparedTurn{SessionDirectory: filepath.Join(home, "session")}, nil
		}}, s: s, repository: repo}
	op, err := thread.TurnOperation(project, "event", thread.TurnInput{Workstream: stream, Agent: demoAgent, Turn: "first"})
	must(t, err)
	result, err := a.Apply(ctx, op)
	must(t, err)
	if result.Outcome != "interrupted" || len(turns.Calls()) != 0 {
		t.Fatalf("result %+v, backend calls %d", result, len(turns.Calls()))
	}
	th, err := repo.Thread(stream, demoAgent)
	must(t, err)
	if q := th.Turns[0]; q.Response == nil || !q.Response.Result.Cancelled || q.Response.Failure != cancelReason {
		t.Fatalf("turn %+v", q)
	}
}
