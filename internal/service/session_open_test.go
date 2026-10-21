package service

import (
	context "context"
	json "encoding/json"
	errors "errors"
	fmt "fmt"
	maps "maps"
	os "os"
	filepath "path/filepath"
	reflect "reflect"
	strings "strings"
	testing "testing"
	time "time"

	config "github.com/kpenfound/osmia/internal/config"
	plan "github.com/kpenfound/osmia/internal/plan"
	reconcile "github.com/kpenfound/osmia/internal/reconcile"
	runtime "github.com/kpenfound/osmia/internal/runtime"
	seal "github.com/kpenfound/osmia/internal/seal"
	shed "github.com/kpenfound/osmia/internal/shed"
	trace "github.com/kpenfound/osmia/internal/trace"
)

// newSessionFixture starts a service with one registered, committed
// project with a local bare upstream remote, so a session can open from a
// real default-branch head.
func newSessionFixture(t *testing.T) (*Service, *Client, config.ProjectID, string) {
	t.Helper()
	opts, clone := projectFixture(t)
	home := filepath.Dir(clone)
	must(t, os.WriteFile(filepath.Join(clone, "README.md"), []byte("hi\n"), 0600))
	demoGit(t, home, "-C", clone, "add", "README.md")
	demoGit(t, home, "-C", clone, "-c", "user.name=Owner", "-c", "user.email=owner@example.invalid", "commit", "-qm", "base")
	demoGit(t, home, "-C", clone, "branch", "-M", "main")
	bare := filepath.Join(home, "remotes", "dagger", "dagger.git")
	must(t, os.MkdirAll(filepath.Dir(bare), 0700))
	demoGit(t, home, "clone", "--quiet", "--bare", "--no-hardlinks", clone, bare)
	demoGit(t, home, "-C", clone, "remote", "add", "upstream", bare)
	s, c := start(t, opts)
	added, err := c.AddProject(context.Background(), request(clone))
	must(t, err)
	writeCharter(t, c, added.Project.ID, "1. Keep changes small.\n")
	return s, c, added.Project.ID, clone
}

func TestSessionOpenRecordsWorkstreamAndWorktree(t *testing.T) {
	t.Parallel()
	s, c, project, clone := newSessionFixture(t)
	home := filepath.Dir(clone)
	head := strings.TrimSpace(demoGit(t, home, "-C", clone, "rev-parse", "HEAD"))
	resp, err := c.OpenSession(context.Background(), SessionOpenRequest{Project: project, Profile: "default", Title: "exploring"})
	must(t, err)
	session := resp.Session
	if session.Project != project || session.Profile != "default" || session.Base != head || session.Branch == "" || session.Title != "exploring" || session.State != trace.SessionIdle || session.Workstream == "" {
		t.Fatalf("session view: %+v, want base %s", session, head)
	}
	listed, err := c.Status(context.Background(), session.Workstream)
	must(t, err)
	if listed.Kind != trace.SessionKind || listed.Profile != "default" || listed.Branch != session.Branch || listed.Base != head {
		t.Fatalf("status view: %+v", listed)
	}
	read, err := c.Session(context.Background(), session.Workstream)
	must(t, err)
	if read.Session != session {
		t.Fatalf("read %+v, want %+v", read.Session, session)
	}
	worktree := filepath.Join(s.cfg.Root.String(), sessionsDirectory, string(project), string(session.Workstream))
	info, err := os.Stat(worktree)
	must(t, err)
	if !info.IsDir() {
		t.Fatalf("%s is not a directory", worktree)
	}
	gotHead := strings.TrimSpace(demoGit(t, home, "-C", worktree, "rev-parse", "HEAD"))
	if gotHead != head {
		t.Fatalf("worktree head %s, want %s", gotHead, head)
	}
	if rel, err := filepath.Rel(clone, worktree); err == nil && (rel == "." || !strings.HasPrefix(rel, "..")) {
		t.Fatalf("worktree %s is inside the target clone %s", worktree, clone)
	}
}

func TestSessionOpenRefusesUnknownProject(t *testing.T) {
	t.Parallel()
	_, c, _, _ := newSessionFixture(t)
	unknown := config.ProjectID("p_00000000000000000000000000000099")
	_, err := c.OpenSession(context.Background(), SessionOpenRequest{Project: unknown, Profile: "default"})
	assertCode(t, err, NotFound)
	sessions, err := c.Sessions(context.Background(), "")
	must(t, err)
	if len(sessions.Sessions) != 0 {
		t.Fatalf("unknown project left a session: %+v", sessions)
	}
}

