package verifytool

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/kpenfound/osmia/internal/callargs"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/isolation"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/kpenfound/osmia/internal/verification"
)

// What became of one invocation a mason declared.
const (
	RerunRan     = "ran"
	RerunSkipped = "skipped"
	RerunRefused = "refused"
)

// MaxDeclared bounds how many invocations one done report may declare.
const MaxDeclared = 50

// Rerun is the part of an Operation that says which declared invocation of
// which report it belongs to and what became of it. A skipped or refused
// invocation has no run ID and no outcome: Reason says why.
type Rerun struct {
	Unit string `json:"unit"`
	// Report is the revision of the unit's report that declared it.
	Report int `json:"report"`
	// Index is the invocation's 1-based position in the declared list.
	Index       int    `json:"index"`
	Disposition string `json:"disposition"`
	Reason      string `json:"reason,omitempty"`
}

// RerunID is the trace record ID of the rerun of the index-th invocation
// (1-based) that revision report of unit's report declared.
func RerunID(unit string, report, index int) string {
	return fmt.Sprintf("verify-rerun-%s-r%d-%d", trace.UnitSubject(unit), report, index)
}

// RerunInput is what RerunDeclared needs to rerun the invocations of one
// report on its captured candidate.
type RerunInput struct {
	Repo   *trace.Repository
	Stream config.WorkstreamID
	Config *config.Config
	Scope  coreadapter.Scope
	Unit   string
	// Report is the revision of the unit's report that declared Declared.
	Report int
	// Commit is the captured candidate, and Dir an export of it.
	Commit string
	Dir    string
	// Declared are the argument lists the mason declared, in order.
	Declared [][]string
	Now      func() time.Time
}

// RerunDeclared reruns at most verification.MaxRerunsPerUnit of the declared
// invocations, in declared order, on the candidate exported in in.Dir, each
// under a run ID of its own, and retains each run's evidence. An invocation
// past the bound is recorded as skipped and one the argument rules refuse as
// refused, each with its reason and without a run. Every invocation is
// recorded in the trace before it runs and again with its outcome, so a retry
// of an interrupted call reuses the run IDs and the evidence already kept and
// repeats nothing that finished. The caller holds the checks slot.
func RerunDeclared(ctx context.Context, in RerunInput) error {
	_, latest, err := Operations(in.Repo, in.Stream)
	if err != nil {
		return err
	}
	for i, args := range in.Declared {
		id := RerunID(in.Unit, in.Report, i+1)
		var op Operation
		doc, found := latest[id]
		if found {
			if err := json.Unmarshal([]byte(doc.Content), &op); err != nil {
				return err
			}
		} else {
			op = in.record(i+1, args)
			doc = trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: id, Revision: 1, Project: in.Repo.Project(), Workstream: in.Stream, Unit: in.Unit, At: in.Now(), Actor: serviceActor, Cause: in.Scope.Turn}, Path: "tools/" + id + ".json"}
			op.ID = id
			if err := writeOperation(ctx, in.Repo, &doc, op, in.Now); err != nil {
				return err
			}
		}
		if op.Rerun == nil || op.Rerun.Disposition != RerunRan || op.Outcome != nil {
			continue
		}
		if kept, ok, err := Retained(in.Repo, in.Stream, op.RunID); err != nil {
			return err
		} else if ok {
			op.Outcome = &kept
		} else {
			outcome := RunExported(ctx, Target{Repo: in.Repo, Stream: in.Stream, Config: in.Config, Unit: in.Unit, Commit: in.Commit}, in.Dir, op)
			if err := ctx.Err(); err != nil {
				return err
			}
			op.Outcome = &outcome
		}
		doc.Revision++
		if err := writeOperation(ctx, in.Repo, &doc, op, in.Now); err != nil {
			return err
		}
	}
	return nil
}

func (in RerunInput) record(index int, args []string) Operation {
	op := Operation{Scope: in.Scope, Workstream: in.Stream, Unit: in.Unit, Commit: in.Commit, Args: args, Timeout: DefaultTimeout.String(),
		Rerun: &Rerun{Unit: in.Unit, Report: in.Report, Index: index, Disposition: RerunRan}}
	refuse := func(disposition, reason string) Operation {
		op.Rerun.Disposition, op.Rerun.Reason = disposition, Diagnostic(reason)
		return op
	}
	switch err := refusal(in.Dir, args); {
	case index > verification.MaxRerunsPerUnit:
		return refuse(RerunSkipped, fmt.Sprintf("only the first %d declared invocations are rerun, and this is number %d", verification.MaxRerunsPerUnit, index))
	case err != nil:
		return refuse(RerunRefused, err.Error())
	}
	op.RunID = NewRunID()
	return op
}

func refusal(dir string, args []string) error {
	if err := callargs.Validate("lever call", args); err != nil {
		return err
	}
	if callargs.SetsRunID(args) {
		return fmt.Errorf("lever call cannot set %s: the service gives every run its own", callargs.RunIDFlag)
	}
	return callargs.ValidateSource("lever call", dir, args)
}

// Reruns returns the service reruns recorded for unit on candidate, whether
// they ran, were skipped or were refused, in the order they were declared.
func Reruns(repo *trace.Repository, stream config.WorkstreamID, unit, candidate string) ([]Operation, error) {
	ops, _, err := Operations(repo, stream)
	if err != nil {
		return nil, err
	}
	var reruns []Operation
	for _, op := range ops {
		if op.Rerun != nil && op.Rerun.Unit == unit && op.Commit == candidate {
			reruns = append(reruns, op)
		}
	}
	return reruns, nil
}

// RunsOn returns the service-recorded runs of unit on candidate that came to a
// report or a limitation: its service reruns and its reviewer's verify runs.
func RunsOn(repo *trace.Repository, stream config.WorkstreamID, unit, candidate string) ([]Operation, error) {
	ops, _, err := Operations(repo, stream)
	if err != nil {
		return nil, err
	}
	var runs []Operation
	for _, op := range ops {
		if op.Commit != candidate || op.Outcome == nil {
			continue
		}
		if op.Rerun != nil && op.Rerun.Unit == unit || op.Rerun == nil && op.Scope.Role == "reviewer" && op.Scope.Unit == unit {
			runs = append(runs, op)
		}
	}
	return runs, nil
}

// PlaceReruns puts into view the retained files of the service reruns of unit
// on candidate, each under its run's directory, and the images also under
// images/. It runs no lever.
func PlaceReruns(repo *trace.Repository, stream config.WorkstreamID, unit, candidate string, view isolation.View) error {
	reruns, err := Reruns(repo, stream, unit, candidate)
	if err != nil {
		return err
	}
	for _, op := range reruns {
		if op.RunID == "" || op.Outcome == nil {
			continue
		}
		if _, err := Place(repo, op.Workstream, op.RunID, view, true); err != nil {
			return err
		}
	}
	return nil
}
