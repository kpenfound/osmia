package intakejourney

import (
	context "context"
	json "encoding/json"
	fmt "fmt"

	"github.com/kpenfound/osmia/internal/service"

	bundle "github.com/kpenfound/osmia/internal/bundle"
	config "github.com/kpenfound/osmia/internal/config"
	coreadapter "github.com/kpenfound/osmia/internal/coreadapter"
	isolation "github.com/kpenfound/osmia/internal/isolation"
	plan "github.com/kpenfound/osmia/internal/plan"
	questions "github.com/kpenfound/osmia/internal/questions"
	scheduler "github.com/kpenfound/osmia/internal/scheduler"
	fixturekit "github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	trace "github.com/kpenfound/osmia/internal/trace"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"

	strings "strings"
	atomic "sync/atomic"
	testing "testing"
	time "time"
)

// abandonCall posts an abandon request to the service's handler.
func abandonCall(t *testing.T, s *Service, id config.WorkstreamID, body string) (int, string) {
	t.Helper()
	return intakefixture.AbandonCall(t, s.handle, id, body)
}

// approveDirectly records a satisfactory review of the unit's current
// candidate and applies it, as a reviewer turn and the reviewer controller do.
func approveDirectly(t *testing.T, s *Service, repository *trace.Repository, stream config.WorkstreamID, unit, criterion string) {
	t.Helper()
	r := &reviewers{masons: newMasonController(s, repository)}
	intakefixture.ApproveDirectly(t, repository, stream, unit, r.prepareUnitReview, func(identity UnitReviewIdentity, turn string) ([]byte, error) {
		return json.Marshal(UnitReviewResult{Identity: identity, Turn: turn, Verdict: UnitVerdict{Decision: "satisfactory", Summary: reviewSummary}})
	}, reviewTurnID, reviewDocument, r.one, UnitApproved)
}

// assembleBoth merges both units of validPlan, checking that the workstream
// stays building while one is unmerged, and asks for the first final review.
// It returns the feature branch tip and the review's operation.
func assembleBoth(t *testing.T, f *shedFixture, repository *trace.Repository, a *finalReviewer, stream config.WorkstreamID, resume, dedupe map[string]string) (string, coreadapter.Operation) {
	t.Helper()
	return intakefixture.AssembleBoth(t, repository, a, stream, func(unit string, files map[string]string) string {
		return mergeDirectly(t, f, repository, stream, unit, files)
	}, resume, dedupe, AssembledState, FinalReviewAction)
}

// attemptOperation reconciles one pending operation as the reconciliation
// controller does and returns what its effect returned: an effect that
// fails records a retry and no result, so the operation stays pending.
func attemptOperation(t *testing.T, s *Service, repository *trace.Repository, stream config.WorkstreamID, op coreadapter.Operation, r coreadapter.Reconciler) (coreadapter.OperationResult, error) {
	t.Helper()
	return intakefixture.AttemptOperation(t, s.now(), foremanActor, repository, stream, op, r)
}

var awaitAcknowledged = intakefixture.AwaitAcknowledged

// awaitExtraction polls status until the latest extraction is terminal.
func awaitExtraction(t *testing.T, c *Client) ExtractionState {
	t.Helper()
	return intakefixture.AwaitExtraction(t, c.Configuration)
}

// awaitMasonRan waits until the unit's first mason turn completed.
func (f *shedFixture) awaitMasonRan(t *testing.T, stream config.WorkstreamID, unit string) trace.Thread {
	t.Helper()
	return f.sharedTurns().AwaitMasonRan(t, stream, unit)
}

var awaitPasses = intakefixture.AwaitPasses

// awaitUnit waits until the unit of the workstream is in state.
func (f *shedFixture) awaitUnit(t *testing.T, stream config.WorkstreamID, unit, state string) {
	t.Helper()
	f.sharedTurns().AwaitUnit(t, stream, unit, state)
}

// backendOf returns the workspace backend the workstream's manifest records.
func backendOf(t *testing.T, f *shedFixture, stream config.WorkstreamID) string {
	t.Helper()
	return intakefixture.BackendOf(t, f.opts.Config, f.project, stream)
}

