package delivery

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

	checkrun "github.com/kpenfound/osmia/internal/checks"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/trace"
)

// FinalCheckRun is the document final/checks-<k>.json: the check run
// on the commit final review k reads, with the selection, command, its
// exit status, diagnostic excerpt, captured output path and the check links
// Dagger's report shows failed. An incomplete run reported no result, and
// Error says why. Attempt links the result to its start evidence, and is zero
// for older records and failures before the checks started.
type FinalCheckRun struct {
	Review             int             `json:"review"`
	Attempt            int             `json:"attempt,omitempty"`
	Operation          string          `json:"operation"`
	Commit             string          `json:"commit"`
	Status             string          `json:"status"`
	Command            []string        `json:"command,omitempty"`
	Selection          *CheckSelection `json:"selection,omitempty"`
	ExitCode           int             `json:"exit_code"`
	Output             string          `json:"output,omitempty"`
	OutputRevision     int             `json:"output_revision,omitempty"`
	OutputPath         string          `json:"output_path,omitempty"`
	Truncated          bool            `json:"truncated,omitempty"`
	Failed             []string        `json:"failed,omitempty"`
	TraceURL           string          `json:"trace_url,omitempty"`
	CIIdentityFallback string          `json:"ci_identity_fallback,omitempty"`
	Error              string          `json:"error,omitempty"`
	StartedAt          time.Time       `json:"started_at"`
	FinishedAt         time.Time       `json:"finished_at"`
}

func FinalChecksDocument(k int) string { return fmt.Sprintf("final-checks-%d", k) }

func FinalChecksPath(k int) string { return fmt.Sprintf("final/checks-%d.json", k) }

// FinalCheckAttempt records checks starting on the rebased, exported commit.
// A result with the same review and attempt, in FinalCheckRun, completes it.
type FinalCheckAttempt struct {
	Review    int             `json:"review"`
	Attempt   int             `json:"attempt"`
	Operation string          `json:"operation"`
	Commit    string          `json:"commit"`
	Selection *CheckSelection `json:"selection,omitempty"`
	StartedAt time.Time       `json:"started_at"`
}

func FinalCheckAttemptDocument(review, attempt int) string {
	return fmt.Sprintf("final-checks-%d-attempt-%d-started", review, attempt)
}

func FinalCheckAttemptPath(review, attempt int) string {
	return fmt.Sprintf("final/checks-%d-attempt-%d-started.json", review, attempt)
}

func (a *FinalReviewer) StartCheckAttempt(ctx context.Context, stream config.WorkstreamID, run *FinalCheckRun) error {
	docs, err := trace.Read[trace.Document](a.Repository, stream)
	if err != nil {
		return err
	}
	attempt := 1
	for _, d := range docs {
		if !strings.HasPrefix(d.Path, fmt.Sprintf("final/checks-%d-attempt-", run.Review)) || !strings.HasSuffix(d.Path, "-started.json") {
			continue
		}
		var prior FinalCheckAttempt
		if err := json.Unmarshal([]byte(d.Content), &prior); err != nil {
			return fmt.Errorf("%s: %w", d.Path, err)
		}
		attempt = max(attempt, prior.Attempt+1)
	}
	evidence := FinalCheckAttempt{Review: run.Review, Attempt: attempt, Operation: run.Operation, Commit: run.Commit, Selection: run.Selection, StartedAt: a.S.Now()}
	data, err := json.MarshalIndent(evidence, "", "  ")
	if err != nil {
		return err
	}
	doc := trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: FinalCheckAttemptDocument(run.Review, attempt), Revision: 1, Project: a.Repository.Project(), Workstream: stream, At: evidence.StartedAt, Actor: checksActor, Cause: run.Operation}, Path: FinalCheckAttemptPath(run.Review, attempt), Content: string(data) + "\n"}
	if err := a.Repository.RecordDocuments(ctx, []trace.Document{doc}); err != nil {
		return err
	}
	run.Attempt, run.StartedAt = attempt, evidence.StartedAt
	return nil
}

// FinalCheckDelay is how long final review checks wait to run again after n
// runs in a row did not complete: checkRetryDelay, doubling with each further
// run, up to an hour.
func FinalCheckDelay(n int) time.Duration {
	delay := checkRetryDelay
	for i := 1; i < n && delay < time.Hour; i++ {
		delay *= 2
	}
	return min(delay, time.Hour)
}