func TestSessionOpenRefusesUnknownOrMissingProfile(t *testing.T) {
	t.Parallel()
	s, c, project, _ := newSessionFixture(t)
	for _, name := range []string{"", "nonexistent", "other"} {
		_, err := c.OpenSession(context.Background(), SessionOpenRequest{Project: project, Profile: name})
		assertCode(t, err, Validation)
	}
	sessions, err := c.Sessions(context.Background(), project)
	must(t, err)
	if len(sessions.Sessions) != 0 {
		t.Fatalf("an invalid profile left a session: %+v", sessions)
	}
	if entries, err := os.ReadDir(filepath.Join(s.cfg.Root.String(), sessionsDirectory, string(project))); err == nil && len(entries) != 0 {
		t.Fatalf("an invalid profile left a worktree: %+v", entries)
	} else if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

// TestSessionOpenRecoversAtEachStep interrupts an open at each of its
// journalled steps and shows that a restart or a retry with the same
// request leaves exactly one usable session with its worktree, never a
// duplicate and never an orphaned worktree, and that the journal is always
// cleared by the time the session is usable.
func TestSessionOpenRecoversAtEachStep(t *testing.T) {
	t.Parallel()
	steps := []string{"session-journal-written", "session-worktree-created", "session-recorded", "session-journal-removed"}
	for _, step := range steps {
		// By "session-journal-removed", the journal is already gone, so a
		// retry with the same request is indistinguishable from a fresh
		// open: it opens a second session by design, not a defect. Only
		// "restart" exercises recovery at that step; the earlier three
		// steps still exercise both.
		modes := []string{"restart", "retry"}
		if step == "session-journal-removed" {
			modes = []string{"restart"}
		}
		for _, mode := range modes {
			t.Run(step+"/"+mode, func(t *testing.T) {
				opts, clone := projectFixture(t)
				home := filepath.Dir(clone)
				must(t, os.WriteFile(filepath.Join(clone, "README.md"), []byte("hi\n"), 0600))
				demoGit(t, home, "-C", clone, "add", "README.md")
				demoGit(t, home, "-C", clone, "-c", "user.name=Owner", "-c", "user.email=owner@example.invalid", "commit", "-qm", "base")
				demoGit(t, home, "-C", clone, "branch", "-M", "main")
				bare := filepath.Join(home, "remotes", "dagger", "dagger.git")
				must(t, os.MkdirAll(filepath.Dir(bare), 0700))
				demoGit(t, home, "clone", "--quiet", "--bare", "--no-hardlinks", clone, bare)
				demoGit(t, home, "-C", clone, "remote", "add", "upstream", bare)
				root := opts.Config.Root
				s, c := start(t, opts)
				added, err := c.AddProject(context.Background(), request(clone))
				must(t, err)
				writeCharter(t, c, added.Project.ID, "1. Keep changes small.\n")
				project := added.Project.ID
				req := SessionOpenRequest{Project: project, Profile: "default"}
				crash := errors.New("crash")
				s.boundary = func(name string) error {
					if name != step {
						return nil
					}
					return crash
				}
				_, err = c.OpenSession(context.Background(), req)
				assertCode(t, err, Internal)
				if !strings.Contains(err.Error(), "incomplete") {
					t.Fatal(err)
				}
				journalPath := filepath.Join(root, "session-open.json")
				data, jerr := os.ReadFile(journalPath)
				var journal pendingSession
				if step == "session-journal-removed" {
					if !os.IsNotExist(jerr) {
						t.Fatalf("journal after the final step: %v", jerr)
					}
					sessions, err := c.Sessions(context.Background(), project)
					must(t, err)
					if len(sessions.Sessions) != 1 {
						t.Fatalf("sessions after the final step: %+v", sessions)
					}
					journal.Workstream = sessions.Sessions[0].Workstream
				} else {
					must(t, jerr)
					must(t, json.Unmarshal(data, &journal))
				}
				id := journal.Workstream
				s.boundary = nil
				if mode == "restart" {
					must(t, s.Close())
					_, c = start(t, opts)
				} else {
					result, err := c.OpenSession(context.Background(), req)
					must(t, err)
					if result.Session.Workstream != id {
						t.Fatalf("retry created %s instead of finishing %s", result.Session.Workstream, id)
					}
				}
				sessions, err := c.Sessions(context.Background(), project)
				must(t, err)
				if len(sessions.Sessions) != 1 || sessions.Sessions[0].Workstream != id {
					t.Fatalf("recovery left %+v, want exactly one session %s", sessions.Sessions, id)
				}
				worktree := filepath.Join(root, sessionsDirectory, string(project), string(id))
				if info, err := os.Stat(worktree); err != nil || !info.IsDir() {
					t.Fatalf("session worktree missing after recovery: %v", err)
				}
				if _, err := os.Lstat(journalPath); !os.IsNotExist(err) {
					t.Fatalf("journal left behind after recovery: %v", err)
				}
			})
		}
	}
}

// TestSessionOpenRetryComparesTheRequestsProject shows that completing an
// interrupted open does not hand its session to a retry naming a different,
// equally valid project with the same profile and title: the retry is
// refused instead of silently receiving the other project's session.
func TestSessionOpenRetryComparesTheRequestsProject(t *testing.T) {
	t.Parallel()
	s, c, projectA, clone := newSessionFixture(t)
	home := filepath.Dir(clone)
	cloneB := filepath.Join(home, "second")
	must(t, os.MkdirAll(cloneB, 0700))
	demoGit(t, home, "init", "--quiet", cloneB)
	must(t, os.WriteFile(filepath.Join(cloneB, "README.md"), []byte("hi\n"), 0600))
	demoGit(t, home, "-C", cloneB, "add", "README.md")
	demoGit(t, home, "-C", cloneB, "-c", "user.name=Owner", "-c", "user.email=owner@example.invalid", "commit", "-qm", "base")
	demoGit(t, home, "-C", cloneB, "branch", "-M", "main")
	bareB := filepath.Join(home, "remotes", "second", "dagger.git")
	must(t, os.MkdirAll(filepath.Dir(bareB), 0700))
	demoGit(t, home, "clone", "--quiet", "--bare", "--no-hardlinks", cloneB, bareB)
	demoGit(t, home, "-C", cloneB, "remote", "add", "upstream", bareB)
	addedB, err := c.AddProject(context.Background(), ProjectAddRequest{Name: "second", Upstream: "second/second", Fork: "owner/second", Clone: cloneB})
	must(t, err)
	writeCharter(t, c, addedB.Project.ID, "1. Keep changes small.\n")
	projectB := addedB.Project.ID

	crash := errors.New("crash")
	s.boundary = func(name string) error {
		if name != "session-journal-written" {
			return nil
		}
		return crash
	}
	_, err = c.OpenSession(context.Background(), SessionOpenRequest{Project: projectA, Profile: "default"})
	assertCode(t, err, Internal)
	s.boundary = nil

	_, err = c.OpenSession(context.Background(), SessionOpenRequest{Project: projectB, Profile: "default"})
	assertCode(t, err, Conflict)

	sessionsB, err := c.Sessions(context.Background(), projectB)
	must(t, err)
	if len(sessionsB.Sessions) != 0 {
		t.Fatalf("the retry naming project B got a session: %+v", sessionsB)
	}
	sessionsA, err := c.Sessions(context.Background(), projectA)
	must(t, err)
	if len(sessionsA.Sessions) != 1 {
		t.Fatalf("project A's interrupted open did not finish: %+v", sessionsA)
	}
}

// newExclusionFixture is a bare, never-started project trace for testing
// that a scheduler role or pass leaves a session alone, with extraConfig
// appended to the top-level configuration.
func newExclusionFixture(t *testing.T, extraConfig string) (*config.Config, *trace.Repository) {
	t.Helper()
	home, err := os.MkdirTemp("", "sess-excl-")
	must(t, err)
	t.Cleanup(func() { os.RemoveAll(home) })
	root := filepath.Join(home, "root")
	must(t, os.MkdirAll(filepath.Join(root, "projects", string(project)), 0700))
	must(t, os.WriteFile(filepath.Join(root, "config.toml"), []byte(fmt.Sprintf(`version = 1
active_projects = [%q]
workspaces = "git"
[profiles.default]
agent = "claude"
model = "test"
%s
`, project, extraConfig)), 0600))
	must(t, os.WriteFile(filepath.Join(root, "projects", string(project), "config.toml"), []byte(`version = 1
upstream = "upstream/repo"
fork = "owner/repo"
clone = "`+filepath.Join(home, "clone")+`"
`), 0600))
	must(t, os.MkdirAll(filepath.Join(home, "clone"), 0700))
	cfg, err := config.Load(config.Options{Root: root})
	must(t, err)
	owner := trace.Actor{Kind: "owner", ID: "local"}
	repo, err := trace.Create(context.Background(), cfg.Root, cfg.Project, demoStart, owner)
	must(t, err)
	t.Cleanup(func() { repo.Close() })
	return cfg, repo
}

// createTestSession records a session workstream directly, without a
// running service or a worktree: enough to prove that a scheduler role or
// pass leaves it alone, whatever its kind-independent records say.
func createTestSession(t *testing.T, repository *trace.Repository) config.WorkstreamID {
	t.Helper()
	id, err := config.NewWorkstreamID()
	must(t, err)
	must(t, repository.CreateSessionWorkstream(context.Background(), id, "default", "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef", sessionBranch(id), "", demoStart, sessionActor))
	return id
}

// sessionSnapshot is a session's workflow state, operations and threads at
// one moment. Several exclusion tests give the session a workflow state,
// thread or operation of its own in setup, the same as the ordinary
// workstream alongside it, so an empty-state check would fail regardless of
// whether the pass under test ran for the session; capturing one before the
// pass runs and comparing it after shows whether the pass itself changed
// anything, whatever was there already.
type sessionSnapshot struct {
	states  map[string]trace.WorkflowState
	ops     []trace.OperationRecord
	threads []trace.Thread
}

func captureSessionSnapshot(t *testing.T, repository *trace.Repository, session config.WorkstreamID) sessionSnapshot {
	t.Helper()
	states, err := repository.WorkflowStates(session)
	must(t, err)
	ops, err := repository.Operations(session)
	must(t, err)
	threads, err := repository.Threads(session)
	must(t, err)
	return sessionSnapshot{states: maps.Clone(states), ops: ops, threads: threads}
}

// assertSessionUnchanged fails the test if the session's workflow state,
// operations or threads differ from before: the sign that the pass ran for
// it.
func assertSessionUnchanged(t *testing.T, repository *trace.Repository, session config.WorkstreamID, before sessionSnapshot) {
	t.Helper()
	after := captureSessionSnapshot(t, repository, session)
	if !reflect.DeepEqual(before.states, after.states) {
		t.Fatalf("session %s workflow state changed: %+v -> %+v", session, before.states, after.states)
	}
	if !reflect.DeepEqual(before.ops, after.ops) {
		t.Fatalf("session %s operations changed: %+v -> %+v", session, before.ops, after.ops)
	}
	if !reflect.DeepEqual(before.threads, after.threads) {
		t.Fatalf("session %s threads changed: %+v -> %+v", session, before.threads, after.threads)
	}
}

// drivenPass idles the service's controller when the fixture left one
// running, then drives pass once inside the repository's operation lock, so
// a hand-driven pass never races a live controller and is never retried
// over a conflict, whether the fixture's service is still live (the
// never-started fixtures below) or was already stopped and handed the
// trace directly (the mason, review, final-review and delivery fixtures).
func drivenPass(t *testing.T, s *Service, repository *trace.Repository, pass func() error) {
	t.Helper()
	if active := s.sole(); active != nil {
		must(t, s.halt(active))
	}
	must(t, repository.Serialize(pass))
}

func sessionHeader(id string, ws config.WorkstreamID, at time.Time) trace.Header {
	return trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: project, Workstream: ws, At: at, Actor: sessionActor, Cause: "fixture"}
}

