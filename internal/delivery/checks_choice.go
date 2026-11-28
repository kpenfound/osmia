package delivery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	checkrun "github.com/kpenfound/osmia/internal/checks"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/trace"
)

// The states of the choice about a push whose check results did not appear.
// A choice is offered by the chief of staff and answered by the owner; a push
// that supersedes the one it concerns, or a completed result for that push,
// closes an offer nobody answered.
const (
	ChecksChoiceOffered     = "offered"
	ChecksChoiceKeepWaiting = api.ChecksChoiceKeepWaiting
	ChecksChoiceContinue    = api.ChecksChoiceContinue
	// ChecksChoiceObsolete is an offer a later push superseded.
	ChecksChoiceObsolete = "obsolete"
)

// ChecksContinued is the status of a final check record whose final read
// started on the owner's choice to continue without check results: the record
// holds no result.
const ChecksContinued = "continued"

// ChecksToolName is the chief-only service tool that asks for a fresh read of
// the pushed commit's statuses or for a rerun of the pull request's checks,
// and ChecksChoiceToolName the one that brings the owner the choice to keep
// waiting or to continue without check results.
const (
	ChecksToolName       = "pull_request_checks"
	ChecksChoiceToolName = "ask_owner_about_checks"
)

// ChecksChoice is the document final/checks-push-<n>-choice.json: the choice
// about the check results of push n. Every change of State is a new revision.
// Push, Operation, PullRequest and PushedCommit identify the push the choice
// concerns, as the push record holds it; no other push can use it. Offer is
// the revision of the offered record an answer belongs to.
type ChecksChoice struct {
	Push         int       `json:"push"`
	Operation    string    `json:"operation"`
	PullRequest  int       `json:"pull_request"`
	PushedCommit string    `json:"pushed_commit"`
	State        string    `json:"state"`
	Offer        int       `json:"offer,omitempty"`
	Note         string    `json:"note,omitempty"`
	OfferedAt    time.Time `json:"offered_at"`
	At           time.Time `json:"at"`
}

// ChecksChoiceRecord is a ChecksChoice and the revision of its document.
type ChecksChoiceRecord struct {
	ChecksChoice
	Revision int
}

func ChecksChoiceDocument(push int) string { return fmt.Sprintf("final-checks-push-%d-choice", push) }

func ChecksChoicePath(push int) string { return fmt.Sprintf("final/checks-push-%d-choice.json", push) }

// Concerns reports whether the choice is the one about push.
func (c ChecksChoice) Concerns(push Push) bool {
	return c.Push == push.Number && c.Operation == push.Operation && c.PullRequest == push.PullRequest && c.PushedCommit == push.Commit
}

// ReadChecksChoice returns the latest record of the choice about push n, and
// whether one is recorded.
func ReadChecksChoice(repository *trace.Repository, stream config.WorkstreamID, push int) (ChecksChoiceRecord, bool, error) {
	docs, err := trace.Read[trace.Document](repository, stream)
	if err != nil {
		return ChecksChoiceRecord{}, false, err
	}
	var out ChecksChoiceRecord
	found := false
	for _, d := range docs {
		if d.ID != ChecksChoiceDocument(push) || d.Revision < out.Revision {
			continue
		}
		var record ChecksChoice
		if err := json.Unmarshal([]byte(d.Content), &record); err != nil {
			return ChecksChoiceRecord{}, false, fmt.Errorf("%s revision %d: %w", d.Path, d.Revision, err)
		}
		out, found = ChecksChoiceRecord{ChecksChoice: record, Revision: d.Revision}, true
	}
	return out, found, nil
}

