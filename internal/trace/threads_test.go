package trace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/coreadapter"
)

func threadAgent() Agent {
	return Agent{Header: header("agent", "mason"), Role: "mason", ThreadID: "thread", Session: coreadapter.BackendSession{Backend: "fake", ID: "original"}}
}
func threadRequest(id string) TurnRequest {
	return TurnRequest{Header: header("turn-request", "request_"+id), AgentID: "mason", ThreadID: "thread", TurnID: id, Profile: coreadapter.Profile{Name: "default", Backend: "fake", Model: "test"}, Prompt: "Message " + id}
}
func threadResponse(q QueuedTurn) TurnResponse {
	h := q.Request.Header
	h.Schema, h.ID, h.At = "osmia.trace.turn-response", "response_"+q.Request.TurnID, at.Add(time.Second)
	return TurnResponse{Header: h, AgentID: "mason", ThreadID: "thread", TurnID: q.Request.TurnID, RequestID: q.Request.ID, RequestRevision: 1, Result: coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "fake", ID: "continued"}, SessionDirectory: q.Claim.SessionDirectory, StartedAt: at, Duration: time.Second, FinalResponse: "Captured final", Outcome: &coreadapter.Outcome{Status: "waiting", Report: "Owner answer needed"}}}
}
func mustThread(t *testing.T, r *Repository) Thread {
	t.Helper()
	th, err := r.Thread(streamID, "mason")
	if err != nil {
		t.Fatal(err)
	}
	return th
}
func enqueue(t *testing.T, r *Repository, id string) QueuedTurn {
	t.Helper()
	q, err := r.EnqueueTurn(context.Background(), threadRequest(id))
	if err != nil {
		t.Fatal(err)
	}
	return q
}
func claimTurn(t *testing.T, r *Repository, token string) QueuedTurn {
	t.Helper()
	q, err := r.ClaimTurn(context.Background(), streamID, "mason", token, "/owned/session/"+token, at)
	if err != nil {
		t.Fatal(err)
	}
	return q
}
func TestThreadRoundTripAndDuplicateCalls(t *testing.T) {
	ctx := context.Background()
	r, root, p := create(t)
	a := threadAgent()
	if err := r.CreateThread(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := r.CreateThread(ctx, a); err != nil {
		t.Fatal(err)
	}
	changed := a
	changed.Role = "reviewer"
	if err := r.CreateThread(ctx, changed); !errors.Is(err, ErrConflict) {
		t.Fatalf("rebind: %v", err)
	}
	enqueue(t, r, "one")
	enqueue(t, r, "two")
	if q := enqueue(t, r, "one"); q.Sequence != 1 {
		t.Fatalf("duplicate sequence: %d", q.Sequence)
	}
	req := threadRequest("one")
	req.Prompt = "replacement"
	if _, err := r.EnqueueTurn(ctx, req); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed enqueue: %v", err)
	}
	q := claimTurn(t, r, "claim1")
	if again := claimTurn(t, r, "claim1"); !reflect.DeepEqual(q, again) {
		t.Fatalf("claim retry: %#v", again)
	}
	if _, err := r.ClaimTurn(ctx, streamID, "mason", "claim2", "/session", at); !errors.Is(err, ErrClaimed) {
		t.Fatalf("active claim: %v", err)
	}
	enqueue(t, r, "three")
	if err := r.CompleteTurn(ctx, streamID, "mason", "one", "claim1", at); !errors.Is(err, ErrClaim) {
		t.Fatalf("uncaptured completion: %v", err)
	}
	res := threadResponse(q)
	if err := r.CaptureTurn(ctx, "wrong", res); !errors.Is(err, ErrClaim) {
		t.Fatalf("foreign result: %v", err)
	}
	if err := r.CaptureTurn(ctx, "claim1", res); err != nil {
		t.Fatal(err)
	}
	if err := r.CaptureTurn(ctx, "claim1", res); err != nil {
		t.Fatal(err)
	}
	changedRes := res
	changedRes.Result.FinalResponse = "different"
	if err := r.CaptureTurn(ctx, "claim1", changedRes); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed result: %v", err)
	}
	for _, rec := range []Record{a, q.Request, res} {
		if err := r.Append(ctx, rec); !errors.Is(err, ErrConflict) {
			t.Fatalf("unmanaged append: %v", err)
		}
	}
	before := mustThread(t, r)
	if before.Status != "captured" || before.Active != "one" || before.Session.ID != "continued" {
		t.Fatalf("captured state: %#v", before)
	}
	r.Close()
	r, err := Open(root, p)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if got := mustThread(t, r); !reflect.DeepEqual(got, before) {
		t.Fatalf("reopen: %#v", got)
	}
	if err := r.CaptureTurn(ctx, "claim1", res); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := r.CompleteTurn(ctx, streamID, "mason", "one", "claim1", at.Add(2*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	if got := mustThread(t, r); got.Status != "waiting" || got.Active != "" || len(got.Turns) != 3 {
		t.Fatalf("complete: %#v", got)
	} else if got.Parked() {
		t.Fatal("thread with queued turns is parked")
	}
	if _, err := r.ClaimTurn(ctx, streamID, "mason", "claim1", q.Claim.SessionDirectory, at); !errors.Is(err, ErrClaim) {
		t.Fatalf("completed token reused: %v", err)
	}
	if next := claimTurn(t, r, "claim2"); next.Request.TurnID != "two" {
		t.Fatalf("next request: %#v", next)
	}
	requests, err := Read[TurnRequest](r, streamID)
	if err != nil || len(requests) != 3 {
		t.Fatalf("owned requests: %v %v", requests, err)
	}
	responses, err := Read[TurnResponse](r, streamID)
	if err != nil || len(responses) != 1 || !reflect.DeepEqual(responses[0], res) {
		t.Fatalf("owned responses: %v %v", responses, err)
	}
}

func TestThreadConcurrentQueueAndClaims(t *testing.T) {
	r, root, p := create(t)
	ctx := context.Background()
	if err := r.CreateThread(ctx, threadAgent()); err != nil {
		t.Fatal(err)
	}
	const count = 8
	var wg sync.WaitGroup
	for i := range count {
		wg.Go(func() {
			if _, err := r.EnqueueTurn(ctx, threadRequest(fmt.Sprintf("turn%d", i))); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	before := mustThread(t, r)
	for i, q := range before.Turns {
		if q.Sequence != uint64(i+1) {
			t.Fatalf("sequence: %#v", before.Turns)
		}
	}
	winners := make(chan QueuedTurn, count)
	for i := range count {
		wg.Go(func() {
			q, err := r.ClaimTurn(ctx, streamID, "mason", fmt.Sprintf("claim%d", i), "/session", at)
			if err == nil {
				winners <- q
			} else if !errors.Is(err, ErrClaimed) {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	close(winners)
	if len(winners) != 1 {
		t.Fatalf("winners: %d", len(winners))
	}
	winner := <-winners
	if winner.Request.TurnID != before.Turns[0].Request.TurnID {
		t.Fatal("claim violated acceptance order")
	}
	enqueue(t, r, "midturn")
	r.Close()
	r, err := Open(root, p)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	got := mustThread(t, r)
	if got.Status != "interrupted" || got.Active != winner.Request.TurnID || len(got.Turns) != count+1 || !reflect.DeepEqual(got.Turns[0].Claim, winner.Claim) {
		t.Fatalf("interrupted state: %#v", got)
	}
	for i := range count {
		if !reflect.DeepEqual(before.Turns[i].Request, got.Turns[i].Request) {
			t.Fatal("reopen reordered queue")
		}
	}
	if _, err := r.ClaimTurn(ctx, streamID, "mason", "new", "/new", at.Add(time.Hour)); !errors.Is(err, ErrClaimed) {
		t.Fatalf("interrupted claim stolen: %v", err)
	}
	if err := r.CaptureTurn(ctx, winner.Claim.Token, threadResponse(winner)); !errors.Is(err, ErrClaim) {
		t.Fatalf("stale service result: %v", err)
	}
}

func TestThreadPublicationFailures(t *testing.T) {
	for _, action := range []string{"create", "enqueue", "claim", "capture", "complete"} {
		steps := []string{"objects-written", "journal-written", "before-ref", "ref-published", "materialized:workflow.json", "before-journal-removal"}
		if action == "create" {
			steps = append(steps, "materialized:identity.jsonl")
		}
		if action == "enqueue" || action == "capture" {
			steps = append(steps, "materialized:log.jsonl")
		}
		for _, step := range steps {
			t.Run(action+"/"+step, func(t *testing.T) {
				ctx := context.Background()
				r, root, p := create(t)
				var q QueuedTurn
				var res TurnResponse
				if action != "create" {
					if err := r.CreateThread(ctx, threadAgent()); err != nil {
						t.Fatal(err)
					}
				}
				if action == "claim" || action == "capture" || action == "complete" {
					enqueue(t, r, "one")
				}
				if action == "capture" || action == "complete" {
					q = claimTurn(t, r, "token")
					res = threadResponse(q)
				}
				if action == "complete" {
					if err := r.CaptureTurn(ctx, "token", res); err != nil {
						t.Fatal(err)
					}
				}
				apply := func() error {
					switch action {
					case "create":
						return r.CreateThread(ctx, threadAgent())
					case "enqueue":
						_, err := r.EnqueueTurn(ctx, threadRequest("one"))
						return err
					case "claim":
						_, err := r.ClaimTurn(ctx, streamID, "mason", "token", "/owned/session/token", at)
						return err
					case "capture":
						return r.CaptureTurn(ctx, "token", res)
					default:
						return r.CompleteTurn(ctx, streamID, "mason", "one", "token", at.Add(2*time.Second))
					}
				}
				injected := errors.New("injected")
				hit := false
				r.failPublication = func(s string) error {
					if s == step {
						hit = true
						return injected
					}
					return nil
				}
				if err := apply(); !errors.Is(err, injected) || !hit {
					t.Fatalf("missing injection: %v %v", err, hit)
				}
				r.failPublication = nil
				// Same-session retry reconciles either side of the publication boundary.
				if err := apply(); err != nil {
					t.Fatalf("retry: %v", err)
				}
				r.Close()
				var err error
				r, err = Open(root, p)
				if err != nil {
					t.Fatal(err)
				}
				defer r.Close()
				th := mustThread(t, r)
				want := 1
				if action == "create" {
					want = 0
				}
				if len(th.Turns) != want {
					t.Fatalf("turn count: %d", len(th.Turns))
				}
				responses, err := Read[TurnResponse](r, streamID)
				if err != nil {
					t.Fatal(err)
				}
				wantResponses := 0
				if action == "capture" || action == "complete" {
					wantResponses = 1
				}
				if len(responses) != wantResponses {
					t.Fatalf("response count %d", len(responses))
				}
				if action == "claim" && th.Status != "interrupted" {
					t.Fatalf("status %s", th.Status)
				}
				if action == "complete" && (th.Status != "waiting" || th.Active != "") {
					t.Fatalf("completion: %#v", th)
				}
			})
		}
	}
}

func TestThreadReopenAtPublicationBoundary(t *testing.T) {
	for _, step := range []string{"before-ref", "ref-published", "materialized:log.jsonl"} {
		t.Run(step, func(t *testing.T) {
			r, root, p := create(t)
			ctx := context.Background()
			if err := r.CreateThread(ctx, threadAgent()); err != nil {
				t.Fatal(err)
			}
			r.failPublication = func(s string) error {
				if s == step {
					return errors.New("crash")
				}
				return nil
			}
			if _, err := r.EnqueueTurn(ctx, threadRequest("one")); err == nil {
				t.Fatal("missing injection")
			}
			r.failPublication = nil
			r.Close()
			r, err := Open(root, p)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			want := 1
			if step == "before-ref" {
				want = 0
			}
			if th := mustThread(t, r); len(th.Turns) != want {
				t.Fatalf("atomic queue: %#v", th)
			}
			reqs, err := Read[TurnRequest](r, streamID)
			if err != nil || len(reqs) != want {
				t.Fatalf("atomic log: %v %v", reqs, err)
			}
			enqueue(t, r, "one")
		})
	}
}

func TestThreadValidationAndCancellation(t *testing.T) {
	r, _, _ := create(t)
	ctx := context.Background()
	if _, err := r.Thread(streamID, "missing"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if err := r.CreateThread(ctx, threadAgent()); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*TurnRequest){
		func(q *TurnRequest) { q.ThreadID = "wrong" }, func(q *TurnRequest) { q.Profile.Backend = "" }, func(q *TurnRequest) { q.Depth = -1 }, func(q *TurnRequest) { q.Revision = 2 },
	} {
		req := threadRequest("bad")
		mutate(&req)
		if _, err := r.EnqueueTurn(ctx, req); err == nil {
			t.Fatal("invalid request accepted")
		}
	}
	if _, err := r.ClaimTurn(ctx, streamID, "mason", "empty", "/session", at); !errors.Is(err, ErrNoTurn) {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := r.EnqueueTurn(cancelled, threadRequest("cancel")); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	enqueue(t, r, "one")
	q := claimTurn(t, r, "token")
	for _, mutate := range []func(*TurnResponse){
		func(x *TurnResponse) { x.Depth++ }, func(x *TurnResponse) { x.Cause = "other" }, func(x *TurnResponse) { x.Result.SessionDirectory = "/other" }, func(x *TurnResponse) { x.RequestID = "other" }, func(x *TurnResponse) { x.Result.Session.Backend = "other" },
	} {
		res := threadResponse(q)
		mutate(&res)
		if err := r.CaptureTurn(ctx, "token", res); err == nil {
			t.Fatal("invalid response accepted")
		}
	}
	if got := mustThread(t, r); got.Status != "running" || len(got.Turns) != 1 {
		t.Fatalf("invalid write changed state: %#v", got)
	}
}

func TestThreadRestartDuringCaptureAndCompletion(t *testing.T) {
	for _, action := range []string{"claim", "capture", "complete"} {
		for _, committed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/committed=%t", action, committed), func(t *testing.T) {
				r, root, p := create(t)
				ctx := context.Background()
				if err := r.CreateThread(ctx, threadAgent()); err != nil {
					t.Fatal(err)
				}
				enqueue(t, r, "one")
				enqueue(t, r, "two")
				var q QueuedTurn
				if action != "claim" {
					q = claimTurn(t, r, "token")
				}
				res := TurnResponse{}
				if action != "claim" {
					res = threadResponse(q)
				}
				if action == "complete" {
					if err := r.CaptureTurn(ctx, "token", res); err != nil {
						t.Fatal(err)
					}
				}
				step := "before-ref"
				if committed {
					step = "ref-published"
				}
				r.failPublication = func(s string) error {
					if s == step {
						return errors.New("crash")
					}
					return nil
				}
				var err error
				switch action {
				case "claim":
					_, err = r.ClaimTurn(ctx, streamID, "mason", "token", "/owned/session/token", at)
				case "capture":
					err = r.CaptureTurn(ctx, "token", res)
				case "complete":
					err = r.CompleteTurn(ctx, streamID, "mason", "one", "token", at.Add(2*time.Second))
				}
				if err == nil {
					t.Fatal("missing crash")
				}
				r.failPublication = nil
				r.Close()
				r, err = Open(root, p)
				if err != nil {
					t.Fatal(err)
				}
				defer r.Close()
				th := mustThread(t, r)
				if len(th.Turns) != 2 || th.Turns[1].Claim != nil {
					t.Fatalf("lost successor: %#v", th)
				}
				hasResult := action == "complete" || action == "capture" && committed
				responses, err := Read[TurnResponse](r, streamID)
				if err != nil {
					t.Fatal(err)
				}
				if (len(responses) == 1) != hasResult {
					t.Fatalf("result atomicity: %#v", responses)
				}
				switch {
				case action == "claim" && !committed:
					if th.Active != "" || th.Status != "idle" {
						t.Fatalf("uncommitted claim: %#v", th)
					}
					if got := claimTurn(t, r, "fresh"); got.Request.TurnID != "one" {
						t.Fatal("lost oldest request")
					}
				case hasResult:
					if err := r.CompleteTurn(ctx, streamID, "mason", "one", "token", at.Add(2*time.Second)); err != nil {
						t.Fatal(err)
					}
					if next := claimTurn(t, r, "next"); next.Request.TurnID != "two" {
						t.Fatal("successor order")
					}
				default:
					if th.Status != "interrupted" || th.Active != "one" {
						t.Fatalf("missing interruption: %#v", th)
					}
					if _, err := r.ClaimTurn(ctx, streamID, "mason", "next", "/next", at.Add(time.Hour)); !errors.Is(err, ErrClaimed) {
						t.Fatalf("blind retry: %v", err)
					}
				}
			})
		}
	}
}

func TestThreadOwnedRecordValidation(t *testing.T) {
	for _, mode := range []string{"missing-log", "changed-queue"} {
		t.Run(mode, func(t *testing.T) {
			r, root, p := create(t)
			ctx := context.Background()
			if err := r.CreateThread(ctx, threadAgent()); err != nil {
				t.Fatal(err)
			}
			enqueue(t, r, "one")
			files := map[string][]byte{}
			if mode == "missing-log" {
				files["workstreams/"+string(streamID)+"/agents/mason/log.jsonl"] = nil
			} else {
				log, _, err := r.loadWorkflow(streamID)
				if err != nil {
					t.Fatal(err)
				}
				th := log.Threads["mason"]
				th.Turns[0].Request.Prompt = "tampered"
				log.Threads["mason"] = th
				data, err := json.Marshal(log)
				if err != nil {
					t.Fatal(err)
				}
				files["workstreams/"+string(streamID)+"/workflow.json"] = data
			}
			if err := r.publish(ctx, files); err != nil {
				t.Fatal(err)
			}
			r.Close()
			reopened, err := Open(root, p)
			if reopened != nil {
				defer reopened.Close()
			}
			if err == nil {
				t.Fatal("inconsistent owned records accepted")
			}
		})
	}
}

func TestThreadsListsSnapshotsByAgent(t *testing.T) {
	ctx := context.Background()
	r, root, p := create(t)
	for _, id := range []string{"zeta", "mason", "alpha"} {
		a := threadAgent()
		if id != "mason" {
			a.ID, a.ThreadID = id, "thread_"+id
		}
		if err := r.CreateThread(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	enqueue(t, r, "one")
	claimTurn(t, r, "token")
	r.Close()
	r, err := Open(root, p)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	threads, err := r.Threads(streamID)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, th := range threads {
		ids = append(ids, th.Identity.ID)
		one, err := r.Thread(streamID, th.Identity.ID)
		if err != nil || !reflect.DeepEqual(one, th) {
			t.Fatalf("thread %s: %+v, want %+v (%v)", th.Identity.ID, th, one, err)
		}
	}
	if want := []string{"alpha", ChiefOfStaff, "mason", "zeta"}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("threads %v, want %v", ids, want)
	}
	if th := threads[2]; th.Status != "interrupted" || th.Active != "one" {
		t.Fatalf("interrupted thread: %+v", th)
	}
	if _, err := r.Threads("w_invalid"); err == nil {
		t.Fatal("invalid workstream accepted")
	}
}

func TestThreadParked(t *testing.T) {
	done := QueuedTurn{CompletedAt: time.Unix(1, 0)}
	for _, c := range []struct {
		name   string
		thread Thread
		want   bool
	}{
		{"waiting, nothing queued", Thread{Status: "waiting", Turns: []QueuedTurn{done}}, true},
		{"waiting, turn queued", Thread{Status: "waiting", Turns: []QueuedTurn{done, {}}}, false},
		{"idle", Thread{Status: "idle", Turns: []QueuedTurn{done}}, false},
		{"failed", Thread{Status: "failed", Turns: []QueuedTurn{done}}, false},
	} {
		if got := c.thread.Parked(); got != c.want {
			t.Errorf("%s: parked %v, want %v", c.name, got, c.want)
		}
	}
}

func TestCancelTurnsCompletesUnheldTurns(t *testing.T) {
	ctx := context.Background()
	r, root, p := create(t)
	if err := r.CreateThread(ctx, threadAgent()); err != nil {
		t.Fatal(err)
	}
	enqueue(t, r, "one")
	enqueue(t, r, "two")
	claimTurn(t, r, "claim1")
	actor := Actor{Kind: "service", ID: "abandon"}
	if _, err := r.CancelTurns(ctx, streamID, time.Time{}, actor, "gone"); err == nil {
		t.Fatal("zero timestamp accepted")
	}
	if _, err := r.CancelTurns(ctx, streamID, at, actor, " "); err == nil {
		t.Fatal("empty reason accepted")
	}
	// This session's reserved turn, and its successor, are left to the runner.
	if n, err := r.CancelTurns(ctx, streamID, at, actor, "gone"); err != nil || n != 0 {
		t.Fatalf("held turn: %d %v", n, err)
	}
	r.Close()
	r, err := Open(root, p)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	later := at.Add(time.Minute)
	n, err := r.CancelTurns(ctx, streamID, later, actor, "gone")
	if err != nil || n != 2 {
		t.Fatalf("cancel: %d %v", n, err)
	}
	th := mustThread(t, r)
	if th.Active != "" || th.Status != "interrupted" {
		t.Fatalf("thread %#v", th)
	}
	for _, q := range th.Turns {
		res := q.Response
		if res == nil || !q.CompletedAt.Equal(later) || !res.Result.Cancelled || res.Failure != "gone" || res.Actor != actor || res.Result.SessionDirectory != q.Claim.SessionDirectory {
			t.Fatalf("turn %s: %#v", q.Request.TurnID, q)
		}
	}
	if th.Turns[0].Claim.Token != "claim1" {
		t.Fatalf("interrupted claim replaced: %#v", th.Turns[0].Claim)
	}
	responses, err := Read[TurnResponse](r, streamID)
	if err != nil || len(responses) != 2 {
		t.Fatalf("responses %v %v", responses, err)
	}
	if n, err := r.CancelTurns(ctx, streamID, later, actor, "gone"); err != nil || n != 0 {
		t.Fatalf("repeat: %d %v", n, err)
	}
	// A turn queued afterwards is cancelled by the next call.
	enqueue(t, r, "three")
	if n, err := r.CancelTurns(ctx, streamID, later, actor, "gone"); err != nil || n != 1 {
		t.Fatalf("new turn: %d %v", n, err)
	}
}

func TestCancelTurnsLeavesCapturedTurns(t *testing.T) {
	ctx := context.Background()
	r, _, _ := create(t)
	if err := r.CreateThread(ctx, threadAgent()); err != nil {
		t.Fatal(err)
	}
	enqueue(t, r, "one")
	enqueue(t, r, "two")
	q := claimTurn(t, r, "claim1")
	if err := r.CaptureTurn(ctx, "claim1", threadResponse(q)); err != nil {
		t.Fatal(err)
	}
	if n, err := r.CancelTurns(ctx, streamID, at, Actor{Kind: "service", ID: "abandon"}, "gone"); err != nil || n != 0 {
		t.Fatalf("captured turn: %d %v", n, err)
	}
	if th := mustThread(t, r); th.Status != "captured" || th.Turns[1].Claim != nil {
		t.Fatalf("thread %#v", th)
	}
}

// TestRecoverySettlesFinalAttempt stops a session after it recorded an
// attempt: recovery writes the response and the final attempt from one value,
// so the recovered trace opens again.
func TestRecoverySettlesFinalAttempt(t *testing.T) {
	actor := Actor{Kind: "service", ID: "abandon"}
	recorded := coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "fake", ID: "partial"}, SessionDirectory: "/owned/session/claim1", StartedAt: at, FinalResponse: "Partial", Cancelled: true}
	for _, c := range []struct {
		name    string
		result  *coreadapter.SessionResult
		recover func(context.Context, *Repository) error
		failure string
	}{
		{"abandon intent", nil, func(ctx context.Context, r *Repository) error {
			return r.AbandonTurn(ctx, streamID, "mason", "one", at.Add(time.Minute))
		}, "the service stopped before the turn captured a result"},
		{"cancel intent", nil, func(ctx context.Context, r *Repository) error {
			_, err := r.CancelTurns(ctx, streamID, at.Add(time.Minute), actor, "gone")
			return err
		}, "gone"},
		{"abandon result", &recorded, func(ctx context.Context, r *Repository) error {
			return r.AbandonTurn(ctx, streamID, "mason", "one", at.Add(time.Minute))
		}, "stopped"},
		{"cancel result", &recorded, func(ctx context.Context, r *Repository) error {
			_, err := r.CancelTurns(ctx, streamID, at.Add(time.Minute), actor, "gone")
			return err
		}, "stopped"},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			r, root, p := create(t)
			if err := r.CreateThread(ctx, threadAgent()); err != nil {
				t.Fatal(err)
			}
			enqueue(t, r, "one")
			q := claimTurn(t, r, "claim1")
			a := TurnAttempt{Number: 1, Profile: q.Request.Profile, Path: "replay", Reason: "fresh", At: at}
			if err := r.RecordAttempt(ctx, streamID, "mason", "one", "claim1", a); err != nil {
				t.Fatal(err)
			}
			if c.result != nil {
				a.Result, a.Failure = c.result, "stopped"
				if err := r.RecordAttempt(ctx, streamID, "mason", "one", "claim1", a); err != nil {
					t.Fatal(err)
				}
			}
			r.Close()
			r, err := Open(root, p)
			if err != nil {
				t.Fatal(err)
			}
			if err := c.recover(ctx, r); err != nil {
				t.Fatal(err)
			}
			r.Close()
			r, err = Open(root, p)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			th := mustThread(t, r)
			got := th.Turns[0]
			last := got.Attempts[0]
			if got.Response == nil || last.Result == nil || !reflect.DeepEqual(*last.Result, got.Response.Result) || last.Failure != c.failure || got.Response.Failure != c.failure {
				t.Fatalf("turn %+v", got)
			}
			if c.result != nil && (!reflect.DeepEqual(got.Response.Result, recorded) || th.Session != recorded.Session) {
				t.Fatalf("recorded result not kept: %+v %+v", got.Response.Result, th.Session)
			}
			if c.result == nil && (!got.Response.Result.Cancelled || th.Session != threadAgent().Session) {
				t.Fatalf("recovered result: %+v %+v", got.Response.Result, th.Session)
			}
			responses, err := Read[TurnResponse](r, streamID)
			if err != nil || len(responses) != 1 || !reflect.DeepEqual(responses[0], *got.Response) {
				t.Fatalf("owned log: %+v %v", responses, err)
			}
		})
	}
}