// TestArchitectPassSkipsASessionAlongsideAHandedWorkstream shows that the
// architect's pass leaves a session alone, while a handed ordinary
// workstream still gets its first draft requested.
func TestArchitectPassSkipsASessionAlongsideAHandedWorkstream(t *testing.T) {
	t.Parallel()
	cfg, repo := newExclusionFixture(t, "")
	ctx := context.Background()
	must(t, repo.CreateWorkstream(ctx, stream, demoStart, sessionActor))
	_, err := repo.SetFeatureState(ctx, sessionHeader("handed", stream, demoStart), HandedState, "fixture hand-in")
	must(t, err)
	session := createTestSession(t, repo)
	_, err = repo.SetFeatureState(ctx, sessionHeader("handed", session, demoStart), HandedState, "fixture hand-in")
	must(t, err)
	before := captureSessionSnapshot(t, repo, session)
	s := &Service{cfg: cfg, projects: []*activeProject{runtimeFor(repo)}, options: Options{Architect: &Architect{}}}
	d := &drafter{S: s.intake().DrafterDependencies(), Repository: repo}
	drivenPass(t, s, repo, func() error { return d.Pass(ctx) })
	draft, err := repo.Workflow(stream, draftSubject)
	must(t, err)
	if draft.Value == "" {
		t.Fatal("the handed workstream's draft was not requested")
	}
	assertSessionUnchanged(t, repo, session, before)
}

