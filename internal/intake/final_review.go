package intake

import (
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

// AssembledState is the feature state of a workstream whose planned units
// have all merged.
const AssembledState = "assembled"

// MaxFinalRounds is how many final reviews of one workstream may find
// blocking problems before further revisions of its assembled plan are the
// owner's decision rather than the manager's.
const MaxFinalRounds = 3

// NextRevision returns the revision the next record of a workstream
// document takes.
func NextRevision(repository *trace.Repository, stream config.WorkstreamID, id string) (int, error) {
	docs, err := trace.Read[trace.Document](repository, stream)
	if err != nil {
		return 0, err
	}
	revision := 1
	for _, d := range docs {
		if d.ID == id && d.Revision >= revision {
			revision = d.Revision + 1
		}
	}
	return revision, nil
}
