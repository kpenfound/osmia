package fixturekit

import (
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
)

func LogUnsettledLifecycleOperations(t *testing.T, repo *trace.Repository, stream config.WorkstreamID, deltaAction string, lookup func(coreadapter.Operation) (string, string, trace.Thread, error)) {
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
		t.Logf("unsettled operation id=%s action=%s claim=%+v retry=%v history=%+v", op.Operation.ID, op.Operation.Action, op.Claim, op.RetryAt, op.History[max(0, len(op.History)-3):])
		in, err := thread.DecodeTurn(op.Operation)
		var th trace.Thread
		agent, turn := in.Agent, in.Turn
		if err == nil {
			th, err = repo.Thread(stream, agent)
		} else if op.Operation.Action == deltaAction && lookup != nil {
			agent, turn, th, err = lookup(op.Operation)
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