// TestShedPassSkipsASessionAlongsideASketchedWorkstream shows that the
// shed's pass, which also assigns a committee, leaves a session alone while
// a sketched ordinary workstream enters the shed.
func TestShedPassSkipsASessionAlongsideASketchedWorkstream(t *testing.T) {
	t.Parallel()
	cfg, repo := newExclusionFixture(t, "[capacity]\ncommittee = 1\n")
	ctx := context.Background()
	must(t, repo.CreateWorkstream(ctx, stream, demoStart, sessionActor))
	must(t, repo.RecordDocuments(ctx, []trace.Document{
		{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: plan.SpecDocument, Revision: 1, Project: project, Workstream: stream, At: demoStart, Actor: sessionActor, Cause: "fixture"}, Path: plan.SpecPath, Content: validSpec},
		{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: plan.PlanDocument, Revision: 1, Project: project, Workstream: stream, At: demoStart, Actor: sessionActor, Cause: "fixture"}, Path: plan.PlanPath, Content: validPlan},
	}))
	_, err := repo.SetFeatureState(ctx, sessionHeader("sketched", stream, demoStart), SketchedState, "fixture draft accepted")
	must(t, err)
	session := createTestSession(t, repo)
	_, err = repo.SetFeatureState(ctx, sessionHeader("sketched", session, demoStart), SketchedState, "fixture draft accepted")
	must(t, err)
	before := captureSessionSnapshot(t, repo, session)
	s := &Service{cfg: cfg, projects: []*activeProject{runtimeFor(repo)}, options: Options{Committee: &Committee{}}}
	d := &debate{S: s.intake().DebateDependencies(), Repository: repo}
	drivenPass(t, s, repo, func() error { return d.Pass(ctx) })
	feature, err := repo.Workflow(stream, trace.FeatureSubject)
	must(t, err)
	if feature.Value != InShedState {
		t.Fatalf("the sketched workstream stayed %q, want %q", feature.Value, InShedState)
	}
	assertSessionUnchanged(t, repo, session, before)
}