type changeRaceGate = intakefixture.ChangeRaceGate

// chiefAnswers has the real chief-of-staff answer tool record a ruling for
// question id, citing cite, through a queued and claimed chief-of-staff
// turn, and delivers it to the asker's thread through the service's own
// Deliverer: the production question path a running service's chief uses.
func chiefAnswers(t *testing.T, f *shedFixture, repository *trace.Repository, stream config.WorkstreamID, id, text, cite string) {
	t.Helper()
	chiefRecordsAnswer(t, f, repository, stream, id, text, cite)
	must(t, f.s.answers(f.s.current(), repository).Pass(context.Background()))
}

var chiefEvent = intakefixture.ChiefEvent

// chiefRecordsAnswer records the production answer without running delivery.
func chiefRecordsAnswer(t *testing.T, f *shedFixture, repository *trace.Repository, stream config.WorkstreamID, id, text, cite string) {
	t.Helper()
	f.sharedTurns().ChiefRecordsAnswer(t, repository, stream, id, text, cite)
}

// completeMasonTurn runs the next queued turn of the unit's mason as a
// finished turn that reported the unit done.
func completeMasonTurn(t *testing.T, f *shedFixture, repository *trace.Repository, stream config.WorkstreamID, unit string, outcome string) trace.QueuedTurn {
	t.Helper()
	return f.sharedTurns().CompleteMasonTurn(t, repository, stream, unit, outcome)
}

// countingSchedule returns a passed counter and the prepare func that makes
// opts.schedulePassed advance it once per completed reconciliation pass.
func countingSchedule() (*atomic.Int64, func(*Options)) {
	passed, scheduled := intakefixture.ScheduleCounter()
	return passed, func(opts *Options) { opts.schedulePassed = scheduled }
}

// crossRepoRatifyFixture is two active projects in one service, each with
// its own clone and charter, ready for the architect to draft a cross-repo
// workgroup's project spec and for one owner ratify to seal it.
type crossRepoRatifyFixture struct {
	*architectFixture
	project2         config.ProjectID
	stream1, stream2 config.WorkstreamID
}

// deliveryFixtureWith is deliveryFixture with extra configuration appended.
func deliveryFixtureWith(t *testing.T, extra string) (*shedFixture, config.WorkstreamID, *trace.Repository, FinalReport) {
	t.Helper()
	f, stream, repository, a := newFinalFixtureWith(t, "delivery", extra)
	report := intakefixture.SeedDelivery(t, repository, stream, func(unit string, files map[string]string) string {
		return mergeDirectly(t, f, repository, stream, unit, files)
	}, a.Pass, func(ctx context.Context) (FinalReport, error) {
		in, sealed, err := a.governing(ctx, stream)
		return FinalReport{Commit: in.Commit, Seal: in.Seal, SpecHash: sealed.SpecHash, Spec: in.Spec, Plan: in.Plan, Charter: in.Charter, Upstream: &sealed.Base}, err
	}, finalReviewActor, finalReportDocument, finalReportPath, finalReviewed)
	f.s.setSole(&activeProject{repository: repository})
	return f, stream, repository, report
}

var documentCommit = intakefixture.DocumentCommit

var done = intakefixture.Done

// drivePass idles the service's reconciliation controller, joining the
// operations it runs beside its schedule hooks, so driving pass by hand
// afterward has nothing left to race for the repository's one operation
// lock and commits in a single attempt.
func drivePass(t *testing.T, s *Service, repo *trace.Repository, pass func() error) {
	t.Helper()
	intakefixture.DrivePass(t, repo, func() error { return s.halt(s.sole()) }, pass)
}

var eventually = intakefixture.Eventually

var exchange = intakefixture.Exchange

type fakeMasons = intakefixture.FakeMasons

// finalOperations returns the workstream's final review operations.
func finalOperations(t *testing.T, repository *trace.Repository, stream config.WorkstreamID) []trace.OperationRecord {
	t.Helper()
	return intakefixture.FinalOperations(t, repository, stream, FinalReviewAction)
}

