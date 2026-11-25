package intake

import (
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

const (
	// AbandonedState is the feature state of an abandoned workstream.
	AbandonedState = "abandoned"
	// DeliveredState is the feature state of a delivered workstream.
	DeliveredState = "delivered"
	// ConcludedState is the terminal feature state of a Hill Climb workstream
	// that kept no attempt and whose owner declined the pull request that holds
	// only the evaluator: concluded, no improvement. It is not an abandonment,
	// and it archives like a delivered or abandoned workstream does.
	ConcludedState = "concluded"
	// AbandonTransition identifies the one transition to abandoned.
	AbandonTransition = "abandon"
	// CancelReason is recorded on every turn abandonment cancels.
	CancelReason = "the owner abandoned the workstream"
)

// AbandonActor is the provenance of the turn results abandonment records.
var AbandonActor = trace.Actor{Kind: "service", ID: "abandon"}

// Abandoned reports whether the workstream's feature state is abandoned.
func Abandoned(repository *trace.Repository, stream config.WorkstreamID) (bool, error) {
	state, err := repository.Workflow(stream, trace.FeatureSubject)
	return state.Value == AbandonedState, err
}

// Finished reports whether a feature state is terminal: delivered, abandoned
// or concluded with no improvement.
func Finished(state string) bool {
	return state == DeliveredState || state == AbandonedState || state == ConcludedState
}
