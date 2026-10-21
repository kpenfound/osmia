package service

import (
	sync "sync"

	testing "testing"

	runtime "github.com/kpenfound/osmia/internal/runtime"
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
)

type demoProblems struct {
	mu   sync.Mutex
	list []string
}

func (p *demoProblems) report(format string, args ...any) {
	intakefixture.ReportProblem(&p.mu, &p.list, format, args...)
}

func (p *demoProblems) check(t *testing.T) {
	t.Helper()
	intakefixture.CheckProblems(t, &p.mu, &p.list)
}

var eventually = intakefixture.Eventually

func factoryPauseOf(t *testing.T, c *Client) (runtime.Pause, bool) {
	t.Helper()
	return intakefixture.FactoryPauseOf(t, c.Runtime)
}
