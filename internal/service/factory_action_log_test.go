package service

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/coreadapter/adaptertest"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
)

// decodeFactoryLog parses text as one JSON-line record per line, failing
// the test if any line is not a JSON object with a timestamp, a level and a
// message.
func decodeFactoryLog(t *testing.T, text string) []map[string]any {
	t.Helper()
	var records []map[string]any
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("line is not a JSON object: %s: %v", line, err)
		}
		if record["time"] == nil || record["level"] == nil || record["msg"] == nil {
			t.Fatalf("line is missing a timestamp, level or message: %s", line)
		}
		records = append(records, record)
	}
	return records
}

// findFactoryLog returns every record whose message is msg.
func findFactoryLog(records []map[string]any, msg string) []map[string]any {
	var out []map[string]any
	for _, r := range records {
		if r["msg"] == msg {
			out = append(out, r)
		}
	}
	return out
}

// requireFactoryLog fails the test unless at least one record named msg
// matches every key/value pair in want.
func requireFactoryLog(t *testing.T, records []map[string]any, msg string, want map[string]any) map[string]any {
	t.Helper()
	for _, r := range findFactoryLog(records, msg) {
		ok := true
		for k, v := range want {
			if r[k] != v {
				ok = false
				break
			}
		}
		if ok {
			return r
		}
	}
	t.Fatalf("no %q record matching %+v among %+v", msg, want, findFactoryLog(records, msg))
	return nil
}

// TestFactoryActionLogRecordsCoreServiceAndFactoryEvents shows that a
// service with fake agents and a fake engine logs, through the logger its
// Options carries: the service starting and stopping, a project starting,
// a reload, an API request, a pause being set and cleared, and an agent
// turn being dispatched and finished, with the project, workstream and
// unit on every record that has one.
func TestFactoryActionLogRecordsCoreServiceAndFactoryEvents(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	home, err := os.MkdirTemp("", "fal-")
	must(t, err)
	t.Cleanup(func() { os.RemoveAll(home) })
	opts := fixtureAt(t, home)
	cfg, err := config.Load(opts.Config)
	must(t, err)
	root := cfg.Root.String()

	clock := &demoClock{At: demoStart}
	owner := trace.Actor{Kind: "owner", ID: "local"}
	repo, err := trace.Create(ctx, cfg.Root, cfg.Project, clock.Now(), owner)
	must(t, err)
	must(t, repo.CreateWorkstream(ctx, stream, clock.Now(), owner))
	identity := trace.Agent{Header: trace.Header{Schema: "osmia.trace.agent", Version: 1, Revision: 1, ID: demoAgent, Project: project, Workstream: stream, At: clock.Now(), Actor: owner, Cause: "workstream_created"}, Role: demoRole, ThreadID: demoThread}
	must(t, repo.CreateThread(ctx, identity))
	must(t, repo.Close())

	// adaptertest.Turns is a fake engine: it answers a scripted result for
	// each turn instead of calling any real model backend.
	reply := adaptertest.Reply[coreadapter.SessionResult]{Value: coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "fake", ID: "session"}, FinalResponse: "Answer"}}
	turns := &adaptertest.Turns{Script: *adaptertest.NewScript[coreadapter.PreparedTurn](reply)}
	lives := make(chan *trace.Repository, 1)
	opts.Threads = func(r *trace.Repository, _ *config.Config) (coreadapter.Reconciler, error) {
		lives <- r
		return thread.Dispatcher{Runner: thread.Runner{Store: r, Turns: turns, Now: clock.Now},
			Prepare: func(_ context.Context, in thread.TurnInput) (coreadapter.PreparedTurn, error) {
				return coreadapter.PreparedTurn{SessionDirectory: filepath.Join(root, "sessions", in.Agent, in.Turn)}, nil
			}}, nil
	}
	ticks := make(chan time.Time)
	opts.Reconciliation.Now, opts.Reconciliation.Ticks = clock.Now, ticks
	var passed atomic.Int64
	opts.schedulePassed = func() { passed.Add(1) }
	var buf syncBuffer
	opts.Logger = slog.New(slog.NewJSONHandler(&buf, nil))

	s, c := start(t, opts)
	repo = <-lives
	tick := func() {
		t.Helper()
		select {
		case ticks <- clock.Now():
		case <-time.After(demoTimeout):
			t.Fatal("reconciliation pass did not finish")
		}
	}
	awaitPasses := func(n int64) {
		t.Helper()
		base := passed.Load()
		deadline := time.Now().Add(demoTimeout)
		for passed.Load()-base < n {
			if time.Now().After(deadline) {
				t.Fatalf("only %d of %d further reconciliation passes completed", passed.Load()-base, n)
			}
			tick()
		}
	}

	// An API request: an ordinary health check.
	_, err = c.Health(ctx)
	must(t, err)

	// A pause set and cleared through the owner's runtime controls.
	target := runtime.Target{Scope: "workstream", Project: project, Workstream: stream}
	mutation(t, c, "PUT", "pause", PauseRequest{Target: target, Mode: "soft", Reason: "Investigating", Source: "owner"})
	mutation(t, c, "DELETE", "pause", ClearPauseRequest(target))

	// A reload of the unchanged configuration on disk.
	_, err = c.Reload(ctx)
	must(t, err)

	// A queued turn dispatched to the fake engine and finished.
	queueTurn(t, repo, "build", clock.Now())
	awaitPasses(2)
	soon(t, "the queued turn to complete", func() bool {
		th, err := repo.Thread(stream, demoAgent)
		must(t, err)
		return len(th.Turns) == 1 && !th.Turns[0].CompletedAt.IsZero()
	})

	must(t, s.Close())

	records := decodeFactoryLog(t, buf.String())

	started := requireFactoryLog(t, records, "service started", nil)
	if started["version"] != opts.Build.Version || started["root"] != root {
		t.Fatalf("service started record: %+v", started)
	}
	requireFactoryLog(t, records, "project started", map[string]any{"project": string(project)})

	requireFactoryLog(t, records, "api request handled", map[string]any{"method": "GET", "path": "/v1/health"})
	requireFactoryLog(t, records, "api request handled", map[string]any{"method": "POST", "path": "/v1/reload"})

	requireFactoryLog(t, records, "pause set", map[string]any{"scope": "workstream", "project": string(project), "workstream": string(stream), "mode": "soft"})
	requireFactoryLog(t, records, "pause cleared", map[string]any{"scope": "workstream", "project": string(project), "workstream": string(stream)})

	requireFactoryLog(t, records, "reload succeeded", nil)

	dispatched := requireFactoryLog(t, records, "agent turn dispatched", map[string]any{"project": string(project), "workstream": string(stream), "role": demoRole})
	if dispatched["profile"] != "default" {
		t.Fatalf("agent turn dispatched record: %+v", dispatched)
	}
	finished := requireFactoryLog(t, records, "agent turn finished", map[string]any{"project": string(project), "workstream": string(stream), "role": demoRole})
	if finished["outcome"] == nil || finished["duration"] == nil {
		t.Fatalf("agent turn finished record: %+v", finished)
	}

	requireFactoryLog(t, records, "project stopped", map[string]any{"project": string(project), "reason": "service stopped"})
	requireFactoryLog(t, records, "service stopped", map[string]any{"reason": "stopped"})
}

