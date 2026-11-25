package clientturns

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/isolation"
	"github.com/kpenfound/osmia/internal/trace"
)

func (r *rig) restart() {
	r.t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if events, err := r.repo.ClientEvents(stream, "t1"); err == nil && slices.ContainsFunc(events, func(e trace.ClientEvent) bool { return e.Kind == trace.ClientPlaced }) {
			break
		}
	}
	must(r.t, r.repo.Close())
	reopened, err := trace.Open(r.root, r.project)
	must(r.t, err)
	r.t.Cleanup(func() { reopened.Close() })
	r.start("mason", reopened)
	must(r.t, r.d.HoldFromTrace(project, reopened))
}

type gatedCapture func(context.Context, coreadapter.Scope, *isolation.FileView, coreadapter.SessionResult) error

// gate makes a capture wait forever, so the path that was running when the
// service stopped never records anything. It returns a channel that is closed
// when the first capture starts waiting.
func gate() (gatedCapture, <-chan struct{}) {
	entered := make(chan struct{})
	var once sync.Once
	return func(context.Context, coreadapter.Scope, *isolation.FileView, coreadapter.SessionResult) error {
		once.Do(func() { close(entered) })
		select {}
	}, entered
}

func (r *rig) resultFile(turn string) string {
	return filepath.Join(r.d.Directory, "results", turn+".json")
}

func assertRecordedOnce(t *testing.T, r *rig, summary string) {
	t.Helper()
	th := thread1(t, r)
	if len(th.Turns) != 1 || th.Turns[0].Response == nil || th.Active != "" || th.Status == "interrupted" {
		t.Fatalf("the turn was not recorded once: %+v", th)
	}
	got := th.Turns[0]
	res := got.Response.Result
	if res.Cancelled || res.Outcome == nil || res.Outcome.Status != "complete" || res.Outcome.Report != summary || res.FinalResponse != summary ||
		res.Session.ID != "session-1" || res.Client != "node-a" || got.Status() != "idle" || len(got.Attempts) != 1 {
		t.Fatalf("the recorded turn is not the original result: %+v attempts %d", res, len(got.Attempts))
	}
	if expired := r.d.Sweep(); len(expired) != 0 || r.d.Held("t1") {
		t.Fatalf("recovery started for a recorded turn: %+v", expired)
	}
	if len(eventsOf(t, r, "t1", trace.ClientLate)) != 0 {
		t.Fatal("a return of an uninterrupted turn was recorded as late")
	}
}

func assertResubmissionsAreAlreadyRecorded(t *testing.T, r *rig, c *fakeClient, token string) {
	t.Helper()
	before := len(r.captures())
	if answer, err := c.result("t1", complete("the original result")); err != nil || answer != api.ViewAlreadyRecorded {
		t.Fatalf("the result again: %q %v", answer, err)
	}
	if answer, refusal := c.upload("t1", token); refusal != nil || answer != api.ViewAlreadyRecorded {
		t.Fatalf("the view again: %q %v", answer, refusal)
	}
	if len(r.captures()) != before || len(thread1(t, r).Turns) != 1 || len(thread1(t, r).Turns[0].Attempts) != 1 {
		t.Fatal("a resubmission recorded something")
	}
}

func TestAResultAcknowledgedBeforeARestartIsRecordedOnceAfterIt(t *testing.T) {
	t.Parallel()
	r := newRig(t, "mason")
	blocked, entered := gate()
	r.turns.Capture = blocked
	c := r.client("node-a")
	c.connect(api.ClientConnectRequest{})
	c.serve()
	r.place(c.node)
	r.queue("t1")
	r.run(context.Background(), "t1")
	offer := c.nextOffer()
	c.claim(offer, map[string]string{"src/main.go": "x"})
	put(t, c.view, "src/main.go", "the view as the session left it")

	if answer, refusal := c.upload("t1", offer.ToolToken); refusal != nil || answer != api.ViewAccepted {
		t.Fatalf("view: %q %v", answer, refusal)
	}
	if answer, err := c.result("t1", complete("the original result"), api.SessionFile{Name: "session.json", Data: []byte(`{"turn":"t1"}`)}); err != nil || answer != api.ViewAccepted {
		t.Fatalf("result: %q %v", answer, err)
	}
	if _, err := os.Stat(r.resultFile("t1")); err != nil {
		t.Fatalf("the result was acknowledged without being stored: %v", err)
	}
	<-entered
	if th := thread1(t, r); th.Turns[0].Response != nil {
		t.Fatalf("the execution path recorded the outcome before the restart: %+v", th.Turns[0].Response)
	}

	r.restart()
	assertRecordedOnce(t, r, "the original result")
	if captures := r.captures(); len(captures) != 1 || captures[0].files["src/main.go"] != "the view as the session left it" {
		t.Fatalf("captures: %+v", captures)
	}
	back := r.client("node-a")
	back.connect(api.ClientConnectRequest{})
	assertResubmissionsAreAlreadyRecorded(t, r, back, offer.ToolToken)
	assertRecordedOnce(t, r, "the original result")
}

