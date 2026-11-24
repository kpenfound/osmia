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
	"github.com/kpenfound/osmia/internal/projectstore"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
)

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

func findFactoryLog(records []map[string]any, msg string) []map[string]any {
	var out []map[string]any
	for _, r := range records {
		if r["msg"] == msg {
			out = append(out, r)
		}
	}
	return out
}

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

type factoryLogRun struct {
	records []map[string]any
	version string
	root    string
}

// runFactoryLogScenario starts a service that logs as JSON, exercises the
// factory and one queued turn, stops the service and returns what it logged.
func runFactoryLogScenario(t *testing.T) factoryLogRun {
	t.Helper()
	ctx := context.Background()
	home, err := os.MkdirTemp("", "fal-")
	fixturekit.Must(t, err)
	t.Cleanup(func() { os.RemoveAll(home) })
	opts := fixtureAt(t, home)
	cfg, err := projectstore.LoadConfig(opts.Config)
	fixturekit.Must(t, err)
	root := cfg.Root.String()

	clock := &fixturekit.DemoClock{At: fixturekit.DemoStart}
	owner := trace.Actor{Kind: "owner", ID: "local"}
	repo, err := trace.Create(ctx, cfg.Root, cfg.Project, clock.Now(), owner)
	fixturekit.Must(t, err)
	fixturekit.Must(t, repo.CreateWorkstream(ctx, fixturekit.Stream, clock.Now(), owner))
	identity := trace.Agent{Header: trace.Header{Schema: "osmia.trace.agent", Version: 1, Revision: 1, ID: fixturekit.DemoAgent, Project: fixturekit.Project, Workstream: fixturekit.Stream, At: clock.Now(), Actor: owner, Cause: "workstream_created"}, Role: fixturekit.DemoRole, ThreadID: fixturekit.DemoThread}
	fixturekit.Must(t, repo.CreateThread(ctx, identity))
	fixturekit.Must(t, repo.Close())

	reply := adaptertest.Reply[coreadapter.SessionResult]{Value: coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "fake", ID: "session"}, FinalResponse: "Answer"}}
	turns := &adaptertest.Turns{Script: *adaptertest.NewScript[coreadapter.PreparedTurn](reply)}
	lives := make(chan *trace.Repository, 1)
	opts.Threads = func(r *trace.Repository, _ *config.Config) (coreadapter.Reconciler, error) {
		lives <- r
		return thread.Dispatcher{Runner: thread.Runner{Jev: fixturekit.UnconfiguredJudge(nil), Store: r, Turns: turns, Now: clock.Now},
			Prepare: func(_ context.Context, in thread.TurnInput) (coreadapter.PreparedTurn, error) {
				return coreadapter.PreparedTurn{SessionDirectory: filepath.Join(root, "sessions", in.Agent, in.Turn)}, nil
			}}, nil
	}
	ticks := make(chan time.Time)
	opts.Reconciliation.Now, opts.Reconciliation.Ticks = clock.Now, ticks
	var passed atomic.Int64
	opts.schedulePassed = func() { passed.Add(1) }
	var buf fixturekit.SyncBuffer
	opts.Logger = slog.New(slog.NewJSONHandler(&buf, nil))

	s, c := start(t, opts)
	repo = <-lives
	tick := func() {
		t.Helper()
		select {
		case ticks <- clock.Now():
		case <-time.After(fixturekit.DemoTimeout):
			t.Fatal("reconciliation pass did not finish")
		}
	}
	awaitPasses := func(n int64) {
		t.Helper()
		base := passed.Load()
		deadline := time.Now().Add(fixturekit.DemoTimeout)
		for passed.Load()-base < n {
			if time.Now().After(deadline) {
				t.Fatalf("only %d of %d further reconciliation passes completed", passed.Load()-base, n)
			}
			tick()
		}
	}

	_, err = c.Health(ctx)
	fixturekit.Must(t, err)

	target := runtime.Target{Scope: "workstream", Project: fixturekit.Project, Workstream: fixturekit.Stream}
	mutation(t, c, "PUT", "pause", PauseRequest{Target: target, Mode: "soft", Reason: "Investigating", Source: "owner"})
	mutation(t, c, "DELETE", "pause", ClearPauseRequest(target))

	_, err = c.Reload(ctx)
	fixturekit.Must(t, err)

	queueTurn(t, repo, "build", clock.Now())
	awaitPasses(2)
	intakefixture.Soon(t, "the queued turn to complete", func() bool {
		th, err := repo.Thread(fixturekit.Stream, fixturekit.DemoAgent)
		fixturekit.Must(t, err)
		return len(th.Turns) == 1 && !th.Turns[0].CompletedAt.IsZero()
	})

	fixturekit.Must(t, s.Close())

	return factoryLogRun{records: decodeFactoryLog(t, buf.String()), version: opts.Build.Version, root: root}
}

