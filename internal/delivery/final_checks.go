package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	checkrun "github.com/kpenfound/osmia/internal/checks"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/pulls"
	"github.com/kpenfound/osmia/internal/trace"
)

// FinalCheckRun is the document final/checks-<k>.json: the check result of
// the commit final review k reads.
//
// A result with Source "pull_request" was read from the Dagger Cloud
// statuses of the workstream's pull request: PullRequest, Push and
// PushedCommit name the push whose commit the statuses belong to (Commit is
// the reviewed commit that push signed), Run is the "Run Dagger Checks"
// posting the result came from, StatusURL its JSON, Counts the counts that
// JSON holds, FailedChecks the failed checks with their `dagger trace` ids
// (Failed lists their names), Flaky the originally failed checks that passed
// their rerun, and Reruns the rerun of the push's failed checks. An
// incomplete run reported no result, and Error says why. A record with status
// "continued" holds no result at all: the final read of the push started
// because the owner chose to continue without check results (ContinuedBy),
// in the choice record's revision Choice. Records written by
// local check runs have no Source; they carry the selection, command, exit
// status, captured output and engines of that run, and Attempt links the
// result to its start evidence.
type FinalCheckRun struct {
	Review             int                   `json:"review"`
	Attempt            int                   `json:"attempt,omitempty"`
	Operation          string                `json:"operation"`
	Commit             string                `json:"commit"`
	Source             string                `json:"source,omitempty"`
	PullRequest        int                   `json:"pull_request,omitempty"`
	Push               int                   `json:"push,omitempty"`
	PushedCommit       string                `json:"pushed_commit,omitempty"`
	PushedAt           time.Time             `json:"pushed_at,omitzero"`
	Status             string                `json:"status"`
	Command            []string              `json:"command,omitempty"`
	Selection          *CheckSelection       `json:"selection,omitempty"`
	ExitCode           int                   `json:"exit_code"`
	Output             string                `json:"output,omitempty"`
	OutputRevision     int                   `json:"output_revision,omitempty"`
	OutputPath         string                `json:"output_path,omitempty"`
	Truncated          bool                  `json:"truncated,omitempty"`
	Run                *api.CheckStatusRef   `json:"run,omitempty"`
	StatusURL          string                `json:"status_url,omitempty"`
	Counts             *api.CheckCounts      `json:"counts,omitempty"`
	FailedChecks       []api.CheckFailure    `json:"failed_checks,omitempty"`
	Reruns             []api.CheckRerun      `json:"reruns,omitempty"`
	ContinuedBy        string                `json:"continued_by,omitempty"`
	Choice             int                   `json:"choice,omitempty"`
	Failed             []string              `json:"failed,omitempty"`
	Flaky              []string              `json:"flaky,omitempty"`
	TraceURL           string                `json:"trace_url,omitempty"`
	CIIdentityFallback string                `json:"ci_identity_fallback,omitempty"`
	Engines            *api.CheckAttribution `json:"engines,omitempty"`
	Error              string                `json:"error,omitempty"`
	StartedAt          time.Time             `json:"started_at"`
	FinishedAt         time.Time             `json:"finished_at"`
}

func FinalChecksDocument(k int) string { return fmt.Sprintf("final-checks-%d", k) }

func FinalChecksPath(k int) string { return fmt.Sprintf("final/checks-%d.json", k) }

// FinalCheckAttempt is the start evidence a local check run on the rebased,
// exported commit recorded, read from workstreams that hold such records.
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

// FinalCheckRerunRecord is the document final/checks-<k>-push-<n>-rerun.json:
// the one rerun of the failed checks of push n, recorded before the rerun is
// requested and read back to tell that it was. The embedded rerun holds the
// request time, the failing "Run Dagger Checks" posting that asked for it and
// the checks that failed in it; that posting stays the push's result until
// a fresh posting settles the rerun. Counts are the counts of the failing
// posting. FinishedAt and Error say how the request itself ended.
type FinalCheckRerunRecord struct {
	Review       int              `json:"review"`
	Push         int              `json:"push"`
	PullRequest  int              `json:"pull_request"`
	PushedCommit string           `json:"pushed_commit"`
	Command      []string         `json:"command"`
	Counts       *api.CheckCounts `json:"counts,omitempty"`
	FinishedAt   time.Time        `json:"finished_at,omitzero"`
	api.CheckRerun
}

