package executionjourney

import (
	context "context"
	errors "errors"
	agent "github.com/kpenfound/agentcore/agent"
	config "github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/lifecycle"
	readview "github.com/kpenfound/osmia/internal/readview"
	runtime "github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/service"
	trace "github.com/kpenfound/osmia/internal/trace"
	slog "log/slog"
	os "os"
	filepath "path/filepath"
	slices "slices"
	strings "strings"
	sync "sync"
	atomic "sync/atomic"
	testing "testing"
	time "time"
)

func assertDestroyedSession(t *testing.T, s *Service, c *Client, project config.ProjectID, stream config.WorkstreamID) {
	t.Helper()
	ctx := context.Background()
	_, err := c.Session(ctx, stream)
	assertCode(t, err, NotFound)
	_, err = c.Design(ctx, stream)
	assertCode(t, err, NotFound)
	_, err = c.DesignConversation(ctx, stream)
	assertCode(t, err, NotFound)
	_, err = c.Status(ctx, stream)
	assertCode(t, err, NotFound)
	list, err := c.Sessions(ctx, project)
	must(t, err)
	for _, v := range list.Sessions {
		if v.Workstream == stream {
			t.Fatal("destroyed session listed")
		}
	}
	designs, err := c.Designs(ctx, project)
	must(t, err)
	for _, v := range designs.Designs {
		if v.Workstream == stream {
			t.Fatal("destroyed design listed")
		}
	}
	root := s.current().Root
	gone, err := trace.WorkstreamTraceGone(root, project, stream)
	must(t, err)
	if !gone {
		t.Fatal("trace remains")
	}
	for _, kind := range append(slices.Clone(turnDirectories), "design", sessionsDirectory, "session-models") {
		assertGone(t, filepath.Join(root.String(), kind, string(project), string(stream)))
	}
	intents, err := trace.PendingSessionDestroys(root, project)
	must(t, err)
	for _, v := range intents {
		if v.Workstream == stream {
			t.Fatal("destroy intent remains")
		}
	}
	repository, err := s.repository(project)
	must(t, err)
	groups, _, err := trace.Workgroups(repository)
	must(t, err)
	if _, found := trace.WorkgroupOf(groups, stream); found {
		t.Fatal("workgroup still names the session")
	}
	cfg := s.current().For(project)
	_, exists, err := sessionGit(cfg).Branch(ctx, sessionBranch(stream))
	must(t, err)
	if exists {
		t.Fatal("local session branch remains")
	}
	for _, entry := range service.InspectForTest(s.core()).ActivityView().Activity {
		if entry.Workstream == stream {
			t.Fatal("activity retains the session")
		}
	}
	if cached, ok := service.InspectForTest(s.core()).StatusCache().Get(); ok {
		for _, w := range cached.Workstreams {
			if w.Workstream == stream {
				t.Fatal("status cache retains the session")
			}
		}
	}
	state, _ := s.store.Snapshot()
	for _, a := range state.Archived {
		if a.Workstream == stream {
			t.Fatal("destroy kept an archive")
		}
	}
	for _, p := range state.Pauses {
		if p.Target.Workstream == stream {
			t.Fatal("pause remains")
		}
	}
	for _, p := range state.Priorities {
		if slices.Contains(p.Workstreams, stream) {
			t.Fatal("priority remains")
		}
	}
	must(t, (&lifecycle.WorkgroupResolver{S: s.lifecycle(), Repository: repository}).Pass(ctx))
	must(t, repository.Serialize(func() error { return service.InspectForTest(s.core()).Schedule(ctx, repository) }))
}

