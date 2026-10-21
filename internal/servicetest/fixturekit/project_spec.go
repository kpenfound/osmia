package fixturekit

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

type corruptedProjectSpecsFile struct {
	Version int                         `json:"version"`
	Specs   []corruptedProjectSpecEntry `json:"specs"`
}
type corruptedProjectSpecEntry struct {
	Workgroup config.WorkgroupID `json:"workgroup"`
	Spec      trace.ProjectSpec  `json:"spec"`
}

func CorruptProjectSpec(t *testing.T, wt *trace.Repository, workgroup config.WorkgroupID, spec trace.ProjectSpec, owner trace.Actor) {
	t.Helper()
	documents, err := trace.Read[trace.Document](wt, "")
	Must(t, err)
	revision := 0
	for _, d := range documents {
		if d.Path == trace.ProjectSpecsPath && d.Revision > revision {
			revision = d.Revision
		}
	}
	data, err := json.Marshal(corruptedProjectSpecsFile{Version: 1, Specs: []corruptedProjectSpecEntry{{Workgroup: workgroup, Spec: spec}}})
	Must(t, err)
	Must(t, wt.Append(context.Background(), trace.Document{
		Header:  trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: trace.ProjectSpecsDocument, Revision: revision + 1, Project: wt.Project(), At: time.Now().UTC(), Actor: owner, Cause: "corrupt-graph"},
		Path:    trace.ProjectSpecsPath,
		Content: string(data),
	}))
}