func FinalCheckRerunDocument(review, push int) string {
	return fmt.Sprintf("final-checks-%d-push-%d-rerun", review, push)
}

func FinalCheckRerunPath(review, push int) string {
	return fmt.Sprintf("final/checks-%d-push-%d-rerun.json", review, push)
}

const (
	// FinalChecksWait is how long after a push the service makes no status
	// poll, so Dagger Cloud has the time to post its first statuses.
	FinalChecksWait = 2 * time.Minute
	// rerunSkew is the clock difference tolerated between the service and
	// GitHub when a status is compared with the time a rerun was requested.
	rerunSkew = 30 * time.Second
)

// FinalCheckDelay is how long final review checks wait to be read again after
// n runs in a row did not complete: checkRetryDelay, doubling with each
// further run, up to an hour.
func FinalCheckDelay(n int) time.Duration {
	delay := checkRetryDelay
	for i := 1; i < n && delay < time.Hour; i++ {
		delay *= 2
	}
	return min(delay, time.Hour)
}

// polled remembers when each push's statuses were last read, so the reads
// follow pull_request_poll. A restart forgets it and reads once at once; the
// wait after the push is measured from the recorded push.
var polled sync.Map // pollKey -> time.Time

type pollKey struct {
	repository *trace.Repository
	stream     config.WorkstreamID
	push       int
}

// WaitingChecksError is the error a final review's operation retries on while
// the pull request's Dagger Cloud checks have no result for it: the wait after
// the push, the interval between status reads, a run still pending, and a
// rerun not yet settled by a fresh posting. Next is when the statuses are read
// again.
type WaitingChecksError struct {
	Review int
	Commit string
	Push   int
	Reason string
	Next   time.Time
}

func (e *WaitingChecksError) Error() string {
	return fmt.Sprintf("final review %d waits for the checks of push %d (%s): %s; the statuses are read again at %s", e.Review, e.Push, e.Commit, e.Reason, e.Next.UTC().Format(time.RFC3339))
}

// FinalChecks returns the completed check result of the commit report reads:
// the Dagger Cloud statuses of the pull request's pushed head commit, once
// they hold a completed "Run Dagger Checks". It reads nothing until
// FinalChecksWait after the push recorded for the commit, then at the project's
// pull_request_poll interval, and records the result in
// final/checks-<k>.json. Until there is one it returns a retryable error and
// the review's operation tries again, so the final read never starts on a
// commit whose checks show nothing:
//
//   - a missing `load` sends the chief of staff one notice for the push;
//   - a failed `load` is a failed result;
//   - a run still pending, or a completed run with cancelled or unaccounted
//     checks, is recorded as incomplete when checks_timeout has passed since
//     the push (or since the rerun was requested), and read again after
//     FinalCheckDelay;
//   - a completed failure asks Dagger Cloud once to rerun the failed checks
//     (see rerunChecks), and only a fresh completed posting settles the push;
//   - the owner's choice to continue without check results for the push
//     (see AnswerChecksChoice) is recorded as a "continued" record that holds no
//     result, and applies to that push alone.
//
// A completed record of the commit, from this watch or from an older local
// run, is returned as it is. Reviews of delivered workstreams (maintenance)
// push nothing and have no pull request statuses to wait for.
func (a *FinalReviewer) FinalChecks(ctx context.Context, cfg *config.Config, stream config.WorkstreamID, report FinalReport) (FinalCheckRun, error) {
	if report.Maintenance {
		return FinalCheckRun{Review: report.Review, Operation: report.Operation, Commit: report.Commit, ExitCode: -1, Error: "a review of a delivered workstream pushes nothing, so no pull request checks ran for it"}, nil
	}
	push, found, err := PushOfCommit(a.Repository, stream, report.Commit)
	if err != nil {
		return FinalCheckRun{}, err
	}
	if !found && !pulls.Credentialed(a.S.PullRequests) {
		// The service holds no credential, so it pushed nothing and there is
		// no pull request whose statuses could be read.
		return FinalCheckRun{Review: report.Review, Operation: report.Operation, Commit: report.Commit, ExitCode: -1, Error: "the service holds no GitHub credential, so it pushed nothing and no pull request checks ran"}, nil
	}
	incomplete, latest, err := a.IncompleteChecks(stream, report.Review, report.Commit)
	if err != nil {
		return FinalCheckRun{}, err
	}
	if incomplete == 0 && latest.Commit == report.Commit && latest.Status != "" {
		switch {
		case latest.Source == "":
			return latest, nil
		case found && latest.Push == push.Number && latest.PushedCommit == push.Commit:
			return latest, nil
		}
	}
	if !found {
		return FinalCheckRun{}, fmt.Errorf("final review %d waits for its pull request: no push of %s is recorded, so there are no Dagger Cloud statuses to read", report.Review, report.Commit)
	}
	if current, _, err := CurrentPush(a.Repository, stream); err != nil {
		return FinalCheckRun{}, err
	} else if current.Number != push.Number {
		return FinalCheckRun{}, fmt.Errorf("final review %d read %s, which push %d pushed; push %d supersedes it", report.Review, report.Commit, push.Number, current.Number)
	}
	if err := a.S.closeObsoleteChecksChoices(ctx, a.Repository, stream); err != nil {
		return FinalCheckRun{}, err
	}
	watch := &finalWatch{a: a, cfg: cfg, stream: stream, report: report, push: push}
	// The owner's choice to continue without check results applies to the
	// push it was made about and to no other.
	if choice, recorded, err := ReadChecksChoice(a.Repository, stream, push.Number); err != nil {
		return FinalCheckRun{}, err
	} else if recorded && choice.State == ChecksChoiceContinue && choice.Concerns(push) {
		return watch.continuedChecks(ctx, choice)
	}
	return watch.step(ctx, incomplete, latest)
}

