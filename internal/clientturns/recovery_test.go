package clientturns

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/trace"
)

func (r *rig) expire() {
	r.clock.advance(3 * time.Minute)
	r.fleet.Expire()
}

func startPartitioned(t *testing.T, lateWait time.Duration) (*rig, *fakeClient, api.TurnOffer, <-chan outcome) {
	t.Helper()
	r := newRig(t, "mason")
	r.d.LateWait = lateWait
	c := r.client("node-a")
	c.connect(api.ClientConnectRequest{})
	c.serve()
	r.place(c.node)
	r.queue("t1")
	done := r.run(context.Background(), "t1")
	offer := c.nextOffer()
	c.claim(offer, map[string]string{"src/main.go": "package main\n", "docs/notes.md": "# notes\n"})
	put(t, c.view, "src/main.go", "package main\n// work done before the partition\n")
	return r, c, offer, done
}

func TestAViewReturnedWithinTheBoundedWaitIsCapturedBeforeRecovery(t *testing.T) {
	t.Parallel()
	r, c, offer, done := startPartitioned(t, 10*time.Second)
	r.expire()
	if answer, refusal := c.upload("t1", offer.ToolToken); refusal != nil || answer != api.ViewAccepted {
		t.Fatalf("the view the lost client returned: %q %v", answer, refusal)
	}
	out := waitFor(t, done, "the interrupted turn")
	if out.turn.Response == nil || !out.turn.Response.Result.Cancelled || out.turn.Status() != "interrupted" {
		t.Fatalf("the turn did not end interrupted: %+v %v", out.turn.Response, out.err)
	}
	captures := r.captures()
	if len(captures) != 1 || captures[0].files["src/main.go"] != "package main\n// work done before the partition\n" {
		t.Fatalf("the returned view was not captured: %+v", captures)
	}

	c.connect(api.ClientConnectRequest{})
	before := thread1(t, r)
	if answer, err := c.result("t1", complete("finished while away")); err != nil || answer != api.ViewSuperseded {
		t.Fatalf("a result after recovery began: %q %v", answer, err)
	}
	if answer, refusal := c.upload("t1", offer.ToolToken); refusal != nil || answer != api.ViewAlreadyRecorded {
		t.Fatalf("the same view again: %q %v", answer, refusal)
	}
	if after := thread1(t, r); len(after.Turns) != len(before.Turns) || after.Turns[0].Response.Result.Cancelled != true || after.Turns[0].Response.Result.Outcome != nil {
		t.Fatalf("a late result changed the recorded turn: %+v", after.Turns[0].Response)
	}
	if len(r.captures()) != 1 {
		t.Fatal("a late return was captured")
	}
	late := eventsOf(t, r, "t1", trace.ClientLate)
	if len(late) != 1 || late[0].What != "result" || late[0].Client != "node-a" {
		t.Fatalf("the late result's arrival was not recorded: %+v", late)
	}
}

func TestAViewThatArrivesAfterRecoveryBeganIsSupersededAndRecorded(t *testing.T) {
	t.Parallel()
	r, c, offer, done := startPartitioned(t, 300*time.Millisecond)
	r.expire()
	out := waitFor(t, done, "the interrupted turn")
	if out.turn.Response == nil || out.turn.Status() != "interrupted" || !out.turn.Response.Result.Cancelled {
		t.Fatalf("the turn did not end interrupted: %+v %v", out.turn.Response, out.err)
	}
	if len(r.captures()) != 0 {
		t.Fatal("a view that never arrived was captured")
	}
	if out.turn.Response.Result.Client != "node-a" {
		t.Fatalf("the interrupted turn does not name its client: %+v", out.turn.Response.Result)
	}

	answer, refusal := c.upload("t1", offer.ToolToken)
	if refusal != nil || answer != api.ViewSuperseded {
		t.Fatalf("the late view: %q %v", answer, refusal)
	}
	c.connect(api.ClientConnectRequest{})
	if answer, err := c.result("t1", complete("done")); err != nil || answer != api.ViewSuperseded {
		t.Fatalf("the late result: %q %v", answer, err)
	}
	if len(r.captures()) != 0 {
		t.Fatal("a late view was captured")
	}
	th := thread1(t, r)
	if len(th.Turns) != 1 || th.Turns[0].Response.Result.Outcome != nil {
		t.Fatalf("a late return changed the recorded turn: %+v", th.Turns)
	}
	late := eventsOf(t, r, "t1", trace.ClientLate)
	if len(late) != 2 || late[0].What != "view" || late[1].What != "result" {
		t.Fatalf("the late arrivals were not recorded: %+v", late)
	}
	if entries, _ := trace.Read[trace.Transition](r.repo, stream); len(entries) != 0 {
		t.Fatalf("losing a client raised a gate: %+v", entries)
	}
}