func (s *Controller) writeChecksChoice(ctx context.Context, repository *trace.Repository, stream config.WorkstreamID, choice ChecksChoice, actor trace.Actor, cause string, keep func(ChecksChoiceRecord, bool) bool) (int, bool, error) {
	data, err := json.MarshalIndent(choice, "", "  ")
	if err != nil {
		return 0, false, err
	}
	for attempt := 0; ; attempt++ {
		latest, found, err := ReadChecksChoice(repository, stream, choice.Push)
		if err != nil {
			return 0, false, err
		}
		if keep != nil && !keep(latest, found) {
			return latest.Revision, false, nil
		}
		revision, err := nextRevision(repository, stream, ChecksChoiceDocument(choice.Push))
		if err != nil {
			return 0, false, err
		}
		doc := trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: ChecksChoiceDocument(choice.Push), Revision: revision, Project: repository.Project(), Workstream: stream, At: choice.At, Actor: actor, Cause: cause},
			Path: ChecksChoicePath(choice.Push), Content: string(data) + "\n"}
		err = repository.RecordDocuments(context.WithoutCancel(ctx), []trace.Document{doc})
		if errors.Is(err, trace.ErrConflict) && attempt < 3 {
			continue
		}
		return revision, err == nil, err
	}
}

func (s *Controller) closeObsoleteChecksChoices(ctx context.Context, repository *trace.Repository, stream config.WorkstreamID) error {
	current, found, err := CurrentPush(repository, stream)
	if err != nil || !found {
		return err
	}
	pushes, err := Pushes(repository, stream)
	if err != nil {
		return err
	}
	for _, p := range pushes {
		if p.Number >= current.Number {
			continue
		}
		record, found, err := ReadChecksChoice(repository, stream, p.Number)
		if err != nil {
			return err
		}
		if !found || record.State != ChecksChoiceOffered {
			continue
		}
		closed := record.ChecksChoice
		closed.State, closed.Offer, closed.At = ChecksChoiceObsolete, record.Revision, s.Now()
		if _, _, err := s.writeChecksChoice(ctx, repository, stream, closed, checksActor, fmt.Sprintf("push %d supersedes push %d", current.Number, p.Number), func(latest ChecksChoiceRecord, found bool) bool {
			return found && latest.State == ChecksChoiceOffered
		}); err != nil {
			return err
		}
	}
	return nil
}

func completedChecksOf(repository *trace.Repository, stream config.WorkstreamID, push Push) (FinalCheckRun, bool, error) {
	docs, err := trace.Read[trace.Document](repository, stream)
	if err != nil {
		return FinalCheckRun{}, false, err
	}
	for _, d := range docs {
		if !finalCheckPath(d.Path) {
			continue
		}
		var run FinalCheckRun
		if err := json.Unmarshal([]byte(d.Content), &run); err != nil {
			return FinalCheckRun{}, false, fmt.Errorf("%s revision %d: %w", d.Path, d.Revision, err)
		}
		if run.Source == api.CheckSourcePullRequest && run.Push == push.Number && run.PushedCommit == push.Commit && (run.Status == "passed" || run.Status == "failed" || run.Status == ChecksContinued) {
			return run, true, nil
		}
	}
	return FinalCheckRun{}, false, nil
}

func finalCheckPath(path string) bool {
	rest, ok := strings.CutPrefix(path, "final/checks-")
	if !ok {
		return false
	}
	rest, ok = strings.CutSuffix(rest, ".json")
	if !ok {
		return false
	}
	for _, r := range rest {
		if r < '0' || r > '9' {
			return false
		}
	}
	return rest != ""
}

func pushNamed(repository *trace.Repository, stream config.WorkstreamID, named int) (Push, string, error) {
	current, found, err := CurrentPush(repository, stream)
	if err != nil {
		return Push{}, "", err
	}
	describe := func(p Push) string {
		return fmt.Sprintf("push %d, commit %s of pull request #%d in %s", p.Number, p.Commit, p.PullRequest, p.Upstream)
	}
	switch {
	case !found:
		return Push{}, "no push of this workstream is recorded, so there is no pull request commit to check", nil
	case named < 1:
		return Push{}, "name the push the request concerns: its number, as the notice gives it; the current one is " + describe(current), nil
	case named < current.Number:
		return Push{}, fmt.Sprintf("push %d is superseded: the current push is %s; a request acts only on the current push", named, describe(current)), nil
	case named > current.Number:
		return Push{}, fmt.Sprintf("push %d is not recorded: the current push is %s", named, describe(current)), nil
	}
	return current, "", nil
}

