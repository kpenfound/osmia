package climb

import (
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

// RunningDocument returns the next revision of climb/running.json, to be
// recorded in the commit of the phase change it describes.
func RunningDocument(r *trace.Repository, stream config.WorkstreamID, run Running, at time.Time) (trace.Document, error) {
	revision, err := nextRevision(r, stream, trace.ClimbRunningID)
	if err != nil {
		return trace.Document{}, err
	}
	return jsonDocument(r, stream, trace.ClimbRunningID, RunningPath, revision, at, "climb-running", run)
}
