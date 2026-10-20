package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/kpenfound/osmia/internal/workspace"
)

// sessionGit is the Git provider of a session's worktrees. Sessions are
// always on the Git backend (session.go), so this bypasses the Provider
// interface for the methods only sessions need.
func sessionGit(cfg *config.Config) *workspace.Git {
	return workspaces(cfg, sessionsDirectory, config.WorkspacesGit).(*workspace.Git)
}

// sessionEnded reports whether the session has ended.
func sessionEnded(repository *trace.Repository, stream config.WorkstreamID) (bool, error) {
	state, err := repository.Workflow(stream, trace.SessionSubject)
	if err != nil {
		return false, err
	}
	return state.Value == trace.SessionEnded, nil
}

// sessionThreadBusy reports whether the session's thread holds a turn with
// no response and no completion, the same state unfinishedSessionTurn reads
// to refuse a message.
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

// sessionChanges reads a session's uncommitted worktree changes relative to
// its head, and the commits on its branch relative to its base commit.
// Once the session has ended and its worktree is released, Changes reads
// empty, but Commits stays readable from the branch's own history.
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
		return SessionChangesResponse{}, &APIError{Internal, fmt.Sprintf("cannot read the branch of session %s; check the trace repository", stream)}
	}
	if !exists {
		return SessionChangesResponse{}, &APIError{Internal, fmt.Sprintf("session %s has no branch", stream)}
	}
	changes := ""
	if wt, found, err := g.Workspace(ctx, string(stream)); err != nil {
		return SessionChangesResponse{}, &APIError{Internal, fmt.Sprintf("cannot read the worktree of session %s; check the workspace directory", stream)}
	} else if found {
		diff, err := g.WorktreeDiff(ctx, wt)
		if err != nil {
			return SessionChangesResponse{}, &APIError{Internal, fmt.Sprintf("cannot diff the worktree of session %s: %v", stream, err)}
		}
		changes = diff
	}
	messages, err := g.Log(ctx, view.Base, head)
	if err != nil {
		return SessionChangesResponse{}, &APIError{Internal, fmt.Sprintf("cannot read the commits of session %s: %v", stream, err)}
	}
	commits := make([]SessionCommitSummary, len(messages))
	for i, m := range messages {
		commits[i] = SessionCommitSummary{Commit: m.Commit, Message: sessionChangesMessage(m.Message)}
	}
	return SessionChangesResponse{Head: head, Changes: changes, Commits: commits}, nil
}

// sessionCommitTrailer names the request key a session commit's message
// carries, so a retry with the same key, including across a restart, finds
// the commit already made instead of making a second one, the way
// landingTrailer does for a unit's landing commit (landing.go).
const sessionCommitTrailer = "Osmia-Session-Request"

// sessionCommitMessage appends the request key's trailer to the owner's
// message.
func sessionCommitMessage(message, key string) string {
	return strings.TrimRight(message, "\n") + "\n\n" + sessionCommitTrailer + ": " + key + "\n"
}

// sessionCommitKey reads the request key a commit message's trailer
// carries, or "" when it has none.
func sessionCommitKey(message string) string {
	for _, line := range strings.Split(message, "\n") {
		if k, ok := strings.CutPrefix(line, sessionCommitTrailer+": "); ok {
			return k
		}
	}
	return ""
}

// sessionChangesMessage strips a commit's own Osmia-Session-Request
// trailer, and the blank line before it, for the owner-facing changes view
// (spec#13). The commit's own message, read by sessionCommitKey for retry
// detection, keeps the trailer; only this owner-facing copy loses it.
func sessionChangesMessage(message string) string {
	body, _, found := strings.Cut(message, "\n\n"+sessionCommitTrailer+": ")
	if !found {
		return message
	}
	return body
}

// sessionCommitLocks serializes commits of the same session, so two
// concurrent commit requests cannot both stage and write the worktree's
// real Git index at once.
var sessionCommitLocks sync.Map // sessionLockKey -> *sync.Mutex