// ChecksPollResult is what an immediate read of a push's statuses found.
//
// Failed holds the failed checks of a failed result, each with its trace id,
// and Detail describes them with their `dagger trace` commands, or says that
// no failing check status was found.
type ChecksPollResult struct {
	Push    Push
	Outcome string
	Detail  string
	Failed  []api.CheckFailure
}

// PollChecksNow reads the combined statuses of the current push's commit at
// once, whatever the wait after the push or the poll interval, and makes the
// watch read them again at its next attempt instead of at the end of the
// interval. The read records nothing: the watch alone records results.
func (s *Controller) PollChecksNow(ctx context.Context, repository *trace.Repository, stream config.WorkstreamID, named int) (ChecksPollResult, string, error) {
	push, reason, err := pushNamed(repository, stream, named)
	if err != nil || reason != "" {
		return ChecksPollResult{}, reason, err
	}
	reader := s.statusReader()
	if reader == nil {
		return ChecksPollResult{}, "the service has no GitHub status client, so the statuses cannot be read", nil
	}
	forgetPoll(repository, stream, push.Number)
	result, _, err := checkrun.ReadCloudStatuses(ctx, reader, push.Upstream, push.Commit)
	repository.WakeWorkflow()
	if ctx.Err() != nil {
		return ChecksPollResult{}, "", ctx.Err()
	}
	if err != nil {
		return ChecksPollResult{Push: push, Outcome: "unreadable", Detail: checkrun.SafeText(err.Error())}, "", nil
	}
	detail := checkrun.SafeText(result.Detail)
	if detail == "" {
		detail = describeOutcome(result)
	}
	failed := checkrun.CloudFailures(result)
	if result.IsFailure() {
		var counts *api.CheckCounts
		if result.Counts != nil {
			counts = &api.CheckCounts{Total: result.Counts.Total, Passed: result.Counts.Passed, Failed: result.Counts.Failed, Cancelled: result.Counts.Cancelled}
		}
		detail += "\n" + checkrun.DescribeFailedChecks(failed, counts, result.StatusURL, true)
	}
	return ChecksPollResult{Push: push, Outcome: string(result.Outcome), Detail: detail, Failed: failed}, "", nil
}

func describeOutcome(result checkrun.CloudResult) string {
	switch result.Outcome {
	case checkrun.CloudLoadMissing:
		return "Dagger Cloud has posted no `load` status for the commit"
	case checkrun.CloudLoadFailed:
		return "the `load` status failed: the module did not load"
	case checkrun.CloudPending:
		return fmt.Sprintf("`load` ran and the %q status is pending", checkrun.CloudRunContext)
	case checkrun.CloudPassed:
		return "the run completed and every check passed"
	case checkrun.CloudIncomplete:
		return "the run completed without a result for every check"
	case checkrun.CloudFailed:
		return "the run completed and a check failed"
	}
	return string(result.Outcome)
}

func forgetPoll(repository *trace.Repository, stream config.WorkstreamID, push int) {
	polled.Range(func(key, _ any) bool {
		if k, ok := key.(pollKey); ok && k.stream == stream && k.push == push && k.repository.Project() == repository.Project() {
			polled.Delete(key)
		}
		return true
	})
}

// ChecksRerunRecord is the document final/checks-push-<n>-request-<m>.json:
// the m-th rerun of every check of push n the chief of staff asked for,
// recorded before the command runs.
type ChecksRerunRecord struct {
	Push         int       `json:"push"`
	PullRequest  int       `json:"pull_request"`
	PushedCommit string    `json:"pushed_commit"`
	Command      []string  `json:"command"`
	RequestedAt  time.Time `json:"requested_at"`
	FinishedAt   time.Time `json:"finished_at,omitzero"`
	Error        string    `json:"error,omitempty"`
}

func checksRerunDocument(push, n int) string {
	return fmt.Sprintf("final-checks-push-%d-request-%d", push, n)
}

func checksRerunPath(push, n int) string {
	return fmt.Sprintf("final/checks-push-%d-request-%d.json", push, n)
}