func seedSessionState(t *testing.T, s *Service, project config.ProjectID, stream config.WorkstreamID) {
	t.Helper()
	must(t, s.refreshRuntime())
	s.StartActivity("historical-session-failure", ActivityAgentTurn, project, stream, "previous turn").Finish(errors.New("old failure"))
	service.InspectForTest(s.core()).StatusCache().Set(s.statusList())
	for _, kind := range append(slices.Clone(turnDirectories), "design", "session-models") {
		dir := filepath.Join(s.current().Root.String(), kind, string(project), string(stream))
		must(t, os.MkdirAll(dir, 0700))
		must(t, os.WriteFile(filepath.Join(dir, "state"), []byte("private history"), 0600))
	}
	must(t, s.store.SetPriority(runtime.Priority{Project: project, Workstreams: []config.WorkstreamID{stream}}))
	must(t, s.store.SetPause(runtime.Pause{Target: runtime.Target{Scope: "workstream", Project: project, Workstream: stream}, Mode: "soft", Source: runtime.PauseOwner, Reason: "owner pause"}))
}

func TestDestroySessionRemovesOpenAndEndedSessionsAndKeepsRemote(t *testing.T) {
	t.Parallel()
	for _, ended := range []bool{false, true} {
		t.Run(map[bool]string{false: "open", true: "ended"}[ended], func(t *testing.T) {
			t.Parallel()
			s, c, _, project, stream, _, clone := newSessionControlsFixture(t)
			ctx := context.Background()
			worktree := sessionWorktreePath(s.current().Root.String(), project, stream)
			must(t, os.WriteFile(filepath.Join(worktree, "note.txt"), []byte("pushed work"), 0600))
			_, err := c.SessionCommit(ctx, stream, "pushed change", "pushed")
			must(t, err)
			// This is a local bare test remote; destroy has no remote effect.
			pushed, err := c.SessionPush(ctx, stream)
			must(t, err)
			must(t, os.WriteFile(filepath.Join(worktree, "note.txt"), []byte("unpushed work"), 0600))
			_, err = c.SessionCommit(ctx, stream, "unpushed change", "unpushed")
			must(t, err)

			if ended {
				_, err = c.SessionEnd(ctx, stream)
				must(t, err)
			}
			seedSessionState(t, s, project, stream)
			result, err := c.DestroySession(ctx, stream)
			must(t, err)
			if !result.Destroyed || result.Project != project || result.Workstream != stream {
				t.Fatalf("response: %+v", result)
			}
			assertDestroyedSession(t, s, c, project, stream)
			bare := filepath.Join(filepath.Dir(clone), "remotes", "dagger", "dagger.git")
			if tip := strings.TrimSpace(demoGit(t, filepath.Dir(clone), "--git-dir", bare, "rev-parse", pushed.Branch)); tip != pushed.Commit {
				t.Fatal("remote branch changed")
			}
			_, err = c.DestroySession(ctx, stream)
			assertCode(t, err, NotFound)
		})
	}
}

func TestDestroyDesignRemovesWorkgroupAndLeavesNeighbors(t *testing.T) {
	t.Parallel()
	s, c, _, project, session, _, _ := newSessionControlsFixture(t)
	ctx := context.Background()
	opened, err := c.OpenDesign(ctx, project)
	must(t, err)
	neighbor, err := c.OpenDesign(ctx, project)
	must(t, err)
	seedSessionState(t, s, project, opened.Design.Workstream)
	result, err := c.DestroyDesign(ctx, opened.Design.Workstream)
	must(t, err)
	if !result.Destroyed {
		t.Fatal("destroy not acknowledged")
	}
	assertDestroyedSession(t, s, c, project, opened.Design.Workstream)
	_, err = c.Design(ctx, neighbor.Design.Workstream)
	must(t, err)
	_, err = c.Session(ctx, session)
	must(t, err)
	// A subsequent wrapping pass cannot resurrect the deleted design.
	cfg, projects := s.runtimes()
	must(t, s.ensureWorkgroups(ctx, cfg, projects, nil))
	assertDestroyedSession(t, s, c, project, opened.Design.Workstream)
}

