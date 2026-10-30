package executionjourney

import (
	context "context"
	bundle "github.com/kpenfound/osmia/internal/bundle"
	config "github.com/kpenfound/osmia/internal/config"
	coreadapter "github.com/kpenfound/osmia/internal/coreadapter"
	isolation "github.com/kpenfound/osmia/internal/isolation"
	questions "github.com/kpenfound/osmia/internal/questions"
	scheduler "github.com/kpenfound/osmia/internal/scheduler"
	fixturekit "github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	trace "github.com/kpenfound/osmia/internal/trace"
	atomic "sync/atomic"
	testing "testing"
	time "time"
)

func attemptOperation(t *testing.T, s *Service, repository *trace.Repository, stream config.WorkstreamID, op coreadapter.Operation, r coreadapter.Reconciler) (coreadapter.OperationResult, error) {
	t.Helper()
	return intakefixture.AttemptOperation(t, s.now(), foremanActor, repository, stream, op, r)
}

var callTool = intakefixture.CallTool

type changeRaceGate = intakefixture.ChangeRaceGate

var chiefEvent = intakefixture.ChiefEvent

var demoGit = fixturekit.DemoGit

var demoStart = fixturekit.DemoStart

const demoTimeout = intakefixture.DemoTimeout

func lifecycleAwait(t *testing.T, f *productionDesignFixture, phase string, ready func() bool) {
	t.Helper()
	f.sharedDesign().LifecycleAwait(t, phase, ready)
}

var must = intakefixture.Must

func newConfiguredProductionDesignFixture(t *testing.T, configuration productionDesignConfiguration) *productionDesignFixture {
	t.Helper()
	return intakefixture.NewProductionDesign(t, func(clock *fixedClock) intakefixture.ProductionDesignStart[*productionDesignFixture] {
		opts, clone, engine, sessions, _ := newArchitectOptions(t)
		intakefixture.ConfigureProductionDesign(t, opts.Config.Root)
		opts.Reconciliation.Now = clock.Now
		opts = Enforce(opts, Enforcement{Engine: engine, Hosts: opts.Architect.Hosts})
		opts.Librarian = nil
		if configuration.options != nil {
			configuration.options(&opts)
		}
		af := &architectFixture{opts: opts, clone: clone, engine: engine, sessions: sessions}
		f := &productionDesignFixture{crossRepoRatifyFixture: &crossRepoRatifyFixture{architectFixture: af}, clock: clock, dispatch: &productionChiefDispatch{}, configuration: configuration}
		return intakefixture.ProductionDesignStart[*productionDesignFixture]{
			Fixture: f, Scenario: f.sharedDesign(), Threads: opts.Threads, Hooks: &f.hook,
			Unwrap: func(turns coreadapter.Turns) *isolation.Turns {
				return turns.(*questions.Turns).Turns.(*verdictTurns).Turns.(*reportingTurns).Turns.(*retriedTurns).Turns.(*isolation.Turns)
			},
			Bind: func(threads func(*trace.Repository, *config.Config) (coreadapter.Reconciler, error), boundary func(context.Context, string) error) {
				af.opts.Threads, af.opts.boundary = threads, boundary
			},
			Start: f.start, Stop: f.stop, Running: func() bool { return f.s != nil },
		}
	}, configuration.handIn, configuration.checkpoint)
}

var noticeOf = intakefixture.NoticeOf

type productionDesignConfiguration struct {
	options    func(*Options)
	wait       func(*testing.T, *productionDesignFixture, string, func() bool)
	checkpoint func(context.Context, string) error
	handIn     bool
}

var providerOf = intakefixture.ProviderOf

func raiseFixtureContest(t *testing.T, r *trace.Repository, stream config.WorkstreamID, unit string) {
	t.Helper()
	intakefixture.RaiseFixtureContest(t, r, stream, unit, func() (trace.Header, bool, error) {
		c, found, err := unitContest(r, stream, unit)
		return c.Header, found, err
	}, chiefActor, escalateContest, chiefContestPath)
}

var recordSpec = intakefixture.RecordSpec

func seedBuild(t *testing.T, f *shedFixture, key, graph, backend, base string) (config.WorkstreamID, *trace.Repository) {
	t.Helper()
	return intakefixture.SeedBuild(t, f.stop, f.s.current, func(t *testing.T, r *trace.Repository, k, g, b, base string) config.WorkstreamID {
		return recordBuild(t, f, r, k, g, b, base)
	}, key, graph, backend, base)
}