// RerunChecksNow starts a Dagger Cloud rerun of every check of the current
// push's pull request, from the workstream's checkout and with the token
// cloud_api_token_env names. The request is recorded before the command runs.
func (s *Controller) RerunChecksNow(ctx context.Context, repository *trace.Repository, stream config.WorkstreamID, named int) (ChecksRerunRecord, string, error) {
	push, reason, err := pushNamed(repository, stream, named)
	if err != nil || reason != "" {
		return ChecksRerunRecord{}, reason, err
	}
	rerunner := s.CloudRerun
	if rerunner == nil {
		return ChecksRerunRecord{}, "the service has no Dagger Cloud rerun runner", nil
	}
	if err := rerunner.CanRerun(); err != nil {
		return ChecksRerunRecord{}, err.Error() + "; set the environment variable dagger.cloud_api_token_env names", nil
	}
	cfg := s.About(repository)
	if cfg == nil {
		return ChecksRerunRecord{}, "", errors.New("the project's configuration is unavailable")
	}
	docs, err := trace.Read[trace.Document](repository, stream)
	if err != nil {
		return ChecksRerunRecord{}, "", err
	}
	n := 1
	for _, d := range docs {
		if strings.HasPrefix(d.ID, fmt.Sprintf("final-checks-push-%d-request-", push.Number)) {
			n++
		}
	}
	org := rerunOrganization(cfg)
	record := ChecksRerunRecord{Push: push.Number, PullRequest: push.PullRequest, PushedCommit: push.Commit, Command: append([]string{"dagger"}, checkrun.RerunAllCommand(push.PullRequest, org)...), RequestedAt: s.Now()}
	write := func() error {
		data, err := json.MarshalIndent(record, "", "  ")
		if err != nil {
			return err
		}
		id := checksRerunDocument(push.Number, n)
		revision, err := nextRevision(repository, stream, id)
		if err != nil {
			return err
		}
		doc := trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: id, Revision: revision, Project: repository.Project(), Workstream: stream, At: s.Now(), Actor: chiefActor, Cause: ChecksToolName},
			Path: checksRerunPath(push.Number, n), Content: string(data) + "\n"}
		return repository.RecordDocuments(context.WithoutCancel(ctx), []trace.Document{doc})
	}
	if err := write(); err != nil {
		return ChecksRerunRecord{}, "", err
	}
	if err := s.Step("final-checks-rerun-all-requested"); err != nil {
		return ChecksRerunRecord{}, "", err
	}
	runErr := rerunner.RerunAll(ctx, s.rerunDirectory(ctx, cfg, repository, stream), push.PullRequest, org)
	if ctx.Err() != nil {
		return ChecksRerunRecord{}, "", ctx.Err()
	}
	record.FinishedAt = s.Now()
	if runErr != nil {
		record.Error = runErr.Error()
	}
	if err := write(); err != nil {
		return ChecksRerunRecord{}, "", err
	}
	forgetPoll(repository, stream, push.Number)
	repository.WakeWorkflow()
	return record, "", nil
}

func rerunOrganization(cfg *config.Config) string {
	if cfg == nil || cfg.Project.Fork != "" {
		return ""
	}
	return cfg.Dagger.Organization
}

func (s *Controller) rerunDirectory(ctx context.Context, cfg *config.Config, repository *trace.Repository, stream config.WorkstreamID) string {
	dir := cfg.Project.Clone
	if g, err := featureWorkspaces(cfg, repository).Of(stream); err == nil {
		if checkout, found, err := g.Workspace(ctx, string(stream)); err == nil && found && checkout.Path != "" {
			dir = checkout.Path
		}
	}
	return dir
}