// TestMasonsPassSkipsASessionAlongsideAReadyUnit shows that the mason
// controller leaves a session alone while a ready unit of an ordinary,
// building workstream starts.
func TestMasonsPassSkipsASessionAlongsideAReadyUnit(t *testing.T) {
	t.Parallel()
	f, _ := newMasonFixture(t, 1, independentPlan)
	base := strings.TrimSpace(demoGit(t, f.clone, "-C", f.clone, "rev-parse", "HEAD"))
	ordinary, repository := seedBuild(t, f, "resume", independentPlan, config.WorkspacesGit, base)
	session := createTestSession(t, repository)
	before := captureSessionSnapshot(t, repository, session)
	m := newMasonController(f.s, repository)
	drivenPass(t, f.s, repository, func() error { return m.Pass(context.Background()) })
	state, err := repository.Workflow(ordinary, trace.UnitSubject("resume"))
	must(t, err)
	if state.Value != UnitImplementing {
		t.Fatalf("the ready unit stayed %q, want %q", state.Value, UnitImplementing)
	}
	assertSessionUnchanged(t, repository, session, before)
}

// TestReviewersPassSkipsASessionAlongsideAReviewingUnit shows that the
// reviewer controller leaves a session alone while a reviewing unit of an
// ordinary workstream gets its reviewer thread.
func TestReviewersPassSkipsASessionAlongsideAReviewingUnit(t *testing.T) {
	t.Parallel()
	f, ordinary, repository := newReviewFixture(t, "resume")
	session := createTestSession(t, repository)
	before := captureSessionSnapshot(t, repository, session)
	r := &reviewers{masons: &masons{s: f.s, cfg: f.s.cfg, repository: repository}}
	drivenPass(t, f.s, repository, func() error { return r.Pass(context.Background()) })
	threads, err := repository.Threads(ordinary)
	must(t, err)
	found := false
	for _, th := range threads {
		if th.Identity.Role == reviewerRole {
			found = true
		}
	}
	if !found {
		t.Fatal("the reviewing unit's reviewer thread was not created")
	}
	assertSessionUnchanged(t, repository, session, before)
}

