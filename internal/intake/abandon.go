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
	// abandonTransition identifies the one transition to abandoned.
	AbandonTransition = "abandon"
	// cancelReason is recorded on every turn abandonment cancels.
	CancelReason = "the owner abandoned the workstream"
)

// abandonActor is the provenance of the turn results abandonment records.
var AbandonActor = trace.Actor{Kind: "service", ID: "abandon"}

// abandoned reports whether the workstream's feature state is abandoned.
func Abandoned(repository *trace.Repository, stream config.WorkstreamID) (bool, error) {
	state, err := repository.Workflow(stream, trace.FeatureSubject)
	return state.Value == AbandonedState, err
}
