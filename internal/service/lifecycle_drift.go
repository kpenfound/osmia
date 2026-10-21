package service

import (
	"context"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
)

func (f *foreman) RequestDrifts(ctx context.Context, due func(config.WorkstreamID) (string, error)) ([]config.WorkstreamID, error) {
	return f.driftDriver().RequestDrifts(ctx, due)
}
func (d drifter) Inspect(ctx context.Context, op coreadapter.Operation) (coreadapter.Observation, error) {
	return d.driftDriver().Inspect(ctx, op)
}
func (d drifter) Apply(ctx context.Context, op coreadapter.Operation) (coreadapter.OperationResult, error) {
	return d.driftDriver().Apply(ctx, op)
}
func (f *foreman) Drifts(ctx context.Context) error {
	driver := f.driftDriver()
	err := driver.Drifts(ctx)
	f.nextDrift = driver.NextDrift
	return err
}

func (d drifter) ensureThread(ctx context.Context, stream config.WorkstreamID, agent, role string) error {
	return d.driftDriver().EnsureThread(ctx, stream, agent, role)
}
