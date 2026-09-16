package coreadapter

import (
	"context"
	"errors"
	"strings"
)

// ErrResumeUnavailable is returned only before the resumed turn accepts work.
// It authorizes a fresh attempt; arbitrary execution failures do not.
var ErrResumeUnavailable = errors.New("resumable session unavailable")

// ErrNotStarted proves that no work was accepted and no side effects can still
// complete. A service may select an explicitly configured fallback on this error.
var ErrNotStarted = errors.New("turn not started")

// ResumeChecker checks backend capabilities, profile compatibility and opaque
// session availability without reading private transcript content. Nil authorizes
// resume. Unavailable or unsupported state selects owned-log replay; other errors
// stop preparation. Implementations must check model/effort changes explicitly.
type ResumeChecker interface {
	CheckResume(context.Context, Profile, Profile, BackendSession) error
}

func ValidSession(s BackendSession) bool {
	return strings.TrimSpace(s.Backend) != "" && strings.TrimSpace(s.ID) != "" &&
		len(s.ID) <= 4096 && !strings.ContainsAny(s.ID, "\x00\r\n")
}

func (r *TurnRunner) CheckResume(ctx context.Context, previous, next Profile, session BackendSession) error {
	if !ValidSession(session) || previous.Backend != next.Backend || session.Backend != next.Backend {
		return ErrResumeUnavailable
	}
	if next.Backend != "claude" && next.Backend != "opencode" {
		return unsupported("resume", "backend does not support resume")
	}
	checker, ok := r.Executor.(ResumeChecker)
	if !ok {
		return unsupported("resume", "executor cannot verify saved session compatibility and availability")
	}
	return checker.CheckResume(ctx, previous, next, session)
}
