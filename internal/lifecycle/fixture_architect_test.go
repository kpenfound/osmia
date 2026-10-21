package lifecycle_test

import (
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
)

func newArchitectFixture(t *testing.T, configure ...func(*Options)) *architectFixture {
	t.Helper()
	opts, clone, engine, sessions, clock := newArchitectOptions(t)
	for _, apply := range configure {
		apply(&opts)
	}
	f := &architectFixture{opts: opts, clone: clone, engine: engine, sessions: sessions, clock: clock}
	f.start(t)
	f.project, f.trace = intakefixture.AddArchitectProject(t, f.c, clone, "1. Keep changes small.\n2. Every change has a test.\n", func(t *testing.T, p config.ProjectID, text string) { writeCharter(t, f.c, p, text) })
	return f
}