// FinalChecks returns the completed check run of the commit report reads,
// selecting and running checks on a fresh export of it, bounded by the
// project's checks_timeout, when none is recorded. A run that cannot export
// or run the checks, or outlasts the timeout, is recorded as incomplete and
// returned with an error, so the review never reads a commit whose checks
// show nothing: the review's operation retries, and the checks run again
// once FinalCheckDelay has passed since the last incomplete run. The service
// stopping leaves its start evidence unfinished, so a retry runs them again.
func (a *FinalReviewer) FinalChecks(ctx context.Context, cfg *config.Config, stream config.WorkstreamID, report FinalReport) (FinalCheckRun, error) {
	incomplete, latest, err := a.IncompleteChecks(stream, report.Review, report.Commit)
	if err != nil {
		return FinalCheckRun{}, err
	}
	if incomplete == 0 && latest.Commit == report.Commit {
		return latest, nil
	}
	if incomplete > 0 {
		if next := latest.FinishedAt.Add(FinalCheckDelay(incomplete)); a.S.Now().Before(next) {
			return latest, IncompleteChecksError(latest, incomplete, next)
		}
	}
	run := FinalCheckRun{Review: report.Review, Operation: report.Operation, Commit: report.Commit, ExitCode: -1, StartedAt: a.S.Now()}
	result, err := a.RunChecks(ctx, cfg, stream, report, &run)
	if ctx.Err() != nil {
		return FinalCheckRun{}, ctx.Err()
	}
	run.TraceURL = result.TraceURL
	run.CIIdentityFallback = result.CIIdentityFallback
	run.ExitCode = result.ExitCode
	run.Output, run.Truncated = checkrun.SummarizeCheckOutput(result.Output, result.ExitCode)
	run.Truncated = run.Truncated || result.Truncated
	run.OutputPath = checkrun.CheckOutputPath(FinalChecksPath(report.Review))
	run.Status, run.Failed, err = checkrun.ClassifyChecks(result, err)
	if err != nil {
		run.Error = err.Error()
	}
	run.FinishedAt = a.S.Now()
	run.OutputRevision, err = nextRevision(a.Repository, stream, FinalChecksDocument(report.Review)+"-output")
	if err != nil {
		return FinalCheckRun{}, err
	}
	data, err := json.MarshalIndent(run, "", "  ")
	if err != nil {
		return FinalCheckRun{}, err
	}
	revision, err := nextRevision(a.Repository, stream, FinalChecksDocument(report.Review))
	if err != nil {
		return FinalCheckRun{}, err
	}
	doc := trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: FinalChecksDocument(report.Review), Revision: revision, Project: a.Repository.Project(), Workstream: stream, At: run.FinishedAt, Actor: checksActor, Cause: report.Operation},
		Path: FinalChecksPath(report.Review), Content: string(data) + "\n"}
	output := doc
	output.ID += "-output"
	output.Revision = run.OutputRevision
	output.Path, output.Content = run.OutputPath, result.Output
	if err := a.Repository.RecordDocuments(context.WithoutCancel(ctx), []trace.Document{doc, output}); err != nil {
		return FinalCheckRun{}, err
	}
	if err := a.S.Step("final-checks-recorded"); err != nil {
		return FinalCheckRun{}, err
	}
	if run.Status == ChecksIncomplete {
		return run, IncompleteChecksError(run, incomplete+1, run.FinishedAt.Add(FinalCheckDelay(incomplete+1)))
	}
	return run, nil
}

// IncompleteChecksError is the error a final review's operation retries on
// while its checks have not completed.
func IncompleteChecksError(latest FinalCheckRun, n int, next time.Time) error {
	return fmt.Errorf("final review %d waits for its checks: %d runs in a row on %s did not complete, the latest because %s; they run again at %s", latest.Review, n, latest.Commit, latest.Error, next.UTC().Format(time.RFC3339))
}