func TestAResultAcknowledgedBeforeItsViewIsCompletedByTheViewAfterARestart(t *testing.T) {
	t.Parallel()
	r := newRig(t, "mason")
	r.d.ViewWait = 24 * time.Hour
	c := r.client("node-a")
	c.connect(api.ClientConnectRequest{})
	c.serve()
	r.place(c.node)
	r.queue("t1")
	r.run(context.Background(), "t1")
	offer := c.nextOffer()
	c.claim(offer, map[string]string{"src/main.go": "x"})
	put(t, c.view, "src/main.go", "the view as the session left it")
	if answer, err := c.result("t1", complete("the original result")); err != nil || answer != api.ViewAccepted {
		t.Fatalf("result: %q %v", answer, err)
	}

	r.restart()
	if !r.d.Held("t1") || len(r.captures()) != 0 {
		t.Fatal("a turn with its result but not its view finished")
	}
	back := r.client("node-a")
	back.view = c.view
	resp := back.connect(api.ClientConnectRequest{Running: []string{"t1"}})
	if len(resp.Turns) != 1 || resp.Turns[0].Answer != api.TurnReattached {
		t.Fatalf("reattach: %+v", resp.Turns)
	}
	if answer, refusal := back.upload("t1", resp.Turns[0].Token); refusal != nil || answer != api.ViewAccepted {
		t.Fatalf("view: %q %v", answer, refusal)
	}
	assertRecordedOnce(t, r, "the original result")
	if captures := r.captures(); len(captures) != 1 || captures[0].files["src/main.go"] != "the view as the session left it" {
		t.Fatalf("captures: %+v", captures)
	}
	assertResubmissionsAreAlreadyRecorded(t, r, back, resp.Turns[0].Token)
}

func TestAReattachedTurnsAcknowledgedResultIsRecordedOnceAfterAnotherRestart(t *testing.T) {
	t.Parallel()
	r, _, _, back, resp := startRestarted(t, true)
	blocked, entered := gate()
	r.d.Bind(project, Binding{Repository: r.repo, Runner: r.runner, Capture: blocked, Rehost: r.turns.Rehost})
	go func() {
		_, _ = r.fleet.Result(context.Background(), back.lease, back.node, "t1", api.TurnResult{Result: complete("the original result")})
	}()
	<-entered
	if th := thread1(t, r); th.Turns[0].Response != nil {
		t.Fatalf("the outcome was recorded before the second restart: %+v", th.Turns[0].Response)
	}
	if _, err := os.Stat(r.resultFile("t1")); err != nil {
		t.Fatalf("the result is not stored: %v", err)
	}

	r.restart()
	assertRecordedOnce(t, r, "the original result")
	if captures := r.captures(); len(captures) != 1 || captures[0].files["src/main.go"] != "the view as the session left it" {
		t.Fatalf("captures: %+v", captures)
	}
	again := r.client("node-a")
	again.connect(api.ClientConnectRequest{})
	assertResubmissionsAreAlreadyRecorded(t, r, again, resp.Turns[0].Token)
}

func TestAResultIsNotAnsweredAcceptedUntilItIsStored(t *testing.T) {
	t.Parallel()
	r := newRig(t, "mason")
	blocked, _ := gate()
	r.turns.Capture = blocked
	c := r.client("node-a")
	c.connect(api.ClientConnectRequest{})
	c.serve()
	r.place(c.node)
	r.queue("t1")
	r.run(context.Background(), "t1")
	offer := c.nextOffer()
	c.claim(offer, map[string]string{"src/main.go": "x"})
	put(t, c.view, "src/main.go", "the view as the session left it")
	if answer, refusal := c.upload("t1", offer.ToolToken); refusal != nil || answer != api.ViewAccepted {
		t.Fatalf("view: %q %v", answer, refusal)
	}

	must(t, os.WriteFile(filepath.Join(r.d.Directory, "results"), []byte("not a directory"), 0o600))
	if answer, err := c.result("t1", complete("the original result")); err == nil || answer == api.ViewAccepted || answer == api.ViewAlreadyRecorded {
		t.Fatalf("a result that could not be stored was answered %q %v", answer, err)
	}
	r.restart()
	if !r.d.Held("t1") || len(r.captures()) != 0 {
		t.Fatal("a result that was never acknowledged finished the turn")
	}
	if th := thread1(t, r); th.Turns[0].Response != nil {
		t.Fatalf("a result that was never stored was recorded: %+v", th.Turns[0].Response)
	}

	must(t, os.Remove(filepath.Join(r.d.Directory, "results")))
	back := r.client("node-a")
	back.view = c.view
	back.connect(api.ClientConnectRequest{Running: []string{"t1"}})
	if answer, err := back.result("t1", complete("the original result")); err != nil || answer != api.ViewAccepted {
		t.Fatalf("the result sent again: %q %v", answer, err)
	}
	assertRecordedOnce(t, r, "the original result")
	if len(r.captures()) != 1 {
		t.Fatalf("captures: %+v", r.captures())
	}
}