func sessionCommitLock(repository *trace.Repository, stream config.WorkstreamID) *sync.Mutex {
	v, _ := sessionCommitLocks.LoadOrStore(sessionLockKey{repository, stream}, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// sessionCommit commits every current worktree change onto the session
// branch under the owner's message, moves the session head and adds a
// thread entry naming the commit. It is refused, recording nothing, while a
// turn is running, when the message is empty, when the worktree has no
// changes, or once the session has ended. A retry with the same request
// key, including across a restart, finds the commit the key already made
// and returns it instead of making a second one.
func (s *Service) sessionCommit(ctx context.Context, raw string, req SessionCommitRequest) (SessionCommitResponse, *APIError) {
	project, stream, repository, api := s.sessionTrace(raw)
	if api != nil {
		return SessionCommitResponse{}, api
	}
	if err := s.step("session-commit-looked-up"); err != nil {
		return SessionCommitResponse{}, &APIError{Internal, err.Error()}
	}
	message := strings.TrimSpace(req.Message)
	if message == "" {
		return SessionCommitResponse{}, &APIError{Validation, "message must not be empty"}
	}
	key := strings.TrimSpace(req.RequestKey)
	if key == "" {
		return SessionCommitResponse{}, &APIError{Validation, "request_key must not be empty"}
	}
	// sessionMessage and sessionEnd hold this same lock, from their own busy
	// check through recording what they do, so a commit cannot observe
	// "not busy" or "not ended" and then write the worktree while a message
	// that was about to start a turn, or an end that was about to release
	// the worktree, slips in between the check and the write.
	lock := sessionLock(repository, stream)
	if !lock.TryLock() {
		return SessionCommitResponse{}, &APIError{Conflict, "a message is being started for this session; try again once it finishes"}
	}
	defer lock.Unlock()
	if _, _, _, api := s.sessionTrace(raw); api != nil {
		return SessionCommitResponse{}, api
	}
	// State and existence are checked under the same guard as end and destroy.
	if ended, err := sessionEnded(repository, stream); err != nil {
		return SessionCommitResponse{}, &APIError{Internal, fmt.Sprintf("cannot read the state of session %s; check the trace repository", stream)}
	} else if ended {
		return SessionCommitResponse{}, &APIError{Conflict, "the session has ended"}
	}
	if busy, err := sessionThreadBusy(repository, stream); err != nil {
		return SessionCommitResponse{}, &APIError{Internal, fmt.Sprintf("cannot read the thread of session %s; check the trace repository", stream)}
	} else if busy {
		return SessionCommitResponse{}, &APIError{Conflict, "a turn is running for this session; try again once it finishes"}
	}
	mu := sessionCommitLock(repository, stream)
	mu.Lock()
	defer mu.Unlock()
	cfg := s.current().For(project)
	g := sessionGit(cfg)
	branch := sessionBranch(stream)
	head, exists, err := g.Branch(ctx, branch)
	if err != nil {
		return SessionCommitResponse{}, &APIError{Internal, fmt.Sprintf("cannot read the branch of session %s; check the trace repository", stream)}
	}
	if !exists {
		return SessionCommitResponse{}, &APIError{Internal, fmt.Sprintf("session %s has no branch", stream)}
	}
	c, err := g.Commit(ctx, head)
	if err != nil {
		return SessionCommitResponse{}, &APIError{Internal, fmt.Sprintf("cannot read the head commit of session %s: %v", stream, err)}
	}
	commit := head
	if sessionCommitKey(c.Message) != key {
		wt, found, err := g.Workspace(ctx, string(stream))
		if err != nil {
			return SessionCommitResponse{}, &APIError{Internal, fmt.Sprintf("cannot read the worktree of session %s; check the workspace directory", stream)}
		}
		if !found {
			return SessionCommitResponse{}, &APIError{Internal, fmt.Sprintf("session %s has no worktree", stream)}
		}
		changed := false
		if commit, changed, err = g.CommitChanges(ctx, wt, sessionCommitMessage(message, key), s.now()); err != nil {
			return SessionCommitResponse{}, &APIError{Internal, fmt.Sprintf("cannot commit the worktree of session %s: %v", stream, err)}
		}
		if !changed {
			return SessionCommitResponse{}, &APIError{Validation, "the worktree has no changes to commit"}
		}
	}
	// A retry whose commit was already made, including one across a restart
	// cut short before this record, finds commit == head here and still
	// records the thread entry, idempotently, the way landingCommit's commit
	// detection still lets land call record (landing.go).
	summary := fmt.Sprintf("Committed %s: %s", commit, strings.SplitN(message, "\n", 2)[0])
	if err := recordSessionEntry(ctx, repository, project, stream, "commit-"+key, s.now(), summary); err != nil {
		return SessionCommitResponse{}, &APIError{Internal, fmt.Sprintf("cannot record the commit entry of session %s; check the trace repository", stream)}
	}
	return SessionCommitResponse{Commit: commit}, nil
}

// sessionChecksRole and sessionChecksThread scope a session check's Jev
// judgment.
const sessionChecksRole = "session-checks"
const sessionChecksThread = "session-checks"

// sessionCheckDocumentID names the record of a session's check run.
func sessionCheckDocumentID(run int) string { return fmt.Sprintf("session-check-%d", run) }

// sessionCheckRecordPath names the trace document a session's check run is
// recorded as.
func sessionCheckRecordPath(run int) string { return fmt.Sprintf("session-checks/checks-%d.json", run) }

var sessionCheckRecordPattern = regexp.MustCompile(`^session-checks/checks-[0-9]+\.json$`)

// sessionCheckRuns returns every check run recorded for the session, at its
// latest revision.
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

// nextSessionCheckRun returns the run number the session's next check uses.
func nextSessionCheckRun(repo *trace.Repository, stream config.WorkstreamID) (int, error) {
	runs, err := sessionCheckRuns(repo, stream)
	if err != nil {
		return 0, err
	}
	run := 0
	for _, r := range runs {
		run = max(run, r.Run)
	}
	return run + 1, nil
}

// sessionCheckKind reports the owner-facing word for a check selection: a
// smart run selected checks through Jev, a full run ran every check.
func sessionCheckKind(sel CheckSelection) string {
	if sel.Mode == selectionSelected {
		return "smart"
	}
	return "full"
}

// recordSessionCheckRun records run as the session's check-<run> record, and
// its full captured output separately when it has an output path, the same
// two-document shape a unit's check run uses.
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

// sessionCheckLocks guards against two check runs of the same session
// running at once.
var sessionCheckLocks sync.Map // sessionLockKey -> *sync.Mutex

func sessionCheckLock(repository *trace.Repository, stream config.WorkstreamID) *sync.Mutex {
	v, _ := sessionCheckLocks.LoadOrStore(sessionLockKey{repository, stream}, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// sessionCheck checks the session head commit: a smart check selected by
// the service's existing smart-check selection over the changes between the
// session's base commit and its head when the service has JEV enabled, or
// the full `dagger check` the pre-review check run uses otherwise. It
// records the commit, whether the run was smart or full, the selected
// checks for a smart run, status, exit code and output, and adds a thread
// entry naming the checked commit. Only one check run per session can be in
// progress at a time; a second request while one runs is refused.
func (s *Service) sessionCheck(ctx context.Context, raw string) (SessionCheckRun, *APIError) {
	project, stream, repository, api := s.sessionTrace(raw)
	if api != nil {
		return SessionCheckRun{}, api
	}
	if err := s.step("session-check-looked-up"); err != nil {
		return SessionCheckRun{}, &APIError{Internal, err.Error()}
	}
	mu := sessionCheckLock(repository, stream)
	if !mu.TryLock() {
		return SessionCheckRun{}, &APIError{Conflict, "a check is already running for this session; try again once it finishes"}
	}
	defer mu.Unlock()
	// Register the control under the admission lock and retain its lease
	// through the external checker and the final trace writes.
	lock := sessionLock(repository, stream)
	if !lock.TryLock() {
		return SessionCheckRun{}, &APIError{Conflict, "the session is ending; try again once it finishes"}
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
		return SessionCheckRun{}, &APIError{Internal, fmt.Sprintf("cannot read the state of session %s; check the trace repository", stream)}
	}
	if ended {
		return SessionCheckRun{}, &APIError{Conflict, "the session has ended"}
	}
	view, api := s.sessionRead(string(stream))
	if api != nil {
		return SessionCheckRun{}, api
	}
	cfg := s.current().For(project)
	g := sessionGit(cfg)
	head, exists, err := g.Branch(ctx, sessionBranch(stream))
	if err != nil || !exists {
		return SessionCheckRun{}, &APIError{Internal, fmt.Sprintf("cannot read the branch of session %s; check the trace repository", stream)}
	}
	checks := s.options.reviewChecks
	if checks == nil {
		return SessionCheckRun{}, &APIError{Internal, "the candidate check runner is unavailable"}
	}
	diff, err := g.Diff(ctx, view.Base, head)
	if err != nil {
		return SessionCheckRun{}, &APIError{Internal, fmt.Sprintf("cannot diff the session's base and head: %v", err)}
	}
	base := filepath.Join(cfg.Root.String(), "checks")
	if err := os.MkdirAll(base, 0700); err != nil {
		return SessionCheckRun{}, &APIError{Internal, err.Error()}
	}
	dir, err := os.MkdirTemp(base, "session-")
	if err != nil {
		return SessionCheckRun{}, &APIError{Internal, err.Error()}
	}
	defer os.RemoveAll(dir)
	if err := g.Export(ctx, head, dir); err != nil {
		return SessionCheckRun{}, &APIError{Internal, fmt.Sprintf("cannot export the session's head: %v", err)}
	}
	release, err := s.checkSlots.acquire(ctx, CheckSlotRun{Project: project, Workstream: stream, Session: true})
	if err != nil {
		return SessionCheckRun{}, &APIError{Internal, err.Error()}
	}
	defer release()
	run, err := nextSessionCheckRun(repository, stream)
	if err != nil {
		return SessionCheckRun{}, &APIError{Internal, err.Error()}
	}
	cause := sessionCheckDocumentID(run)
	startedAt := s.now()
	scope := coreadapter.Scope{Project: string(project), Workstream: string(stream), Thread: sessionChecksThread, Turn: cause, Role: sessionChecksRole}
	selection := selectChecksFor(ctx, s.jev, repository, scope, cause, nil, dir, diff, checks)
	timeout := s.about(repository).Project.CheckTimeout()
	bounded, cancel := context.WithTimeout(ctx, timeout)
	result, checkErr := checks.Check(bounded, dir, selection.Links)
	cancel()
	if errors.Is(checkErr, context.DeadlineExceeded) {
		checkErr = fmt.Errorf("checks did not finish within checks_timeout %s", timeout)
	}
	status, failed, classifyErr := classifyChecks(result, checkErr)
	sr := SessionCheckRun{
		Run: run, Commit: head, Kind: sessionCheckKind(selection), Checks: selection.Links, Selection: &selection,
		Command:   append([]string{"dagger", "check", "--progress=report", "--failfast"}, selection.Links...),
		Status:    status,
		ExitCode:  result.ExitCode,
		Failed:    failed,
		StartedAt: startedAt,
	}
	if classifyErr != nil {
		sr.Error = classifyErr.Error()
	}
	sr.Output, sr.Truncated = summarizeCheckOutput(result.Output, result.ExitCode)
	sr.Truncated = sr.Truncated || result.Truncated
	sr.OutputPath = checkOutputPath(sessionCheckRecordPath(run))
	sr.OutputRevision = 1
	sr.FinishedAt = s.now()
	if err := recordSessionCheckRun(ctx, repository, project, stream, sr, result.Output); err != nil {
		return SessionCheckRun{}, &APIError{Internal, fmt.Sprintf("cannot record the check run of session %s; check the trace repository: %v", stream, err)}
	}
	summary := fmt.Sprintf("Check run %d on commit %s %s: %s.", run, head, sr.Status, selection.describe())
	if err := recordSessionEntry(ctx, repository, project, stream, cause+"-entry", sr.FinishedAt, summary); err != nil {
		return SessionCheckRun{}, &APIError{Internal, fmt.Sprintf("cannot record the check entry of session %s; check the trace repository", stream)}
	}
	return sr, nil
}

// sessionPush pushes the session branch to the project's remote under the
// session's branch name, with no force push and no pull request, using
// credentials the service holds. It is refused when the remote branch has
// commits not on the session branch.
func (s *Service) sessionPush(ctx context.Context, raw string) (SessionPushResponse, *APIError) {
	project, stream, repository, api := s.sessionTrace(raw)
	if api != nil {
		return SessionPushResponse{}, api
	}
	if err := s.step("session-push-looked-up"); err != nil {
		return SessionPushResponse{}, &APIError{Internal, err.Error()}
	}
	// The lease covers every remote phase after admission.
	lock := sessionLock(repository, stream)
	if !lock.TryLock() {
		return SessionPushResponse{}, &APIError{Conflict, "the session is ending; try again once it finishes"}
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
		return SessionPushResponse{}, &APIError{Internal, fmt.Sprintf("cannot read the state of session %s; check the trace repository", stream)}
	}
	if ended {
		return SessionPushResponse{}, &APIError{Conflict, "the session has ended"}
	}
	if err := s.step("session-push-admitted"); err != nil {
		return SessionPushResponse{}, &APIError{Internal, err.Error()}
	}
	cfg := s.current().For(project)
	g := sessionGit(cfg)
	branch := sessionBranch(stream)
	head, exists, err := g.Branch(ctx, branch)
	if err != nil {
		return SessionPushResponse{}, &APIError{Internal, fmt.Sprintf("cannot read the branch of session %s; check the trace repository", stream)}
	}
	if !exists {
		return SessionPushResponse{}, &APIError{Internal, fmt.Sprintf("session %s has no branch", stream)}
	}
	remote, err := g.Remote(ctx, cfg.Project.Upstream)
	if err != nil {
		return SessionPushResponse{}, &APIError{Internal, fmt.Sprintf("cannot find the clone's remote for %s: %v", cfg.Project.Upstream, err)}
	}
	remoteCommit, remoteExists, err := g.RemoteBranch(ctx, remote, branch)
	if err != nil {
		return SessionPushResponse{}, &APIError{Internal, fmt.Sprintf("cannot read the remote branch %s: %v", branch, err)}
	}
	expected := ""
	if remoteExists {
		// A remote tip the clone has never fetched is unknown to it, and
		// asking merge-base to compare against an unknown commit fails
		// outright instead of answering. Any commit the session branch
		// already contains is necessarily present locally, so an unknown
		// remote tip cannot be one of them: it is refused the same as a
		// known commit merge-base reports is not an ancestor.
		known, err := g.Known(ctx, remoteCommit)
		if err != nil {
			return SessionPushResponse{}, &APIError{Internal, fmt.Sprintf("cannot check whether the remote branch %s is known locally: %v", branch, err)}
		}
		ancestor := false
		if known {
			if ancestor, err = g.Ancestor(ctx, remoteCommit, head); err != nil {
				return SessionPushResponse{}, &APIError{Internal, fmt.Sprintf("cannot compare the remote branch %s with the session branch: %v", branch, err)}
			}
		}
		if !ancestor {
			return SessionPushResponse{}, &APIError{Conflict, fmt.Sprintf("the remote branch %s has commits not on the session branch; push refused", branch)}
		}
		expected = remoteCommit
	}
	if err := g.Push(ctx, remote, head, branch, expected); err != nil {
		return SessionPushResponse{}, &APIError{Internal, fmt.Sprintf("cannot push %s to %s: %v", branch, remote, err)}
	}
	return SessionPushResponse{Remote: remote, Branch: branch, Commit: head}, nil
}

// sessionEnd ends a session: refused while a turn is running, otherwise the
// session's state moves to ended and its worktree and stored model-session
// state are released. Its branch and commits, thread and trace stay
// readable. Ending an already-ended session retries releasing whatever an
// earlier, interrupted end left behind, and otherwise does nothing more.
//
// The transition to ended and both releases all happen under the same
// per-session lock sessionMessage holds from its busy check through
// recording the turn it starts, and sessionCommit and sessionCheck recheck
// before acting: holding it across the whole operation, rather than only
// across the transition, closes the window a split critical section would
// leave open, where a commit or check could see ended still false, then
// acquire the lock only after the transition finished but before the
// worktree and model-session state were actually released, and act on a
// worktree sessionEnd was concurrently tearing down.
func (s *Service) sessionEnd(ctx context.Context, raw string) (SessionView, *APIError) {
	project, stream, repository, api := s.sessionTrace(raw)
	if api != nil {
		return SessionView{}, api
	}
	if err := s.step("session-end-looked-up"); err != nil {
		return SessionView{}, &APIError{Internal, err.Error()}
	}
	lock := sessionLock(repository, stream)
	if !lock.TryLock() {
		return SessionView{}, &APIError{Conflict, "a message is being started for this session; try again once it finishes"}
	}
	defer lock.Unlock()
	if _, _, _, api := s.sessionTrace(raw); api != nil {
		return SessionView{}, api
	}
	ended, err := sessionEnded(repository, stream)
	if err != nil {
		return SessionView{}, &APIError{Internal, fmt.Sprintf("cannot read the state of session %s; check the trace repository", stream)}
	}
	if !ended {
		if busy, err := sessionThreadBusy(repository, stream); err != nil {
			return SessionView{}, &APIError{Internal, fmt.Sprintf("cannot read the thread of session %s; check the trace repository", stream)}
		} else if busy {
			return SessionView{}, &APIError{Conflict, "a turn is running for this session; try again once it finishes"}
		}
		if err := setSessionState(ctx, repository, project, stream, s.now(), "session-ended-"+string(stream), "owner-end", trace.SessionEnded); err != nil {
			return SessionView{}, &APIError{Internal, fmt.Sprintf("cannot record session %s as ended; check the trace repository", stream)}
		}
	}
	cfg := s.current().For(project)
	g := sessionGit(cfg)
	if wt, found, err := g.Workspace(ctx, string(stream)); err != nil {
		return SessionView{}, &APIError{Internal, fmt.Sprintf("cannot read the worktree of session %s; check the workspace directory", stream)}
	} else if found {
		if err := g.Release(ctx, wt); err != nil {
			return SessionView{}, &APIError{Internal, fmt.Sprintf("cannot release the worktree of session %s: %v", stream, err)}
		}
	}
	if err := os.RemoveAll(runtime.ModelSessionDirectory(cfg.Root.String(), project, stream)); err != nil {
		return SessionView{}, &APIError{Internal, fmt.Sprintf("cannot release the model-session state of session %s: %v", stream, err)}
	}
	return s.sessionRead(string(stream))
}