func TestResendingAResultAndViewIsAnsweredAlreadyRecorded(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newRig(t, "mason")
	c := r.client("node-a")
	c.connect(api.ClientConnectRequest{})
	c.serve()
	r.place(c.node)
	r.queue("t1")
	done := r.run(ctx, "t1")
	offer := c.nextOffer()
	c.claim(offer, map[string]string{"src/main.go": "x"})
	put(t, c.view, "src/main.go", "y")

	if answer, refusal := c.upload("t1", offer.ToolToken); refusal != nil || answer != api.ViewAccepted {
		t.Fatalf("view: %q %v", answer, refusal)
	}
	if answer, refusal := c.upload("t1", offer.ToolToken); refusal != nil || answer != api.ViewAlreadyRecorded {
		t.Fatalf("view again before the result: %q %v", answer, refusal)
	}
	if answer, err := c.result("t1", complete("done")); err != nil || answer != api.ViewAccepted {
		t.Fatalf("result: %q %v", answer, err)
	}
	waitFor(t, done, "the turn")

	check := func(when string) {
		t.Helper()
		if answer, refusal := c.upload("t1", offer.ToolToken); refusal != nil || answer != api.ViewAlreadyRecorded {
			t.Fatalf("%s: view %q %v", when, answer, refusal)
		}
		if answer, err := c.result("t1", complete("done")); err != nil || answer != api.ViewAlreadyRecorded {
			t.Fatalf("%s: result %q %v", when, answer, err)
		}
		if th := thread1(t, r); len(th.Turns) != 1 || th.Turns[0].Response == nil || len(th.Turns[0].Attempts) != 1 {
			t.Fatalf("%s: %+v", when, th.Turns)
		}
		if len(r.captures()) != 1 {
			t.Fatalf("%s: %d captures", when, len(r.captures()))
		}
	}
	check("after a lost acknowledgement")

	r.start("mason", r.repo)
	c.connect(api.ClientConnectRequest{})
	check("after a server restart")
	if len(eventsOf(t, r, "t1", trace.ClientLate)) != 0 {
		t.Fatal("a copy of an accepted return was recorded as late")
	}
	if wrong, refusal := c.upload("t1", "not-the-token"); refusal == nil || wrong != "" {
		t.Fatalf("a restarted server took a view without the turn's token: %q %v", wrong, refusal)
	}
}

