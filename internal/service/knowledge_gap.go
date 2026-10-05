package service

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/trace"
)

func (in refreshInput) key() string {
	if in.Inspection == "" {
		return refreshKey(in.Commit)
	}
	return fmt.Sprintf("refresh-gap-%x", sha256.Sum256([]byte(string(in.Workstream)+"/"+in.Question+"/"+in.Inspection)))
}

func (r *refresher) codeSource(in refreshInput) (trace.Ruling, trace.CodeInspection, error) {
	inspection, err := r.repository.CodeInspection(in.Workstream, in.Inspection)
	if err != nil {
		return trace.Ruling{}, inspection, err
	}
	if inspection.Commit != in.Commit {
		return trace.Ruling{}, inspection, fmt.Errorf("knowledge gap inspection commit differs")
	}
	rulings, err := trace.Read[trace.Ruling](r.repository, in.Workstream)
	if err != nil {
		return trace.Ruling{}, inspection, err
	}
	for _, ruling := range rulings {
		if ruling.QuestionID == in.Question && ruling.Decision == trace.DecisionAnswer && slices.Contains(ruling.Citations, "inspection#"+in.Inspection) {
			return ruling, inspection, nil
		}
	}
	return trace.Ruling{}, inspection, fmt.Errorf("no answer cites this code inspection")
}

func (r *refresher) checkSource(in refreshInput) error {
	if in.Inspection != "" {
		_, _, err := r.codeSource(in)
		return err
	}
	_, _, err := r.sources(in)
	return err
}

// requestKnowledgeGap derives durable refresh intent from the answer itself.
// It shares the librarian's serialized queue with extraction and landings.
func (r *refresher) requestKnowledgeGap(ctx context.Context, streams []config.WorkstreamID, known map[string]bool) error {
	stream := librarianWorkstream(r.repository.Project())
	for _, ws := range streams {
		rulings, err := trace.Read[trace.Ruling](r.repository, ws)
		if err != nil {
			return err
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
				inspection, err := r.repository.CodeInspection(ws, inspectionID)
				if err != nil {
					return err
				}
				in := refreshInput{Workstream: ws, Question: ruling.QuestionID, Inspection: inspectionID, Commit: inspection.Commit}
				id := in.key()
				if known[id] {
					continue
				}
				data, err := json.Marshal(in)
				if err != nil {
					return err
				}
				event := trace.EventID(id, "run")
				op := coreadapter.Operation{ID: trace.OperationID(r.repository.Project(), stream, event), Boundary: coreadapter.RunnerBoundary, Action: RefreshAction, Input: data}
				h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: r.repository.Project(), Workstream: stream, At: r.s.now(), Actor: librarianActor, Cause: ruling.ID}
				_, err = r.repository.Transact(ctx, trace.Transaction{Transition: trace.Transition{Header: h, Subject: id, To: "requested", Reason: fmt.Sprintf("Fill the code knowledge gap behind question %s of %s", ruling.QuestionID, ws)}, Events: []trace.Event{{ID: event, Kind: RefreshAction, Body: "Refresh knowledge from a code-based answer", Operation: &op}}})
				return err
			}
		}
	}
	return nil
}
