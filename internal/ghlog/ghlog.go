// Package ghlog records the service's structured log of every GitHub touch
// point: REST API requests and git remote operations against a project's
// upstream or fork. A logger and the project, workstream and unit a request
// is made for travel on the context, so the GitHub REST clients and the
// workspace Provider need no logging parameters of their own.
package ghlog

import (
	"context"
	"io"
	"log/slog"
	"time"

	"github.com/kpenfound/osmia/internal/redact"
)

type loggerKey struct{}
type scopeKey struct{}

// discard is the logger used when no logger reaches the context: logging
// stays silent rather than writing to a stream nobody configured.
var discard = slog.New(slog.NewJSONHandler(io.Discard, nil))

// WithLogger attaches logger to ctx. Every record ctx produces, service
// messages and GitHub activity alike, goes through it.
func WithLogger(ctx context.Context, logger *slog.Logger) context.Context {
	return context.WithValue(ctx, loggerKey{}, logger)
}

// Logger returns the logger ctx carries, or a silent logger when it carries
// none.
func Logger(ctx context.Context) *slog.Logger {
	if logger, ok := ctx.Value(loggerKey{}).(*slog.Logger); ok && logger != nil {
		return logger
	}
	return discard
}

// Scope names the project, workstream and unit a GitHub request is made
// for. Any field may be empty when the service does not know it.
type Scope struct {
	Project, Workstream, Unit string
}

// WithScope attaches scope to ctx, for every GitHub request made while
// handling it.
func WithScope(ctx context.Context, scope Scope) context.Context {
	return context.WithValue(ctx, scopeKey{}, scope)
}

// ScopeOf returns the scope ctx carries, zero valued when it carries none.
func ScopeOf(ctx context.Context) Scope {
	scope, _ := ctx.Value(scopeKey{}).(Scope)
	return scope
}

// Marker is the field every GitHub activity record carries, and no other
// record carries.
const Marker = "activity"

// MarkerValue is Marker's value on a GitHub activity record.
const MarkerValue = "github"

// Access values say whether a GitHub activity record reads from or writes
// to GitHub.
const (
	Read  = "read"
	Write = "write"
)

// Entry is one completed GitHub touch point: a REST API request or a git
// remote operation against a project's upstream or fork.
type Entry struct {
	// Action names what was done, such as "pulls.find" or "workspace.push".
	Action string
	// Access is Read or Write.
	Access string
	// Repository is the target repository, owner/name.
	Repository string
	// Method and Path describe a REST API request; both are empty for a git
	// remote operation.
	Method, Path string
	// Status is the REST API response status, 0 when no response arrived.
	Status int
	// NoResponse marks a REST API request that got no response, a transport
	// failure rather than an unexpected status.
	NoResponse bool
	// Remote and Branch describe a git remote operation; both are empty for
	// a REST API request.
	Remote, Branch string
	// Commits are the commits a git remote operation involved, such as the
	// pushed commit and the expected remote commit.
	Commits []string
	// Outcome names what happened, such as "ok", "rejected" or "failed".
	// Empty defaults to "ok" on success and "failed" on a non-nil Err.
	Outcome string
	// Err is the failure, if the touch point failed.
	Err error
}

// Log records one completed GitHub touch point: e's fields, the scope and
// logger ctx carries, the marker, and how long it took since start. A
// failure, including an unexpected REST status, logs at Error; a success
// logs at Info.
func Log(ctx context.Context, start time.Time, e Entry) {
	logger := Logger(ctx)
	level := slog.LevelInfo
	unexpected := e.Status != 0 && (e.Status < 200 || e.Status >= 300)
	if e.Err != nil || unexpected {
		level = slog.LevelError
	}
	if !logger.Enabled(ctx, level) {
		return
	}
	attrs := []slog.Attr{
		slog.String(Marker, MarkerValue),
		slog.String("action", e.Action),
		slog.String("access", e.Access),
		slog.Duration("duration", time.Since(start)),
	}
	if e.Repository != "" {
		attrs = append(attrs, slog.String("repository", e.Repository))
	}
	if e.Method != "" {
		attrs = append(attrs, slog.String("method", e.Method))
	}
	if e.Path != "" {
		attrs = append(attrs, slog.String("path", e.Path))
	}
	if e.Status != 0 {
		attrs = append(attrs, slog.Int("status", e.Status))
	}
	if e.NoResponse {
		attrs = append(attrs, slog.Bool("no_response", true))
	}
	if e.Remote != "" {
		attrs = append(attrs, slog.String("remote", e.Remote))
	}
	if e.Branch != "" {
		attrs = append(attrs, slog.String("branch", e.Branch))
	}
	if len(e.Commits) > 0 {
		values := make([]any, len(e.Commits))
		for i, c := range e.Commits {
			values[i] = c
		}
		attrs = append(attrs, slog.Any("commits", values))
	}
	outcome := e.Outcome
	if outcome == "" {
		outcome = "ok"
		if e.Err != nil {
			outcome = "failed"
		}
	}
	attrs = append(attrs, slog.String("outcome", outcome))
	if scope := ScopeOf(ctx); scope != (Scope{}) {
		if scope.Project != "" {
			attrs = append(attrs, slog.String("project", scope.Project))
		}
		if scope.Workstream != "" {
			attrs = append(attrs, slog.String("workstream", scope.Workstream))
		}
		if scope.Unit != "" {
			attrs = append(attrs, slog.String("unit", scope.Unit))
		}
	}
	msg := "github request completed"
	if e.Err != nil {
		attrs = append(attrs, slog.String("error", redact.Secrets(e.Err.Error(), redact.KnownSecrets()...)))
		msg = "github request failed"
	} else if unexpected {
		msg = "github request answered unexpectedly"
	}
	logger.LogAttrs(ctx, level, msg, attrs...)
}