// OfferChecksChoice records that the owner is asked whether the final review
// of the current push keeps waiting for check results or continues without
// them, and returns the offered record. It refuses, recording nothing, with a
// reason when the named push is not the current one, the workstream is not
// assembled, the push already has a completed result or a choice to continue,
// or an offer for it waits for the owner. note is what the chief of staff
// found.
func (s *Controller) OfferChecksChoice(ctx context.Context, repository *trace.Repository, stream config.WorkstreamID, named int, note, turn string) (ChecksChoiceRecord, string, error) {
	push, reason, err := pushNamed(repository, stream, named)
	if err != nil || reason != "" {
		return ChecksChoiceRecord{}, reason, err
	}
	if err := s.closeObsoleteChecksChoices(ctx, repository, stream); err != nil {
		return ChecksChoiceRecord{}, "", err
	}
	feature, err := repository.Workflow(stream, trace.FeatureSubject)
	if err != nil {
		return ChecksChoiceRecord{}, "", err
	}
	if feature.Value != AssembledState {
		return ChecksChoiceRecord{}, "the workstream is " + feature.Value + "; the final review it would continue exists only while it is assembled", nil
	}
	if run, done, err := completedChecksOf(repository, stream, push); err != nil {
		return ChecksChoiceRecord{}, "", err
	} else if done {
		return ChecksChoiceRecord{}, fmt.Sprintf("push %d already has its check outcome (%s); there is nothing to wait for", push.Number, run.Status), nil
	}
	note = strings.TrimSpace(note)
	if note == "" {
		return ChecksChoiceRecord{}, "a note is required: say what you found, which the owner reads", nil
	}
	choice := ChecksChoice{Push: push.Number, Operation: push.Operation, PullRequest: push.PullRequest, PushedCommit: push.Commit, State: ChecksChoiceOffered, Note: note, OfferedAt: s.Now(), At: s.Now()}
	var refusal string
	revision, written, err := s.writeChecksChoice(ctx, repository, stream, choice, chiefActor, turn, func(latest ChecksChoiceRecord, found bool) bool {
		switch {
		case found && latest.State == ChecksChoiceOffered:
			refusal = fmt.Sprintf("the owner is already asked about push %d and has not answered; the entry stays in the inbox", push.Number)
		case found && latest.State == ChecksChoiceContinue:
			refusal = fmt.Sprintf("the owner already chose to continue without check results for push %d", push.Number)
		}
		return refusal == ""
	})
	if err != nil || !written {
		return ChecksChoiceRecord{}, refusal, err
	}
	return ChecksChoiceRecord{ChecksChoice: choice, Revision: revision}, "", nil
}