func TestARemoteTurnReattachesAfterARestartAndItsResultIsRecordedOnce(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := newRig(t, "mason")
	c := r.client("node-a")
	c.connect(api.ClientConnectRequest{})
	c.serve()
	r.place(c.node)
	r.queue("t1")
	r.run(ctx, "t1")
	offer := c.nextOffer()
	c.claim(offer, map[string]string{"src/main.go": "x"})
	put(t, c.view, "src/main.go", "finished after the restart")

	must(t, r.repo.Close())
	reopened, err := trace.Open(r.root, r.project)
	must(t, err)
	t.Cleanup(func() { reopened.Close() })
	r.start("mason", reopened)
	must(t, r.d.HoldFromTrace(project, reopened))
	if !r.d.Held("t1") {
		t.Fatal("a turn running on a remote client is not held after a restart")
	}
	if th := thread1(t, r); th.Status == "interrupted" || th.Active != "t1" {
		t.Fatalf("a held turn is reported interrupted: %+v", th)
	}

	back := r.client("node-a")
	back.view = c.view
	resp := back.connect(api.ClientConnectRequest{Running: []string{"t1"}})
	if len(resp.Turns) != 1 || resp.Turns[0].Answer != api.TurnReattached || resp.Turns[0].Token == "" {
		t.Fatalf("reattach: %+v", resp.Turns)
	}
	if answer, refusal := c.upload("t1", offer.ToolToken); refusal == nil || answer != "" {
		t.Fatalf("the token from before the restart still works: %q", answer)
	}
	if answer, refusal := back.upload("t1", resp.Turns[0].Token); refusal != nil || answer != api.ViewAccepted {
		t.Fatalf("view: %q %v", answer, refusal)
	}
	if len(r.captures()) != 0 {
		t.Fatal("the view was captured before the result arrived")
	}
	if answer, err := back.result("t1", complete("done")); err != nil || answer != api.ViewAccepted {
		t.Fatalf("result: %q %v", answer, err)
	}

	th := thread1(t, r)
	if len(th.Turns) != 1 || th.Turns[0].Response == nil || th.Turns[0].CompletedAt.IsZero() || th.Active != "" {
		t.Fatalf("the reattached turn was not recorded: %+v", th)
	}
	res := th.Turns[0].Response.Result
	if res.Client != "node-a" || res.Outcome == nil || res.Outcome.Status != "complete" || res.Cancelled || th.Turns[0].Status() != "idle" {
		t.Fatalf("recorded result: %+v", res)
	}
	if captures := r.captures(); len(captures) != 1 || captures[0].files["src/main.go"] != "finished after the restart" {
		t.Fatalf("captures: %+v", captures)
	}
	if answer, err := back.result("t1", complete("done")); err != nil || answer != api.ViewAlreadyRecorded {
		t.Fatalf("result again: %q %v", answer, err)
	}
	if answer, refusal := back.upload("t1", resp.Turns[0].Token); refusal != nil || answer != api.ViewAlreadyRecorded {
		t.Fatalf("view again: %q %v", answer, refusal)
	}
	if len(r.captures()) != 1 || len(thread1(t, r).Turns) != 1 {
		t.Fatal("a second copy recorded something")
	}
	if holder, ok := thread1(t, r).SessionClient(); !ok || holder != "node-a" {
		t.Fatalf("session holder after the restart: %q %v", holder, ok)
	}
}

func TestAReattachedTurnsToolCallsWorkAfterARestartOnTheClientsFiles(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := newRig(t, "mason")
	c := r.client("node-a")
	c.connect(api.ClientConnectRequest{})
	c.serve()
	r.place(c.node)
	r.queue("t1")
	r.run(ctx, "t1")
	offer := c.nextOffer()
	c.claim(offer, map[string]string{"src/main.go": "x"})
	if status, _ := r.relay("t1", offer.ToolToken, "note", `{}`); status != 200 {
		t.Fatalf("a call before the restart: %d", status)
	}
	put(t, c.view, "src/main.go", "as the session left it")

	must(t, r.repo.Close())
	reopened, err := trace.Open(r.root, r.project)
	must(t, err)
	t.Cleanup(func() { reopened.Close() })
	r.start("mason", reopened)
	must(t, r.d.HoldFromTrace(project, reopened))
	if status, _ := r.relay("t1", offer.ToolToken, "note", `{}`); status == 200 {
		t.Fatal("a held turn's tools answered before its client reattached")
	}

	back := r.client("node-a")
	back.view = c.view
	resp := back.connect(api.ClientConnectRequest{Running: []string{"t1"}})
	back.serve()
	if len(resp.Turns) != 1 || resp.Turns[0].Answer != api.TurnReattached || resp.Turns[0].Token == "" || resp.Turns[0].Token == offer.ToolToken {
		t.Fatalf("reattach: %+v", resp.Turns)
	}
	token := resp.Turns[0].Token
	before := len(r.audits())
	if status, body := r.relay("t1", token, "note", `{"after":"restart"}`); status != 200 || !strings.Contains(body, "noted") {
		t.Fatalf("a tool call after the restart: %d %s", status, body)
	}
	if status, body := r.relay("t1", token, "file_read", `{"path":"src/main.go"}`); status != 200 || !strings.Contains(body, "as the session left it") {
		t.Fatalf("a file read after the restart does not see the client's file: %d %s", status, body)
	}
	if status, body := r.relay("t1", token, "file_write", `{"path":"src/new.go","content":"written after the restart"}`); status != 200 {
		t.Fatalf("a file write after the restart: %d %s", status, body)
	}
	if got := read(t, c.view, "src/new.go"); got != "written after the restart" {
		t.Fatalf("the write is not in the client's view: %q", got)
	}
	if status, _ := r.relay("t1", token, "file_read", `{"path":"../outside"}`); status == 200 {
		t.Fatal("a path outside the view was read after the restart")
	}
	for name, bad := range map[string]string{"the token from before the restart": offer.ToolToken, "another turn's token": "token-t2", "no token": ""} {
		if status, _ := r.relay("t1", bad, "note", `{}`); status != 403 {
			t.Fatalf("a call with %s was answered %d", name, status)
		}
	}
	if audits := r.audits(); len(audits) != before+4 || !strings.HasPrefix(audits[before], "t1 note ") {
		t.Fatalf("the calls after the restart were not audited: %v", audits)
	}
	if answer, refusal := back.upload("t1", token); refusal != nil || answer != api.ViewAccepted {
		t.Fatalf("view: %q %v", answer, refusal)
	}
	if answer, err := back.result("t1", complete("done")); err != nil || answer != api.ViewAccepted {
		t.Fatalf("result: %q %v", answer, err)
	}
	if status, _ := r.relay("t1", token, "note", `{}`); status != 403 {
		t.Fatalf("a finished turn's tools answered %d", status)
	}
}