// TestFactoryActionLogDoesNotChangeOutcomeWhenTheWriterFails shows that a
// failing log writer never fails or changes the result of the action being
// logged: the owner's pause still applies, and the API still answers
// successfully, even though every write to the logger's underlying writer
// fails.
func TestFactoryActionLogDoesNotChangeOutcomeWhenTheWriterFails(t *testing.T) {
	t.Parallel()
	opts := fixture(t)
	opts.Logger = slog.New(slog.NewJSONHandler(failingWriter{}, nil))
	_, c := start(t, opts)
	target := runtime.Target{Scope: "factory"}
	mutation(t, c, "PUT", "pause", PauseRequest{Target: target, Mode: "soft", Reason: "Investigating", Source: "owner"})
	health, err := c.Health(context.Background())
	must(t, err)
	if !health.Ready {
		t.Fatal(health)
	}
	mutation(t, c, "DELETE", "pause", ClearPauseRequest(target))
}

// TestDescribeQueuedTurnReadsRoleProfileAndUnit shows that the agent turn
// log records' role, profile and unit come from the thread's own identity
// and the queued turn's request, and that an unknown turn ID yields the
// role alone rather than failing.
func TestDescribeQueuedTurnReadsRoleProfileAndUnit(t *testing.T) {
	t.Parallel()
	th := trace.Thread{Identity: trace.Agent{Role: "mason"}, Turns: []trace.QueuedTurn{
		{Request: trace.TurnRequest{Header: trace.Header{Unit: "unit-a"}, TurnID: "t1", Profile: coreadapter.Profile{Name: "sonnet"}}},
		{Request: trace.TurnRequest{Header: trace.Header{Unit: "unit-b"}, TurnID: "t2", Profile: coreadapter.Profile{Name: "opus"}}},
	}}
	if role, profile, unit := describeQueuedTurn(th, "t2"); role != "mason" || profile != "opus" || unit != "unit-b" {
		t.Fatalf("describeQueuedTurn(t2) = %q, %q, %q", role, profile, unit)
	}
	if role, profile, unit := describeQueuedTurn(th, "unknown"); role != "mason" || profile != "" || unit != "" {
		t.Fatalf("describeQueuedTurn(unknown) = %q, %q, %q", role, profile, unit)
	}
}

// TestFactoryActionLogRecordsNotificationAttemptWithoutTheWebhookURL shows
// that a notification post attempt and its outcome are logged, and that the
// webhook URL never appears anywhere in the logged output, even though the
// post itself succeeds against it.
func TestFactoryActionLogRecordsNotificationAttemptWithoutTheWebhookURL(t *testing.T) {
	t.Parallel()
	hook := newFakeWebhook(t)
	opts, inbox := notifyFixture(t, "")
	hook.records(opts)
	setWebhook(t, opts, hook.hook())
	var buf syncBuffer
	opts.Logger = slog.New(slog.NewJSONHandler(&buf, nil))
	s, _ := startNotifying(t, opts, inbox)
	// The notifier's first pass with a webhook set skips whatever is already
	// open rather than back-filling it; wait for that bootstrap pass before
	// opening the entry this test posts.
	soon(t, "the ledger enabled", func() bool { return readLedger(t, opts).Enabled })
	inbox.set(entryOf(InboxEscalation, 1))
	hook.await(t, 1)
	soon(t, "the notification recorded as sent", func() bool {
		rec := readLedger(t, opts).Notifications[notificationKey(project, entryOf(InboxEscalation, 1))]
		return rec != nil && rec.State == notificationSent
	})
	must(t, s.Close())

	text := buf.String()
	if strings.Contains(text, hook.hook()) || strings.Contains(text, hook.URL) {
		t.Fatalf("log output carries the webhook URL: %s", text)
	}
	records := decodeFactoryLog(t, buf.String())
	requireFactoryLog(t, records, "notification post attempt succeeded", map[string]any{"kind": InboxEscalation, "workstream": string(stream)})
}

// failingWriter always fails to write, so every structured log record the
// service produces fails to record.
type failingWriter struct{}

func (failingWriter) Write(p []byte) (int, error) {
	return 0, errInjectedWriteFailure
}

var errInjectedWriteFailure = errors.New("injected log writer failure")
