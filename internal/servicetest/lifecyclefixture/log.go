// Package lifecyclefixture holds test support that reads lifecycle state.
package lifecyclefixture

import (
	"context"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
)

// LogUnsettled logs every unsettled operation of the stream and the turn it
// waits on. A delta turn is resolved through load when load is not nil.
func LogUnsettled(t *testing.T, repo *trace.Repository, stream config.WorkstreamID, load func(context.Context, lifecycle.DeltaTurnInput) (*trace.Repository, lifecycle.WorkgroupChange, bool, func(), error)) {
	t.Helper()
	ops, err := repo.Operations(stream)
	if err != nil {
		t.Logf("unsettled operations: %v", err)
		return
	}
	printed := 0
	for _, op := range ops {
		if op.Result != nil {
			continue
		}
		printed++
		if printed > 30 {
			t.Log("additional unsettled operations omitted")
			break
		}
		var failure string
		for i := len(op.History) - 1; i >= 0; i-- {
			if op.History[i].Failure != "" {
				failure = op.History[i].Failure
				break
			}
		}
		t.Logf("unsettled operation member=%s id=%s action=%s claim=%+v retry=%v last-failure=%q history=%+v", stream, op.Operation.ID, op.Operation.Action, op.Claim, op.RetryAt, failure, op.History[max(0, len(op.History)-3):])
		in, err := thread.DecodeTurn(op.Operation)
		var th trace.Thread
		agent, turn := in.Agent, in.Turn
		if err == nil {
			th, err = repo.Thread(stream, agent)
		} else if op.Operation.Action == lifecycle.DeltaTurnAction && load != nil {
			agent, turn, th, err = deltaThread(op.Operation, load)
		}

		if err != nil {
			t.Logf("operation turn unavailable: %v; input=%s", err, op.Operation.Input)
			continue
		}
		t.Logf("operation thread=%s turn=%s recorded-turns=%d", agent, turn, len(th.Turns))
		for _, q := range th.Turns {
			if q.Request.TurnID == turn {
				t.Logf("operation thread=%s turn=%s outcome=%s claim=%+v response=%+v completed=%v", agent, turn, q.Status(), q.Claim, q.Response, q.CompletedAt)
			}
		}
	}
}

func deltaThread(op coreadapter.Operation, load func(context.Context, lifecycle.DeltaTurnInput) (*trace.Repository, lifecycle.WorkgroupChange, bool, func(), error)) (string, string, trace.Thread, error) {
	delta, err := lifecycle.DecodeDeltaTurn(op)
	if err != nil {
		return "", "", trace.Thread{}, err
	}
	wt, c, _, closeRepository, err := load(context.Background(), delta)
	if err != nil {
		return "", "", trace.Thread{}, err
	}
	defer closeRepository()
	agent, turn := delta.Agent, lifecycle.DeltaTurnID(c, delta.Agent, delta.Tool)
	th, err := wt.Thread(c.Host, agent)
	return agent, turn, th, err
}
