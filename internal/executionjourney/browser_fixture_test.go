package executionjourney

import (
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/browsertest"
	"github.com/kpenfound/osmia/internal/checks"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/servicetest/journey"
)

type page struct{ *browsertest.Page }

func assertFeedAnnotations(t *testing.T, p *page, selector string, n int) {
	t.Helper()
	browsertest.AssertFeedAnnotations(t, p.Page, selector, n)
}

func openBrowser(t *testing.T) *page { t.Helper(); return &page{browsertest.OpenBrowser(t)} }

func startFeedChecks(t *testing.T, f *journey.ShedFixture, checker checks.ReviewChecks, action string) func() {
	t.Helper()
	return intakefixture.StartFeedChecks(t, f.Opts.Config.Root, f.Clock.Now, action, func(ticks chan time.Time, hold func(config.WorkstreamID, coreadapter.Operation) bool) {
		f.Opts.Reconciliation.Ticks = ticks
		f.Opts.Reconciliation.Hold = hold
		f.Opts.ReviewChecks = checker
	}, f.Start, f.Stop)
}

func openCheckFeed(t *testing.T, f *journey.ShedFixture, stream config.WorkstreamID) *page {
	t.Helper()
	return &page{browsertest.OpenCheckFeed(t, f.S.WebAddr(), stream)}
}

func awaitFeedEvent(p *page, ws config.WorkstreamID) { browsertest.AwaitFeedEvent(p.Page, ws) }
