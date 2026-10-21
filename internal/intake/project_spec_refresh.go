package intake

import (
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
)

// sliceMatchesPin compares the slice with the exact recorded member revisions
// used by both owner ratification and recovery of member decisions.
func SliceMatchesPin(spec trace.ProjectSpec, repo *trace.Repository, member config.WorkstreamID, pin shed.Pin) (bool, error) {
	slice, covered := trace.SliceFor(spec, member)
	if !covered {
		return false, nil
	}
	specDoc, err := trace.Get[trace.Document](repo, member, plan.SpecDocument, pin.Spec)
	if err != nil {
		return false, err
	}
	planDoc, err := trace.Get[trace.Document](repo, member, plan.PlanDocument, pin.Plan)
	if err != nil {
		return false, err
	}
	return slice.Spec == specDoc.Content && slice.Plan == planDoc.Content, nil
}