func TestAHeldTurnIsReleasedWhenItsClientDoesNotReattachInTime(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := newRig(t, "mason")
	c := r.client("node-a")
	c.connect(api.ClientConnectRequest{})
	c.serve()
	r.place(c.node)
	r.queue("t1")
	r.run(ctx, "t1")
	offer := c.nextOffer()
	c.claim(offer, map[string]string{"src/main.go": "x"})

	must(t, r.repo.Close())
	reopened, err := trace.Open(r.root, r.project)
	must(t, err)
	t.Cleanup(func() { reopened.Close() })
	r.start("mason", reopened)
	must(t, r.d.HoldFromTrace(project, reopened))
	if len(r.d.Sweep()) != 0 {
		t.Fatal("a turn was released inside its window")
	}
	r.clock.advance(2 * time.Minute)
	expired := r.d.Sweep()
	if len(expired) != 1 || expired[0].Turn != "t1" || expired[0].Agent != "agent" || r.d.Held("t1") {
		t.Fatalf("expired: %+v", expired)
	}
	if th := thread1(t, r); th.Status != "interrupted" {
		t.Fatalf("a released turn is not interrupted: %+v", th)
	}
	back := r.client("node-a")
	resp := back.connect(api.ClientConnectRequest{Running: []string{"t1"}})
	if len(resp.Turns) != 1 || resp.Turns[0].Answer != api.TurnLost {
		t.Fatalf("a client that reattached too late: %+v", resp.Turns)
	}
	if answer, err := back.result("t1", complete("done")); err != nil || answer != api.ViewSuperseded {
		t.Fatalf("a result for a released turn: %q %v", answer, err)
	}
}

func startRestarted(t *testing.T, accept bool) (*rig, *fakeClient, api.TurnOffer, *fakeClient, api.ClientConnectResponse) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	r := newRig(t, "mason")
	c := r.client("node-a")
	c.connect(api.ClientConnectRequest{})
	c.serve()
	r.place(c.node)
	r.queue("t1")
	r.run(ctx, "t1")
	offer := c.nextOffer()
	c.claim(offer, map[string]string{"src/main.go": "x"})
	put(t, c.view, "src/main.go", "the view as the session left it")
	if accept {
		if answer, refusal := c.upload("t1", offer.ToolToken); refusal != nil || answer != api.ViewAccepted {
			t.Fatalf("view before the restart: %q %v", answer, refusal)
		}
	}

	must(t, r.repo.Close())
	reopened, err := trace.Open(r.root, r.project)
	must(t, err)
	t.Cleanup(func() { reopened.Close() })
	r.start("mason", reopened)
	must(t, r.d.HoldFromTrace(project, reopened))

	back := r.client("node-a")
	back.view = c.view
	resp := back.connect(api.ClientConnectRequest{Running: []string{"t1"}})
	if len(resp.Turns) != 1 || resp.Turns[0].Answer != api.TurnReattached {
		t.Fatalf("reattach: %+v", resp.Turns)
	}
	return r, c, offer, back, resp
}

