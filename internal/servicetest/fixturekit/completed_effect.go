package fixturekit

import (
	"context"

	"github.com/kpenfound/osmia/internal/coreadapter"
)

type CompletedEffect struct{}

func (CompletedEffect) Inspect(context.Context, coreadapter.Operation) (coreadapter.Observation, error) {
	return coreadapter.Observation{State: coreadapter.EffectCompleted, Evidence: "completed", Result: &coreadapter.OperationResult{Outcome: "succeeded", Evidence: "completed"}}, nil
}

func (CompletedEffect) Apply(context.Context, coreadapter.Operation) (coreadapter.OperationResult, error) {
	return coreadapter.OperationResult{Outcome: "succeeded", Evidence: "completed"}, nil
}
