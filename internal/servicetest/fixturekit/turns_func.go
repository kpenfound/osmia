package fixturekit

import (
	"context"

	"github.com/kpenfound/osmia/internal/coreadapter"
)

type TurnsFunc func(context.Context, coreadapter.PreparedTurn) (coreadapter.SessionResult, error)

func (f TurnsFunc) Run(ctx context.Context, p coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
	return f(ctx, p)
}
