package executionjourney

import (
	"testing"

	"github.com/kpenfound/osmia/internal/browsertest"
)

type page struct{ *browsertest.Page }

var quote = browsertest.Quote

func assertFeedAnnotations(t *testing.T, p *page, selector string, n int) {
	t.Helper()
	browsertest.AssertFeedAnnotations(t, p.Page, selector, n)
}

func openBrowser(t *testing.T) *page { t.Helper(); return &page{browsertest.OpenBrowser(t)} }

var newLinkProxy = browsertest.NewLinkProxy
