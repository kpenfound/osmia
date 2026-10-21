package intakejourney

import (
	"github.com/kpenfound/osmia/internal/browsertest"
	"github.com/kpenfound/osmia/internal/coreadapter"

	"testing"

	"github.com/kpenfound/osmia/internal/servicetest"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
)

var openBrowser = browsertest.OpenBrowser
var quote = browsertest.Quote
var newLinkProxy = browsertest.NewLinkProxy

var decisionCard = intakefixture.DecisionCard
var entriesOf = intakefixture.EntriesOf

type pageFixture struct {
	s     *Service
	c     *Client
	chief coreadapter.Scope
}

func newPageFixture(t *testing.T) *pageFixture {
	t.Helper()
	f := servicetest.NewPageFixture(t)
	return &pageFixture{s: &Service{Service: f.S}, c: &Client{Client: f.C, defaultTimeout: f.C.TimeoutForTest()}, chief: f.Chief}
}
