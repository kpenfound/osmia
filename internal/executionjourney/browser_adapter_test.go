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