// finalWatch is one attempt to settle the check result of one push.
type finalWatch struct {
	a      *FinalReviewer
	cfg    *config.Config
	stream config.WorkstreamID
	report FinalReport
	push   Push
	rerun  *FinalCheckRerunRecord
}

func (w *finalWatch) waiting(reason string, next time.Time) error {
	return &WaitingChecksError{Review: w.report.Review, Commit: w.report.Commit, Push: w.push.Number, Reason: reason, Next: next}
}

func (w *finalWatch) step(ctx context.Context, incomplete int, latest FinalCheckRun) (FinalCheckRun, error) {
	a, now := w.a, w.a.S.Now()
	if incomplete > 0 {
		if next := latest.FinishedAt.Add(FinalCheckDelay(incomplete)); now.Before(next) {
			return latest, IncompleteChecksError(latest, incomplete, next)
		}
	}
	if start := w.push.At.Add(a.S.postPushWait()); now.Before(start) {
		return FinalCheckRun{}, w.waiting(fmt.Sprintf("no status is read until %s after the push", a.S.postPushWait()), start)
	}
	key := pollKey{a.Repository, w.stream, w.push.Number}
	if incomplete == 0 {
		if at, ok := polled.Load(key); ok {
			if next := at.(time.Time).Add(w.cfg.Project.PullRequestPollInterval()); now.Before(next) {
				return FinalCheckRun{}, w.waiting("the statuses were read a moment ago", next)
			}
		}
	}
	polled.Store(key, now)
	rerun, err := ReadFinalCheckRerun(a.Repository, w.stream, w.report.Review, w.push.Number)
	if err != nil {
		return FinalCheckRun{}, err
	}
	w.rerun = rerun
	deadline := w.push.At.Add(w.cfg.Project.CheckTimeout())
	if rerun != nil {
		deadline = rerun.RequestedAt.Add(w.cfg.Project.CheckTimeout())
	}
	reader := a.S.statusReader()
	if reader == nil {
		return FinalCheckRun{}, fmt.Errorf("final review %d cannot read the pull request statuses: the service has no GitHub status client", w.report.Review)
	}
	result, read, err := checkrun.ReadCloudStatuses(ctx, reader, a.S.CloudStatusFetcher, w.push.Upstream, w.push.Commit)
	if ctx.Err() != nil {
		return FinalCheckRun{}, ctx.Err()
	}
	if err != nil {
		if now.Before(deadline) {
			return FinalCheckRun{}, w.waiting("the statuses could not be read: "+err.Error(), now.Add(w.cfg.Project.PullRequestPollInterval()))
		}
		return w.recordIncomplete(ctx, incomplete, latest, nil, "the Dagger Cloud statuses could not be read: "+err.Error())
	}
	pending := func(reason string) (FinalCheckRun, error) {
		if !now.Before(deadline) {
			return w.recordIncomplete(ctx, incomplete, latest, &result, fmt.Sprintf("no completed %q status within checks_timeout %s of the %s: %s", checkrun.CloudRunContext, w.cfg.Project.CheckTimeout(), w.since(), reason))
		}
		return FinalCheckRun{}, w.waiting(reason, now.Add(w.cfg.Project.PullRequestPollInterval()))
	}
	switch result.Outcome {
	case checkrun.CloudLoadMissing:
		if err := w.noticeLoadMissing(ctx); err != nil {
			return FinalCheckRun{}, err
		}
		return FinalCheckRun{}, w.waiting("Dagger Cloud has posted no `load` status", now.Add(w.cfg.Project.PullRequestPollInterval()))
	case checkrun.CloudLoadFailed:
		if rerun != nil && !w.freshStatus(result.Load) {
			return pending("the failure of the rerun's predecessor is still showing")
		}
		return w.record(ctx, result, read, "failed")
	case checkrun.CloudPending:
		return pending(fmt.Sprintf("the %q status is pending", checkrun.CloudRunContext))
	}
	if rerun != nil && !w.freshStatus(result.Run) {
		return pending(fmt.Sprintf("the %q status showing is %d, which was posted before the rerun was requested", checkrun.CloudRunContext, statusID(result.Run)))
	}
	switch result.Outcome {
	case checkrun.CloudPassed:
		return w.record(ctx, result, read, "passed")
	case checkrun.CloudIncomplete:
		return w.recordIncomplete(ctx, incomplete, latest, &result, "the completed run is not a pass: "+result.Detail)
	}
	if rerun == nil {
		if err := w.rerunChecks(ctx, result); err != nil {
			return FinalCheckRun{}, err
		}
		return FinalCheckRun{}, w.waiting("the failed checks were rerun, and the failure still showing is not the rerun's result", now.Add(w.cfg.Project.PullRequestPollInterval()))
	}
	return w.record(ctx, result, read, "failed")
}