// TestUnitChecksPassSkipsASessionAlongsideACheckingUnit shows that the
// pre-review check run leaves a session alone while a checking unit of an
// ordinary workstream has its check requested.
func TestUnitChecksPassSkipsASessionAlongsideACheckingUnit(t *testing.T) {
	t.Parallel()
	f, _ := newMasonFixture(t, 1, independentPlan)
	base := strings.TrimSpace(demoGit(t, f.clone, "-C", f.clone, "rev-parse", "HEAD"))
	ordinary, repository := seedBuild(t, f, "resume", independentPlan, config.WorkspacesGit, base)
	seedChecking(t, f, repository, ordinary, "resume", resumeReport)
	session := createTestSession(t, repository)
	before := captureSessionSnapshot(t, repository, session)
	c := &checkers{masons: &masons{s: f.s, cfg: f.s.cfg, repository: repository}}
	drivenPass(t, f.s, repository, func() error { return c.Pass(context.Background()) })
	ops, err := repository.Operations(ordinary)
	must(t, err)
	if len(ops) == 0 {
		t.Fatal("the checking unit's check was not requested")
	}
	assertSessionUnchanged(t, repository, session, before)
}

// TestBaseRefreshPassSkipsASessionAlongsideADependentWorkstream shows that
// base refresh leaves a session alone, even one given the same declared
// dependency and seal, while an ordinary dependent workstream still gets
// its refresh requested.
func TestBaseRefreshPassSkipsASessionAlongsideADependentWorkstream(t *testing.T) {
	t.Parallel()
	cfg, repo := newExclusionFixture(t, "")
	ctx := context.Background()
	must(t, repo.CreateWorkstream(ctx, quiet, demoStart, sessionActor))
	must(t, repo.CreateWorkstream(ctx, stream, demoStart, sessionActor))
	_, err := repo.SetWorkstreamBase(ctx, stream, quiet, 0, demoStart)
	must(t, err)
	sealFixtureStream(t, repo, stream)
	session := createTestSession(t, repo)
	_, err = repo.SetWorkstreamBase(ctx, session, quiet, 0, demoStart)
	must(t, err)
	sealFixtureStream(t, repo, session)
	before := captureSessionSnapshot(t, repo, session)
	clock := &demoClock{At: demoStart}
	s := &Service{cfg: cfg, projects: []*activeProject{runtimeFor(repo)}, options: Options{Reconciliation: reconcile.Options{Now: clock.Now}}}
	b := &baseRefresher{s: s, repository: repo}
	drivenPass(t, s, repo, func() error { return b.Pass(ctx) })
	if baseRefreshOperationCount2(t, repo, stream) == 0 {
		t.Fatal("the dependent workstream's base refresh was not requested")
	}
	assertSessionUnchanged(t, repo, session, before)
}

func baseRefreshOperationCount2(t *testing.T, repo *trace.Repository, ws config.WorkstreamID) int {
	t.Helper()
	ops, err := repo.Operations(ws)
	must(t, err)
	n := 0
	for _, o := range ops {
		if o.Operation.Action == baseRefreshAction {
			n++
		}
	}
	return n
}

// sealFixtureStream records a minimal seal document for ws, enough for the
// passes that require a sealed workstream to act.
func sealFixtureStream(t *testing.T, repo *trace.Repository, ws config.WorkstreamID) {
	t.Helper()
	sealed := seal.Seal{Version: seal.Version, Seal: 1, Round: 1, Revision: shed.Pin{Spec: 1, Plan: 1}, SpecHash: seal.SpecHash("spec"),
		Base: seal.Base{Remote: "upstream", Branch: "main", Commit: "upstream-sha"}, Branch: featureBranch(ws), Workspace: "/owned/feature"}
	data, err := seal.Encode(sealed)
	must(t, err)
	h := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: seal.DocumentID, Revision: 1, Project: project, Workstream: ws, At: demoStart, Actor: sessionActor, Cause: "seal"}
	must(t, repo.RecordDocuments(context.Background(), []trace.Document{{Header: h, Path: seal.Path, Content: string(data)}}))
}