// AnswerChecksChoice records the owner's answer to a checks-choice entry. The pin
// the request carries must be the workstream's current push and the offer
// still waiting for an answer; an answer about a superseded push, or one the
// entry no longer offers, is a conflict and changes nothing. Keep waiting
// leaves the watch as it is. Continue lets the final review of that push's
// commit start without check results, and of no other push's.
func (s *Controller) AnswerChecksChoice(ctx context.Context, raw string, req ChecksChoiceRequest) (ChecksChoiceResponse, *APIError) {
	if req.Decision != ChecksChoiceKeepWaiting && req.Decision != ChecksChoiceContinue {
		return ChecksChoiceResponse{}, &APIError{Code: Validation, Message: "decision is keep_waiting or continue"}
	}
	project, stream, repository, apiErr := s.ConversationTrace(raw)
	if apiErr != nil {
		return ChecksChoiceResponse{}, apiErr
	}
	internal := func(err error) (ChecksChoiceResponse, *APIError) {
		return ChecksChoiceResponse{}, &APIError{Code: Internal, Message: "cannot record the answer: " + err.Error()}
	}
	if err := s.closeObsoleteChecksChoices(ctx, repository, stream); err != nil {
		return internal(err)
	}
	current, found, err := CurrentPush(repository, stream)
	if err != nil {
		return internal(err)
	}
	stale := func(why string) (ChecksChoiceResponse, *APIError) {
		return ChecksChoiceResponse{}, &APIError{Code: Conflict, Message: why + "; read the inbox again"}
	}
	switch {
	case !found:
		return stale("the workstream has no push")
	case req.Push != current.Number:
		return stale(fmt.Sprintf("the question concerned push %d, which push %d now supersedes: the answer would not apply to it", req.Push, current.Number))
	case req.PullRequest != current.PullRequest || req.PushedCommit != current.Commit || req.Operation != current.Operation:
		return stale(fmt.Sprintf("the question concerned commit %s of pull request #%d, not push %d as it is recorded now", req.PushedCommit, req.PullRequest, current.Number))
	}
	record, recorded, err := ReadChecksChoice(repository, stream, current.Number)
	if err != nil {
		return internal(err)
	}
	if !recorded || !record.Concerns(current) {
		return stale(fmt.Sprintf("no question about push %d is open", current.Number))
	}
	if record.State == req.Decision && record.Offer == req.Offer && req.Offer > 0 {
		return ChecksChoiceResponse{Project: project, Workstream: stream, Push: current.Number, PullRequest: current.PullRequest, PushedCommit: current.Commit, Decision: req.Decision, Detail: "already recorded", At: record.At}, nil
	}
	if record.State != ChecksChoiceOffered || record.Revision != req.Offer {
		return stale(fmt.Sprintf("the question about push %d is no longer open (%s)", current.Number, record.State))
	}
	if run, done, err := completedChecksOf(repository, stream, current); err != nil {
		return internal(err)
	} else if done {
		return stale(fmt.Sprintf("push %d already has its check outcome (%s)", current.Number, run.Status))
	}
	answer := record.ChecksChoice
	answer.State, answer.Offer, answer.At = req.Decision, record.Revision, s.Now()
	var refusal string
	if _, written, err := s.writeChecksChoice(ctx, repository, stream, answer, ownerActor, "inbox", func(latest ChecksChoiceRecord, found bool) bool {
		if !found || latest.State != ChecksChoiceOffered || latest.Revision != record.Revision {
			refusal = "the question about push " + fmt.Sprint(current.Number) + " changed"
		}
		return refusal == ""
	}); err != nil {
		return internal(err)
	} else if !written {
		return stale(refusal)
	}
	repository.WakeWorkflow()
	detail := "the watch keeps polling the pull request's statuses; this question is closed"
	if req.Decision == ChecksChoiceContinue {
		detail = fmt.Sprintf("the final review of push %d starts without check results; a later push waits for its own", current.Number)
	}
	return ChecksChoiceResponse{Project: project, Workstream: stream, Push: current.Number, PullRequest: current.PullRequest, PushedCommit: current.Commit, Decision: req.Decision, Detail: detail, At: answer.At}, nil
}

// ChecksChoiceEntry is the owner's choice about the current push while the
// chief of staff's offer waits for an answer and the push has no check
// outcome. A push that supersedes the one it concerns closes it, as does a
// completed result or an answer.
func ChecksChoiceEntry(repository *trace.Repository, w trace.WorkstreamStatus) (InboxEntry, bool, error) {
	stream := w.Workstream
	current, found, err := CurrentPush(repository, stream)
	if err != nil || !found {
		return InboxEntry{}, false, err
	}
	record, recorded, err := ReadChecksChoice(repository, stream, current.Number)
	if err != nil || !recorded || record.State != ChecksChoiceOffered || !record.Concerns(current) {
		return InboxEntry{}, false, err
	}
	if _, done, err := completedChecksOf(repository, stream, current); err != nil || done {
		return InboxEntry{}, false, err
	}
	body := map[string]any{"push": current.Number, "pull_request": current.PullRequest, "pushed_commit": current.Commit, "operation": current.Operation, "offer": record.Revision}
	e := decision(InboxChecksChoice, stream, record.OfferedAt, "/checks-choice/"+string(stream), body)
	e.Revision = record.Revision
	e.Options = []string{ChecksChoiceKeepWaiting, ChecksChoiceContinue}
	e.Question = fmt.Sprintf("No check results are available for push %d, commit %s of pull request #%d in %s. The chief of staff found: %s Keep waiting for the checks, or continue the final review without check results? Continuing applies to this push only: a later push waits for its own results.", current.Number, current.Commit, current.PullRequest, current.Upstream, strings.TrimSpace(record.Note))
	e.Blocked = "The final review of the assembled branch. It starts when the checks of this push complete, or when you choose to continue without them; the service keeps reading the pull request's check statuses meanwhile, and this entry closes once a run completes."
	return e, true, nil
}