func TestDestroySessionDoesNotRestoreStatusCache(t *testing.T) {
	t.Parallel()
	s, c, _, project, stream, _, _ := newSessionControlsFixture(t)
	seedSessionState(t, s, project, stream)
	snapshot := s.statusList()
	if !slices.ContainsFunc(snapshot.Workstreams, func(w WorkstreamStatus) bool { return w.Workstream == stream }) {
		t.Fatal("status snapshot does not include the session")
	}
	entered, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	finished := make(chan struct{})
	go func() {
		readview.Bounded(context.Background(), service.InspectForTest(s.core()).StatusCache(), time.Minute, func() StatusResponse {
			close(entered)
			<-release
			return snapshot
		})
		close(finished)
	}()
	<-entered
	_, err := c.DestroySession(context.Background(), stream)
	must(t, err)
	unblock()
	select {
	case <-finished:
	case <-time.After(demoTimeout):
		t.Fatal("status read did not finish")
	}
	assertDestroyedSession(t, s, c, project, stream)
}

func TestDestroySessionRecoveryAtEveryRemovalBoundary(t *testing.T) {
	t.Parallel()
	for _, design := range []bool{false, true} {
		for _, stage := range []string{"session-destroy-recorded", "session-destroy-released", "session-destroy-trace-moved"} {
			name := map[bool]string{false: "session", true: "design"}[design] + "/" + stage
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				var interrupt atomic.Bool
				s, c, opts, project, stream, _, _ := newSessionControlsFixture(t, destroyTestBoundary(func(_ context.Context, step string) error {
					if interrupt.Load() && step == stage {
						return errors.New("interrupted destroy")
					}
					return nil
				}))
				ctx := context.Background()
				if design {
					opened, err := c.OpenDesign(ctx, project)
					must(t, err)
					stream = opened.Design.Workstream
				}
				neighbor, err := c.OpenDesign(ctx, project)
				must(t, err)
				seedSessionState(t, s, project, stream)
				if !design {
					view, err := c.Session(ctx, stream)
					must(t, err)
					v := view.Session
					must(t, service.InspectForTest(s.core()).WritePendingSession(pendingSession{Version: 1, Project: project, Workstream: stream, Profile: v.Profile, Base: v.Base, Branch: v.Branch}))
				}
				interrupt.Store(true)
				if design {
					_, err = c.DestroyDesign(ctx, stream)
				} else {
					_, err = c.DestroySession(ctx, stream)
				}
				assertCode(t, err, Internal)
				pending, err := trace.SessionDestroyPending(s.current().Root, project, stream)
				must(t, err)
				if !pending {
					t.Fatal("interruption lost destroy intent")
				}
				if cached, ok := service.InspectForTest(s.core()).StatusCache().Get(); ok && slices.ContainsFunc(cached.Workstreams, func(w WorkstreamStatus) bool { return w.Workstream == stream }) {
					t.Fatal("pending destroy remains in the status cache")
				}
				if stage == "session-destroy-recorded" {
					repo := s.sole().repository
					h := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: "late-document", Revision: 1, Project: project, Workstream: stream, At: s.now(), Actor: ownerActor, Cause: "late-document"}
					err := repo.Append(ctx, trace.Document{Header: h, Path: "tools/late.json", Content: "must not be saved"})
					if !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("late document write was not fenced: %v", err)
					}
					_, err = repo.RecordDocumentsWith(ctx, []trace.Document{{Header: h, Path: "tools/late.json", Content: "must not be saved"}})
					if !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("late document batch was not fenced: %v", err)
					}
				}

				_, err = c.Session(ctx, stream)
				assertCode(t, err, NotFound)
				_, err = c.Design(ctx, stream)
				assertCode(t, err, NotFound)
				must(t, (&lifecycle.WorkgroupResolver{S: s.lifecycle(), Repository: s.sole().repository}).Pass(ctx))
				must(t, s.Close())
				interrupt.Store(false)
				s, c = start(t, opts)
				assertDestroyedSession(t, s, c, project, stream)
				_, err = c.Design(ctx, neighbor.Design.Workstream)
				must(t, err)
				// Repeated restart leaves no pending destroy, archive or recreated wrapper.
				must(t, s.Close())
				s, c = start(t, opts)
				assertDestroyedSession(t, s, c, project, stream)
			})
		}
	}
}

