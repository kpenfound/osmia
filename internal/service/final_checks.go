package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

// FinalCheckRun is the document final/checks-<k>.json: the run of every
// project check on the commit final review k reads, with the command, its
// exit status, diagnostic excerpt, captured output path and the check links
// Dagger's report shows failed. An incomplete run reported no result, and
// Error says why.
type FinalCheckRun struct {
	Review         int       `json:"review"`
	Operation      string    `json:"operation"`
	Commit         string    `json:"commit"`
	Status         string    `json:"status"`
	Command        []string  `json:"command,omitempty"`
	ExitCode       int       `json:"exit_code"`
	Output         string    `json:"output,omitempty"`
	OutputRevision int       `json:"output_revision,omitempty"`
	OutputPath     string    `json:"output_path,omitempty"`
	Truncated      bool      `json:"truncated,omitempty"`
	Failed         []string  `json:"failed,omitempty"`
	Error          string    `json:"error,omitempty"`
	StartedAt      time.Time `json:"started_at"`
	FinishedAt     time.Time `json:"finished_at"`
}

func finalChecksDocument(k int) string { return fmt.Sprintf("final-checks-%d", k) }

func finalChecksPath(k int) string { return fmt.Sprintf("final/checks-%d.json", k) }

// finalChecks returns the check run of the commit report reads, running
// every project check on a fresh export of it, bounded by the project's
// checks_timeout, when none is recorded. A run that cannot export or run the
// checks, or outlasts the timeout, is recorded as incomplete; the service
// stopping records nothing, so a retry runs them again.
func (a *finalReviewer) finalChecks(ctx context.Context, cfg *config.Config, stream config.WorkstreamID, report FinalReport) (FinalCheckRun, error) {
	if run, found, err := a.recordedChecks(stream, report.Review); err != nil || found && run.Commit == report.Commit {
		return run, err
	}
	run := FinalCheckRun{Review: report.Review, Operation: report.Operation, Commit: report.Commit, ExitCode: -1, StartedAt: a.s.now()}
	result, err := a.runChecks(ctx, cfg, stream, report.Commit, &run)
	if ctx.Err() != nil {
		return FinalCheckRun{}, ctx.Err()
	}
	run.ExitCode = result.ExitCode
	run.Output, run.Truncated = summarizeCheckOutput(result.Output, result.ExitCode)
	run.Truncated = run.Truncated || result.Truncated
	run.OutputPath = checkOutputPath(finalChecksPath(report.Review))
	run.Status, run.Failed, err = classifyChecks(result, err)
	if err != nil {
		run.Error = err.Error()
	}
	run.FinishedAt = a.s.now()
	run.OutputRevision, err = nextRevision(a.repository, stream, finalChecksDocument(report.Review)+"-output")
	if err != nil {
		return FinalCheckRun{}, err
	}
	data, err := json.MarshalIndent(run, "", "  ")
	if err != nil {
		return FinalCheckRun{}, err
	}
	revision, err := nextRevision(a.repository, stream, finalChecksDocument(report.Review))
	if err != nil {
		return FinalCheckRun{}, err
	}
	doc := trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: finalChecksDocument(report.Review), Revision: revision, Project: a.repository.Project(), Workstream: stream, At: run.FinishedAt, Actor: checksActor, Cause: report.Operation},
		Path: finalChecksPath(report.Review), Content: string(data) + "\n"}
	output := doc
	output.ID += "-output"
	output.Revision = run.OutputRevision
	output.Path, output.Content = run.OutputPath, result.Output
	if err := a.repository.RecordDocuments(context.WithoutCancel(ctx), []trace.Document{doc, output}); err != nil {
		return FinalCheckRun{}, err
	}
	return run, a.s.step("final-checks-recorded")
}

// runChecks runs every project check on a fresh export of commit, setting
// the run's command once the export is ready.
func (a *finalReviewer) runChecks(ctx context.Context, cfg *config.Config, stream config.WorkstreamID, commit string, run *FinalCheckRun) (CheckResult, error) {
	checks := a.s.options.reviewChecks
	if checks == nil {
		return CheckResult{ExitCode: -1}, errors.New("candidate check runner is unavailable")
	}
	g, err := featureWorkspaces(cfg, a.repository).of(stream)
	if err != nil {
		return CheckResult{ExitCode: -1}, err
	}
	base := filepath.Join(cfg.Root.String(), "checks", string(cfg.Project.ID), string(stream))
	if err := os.MkdirAll(base, 0700); err != nil {
		return CheckResult{ExitCode: -1}, err
	}
	dir, err := os.MkdirTemp(base, "final-")
	if err != nil {
		return CheckResult{ExitCode: -1}, err
	}
	defer os.RemoveAll(dir)
	if err := g.Export(ctx, commit, dir); err != nil {
		return CheckResult{ExitCode: -1}, fmt.Errorf("commit export: %w", err)
	}
	run.Command = []string{"dagger", "check", "--progress=report", "--fail-fast"}
	timeout := cfg.Project.CheckTimeout()
	bounded, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	result, err := checks.Check(bounded, dir, nil)
	if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
		err = fmt.Errorf("checks did not finish within checks_timeout %s", timeout)
	}
	return result, err
}

// recordedChecks returns the latest recorded check run of final review k,
// and whether one is.
func (a *finalReviewer) recordedChecks(stream config.WorkstreamID, k int) (FinalCheckRun, bool, error) {
	docs, err := trace.Read[trace.Document](a.repository, stream)
	if err != nil {
		return FinalCheckRun{}, false, err
	}
	for _, d := range slices.Backward(docs) {
		if d.ID != finalChecksDocument(k) {
			continue
		}
		var run FinalCheckRun
		if err := json.Unmarshal([]byte(d.Content), &run); err != nil {
			return FinalCheckRun{}, false, fmt.Errorf("%s revision %d: %w", d.Path, d.Revision, err)
		}
		return run, true, nil
	}
	return FinalCheckRun{}, false, nil
}

// finalCheckEvidence is how the final reader's prompt carries the check run
// of the commit it reads.
func finalCheckEvidence(run FinalCheckRun) string {
	out := fmt.Sprintf("The service ran every project check on this exact commit before your read, recorded in %s; you do not run them. The run %s.", finalChecksPath(run.Review), run.Status)
	if run.Status == ChecksIncomplete {
		out = fmt.Sprintf("The service ran the project's checks on this exact commit before your read, recorded in %s, and they did not complete: %s. The checks show nothing about this commit; a criterion that rests on them is a gap.", finalChecksPath(run.Review), run.Error)
	}
	if len(run.Command) > 0 {
		out += "\nCommand: " + checkExcerpt(strings.Join(quoteLinks(run.Command), " "), 2048)
	}
	if len(run.Failed) > 0 {
		out += "\nFailed: " + checkExcerpt(strings.Join(run.Failed, ", "), 2048)
	}
	return out + outputEvidence(run.Output, run.OutputPath, run.OutputRevision, run.Truncated)
}
