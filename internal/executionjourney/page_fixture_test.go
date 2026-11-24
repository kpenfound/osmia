package executionjourney

import (
	"testing"

	"github.com/kpenfound/osmia/internal/servicetest"
)

type pageFixture struct {
	s *Service
	c *Client
}

func newPageFixture(t *testing.T) *pageFixture {
	t.Helper()
	f := servicetest.NewPageFixture(t)
	return &pageFixture{s: &Service{Service: f.S}, c: &Client{Client: f.C}}
}