func TestDestroySessionFencesInterruptedOpen(t *testing.T) {
	t.Parallel()
	entered, release := make(chan struct{}), make(chan struct{})
	closeRelease := sync.OnceFunc(func() { close(release) })
	defer closeRelease()
	var interrupt atomic.Bool
	s, c, opts, project, _, _, _ := newSessionControlsFixture(t, destroyTestBoundary(func(ctx context.Context, step string) error {
		if interrupt.Load() && step == "session-recorded" {
			close(entered)
			select {
			case <-release:
				return errors.New("interrupted session open")
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}))
	interrupt.Store(true)
	ctx := context.Background()
	done := make(chan error, 1)
	go func() {
		_, err := c.OpenSession(ctx, SessionOpenRequest{Project: project, Profile: "default"})
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(demoTimeout):
		t.Fatal("session open never recorded its trace")
	}
	pending, err := service.InspectForTest(s.core()).ReadPendingSession()
	must(t, err)
	if pending == nil {
		t.Fatal("session open has no recovery journal")
	}
	_, err = c.DestroySession(ctx, pending.Workstream)
	assertCode(t, err, Conflict)
	_, err = c.Session(ctx, pending.Workstream)
	must(t, err)
	closeRelease()
	select {
	case err := <-done:
		assertCode(t, err, Internal)
	case <-time.After(demoTimeout):
		t.Fatal("interrupted open did not return")
	}
	interrupt.Store(false)
	_, err = c.DestroySession(ctx, pending.Workstream)
	must(t, err)
	// A replay that read the old journal must revalidate before any effect.
	_, err = service.InspectForTest(s.core()).CompleteSessionOpen(ctx, *pending)
	if !errors.Is(err, errSessionDiscarded) {
		t.Fatalf("stale open replay was not refused: %v", err)
	}
	journal, err := s.current().Root.PendingSessionOpen()
	must(t, err)
	assertGone(t, journal)
	assertDestroyedSession(t, s, c, project, pending.Workstream)
	must(t, s.Close())
	s, c = start(t, opts)
	assertDestroyedSession(t, s, c, project, pending.Workstream)
}

func TestDestroySessionRefusesInFlightCheckAndPush(t *testing.T) {
	t.Parallel()
	for _, control := range []string{"check", "push"} {
		t.Run(control, func(t *testing.T) {
			t.Parallel()
			entered, release := make(chan struct{}), make(chan struct{})
			s, c, _, project, stream, _, _ := newSessionControlsFixture(t, destroyTestBoundary(func(ctx context.Context, step string) error {
				if control == "push" && step == "session-push-admitted" {
					close(entered)
					select {
					case <-release:
					case <-ctx.Done():
						return ctx.Err()
					}
				}
				return nil
			}))
			if control == "check" {
				service.InspectForTest(s.core()).SetReviewChecks(checkFunc(func(ctx context.Context, _ string) (CheckResult, error) {
					close(entered)
					select {
					case <-release:
					case <-ctx.Done():
						return CheckResult{}, ctx.Err()
					}
					return CheckResult{Output: passedReport}, nil
				}))
			}
			done := make(chan error, 1)
			go func() {
				var err error
				if control == "check" {
					_, err = c.SessionCheck(context.Background(), stream)
				} else {
					_, err = c.SessionPush(context.Background(), stream)
				}
				done <- err
			}()
			select {
			case <-entered:
			case <-time.After(demoTimeout):
				t.Fatal("control never entered")
			}
			_, err := c.DestroySession(context.Background(), stream)
			assertCode(t, err, Conflict)
			_, err = c.Session(context.Background(), stream)
			must(t, err)
			if _, found, err := sessionGit(s.current().For(project)).Workspace(context.Background(), string(stream)); err != nil || !found {
				t.Fatalf("worktree changed: %v", err)
			}
			close(release)
			select {
			case err := <-done:
				must(t, err)
			case <-time.After(demoTimeout):
				t.Fatal("control did not finish")
			}
			_, err = c.DestroySession(context.Background(), stream)
			must(t, err)
			assertDestroyedSession(t, s, c, project, stream)
		})
	}
}

func TestDestroySessionFencesControlsWithStaleLookups(t *testing.T) {
	t.Parallel()
	for _, control := range []string{"commit", "check", "push", "end", "message"} {
		t.Run(control, func(t *testing.T) {
			t.Parallel()
			entered, release := make(chan struct{}), make(chan struct{})
			var logs syncBuffer
			var passes atomic.Int64
			s, c, _, project, stream, _, _ := newSessionControlsFixture(t, func(opts *Options) {
				opts.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
				opts.schedulePassed = func() { passes.Add(1) }
				opts.boundary = func(ctx context.Context, step string) error {
					if step == "session-"+control+"-looked-up" {
						close(entered)
						select {
						case <-release:
						case <-ctx.Done():
							return ctx.Err()
						}
					}
					return nil
				}
			})
			done := make(chan error, 1)
			go func() {
				ctx := context.Background()
				var err error
				switch control {
				case "commit":
					_, err = c.SessionCommit(ctx, stream, "commit", "stale")
				case "check":
					_, err = c.SessionCheck(ctx, stream)
				case "push":
					_, err = c.SessionPush(ctx, stream)
				case "end":
					_, err = c.SessionEnd(ctx, stream)
				case "message":
					_, err = c.SessionMessage(ctx, stream, "hello")
				}
				done <- err
			}()
			select {
			case <-entered:
			case <-time.After(demoTimeout):
				t.Fatal("lookup never entered")
			}
			_, err := c.DestroySession(context.Background(), stream)
			must(t, err)
			close(release)
			select {
			case err := <-done:
				assertCode(t, err, NotFound)
			case <-time.After(demoTimeout):
				t.Fatal("control did not finish")
			}
			assertDestroyedSession(t, s, c, project, stream)
			awaitFurtherPasses(t, &passes, 2)
			if strings.Contains(logs.String(), `"level":"ERROR"`) {
				t.Fatalf("background error: %s", logs.String())
			}
		})
	}
}

func TestDestroyRefusesTurnsAndConcurrentDesignStart(t *testing.T) {
	t.Parallel()
	s, c, project := newDesignFixture(t)
	ctx := context.Background()
	opened, err := c.OpenDesign(ctx, project)
	must(t, err)
	stream := opened.Design.Workstream
	// The design lock is held across both message admission and workgroup start.
	lock := intake.DesignLock(s.sole().repository, stream)
	lock.Lock()
	_, err = c.DestroyDesign(ctx, stream)
	assertCode(t, err, Conflict)
	lock.Unlock()
	_, err = c.DesignMessage(ctx, stream, "discuss")
	must(t, err)
	_, err = c.DestroyDesign(ctx, stream)
	assertCode(t, err, Conflict)
	_, err = c.Design(ctx, stream)
	must(t, err)
}

func TestDestroyRefusesOtherKindsAndStartedDesign(t *testing.T) {
	t.Parallel()
	s, c, project := newDesignFixture(t)
	ctx := context.Background()
	input := "ordinary feature"
	ordinary, err := c.HandIn(ctx, HandInRequest{Project: project, Stdin: &input, Key: "ordinary-destroy-refusal"})
	must(t, err)
	opened, err := c.OpenDesign(ctx, project)
	must(t, err)
	repo := s.sole().repository
	// This marker is the same guard as the owner start transition.
	state, err := repo.Workflow(opened.Design.Workstream, designSessionSubject)
	must(t, err)
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "started-test", Revision: 1, Project: project, Workstream: opened.Design.Workstream, At: s.now(), Actor: ownerActor, Cause: "started-test"}
	_, err = repo.Transact(ctx, trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: h, Subject: designSessionSubject, From: designOpenValue, To: designClosedValue, Reason: "owner started workgroup"}})
	must(t, err)
	for _, id := range []config.WorkstreamID{ordinary.Workstream, opened.Design.Workstream, librarianWorkstream(project)} {
		before, err := repo.WorkflowStates(id)
		must(t, err)
		_, err = c.DestroySession(ctx, id)
		assertCode(t, err, Conflict)
		_, err = c.DestroyDesign(ctx, id)
		assertCode(t, err, Conflict)
		after, err := repo.WorkflowStates(id)
		must(t, err)
		if len(before) != len(after) {
			t.Fatal("refusal changed workflow")
		}
	}
	groups, revision, err := trace.Workgroups(repo)
	must(t, err)
	for _, route := range []string{"design", "sessions"} {
		var out SessionDestroyResponse
		err = c.Do(ctx, "POST", Prefix+"/"+route+"/"+string(groups[0].ID)+"/destroy", nil, &out)
		assertCode(t, err, Conflict)
	}
	_, afterRevision, err := trace.Workgroups(repo)
	must(t, err)
	if afterRevision != revision {
		t.Fatal("refusal changed the workgroup")
	}
}

