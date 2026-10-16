package trace

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/kpenfound/osmia/internal/config"
)

// ProjectAmendment is a request against a workgroup's project spec: raised
// when a contract section, read-only inside any one member workstream, must
// change. It is never applied inside the workstream that raised it; the
// committee debates it as a delta against the ratified project spec, and
// the owner rules on it at the project level.
type ProjectAmendment struct {
	ID         string              `json:"id"`
	Workgroup  config.WorkgroupID  `json:"workgroup"`
	Origin     config.WorkstreamID `json:"origin"`
	Role       string              `json:"role"`
	Requester  Actor               `json:"requester"`
	QuestionID string              `json:"question_id,omitempty"`
	Citations  []string            `json:"citations"`
	Change     string              `json:"change"`
	Reason     string              `json:"reason"`
	At         time.Time           `json:"at"`
}

// ProjectAmendmentsPath is the project document holding every workgroup's
// project-level amendment requests, and ProjectAmendmentsDocument its record
// ID.
const (
	ProjectAmendmentsPath     = "project-amendments.json"
	ProjectAmendmentsDocument = "project-amendments"
)

type projectAmendmentsFile struct {
	Version    int                `json:"version"`
	Amendments []ProjectAmendment `json:"amendments"`
}

// ProjectAmendments returns every project-level amendment recorded on the
// project, in recorded order. r must be the project's own repository.
func ProjectAmendments(r *Repository) ([]ProjectAmendment, int, error) {
	content, revision, err := latestDocument(r, ProjectAmendmentsPath)
	if err != nil {
		return nil, 0, err
	}
	if content == "" {
		return nil, 0, nil
	}
	var file projectAmendmentsFile
	if err := json.Unmarshal([]byte(content), &file); err != nil {
		return nil, revision, err
	}
	return file.Amendments, revision, nil
}

// FileProjectAmendment records one project-level amendment request in one
// commit, numbered from 1 within its workgroup. A request already filed by
// the same origin workstream for the same question (or, with no question,
// the same citations and change) is returned as-is rather than filed again,
// so a retried routing files nothing twice. r must be the project's own
// repository.
func FileProjectAmendment(ctx context.Context, r *Repository, pa ProjectAmendment, at time.Time) (ProjectAmendment, error) {
	if pa.Workgroup == "" || pa.Origin == "" || len(pa.Citations) == 0 || !present(pa.Change) || !present(pa.Reason) {
		return ProjectAmendment{}, refused("a project amendment requires its workgroup, origin workstream, citations, change and reason")
	}
	existing, revision, err := ProjectAmendments(r)
	if err != nil {
		return ProjectAmendment{}, err
	}
	for _, e := range existing {
		if e.Workgroup != pa.Workgroup || e.Origin != pa.Origin {
			continue
		}
		if pa.QuestionID != "" && e.QuestionID == pa.QuestionID {
			return e, nil
		}
		if pa.QuestionID == "" && e.QuestionID == "" && e.Change == pa.Change && slices.Equal(e.Citations, pa.Citations) {
			return e, nil
		}
	}
	n := 1
	for _, e := range existing {
		if e.Workgroup != pa.Workgroup {
			continue
		}
		if i, err := strconv.Atoi(e.ID); err == nil && i >= n {
			n = i + 1
		}
	}
	pa.ID = strconv.Itoa(n)
	pa.At = at
	next := append(append([]ProjectAmendment{}, existing...), pa)
	data, err := json.Marshal(projectAmendmentsFile{Version: 1, Amendments: next})
	if err != nil {
		return ProjectAmendment{}, err
	}
	if err := r.Append(ctx, Document{
		Header:  Header{Schema: "osmia.trace.document", Version: Version, ID: ProjectAmendmentsDocument, Revision: revision + 1, Project: r.Project(), At: at, Actor: pa.Requester, Cause: fmt.Sprintf("project-amendment-%s-%s", pa.Workgroup, pa.ID)},
		Path:    ProjectAmendmentsPath,
		Content: string(data),
	}); err != nil {
		return ProjectAmendment{}, err
	}
	return pa, nil
}