// finalTurn installs the fake committee member's turn of final review k.
func (f *shedFixture) finalTurn(k, attempt int, run func(context.Context, *mcp.ClientSession) error) string {
	return intakefixture.FinalTurn(f.engine, finalTurnID(k, committeeAgent(1), attempt), run)
}

// landOperations returns the workstream's landing operations.
func landOperations(t *testing.T, repository *trace.Repository, stream config.WorkstreamID) []trace.OperationRecord {
	t.Helper()
	return intakefixture.LandOperations(t, repository, stream, LandAction)
}

// lifecycleAwait observes the live controller's durable writes. It never runs a
// second controller or a reconciliation pass from the test.
func lifecycleAwait(t *testing.T, f *productionDesignFixture, phase string, ready func() bool) {
	t.Helper()
	f.sharedDesign().LifecycleAwait(t, phase, ready)
}

// listenWeb is the configuration that opens the web listener on a free
// loopback port.
const listenWeb = "[listen]\nweb = \"127.0.0.1:0\"\n"

// masonRoles runs the chief of staff, mason and reviewer in containers, the
// sandbox the fake engine's boundary check accepts.
const masonRoles = chiefRole + "[roles.mason]\nsandbox = \"container\"\nimage = \"fixture-image\"\n[roles.reviewer]\nsandbox = \"container\"\nimage = \"fixture-image\"\n"

// mergeDirectly commits files onto the feature branch as the unit's landing
// and records the unit merged, as a landing does, and returns the new tip.
func mergeDirectly(t *testing.T, f *shedFixture, repository *trace.Repository, stream config.WorkstreamID, unit string, files map[string]string) string {
	t.Helper()
	return intakefixture.MergeDirectly(t, repository, stream, unit, moveFeature(t, f, stream, files), foremanActor, UnitMerged)
}

// moveFeature commits files onto the workstream's feature branch, as a
// landing does, removing those whose content is empty, and returns the new
// tip.
func moveFeature(t *testing.T, f *shedFixture, stream config.WorkstreamID, files map[string]string) string {
	t.Helper()
	return intakefixture.MoveFeature(t, workspaces(f.s.current(), branchesDirectory, backendOf(t, f, stream)), stream, files)
}

// newApprovedFixture creates two units with completed mason turns, candidate
// snapshots and approved reviews. It returns the stopped service and open trace.
func newApprovedFixture(t *testing.T, key string) (*shedFixture, config.WorkstreamID, *trace.Repository) {
	t.Helper()
	return newApprovedFixtureOn(t, key, config.WorkspacesGit)
}

// newApprovedFixtureOn creates approved units on the given workspace backend.
func newApprovedFixtureOn(t *testing.T, key, backend string) (*shedFixture, config.WorkstreamID, *trace.Repository) {
	t.Helper()
	f, _ := newMasonFixtureOn(t, backend, "masons = 1\n", independentPlan, "")
	base := strings.TrimSpace(demoGit(t, f.clone, "-C", f.clone, "rev-parse", "HEAD"))
	stream, repository := seedBuild(t, f, key, independentPlan, backend, base)
	intakefixture.SeedApprovedUnits(t, func(u, o string) { seedReview(t, f, repository, stream, u, o) }, func(u, c string) { approveDirectly(t, f.s, repository, stream, u, c) })
	return f, stream, repository
}

// newFinalFixture seeds a sealed validPlan with a committee runner and no
// masons. It returns the stopped service, workstream, open trace and assembly
// controller. Intake, debate and ratification are covered by lifecycle tests.
func newFinalFixture(t *testing.T, key string) (*shedFixture, config.WorkstreamID, *trace.Repository, *finalReviewer) {
	t.Helper()
	return newFinalFixtureWith(t, key, "")
}

// newFinalFixtureWith is newFinalFixture with extra configuration appended.
func newFinalFixtureWith(t *testing.T, key, extra string) (*shedFixture, config.WorkstreamID, *trace.Repository, *finalReviewer) {
	t.Helper()
	f := newDebateFixtureWith(t, 1, 1, extra, nil)
	stream, repository := f.buildFixture().SeedGit(t, key)
	return f, stream, repository, &finalReviewer{s: f.s, repository: repository}
}

