package fixturekit

import (
	"slices"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

func OperationRecord(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, action string) trace.OperationRecord {
	t.Helper()
	ops, err := repository.Operations(stream)
	Must(t, err)
	ops = slices.DeleteFunc(ops, func(o trace.OperationRecord) bool { return o.Operation.Action != action })
	if len(ops) != 1 {
		t.Fatalf("%s operations: %+v", action, ops)
	}
	return ops[0]
}