func heartbeatPast(t *testing.T, r *rig, c *fakeClient, total time.Duration) {
	t.Helper()
	for elapsed := time.Duration(0); elapsed < total; elapsed += 30 * time.Second {
		r.clock.advance(30 * time.Second)
		if _, err := r.fleet.Heartbeat(c.lease, c.node, api.HeartbeatRequest{}); err != nil {
			t.Fatalf("heartbeat: %v", err)
		}
		if expired := r.d.Sweep(); len(expired) != 0 {
			t.Fatalf("a reattached turn whose client holds its lease expired: %+v", expired)
		}
	}
}

func TestAReattachedTurnOutlivesTheStartupHoldWhileItsClientHoldsItsLease(t *testing.T) {
	t.Parallel()
	r, _, _, back, resp := startRestarted(t, false)
	heartbeatPast(t, r, back, 5*time.Minute)
	if !r.d.Held("t1") {
		t.Fatal("the reattached turn is not held while its client's lease is live")
	}
	if th := thread1(t, r); th.Status == "interrupted" || th.Active != "t1" {
		t.Fatalf("the reattached turn is reported interrupted: %+v", th)
	}
	token := resp.Turns[0].Token
	if answer, refusal := back.upload("t1", token); refusal != nil || answer != api.ViewAccepted {
		t.Fatalf("view: %q %v", answer, refusal)
	}
	if answer, err := back.result("t1", complete("done")); err != nil || answer != api.ViewAccepted {
		t.Fatalf("result: %q %v", answer, err)
	}
	th := thread1(t, r)
	if len(th.Turns) != 1 || th.Turns[0].Response == nil || th.Turns[0].Response.Result.Cancelled || th.Turns[0].Response.Result.Outcome == nil ||
		th.Turns[0].Status() != "idle" || len(th.Turns[0].Attempts) != 1 {
		t.Fatalf("the turn was not recorded once, uninterrupted: %+v", th.Turns)
	}
	if captures := r.captures(); len(captures) != 1 || captures[0].files["src/main.go"] != "the view as the session left it" {
		t.Fatalf("captures: %+v", captures)
	}
	if expired := r.d.Sweep(); len(expired) != 0 {
		t.Fatalf("recovery started for a finished turn: %+v", expired)
	}
	if len(eventsOf(t, r, "t1", trace.ClientLate)) != 0 {
		t.Fatal("a return of an uninterrupted turn was recorded as late")
	}
}

func TestAReattachedTurnWhoseLeaseEndsWaitsForTheViewThenIsRecoveredAsInterrupted(t *testing.T) {
	t.Parallel()
	r, _, _, back, resp := startRestarted(t, false)
	token := resp.Turns[0].Token
	heartbeatPast(t, r, back, 3*time.Minute)
	r.clock.advance(3 * time.Minute)
	r.fleet.Expire()
	if expired := r.d.Sweep(); len(expired) != 0 || !r.d.Held("t1") {
		t.Fatalf("the turn was recovered without the bounded wait for the returned view: %+v", expired)
	}
	r.clock.advance(time.Second)
	expired := r.d.Sweep()
	if len(expired) != 1 || expired[0].Turn != "t1" || expired[0].Agent != "agent" || r.d.Held("t1") {
		t.Fatalf("expired after the bounded wait: %+v", expired)
	}
	if th := thread1(t, r); th.Status != "interrupted" {
		t.Fatalf("the turn is not interrupted: %+v", th)
	}
	back.connect(api.ClientConnectRequest{Running: []string{"t1"}})
	if answer, refusal := back.upload("t1", token); refusal != nil || answer != api.ViewSuperseded {
		t.Fatalf("the view after recovery began: %q %v", answer, refusal)
	}
	if answer, err := back.result("t1", complete("done")); err != nil || answer != api.ViewSuperseded {
		t.Fatalf("the result after recovery began: %q %v", answer, err)
	}
	if len(r.captures()) != 0 {
		t.Fatal("a late view was captured")
	}
	if th := thread1(t, r); len(th.Turns) != 1 || th.Turns[0].Response != nil && th.Turns[0].Response.Result.Outcome != nil {
		t.Fatalf("a late return was recorded: %+v", th.Turns)
	}
	if entries, _ := trace.Read[trace.Transition](r.repo, stream); len(entries) != 0 {
		t.Fatalf("losing a client raised a gate: %+v", entries)
	}
}