// newMasonController returns the mason controller of the service's
// configuration over repository.
func newMasonController(s *Service, repository *trace.Repository) *masons {
	return &masons{s: s, cfg: s.current(), repository: repository}
}

// newMasonFixtureOn is newConfiguredMasonFixture whose workstreams are
// handed in on the workspace backend given.
func newMasonFixtureOn(t *testing.T, backend, capacity, drafted, classifier string) (*shedFixture, *fakeMasons) {
	t.Helper()
	return newMasonFixtureWith(t, backend, capacity, drafted, classifier, nil)
}

// newMasonFixtureWith is newMasonFixtureOn with prepare, when set, changing
// the options and the configuration file before the service starts.
func newMasonFixtureWith(t *testing.T, backend, capacity, drafted, classifier string, prepare func(*Options)) (*shedFixture, *fakeMasons) {
	t.Helper()
	f := newDebateFixtureWith(t, 1, 1, masonRoles, func(opts *Options) {
		opts.WriteTimeout = time.Minute
		enforced := Enforce(*opts, Enforcement{Engine: opts.Committee.Engine, Hosts: opts.Committee.Hosts})
		opts.Threads, opts.controls = enforced.Threads, enforced.controls
		opts.Threads = intakefixture.ConfigureMasonThreads(t, opts.Config.Root, backend, capacity, classifier, opts.Threads)
		if prepare != nil {
			prepare(opts)
		}
	})
	f.script("draft-1-1", map[string]string{plan.SpecPath: validSpec, plan.PlanPath: drafted}, nil)
	f.upstream(t)
	return f, intakefixture.ScriptMasons(f.engine, masonTurnID)
}

// newParallelMasonFixture is a mason fixture with capacity.masons set to
// masons and capacity.per_workstream to perWorkstream, whose entity map adds
// internal.upload and internal.audit to the seeded one before any plan is
// drafted. Every unit's first mason turn is played by the returned fake
// masons, and its follow-up turns fail as newMasonFixtureOn's do, so a unit
// whose first turn calls no outcome tool stays implementing.
func newParallelMasonFixture(t *testing.T, masons, perWorkstream int, drafted string) (*shedFixture, *fakeMasons) {
	t.Helper()
	return newParallelMasonFixtureOn(t, config.WorkspacesGit, masons, perWorkstream, drafted)
}

// newParallelMasonFixtureOn is newParallelMasonFixture whose workstreams are
// handed in on the workspace backend given.
func newParallelMasonFixtureOn(t *testing.T, backend string, masons, perWorkstream int, drafted string) (*shedFixture, *fakeMasons) {
	t.Helper()
	f, fake := newMasonFixtureOn(t, backend, fmt.Sprintf("masons = %d\nper_workstream = %d\n", masons, perWorkstream), drafted, "")
	intakefixture.ExtendParallelMasons(t, f.repository(), f.clock.Now(), f.engine, fake, masonTurnID, librarianActor)
	return f, fake
}

// newProductionDesignFixture prepares repos and injected engines only. The
// owner endpoint and production architect tools create all feature members,
// handed inputs and draft documents after the real design exchange.
func newProductionDesignFixture(t *testing.T) *productionDesignFixture {
	t.Helper()
	return newConfiguredProductionDesignFixture(t, productionDesignConfiguration{})
}

var noticeOf = intakefixture.NoticeOf

// parallelPlan has resume and dedupe in internal.trace, and upload and audit
// in internal.upload and internal.audit, disjoint from it and from each other.
// No unit depends on another.
const parallelPlan = `{"version": 1, "units": [
  {"id": "resume", "title": "Resume from the last chunk", "addresses": [{"criterion": "spec#1", "proof": {"kind": "new-test", "name": "TestResume"}}], "depends_on": [], "footprint": ["internal.trace"]},
  {"id": "upload", "title": "Send chunks", "addresses": [{"criterion": "spec#1", "proof": {"kind": "new-test", "name": "TestUpload"}}], "depends_on": [], "footprint": ["internal.upload"]},
  {"id": "dedupe", "title": "Skip acknowledged chunks", "addresses": [{"criterion": "spec#2", "proof": {"kind": "reviewer-judgement", "name": "no chunk is sent twice"}}], "depends_on": [], "footprint": ["internal.trace"]},
  {"id": "audit", "title": "Record acknowledged chunks", "addresses": [{"criterion": "spec#2", "proof": {"kind": "reviewer-judgement", "name": "acknowledgements are recorded"}}], "depends_on": [], "footprint": ["internal.audit"]}
]}
`

