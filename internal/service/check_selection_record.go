package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/kpenfound/osmia/internal/trace"
)

// UnitCheckSelection records a run's choice before its check runner starts.
// It has no result; only the separate UnitCheckRun establishes completion.
type UnitCheckSelection struct {
	Unit       string         `json:"unit"`
	Run        int            `json:"run"`
	Operation  string         `json:"operation"`
	Candidate  string         `json:"candidate"`
	Base       string         `json:"base"`
	DiffSHA256 string         `json:"diff_sha256"`
	Report     int            `json:"report"`
	Selection  CheckSelection `json:"selection"`
	StartedAt  time.Time      `json:"started_at"`
}

func checkSelectionDocument(unit string, run int) string {
	return checkDocumentID(unit, run) + "-selection"
}

func checkSelectionPath(unit string, run int) string {
	return fmt.Sprintf("units/%s/checks-%d-selection.json", unit, run)
}

// restoreSelection restores the run's immutable start evidence before preparation
// can record a terminal result. Once the diff is available, it must match too.
func (c *checkers) restoreSelection(in checkInput, run *UnitCheckRun) (bool, error) {
	docs, err := trace.Read[trace.Document](c.repository, in.Workstream)
	if err != nil {
		return false, err
	}
	for _, d := range docs {
		if d.ID != checkSelectionDocument(in.Unit, in.Run) {
			continue
		}
		var prior UnitCheckSelection
		if err := json.Unmarshal([]byte(d.Content), &prior); err != nil {
			return false, fmt.Errorf("%s: %w", d.Path, err)
		}
		if prior.Unit != in.Unit || prior.Run != in.Run || prior.Operation != run.Operation || prior.Candidate != in.Candidate || prior.Base != in.Base || prior.Report != in.Report || (run.DiffSHA256 != "" && prior.DiffSHA256 != run.DiffSHA256) {
			return false, fmt.Errorf("%s does not match the check run", d.Path)
		}
		run.Selection = &prior.Selection
		run.StartedAt = prior.StartedAt
		run.DiffSHA256 = prior.DiffSHA256
		return true, nil
	}
	return false, nil
}

// selection reuses the run's immutable choice, including full-run fallbacks,
// even if listing or the boost configuration changes across a restart.
func (c *checkers) selection(ctx context.Context, in checkInput, run *UnitCheckRun, dir, diff string, checks ReviewChecks) (CheckSelection, error) {
	if found, err := c.restoreSelection(in, run); err != nil {
		return CheckSelection{}, err
	} else if found {
		return *run.Selection, nil
	}
	selection := c.selectChecks(ctx, in, dir, diff, checks)
	evidence := UnitCheckSelection{Unit: in.Unit, Run: in.Run, Operation: run.Operation, Candidate: in.Candidate, Base: in.Base, Report: in.Report, DiffSHA256: run.DiffSHA256, Selection: selection, StartedAt: run.StartedAt}
	data, err := json.MarshalIndent(evidence, "", "  ")
	if err != nil {
		return CheckSelection{}, err
	}
	doc := trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: checkSelectionDocument(in.Unit, in.Run), Revision: 1, Project: c.repository.Project(), Workstream: in.Workstream, Unit: in.Unit, At: c.s.now(), Actor: checksActor, Cause: checkRequestID(in.Unit, in.Run)}, Path: checkSelectionPath(in.Unit, in.Run), Content: string(data) + "\n"}
	if err := c.repository.RecordDocuments(ctx, []trace.Document{doc}); err != nil {
		if errors.Is(err, trace.ErrConflict) {
			if found, readErr := c.restoreSelection(in, run); readErr != nil {
				return CheckSelection{}, readErr
			} else if found {
				return *run.Selection, nil
			}
		}
		return CheckSelection{}, err
	}
	return selection, nil
}