func seedChecking(t *testing.T, f *shedFixture, repository *trace.Repository, stream config.WorkstreamID, unit string, outcome string) {
	t.Helper()
	intakefixture.SeedChecking(t, f.unitSeedDriver(repository), repository, stream, unit, outcome)
}

func seedReview(t *testing.T, f *shedFixture, repository *trace.Repository, stream config.WorkstreamID, unit string, outcome string) {
	t.Helper()
	intakefixture.SeedReview(t, repository, stream, unit, outcome, UnitReviewing, func(t *testing.T, r *trace.Repository, w config.WorkstreamID, u, o string) {
		seedChecking(t, f, r, w, u, o)
	}, func(t *testing.T, r *trace.Repository, w config.WorkstreamID) { runChecks(t, f.s, r, w) })
}

var streamDocuments = intakefixture.StreamDocuments

var transitionByID = intakefixture.TransitionByID

const validPlan = intakefixture.ValidPlan

const validSpec = intakefixture.ValidSpec

var workstreamDocument = fixturekit.WorkstreamDocument

type crossRepoRatifyFixture struct {
	*architectFixture
	project2         config.ProjectID
	stream1, stream2 config.WorkstreamID
}

type fixedClock = fixturekit.FixedClock

type productionChiefDispatch = intakefixture.ProductionChiefDispatch

type productionDesignFixture struct {
	*crossRepoRatifyFixture
	origin        config.WorkstreamID
	before        trace.Thread
	handed        trace.Document
	clock         *fixedClock
	hook          atomic.Pointer[designInterruption]
	repos         []productionDesignRepo
	owner         ConversationEntry
	ownerAddress  config.WorkstreamID
	dispatch      *productionChiefDispatch
	configuration productionDesignConfiguration
}

func recordBuild(t *testing.T, f *shedFixture, repository *trace.Repository, key, graph, backend, base string) config.WorkstreamID {
	t.Helper()
	w, err := InspectForTest(f.s).PrepareBuild(context.Background(), repository, backend, base)
	must(t, err)
	return fixturekit.RecordBuild(t, repository, fixturekit.BuildInput{Stream: w.Stream, At: w.At, Key: key, Graph: graph, Spec: validSpec, Branch: w.Branch, Workspace: w.Directory, Base: base, BaseBranch: w.BaseBranch})
}

func (f *shedFixture) seedBuilding(t *testing.T, key, graph string) config.WorkstreamID {
	t.Helper()
	return intakefixture.SeedBuilding(t, f.clone, key, graph, func() (string, error) {
		b, err := f.s.newWorkspaces(context.Background(), f.s.about(f.repository()))
		return b.Backend, err
	}, func(k, g, b, base string) config.WorkstreamID {
		return recordBuild(t, f, f.repository(), k, g, b, base)
	}, f.s.refreshRuntime)
}

func (f *shedFixture) unitSeedDriver(repository *trace.Repository) intakefixture.UnitSeedDriver[building] {
	m := newMasonController(f.s, repository)
	return intakefixture.UnitSeedDriver[building]{Read: m.read, Start: m.start, Finish: m.finish,
		Open: newUnitWorkspaces(f.s.current(), repository).open,
		Complete: func(t *testing.T, r *trace.Repository, w config.WorkstreamID, u, o string) trace.QueuedTurn {
			return completeMasonTurn(t, f, r, w, u, o)
		},
	}
}

type demoClock = fixturekit.DemoClock

type demoEngine = fixturekit.DemoEngine

type demoSessions = fixturekit.DemoSessions

type designInterruption = intakefixture.DesignInterruption

type fakeTurn = intakefixture.FakeTurn

type productionDesignRepo = intakefixture.ProductionDesignRepo

func (f *productionDesignFixture) awaitPacket(t *testing.T) PacketResponse {
	t.Helper()
	return f.sharedDesign().AwaitPacket(t)
}

func (f *productionDesignFixture) startOwner(t *testing.T) {
	t.Helper()
	f.sharedDesign().StartOwner(t)
}