func TestAReattachedTurnWhoseLeaseEndsIsRecoveredFromTheViewItReturnsInTheBoundedWait(t *testing.T) {
	t.Parallel()
	r, _, _, back, resp := startRestarted(t, false)
	token := resp.Turns[0].Token
	r.clock.advance(3 * time.Minute)
	r.fleet.Expire()
	if r.d.Sweep(); !r.d.Held("t1") {
		t.Fatal("the turn was recovered before the client could return its view")
	}
	if answer, refusal := back.upload("t1", token); refusal != nil || answer != api.ViewAccepted {
		t.Fatalf("the view inside the bounded wait: %q %v", answer, refusal)
	}
	expired := r.d.Sweep()
	if len(expired) != 1 || expired[0].Turn != "t1" {
		t.Fatalf("expired: %+v", expired)
	}
	ready, err := os.ReadFile(filepath.Join(r.root.String(), "views", string(project), string(stream), "thread", "t1", "ready"))
	if err != nil {
		t.Fatalf("the returned view is not kept for recovery: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(r.root.String(), "views", string(project), string(stream), "thread", "t1", string(ready), "src", "main.go"))
	if err != nil || string(data) != "the view as the session left it" {
		t.Fatalf("the kept view: %q %v", data, err)
	}
	if len(r.captures()) != 0 {
		t.Fatal("the view was captured by the dispatcher instead of the recovery")
	}
}

func TestAViewAcceptedBeforeARestartIsKeptSoTheResultAloneCompletesTheTurn(t *testing.T) {
	t.Parallel()
	r, _, _, back, resp := startRestarted(t, true)
	if !r.d.Held("t1") {
		t.Fatal("the turn is not held after the restart")
	}
	if len(r.captures()) != 0 {
		t.Fatal("the view was captured before the result arrived")
	}
	if answer, err := back.result("t1", complete("done")); err != nil || answer != api.ViewAccepted {
		t.Fatalf("result: %q %v", answer, err)
	}
	th := thread1(t, r)
	if len(th.Turns) != 1 || th.Turns[0].Response == nil || th.Turns[0].Response.Result.Cancelled || th.Turns[0].Response.Result.Outcome == nil ||
		th.Turns[0].Status() != "idle" || len(th.Turns[0].Attempts) != 1 || th.Active != "" {
		t.Fatalf("the turn was not recorded once, uninterrupted: %+v", th)
	}
	if captures := r.captures(); len(captures) != 1 || captures[0].files["src/main.go"] != "the view as the session left it" {
		t.Fatalf("captures: %+v", captures)
	}
	if answer, refusal := back.upload("t1", resp.Turns[0].Token); refusal != nil || answer != api.ViewAlreadyRecorded {
		t.Fatalf("the view again: %q %v", answer, refusal)
	}
	if answer, err := back.result("t1", complete("done")); err != nil || answer != api.ViewAlreadyRecorded {
		t.Fatalf("the result again: %q %v", answer, err)
	}
	if expired := r.d.Sweep(); len(expired) != 0 {
		t.Fatalf("recovery started: %+v", expired)
	}
	if len(r.captures()) != 1 || len(thread1(t, r).Turns) != 1 || len(eventsOf(t, r, "t1", trace.ClientLate)) != 0 {
		t.Fatal("a second copy recorded something")
	}
}
