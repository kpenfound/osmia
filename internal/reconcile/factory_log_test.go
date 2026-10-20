package reconcile

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/ghlog"
)

// decodeLog parses text as one JSON object per line, failing the test if
// any line is not a JSON object.
func decodeLog(t *testing.T, text string) []map[string]any {
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
		records = append(records, record)
	}
	return records
}

// findLog returns the first record whose message is msg, or nil.
func findLog(records []map[string]any, msg string) map[string]any {
	for _, r := range records {
		if r["msg"] == msg {
			return r
		}
	}
	return nil
}

// TestControllerLogsOperationStartAndOutcome shows that the controller logs
// an operation's start and its outcome, named by its action and boundary
// and carrying the project and workstream it belongs to, through the
// logger its context carries - the same context-carried logger the
// service's own records use.
func TestControllerLogsOperationStartAndOutcome(t *testing.T) {
	f := setup(t, coreadapter.RepositoryBoundary)
	var buf bytes.Buffer
	ctx := ghlog.WithLogger(context.Background(), slog.New(slog.NewJSONHandler(&buf, nil)))
	must(t, f.controller(t).Pass(ctx))
	f.completed(t)
	records := decodeLog(t, buf.String())
	started := findLog(records, "operation started")
	if started == nil || started["project"] != string(projectID) || started["workstream"] != string(streamID) ||
		started["action"] != "prepare" || started["boundary"] != string(coreadapter.RepositoryBoundary) || started["level"] != "INFO" {
		t.Fatalf("operation started record: %+v (all: %+v)", started, records)
	}
	finished := findLog(records, "operation finished")
	if finished == nil || finished["project"] != string(projectID) || finished["workstream"] != string(streamID) ||
		finished["outcome"] != "complete" || finished["duration"] == nil || finished["level"] != "INFO" {
		t.Fatalf("operation finished record: %+v (all: %+v)", finished, records)
	}
}

// TestControllerLogsFailedOperationAtErrorLevel shows that a failed
// operation's effect is logged as a failure, at a higher level than a
// routine, successful one.
func TestControllerLogsFailedOperationAtErrorLevel(t *testing.T) {
	f := setup(t, coreadapter.RepositoryBoundary)
	f.system.applyErr = errInjected
	var buf bytes.Buffer
	ctx := ghlog.WithLogger(context.Background(), slog.New(slog.NewJSONHandler(&buf, nil)))
	must(t, f.controller(t).Pass(ctx))
	records := decodeLog(t, buf.String())
	started := findLog(records, "operation started")
	if started == nil || started["level"] != "INFO" {
		t.Fatalf("operation started record: %+v (all: %+v)", started, records)
	}
	failed := findLog(records, "operation failed")
	if failed == nil || failed["level"] != "ERROR" || failed["error"] == nil ||
		failed["project"] != string(projectID) || failed["workstream"] != string(streamID) {
		t.Fatalf("operation failed record: %+v (all: %+v)", failed, records)
	}
}

func TestControllerLogsOperationsOverThreshold(t *testing.T) {
	f := setup(t, coreadapter.RepositoryBoundary)
	var buf bytes.Buffer
	ctx := ghlog.WithLogger(context.Background(), slog.New(slog.NewJSONHandler(&buf, nil)))
	c := f.controller(t)
	c.options.SlowThreshold = time.Nanosecond
	must(t, c.Pass(ctx))
	record := findLog(decodeLog(t, buf.String()), "operation exceeded threshold")
	if record == nil || record["project"] != string(projectID) || record["workstream"] != string(streamID) || record["operation"] != f.event.Operation.ID || record["duration"] == nil || record["level"] != "WARN" {
		t.Fatalf("slow operation log: %+v", record)
	}
}

func TestRunRetriesSchedulingFailuresWithoutStoppingAdmittedWork(t *testing.T) {
	for _, atStartup := range []bool{true, false} {
		name := "while-running"
		if atStartup {
			name = "at-startup"
		}
		t.Run(name, func(t *testing.T) {
			f := setup(t, coreadapter.RepositoryBoundary)
			f.publish(t, "session")
			s := newSessions()
			c := f.concurrent(t, s, "session")
			c.options.Adapters[coreadapter.RepositoryBoundary] = f.system
			ticks := make(chan time.Time)
			c.options.Ticks = ticks
			var failing atomic.Bool
			failing.Store(atStartup)
			failed := make(chan struct{}, 1)
			c.options.Schedule = func(context.Context) error {
				if failing.Load() {
					select {
					case failed <- struct{}{}:
					default:
					}
					return errInjected
				}
				return nil
			}
			var buf bytes.Buffer
			ctx, cancel := context.WithCancel(ghlog.WithLogger(context.Background(), slog.New(slog.NewJSONHandler(&buf, nil))))
			done := make(chan error, 1)
			go func() { done <- c.Run(ctx) }()
			defer func() {
				cancel()
				select {
				case err := <-done:
					if !errors.Is(err, context.Canceled) {
						t.Errorf("run: %v", err)
					}
				case <-time.After(overlapTimeout):
					t.Error("run did not join its operations")
				}
			}()
			if !atStartup {
				f.awaitStarted(t, s, "session")
				failing.Store(true)
				f.repository.WakeWorkflow()
			}
			// Repeated failed schedules must leave the loop and its work alive.
			for range 2 {
				await(t, failed)
				select {
				case ticks <- epoch:
				case <-time.After(overlapTimeout):
					t.Fatal("failed schedule stopped waiting for ticks")
				}
			}
			failing.Store(false)
			f.repository.WakeWorkflow()
			if atStartup {
				f.awaitStarted(t, s, "session")
			}
			close(s.release)
			awaitAcknowledged(t, f, streamID, "session")
			cancel()
			// The deferred join runs before the log is inspected.
			t.Cleanup(func() {
				record := findLog(decodeLog(t, buf.String()), "reconciliation schedule failed; will retry")
				if record == nil || record["project"] != string(projectID) || record["error"] != errInjected.Error() {
					t.Errorf("schedule failure log: %+v", record)
				}
			})
		})
	}
}