var providerOf = intakefixture.ProviderOf

// raiseFixtureContest records explicit escalation for owner-inbox fixtures.
func raiseFixtureContest(t *testing.T, r *trace.Repository, stream config.WorkstreamID, unit string) {
	t.Helper()
	intakefixture.RaiseFixtureContest(t, r, stream, unit, func() (trace.Header, bool, error) {
		c, found, err := unitContest(r, stream, unit)
		return c.Header, found, err
	}, chiefActor, escalateContest, chiefContestPath)
}

// recordBuild publishes the sealed documents and unit states atomically so
// running controllers only see a complete build fixture.
func recordBuild(t *testing.T, f *shedFixture, repository *trace.Repository, key, graph, backend, base string) config.WorkstreamID {
	t.Helper()
	w, err := InspectForTest(f.s).PrepareBuild(context.Background(), repository, backend, base)
	must(t, err)
	return fixturekit.RecordBuild(t, repository, fixturekit.BuildInput{Stream: w.Stream, At: w.At, Key: key, Graph: graph, Spec: validSpec, Branch: w.Branch, Workspace: w.Directory, Base: base, BaseBranch: w.BaseBranch})
}

var recordSpec = intakefixture.RecordSpec

// reviewSummary is how a fixture reviewer says it verified the acceptance.
const reviewSummary = "The candidate does the task and its acceptance holds"

// runChecks runs the checks controller's passes and reconciles each check
// run they request as the reconciliation controller does, one at a time as
// in a workstream, until none is pending, so each checking unit moves on its
// run's result.
func runChecks(t *testing.T, s *Service, repository *trace.Repository, stream config.WorkstreamID) {
	t.Helper()
	intakefixture.RunChecks(t, repository, stream, &checkers{masons: newMasonController(s, repository)}, CheckAction, func(op coreadapter.Operation, r coreadapter.Reconciler) (coreadapter.OperationResult, error) {
		return attemptOperation(t, s, repository, stream, op, r)
	})
}

// runtimeFor is the runtime of an open trace, as the service keeps it.
func runtimeFor(repository *trace.Repository) *activeProject {
	return &activeProject{id: repository.Project(), repository: repository}
}

// scriptDrafts installs the fake architect's behaviour for both members'
// own draft turns, routed by the workspace path's workstream, and for the
// workgroup's project-spec draft turn.
func (f *crossRepoRatifyFixture) scriptDrafts(t *testing.T) {
	t.Helper()
	intakefixture.ScriptCrossRepoDrafts(t, f.engine, func() (config.WorkstreamID, config.WorkstreamID) { return f.stream1, f.stream2 })
}

// seedBuild records a sealed plan and builds its unit graph. It returns a
// stopped service's open repository for controller tests.
func seedBuild(t *testing.T, f *shedFixture, key, graph, backend, base string) (config.WorkstreamID, *trace.Repository) {
	t.Helper()
	return intakefixture.SeedBuild(t, f.stop, f.s.current, func(t *testing.T, r *trace.Repository, k, g, b, base string) config.WorkstreamID {
		return recordBuild(t, f, r, k, g, b, base)
	}, key, graph, backend, base)
}

// seedBuilding gives a running service a sealed build ready for unit dispatch.
func (f *shedFixture) seedBuilding(t *testing.T, key, graph string) config.WorkstreamID {
	t.Helper()
	return intakefixture.SeedBuilding(t, f.clone, key, graph, func() (string, error) {
		b, err := f.s.newWorkspaces(context.Background(), f.s.about(f.repository()))
		return b.Backend, err
	}, func(k, g, b, base string) config.WorkstreamID {
		return recordBuild(t, f, f.repository(), k, g, b, base)
	}, f.s.refreshRuntime)
}

