package executionjourney

import (
	context "context"
	json "encoding/json"
	errors "errors"
	fmt "fmt"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/intake"
	maps "maps"
	os "os"
	filepath "path/filepath"
	reflect "reflect"
	strings "strings"
	testing "testing"
	time "time"

	config "github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/lifecycle"
	plan "github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/projectstore"
	reconcile "github.com/kpenfound/osmia/internal/reconcile"
	runtime "github.com/kpenfound/osmia/internal/runtime"
	seal "github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	shed "github.com/kpenfound/osmia/internal/shed"
	trace "github.com/kpenfound/osmia/internal/trace"
)

func newSessionFixture(t *testing.T) (*Service, *Client, config.ProjectID, string) {
	t.Helper()
	opts, clone := projectFixture(t)
	home := filepath.Dir(clone)
	fixturekit.Must(t, os.WriteFile(filepath.Join(clone, "README.md"), []byte("hi\n"), 0600))
	fixturekit.DemoGit(t, home, "-C", clone, "add", "README.md")
	fixturekit.DemoGit(t, home, "-C", clone, "-c", "user.name=Owner", "-c", "user.email=owner@example.invalid", "commit", "-qm", "base")
	fixturekit.DemoGit(t, home, "-C", clone, "branch", "-M", "main")
	bare := filepath.Join(home, "remotes", "dagger", "dagger.git")
	fixturekit.Must(t, os.MkdirAll(filepath.Dir(bare), 0700))
	fixturekit.DemoGit(t, home, "clone", "--quiet", "--bare", "--no-hardlinks", clone, bare)
	fixturekit.DemoGit(t, home, "-C", clone, "remote", "add", "upstream", bare)
	s, c := start(t, opts)
	added, err := c.AddProject(context.Background(), fixturekit.Request(clone))
	fixturekit.Must(t, err)
	s.cfg = s.current()
	writeCharter(t, c, added.Project.ID, "1. Keep changes small.\n")
	return s, c, added.Project.ID, clone
}

func TestSessionOpenRecordsWorkstreamAndWorktree(t *testing.T) {
	t.Parallel()
	s, c, project, clone := newSessionFixture(t)
	home := filepath.Dir(clone)
	head := strings.TrimSpace(fixturekit.DemoGit(t, home, "-C", clone, "rev-parse", "HEAD"))
	resp, err := c.OpenSession(context.Background(), api.SessionOpenRequest{Project: project, Profile: "default", Title: "exploring"})
	fixturekit.Must(t, err)
	session := resp.Session
	if session.Project != project || session.Profile != "default" || session.Base != head || session.Branch == "" || session.Title != "exploring" || session.State != trace.SessionIdle || session.Workstream == "" {
		t.Fatalf("session view: %+v, want base %s", session, head)
	}
	listed, err := c.Status(context.Background(), session.Workstream)
	fixturekit.Must(t, err)
	if listed.Kind != trace.SessionKind || listed.Profile != "default" || listed.Branch != session.Branch || listed.Base != head {
		t.Fatalf("status view: %+v", listed)
	}
	read, err := c.Session(context.Background(), session.Workstream)
	fixturekit.Must(t, err)
	if read.Session != session {
		t.Fatalf("read %+v, want %+v", read.Session, session)
	}
	worktree := filepath.Join(s.cfg.Root.String(), service.TestSessionsDirectory, string(project), string(session.Workstream))
	info, err := os.Stat(worktree)
	fixturekit.Must(t, err)
	if !info.IsDir() {
		t.Fatalf("%s is not a directory", worktree)
	}
	gotHead := strings.TrimSpace(fixturekit.DemoGit(t, home, "-C", worktree, "rev-parse", "HEAD"))
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
	_, err := c.OpenSession(context.Background(), api.SessionOpenRequest{Project: unknown, Profile: "default"})
	fixturekit.AssertCode(t, err, api.NotFound)
	sessions, err := c.Sessions(context.Background(), "")
	fixturekit.Must(t, err)
	if len(sessions.Sessions) != 0 {
		t.Fatalf("unknown project left a session: %+v", sessions)
	}
}