// TestDriftPassSkipsASessionAlongsideABuildingWorkstream shows that drift
// rebase scheduling leaves a session alone, even one the owner asked for a
// drift rebase of, while an ordinary building, sealed workstream the owner
// asked for one gets it requested.
func TestDriftPassSkipsASessionAlongsideABuildingWorkstream(t *testing.T) {
	t.Parallel()
	cfg, repo := newExclusionFixture(t, "")
	ctx := context.Background()
	must(t, repo.CreateWorkstream(ctx, stream, demoStart, sessionActor))
	_, err := repo.SetFeatureState(ctx, sessionHeader("building", stream, demoStart), BuildingState, "fixture sealed")
	must(t, err)
	sealFixtureStream(t, repo, stream)
	askDriftFixture(t, repo, stream)
	session := createTestSession(t, repo)
	_, err = repo.SetFeatureState(ctx, sessionHeader("building", session, demoStart), BuildingState, "fixture sealed")
	must(t, err)
	sealFixtureStream(t, repo, session)
	askDriftFixture(t, repo, session)
	before := captureSessionSnapshot(t, repo, session)
	clock := &demoClock{At: demoStart}
	store, _, err := runtime.Open(runtime.Inputs{Config: cfg, Workstreams: map[config.ProjectID][]config.WorkstreamID{project: {stream, session}}})
	must(t, err)
	defer store.Close()
	s := &Service{cfg: cfg, projects: []*activeProject{runtimeFor(repo)}, options: Options{Reconciliation: reconcile.Options{Now: clock.Now}}, store: store}
	f := &foreman{masons: &masons{s: s, cfg: cfg, repository: repo}}
	var requested []config.WorkstreamID
	drivenPass(t, s, repo, func() error {
		var err error
		requested, err = f.requestDrifts(ctx, func(ws config.WorkstreamID) (string, error) { return "owner requested", nil })
		return err
	})
	found := false
	for _, ws := range requested {
		if ws == stream {
			found = true
		}
		if ws == session {
			t.Fatalf("drift was requested for session %s", session)
		}
	}
	if !found {
		t.Fatal("the building workstream's drift rebase was not requested")
	}
	assertSessionUnchanged(t, repo, session, before)
}

// askDriftFixture records that the owner asked for a drift rebase of ws,
// the way askDrift does, without needing a live service.
func askDriftFixture(t *testing.T, repo *trace.Repository, ws config.WorkstreamID) {
	t.Helper()
	id := fmt.Sprintf("%s-1", driftRequestSubject)
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: project, Workstream: ws, At: demoStart, Actor: ownerActor, Cause: driftRequestSubject}
	_, err := repo.Transact(context.Background(), trace.Transaction{Transition: trace.Transition{Header: h, Subject: driftRequestSubject, To: "requested-1", Reason: "fixture"}})
	must(t, err)
}

// TestFinalReviewPassSkipsASessionAlongsideAFullyMergedWorkstream shows
// that final review (which requests the committee's round) leaves a
// session alone while a building, sealed, fully-merged ordinary workstream
// assembles.
func TestFinalReviewPassSkipsASessionAlongsideAFullyMergedWorkstream(t *testing.T) {
	t.Parallel()
	f, stream, repository, a := newFinalFixture(t, "assemble")
	mergeDirectly(t, f, repository, stream, "resume", map[string]string{"resume.go": "package demo\n"})
	mergeDirectly(t, f, repository, stream, "dedupe", map[string]string{"dedupe.go": "package demo\n"})
	session := createTestSession(t, repository)
	before := captureSessionSnapshot(t, repository, session)
	drivenPass(t, f.s, repository, func() error { return a.Pass(context.Background()) })
	feature, err := repository.Workflow(stream, trace.FeatureSubject)
	must(t, err)
	if feature.Value != AssembledState {
		t.Fatalf("the fully-merged workstream stayed %q, want %q", feature.Value, AssembledState)
	}
	assertSessionUnchanged(t, repository, session, before)
}

// approvedDeliveryFixture builds an ordinary, assembled workstream and
// records the owner's approval of its delivery, through the same path the
// publication tests use, returning the approval so an alongside session can
// be given the identical one.
func approvedDeliveryFixture(t *testing.T) (*shedFixture, config.WorkstreamID, *trace.Repository, DeliveryApproval) {
	t.Helper()
	f, stream, repository, _ := deliveryFixture(t)
	presented, api := f.s.deliveryPresentation(context.Background(), string(stream))
	if api != nil {
		t.Fatal(api)
	}
	approval, api := f.s.approveDelivery(context.Background(), string(stream), DeliveryDecision{Review: presented.Report.Review, ReviewRevision: presented.ReviewRevision, Commit: presented.Report.Commit, DraftHash: presented.DraftHash})
	if api != nil {
		t.Fatal(api)
	}
	return f, stream, repository, approval
}

