package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	checkrun "github.com/kpenfound/osmia/internal/checks"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/kpenfound/osmia/internal/workspace"
)

func sessionGit(cfg *config.Config) *workspace.Git {
	return workspaces(cfg, sessionsDirectory, config.WorkspacesGit).(*workspace.Git)
}

func sessionEnded(repository *trace.Repository, stream config.WorkstreamID) (bool, error) {
	state, err := repository.Workflow(stream, trace.SessionSubject)
	if err != nil {
		return false, err
	}
	return state.Value == trace.SessionEnded, nil
}

func sessionThreadBusy(repository *trace.Repository, stream config.WorkstreamID) (bool, error) {
	th, err := repository.Thread(stream, sessionAgentID)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	state, err := repository.Workflow(stream, trace.SessionSubject)
	return unfinishedSessionTurn(th) || state.Value == trace.SessionWorking, err
}

func (s *Service) sessionChanges(ctx context.Context, raw string) (SessionChangesResponse, *APIError) {
	project, stream, _, api := s.sessionTrace(raw)
	if api != nil {
		return SessionChangesResponse{}, api
	}
	view, api := s.sessionRead(string(stream))
	if api != nil {
		return SessionChangesResponse{}, api
	}
	cfg := s.current().For(project)
	g := sessionGit(cfg)
	head, exists, err := g.Branch(ctx, sessionBranch(stream))
	if err != nil {
		return SessionChangesResponse{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot read the branch of session %s; check the trace repository", stream)}
	}
	if !exists {
		return SessionChangesResponse{}, &APIError{Code: Internal, Message: fmt.Sprintf("session %s has no branch", stream)}
	}
	changes := ""
	if wt, found, err := g.Workspace(ctx, string(stream)); err != nil {
		return SessionChangesResponse{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot read the worktree of session %s; check the workspace directory", stream)}
	} else if found {
		diff, err := g.WorktreeDiff(ctx, wt)
		if err != nil {
			return SessionChangesResponse{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot diff the worktree of session %s: %v", stream, err)}
		}
		changes = diff
	}
	messages, err := g.Log(ctx, view.Base, head)
	if err != nil {
		return SessionChangesResponse{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot read the commits of session %s: %v", stream, err)}
	}
	commits := make([]SessionCommitSummary, len(messages))
	for i, m := range messages {
		commits[i] = SessionCommitSummary{Commit: m.Commit, Message: sessionChangesMessage(m.Message)}
	}
	return SessionChangesResponse{Head: head, Changes: changes, Commits: commits}, nil
}

const sessionCommitTrailer = "Osmia-Session-Request"

func sessionCommitMessage(message, key string) string {
	return strings.TrimRight(message, "\n") + "\n\n" + sessionCommitTrailer + ": " + key + "\n"
}

func sessionCommitKey(message string) string {
	for _, line := range strings.Split(message, "\n") {
		if k, ok := strings.CutPrefix(line, sessionCommitTrailer+": "); ok {
			return k
		}
	}
	return ""
}

func sessionChangesMessage(message string) string {
	body, _, found := strings.Cut(message, "\n\n"+sessionCommitTrailer+": ")
	if !found {
		return message
	}
	return body
}

var sessionCommitLocks sync.Map

func sessionCommitLock(repository *trace.Repository, stream config.WorkstreamID) *sync.Mutex {
	v, _ := sessionCommitLocks.LoadOrStore(sessionLockKey{repository, stream}, &sync.Mutex{})
	return v.(*sync.Mutex)
}

func (s *Service) sessionCommit(ctx context.Context, raw string, req SessionCommitRequest) (SessionCommitResponse, *APIError) {
	project, stream, repository, api := s.sessionTrace(raw)
	if api != nil {
		return SessionCommitResponse{}, api
	}
	if err := s.step("session-commit-looked-up"); err != nil {
		return SessionCommitResponse{}, &APIError{Code: Internal, Message: err.Error()}
	}
	message := strings.TrimSpace(req.Message)
	if message == "" {
		return SessionCommitResponse{}, &APIError{Code: Validation, Message: "message must not be empty"}
	}
	key := strings.TrimSpace(req.RequestKey)
	if key == "" {
		return SessionCommitResponse{}, &APIError{Code: Validation, Message: "request_key must not be empty"}
	}
	lock := sessionLock(repository, stream)
	if !lock.TryLock() {
		return SessionCommitResponse{}, &APIError{Code: Conflict, Message: "a message is being started for this session; try again once it finishes"}
	}
	defer lock.Unlock()
	if _, _, _, api := s.sessionTrace(raw); api != nil {
		return SessionCommitResponse{}, api
	}
	if ended, err := sessionEnded(repository, stream); err != nil {
		return SessionCommitResponse{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot read the state of session %s; check the trace repository", stream)}
	} else if ended {
		return SessionCommitResponse{}, &APIError{Code: Conflict, Message: "the session has ended"}
	}
	if busy, err := sessionThreadBusy(repository, stream); err != nil {
		return SessionCommitResponse{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot read the thread of session %s; check the trace repository", stream)}
	} else if busy {
		return SessionCommitResponse{}, &APIError{Code: Conflict, Message: "a turn is running for this session; try again once it finishes"}
	}
	mu := sessionCommitLock(repository, stream)
	mu.Lock()
	defer mu.Unlock()
	cfg := s.current().For(project)
	g := sessionGit(cfg)
	branch := sessionBranch(stream)
	head, exists, err := g.Branch(ctx, branch)
	if err != nil {
		return SessionCommitResponse{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot read the branch of session %s; check the trace repository", stream)}
	}
	if !exists {
		return SessionCommitResponse{}, &APIError{Code: Internal, Message: fmt.Sprintf("session %s has no branch", stream)}
	}
	c, err := g.Commit(ctx, head)
	if err != nil {
		return SessionCommitResponse{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot read the head commit of session %s: %v", stream, err)}
	}
	commit := head
	if sessionCommitKey(c.Message) != key {
		wt, found, err := g.Workspace(ctx, string(stream))
		if err != nil {
			return SessionCommitResponse{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot read the worktree of session %s; check the workspace directory", stream)}
		}
		if !found {
			return SessionCommitResponse{}, &APIError{Code: Internal, Message: fmt.Sprintf("session %s has no worktree", stream)}
		}
		changed := false
		if commit, changed, err = g.CommitChanges(ctx, wt, sessionCommitMessage(message, key), s.now()); err != nil {
			return SessionCommitResponse{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot commit the worktree of session %s: %v", stream, err)}
		}
		if !changed {
			return SessionCommitResponse{}, &APIError{Code: Validation, Message: "the worktree has no changes to commit"}
		}
	}
	summary := fmt.Sprintf("Committed %s: %s", commit, strings.SplitN(message, "\n", 2)[0])
	if err := recordSessionEntry(ctx, repository, project, stream, "commit-"+key, s.now(), summary); err != nil {
		return SessionCommitResponse{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot record the commit entry of session %s; check the trace repository", stream)}
	}
	return SessionCommitResponse{Commit: commit}, nil
}

const sessionChecksRole = "session-checks"
const sessionChecksThread = "session-checks"

func sessionCheckDocumentID(run int) string { return fmt.Sprintf("session-check-%d", run) }

func sessionCheckRecordPath(run int) string { return fmt.Sprintf("session-checks/checks-%d.json", run) }

var sessionCheckRecordPattern = regexp.MustCompile(`^session-checks/checks-[0-9]+\.json$`)

func sessionCheckRuns(repo *trace.Repository, stream config.WorkstreamID) ([]SessionCheckRun, error) {
	docs, err := trace.Read[trace.Document](repo, stream)
	if err != nil {
		return nil, err
	}
	latest := map[string]trace.Document{}
	var order []string
	for _, d := range docs {
		if !sessionCheckRecordPattern.MatchString(d.Path) {
			continue
		}
		if _, seen := latest[d.ID]; !seen {
			order = append(order, d.ID)
		}
		if d.Revision > latest[d.ID].Revision {
			latest[d.ID] = d
		}
	}
	runs := make([]SessionCheckRun, 0, len(order))
	for _, id := range order {
		var run SessionCheckRun
		if err := json.Unmarshal([]byte(latest[id].Content), &run); err != nil {
			return nil, fmt.Errorf("%s: %w", latest[id].Path, err)
		}
		runs = append(runs, run)
	}
	return runs, nil
}

// SessionCheckStart is the unfinished evidence of a session check run: the
// link Dagger printed while the run was still going, recorded under the run's
// number before any result exists. It is not a result; only the run's
// SessionCheckRun establishes that it finished. Each revision holds the
// latest link the run printed.
type SessionCheckStart struct {
	Run       int             `json:"run"`
	Commit    string          `json:"commit"`
	Kind      string          `json:"kind"`
	Selection *CheckSelection `json:"selection,omitempty"`
	TraceURL  string          `json:"trace_url"`
	StartedAt time.Time       `json:"started_at"`
}

func sessionCheckStartDocumentID(run int) string {
	return sessionCheckDocumentID(run) + "-started"
}

func sessionCheckStartPath(run int) string {
	return fmt.Sprintf("session-checks/checks-%d-started.json", run)
}

var sessionCheckStartPattern = regexp.MustCompile(`^session-checks/checks-[0-9]+-started\.json$`)

func sessionCheckStarts(repo *trace.Repository, stream config.WorkstreamID) ([]SessionCheckStart, error) {
	docs, err := trace.Read[trace.Document](repo, stream)
	if err != nil {
		return nil, err
	}
	latest := map[string]trace.Document{}
	var order []string
	for _, d := range docs {
		if !sessionCheckStartPattern.MatchString(d.Path) {
			continue
		}
		if _, seen := latest[d.ID]; !seen {
			order = append(order, d.ID)
		}
		if d.Revision > latest[d.ID].Revision {
			latest[d.ID] = d
		}
	}
	starts := make([]SessionCheckStart, 0, len(order))
	for _, id := range order {
		var start SessionCheckStart
		if err := json.Unmarshal([]byte(latest[id].Content), &start); err != nil {
			return nil, fmt.Errorf("%s: %w", latest[id].Path, err)
		}
		starts = append(starts, start)
	}
	return starts, nil
}

func nextSessionCheckRun(repo *trace.Repository, stream config.WorkstreamID) (int, error) {
	runs, err := sessionCheckRuns(repo, stream)
	if err != nil {
		return 0, err
	}
	starts, err := sessionCheckStarts(repo, stream)
	if err != nil {
		return 0, err
	}
	run := 0
	for _, r := range runs {
		run = max(run, r.Run)
	}
	for _, st := range starts {
		run = max(run, st.Run)
	}
	return run + 1, nil
}

func sessionCheckKind(sel CheckSelection) string {
	if sel.Mode == checkrun.SelectionSelected {
		return "smart"
	}
	return "full"
}

func recordSessionCheckRun(ctx context.Context, repository *trace.Repository, project config.ProjectID, stream config.WorkstreamID, run SessionCheckRun, captured string) error {
	data, err := json.MarshalIndent(run, "", "  ")
	if err != nil {
		return err
	}
	id := sessionCheckDocumentID(run.Run)
	h := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: id, Revision: 1, Project: project, Workstream: stream, At: run.FinishedAt, Actor: ownerActor, Cause: id}
	docs := []trace.Document{{Header: h, Path: sessionCheckRecordPath(run.Run), Content: string(data) + "\n"}}
	if run.OutputPath != "" {
		oh := h
		oh.ID += "-output"
		docs = append(docs, trace.Document{Header: oh, Path: run.OutputPath, Content: captured})
	}
	return repository.RecordDocuments(ctx, docs)
}

type sessionTraceEvidence struct {
	ctx        context.Context
	repository *trace.Repository
	project    config.ProjectID
	stream     config.WorkstreamID
	start      SessionCheckStart
	now        func() time.Time
	log        *slog.Logger

	mu      sync.Mutex
	pending string
	latest  string
	wake    chan struct{}
	done    chan struct{}
}

func newSessionTraceEvidence(ctx context.Context, repository *trace.Repository, project config.ProjectID, stream config.WorkstreamID, start SessionCheckStart, now func() time.Time, log *slog.Logger) *sessionTraceEvidence {
	e := &sessionTraceEvidence{ctx: context.WithoutCancel(ctx), repository: repository, project: project, stream: stream, start: start, now: now, log: log, wake: make(chan struct{}, 1), done: make(chan struct{})}
	go e.record()
	return e
}

func (e *sessionTraceEvidence) see(url string) {
	e.mu.Lock()
	e.pending, e.latest = url, url
	e.mu.Unlock()
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

func (e *sessionTraceEvidence) record() {
	defer close(e.done)
	revision, written := 0, ""
	for range e.wake {
		e.mu.Lock()
		url := e.pending
		e.mu.Unlock()
		if url == "" || url == written {
			continue
		}
		start := e.start
		start.TraceURL = url
		data, err := json.MarshalIndent(start, "", "  ")
		if err != nil {
			continue
		}
		id := sessionCheckStartDocumentID(start.Run)
		h := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: id, Revision: revision + 1, Project: e.project, Workstream: e.stream, At: e.now(), Actor: ownerActor, Cause: sessionCheckDocumentID(start.Run)}
		doc := trace.Document{Header: h, Path: sessionCheckStartPath(start.Run), Content: string(data) + "\n"}
		if err := e.repository.RecordDocuments(e.ctx, []trace.Document{doc}); err != nil {
			e.log.Warn("session check trace link not recorded", "workstream", string(e.stream), "run", start.Run, "error", err.Error())
			continue
		}
		revision, written = revision+1, url
	}
}

func (e *sessionTraceEvidence) finish() string {
	close(e.wake)
	<-e.done
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.latest
}

var sessionCheckLocks sync.Map

func sessionCheckLock(repository *trace.Repository, stream config.WorkstreamID) *sync.Mutex {
	v, _ := sessionCheckLocks.LoadOrStore(sessionLockKey{repository, stream}, &sync.Mutex{})
	return v.(*sync.Mutex)
}

func (s *Service) sessionCheck(ctx context.Context, raw string) (SessionCheckRun, *APIError) {
	project, stream, repository, api := s.sessionTrace(raw)
	if api != nil {
		return SessionCheckRun{}, api
	}
	if err := s.step("session-check-looked-up"); err != nil {
		return SessionCheckRun{}, &APIError{Code: Internal, Message: err.Error()}
	}
	mu := sessionCheckLock(repository, stream)
	if !mu.TryLock() {
		return SessionCheckRun{}, &APIError{Code: Conflict, Message: "a check is already running for this session; try again once it finishes"}
	}
	defer mu.Unlock()
	lock := sessionLock(repository, stream)
	if !lock.TryLock() {
		return SessionCheckRun{}, &APIError{Code: Conflict, Message: "the session is ending; try again once it finishes"}
	}
	if _, _, _, api := s.sessionTrace(raw); api != nil {
		lock.Unlock()
		return SessionCheckRun{}, api
	}
	lock.controls.Add(1)
	defer lock.controls.Add(-1)
	ended, err := sessionEnded(repository, stream)
	lock.Unlock()
	if err != nil {
		return SessionCheckRun{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot read the state of session %s; check the trace repository", stream)}
	}
	if ended {
		return SessionCheckRun{}, &APIError{Code: Conflict, Message: "the session has ended"}
	}
	view, api := s.sessionRead(string(stream))
	if api != nil {
		return SessionCheckRun{}, api
	}
	cfg := s.current().For(project)
	g := sessionGit(cfg)
	head, exists, err := g.Branch(ctx, sessionBranch(stream))
	if err != nil || !exists {
		return SessionCheckRun{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot read the branch of session %s; check the trace repository", stream)}
	}
	checks, cloud := s.configuredChecks(cfg), s.configuredCloudChecks(cfg)
	if checks == nil {
		return SessionCheckRun{}, &APIError{Code: Internal, Message: "the candidate check runner is unavailable"}
	}
	diff, err := g.Diff(ctx, view.Base, head)
	if err != nil {
		return SessionCheckRun{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot diff the session's base and head: %v", err)}
	}
	base := filepath.Join(cfg.Root.String(), "checks")
	if err := os.MkdirAll(base, 0700); err != nil {
		return SessionCheckRun{}, &APIError{Code: Internal, Message: err.Error()}
	}
	dir, err := os.MkdirTemp(base, "session-")
	if err != nil {
		return SessionCheckRun{}, &APIError{Code: Internal, Message: err.Error()}
	}
	defer os.RemoveAll(dir)
	if err := g.Export(ctx, head, dir); err != nil {
		return SessionCheckRun{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot export the session's head: %v", err)}
	}
	release, err := s.checkSlots.acquire(ctx, CheckSlotRun{Project: project, Workstream: stream, Session: true})
	if err != nil {
		return SessionCheckRun{}, &APIError{Code: Internal, Message: err.Error()}
	}
	defer release()
	run, err := nextSessionCheckRun(repository, stream)
	if err != nil {
		return SessionCheckRun{}, &APIError{Code: Internal, Message: err.Error()}
	}
	cause := sessionCheckDocumentID(run)
	startedAt := s.now()
	scope := coreadapter.Scope{Project: string(project), Workstream: string(stream), Thread: sessionChecksThread, Turn: cause, Role: sessionChecksRole}
	identity := checkrun.Identity{Candidate: head, Branch: sessionBranch(stream), Remote: "https://github.com/" + cfg.Project.Upstream, Repository: cfg.Project.Clone}
	selection := checkrun.Select(ctx, s.jev, repository, scope, cause, nil, dir, diff, identity, checks)
	timeout := s.about(repository).Project.CheckTimeout()
	var remembered checkrun.Remembered
	if cloud != nil {
		if remembered, err = checkrun.RememberedIn(repository, stream); err != nil {
			return SessionCheckRun{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot read the check runs of session %s; check the trace repository", stream)}
		}
	}
	chosen := selection
	evidence := newSessionTraceEvidence(ctx, repository, project, stream, SessionCheckStart{Run: run, Commit: head, Kind: sessionCheckKind(chosen), Selection: &chosen, StartedAt: startedAt}, s.now, s.log())
	result, selection, engines, checkErr := checkrun.RunSelectionWithFallback(checkrun.WithTraceObserver(ctx, evidence.see), checks, cloud, timeout, remembered, dir, identity, selection)
	latestLink := evidence.finish()
	if result.TraceURL == "" {
		result.TraceURL = latestLink
	}
	if ctx.Err() != nil && checkErr != nil {
		return SessionCheckRun{}, &APIError{Code: Internal, Message: fmt.Sprintf("the check run of session %s was cancelled before it finished", stream)}
	}
	ctx = context.WithoutCancel(ctx)
	if errors.Is(checkErr, context.DeadlineExceeded) {
		checkErr = fmt.Errorf("checks did not finish within checks_timeout %s", timeout)
	}
	status, failed, classifyErr := checkrun.ClassifyChecks(result, checkErr)
	sr := SessionCheckRun{
		TraceURL:           result.TraceURL,
		CIIdentityFallback: result.CIIdentityFallback,
		Run:                run, Commit: head, Kind: sessionCheckKind(selection), Checks: selection.Links, Selection: &selection,
		Command:   checkrun.ResultCommand(result, selection.Links),
		Status:    status,
		ExitCode:  result.ExitCode,
		Failed:    failed,
		Flaky:     result.Flaky,
		StartedAt: startedAt,
	}
	if classifyErr != nil {
		sr.Error = classifyErr.Error()
	}
	sr.Output, sr.Truncated = checkrun.SummarizeCheckOutput(result.Output, result.ExitCode)
	sr.Truncated = sr.Truncated || result.Truncated
	sr.OutputPath = checkrun.CheckOutputPath(sessionCheckRecordPath(run))
	sr.OutputRevision = 1
	sr.FinishedAt = s.now()
	if err := recordSessionCheckRun(ctx, repository, project, stream, sr, result.Output); err != nil {
		return SessionCheckRun{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot record the check run of session %s; check the trace repository: %v", stream, err)}
	}
	summary := fmt.Sprintf("Check run %d on commit %s %s: %s.", run, head, sr.Status, checkrun.DescribeSelectionRun(selection, engines))
	if flaky := checkrun.DescribeFlaky(sr.Flaky); flaky != "" {
		summary += " " + flaky + "."
	}
	if err := recordSessionEntry(ctx, repository, project, stream, cause+"-entry", sr.FinishedAt, summary); err != nil {
		return SessionCheckRun{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot record the check entry of session %s; check the trace repository", stream)}
	}
	return sr, nil
}

func (s *Service) sessionPush(ctx context.Context, raw string) (SessionPushResponse, *APIError) {
	project, stream, repository, api := s.sessionTrace(raw)
	if api != nil {
		return SessionPushResponse{}, api
	}
	if err := s.step("session-push-looked-up"); err != nil {
		return SessionPushResponse{}, &APIError{Code: Internal, Message: err.Error()}
	}
	lock := sessionLock(repository, stream)
	if !lock.TryLock() {
		return SessionPushResponse{}, &APIError{Code: Conflict, Message: "the session is ending; try again once it finishes"}
	}
	if _, _, _, api := s.sessionTrace(raw); api != nil {
		lock.Unlock()
		return SessionPushResponse{}, api
	}
	lock.controls.Add(1)
	defer lock.controls.Add(-1)
	ended, err := sessionEnded(repository, stream)
	lock.Unlock()
	if err != nil {
		return SessionPushResponse{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot read the state of session %s; check the trace repository", stream)}
	}
	if ended {
		return SessionPushResponse{}, &APIError{Code: Conflict, Message: "the session has ended"}
	}
	if err := s.step("session-push-admitted"); err != nil {
		return SessionPushResponse{}, &APIError{Code: Internal, Message: err.Error()}
	}
	cfg := s.current().For(project)
	g := sessionGit(cfg)
	branch := sessionBranch(stream)
	head, exists, err := g.Branch(ctx, branch)
	if err != nil {
		return SessionPushResponse{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot read the branch of session %s; check the trace repository", stream)}
	}
	if !exists {
		return SessionPushResponse{}, &APIError{Code: Internal, Message: fmt.Sprintf("session %s has no branch", stream)}
	}
	remote, err := g.Remote(ctx, cfg.Project.Upstream)
	if err != nil {
		return SessionPushResponse{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot find the clone's remote for %s: %v", cfg.Project.Upstream, err)}
	}
	remoteCommit, remoteExists, err := g.RemoteBranch(ctx, remote, branch)
	if err != nil {
		return SessionPushResponse{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot read the remote branch %s: %v", branch, err)}
	}
	expected := ""
	if remoteExists {
		known, err := g.Known(ctx, remoteCommit)
		if err != nil {
			return SessionPushResponse{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot check whether the remote branch %s is known locally: %v", branch, err)}
		}
		ancestor := false
		if known {
			if ancestor, err = g.Ancestor(ctx, remoteCommit, head); err != nil {
				return SessionPushResponse{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot compare the remote branch %s with the session branch: %v", branch, err)}
			}
		}
		if !ancestor {
			return SessionPushResponse{}, &APIError{Code: Conflict, Message: fmt.Sprintf("the remote branch %s has commits not on the session branch; push refused", branch)}
		}
		expected = remoteCommit
	}
	if err := g.Push(ctx, remote, head, branch, expected); err != nil {
		return SessionPushResponse{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot push %s to %s: %v", branch, remote, err)}
	}
	return SessionPushResponse{Remote: remote, Branch: branch, Commit: head}, nil
}

func (s *Service) sessionEnd(ctx context.Context, raw string) (SessionView, *APIError) {
	project, stream, repository, api := s.sessionTrace(raw)
	if api != nil {
		return SessionView{}, api
	}
	if err := s.step("session-end-looked-up"); err != nil {
		return SessionView{}, &APIError{Code: Internal, Message: err.Error()}
	}
	lock := sessionLock(repository, stream)
	if !lock.TryLock() {
		return SessionView{}, &APIError{Code: Conflict, Message: "a message is being started for this session; try again once it finishes"}
	}
	defer lock.Unlock()
	if _, _, _, api := s.sessionTrace(raw); api != nil {
		return SessionView{}, api
	}
	ended, err := sessionEnded(repository, stream)
	if err != nil {
		return SessionView{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot read the state of session %s; check the trace repository", stream)}
	}
	if !ended {
		if busy, err := sessionThreadBusy(repository, stream); err != nil {
			return SessionView{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot read the thread of session %s; check the trace repository", stream)}
		} else if busy {
			return SessionView{}, &APIError{Code: Conflict, Message: "a turn is running for this session; try again once it finishes"}
		}
		if err := setSessionState(ctx, repository, project, stream, s.now(), "session-ended-"+string(stream), "owner-end", trace.SessionEnded); err != nil {
			return SessionView{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot record session %s as ended; check the trace repository", stream)}
		}
	}
	cfg := s.current().For(project)
	g := sessionGit(cfg)
	if wt, found, err := g.Workspace(ctx, string(stream)); err != nil {
		return SessionView{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot read the worktree of session %s; check the workspace directory", stream)}
	} else if found {
		if err := g.Release(ctx, wt); err != nil {
			return SessionView{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot release the worktree of session %s: %v", stream, err)}
		}
	}
	if err := os.RemoveAll(runtime.ModelSessionDirectory(cfg.Root.String(), project, stream)); err != nil {
		return SessionView{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot release the model-session state of session %s: %v", stream, err)}
	}
	return s.sessionRead(string(stream))
}