func (w *finalWatch) continuedChecks(ctx context.Context, choice ChecksChoiceRecord) (FinalCheckRun, error) {
	run := w.base(checkrun.CloudResult{})
	run.Status, run.ExitCode = ChecksContinued, -1
	run.ContinuedBy, run.Choice = "owner", choice.Revision
	run.Error = fmt.Sprintf("no check results exist for commit %s of pull request #%d (push %d); the owner chose to continue the final review without them", w.push.Commit, w.push.PullRequest, w.push.Number)
	run.StartedAt = choice.At
	if err := w.write(ctx, &run); err != nil {
		return FinalCheckRun{}, err
	}
	return run, nil
}

func chiefChecksTarget(load func() *Controller, repository *trace.Repository, scope coreadapter.Scope, name string, raw json.RawMessage, in any, workstream func() config.WorkstreamID) (*Controller, *trace.Repository, config.WorkstreamID, json.RawMessage, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(in); err != nil {
		return nil, nil, "", nil, fmt.Errorf("tool input: %w", err)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return nil, nil, "", nil, errors.New("tool input must be one object")
	}
	s := load()
	if s == nil {
		return nil, nil, "", nil, errors.New(name + " is unavailable")
	}
	if err := repository.ActiveTurn(scope); err != nil {
		return nil, nil, "", nil, err
	}
	if scope.Role != trace.Manager {
		out, err := refusal(name + " is only available to the chief of staff")
		return nil, nil, "", out, err
	}
	target, stream, why, err := repository.ManagerMember(config.WorkstreamID(scope.Workstream), workstream())
	if err != nil {
		return nil, nil, "", nil, err
	}
	if why != "" {
		out, err := refusal(why)
		return nil, nil, "", out, err
	}
	return s, target, stream, nil, nil
}

type pollFailedCheck struct {
	Name      string `json:"name"`
	TraceID   string `json:"trace_id,omitempty"`
	Command   string `json:"command,omitempty"`
	StatusURL string `json:"status_url,omitempty"`
}

// ChecksToolOf returns the pull_request_checks tool of one claimed
// chief-of-staff turn, over the controller load returns when the tool is
// called. It asks the watch of the workstream's current push to read the
// pushed commit's statuses now, or asks Dagger Cloud to rerun every check of
// the push's pull request. A request naming any push but the current one is
// refused, naming the current one. A call the service refuses is an ordinary
// result, {"done":false,"reason":...}, that does nothing.
func ChecksToolOf(load func() *Controller, repository *trace.Repository, scope coreadapter.Scope) coreadapter.Tool {
	tool := coreadapter.Tool{Name: ChecksToolName, Effect: coreadapter.ToolMemory,
		Description: "Act on the Dagger Cloud checks of the workstream's current push, the one the load-never-ran notice names. action poll: read the combined statuses of the pushed commit now and say what they show; the watch then reads them again at once instead of waiting for pull_request_poll. action rerun: ask Dagger Cloud to rerun every check of the pull request (`dagger cloud checks rerun --all --pr <N>`, from the workstream's checkout). push: the push number the notice gives; a request naming a superseded push is refused and names the current one. workstream: a member workstream ID of your workgroup to act on; omit for this conversation's own workstream.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"action":{"type":"string","enum":["poll","rerun"]},"push":{"type":"integer","minimum":1},"workstream":{"type":"string"}},"required":["action","push"],"additionalProperties":false}`)}
	tool.Handle = func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
		var in struct {
			Action     string              `json:"action"`
			Push       int                 `json:"push"`
			Workstream config.WorkstreamID `json:"workstream"`
		}
		s, target, stream, refused, err := chiefChecksTarget(load, repository, scope, ChecksToolName, raw, &in, func() config.WorkstreamID { return in.Workstream })
		if refused != nil || err != nil {
			return refused, err
		}
		switch in.Action {
		case "poll":
			result, reason, err := s.PollChecksNow(ctx, target, stream, in.Push)
			if err != nil {
				return nil, err
			}
			if reason != "" {
				return refusal(reason)
			}
			failed := make([]pollFailedCheck, 0, len(result.Failed))
			for _, f := range result.Failed {
				failed = append(failed, pollFailedCheck{Name: checkrun.SafeText(f.Name), TraceID: f.TraceID, Command: checkrun.TraceCommand(f.TraceID), StatusURL: checkrun.SafeLink(f.TargetURL)})
			}
			return json.Marshal(struct {
				Done         bool              `json:"done"`
				Push         int               `json:"push"`
				PullRequest  int               `json:"pull_request"`
				Commit       string            `json:"commit"`
				Outcome      string            `json:"outcome"`
				Detail       string            `json:"detail"`
				FailedChecks []pollFailedCheck `json:"failed_checks,omitempty"`
			}{true, result.Push.Number, result.Push.PullRequest, result.Push.Commit, result.Outcome, result.Detail, failed})
		case "rerun":
			record, reason, err := s.RerunChecksNow(ctx, target, stream, in.Push)
			if err != nil {
				return nil, err
			}
			if reason != "" {
				return refusal(reason)
			}
			out := struct {
				Done        bool     `json:"done"`
				Push        int      `json:"push"`
				PullRequest int      `json:"pull_request"`
				Command     []string `json:"command"`
				Error       string   `json:"error,omitempty"`
				Next        string   `json:"next"`
			}{true, record.Push, record.PullRequest, record.Command, record.Error, "The watch keeps reading the statuses of the pushed commit; the rerun's postings appear there."}
			if record.Error != "" {
				out.Done, out.Next = false, "The request failed; nothing was rerun."
			}
			return json.Marshal(out)
		}
		return refusal("action is poll or rerun")
	}
	return tool
}

