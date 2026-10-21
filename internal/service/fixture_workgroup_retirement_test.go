package service

import (
	"context"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

func logUnsettledLifecycleOperations(t *testing.T, repo *trace.Repository, stream config.WorkstreamID, services ...*Service) {
	t.Helper()
	var lookup func(coreadapter.Operation) (string, string, trace.Thread, error)
	if len(services) > 0 {
		lookup = func(op coreadapter.Operation) (string, string, trace.Thread, error) {
			delta, err := decodeDeltaTurn(op)
			if err != nil {
				return "", "", trace.Thread{}, err
			}
			wt, c, _, close, err := (deltaRunner{S: services[0].lifecycle()}).Load(context.Background(), delta)
			if err != nil {
				return "", "", trace.Thread{}, err
			}
			defer close()
			agent, turn := delta.Agent, deltaTurnID(c, delta.Agent, delta.Tool)
			th, err := wt.Thread(c.Host, agent)
			return agent, turn, th, err
		}
	}
	fixturekit.LogUnsettledLifecycleOperations(t, repo, stream, deltaTurnAction, lookup)
}
