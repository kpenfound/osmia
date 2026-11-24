package service

import (
	"context"
	"encoding/json"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"net/http"
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
	"github.com/kpenfound/osmia/internal/projectstore"
	"github.com/kpenfound/osmia/internal/reconcile"
	"github.com/kpenfound/osmia/internal/router"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
)

func abandonCall(t *testing.T, s *Service, id config.WorkstreamID, body string) (int, string) {
	t.Helper()
	return intakefixture.AbandonCall(t, s.handle, id, body)
}

func TestAbandonHandedWorkstream(t *testing.T) {
	t.Parallel()
	f := newHandInFixture(t)
	ctx := context.Background()
	out := f.handIn(t, HandInRequest{Key: "k", Stdin: intakefixture.Ptr("# Design\n")})
	stream := out.Workstream
	r := f.repository()

	got, err := f.c.Abandon(ctx, stream, "  Superseded.  ")
	intakefixture.Must(t, err)
	if got != (AbandonResponse{Project: f.project, Workstream: stream, State: AbandonedState, Reason: "Superseded."}) {
		t.Fatalf("response %+v", got)
	}
	state, err := r.Workflow(stream, trace.FeatureSubject)
	intakefixture.Must(t, err)
	if state != (trace.WorkflowState{Version: 2, Value: AbandonedState}) {
		t.Fatalf("state %+v", state)
	}
	transitions, err := trace.Read[trace.Transition](r, stream)
	intakefixture.Must(t, err)
	transitions = slices.DeleteFunc(transitions, func(tr trace.Transition) bool { return tr.Subject != trace.FeatureSubject })
	last := transitions[len(transitions)-1]
	if len(transitions) != 2 || last.From != HandedState || last.To != AbandonedState || last.Actor != ownerActor || last.Reason != "Superseded." || last.Subject != trace.FeatureSubject {
		t.Fatalf("transitions %+v", transitions)
	}
	outbox, err := r.Outbox(stream)
	intakefixture.Must(t, err)
	if !slices.ContainsFunc(outbox, func(e trace.OutboxEntry) bool {
		return e.TransitionID == last.ID && strings.Contains(e.Event.Body, "from handed to abandoned: Superseded.")
	}) {
		t.Fatalf("no notice for the abandonment: %+v", outbox)
	}
	if data, err := fixturekit.WorkstreamDocument(t, f.s.cfg.Root, f.project, stream, "handed/stdin"); err != nil || string(data) != "# Design\n" {
		t.Fatalf("handed copy: %q %v", data, err)
	}
	docs, err := trace.Read[trace.Document](r, stream)
	intakefixture.Must(t, err)
	var handedCopies, routes []trace.Document
	for _, d := range docs {
		switch {
		case d.Path == "handed/stdin":
			handedCopies = append(handedCopies, d)
		case strings.HasPrefix(d.Path, "routes/"):
			routes = append(routes, d)
		}
	}
	if len(docs) != 2 || len(handedCopies) != 1 || len(routes) != 1 || handedCopies[0].Revision != 1 || handedCopies[0].Content != "# Design\n" {
		t.Fatalf("documents %+v", docs)
	}
	var route router.Record
	intakefixture.Must(t, json.Unmarshal([]byte(routes[0].Content), &route))
	if route.Rule != "pinned" || route.Authority != "owner" || route.DecidedBy != "owner" || route.Target != AbandonedState || route.Default != AbandonedState ||
		route.JevRequested || route.Judgment != "" || route.State != router.StateCommitted {
		t.Fatalf("route record %+v", route)
	}
	st, err := f.c.Status(ctx, stream)
	intakefixture.Must(t, err)
	if st.State == nil || *st.State != AbandonedState {
		t.Fatalf("status %+v", st)
	}

	groups, _, err := trace.Workgroups(r)
	intakefixture.Must(t, err)
	group, found := trace.WorkgroupOf(groups, stream)
	if !found {
		t.Fatal("handed workstream has no workgroup")
	}
	changes, closeChanges := serviceChanges(t, f.s)
	defer closeChanges()
	deadline := time.Now().Add(intakefixture.DemoTimeout)
	for {
		resolved, _, err := trace.ResolvedWorkgroups(r)
		intakefixture.Must(t, err)
		if slices.Contains(resolved, group.ID) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("successful abandonment did not settle: resolved=%v", resolved)
		}
		changes()
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
	t.Parallel()
	f := newHandInFixture(t)
	ctx := context.Background()
	delivered := f.handIn(t, HandInRequest{Key: "d", Stdin: intakefixture.Ptr("delivered")}).Workstream
	handed := f.handIn(t, HandInRequest{Key: "h", Stdin: intakefixture.Ptr("handed")}).Workstream
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "deliver", Revision: 1, Project: f.project, Workstream: delivered, At: time.Now().UTC(), Actor: ownerActor, Cause: "deliver"}
	_, err := f.repository().SetFeatureState(ctx, h, DeliveredState, "pull request opened")
	intakefixture.Must(t, err)
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
		{"unknown", unknown, `{"reason":"r"}`, 422, "is not in an active project"},
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

func TestAbandonCancelsTurnsAcrossRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	home, err := os.MkdirTemp("", "ab-")
	intakefixture.Must(t, err)
	t.Cleanup(func() { os.RemoveAll(home) })
	opts := fixtureAt(t, home)
	global := filepath.Join(opts.Config.Root, "config.toml")
	data, err := os.ReadFile(global)
	intakefixture.Must(t, err)
	intakefixture.Must(t, os.WriteFile(global, append(data, []byte("[events]\nwindow = \"1000h\"\n")...), 0600))
	cfg, err := projectstore.LoadConfig(opts.Config)
	intakefixture.Must(t, err)
	root := cfg.Root.String()

	clock := &fixturekit.DemoClock{At: fixturekit.DemoStart}
	owner := trace.Actor{Kind: "owner", ID: "local"}
	repo, err := trace.Create(ctx, cfg.Root, cfg.Project, clock.Now(), owner)
	intakefixture.Must(t, err)
	intakefixture.Must(t, repo.CreateWorkstream(ctx, intakefixture.Stream, clock.Now(), owner))
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "handin", Revision: 1, Project: intakefixture.Project, Workstream: intakefixture.Stream, At: clock.Now(), Actor: owner, Cause: "handin"}
	_, err = repo.SetFeatureState(ctx, h, HandedState, "handed in")
	intakefixture.Must(t, err)
	identity := trace.Agent{Header: trace.Header{Schema: "osmia.trace.agent", Version: 1, Revision: 1, ID: intakefixture.DemoAgent, Project: intakefixture.Project, Workstream: intakefixture.Stream, At: clock.Now(), Actor: owner, Cause: "workstream_created"}, Role: fixturekit.DemoRole, ThreadID: intakefixture.DemoThread}
	intakefixture.Must(t, repo.CreateThread(ctx, identity))
	queueTurn(t, repo, "first", clock.Now())
	queueTurn(t, repo, "second", clock.Now())
	intakefixture.Must(t, repo.Close())

	reply := adaptertest.Reply[coreadapter.SessionResult]{Value: coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "fake", ID: "session"}, FinalResponse: "Partial answer", Cancelled: true}}
	turns := &fixturekit.BlockingTurns{Fake: &adaptertest.Turns{Script: *adaptertest.NewScript[coreadapter.PreparedTurn](reply, reply, reply)}, Hooks: map[string]func(context.Context){}}
	var bound sync.Mutex
	var live *trace.Repository
	opts.Threads = func(r *trace.Repository, _ *config.Config) (coreadapter.Reconciler, error) {
		bound.Lock()
		live = r
		bound.Unlock()
		return thread.Dispatcher{Runner: thread.Runner{Jev: fixturekit.UnconfiguredJudge(nil), Store: r, Turns: turns, Now: clock.Now},
			Prepare: func(_ context.Context, in thread.TurnInput) (coreadapter.PreparedTurn, error) {
				return coreadapter.PreparedTurn{SessionDirectory: filepath.Join(root, "sessions", in.Agent, in.Turn)}, nil
			}}, nil
	}
	ticks := make(chan time.Time)
	opts.Reconciliation.Now, opts.Reconciliation.Ticks = clock.Now, ticks

	entered, stopped := make(chan struct{}), make(chan struct{})
	turns.Hooks["first"] = func(ctx context.Context) {
		close(entered)
		<-ctx.Done()
		close(stopped)
	}
	s, err := Start(ctx, opts)
	intakefixture.Must(t, err)
	defer s.Close()
	select {
	case <-entered:
	case <-time.After(intakefixture.DemoTimeout):
		t.Fatal("queued turn did not start")
	}
	bound.Lock()
	repo = live
	bound.Unlock()

	code, body := abandonCall(t, s, intakefixture.Stream, `{"reason":"Superseded."}`)
	if code != http.StatusOK {
		t.Fatalf("abandon: %d %s", code, body)
	}
	select {
	case <-stopped:
	case <-time.After(intakefixture.DemoTimeout):
		t.Fatal("abandoning did not cancel the running turn")
	}
	completed := func() trace.Thread {
		t.Helper()
		var th trace.Thread
		intakefixture.Soon(t, "the abandoned turns to complete", func() bool {
			var err error
			th, err = repo.Thread(intakefixture.Stream, intakefixture.DemoAgent)
			intakefixture.Must(t, err)
			return th.Active == "" && !slices.ContainsFunc(th.Turns, func(q trace.QueuedTurn) bool { return q.CompletedAt.IsZero() })
		})
		return th
	}
	th := completed()
	first, second := th.Turns[0], th.Turns[1]
	if first.Response.Result.FinalResponse != "Partial answer" || first.Status() != "interrupted" || len(first.Attempts) != 1 {
		t.Fatalf("running turn lost its recorded work: %+v", first.Response)
	}
	if second.Status() != "interrupted" || second.Response.Failure != cancelReason || second.Response.Actor != abandonActor {
		t.Fatalf("waiting turn: %+v", second.Response)
	}

	queueTurn(t, repo, "third", clock.Now())
	ticks <- clock.Now()
	ticks <- clock.Now()
	if _, ok := turnOperations(t, repo)["third"]; ok {
		t.Fatal("a turn of an abandoned workstream was dispatched")
	}
	intakefixture.Must(t, s.Close())

	s, err = Start(ctx, opts)
	intakefixture.Must(t, err)
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
	for _, call := range turns.Fake.Calls() {
		ran = append(ran, call.Scope.Turn)
	}
	if !slices.Equal(ran, []string{"first"}) {
		t.Fatalf("backend runs %v", ran)
	}
	st, api := s.workstreamStatus(string(intakefixture.Stream))
	if api != nil || st.State == nil || *st.State != AbandonedState {
		t.Fatalf("status %+v %v", st, api)
	}
}

