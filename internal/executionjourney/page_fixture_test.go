package executionjourney

import (
	"github.com/kpenfound/osmia/internal/servicetest"
	"testing"
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