// seedChecking records a completed mason report and snapshots its
// candidate, so the unit is checking.
func seedChecking(t *testing.T, f *shedFixture, repository *trace.Repository, stream config.WorkstreamID, unit string, outcome string) {
	t.Helper()
	intakefixture.SeedChecking(t, f.unitSeedDriver(repository), repository, stream, unit, outcome)
}

// seedReview records a completed mason report, snapshots its candidate and
// passes its checks, so the unit is reviewing.
func seedReview(t *testing.T, f *shedFixture, repository *trace.Repository, stream config.WorkstreamID, unit string, outcome string) {
	t.Helper()
	intakefixture.SeedReview(t, repository, stream, unit, outcome, UnitReviewing, func(t *testing.T, r *trace.Repository, w config.WorkstreamID, u, o string) {
		seedChecking(t, f, r, w, u, o)
	}, func(t *testing.T, r *trace.Repository, w config.WorkstreamID) { runChecks(t, f.s, r, w) })
}

var settle = intakefixture.Settle

var snapshot = intakefixture.Snapshot

var soon = intakefixture.Soon

var streamDocuments = intakefixture.StreamDocuments

var transitionByID = intakefixture.TransitionByID

var turn = intakefixture.Turn

// twoProjectFixture is a root with two active projects that share one mason
// slot, each with its own trace, workstream and mason thread.
type twoProjectFixture struct {
	opts     Options
	cfg      *config.Config
	clock    *demoClock
	turns    *blockingTurns
	streams  map[config.ProjectID]config.WorkstreamID
	scenario *fixturekit.TwoProjectScenario
}

// unitState reads the unit's state from the running service's trace.
func (f *shedFixture) unitState(stream config.WorkstreamID, unit string) (string, error) {
	states, err := f.repository().WorkflowStates(stream)
	return states[trace.UnitSubject(unit)].Value, err
}

// withWeb appends a listen.web setting to the fixture's top-level file.
func withWeb(t *testing.T, opts Options, addr string) {
	t.Helper()
	intakefixture.WithWeb(t, opts.Config.Root, addr)
}

const quiet = intakefixture.Quiet

const (
	otherProject config.ProjectID    = "p_fedcba9876543210fedcba9876543210"
	otherStream  config.WorkstreamID = "w_fedcba9876543210fedcba9876543210"
)

const cyclicPlan = fixturekit.CyclicPlan

const demoAgent = intakefixture.DemoAgent

const demoRole = fixturekit.DemoRole

const demoSecret = intakefixture.DemoSecret

const demoThread = intakefixture.DemoThread

const demoTimeout = intakefixture.DemoTimeout

const handedDesign = intakefixture.HandedDesign

const productionBrief = intakefixture.ProductionBrief

const project = intakefixture.Project

const stream = intakefixture.Stream

const validPlan = intakefixture.ValidPlan

const validSpec = intakefixture.ValidSpec

func (f *productionDesignFixture) assertBarrier(t *testing.T) {
	t.Helper()
	f.sharedDesign().AssertBarrier(t)
}

func (f *productionDesignFixture) awaitApproved(t *testing.T) {
	t.Helper()
	f.sharedDesign().AwaitApproved(t)
}

func (f *productionDesignFixture) awaitPacket(t *testing.T) PacketResponse {
	t.Helper()
	return f.sharedDesign().AwaitPacket(t)
}

func (f *productionDesignFixture) checkContinuation(t *testing.T) {
	t.Helper()
	f.sharedDesign().CheckContinuation(t)
}

func (f *productionDesignFixture) startOwner(t *testing.T) {
	t.Helper()
	f.sharedDesign().StartOwner(t)
}

func (f *productionDesignFixture) timeout(t *testing.T, phase string) {
	t.Helper()
	f.sharedDesign().Timeout(t, phase)
}

func (f *productionDesignFixture) wait(t *testing.T, phase string, done func() bool) {
	t.Helper()
	f.sharedDesign().Wait(t, phase, done)
}