func statusID(s *pulls.CommitStatus) int64 {
	if s == nil {
		return 0
	}
	return s.ID
}

// since says what the checks_timeout of the run counts from.
func (w *finalWatch) since() string {
	if w.rerun != nil {
		return "rerun request"
	}
	return "push"
}

// freshStatus reports whether status was posted by the rerun the record
// holds: a posting other than the failing one that asked for it, and not
// created before the request (less rerunSkew for the difference between the
// two clocks). The failure still showing after the request is the same
// posting as the recorded one, so it is never fresh.
func (w *finalWatch) freshStatus(status *pulls.CommitStatus) bool {
	if w.rerun == nil {
		return true
	}
	return status != nil && status.ID != w.rerun.Original.ID && !status.CreatedAt.Before(w.rerun.RequestedAt.Add(-rerunSkew))
}

func (w *finalWatch) record(ctx context.Context, result checkrun.CloudResult, read pulls.CommitStatuses, status string) (FinalCheckRun, error) {
	run := w.base(result)
	run.Status = status
	switch status {
	case "passed":
		run.ExitCode = 0
	default:
		run.ExitCode = 1
	}
	for _, f := range result.Failed {
		run.FailedChecks = append(run.FailedChecks, api.CheckFailure{Name: f.Name, TraceID: f.TraceID, StatusID: f.Status.ID})
		run.Failed = append(run.Failed, f.Name)
	}
	if w.rerun != nil {
		rerun := w.rerun.CheckRerun
		fresh := checkRef(result.Run)
		rerun.Fresh = &fresh
		rerun.Flakes = w.flakes(result, read)
		run.Reruns = []api.CheckRerun{rerun}
		run.Flaky = rerun.Flakes
	}
	if result.Outcome == checkrun.CloudLoadFailed {
		run.Error = "the module did not load"
	}
	if err := w.write(ctx, &run); err != nil {
		return FinalCheckRun{}, err
	}
	polled.Delete(pollKey{w.a.Repository, w.stream, w.push.Number})
	return run, nil
}

// flakes returns the originally failed checks the rerun's evidence shows
// passed: all of them when the fresh run has no failure, otherwise those whose
// own status was posted as a success after the request. Whatever has no such
// evidence stays failed.
func (w *finalWatch) flakes(result checkrun.CloudResult, read pulls.CommitStatuses) []string {
	var out []string
	for _, original := range w.rerun.Failed {
		switch {
		case result.Outcome == checkrun.CloudPassed:
			out = append(out, original.Name)
		case result.Outcome == checkrun.CloudFailed:
			if s, ok := read.Status(original.Name); ok && s.State == "success" && s.ID != original.StatusID && !s.CreatedAt.Before(w.rerun.RequestedAt.Add(-rerunSkew)) {
				out = append(out, original.Name)
			}
		}
	}
	slices.Sort(out)
	return out
}

