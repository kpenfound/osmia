package service

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/errorreport"
	"github.com/kpenfound/osmia/internal/ghlog"
)

func capturingLogger(sources ...string) (*slog.Logger, *errorCapture) {
	c := newErrorCapture(4, nil)
	c.configure(config.ErrorReports{Enabled: true, Sources: sources})
	return slog.New(c.wrap(slog.NewJSONHandler(io.Discard, nil))), c
}

// The capture picks the allowlisted fields of the three kinds of error record
// and nothing else, with the endpoint as a route template.
func TestErrorCaptureQueuesOnlyTheErrorsItReports(t *testing.T) {
	t.Parallel()
	logger, c := capturingLogger(config.ErrorSources...)
	ctx := ghlog.WithLogger(context.Background(), logger)

	logger.Info("api request handled", "listener", "socket", "method", "GET", "path", "/v1/status/w_0123456789abcdef", "status", 200)
	logger.Info("api request handled", "listener", "socket", "method", "GET", "path", "/v1/images/w_0123456789abcdef/abc.png", "status", 500)
	logger.Info("api request handled", "listener", "web", "method", "GET", "path", "/somewhere/else", "status", 503)
	logger.Error("operation failed", "project", "p_0123456789abcdef", "workstream", "w_0123456789abcdef", "action", "workspace.draft", "error", "draft failed")
	logger.Error("operation failed", "action", "thread-turn", "error", "provider overloaded")
	logger.Error("agent turn finished", "outcome", "failed", "error", "provider overloaded")
	ghlog.Log(ctx, time.Now(), ghlog.Entry{Action: "pulls.find", Access: ghlog.Read, Repository: "o/r", Method: "GET", Path: "/repos/o/r/pulls/12", Status: 502, Err: errors.New("bad gateway")})
	ghlog.Log(ctx, time.Now(), ghlog.Entry{Action: "workspace.push", Access: ghlog.Write, Repository: "o/r", Remote: "origin", Branch: "b", Err: errors.New("rejected")})
	ghlog.Log(ghlog.WithReporter(ctx), time.Now(), ghlog.Entry{Action: "issues.create", Access: ghlog.Write, Repository: "o/r", Method: "POST", Path: "/repos/o/r/issues", Status: 500, Err: errors.New("failed")})
	ghlog.Log(ctx, time.Now(), ghlog.Entry{Action: "pulls.find", Access: ghlog.Read, Repository: "o/r", Method: "GET", Path: "/repos/o/r/pulls", Status: 200})

	var got []errorreport.Event
	for {
		q, ok := c.head()
		if !ok {
			break
		}
		got = append(got, q.event)
		c.consume(q)
	}
	if len(got) != 4 {
		t.Fatalf("captured %d errors: %+v", len(got), got)
	}
	want := []struct {
		source   errorreport.Source
		template string
		status   int
	}{
		{errorreport.SourceAPI, "GET /v1/images/{workstream}/{image}", 500},
		{errorreport.SourceAPI, "GET unrouted", 503},
		{errorreport.SourceOperation, "workspace.draft", 0},
		{errorreport.SourceGitHub, "GET /repos/o/r/pulls/12", 502},
	}
	for i, w := range want {
		if got[i].Source != w.source || got[i].Template != w.template || got[i].Status != w.status {
			t.Fatalf("error %d: %+v, want %+v", i, got[i], w)
		}
		if got[i].Attributes != nil {
			t.Fatalf("error %d carries free-form attributes: %+v", i, got[i].Attributes)
		}
	}
	if got[2].ProjectID != "p_0123456789abcdef" || got[2].WorkstreamID != "w_0123456789abcdef" {
		t.Fatalf("operation error IDs: %+v", got[2])
	}
}

// A source that is switched off is not captured, nothing is captured while the
// reporter is disabled, and disabling empties the queue.
func TestErrorCaptureFollowsTheConfiguration(t *testing.T) {
	t.Parallel()
	logger, c := capturingLogger("operation")
	logger.Info("api request handled", "method", "GET", "path", "/v1/health", "status", 503)
	logger.Error("operation failed", "action", "workspace.draft", "error", "x")
	if n := c.queued(); n != 1 {
		t.Fatalf("%d queued with only operation errors on", n)
	}
	c.configure(config.ErrorReports{})
	if n := c.queued(); n != 0 {
		t.Fatalf("%d queued after disabling", n)
	}
	logger.Error("operation failed", "action", "workspace.draft", "error", "x")
	if n := c.queued(); n != 0 {
		t.Fatalf("%d captured while disabled", n)
	}
}

// Attributes a logger carries with With are seen, a full queue drops the oldest
// error and counts it, and a worker holding an error from before the queue was
// emptied cannot consume a newer one.
func TestErrorCaptureBoundsItsQueue(t *testing.T) {
	t.Parallel()
	logger, c := capturingLogger("operation")
	scoped := logger.With("project", "p_0123456789abcdef")
	for _, action := range []string{"a.op", "b.op", "c.op", "d.op", "e.op", "f.op"} {
		scoped.Error("operation failed", "action", action, "error", "x")
	}
	head, ok := c.head()
	if !ok || head.event.Template != "c.op" || head.event.ProjectID != "p_0123456789abcdef" {
		t.Fatalf("head %+v", head)
	}
	if c.queued() != 4 || c.takeDropped() != 2 || c.takeDropped() != 0 {
		t.Fatal("the queue is not bounded and counted")
	}
	c.clear()
	if c.consume(head) {
		t.Fatal("consumed an error from an emptied queue")
	}
	scoped.WithGroup("g").Error("operation failed", "action", "g.op", "error", "x")
	if c.queued() != 0 {
		t.Fatal("captured a record under a group")
	}
}
