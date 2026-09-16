package trace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
)

func transaction(id string, expected uint64, from, to string, count int) Transaction {
	tx := Transaction{ExpectedVersion: expected, Transition: Transition{Header: header("transition", id), Subject: "feature", From: from, To: to, Reason: "Owner requested transition"}}
	for i := 0; i < count; i++ {
		tx.Events = append(tx.Events, Event{ID: EventID(id, fmt.Sprint(i)), Kind: "state-changed", Body: fmt.Sprintf("Event %d\n", i)})
	}
	return tx
}
func transact(t *testing.T, r *Repository, tx Transaction) WorkflowState {
	t.Helper()
	s, err := r.Transact(context.Background(), tx)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func ready(t *testing.T, r *Repository, now time.Time) []OutboxEntry {
	t.Helper()
	entries, err := r.Ready(streamID, now)
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

func TestWorkflowRoundTripAndIdentity(t *testing.T) {
	r, root, p := create(t)
	ctx := context.Background()
	tx := transaction("first", 0, "", "active", 2)
	want := WorkflowState{Version: 1, Value: "active"}
	if got := transact(t, r, tx); got != want {
		t.Fatalf("state %v", got)
	}
	transact(t, r, transaction("second", 1, "active", "waiting", 0))
	if got := transact(t, r, tx); got != want {
		t.Fatalf("retry returns original result: %v", got)
	}
	for _, change := range []func(*Transaction){
		func(x *Transaction) { x.Transition.Reason = "different" },
		func(x *Transaction) { x.Events[0].Body = "different" },
		func(x *Transaction) { x.ExpectedVersion = 2 },
		func(x *Transaction) { x.Transition.Actor = owner },
	} {
		x := tx
		x.Events = append([]Event(nil), tx.Events...)
		change(&x)
		if _, err := r.Transact(ctx, x); !errors.Is(err, ErrConflict) {
			t.Fatalf("identity reuse: %v", err)
		}
	}
	stale := transaction("stale", 0, "", "other", 1)
	if _, err := r.Transact(ctx, stale); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale: %v", err)
	}
	reused := transaction("third", 2, "waiting", "done", 1)
	reused.Events[0] = tx.Events[0]
	if _, err := r.Transact(ctx, reused); !errors.Is(err, ErrConflict) {
		t.Fatalf("event reuse: %v", err)
	}
	revised := tx.Transition
	revised.Revision = 2
	if err := r.Append(ctx, revised); !errors.Is(err, ErrConflict) {
		t.Fatalf("managed transition append: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(root, p)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	s, err := reopened.Workflow(streamID, "feature")
	if err != nil || s != (WorkflowState{2, "waiting"}) {
		t.Fatalf("reopened: %v %v", s, err)
	}
	records, err := Read[Transition](reopened, streamID)
	if err != nil || len(records) != 2 || !reflect.DeepEqual(records[0], tx.Transition) {
		t.Fatalf("transitions: %#v %v", records, err)
	}
	if entries := ready(t, reopened, at); len(entries) != 2 {
		t.Fatalf("ready without wakeup: %#v", entries)
	}
	if got := transact(t, reopened, tx); got != want {
		t.Fatalf("reopened retry: %v", got)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := reopened.WaitWorkflow(cancelled, nil); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestWorkflowPublicationFailures(t *testing.T) {
	pre := []string{"objects-written", "objects-synced", "journal-written", "before-ref"}
	for _, phase := range []string{"written", "synced", "renamed", "directory-synced"} {
		pre = append(pre, "file:"+publicationFile+":"+phase)
	}
	pre = append(pre, "file:.git/refs/heads/main:written", "file:.git/refs/heads/main:synced")
	post := []string{"file:.git/refs/heads/main:renamed", "file:.git/refs/heads/main:directory-synced", "ref-published", "recovery-ref-synced", "materialized:events.jsonl", "materialized:workflow.json", "before-journal-removal"}
	for _, name := range []string{"events.jsonl", "workflow.json"} {
		for _, phase := range []string{"written", "synced", "renamed", "directory-synced"} {
			post = append(post, "file:workstreams/"+string(streamID)+"/"+name+":"+phase)
		}
	}
	for _, step := range append(pre, post...) {
		t.Run(step, func(t *testing.T) {
			r, root, p := create(t)
			transact(t, r, transaction("initial", 0, "", "active", 1))
			tx := transaction("next", 1, "active", "waiting", 2)
			injected := errors.New("injected publication failure")
			hit := false
			r.failPublication = func(got string) error {
				if got == step {
					hit = true
					return injected
				}
				return nil
			}
			if _, err := r.Transact(context.Background(), tx); !errors.Is(err, injected) || !hit {
				t.Fatalf("injection not reached: %v", err)
			}
			r.failPublication = nil
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}
			r, err := Open(root, p)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			committed := false
			for _, s := range post {
				committed = committed || step == s
			}
			wantVersion, wantEvents := uint64(1), 1
			if committed {
				wantVersion, wantEvents = 2, 3
			}
			state, err := r.Workflow(streamID, "feature")
			if err != nil || state.Version != wantVersion {
				t.Fatalf("state: %v %v", state, err)
			}
			if got := ready(t, r, at); len(got) != wantEvents {
				t.Fatalf("partial outbox: %#v", got)
			}
			transitions, err := Read[Transition](r, streamID)
			if err != nil || len(transitions) != int(wantVersion) {
				t.Fatalf("partial trace: %#v %v", transitions, err)
			}
			transact(t, r, tx)
			transact(t, r, tx)
			if got := ready(t, r, at); len(got) != 3 {
				t.Fatalf("duplicate retry: %#v", got)
			}
			if _, err := r.dir.Stat(publicationFile); !os.IsNotExist(err) {
				t.Fatalf("journal retained: %v", err)
			}
		})
	}
}

func TestOutboxClaimsAndRedelivery(t *testing.T) {
	r, root, p := create(t)
	ctx := context.Background()
	tx := transaction("first", 0, "", "active", 1)
	transact(t, r, tx)
	event := tx.Events[0].ID
	claim := func(token string, now time.Time) DeliveryAction {
		t.Helper()
		a, err := r.Claim(ctx, streamID, event, token, "worker", now, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	first := claim("attempt1", at)
	if again := claim("attempt1", at); again != first {
		t.Fatalf("claim retry: %v", again)
	}
	if _, err := r.Claim(ctx, streamID, event, "attempt1", "other", at, time.Minute); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	if got := ready(t, r, at); len(got) != 0 {
		t.Fatalf("claimed entry ready: %v", got)
	}
	if _, err := r.Claim(ctx, streamID, event, "attempt2", "other", at, time.Minute); !errors.Is(err, ErrClaimed) {
		t.Fatal(err)
	}
	later := at.Add(time.Minute)
	if got := ready(t, r, later); len(got) != 1 {
		t.Fatalf("expired claim missing: %v", got)
	}
	if err := r.Acknowledge(ctx, streamID, event, "attempt1", later); !errors.Is(err, ErrClaim) {
		t.Fatalf("expired ack: %v", err)
	}
	claim("attempt2", later)
	if err := r.Release(ctx, streamID, event, "attempt1", later); !errors.Is(err, ErrClaim) {
		t.Fatalf("stale release: %v", err)
	}
	if err := r.Acknowledge(ctx, streamID, event, "attempt1", later); !errors.Is(err, ErrClaim) {
		t.Fatalf("stale ack: %v", err)
	}
	if err := r.Release(ctx, streamID, event, "attempt2", later); err != nil {
		t.Fatal(err)
	}
	if err := r.Release(ctx, streamID, event, "attempt2", later.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if got := ready(t, r, later); len(got) != 1 {
		t.Fatalf("released claim missing: %v", got)
	}
	claim("attempt3", later)
	if err := r.Release(ctx, streamID, event, "attempt2", later); err != nil {
		t.Fatal(err)
	}
	if got := ready(t, r, later); len(got) != 0 {
		t.Fatalf("duplicate release cleared newer claim: %v", got)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := Open(root, p)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if got := ready(t, r, later); len(got) != 1 {
		t.Fatalf("restart claim missing: %v", got)
	}
	if err := r.Acknowledge(ctx, streamID, event, "attempt3", later); !errors.Is(err, ErrClaim) {
		t.Fatalf("old session ack: %v", err)
	}
	if _, err := r.Claim(ctx, streamID, event, "attempt3", "worker", later, time.Minute); !errors.Is(err, ErrClaim) {
		t.Fatalf("old claim retry: %v", err)
	}
	claim("attempt4", later)
	if err := r.Acknowledge(ctx, streamID, event, "attempt4", later); err != nil {
		t.Fatal(err)
	}
	if err := r.Acknowledge(ctx, streamID, event, "attempt4", later.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if got := ready(t, r, later.Add(time.Hour)); len(got) != 0 {
		t.Fatalf("acknowledged entry ready: %v", got)
	}
	all, err := r.Outbox(streamID)
	if err != nil || len(all) != 1 || len(all[0].History) != 6 || !all[0].Acknowledged {
		t.Fatalf("history: %#v %v", all, err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r, err = Open(root, p)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := r.Acknowledge(ctx, streamID, event, "attempt4", later); err != nil {
		t.Fatal(err)
	}
	if got := ready(t, r, later); len(got) != 0 {
		t.Fatalf("ack lost on restart: %v", got)
	}
}

func TestWorkflowConcurrentWritersAndClaimers(t *testing.T) {
	r, root, p := create(t)
	if other, err := Open(root, p); !errors.Is(err, ErrLocked) {
		if other != nil {
			other.Close()
		}
		t.Fatalf("second writer: %v", err)
	}
	const count = 8
	results := make(chan error, count)
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := r.Transact(context.Background(), transaction(fmt.Sprintf("tx%d", i), 0, "", "active", 1))
			results <- err
		}(i)
	}
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		} else if !errors.Is(err, ErrConflict) {
			t.Fatal(err)
		}
	}
	if successes != 1 {
		t.Fatalf("successful writers: %d", successes)
	}
	entries := ready(t, r, at)
	if len(entries) != 1 {
		t.Fatal(entries)
	}
	event := entries[0].Event.ID
	results = make(chan error, count)
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := r.Claim(context.Background(), streamID, event, fmt.Sprintf("claim%d", i), "worker", at, time.Minute)
			results <- err
		}(i)
	}
	wg.Wait()
	close(results)
	successes = 0
	for err := range results {
		if err == nil {
			successes++
		} else if !errors.Is(err, ErrClaimed) {
			t.Fatal(err)
		}
	}
	if successes != 1 {
		t.Fatalf("successful claims: %d", successes)
	}
}

func TestDeliveryPublicationFailure(t *testing.T) {
	for _, step := range []string{"before-ref", "ref-published", "materialized:workflow.json"} {
		t.Run(step, func(t *testing.T) {
			r, root, p := create(t)
			ctx := context.Background()
			tx := transaction("tx", 0, "", "active", 1)
			transact(t, r, tx)
			event := tx.Events[0].ID
			if _, err := r.Claim(ctx, streamID, event, "attempt", "worker", at, time.Minute); err != nil {
				t.Fatal(err)
			}
			injected := errors.New("stop")
			r.failPublication = func(got string) error {
				if got == step {
					return injected
				}
				return nil
			}
			if err := r.Acknowledge(ctx, streamID, event, "attempt", at); !errors.Is(err, injected) {
				t.Fatal(err)
			}
			r.failPublication = nil
			r.Close()
			r, err := Open(root, p)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			want := 0
			if step == "before-ref" {
				want = 1
			}
			if got := ready(t, r, at); len(got) != want {
				t.Fatalf("ready %v", got)
			}
			if want == 0 {
				if err := r.Acknowledge(ctx, streamID, event, "attempt", at); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestWorkflowRejectsInvalidAndCorruptData(t *testing.T) {
	r, root, p := create(t)
	ctx := context.Background()
	for _, modify := range []func(*Transaction){
		func(tx *Transaction) { tx.Transition.From = "unobserved" },
		func(tx *Transaction) { tx.Transition.Revision = 2 },
		func(tx *Transaction) { tx.Transition.Actor = Actor{} },
		func(tx *Transaction) { tx.Events = append(tx.Events, tx.Events[0]) },
		func(tx *Transaction) { tx.Events[0].ID = "../escape" },
	} {
		tx := transaction("tx", 0, "", "active", 1)
		modify(&tx)
		if _, err := r.Transact(ctx, tx); err == nil {
			t.Fatal("invalid transaction accepted")
		}
	}
	if got := ready(t, r, at); len(got) != 0 {
		t.Fatal(got)
	}
	tx := transaction("tx", 0, "", "active", 1)
	transact(t, r, tx)
	name := "workstreams/" + string(streamID) + "/workflow.json"
	data, err := r.readFile(name)
	if err != nil {
		t.Fatal(err)
	}
	data = []byte(strings.Replace(string(data), `"expected_version": 0`, `"expected_version": 99`, 1))
	if err := r.writeFile(name, data); err != nil {
		t.Fatal(err)
	}
	if err := r.commit(ctx, []string{name}, "Corrupt fixture"); err != nil {
		t.Fatal(err)
	}
	r.Close()
	r, err = Open(root, p)
	if r != nil {
		defer r.Close()
	}
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("corrupt workflow accepted: %v", err)
	}
}

func TestWorkflowProcessInterruption(t *testing.T) {
	if step := os.Getenv("OSMIA_WORKFLOW_CRASH_STEP"); step != "" {
		root, err := config.ResolveRoot(os.Getenv("OSMIA_WORKFLOW_ROOT"), "")
		if err != nil {
			t.Fatal(err)
		}
		p := config.Project{ID: projectID, Clone: os.Getenv("OSMIA_WORKFLOW_CLONE")}
		r, err := Open(root, p)
		if err != nil {
			t.Fatal(err)
		}
		r.failPublication = func(got string) error {
			if got == step {
				os.Exit(23)
			}
			return nil
		}
		_, err = r.Transact(context.Background(), transaction("crashed", 0, "", "active", 2))
		t.Fatalf("crash boundary not reached: %v", err)
	}
	for _, step := range []string{"objects-written", "before-ref", "ref-published", "materialized:events.jsonl"} {
		t.Run(step, func(t *testing.T) {
			r, root, p := create(t)
			r.Close()
			cmd := exec.Command(os.Args[0], "-test.run=^TestWorkflowProcessInterruption$")
			cmd.Env = append(os.Environ(), "OSMIA_WORKFLOW_CRASH_STEP="+step, "OSMIA_WORKFLOW_ROOT="+root.String(), "OSMIA_WORKFLOW_CLONE="+p.Clone)
			out, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 23 {
				t.Fatalf("child: %v %s", err, out)
			}
			r, err = Open(root, p)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			want := 0
			if step == "ref-published" || step == "materialized:events.jsonl" {
				want = 2
			}
			if got := ready(t, r, at); len(got) != want {
				t.Fatalf("crash recovery: %#v", got)
			}
			transact(t, r, transaction("crashed", 0, "", "active", 2))
			if got := ready(t, r, at); len(got) != 2 {
				t.Fatalf("crash retry: %#v", got)
			}
		})
	}
}

func TestWorkflowWakeIsOnlyAHint(t *testing.T) {
	r, root, p := create(t)
	tx := transaction("tx", 0, "", "active", 1)
	transact(t, r, tx)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := r.WaitWorkflow(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if got := ready(t, r, at); len(got) != 1 {
		t.Fatal(got)
	}
	r.Close()
	r, err := Open(root, p)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	noWake, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := r.WaitWorkflow(noWake, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("unexpected replayed wake: %v", err)
	}
	if got := ready(t, r, at); len(got) != 1 {
		t.Fatal(got)
	}
	ticks := make(chan time.Time, 1)
	ticks <- at
	if err := r.WaitWorkflow(context.Background(), ticks); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentTransactionRetries(t *testing.T) {
	r, _, _ := create(t)
	tx := transaction("tx", 0, "", "active", 2)
	var wg sync.WaitGroup
	results := make(chan error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := r.Transact(context.Background(), tx); results <- err }()
	}
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	transitions, err := Read[Transition](r, streamID)
	if err != nil || len(transitions) != 1 {
		t.Fatalf("duplicate transitions: %v %v", transitions, err)
	}
	if got := ready(t, r, at); len(got) != 2 {
		t.Fatalf("duplicate events: %#v", got)
	}
}