func (w *finalWatch) recordIncomplete(ctx context.Context, incomplete int, latest FinalCheckRun, result *checkrun.CloudResult, why string) (FinalCheckRun, error) {
	var base checkrun.CloudResult
	if result != nil {
		base = *result
	}
	run := w.base(base)
	run.Status, run.ExitCode, run.Error = ChecksIncomplete, -1, why
	if latest.Commit == w.report.Commit && latest.Source != "" && !latest.FinishedAt.IsZero() {
		run.StartedAt = latest.FinishedAt
	}
	if w.rerun != nil {
		run.Reruns = []api.CheckRerun{w.rerun.CheckRerun}
	}
	if err := w.write(ctx, &run); err != nil {
		return FinalCheckRun{}, err
	}
	return run, IncompleteChecksError(run, incomplete+1, run.FinishedAt.Add(FinalCheckDelay(incomplete+1)))
}

func checkRef(s *pulls.CommitStatus) api.CheckStatusRef {
	if s == nil {
		return api.CheckStatusRef{}
	}
	return api.CheckStatusRef{ID: s.ID, State: s.State, TargetURL: s.TargetURL, CreatedAt: s.CreatedAt}
}

// base is the record of a result before its status: the push it belongs to
// and what the statuses showed.
func (w *finalWatch) base(result checkrun.CloudResult) FinalCheckRun {
	now := w.a.S.Now()
	run := FinalCheckRun{Review: w.report.Review, Operation: w.report.Operation, Commit: w.report.Commit, Source: api.CheckSourcePullRequest, PullRequest: w.push.PullRequest, Push: w.push.Number, PushedCommit: w.push.Commit, PushedAt: w.push.At, StartedAt: w.push.At, FinishedAt: now, StatusURL: result.StatusURL}
	if result.Run != nil {
		ref := checkRef(result.Run)
		run.Run = &ref
	}
	if result.Counts != nil {
		run.Counts = &api.CheckCounts{Total: result.Counts.Total, Passed: result.Counts.Passed, Failed: result.Counts.Failed, Cancelled: result.Counts.Cancelled}
	}
	return run
}

func (w *finalWatch) write(ctx context.Context, run *FinalCheckRun) error {
	a := w.a
	data, err := json.MarshalIndent(run, "", "  ")
	if err != nil {
		return err
	}
	revision, err := nextRevision(a.Repository, w.stream, FinalChecksDocument(run.Review))
	if err != nil {
		return err
	}
	doc := trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: FinalChecksDocument(run.Review), Revision: revision, Project: a.Repository.Project(), Workstream: w.stream, At: run.FinishedAt, Actor: checksActor, Cause: run.Operation},
		Path: FinalChecksPath(run.Review), Content: string(data) + "\n"}
	if err := a.Repository.RecordDocuments(context.WithoutCancel(ctx), []trace.Document{doc}); err != nil {
		return err
	}
	return a.S.Step("final-checks-recorded")
}

// ReadFinalCheckRerun returns the rerun recorded for push n of final review
// k, or nil when none was requested.
func ReadFinalCheckRerun(repository *trace.Repository, stream config.WorkstreamID, k, push int) (*FinalCheckRerunRecord, error) {
	docs, err := trace.Read[trace.Document](repository, stream)
	if err != nil {
		return nil, err
	}
	var out *FinalCheckRerunRecord
	revision := 0
	for _, d := range docs {
		if d.ID != FinalCheckRerunDocument(k, push) || d.Revision < revision {
			continue
		}
		var record FinalCheckRerunRecord
		if err := json.Unmarshal([]byte(d.Content), &record); err != nil {
			return nil, fmt.Errorf("%s revision %d: %w", d.Path, d.Revision, err)
		}
		out, revision = &record, d.Revision
	}
	return out, nil
}

