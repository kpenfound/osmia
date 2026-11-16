package service

import (
	"context"
	"fmt"

	"github.com/kpenfound/osmia/internal/coreadapter"
)

const (
	// retiredExtractAction and retiredRefreshAction name the runner operations
	// of the librarian role: a knowledge-base extraction, and a refresh from
	// landings or from a code-based answer. An older trace can hold them
	// pending.
	retiredExtractAction = "kb-extract"
	retiredRefreshAction = "kb-refresh"
)

// retiredAction reports whether action is a librarian operation.
func retiredAction(action string) bool {
	return action == retiredExtractAction || action == retiredRefreshAction
}

// retiredOperations closes the librarian operations a trace holds pending. No effect runs: each operation ends with the terminal result
// "retired", which the operation log records once, so a restart never retries
// it or records a second result.
type retiredOperations struct{}

var _ coreadapter.Reconciler = retiredOperations{}

func (retiredOperations) check(op coreadapter.Operation) error {
	if op.Boundary != coreadapter.RunnerBoundary || !retiredAction(op.Action) {
		return fmt.Errorf("unsupported runner operation %q", op.Action)
	}
	return nil
}

func (r retiredOperations) Inspect(_ context.Context, op coreadapter.Operation) (coreadapter.Observation, error) {
	if err := r.check(op); err != nil {
		return coreadapter.Observation{}, err
	}
	return coreadapter.Observation{State: coreadapter.EffectAbsent, Evidence: "the librarian role is retired; this operation has no effect to find"}, nil
}

func (r retiredOperations) Apply(_ context.Context, op coreadapter.Operation) (coreadapter.OperationResult, error) {
	if err := r.check(op); err != nil {
		return coreadapter.OperationResult{}, err
	}
	return coreadapter.OperationResult{Outcome: "retired", Evidence: "the librarian role is retired; this " + op.Action + " operation was closed without running"}, nil
}