func TestFactoryActionLogRecordsTheServiceStartingAndStopping(t *testing.T) {
	t.Parallel()
	run := runFactoryLogScenario(t)
	started := requireFactoryLog(t, run.records, "service started", nil)
	if started["version"] != run.version || started["root"] != run.root {
		t.Fatalf("service started record: %+v", started)
	}
	requireFactoryLog(t, run.records, "project started", map[string]any{"project": string(fixturekit.Project)})
	requireFactoryLog(t, run.records, "project stopped", map[string]any{"project": string(fixturekit.Project), "reason": "service stopped"})
	requireFactoryLog(t, run.records, "service stopped", map[string]any{"reason": "stopped"})
}

func TestFactoryActionLogRecordsEachAPIRequest(t *testing.T) {
	t.Parallel()
	run := runFactoryLogScenario(t)
	requireFactoryLog(t, run.records, "api request handled", map[string]any{"method": "GET", "path": "/v1/health"})
	requireFactoryLog(t, run.records, "api request handled", map[string]any{"method": "POST", "path": "/v1/reload"})
}

func TestFactoryActionLogRecordsPausesSetAndCleared(t *testing.T) {
	t.Parallel()
	run := runFactoryLogScenario(t)
	requireFactoryLog(t, run.records, "pause set", map[string]any{"scope": "workstream", "project": string(fixturekit.Project), "workstream": string(fixturekit.Stream), "mode": "soft"})
	requireFactoryLog(t, run.records, "pause cleared", map[string]any{"scope": "workstream", "project": string(fixturekit.Project), "workstream": string(fixturekit.Stream)})
}

func TestFactoryActionLogRecordsASuccessfulReload(t *testing.T) {
	t.Parallel()
	requireFactoryLog(t, runFactoryLogScenario(t).records, "reload succeeded", nil)
}

func TestFactoryActionLogRecordsAnAgentTurnDispatchedWithItsProfile(t *testing.T) {
	t.Parallel()
	run := runFactoryLogScenario(t)
	dispatched := requireFactoryLog(t, run.records, "agent turn dispatched", map[string]any{"project": string(fixturekit.Project), "workstream": string(fixturekit.Stream), "role": fixturekit.DemoRole})
	if dispatched["profile"] != "default" {
		t.Fatalf("agent turn dispatched record: %+v", dispatched)
	}
}

func TestFactoryActionLogRecordsAnAgentTurnFinishedWithItsOutcomeAndDuration(t *testing.T) {
	t.Parallel()
	run := runFactoryLogScenario(t)
	finished := requireFactoryLog(t, run.records, "agent turn finished", map[string]any{"project": string(fixturekit.Project), "workstream": string(fixturekit.Stream), "role": fixturekit.DemoRole})
	if finished["outcome"] == nil || finished["duration"] == nil {
		t.Fatalf("agent turn finished record: %+v", finished)
	}
}

func TestFactoryActionLogDoesNotChangeOutcomeWhenTheWriterFails(t *testing.T) {
	t.Parallel()
	opts := fixture(t)
	opts.Logger = slog.New(slog.NewJSONHandler(failingWriter{}, nil))
	_, c := start(t, opts)
	target := runtime.Target{Scope: "factory"}
	mutation(t, c, "PUT", "pause", PauseRequest{Target: target, Mode: "soft", Reason: "Investigating", Source: "owner"})
	health, err := c.Health(context.Background())
	fixturekit.Must(t, err)
	if !health.Ready {
		t.Fatal(health)
	}
	mutation(t, c, "DELETE", "pause", ClearPauseRequest(target))
}

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

func TestFactoryActionLogRecordsNotificationAttemptWithoutTheWebhookURL(t *testing.T) {
	t.Parallel()
	hook := newFakeWebhook(t)
	opts, inbox := notifyFixture(t, "")
	hook.records(opts)
	setWebhook(t, opts, hook.hook())
	var buf fixturekit.SyncBuffer
	opts.Logger = slog.New(slog.NewJSONHandler(&buf, nil))
	s, _ := startNotifying(t, opts, inbox)
	intakefixture.Soon(t, "the ledger enabled", func() bool { return readLedger(t, opts).Enabled })
	inbox.set(fixturekit.NotificationEntry(InboxEscalation, 1))
	hook.await(t, 1)
	intakefixture.Soon(t, "the notification recorded as sent", func() bool {
		rec := readLedger(t, opts).Notifications[notificationKey(fixturekit.Project, fixturekit.NotificationEntry(InboxEscalation, 1))]
		return rec != nil && rec.State == notificationSent
	})
	fixturekit.Must(t, s.Close())

	text := buf.String()
	if strings.Contains(text, hook.hook()) || strings.Contains(text, hook.URL) {
		t.Fatalf("log output carries the webhook URL: %s", text)
	}
	records := decodeFactoryLog(t, buf.String())
	requireFactoryLog(t, records, "notification post attempt succeeded", map[string]any{"kind": InboxEscalation, "workstream": string(fixturekit.Stream)})
}

type failingWriter struct{}

func (failingWriter) Write(p []byte) (int, error) {
	return 0, errInjectedWriteFailure
}

var errInjectedWriteFailure = errors.New("injected log writer failure")
