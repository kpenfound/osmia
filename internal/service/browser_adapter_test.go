package service

import (
	"testing"

	"github.com/kpenfound/osmia/internal/browsertest"
	"github.com/kpenfound/osmia/internal/trace"
)

type page struct{ *browsertest.Page }

func openBrowser(t *testing.T) *page { t.Helper(); return &page{browsertest.OpenBrowser(t)} }

var quote = browsertest.Quote
var newLinkProxy = browsertest.NewLinkProxy

const browserTimeout = browsertest.BrowserTimeout

func assertFeedAnnotations(t *testing.T, p *page, selector string, n int) {
	t.Helper()
	browsertest.AssertFeedAnnotations(t, p.Page, selector, n)
}

func sidebarFixture(t *testing.T) (Options, *trace.Repository) {
	t.Helper()
	opts, _, repo := activityFixture(t)
	withWeb(t, opts, "127.0.0.1:0")
	return opts, repo
}