// TestAbandonedTurnOperationDoesNotRun applies a turn operation of an
// abandoned workstream: the turn completes as cancelled without a backend call.
func TestAbandonedTurnOperationDoesNotRun(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	home, err := os.MkdirTemp("", "ao-")
	intakefixture.Must(t, err)
	t.Cleanup(func() { os.RemoveAll(home) })
	cfg, err := projectstore.LoadConfig(fixtureAt(t, home).Config)
	intakefixture.Must(t, err)
	clock := &fixturekit.DemoClock{At: fixturekit.DemoStart}
	owner := trace.Actor{Kind: "owner", ID: "local"}
	repo, err := trace.Create(ctx, cfg.Root, cfg.Project, clock.Now(), owner)
	intakefixture.Must(t, err)
	defer repo.Close()
	intakefixture.Must(t, repo.CreateWorkstream(ctx, intakefixture.Stream, clock.Now(), owner))
	identity := trace.Agent{Header: trace.Header{Schema: "osmia.trace.agent", Version: 1, Revision: 1, ID: intakefixture.DemoAgent, Project: intakefixture.Project, Workstream: intakefixture.Stream, At: clock.Now(), Actor: owner, Cause: "workstream_created"}, Role: fixturekit.DemoRole, ThreadID: intakefixture.DemoThread}
	intakefixture.Must(t, repo.CreateThread(ctx, identity))
	queueTurn(t, repo, "first", clock.Now())
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: abandonTransition, Revision: 1, Project: intakefixture.Project, Workstream: intakefixture.Stream, At: clock.Now(), Actor: owner, Cause: abandonTransition}
	_, err = repo.SetFeatureState(ctx, h, AbandonedState, "gone")
	intakefixture.Must(t, err)

	turns := &adaptertest.Turns{}
	s := &Service{options: Options{Reconciliation: reconcile.Options{Now: clock.Now}}}
	a := abandonable{Reconciler: thread.Dispatcher{Runner: thread.Runner{Jev: fixturekit.UnconfiguredJudge(nil), Store: repo, Turns: turns, Now: clock.Now},
		Prepare: func(context.Context, thread.TurnInput) (coreadapter.PreparedTurn, error) {
			return coreadapter.PreparedTurn{SessionDirectory: filepath.Join(home, "session")}, nil
		}}, s: s, repository: repo}
	op, err := thread.TurnOperation(intakefixture.Project, "event", thread.TurnInput{Workstream: intakefixture.Stream, Agent: intakefixture.DemoAgent, Turn: "first"})
	intakefixture.Must(t, err)
	result, err := a.Apply(ctx, op)
	intakefixture.Must(t, err)
	if result.Outcome != "interrupted" || len(turns.Calls()) != 0 {
		t.Fatalf("result %+v, backend calls %d", result, len(turns.Calls()))
	}
	th, err := repo.Thread(intakefixture.Stream, intakefixture.DemoAgent)
	intakefixture.Must(t, err)
	if q := th.Turns[0]; q.Response == nil || !q.Response.Result.Cancelled || q.Response.Failure != cancelReason {
		t.Fatalf("turn %+v", q)
	}
}
