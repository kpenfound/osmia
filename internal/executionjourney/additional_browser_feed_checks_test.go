package executionjourney

import (
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"

	"github.com/kpenfound/osmia/internal/browsertest"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
)

func startFeedChecks(t *testing.T, f *shedFixture, checks ReviewChecks, action string) func() {
	t.Helper()
	return intakefixture.StartFeedChecks(t, f.opts.Config.Root, f.clock.Now, action, func(ticks chan time.Time, hold func(config.WorkstreamID, coreadapter.Operation) bool) {
		f.opts.Reconciliation.Ticks = ticks
		f.opts.Reconciliation.Hold = hold
		f.opts.reviewChecks = checks
	}, f.start, f.stop)
}

func openCheckFeed(t *testing.T, f *shedFixture, stream config.WorkstreamID) *page {
	t.Helper()
	return &page{browsertest.OpenCheckFeed(t, f.s.WebAddr(), stream)}
}

func awaitFeedEvent(p *page, ws config.WorkstreamID) { browsertest.AwaitFeedEvent(p.Page, ws) }