// approveSessionDelivery gives session the assembled state, the same final
// report and the identical delivery approval as approval, recorded directly
// on the trace: enough that, if the publisher's pass did not skip sessions,
// it would request a publish for it exactly as it does for the ordinary
// workstream approved alongside it.
func approveSessionDelivery(t *testing.T, f *shedFixture, repository *trace.Repository, session config.WorkstreamID, approval DeliveryApproval) {
	t.Helper()
	ctx := context.Background()
	// Unlike every other fixture in this file, the repository here comes
	// from approvedDeliveryFixture's live AddProject call, so its project ID
	// is not the package's fixed "project" constant that sessionHeader
	// assumes; build the header from the repository's own project instead.
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "assembled", Revision: 1, Project: repository.Project(), Workstream: session, At: demoStart, Actor: sessionActor, Cause: "fixture"}
	_, err := repository.SetFeatureState(ctx, h, AssembledState, "fixture assembled")
	must(t, err)
	report := FinalReport{Review: approval.Review, Outcome: finalReviewed, Branch: featureBranch(session), Commit: approval.Commit, Seal: approval.Seal, SpecHash: approval.SpecHash, Spec: approval.Spec, Plan: approval.Plan, Charter: approval.Charter, Summary: "session delivery"}
	content, err := json.Marshal(report)
	must(t, err)
	must(t, repository.RecordDocuments(ctx, []trace.Document{{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: finalReportDocument, Revision: 1, Project: repository.Project(), Workstream: session, At: demoStart, Actor: sessionActor, Cause: "final-review"}, Path: finalReportPath, Content: string(content)}}))
	data, err := json.Marshal(approval)
	must(t, err)
	must(t, repository.RecordDocuments(ctx, []trace.Document{{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: deliveryDocument, Revision: 1, Project: repository.Project(), Workstream: session, At: demoStart, Actor: sessionActor, Cause: "delivery-approval"}, Path: deliveryPath, Content: string(data)}}))
}

// TestPublisherPassSkipsASessionAlongsideAnApprovedWorkstream shows that
// delivery leaves a session alone, even one given the same approval, while
// an ordinary, assembled and approved workstream gets its publish
// requested.
func TestPublisherPassSkipsASessionAlongsideAnApprovedWorkstream(t *testing.T) {
	t.Parallel()
	f, stream, repository, decision := approvedDeliveryFixture(t)
	session := createTestSession(t, repository)
	approveSessionDelivery(t, f, repository, session, decision)
	before := captureSessionSnapshot(t, repository, session)
	p := &publisher{s: f.s, repository: repository}
	drivenPass(t, f.s, repository, func() error { return p.Pass(context.Background()) })
	ops, err := repository.Operations(stream)
	must(t, err)
	found := false
	for _, o := range ops {
		if o.Operation.Action == PublishAction || o.Operation.Action == publishUpstreamAction {
			found = true
		}
	}
	if !found {
		t.Fatal("the approved workstream's publish was not requested")
	}
	assertSessionUnchanged(t, repository, session, before)
}

// TestEnsureChiefsOfStaffSkipsASessionAlongsideAnOrdinaryWorkstream shows
// that the chief-of-staff backfill a project's opening runs leaves a
// session without the thread it never has, while an ordinary workstream
// keeps the thread its own creation already gave it.
func TestEnsureChiefsOfStaffSkipsASessionAlongsideAnOrdinaryWorkstream(t *testing.T) {
	t.Parallel()
	_, repo := newExclusionFixture(t, "")
	ctx := context.Background()
	must(t, repo.CreateWorkstream(ctx, stream, demoStart, sessionActor))
	session := createTestSession(t, repo)
	before := captureSessionSnapshot(t, repo, session)
	must(t, ensureChiefsOfStaff(ctx, repo, demoStart.Add(time.Hour)))
	if _, err := repo.ChiefOfStaffThread(stream); err != nil {
		t.Fatalf("the ordinary workstream's chief-of-staff thread: %v", err)
	}
	if _, err := repo.ChiefOfStaffThread(session); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the session got a chief-of-staff thread: %v", err)
	}
	assertSessionUnchanged(t, repo, session, before)
}