func TestSessionOpenRefusesUnknownOrMissingProfile(t *testing.T) {
	t.Parallel()
	s, c, project, _ := newSessionFixture(t)
	for _, name := range []string{"", "nonexistent", "other"} {
		_, err := c.OpenSession(context.Background(), api.SessionOpenRequest{Project: project, Profile: name})
		fixturekit.AssertCode(t, err, api.Validation)
	}
	sessions, err := c.Sessions(context.Background(), project)
	fixturekit.Must(t, err)
	if len(sessions.Sessions) != 0 {
		t.Fatalf("an invalid profile left a session: %+v", sessions)
	}
	if entries, err := os.ReadDir(filepath.Join(s.cfg.Root.String(), service.TestSessionsDirectory, string(project))); err == nil && len(entries) != 0 {
		t.Fatalf("an invalid profile left a worktree: %+v", entries)
	} else if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func TestSessionOpenRecoversAtEachStep(t *testing.T) {
	t.Parallel()
	steps := []string{"session-journal-written", "session-worktree-created", "session-recorded", "session-journal-removed"}
	for _, step := range steps {

		modes := []string{"restart", "retry"}
		if step == "session-journal-removed" {
			modes = []string{"restart"}
		}
		for _, mode := range modes {
			t.Run(step+"/"+mode, func(t *testing.T) {
				opts, clone := projectFixture(t)
				home := filepath.Dir(clone)
				fixturekit.Must(t, os.WriteFile(filepath.Join(clone, "README.md"), []byte("hi\n"), 0600))
				fixturekit.DemoGit(t, home, "-C", clone, "add", "README.md")
				fixturekit.DemoGit(t, home, "-C", clone, "-c", "user.name=Owner", "-c", "user.email=owner@example.invalid", "commit", "-qm", "base")
				fixturekit.DemoGit(t, home, "-C", clone, "branch", "-M", "main")
				bare := filepath.Join(home, "remotes", "dagger", "dagger.git")
				fixturekit.Must(t, os.MkdirAll(filepath.Dir(bare), 0700))
				fixturekit.DemoGit(t, home, "clone", "--quiet", "--bare", "--no-hardlinks", clone, bare)
				fixturekit.DemoGit(t, home, "-C", clone, "remote", "add", "upstream", bare)
				root := opts.Config.Root
				s, c := start(t, opts)
				added, err := c.AddProject(context.Background(), fixturekit.Request(clone))
				fixturekit.Must(t, err)
				writeCharter(t, c, added.Project.ID, "1. Keep changes small.\n")
				project := added.Project.ID
				req := api.SessionOpenRequest{Project: project, Profile: "default"}
				crash := errors.New("crash")
				s.boundary = func(name string) error {
					if name != step {
						return nil
					}
					return crash
				}
				_, err = c.OpenSession(context.Background(), req)
				fixturekit.AssertCode(t, err, api.Internal)
				if !strings.Contains(err.Error(), "incomplete") {
					t.Fatal(err)
				}
				journalPath := filepath.Join(root, "session-open.json")
				data, jerr := os.ReadFile(journalPath)
				var journal service.TestPendingSession
				if step == "session-journal-removed" {
					if !os.IsNotExist(jerr) {
						t.Fatalf("journal after the final step: %v", jerr)
					}
					sessions, err := c.Sessions(context.Background(), project)
					fixturekit.Must(t, err)
					if len(sessions.Sessions) != 1 {
						t.Fatalf("sessions after the final step: %+v", sessions)
					}
					journal.Workstream = sessions.Sessions[0].Workstream
				} else {
					fixturekit.Must(t, jerr)
					fixturekit.Must(t, json.Unmarshal(data, &journal))
				}
				id := journal.Workstream
				s.boundary = nil
				if mode == "restart" {
					fixturekit.Must(t, s.Close())
					_, c = start(t, opts)
				} else {
					result, err := c.OpenSession(context.Background(), req)
					fixturekit.Must(t, err)
					if result.Session.Workstream != id {
						t.Fatalf("retry created %s instead of finishing %s", result.Session.Workstream, id)
					}
				}
				sessions, err := c.Sessions(context.Background(), project)
				fixturekit.Must(t, err)
				if len(sessions.Sessions) != 1 || sessions.Sessions[0].Workstream != id {
					t.Fatalf("recovery left %+v, want exactly one session %s", sessions.Sessions, id)
				}
				worktree := filepath.Join(root, service.TestSessionsDirectory, string(project), string(id))
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

func TestSessionOpenRetryComparesTheRequestsProject(t *testing.T) {
	t.Parallel()
	s, c, projectA, clone := newSessionFixture(t)
	home := filepath.Dir(clone)
	cloneB := filepath.Join(home, "second")
	fixturekit.Must(t, os.MkdirAll(cloneB, 0700))
	fixturekit.DemoGit(t, home, "init", "--quiet", cloneB)
	fixturekit.Must(t, os.WriteFile(filepath.Join(cloneB, "README.md"), []byte("hi\n"), 0600))
	fixturekit.DemoGit(t, home, "-C", cloneB, "add", "README.md")
	fixturekit.DemoGit(t, home, "-C", cloneB, "-c", "user.name=Owner", "-c", "user.email=owner@example.invalid", "commit", "-qm", "base")
	fixturekit.DemoGit(t, home, "-C", cloneB, "branch", "-M", "main")
	bareB := filepath.Join(home, "remotes", "second", "dagger.git")
	fixturekit.Must(t, os.MkdirAll(filepath.Dir(bareB), 0700))
	fixturekit.DemoGit(t, home, "clone", "--quiet", "--bare", "--no-hardlinks", cloneB, bareB)
	fixturekit.DemoGit(t, home, "-C", cloneB, "remote", "add", "upstream", bareB)
	addedB, err := c.AddProject(context.Background(), api.ProjectAddRequest{Name: "second", Upstream: "second/second", Fork: "owner/second", Clone: cloneB})
	fixturekit.Must(t, err)
	writeCharter(t, c, addedB.Project.ID, "1. Keep changes small.\n")
	projectB := addedB.Project.ID

	crash := errors.New("crash")
	s.boundary = func(name string) error {
		if name != "session-journal-written" {
			return nil
		}
		return crash
	}
	_, err = c.OpenSession(context.Background(), api.SessionOpenRequest{Project: projectA, Profile: "default"})
	fixturekit.AssertCode(t, err, api.Internal)
	s.boundary = nil

	_, err = c.OpenSession(context.Background(), api.SessionOpenRequest{Project: projectB, Profile: "default"})
	fixturekit.AssertCode(t, err, api.Conflict)

	sessionsB, err := c.Sessions(context.Background(), projectB)
	fixturekit.Must(t, err)
	if len(sessionsB.Sessions) != 0 {
		t.Fatalf("the retry naming project B got a session: %+v", sessionsB)
	}
	sessionsA, err := c.Sessions(context.Background(), projectA)
	fixturekit.Must(t, err)
	if len(sessionsA.Sessions) != 1 {
		t.Fatalf("project A's interrupted open did not finish: %+v", sessionsA)
	}
}

func newExclusionFixture(t *testing.T, extraConfig string) (*config.Config, *trace.Repository) {
	t.Helper()
	home, err := os.MkdirTemp("", "sess-excl-")
	fixturekit.Must(t, err)
	t.Cleanup(func() { os.RemoveAll(home) })
	root := filepath.Join(home, "root")
	fixturekit.Must(t, os.MkdirAll(root, 0700))
	fixturekit.Must(t, os.WriteFile(filepath.Join(root, "config.toml"), []byte(fmt.Sprintf(`version = 1
workspaces = "git"
[profiles.default]
agent = "claude"
model = "test"
%s
`, extraConfig)), 0600))
	fixturekit.RegisterProject(t, root, fixturekit.Project, `version = 1
upstream = "upstream/repo"
fork = "owner/repo"
clone = "`+filepath.Join(home, "clone")+`"
`)
	fixturekit.Must(t, os.MkdirAll(filepath.Join(home, "clone"), 0700))
	cfg, err := projectstore.LoadConfig(config.Options{Root: root})
	fixturekit.Must(t, err)
	owner := trace.Actor{Kind: "owner", ID: "local"}
	repo, err := trace.Create(context.Background(), cfg.Root, cfg.Project, fixturekit.DemoStart, owner)
	fixturekit.Must(t, err)
	t.Cleanup(func() { repo.Close() })
	return cfg, repo
}

func createTestSession(t *testing.T, repository *trace.Repository) config.WorkstreamID {
	t.Helper()
	id, err := config.NewWorkstreamID()
	fixturekit.Must(t, err)
	fixturekit.Must(t, repository.CreateSessionWorkstream(context.Background(), id, "default", "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef", service.TestSessionBranch(id), "", fixturekit.DemoStart, service.TestSessionActor))
	return id
}

type sessionSnapshot struct {
	states  map[string]trace.WorkflowState
	ops     []trace.OperationRecord
	threads []trace.Thread
}

func captureSessionSnapshot(t *testing.T, repository *trace.Repository, session config.WorkstreamID) sessionSnapshot {
	t.Helper()
	states, err := repository.WorkflowStates(session)
	fixturekit.Must(t, err)
	ops, err := repository.Operations(session)
	fixturekit.Must(t, err)
	threads, err := repository.Threads(session)
	fixturekit.Must(t, err)
	return sessionSnapshot{states: maps.Clone(states), ops: ops, threads: threads}
}

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

func drivenPass(t *testing.T, s *Service, repository *trace.Repository, pass func() error) {
	t.Helper()
	if active := s.sole(); active != nil {
		fixturekit.Must(t, s.halt(active))
	}
	fixturekit.Must(t, repository.Serialize(pass))
}

func sessionHeader(id string, ws config.WorkstreamID, at time.Time) trace.Header {
	return trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: fixturekit.Project, Workstream: ws, At: at, Actor: service.TestSessionActor, Cause: "fixture"}
}

func TestArchitectPassSkipsASessionAlongsideAHandedWorkstream(t *testing.T) {
	t.Parallel()
	cfg, repo := newExclusionFixture(t, "")
	ctx := context.Background()
	fixturekit.Must(t, repo.CreateWorkstream(ctx, fixturekit.Stream, fixturekit.DemoStart, service.TestSessionActor))
	_, err := repo.SetFeatureState(ctx, sessionHeader("handed", fixturekit.Stream, fixturekit.DemoStart), intake.HandedState, "fixture hand-in")
	fixturekit.Must(t, err)
	session := createTestSession(t, repo)
	_, err = repo.SetFeatureState(ctx, sessionHeader("handed", session, fixturekit.DemoStart), intake.HandedState, "fixture hand-in")
	fixturekit.Must(t, err)
	before := captureSessionSnapshot(t, repo, session)
	s := &Service{cfg: cfg, projects: []*activeProject{runtimeFor(repo)}, options: Options{Options: service.Options{Architect: &intake.Architect{}}}}
	d := &intake.Drafter{S: s.intake().DrafterDependencies(), Repository: repo}
	drivenPass(t, s, repo, func() error { return d.Pass(ctx) })
	draft, err := repo.Workflow(fixturekit.Stream, intake.DraftSubject)
	fixturekit.Must(t, err)
	if draft.Value == "" {
		t.Fatal("the handed workstream's draft was not requested")
	}
	assertSessionUnchanged(t, repo, session, before)
}

func TestShedPassSkipsASessionAlongsideASketchedWorkstream(t *testing.T) {
	t.Parallel()
	cfg, repo := newExclusionFixture(t, "[capacity]\ncommittee = 1\n")
	ctx := context.Background()
	fixturekit.Must(t, repo.CreateWorkstream(ctx, fixturekit.Stream, fixturekit.DemoStart, service.TestSessionActor))
	fixturekit.Must(t, repo.RecordDocuments(ctx, []trace.Document{
		{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: plan.SpecDocument, Revision: 1, Project: fixturekit.Project, Workstream: fixturekit.Stream, At: fixturekit.DemoStart, Actor: service.TestSessionActor, Cause: "fixture"}, Path: plan.SpecPath, Content: fixturekit.ValidSpec},
		{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: plan.PlanDocument, Revision: 1, Project: fixturekit.Project, Workstream: fixturekit.Stream, At: fixturekit.DemoStart, Actor: service.TestSessionActor, Cause: "fixture"}, Path: plan.PlanPath, Content: fixturekit.ValidPlan},
	}))
	_, err := repo.SetFeatureState(ctx, sessionHeader("sketched", fixturekit.Stream, fixturekit.DemoStart), intake.SketchedState, "fixture draft accepted")
	fixturekit.Must(t, err)
	session := createTestSession(t, repo)
	_, err = repo.SetFeatureState(ctx, sessionHeader("sketched", session, fixturekit.DemoStart), intake.SketchedState, "fixture draft accepted")
	fixturekit.Must(t, err)
	before := captureSessionSnapshot(t, repo, session)
	s := &Service{cfg: cfg, projects: []*activeProject{runtimeFor(repo)}, options: Options{Options: service.Options{Committee: &intake.Committee{}}}}
	d := &intake.Debate{S: s.intake().DebateDependencies(), Repository: repo}
	drivenPass(t, s, repo, func() error { return d.Pass(ctx) })
	feature, err := repo.Workflow(fixturekit.Stream, trace.FeatureSubject)
	fixturekit.Must(t, err)
	if feature.Value != intake.InShedState {
		t.Fatalf("the sketched workstream stayed %q, want %q", feature.Value, intake.InShedState)
	}
	assertSessionUnchanged(t, repo, session, before)
}

func TestMasonsPassSkipsASessionAlongsideAReadyUnit(t *testing.T) {
	t.Parallel()
	f, _ := newMasonFixture(t, 1, independentPlan)
	base := strings.TrimSpace(fixturekit.DemoGit(t, f.clone, "-C", f.clone, "rev-parse", "HEAD"))
	ordinary, repository := seedBuild(t, f, "resume", independentPlan, config.WorkspacesGit, base)
	session := createTestSession(t, repository)
	before := captureSessionSnapshot(t, repository, session)
	m := newMasonController(f.s, repository)
	drivenPass(t, f.s, repository, func() error { return m.Pass(context.Background()) })
	state, err := repository.Workflow(ordinary, trace.UnitSubject("resume"))
	fixturekit.Must(t, err)
	if state.Value != intake.UnitImplementing {
		t.Fatalf("the ready unit stayed %q, want %q", state.Value, intake.UnitImplementing)
	}
	assertSessionUnchanged(t, repository, session, before)
}

func TestReviewersPassSkipsASessionAlongsideAReviewingUnit(t *testing.T) {
	t.Parallel()
	f, ordinary, repository := newReviewFixture(t, "resume")
	session := createTestSession(t, repository)
	before := captureSessionSnapshot(t, repository, session)
	r := &reviewers{masons: &masons{s: f.s, cfg: f.s.cfg, repository: repository}}
	drivenPass(t, f.s, repository, func() error { return r.Pass(context.Background()) })
	threads, err := repository.Threads(ordinary)
	fixturekit.Must(t, err)
	found := false
	for _, th := range threads {
		if th.Identity.Role == intake.ReviewerRole {
			found = true
		}
	}
	if !found {
		t.Fatal("the reviewing unit's reviewer thread was not created")
	}
	assertSessionUnchanged(t, repository, session, before)
}

func TestUnitChecksPassSkipsASessionAlongsideACheckingUnit(t *testing.T) {
	t.Parallel()
	f, _ := newMasonFixture(t, 1, independentPlan)
	base := strings.TrimSpace(fixturekit.DemoGit(t, f.clone, "-C", f.clone, "rev-parse", "HEAD"))
	ordinary, repository := seedBuild(t, f, "resume", independentPlan, config.WorkspacesGit, base)
	seedChecking(t, f, repository, ordinary, "resume", resumeReport)
	session := createTestSession(t, repository)
	before := captureSessionSnapshot(t, repository, session)
	c := &checkers{masons: &masons{s: f.s, cfg: f.s.cfg, repository: repository}}
	drivenPass(t, f.s, repository, func() error { return c.Pass(context.Background()) })
	ops, err := repository.Operations(ordinary)
	fixturekit.Must(t, err)
	if len(ops) == 0 {
		t.Fatal("the checking unit's check was not requested")
	}
	assertSessionUnchanged(t, repository, session, before)
}

func TestBaseRefreshPassSkipsASessionAlongsideADependentWorkstream(t *testing.T) {
	t.Parallel()
	cfg, repo := newExclusionFixture(t, "")
	ctx := context.Background()
	fixturekit.Must(t, repo.CreateWorkstream(ctx, fixturekit.Quiet, fixturekit.DemoStart, service.TestSessionActor))
	fixturekit.Must(t, repo.CreateWorkstream(ctx, fixturekit.Stream, fixturekit.DemoStart, service.TestSessionActor))
	_, err := repo.SetWorkstreamBase(ctx, fixturekit.Stream, fixturekit.Quiet, 0, fixturekit.DemoStart)
	fixturekit.Must(t, err)
	sealFixtureStream(t, repo, fixturekit.Stream)
	session := createTestSession(t, repo)
	_, err = repo.SetWorkstreamBase(ctx, session, fixturekit.Quiet, 0, fixturekit.DemoStart)
	fixturekit.Must(t, err)
	sealFixtureStream(t, repo, session)
	before := captureSessionSnapshot(t, repo, session)
	clock := &fixturekit.DemoClock{At: fixturekit.DemoStart}
	s := &Service{cfg: cfg, projects: []*activeProject{runtimeFor(repo)}, options: Options{Options: service.Options{Reconciliation: reconcile.Options{Now: clock.Now}}}}
	b := &lifecycle.BaseRefresher{S: s.lifecycle(), Repository: repo}
	drivenPass(t, s, repo, func() error { return b.Pass(ctx) })
	if baseRefreshOperationCount2(t, repo, fixturekit.Stream) == 0 {
		t.Fatal("the dependent workstream's base refresh was not requested")
	}
	assertSessionUnchanged(t, repo, session, before)
}

func baseRefreshOperationCount2(t *testing.T, repo *trace.Repository, ws config.WorkstreamID) int {
	t.Helper()
	ops, err := repo.Operations(ws)
	fixturekit.Must(t, err)
	n := 0
	for _, o := range ops {
		if o.Operation.Action == service.TestBaseRefreshAction {
			n++
		}
	}
	return n
}

func sealFixtureStream(t *testing.T, repo *trace.Repository, ws config.WorkstreamID) {
	t.Helper()
	sealed := seal.Seal{Version: seal.Version, Seal: 1, Round: 1, Revision: shed.Pin{Spec: 1, Plan: 1}, SpecHash: seal.SpecHash("spec"),
		Base: seal.Base{Remote: "upstream", Branch: "main", Commit: "upstream-sha"}, Branch: intake.FeatureBranch(ws), Workspace: "/owned/feature"}
	data, err := seal.Encode(sealed)
	fixturekit.Must(t, err)
	h := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: seal.DocumentID, Revision: 1, Project: fixturekit.Project, Workstream: ws, At: fixturekit.DemoStart, Actor: service.TestSessionActor, Cause: "seal"}
	fixturekit.Must(t, repo.RecordDocuments(context.Background(), []trace.Document{{Header: h, Path: seal.Path, Content: string(data)}}))
}

func TestDriftPassSkipsASessionAlongsideABuildingWorkstream(t *testing.T) {
	t.Parallel()
	cfg, repo := newExclusionFixture(t, "")
	ctx := context.Background()
	fixturekit.Must(t, repo.CreateWorkstream(ctx, fixturekit.Stream, fixturekit.DemoStart, service.TestSessionActor))
	_, err := repo.SetFeatureState(ctx, sessionHeader("building", fixturekit.Stream, fixturekit.DemoStart), intake.BuildingState, "fixture sealed")
	fixturekit.Must(t, err)
	sealFixtureStream(t, repo, fixturekit.Stream)
	askDriftFixture(t, repo, fixturekit.Stream)
	session := createTestSession(t, repo)
	_, err = repo.SetFeatureState(ctx, sessionHeader("building", session, fixturekit.DemoStart), intake.BuildingState, "fixture sealed")
	fixturekit.Must(t, err)
	sealFixtureStream(t, repo, session)
	askDriftFixture(t, repo, session)
	before := captureSessionSnapshot(t, repo, session)
	clock := &fixturekit.DemoClock{At: fixturekit.DemoStart}
	store, _, err := runtime.Open(runtime.Inputs{Config: cfg, Workstreams: map[config.ProjectID][]config.WorkstreamID{fixturekit.Project: {fixturekit.Stream, session}}})
	fixturekit.Must(t, err)
	defer store.Close()
	s := &Service{cfg: cfg, projects: []*activeProject{runtimeFor(repo)}, options: Options{Options: service.Options{Reconciliation: reconcile.Options{Now: clock.Now}}}, store: store}
	f := &foreman{masons: &masons{s: s, cfg: cfg, repository: repo}}
	var requested []config.WorkstreamID
	drivenPass(t, s, repo, func() error {
		var err error
		requested, err = f.RequestDrifts(ctx, func(ws config.WorkstreamID) (string, error) { return "owner requested", nil })
		return err
	})
	found := false
	for _, ws := range requested {
		if ws == fixturekit.Stream {
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

func askDriftFixture(t *testing.T, repo *trace.Repository, ws config.WorkstreamID) {
	t.Helper()
	id := fmt.Sprintf("%s-1", lifecycle.DriftRequestSubject)
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: fixturekit.Project, Workstream: ws, At: fixturekit.DemoStart, Actor: intake.OwnerActor, Cause: lifecycle.DriftRequestSubject}
	_, err := repo.Transact(context.Background(), trace.Transaction{Transition: trace.Transition{Header: h, Subject: lifecycle.DriftRequestSubject, To: "requested-1", Reason: "fixture"}})
	fixturekit.Must(t, err)
}

func TestFinalReviewPassSkipsASessionAlongsideAFullyMergedWorkstream(t *testing.T) {
	t.Parallel()
	f, stream, repository, a := newFinalFixture(t, "assemble")
	mergeDirectly(t, f, repository, stream, "resume", map[string]string{"resume.go": "package demo\n"})
	mergeDirectly(t, f, repository, stream, "dedupe", map[string]string{"dedupe.go": "package demo\n"})
	session := createTestSession(t, repository)
	before := captureSessionSnapshot(t, repository, session)
	drivenPass(t, f.s, repository, func() error { return a.Pass(context.Background()) })
	feature, err := repository.Workflow(stream, trace.FeatureSubject)
	fixturekit.Must(t, err)
	if feature.Value != intake.AssembledState {
		t.Fatalf("the fully-merged workstream stayed %q, want %q", feature.Value, intake.AssembledState)
	}
	assertSessionUnchanged(t, repository, session, before)
}

func approvedDeliveryFixture(t *testing.T) (*shedFixture, config.WorkstreamID, *trace.Repository, api.DeliveryApproval) {
	t.Helper()
	f, stream, repository, _ := deliveryFixture(t)
	presented, apiErr := f.s.deliveryPresentation(context.Background(), string(stream))
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	approval, apiErr := f.s.approveDelivery(context.Background(), string(stream), api.DeliveryDecision{Review: presented.Report.Review, ReviewRevision: presented.ReviewRevision, Commit: presented.Report.Commit, DraftHash: presented.DraftHash})
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	return f, stream, repository, approval
}

func approveSessionDelivery(t *testing.T, f *shedFixture, repository *trace.Repository, session config.WorkstreamID, approval api.DeliveryApproval) {
	t.Helper()
	ctx := context.Background()

	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "assembled", Revision: 1, Project: repository.Project(), Workstream: session, At: fixturekit.DemoStart, Actor: service.TestSessionActor, Cause: "fixture"}
	_, err := repository.SetFeatureState(ctx, h, intake.AssembledState, "fixture assembled")
	fixturekit.Must(t, err)
	report := api.FinalReport{Review: approval.Review, Outcome: delivery.FinalReviewed, Branch: intake.FeatureBranch(session), Commit: approval.Commit, Seal: approval.Seal, SpecHash: approval.SpecHash, Spec: approval.Spec, Plan: approval.Plan, Charter: approval.Charter, Summary: "session delivery"}
	content, err := json.Marshal(report)
	fixturekit.Must(t, err)
	fixturekit.Must(t, repository.RecordDocuments(ctx, []trace.Document{{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: delivery.FinalReportDocument, Revision: 1, Project: repository.Project(), Workstream: session, At: fixturekit.DemoStart, Actor: service.TestSessionActor, Cause: "final-review"}, Path: delivery.FinalReportPath, Content: string(content)}}))
	data, err := json.Marshal(approval)
	fixturekit.Must(t, err)
	fixturekit.Must(t, repository.RecordDocuments(ctx, []trace.Document{{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: delivery.DeliveryDocument, Revision: 1, Project: repository.Project(), Workstream: session, At: fixturekit.DemoStart, Actor: service.TestSessionActor, Cause: "delivery-approval"}, Path: delivery.DeliveryPath, Content: string(data)}}))
}

func TestPublisherPassSkipsASessionAlongsideAnApprovedWorkstream(t *testing.T) {
	t.Parallel()
	f, stream, repository, decision := approvedDeliveryFixture(t)
	session := createTestSession(t, repository)
	approveSessionDelivery(t, f, repository, session, decision)
	before := captureSessionSnapshot(t, repository, session)
	p := &publisher{s: f.s, repository: repository}
	drivenPass(t, f.s, repository, func() error { return p.Pass(context.Background()) })
	ops, err := repository.Operations(stream)
	fixturekit.Must(t, err)
	found := false
	for _, o := range ops {
		if o.Operation.Action == delivery.PublishAction || o.Operation.Action == publishUpstreamAction {
			found = true
		}
	}
	if !found {
		t.Fatal("the approved workstream's publish was not requested")
	}
	assertSessionUnchanged(t, repository, session, before)
}

func TestEnsureChiefsOfStaffSkipsASessionAlongsideAnOrdinaryWorkstream(t *testing.T) {
	t.Parallel()
	_, repo := newExclusionFixture(t, "")
	ctx := context.Background()
	fixturekit.Must(t, repo.CreateWorkstream(ctx, fixturekit.Stream, fixturekit.DemoStart, service.TestSessionActor))
	session := createTestSession(t, repo)
	before := captureSessionSnapshot(t, repo, session)
	fixturekit.Must(t, service.TestEnsureChiefsOfStaff(ctx, repo, fixturekit.DemoStart.Add(time.Hour)))
	if _, err := repo.ChiefOfStaffThread(fixturekit.Stream); err != nil {
		t.Fatalf("the ordinary workstream's chief-of-staff thread: %v", err)
	}
	if _, err := repo.ChiefOfStaffThread(session); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the session got a chief-of-staff thread: %v", err)
	}
	assertSessionUnchanged(t, repo, session, before)
}