func destroyTestBoundary(boundary func(context.Context, string) error) func(*Options) {
	return func(opts *Options) { opts.boundary = boundary }
}

func TestDestroySessionRefusesRunningTurn(t *testing.T) {
	t.Parallel()
	s, c, _, project, stream, engine, _ := newSessionControlsFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	engine.Turn = func(ctx context.Context, _ agent.Request) (*agent.Result, error) {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return &agent.Result{ClaudeID: "session", ResultText: "done"}, nil
	}
	_, err := c.SessionMessage(context.Background(), stream, "work")
	must(t, err)
	select {
	case <-entered:
	case <-time.After(demoTimeout):
		t.Fatal("turn did not start")
	}
	_, err = c.DestroySession(context.Background(), stream)
	assertCode(t, err, Conflict)
	_, err = c.Session(context.Background(), stream)
	must(t, err)
	if _, found, err := sessionGit(s.current().For(project)).Workspace(context.Background(), string(stream)); err != nil || !found {
		t.Fatalf("worktree changed: %v", err)
	}
	close(release)
	awaitSession(t, c, stream, func(v SessionView) bool { return v.State == trace.SessionIdle })
}

func TestDestroyDesignRefusesWorkgroupStartInProgress(t *testing.T) {
	t.Parallel()
	entered, release := make(chan struct{}), make(chan struct{})
	s, c, _, project, _, _, _ := newSessionControlsFixture(t, destroyTestBoundary(func(ctx context.Context, step string) error {
		if step == "design-brief-handed" {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}))
	opened, err := c.OpenDesign(context.Background(), project)
	must(t, err)
	must(t, recordDesignDraft(t, s.sole().repository, opened.Design.Workstream, "Ship a small change."))
	finished := make(chan error, 1)
	go func() { _, err := c.DesignStart(context.Background(), opened.Design.Workstream); finished <- err }()
	select {
	case <-entered:
	case <-time.After(demoTimeout):
		t.Fatal("start did not reach handoff")
	}
	_, err = c.DestroyDesign(context.Background(), opened.Design.Workstream)
	assertCode(t, err, Conflict)
	_, err = c.Design(context.Background(), opened.Design.Workstream)
	must(t, err)
	close(release)
	select {
	case err := <-finished:
		must(t, err)
	case <-time.After(demoTimeout):
		t.Fatal("start did not finish")
	}
	_, err = c.DestroyDesign(context.Background(), opened.Design.Workstream)
	assertCode(t, err, Conflict)
}