// rerunChecks asks Dagger Cloud once to rerun the failed checks of the push
// whose failure result shows. The request is recorded durably, with the push,
// the time and the identity of the failing posting and its failed checks,
// before the command runs, so a restart that finds the record never runs it
// again, and the failing posting that stays on the commit is told apart from
// the rerun's own. The command runs in the workstream's checkout with the
// token cloud_token_env names.
func (w *finalWatch) rerunChecks(ctx context.Context, result checkrun.CloudResult) error {
	a := w.a
	rerunner := a.S.CloudRerun
	if rerunner == nil {
		return fmt.Errorf("final review %d cannot rerun the failed checks of push %d: the service has no Dagger Cloud rerun runner", w.report.Review, w.push.Number)
	}
	if err := rerunner.CanRerun(); err != nil {
		return fmt.Errorf("final review %d cannot rerun the failed checks of push %d: %w; set the environment variable dagger.cloud_token_env names", w.report.Review, w.push.Number, err)
	}
	record := FinalCheckRerunRecord{Review: w.report.Review, Push: w.push.Number, PullRequest: w.push.PullRequest, PushedCommit: w.push.Commit, Command: append([]string{"dagger"}, checkrun.RerunCommand(w.push.PullRequest)...)}
	record.RequestedAt, record.Original = a.S.Now(), checkRef(result.Run)
	if result.Counts != nil {
		record.Counts = &api.CheckCounts{Total: result.Counts.Total, Passed: result.Counts.Passed, Failed: result.Counts.Failed, Cancelled: result.Counts.Cancelled}
	}
	for _, f := range result.Failed {
		record.Failed = append(record.Failed, api.CheckFailure{Name: f.Name, TraceID: f.TraceID, StatusID: f.Status.ID})
	}
	if err := w.writeRerun(ctx, record); err != nil {
		return err
	}
	if err := a.S.Step("final-checks-rerun-requested"); err != nil {
		return err
	}
	dir := a.S.rerunDirectory(ctx, w.cfg, a.Repository, w.stream)
	runErr := rerunner.RerunFailed(ctx, dir, w.push.PullRequest)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	record.FinishedAt = a.S.Now()
	if runErr != nil {
		record.Error = runErr.Error()
	}
	if err := w.writeRerun(ctx, record); err != nil {
		return err
	}
	return a.S.Step("final-checks-rerun-run")
}

func (w *finalWatch) writeRerun(ctx context.Context, record FinalCheckRerunRecord) error {
	a := w.a
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	id := FinalCheckRerunDocument(record.Review, record.Push)
	revision, err := nextRevision(a.Repository, w.stream, id)
	if err != nil {
		return err
	}
	doc := trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: id, Revision: revision, Project: a.Repository.Project(), Workstream: w.stream, At: a.S.Now(), Actor: checksActor, Cause: w.report.Operation},
		Path: FinalCheckRerunPath(record.Review, record.Push), Content: string(data) + "\n"}
	return a.Repository.RecordDocuments(context.WithoutCancel(ctx), []trace.Document{doc})
}

// loadMissingSubject is the workflow subject that carries the one chief of
// staff notice sent for a push whose `load` status never appeared.
func loadMissingSubject(review, push int) string {
	return fmt.Sprintf("final-checks-%d-push-%d-load", review, push)
}

// noticeLoadMissing sends the chief of staff the notice that Dagger Cloud has
// posted no `load` status for the pushed commit, once for the push: a retry,
// a later poll and a restart find the notice recorded and send nothing.
func (w *finalWatch) noticeLoadMissing(ctx context.Context) error {
	a := w.a
	subject := loadMissingSubject(w.report.Review, w.push.Number)
	state, err := a.Repository.Workflow(w.stream, subject)
	if err != nil {
		return err
	}
	if state.Value != "" {
		return nil
	}
	body := fmt.Sprintf("Dagger Cloud has posted no `load` status for commit %s of pull request #%d in %s, push %d, %s after the push, so the final review of the assembled branch has no check results to wait for. Investigate whether Cloud Checks run for this repository's pull requests. The service keeps reading the statuses. Call %s with action poll and push %d to read them now, or with action rerun to rerun every check of the pull request. If that does not help, or the project has no Cloud Checks, call %s with push %d to ask the owner whether to keep waiting or to continue the final review without check results. Only the owner can choose to continue, and only for this push: a later push closes the question.", w.push.Commit, w.push.PullRequest, w.push.Upstream, w.push.Number, a.S.Now().Sub(w.push.At).Round(time.Second), ChecksToolName, w.push.Number, ChecksChoiceToolName, w.push.Number)
	id := fmt.Sprintf("final-checks-%d-push-%d-load-missing", w.report.Review, w.push.Number)
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: a.Repository.Project(), Workstream: w.stream, At: a.S.Now(), Actor: FinalReviewActor, Cause: w.report.Operation}
	tx := trace.Transaction{ExpectedVersion: state.Version,
		Transition: trace.Transition{Header: h, Subject: subject, From: state.Value, To: "noticed", Reason: fmt.Sprintf("no load status for %s after push %d", w.push.Commit, w.push.Number)},
		Events:     []trace.Event{trace.Notice(id, "chief", body)}}
	if _, err := a.Repository.Transact(ctx, tx); err != nil && !errors.Is(err, trace.ErrConflict) {
		return err
	}
	return nil
}

