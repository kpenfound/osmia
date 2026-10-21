package fixturekit

import (
	"context"
	"sync"

	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/coreadapter/adaptertest"
)

// blockingTurns delegates to scripted fake agent results, first calling the
// per-turn hook with the turn's context. Its fake, adaptertest.Turns, stands
// in for a real model session; it leaves the real agent backend's actual
// behaviour, timing and output unverified.
type BlockingTurns struct {
	Fake  *adaptertest.Turns
	Mu    sync.Mutex
	Hooks map[string]func(context.Context)
}

func (b *BlockingTurns) Run(ctx context.Context, p coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
	b.Mu.Lock()
	hook := b.Hooks[p.Scope.Turn]
	b.Mu.Unlock()
	if hook != nil {
		hook(ctx)
	}
	return b.Fake.Run(context.WithoutCancel(ctx), p)
}