func (f *productionDesignFixture) sharedDesign() *intakefixture.ProductionDesign {
	shared := &intakefixture.ProductionDesign{
		Changes: func(t *testing.T) (func(), func()) { return serviceChanges(t, f.s) },
		Scheduled: func() (*config.Config, []intakefixture.ScheduledProject) {
			cfg, active := f.s.runtimes()
			return cfg, intakefixture.ScheduledProjects(active, func(p *activeProject) intakefixture.ScheduledProject {
				interval := f.s.about(p.repository).Project.PullRequestPollInterval()
				return intakefixture.ScheduledProject{ID: p.id, Repository: p.repository, Interval: interval, NextPoll: f.s.polls.nextDue(p.id, interval, f.clock.Now())}
			})
		},
		LastEdgePoll: func(p config.ProjectID, w config.WorkstreamID) (time.Time, bool) {
			attempt, ok := f.s.polls.last(p, w, edgePoll)
			return attempt.at, ok
		},
		EdgeObservationAt: func(r *trace.Repository, w config.WorkstreamID) (int, time.Time, error) {
			_, rev, at, err := edgeObservationAt(r, w)
			return rev, at, err
		},

		Project: &f.project, Project2: &f.project2, Trace: &f.trace,
		Origin: &f.origin, Stream1: &f.stream1, Stream2: &f.stream2,
		Before: &f.before, Handed: &f.handed, Owner: &f.owner, OwnerAddress: &f.ownerAddress, Repos: &f.repos, ContextMode: func(p config.ProjectID) bundle.Mode { return f.s.Context().Mode(p) },
		Clone: f.clone, Engine: f.engine, Clock: f.clock, Dispatch: f.dispatch,
		Client:     func() intakefixture.ProductionDesignClient { return f.c },
		Config:     func() *config.Config { return f.s.current() },
		Repository: func(p config.ProjectID) (*trace.Repository, error) { return f.s.repository(p) },
		ChiefRepository: func(ctx context.Context, r *trace.Repository, w config.WorkstreamID) (*trace.Repository, config.WorkstreamID, config.WorkstreamID, func(), error) {
			return f.s.chiefRepository(ctx, r, w)
		},
		Feed:  func(w string) (FeedResponse, *APIError) { return f.s.feedList(w) },
		Ready: func() bool { return f.s.ready.Load() }, Done: func() <-chan struct{} { return f.s.done },
		Lifetime: func() context.Context { return f.s.lifetime }, Error: func() error { return f.s.testError() }, Runs: f.runs,
		LogHook: func(t *testing.T) {
			if hook := f.hook.Load(); hook != nil {
				t.Logf("injection step=%s hits=%d target=%d", hook.Step, hook.Count.Load(), hook.Nth)
			}
		},
		WriteCharter: func(t *testing.T, p config.ProjectID, text string) { writeCharter(t, f.c, p, text) },
		Upstream:     f.upstream, Script: f.script, ScriptDrafts: f.scriptDrafts, Workspaces: workspaces,
		ReadBuilding: func(r *trace.Repository, w config.WorkstreamID) (bool, error) {
			_, ok, err := (&masons{s: f.s, repository: r}).read(w)
			return ok, err
		},
		Holds: func(p config.ProjectID, w config.WorkstreamID, r *trace.Repository, c scheduler.Candidate) (bool, error) {
			return f.s.holds(p, w, r, c)
		},
		EdgeWaiting: edgeWaiting,
		Builder: func(r *trace.Repository) coreadapter.Reconciler {
			return &builder{S: f.s.intake().BuilderDependencies(), Repository: r}
		},
		Land: func(ctx context.Context, r *trace.Repository, m config.WorkstreamID) (coreadapter.OperationResult, error) {
			_, event := landIDs("accept", 1)
			return intakefixture.TryLanding(ctx, r, m, event, LandAction, landInput{Unit: "accept", Review: 1, Candidate: "candidate", Base: "base"}, (&foreman{masons: &masons{s: f.s, repository: r}}).Apply)
		},
		LeastWorkstream: leastWorkstream, NextReadyTurn: nextReadyTurn, LibrarianWorkstream: librarianWorkstream,
	}
	if f.configuration.wait != nil {
		shared.WaitMode = func(t *testing.T, phase string, done func() bool) { f.configuration.wait(t, f, phase, done) }
	}
	return shared
}

func (f *architectFixture) sharedTurns() *intakefixture.TurnScenario {
	return &intakefixture.TurnScenario{
		Root: func() config.Root { return f.s.current().Root }, Project: f.project,
		Now: f.clock.Now, Repository: f.repository,
		Changes: func(t *testing.T) (func(), func()) { return serviceChanges(t, f.s) },
		Done:    func() <-chan struct{} { return f.s.done }, WaitError: f.s.Wait, ForemanActor: foremanActor,
	}
}