func (f *twoProjectFixture) repository(id config.ProjectID) *trace.Repository {
	return f.scenario.Repository(id)
}

var acceptancePlan = intakefixture.AcceptancePlan

var assertCode = intakefixture.AssertCode

var await = intakefixture.Await

func deliveryFixture(t *testing.T) (*shedFixture, config.WorkstreamID, *trace.Repository, FinalReport) {
	t.Helper()
	return deliveryFixtureWith(t, "")
}

var entitiesWith = intakefixture.EntitiesWith

var equalJSONTest = intakefixture.EqualJSONTest

var errorCode = intakefixture.ErrorCode

var hasDiagnostic = intakefixture.HasDiagnostic

func lifecycleWorker(t *testing.T, f *productionDesignFixture, project config.ProjectID, stream config.WorkstreamID, worker, turn, prompt string) {
	t.Helper()
	f.sharedDesign().LifecycleWorker(t, project, stream, worker, turn, prompt)
}

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
	}, configuration.handIn, configuration.wait != nil, configuration.checkpoint)
}

func newTwoProjectFixture(t *testing.T) *twoProjectFixture {
	t.Helper()
	data := fixturekit.NewTwoProjectScenario(t)
	opts := testOptions(data.Config)
	opts.Workstreams, opts.Threads, opts.Reconciliation.Now = nil, data.Threads, data.Clock.Now
	return &twoProjectFixture{opts: opts, cfg: data.Loaded, clock: data.Clock, streams: data.Streams, turns: data.Turns, scenario: data}
}

var ptr = intakefixture.Ptr

var webHTTP = intakefixture.WebHTTP

type blockingTurns = fixturekit.BlockingTurns

type completedRunner = fixturekit.CompletedRunner

type demoClock = fixturekit.DemoClock

type demoEngine = fixturekit.DemoEngine

type demoSessions = fixturekit.DemoSessions

type demoTransport = fixturekit.DemoTransport

type demoTurn = intakefixture.DemoTurn

type designInterruption = intakefixture.DesignInterruption

type fakeIssues = fixturekit.FakeIssues

type fakeTurn = intakefixture.FakeTurn

type fixedClock = fixturekit.FixedClock

type productionChiefDispatch = intakefixture.ProductionChiefDispatch

type productionDesignConfiguration struct {
	options    func(*Options)
	wait       func(*testing.T, *productionDesignFixture, string, func() bool)
	checkpoint func(context.Context, string) error
	handIn     bool
}

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

type productionDesignRepo = intakefixture.ProductionDesignRepo

var callTool = intakefixture.CallTool

var claimTurn = fixturekit.ClaimTurn

var demoGit = fixturekit.DemoGit

var demoStart = fixturekit.DemoStart

var must = intakefixture.Must

var projectDocument = fixturekit.ProjectDocument

var request = fixturekit.Request

var traceCommits = fixturekit.TraceCommits

var traceExec = fixturekit.TraceExec

var traceFile = fixturekit.TraceFile

var workstreamDocument = fixturekit.WorkstreamDocument

var workstreamTraceDirectory = intakefixture.WorkstreamTraceDirectory

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

func (f *shedFixture) unitSeedDriver(repository *trace.Repository) intakefixture.UnitSeedDriver[service.TestBuilding] {
	m := newMasonController(f.s, repository)
	return intakefixture.UnitSeedDriver[service.TestBuilding]{Read: m.read, Start: m.start, Finish: m.finish,
		Open: newUnitWorkspaces(f.s.current(), repository).open,
		Complete: func(t *testing.T, r *trace.Repository, w config.WorkstreamID, u, o string) trace.QueuedTurn {
			return completeMasonTurn(t, f, r, w, u, o)
		},
	}
}

func (f *shedFixture) buildFixture() intakefixture.BuildFixture {
	return intakefixture.BuildFixture{
		Upstream: f.upstream,
		Seed: func(t *testing.T, key, graph, backend, base string) (config.WorkstreamID, *trace.Repository) {
			return seedBuild(t, f, key, graph, backend, base)
		},
		Start: f.start,
	}
}
