package verifytool

import (
	"fmt"
	"strings"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/kpenfound/osmia/internal/verification"
)

// Measured reads a recorded operation as the run a measure counts. A service
// rerun is the source rerun and a unit reviewer's verify call the source
// reviewer; any other call, such as the manager's or the final
// reviewer's, has no source and returns false.
func Measured(op Operation) (verification.MeasuredRun, bool) {
	run := verification.MeasuredRun{Outcome: verification.OutcomeLimited}
	switch {
	case op.Rerun != nil:
		run.Source = verification.SourceRerun
		switch op.Rerun.Disposition {
		case RerunSkipped:
			run.Outcome = verification.OutcomeSkipped
			return run, true
		case RerunRefused:
			run.Outcome = verification.OutcomeRefused
			return run, true
		}
	case op.Scope.Role == "reviewer" && op.Scope.Unit != "":
		run.Source = verification.SourceReviewer
	default:
		return run, false
	}
	if o := op.Outcome; o != nil && o.Limitation == "" {
		run.Outcome, run.Result = verification.OutcomeResult, o.Result
	}
	return run, true
}

// UnitRuns returns the runs recorded for unit that a measure counts: the
// service reruns of its declared invocations, whether they ran, were skipped or
// were refused, and the verify calls its reviewer made, across every candidate
// the unit had. A call that came to no outcome yet is not a run.
func UnitRuns(repo *trace.Repository, stream config.WorkstreamID, unit string) ([]verification.MeasuredRun, error) {
	ops, _, err := Operations(repo, stream)
	if err != nil {
		return nil, err
	}
	var runs []verification.MeasuredRun
	for _, op := range ops {
		run, ok := Measured(op)
		if !ok {
			continue
		}
		if op.Rerun != nil && op.Rerun.Unit != unit || op.Rerun == nil && op.Scope.Unit != unit {
			continue
		}
		if op.Outcome == nil && (op.Rerun == nil || op.Rerun.Disposition == RerunRan) {
			continue
		}
		runs = append(runs, run)
	}
	return runs, nil
}

// FinalRuns returns the verify calls the final reviewer of review made on
// commit that came to an outcome, in the order they were recorded. role is the
// final reviewer's role: its calls are told from every other role's by it and
// by the review's turn prefix.
func FinalRuns(repo *trace.Repository, stream config.WorkstreamID, role string, review int, commit string) ([]Operation, error) {
	ops, _, err := Operations(repo, stream)
	if err != nil {
		return nil, err
	}
	prefix := fmt.Sprintf("final-%d-", review)
	var runs []Operation
	for _, op := range ops {
		if op.Rerun == nil && op.Scope.Role == role && op.Workstream == stream && op.Commit == commit && op.Outcome != nil && strings.HasPrefix(op.Scope.Turn, prefix) {
			runs = append(runs, op)
		}
	}
	return runs, nil
}

// FinalResults splits the final review's runs into the reports they gave and
// the number that gave none.
func FinalResults(runs []Operation) (results []verification.Result, limited int) {
	for _, op := range runs {
		if op.Outcome.Limitation != "" {
			limited++
			continue
		}
		results = append(results, op.Outcome.Result)
	}
	return results, limited
}