// ChecksChoiceToolOf returns the ask_owner_about_checks tool of one claimed
// chief-of-staff turn. It brings the owner the choice to keep waiting for the
// current push's check results or to continue the final review without them;
// only the owner's answer, through the inbox, continues. A call naming any
// push but the current one is refused, naming the current one.
func ChecksChoiceToolOf(load func() *Controller, repository *trace.Repository, scope coreadapter.Scope) coreadapter.Tool {
	tool := coreadapter.Tool{Name: ChecksChoiceToolName, Effect: coreadapter.ToolMemory,
		Description: "Ask the owner whether the final review keeps waiting for the Dagger Cloud check results of the workstream's current push or continues without check results, after a fresh poll or a rerun did not help or the project has no Cloud Checks. The owner sees an inbox entry with both answers; only the owner's answer continues, and it applies to this push only: a later push closes the question. push: the push number the load-never-ran notice gives; a call naming a superseded push is refused and names the current one. note: what you found, which the owner reads. workstream: a member workstream ID of your workgroup to act on; omit for this conversation's own workstream.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"push":{"type":"integer","minimum":1},"note":{"type":"string"},"workstream":{"type":"string"}},"required":["push","note"],"additionalProperties":false}`)}
	tool.Handle = func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
		var in struct {
			Push       int                 `json:"push"`
			Note       string              `json:"note"`
			Workstream config.WorkstreamID `json:"workstream"`
		}
		s, target, stream, refused, err := chiefChecksTarget(load, repository, scope, ChecksChoiceToolName, raw, &in, func() config.WorkstreamID { return in.Workstream })
		if refused != nil || err != nil {
			return refused, err
		}
		record, reason, err := s.OfferChecksChoice(ctx, target, stream, in.Push, in.Note, scope.Turn)
		if err != nil {
			return nil, err
		}
		if reason != "" {
			return refusal(reason)
		}
		return json.Marshal(struct {
			Done        bool   `json:"done"`
			Push        int    `json:"push"`
			PullRequest int    `json:"pull_request"`
			Commit      string `json:"commit"`
			Offer       int    `json:"offer"`
			Next        string `json:"next"`
		}{true, record.Push, record.PullRequest, record.PushedCommit, record.Revision, "The owner sees the choice in the inbox. The service keeps watching the statuses meanwhile; a completed result closes the question."})
	}
	return tool
}

func refusal(reason string) (json.RawMessage, error) {
	return json.Marshal(struct {
		Done   bool   `json:"done"`
		Reason string `json:"reason"`
	}{false, reason})
}
