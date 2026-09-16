package coreadapter

import (
	"context"
	"errors"
	"testing"

	"github.com/kpenfound/busybees/core/agent"
)

type resumeExecutor struct {
	calls int
	err   error
}

func (*resumeExecutor) Check(context.Context, Isolation, ExecutionSettings) error { return nil }
func (*resumeExecutor) Run(context.Context, agent.Request, ExecutionSettings) (*agent.Result, error) {
	panic("capability check launched work")
}
func (e *resumeExecutor) CheckResume(_ context.Context, old, next Profile, s BackendSession) error {
	e.calls++
	if old.Model != next.Model {
		return ErrResumeUnavailable
	}
	return e.err
}
func TestResumeCapabilityAndMetadata(t *testing.T) {
	p := Profile{Name: "a", Backend: "claude", Model: "model"}
	executor := &resumeExecutor{}
	runner := TurnRunner{Executor: executor}
	s := BackendSession{Backend: "claude", ID: "session"}
	next := p
	next.Name = "b"
	if err := runner.CheckResume(context.Background(), p, next, s); err != nil || executor.calls != 1 {
		t.Fatalf("same-backend profile: %v", err)
	}
	next.Model = "changed"
	if err := runner.CheckResume(context.Background(), p, next, s); !errors.Is(err, ErrResumeUnavailable) {
		t.Fatal(err)
	}
	for _, session := range []BackendSession{{}, {Backend: "claude", ID: " "}, {Backend: "claude", ID: "bad\nstate"}, {Backend: "other", ID: "session"}} {
		calls := executor.calls
		if err := runner.CheckResume(context.Background(), p, p, session); err == nil || executor.calls != calls {
			t.Fatalf("invalid session: %#v %v", session, err)
		}
	}
	for _, backend := range []string{"codex", "unknown"} {
		profile := p
		profile.Backend = backend
		if err := runner.CheckResume(context.Background(), profile, profile, BackendSession{Backend: backend, ID: "id"}); !errors.Is(err, ErrUnsupported) {
			t.Fatal(err)
		}
	}
	executor.err = ErrResumeUnavailable
	if err := runner.CheckResume(context.Background(), p, p, s); !errors.Is(err, ErrResumeUnavailable) {
		t.Fatal(err)
	}
	runner.Executor = CoreExecutor{}
	if err := runner.CheckResume(context.Background(), p, p, s); !errors.Is(err, ErrUnsupported) {
		t.Fatal(err)
	}
}
