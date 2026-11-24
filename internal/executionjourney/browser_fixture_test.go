package executionjourney

import (
	"github.com/kpenfound/osmia/internal/browsertest"
	"github.com/kpenfound/osmia/internal/checks"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/servicetest"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"testing"
	"time"
)

type page struct{ *browsertest.Page }

func assertFeedAnnotations(t *testing.T, p *page, selector string, n int) {
	t.Helper()
	browsertest.AssertFeedAnnotations(t, p.Page, selector, n)
}

func openBrowser(t *testing.T) *page { t.Helper(); return &page{browsertest.OpenBrowser(t)} }

type pageFixture struct {
	s *Service
	c *Client
}

func newPageFixture(t *testing.T) *pageFixture {
	t.Helper()
	f := servicetest.NewPageFixture(t)
	return &pageFixture{s: &Service{Service: f.S}, c: &Client{Client: f.C}}
}

func startFeedChecks(t *testing.T, f *shedFixture, checker checks.ReviewChecks, action string) func() {
	t.Helper()
	return intakefixture.StartFeedChecks(t, f.opts.Config.Root, f.clock.Now, action, func(ticks chan time.Time, hold func(config.WorkstreamID, coreadapter.Operation) bool) {
		f.opts.Reconciliation.Ticks = ticks
		f.opts.Reconciliation.Hold = hold
		f.opts.reviewChecks = checker
	}, f.start, f.stop)
}

func openCheckFeed(t *testing.T, f *shedFixture, stream config.WorkstreamID) *page {
	t.Helper()
	return &page{browsertest.OpenCheckFeed(t, f.s.WebAddr(), stream)}
}

func awaitFeedEvent(p *page, ws config.WorkstreamID) { browsertest.AwaitFeedEvent(p.Page, ws) }
