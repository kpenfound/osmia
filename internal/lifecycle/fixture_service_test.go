package lifecycle_test

import (
	"context"
	"os"
	"testing"

	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
)

func fixture(t *testing.T) Options {
	t.Helper()
	home, err := os.MkdirTemp("", "os-")
	intakefixture.Must(t, err)
	t.Cleanup(func() { os.RemoveAll(home) })
	return fixtureAt(t, home)
}

func fixtureAt(t *testing.T, home string) Options {
	t.Helper()
	return testOptions(fixturekit.ConfigAt(t, home))
}
func start(t *testing.T, opts Options) (*Service, *Client) {
	t.Helper()
	s, err := Start(context.Background(), opts)
	intakefixture.Must(t, err)
	c := NewClient(s.Socket())
	t.Cleanup(func() {
		c.Close()
		if err := s.Close(); err != nil {
			t.Logf("service close: %v", err)
		}
	})
	return s, c
}