// RunChecks selects checks for the rebased branch against its upstream base
// and runs them on a fresh export, recording the selection before execution.
func (a *FinalReviewer) RunChecks(ctx context.Context, cfg *config.Config, stream config.WorkstreamID, report FinalReport, run *FinalCheckRun) (CheckResult, error) {
	checks := a.S.Checks
	if checks == nil {
		return CheckResult{ExitCode: -1}, errors.New("candidate check runner is unavailable")
	}
	g, err := featureWorkspaces(cfg, a.Repository).Of(stream)
	if err != nil {
		return CheckResult{ExitCode: -1}, err
	}
	if report.Upstream == nil {
		return CheckResult{ExitCode: -1}, errors.New("final check diff: upstream base is missing")
	}
	diff, err := g.Diff(ctx, report.Upstream.Commit, report.Commit)
	if err != nil {
		return CheckResult{ExitCode: -1}, fmt.Errorf("final check diff: %w", err)
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
	if err := a.S.Step("final-checks-exporting"); err != nil {
		return CheckResult{ExitCode: -1}, err
	}
	if err := g.Export(ctx, report.Commit, dir); err != nil {
		return CheckResult{ExitCode: -1}, fmt.Errorf("commit export: %w", err)
	}
	release, err := a.S.AcquireReviewCheckSlot(ctx, a.Repository.Project(), stream, report.Review, report.Operation)
	if err != nil {
		return CheckResult{ExitCode: -1}, err
	}
	defer release()
	// Attempts share the judgment identity for this review and commit, even
	// when a stopped service left only an unfinished start record.
	cause := fmt.Sprintf("final-checks-%d-%s", report.Review, report.Commit)
	scope := coreadapter.Scope{Project: string(a.Repository.Project()), Workstream: string(stream), Thread: FinalReviewSubject, Turn: cause, Role: checksRole}
	identity := checkrun.Identity{Candidate: report.Commit, Branch: featureBranch(stream), Remote: "https://github.com/" + cfg.Project.Upstream, Repository: cfg.Project.Clone}
	selection := checkrun.Select(ctx, a.S.Jev, a.Repository, scope, cause, nil, dir, diff, identity, checks)
	run.Selection = &selection
	run.Command = checkrun.Command(selection.Links)
	timeout := cfg.Project.CheckTimeout()
	if err := a.StartCheckAttempt(ctx, stream, run); err != nil {
		return CheckResult{ExitCode: -1}, err
	}
	bounded, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	result, err := checks.Check(bounded, dir, identity, selection.Links)
	if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
		err = fmt.Errorf("checks did not finish within checks_timeout %s", timeout)
	}
	return result, err
}

// IncompleteChecks returns how many of final review k's latest recorded
// check runs on commit did not complete, in a row, and its latest recorded
// run, which is zero when none is recorded.
func (a *FinalReviewer) IncompleteChecks(stream config.WorkstreamID, k int, commit string) (int, FinalCheckRun, error) {
	return IncompleteFinalChecks(a.Repository, stream, k, commit)
}

func IncompleteFinalChecks(repository *trace.Repository, stream config.WorkstreamID, k int, commit string) (int, FinalCheckRun, error) {
	docs, err := trace.Read[trace.Document](repository, stream)
	if err != nil {
		return 0, FinalCheckRun{}, err
	}
	n := 0
	var latest FinalCheckRun
	for _, d := range slices.Backward(docs) {
		if d.ID != FinalChecksDocument(k) {
			continue
		}
		var run FinalCheckRun
		if err := json.Unmarshal([]byte(d.Content), &run); err != nil {
			return 0, FinalCheckRun{}, fmt.Errorf("%s revision %d: %w", d.Path, d.Revision, err)
		}
		if latest.Review == 0 {
			latest = run
		}
		if run.Commit != commit || run.Status != ChecksIncomplete {
			break
		}
		n++
	}
	return n, latest, nil
}

// FinalCheckEvidence is how the final reader's prompt carries the check run
// of the commit it reads.
func FinalCheckEvidence(run FinalCheckRun) string {
	out := fmt.Sprintf("The service ran checks on this exact commit before your read, recorded in %s; you do not run them. The run %s.", FinalChecksPath(run.Review), run.Status)
	if run.Selection != nil {
		out += "\n" + checkrun.DescribeSelection(*run.Selection) + "."
	}
	if len(run.Command) > 0 {
		out += "\nCommand: " + checkrun.CheckExcerpt(strings.Join(quoteLinks(run.Command), " "), 2048)
	}
	if len(run.Failed) > 0 {
		out += "\nFailed: " + checkrun.CheckExcerpt(strings.Join(run.Failed, ", "), 2048)
	}
	return out + checkrun.OutputEvidence(run.Output, run.OutputPath, run.OutputRevision, run.Truncated)
}
