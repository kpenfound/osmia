package fixturekit

import (
	"context"
	"errors"

	"github.com/kpenfound/osmia/internal/coreadapter"
)

type CompletedRunner struct{ Inspections int }

func (c *CompletedRunner) Inspect(context.Context, coreadapter.Operation) (coreadapter.Observation, error) {
	c.Inspections++
	return coreadapter.Observation{State: coreadapter.EffectCompleted, Evidence: "threads", Result: &coreadapter.OperationResult{Outcome: "from-threads", Evidence: "threads"}}, nil
}

func (c *CompletedRunner) Apply(context.Context, coreadapter.Operation) (coreadapter.OperationResult, error) {
	return coreadapter.OperationResult{}, errors.New("completed operations are not applied")
}
