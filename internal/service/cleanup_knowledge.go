package service

import (
	"encoding/json"
	"slices"
	"strings"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

// learningsIncorporated reports whether the librarian refresh covering a
// stream's landed units and code-cited knowledge gaps has completed.
func learningsIncorporated(repo *trace.Repository, stream config.WorkstreamID) (bool, error) {
	librarian := librarianWorkstream(repo.Project())
	streams, err := repo.Workstreams()
	if err != nil {
		return false, err
	}
	if !slices.Contains(streams, librarian) {
		return true, nil
	}
	ops, err := repo.Operations(librarian)
	if err != nil {
		return false, err
	}
	known := map[string]bool{}
	for _, op := range ops {
		if op.Operation.Action != RefreshAction {
			continue
		}
		var in refreshInput
		if err := json.Unmarshal(op.Operation.Input, &in); err != nil {
			return false, err
		}
		if op.Result == nil {
			if in.Workstream == stream {
				return false, nil
			}
			continue
		}
		for _, l := range in.landings() {
			known[l.Commit] = true
		}
		known[in.key()] = true
	}
	r := &refresher{extractor: &extractor{repository: repo}}
	landings, err := r.unfolded(stream, known)
	if err != nil {
		return false, err
	}
	if len(landings) != 0 {
		return false, nil
	}
	rulings, err := trace.Read[trace.Ruling](repo, stream)
	if err != nil {
		return false, err
	}
	for _, ruling := range rulings {
		if ruling.Decision != trace.DecisionAnswer {
			continue
		}
		for _, citation := range ruling.Citations {
			inspectionID, ok := strings.CutPrefix(citation, "inspection#")
			if !ok {
				continue
			}
			inspection, err := repo.CodeInspection(stream, inspectionID)
			if err != nil {
				return false, err
			}
			in := refreshInput{Workstream: stream, Question: ruling.QuestionID, Inspection: inspectionID, Commit: inspection.Commit}
			if !known[in.key()] {
				return false, nil
			}
		}
	}
	return true, nil
}