// IncompleteChecksError is the error a final review's operation retries on
// while its checks have not completed.
func IncompleteChecksError(latest FinalCheckRun, n int, next time.Time) error {
	because := latest.Error
	if cloud := checkrun.DescribeAttribution(latest.Engines); cloud != "" {
		because += "; " + strings.TrimSuffix(cloud, ".")
	}
	return fmt.Errorf("final review %d waits for its checks: %d runs in a row on %s did not complete, the latest because %s; the statuses are read again at %s", latest.Review, n, latest.Commit, because, next.UTC().Format(time.RFC3339))
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

// FinalCheckEvidence is how the final reader's prompt carries the check
// result of the commit it reads.
func FinalCheckEvidence(run FinalCheckRun) string {
	if run.Source == api.CheckSourcePullRequest {
		return pullRequestEvidence(run)
	}
	if run.Status == "" {
		return "No check results exist for this commit: " + run.Error + ". You do not run checks."
	}
	out := fmt.Sprintf("The service ran checks on this exact commit before your read, recorded in %s; you do not run them. The run %s.", FinalChecksPath(run.Review), run.Status)
	if run.Selection != nil {
		out += "\n" + checkrun.DescribeSelectionRun(*run.Selection, run.Engines) + "."
	}
	if len(run.Command) > 0 {
		out += "\nCommand: " + checkrun.CheckExcerpt(strings.Join(quoteLinks(run.Command), " "), 2048)
	}
	if len(run.Failed) > 0 {
		out += "\nFailed: " + checkrun.CheckExcerpt(strings.Join(run.Failed, ", "), 2048)
	}
	if flaky := checkrun.DescribeFlaky(run.Flaky); flaky != "" {
		out += "\n" + flaky
	}
	return out + checkrun.OutputEvidence(run.Output, run.OutputPath, run.OutputRevision, run.Truncated)
}

func pullRequestEvidence(run FinalCheckRun) string {
	if run.Status == ChecksContinued {
		return fmt.Sprintf("No check results exist for this commit. Dagger Cloud's checks on pull request #%d, push %d, commit %s produced no result the service could read, and the owner chose to continue the final review without check results for this push, recorded in %s. You do not run checks, and nothing here says the checks passed or failed.", run.PullRequest, run.Push, run.PushedCommit, FinalChecksPath(run.Review))
	}
	out := fmt.Sprintf("Dagger Cloud ran the project's checks on this branch; the service read their commit statuses on pull request #%d, push %d, commit %s, recorded in %s. You do not run checks. The result is %s.", run.PullRequest, run.Push, run.PushedCommit, FinalChecksPath(run.Review), run.Status)
	if run.Counts != nil {
		c := run.Counts
		out += fmt.Sprintf("\nCounts: %d checks, %d passed, %d failed, %d cancelled.", c.Total, c.Passed, c.Failed, c.Cancelled)
	}
	if run.StatusURL != "" {
		out += "\nStatus: " + run.StatusURL
	}
	if len(run.FailedChecks) > 0 {
		var failed []string
		for _, f := range run.FailedChecks {
			if f.TraceID != "" {
				failed = append(failed, fmt.Sprintf("%s (dagger trace %s)", f.Name, f.TraceID))
			} else {
				failed = append(failed, f.Name)
			}
		}
		out += "\nFailed: " + checkrun.CheckExcerpt(strings.Join(failed, ", "), 2048)
	}
	for _, rerun := range run.Reruns {
		out += fmt.Sprintf("\nThe failed checks were rerun once, requested at %s.", rerun.RequestedAt.UTC().Format(time.RFC3339))
		if len(rerun.Flakes) > 0 {
			out += " Flaky, failed and then passed on rerun: " + checkrun.CheckExcerpt(strings.Join(rerun.Flakes, ", "), 2048) + "."
		}
	}
	if run.Error != "" {
		out += "\nNote: " + run.Error
	}
	return out
}
