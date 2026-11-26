package journey

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/bundle"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/isolation"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/scheduler"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ApproveDirectly records a satisfactory review of the unit's current
// candidate and applies it, as a reviewer turn and the reviewer controller do.
func ApproveDirectly(t *testing.T, s *Service, repository *trace.Repository, stream config.WorkstreamID, unit, criterion string) {
	t.Helper()
	r := &Reviewers{Masons: NewMasonController(s, repository)}
	intakefixture.ApproveDirectly(t, repository, stream, unit, r.PrepareUnitReview, func(identity service.UnitReviewIdentity, turn string) ([]byte, error) {
		return json.Marshal(service.UnitReviewResult{Identity: identity, Turn: turn, Verdict: service.UnitVerdict{Decision: "satisfactory", Summary: fixturekit.ReviewSummary}})
	}, execution.ReviewTurnID, execution.ReviewDocument, r.One, intake.UnitApproved)
}

// AssembleBoth merges both units of intakefixture.ValidPlan, checking that the workstream
// stays building while one is unmerged, and asks for the first final review.
// It returns the feature branch tip and the review's operation.
func AssembleBoth(t *testing.T, f *ShedFixture, repository *trace.Repository, a *FinalReviewer, stream config.WorkstreamID, resume, dedupe map[string]string) (string, coreadapter.Operation) {
	t.Helper()
	return intakefixture.AssembleBoth(t, repository, a, stream, func(unit string, files map[string]string) string {
		return MergeDirectly(t, f, repository, stream, unit, files)
	}, resume, dedupe, intake.AssembledState, service.FinalReviewAction)
}

// AttemptOperation reconciles one pending operation as the reconciliation
// controller does and returns what its effect returned: an effect that
// fails records a retry and no result, so the operation stays pending.
func AttemptOperation(t *testing.T, s *Service, repository *trace.Repository, stream config.WorkstreamID, op coreadapter.Operation, r coreadapter.Reconciler) (coreadapter.OperationResult, error) {
	t.Helper()
	return intakefixture.AttemptOperation(t, s.Now(), execution.ForemanActor, repository, stream, op, r)
}

// AwaitMasonRan waits until the unit's first mason turn completed.
func (f *ShedFixture) AwaitMasonRan(t *testing.T, stream config.WorkstreamID, unit string) trace.Thread {
	t.Helper()
	return f.SharedTurns().AwaitMasonRan(t, stream, unit)
}

// AwaitUnit waits until the unit of the workstream is in state.
func (f *ShedFixture) AwaitUnit(t *testing.T, stream config.WorkstreamID, unit, state string) {
	t.Helper()
	f.SharedTurns().AwaitUnit(t, stream, unit, state)
}

// BackendOf returns the workspace backend the workstream's manifest records.
func BackendOf(t *testing.T, f *ShedFixture, stream config.WorkstreamID) string {
	t.Helper()
	return intakefixture.BackendOf(t, f.Opts.Config, f.Project, stream)
}

// ChiefAnswers has the real chief-of-staff answer tool record a ruling for
// question id, citing cite, through a queued and claimed chief-of-staff
// turn, and delivers it to the asker's thread through the service's own
// Deliverer: the production question path a running service's chief uses.
func ChiefAnswers(t *testing.T, f *ShedFixture, repository *trace.Repository, stream config.WorkstreamID, id, text, cite string) {
	t.Helper()
	ChiefRecordsAnswer(t, f, repository, stream, id, text, cite)
	fixturekit.Must(t, f.S.Answers(f.S.Current(), repository).Pass(context.Background()))
}

// ChiefRecordsAnswer records the production answer without running delivery.
func ChiefRecordsAnswer(t *testing.T, f *ShedFixture, repository *trace.Repository, stream config.WorkstreamID, id, text, cite string) {
	t.Helper()
	f.SharedTurns().ChiefRecordsAnswer(t, repository, stream, id, text, cite)
}

// CompleteMasonTurn runs the next queued turn of the unit's mason as a
// finished turn that reported the unit done.
func CompleteMasonTurn(t *testing.T, f *ShedFixture, repository *trace.Repository, stream config.WorkstreamID, unit string, outcome string) trace.QueuedTurn {
	t.Helper()
	return f.SharedTurns().CompleteMasonTurn(t, repository, stream, unit, outcome)
}

// CountingSchedule returns a passed counter and the prepare func that makes
// opts.SchedulePassed advance it once per completed reconciliation pass.
func CountingSchedule() (*atomic.Int64, func(*Options)) {
	passed, scheduled := intakefixture.ScheduleCounter()
	return passed, func(opts *Options) { opts.SchedulePassed = scheduled }
}

// CrossRepoRatifyFixture is two active projects in one service, each with
// its own clone and charter, ready for the architect to draft a cross-repo
// workgroup's project spec and for one owner ratify to seal it.
type CrossRepoRatifyFixture struct {
	*ArchitectFixture
	Project2         config.ProjectID
	Stream1, Stream2 config.WorkstreamID
}

// DeliveryFixtureWith is DeliveryFixture with extra configuration appended.
func DeliveryFixtureWith(t *testing.T, extra string) (*ShedFixture, config.WorkstreamID, *trace.Repository, api.FinalReport) {
	t.Helper()
	f, stream, repository, a := NewFinalFixtureWith(t, "delivery", extra)
	report := intakefixture.SeedDelivery(t, repository, stream, func(unit string, files map[string]string) string {
		return MergeDirectly(t, f, repository, stream, unit, files)
	}, a.Pass, func(ctx context.Context) (api.FinalReport, error) {
		in, sealed, err := a.Governing(ctx, stream)
		return api.FinalReport{Commit: in.Commit, Seal: in.Seal, SpecHash: sealed.SpecHash, Spec: in.Spec, Plan: in.Plan, Charter: in.Charter, Upstream: &sealed.Base}, err
	}, delivery.FinalReviewActor, delivery.FinalReportDocument, delivery.FinalReportPath, delivery.FinalReviewed)
	f.S.SetSole(&ActiveProject{Repository: repository})
	return f, stream, repository, report
}

// FinalOperations returns the workstream's final review operations.
func FinalOperations(t *testing.T, repository *trace.Repository, stream config.WorkstreamID) []trace.OperationRecord {
	t.Helper()
	return intakefixture.OperationsOf(t, repository, stream, service.FinalReviewAction)
}

// FinalTurn installs the fake committee member's turn of final review k.
func (f *ShedFixture) FinalTurn(k, attempt int, run func(context.Context, *mcp.ClientSession) error) string {
	return intakefixture.FinalTurn(f.Engine, delivery.FinalTurnID(k, intake.CommitteeAgent(1), attempt), run)
}

// LandOperations returns the workstream's landing operations.
func LandOperations(t *testing.T, repository *trace.Repository, stream config.WorkstreamID) []trace.OperationRecord {
	t.Helper()
	return intakefixture.OperationsOf(t, repository, stream, service.LandAction)
}

// LifecycleAwait observes the live controller's durable writes. It never runs a
// second controller or a reconciliation pass from the test.
func LifecycleAwait(t *testing.T, f *ProductionDesignFixture, phase string, ready func() bool) {
	t.Helper()
	f.SharedDesign().LifecycleAwait(t, phase, ready)
}

// MergeDirectly commits files onto the feature branch as the unit's landing
// and records the unit merged, as a landing does, and returns the new tip.
func MergeDirectly(t *testing.T, f *ShedFixture, repository *trace.Repository, stream config.WorkstreamID, unit string, files map[string]string) string {
	t.Helper()
	return intakefixture.MergeDirectly(t, repository, stream, unit, MoveFeature(t, f, stream, files), execution.ForemanActor, intake.UnitMerged)
}

// MoveFeature commits files onto the workstream's feature branch, as a
// landing does, removing those whose content is empty, and returns the new
// tip.
func MoveFeature(t *testing.T, f *ShedFixture, stream config.WorkstreamID, files map[string]string) string {
	t.Helper()
	return intakefixture.MoveFeature(t, intake.Workspaces(f.S.Current(), intake.BranchesDirectory, BackendOf(t, f, stream)), stream, files)
}

// NewApprovedFixture creates two units with completed mason turns, candidate
// snapshots and approved reviews. It returns the stopped service and open trace.
func NewApprovedFixture(t *testing.T, key string) (*ShedFixture, config.WorkstreamID, *trace.Repository) {
	t.Helper()
	return NewApprovedFixtureOn(t, key, config.WorkspacesGit)
}

// NewApprovedFixtureOn creates approved units on the given workspace backend.
func NewApprovedFixtureOn(t *testing.T, key, backend string) (*ShedFixture, config.WorkstreamID, *trace.Repository) {
	t.Helper()
	f, _ := NewMasonFixtureOn(t, backend, "masons = 1\n", intakefixture.IndependentPlan)
	base := strings.TrimSpace(fixturekit.DemoGit(t, f.Clone, "-C", f.Clone, "rev-parse", "HEAD"))
	stream, repository := SeedBuild(t, f, key, intakefixture.IndependentPlan, backend, base)
	intakefixture.SeedApprovedUnits(t, func(u, o string) { SeedReview(t, f, repository, stream, u, o) }, func(u, c string) { ApproveDirectly(t, f.S, repository, stream, u, c) })
	return f, stream, repository
}

// NewFinalFixture seeds a sealed intakefixture.ValidPlan with a committee runner and no
// masons. It returns the stopped service, workstream, open trace and assembly
// controller. Intake, debate and ratification are covered by lifecycle tests.
func NewFinalFixture(t *testing.T, key string) (*ShedFixture, config.WorkstreamID, *trace.Repository, *FinalReviewer) {
	t.Helper()
	f, stream, repository, a := NewFinalFixtureWith(t, key, "")
	HostFinalReviews(t, f, stream)
	return f, stream, repository, a
}

// NewFinalFixtureWith is NewFinalFixture with extra configuration appended and
// no pull request host, so the service holds the fixture's default clients.
func NewFinalFixtureWith(t *testing.T, key, extra string) (*ShedFixture, config.WorkstreamID, *trace.Repository, *FinalReviewer) {
	t.Helper()
	f := NewDebateFixtureWith(t, 1, 1, extra, nil)
	stream, repository := f.BuildFixture().SeedGit(t, key)
	return f, stream, repository, &FinalReviewer{S: f.S, Repository: repository}
}

// HostFinalReviews gives the stopped service a credential for the pull request
// host: a local bare fork of owner/dagger as the clone's origin and a fake host
// that follows the fork's feature branch. The fixture's options already read a
// passing Cloud run for every pushed commit with no wait after a push, so a
// final review publishes its draft and reads that result at once. The
// restarted service holds the same host.
func HostFinalReviews(t *testing.T, f *ShedFixture, stream config.WorkstreamID) *fixturekit.FakePulls {
	t.Helper()
	_, pulls := HostService(t, f, func() config.WorkstreamID { return stream })
	service.InspectForTest(f.S.Core()).SetStoppedPullRequests(pulls)
	return pulls
}

// NewMasonController returns the mason controller of the service's
// configuration over repository.
func NewMasonController(s *Service, repository *trace.Repository) *Masons {
	return &Masons{S: s, Cfg: s.Cfg, Repository: repository}
}

// NewMasonFixtureOn is NewMasonFixtureWith whose workstreams are
// handed in on the workspace backend given.
func NewMasonFixtureOn(t *testing.T, backend, capacity, drafted string) (*ShedFixture, *intakefixture.FakeMasons) {
	t.Helper()
	return NewMasonFixtureWith(t, backend, capacity, drafted, nil)
}

// NewMasonFixtureWith is NewMasonFixtureOn with prepare, when set, changing
// the options and the configuration file before the service starts.
func NewMasonFixtureWith(t *testing.T, backend, capacity, drafted string, prepare func(*Options)) (*ShedFixture, *intakefixture.FakeMasons) {
	t.Helper()
	f := NewDebateFixtureWith(t, 1, 1, intakefixture.MasonRoles, func(opts *Options) {
		opts.WriteTimeout = time.Minute
		enforced := Enforce(*opts, service.Enforcement{Engine: opts.Committee.Engine, Hosts: opts.Committee.Hosts})
		opts.Threads, opts.Controls = enforced.Threads, enforced.Controls
		opts.Threads = intakefixture.ConfigureMasonThreads(t, opts.Config.Root, backend, capacity, opts.Threads)
		if prepare != nil {
			prepare(opts)
		}
	})
	f.Script("draft-1-1", map[string]string{plan.SpecPath: fixturekit.ValidSpec, plan.PlanPath: drafted}, nil)
	f.Upstream(t)
	return f, intakefixture.ScriptMasons(f.Engine, execution.MasonTurnID)
}

// NewParallelMasonFixture is a mason fixture with capacity.masons set to
// masons and capacity.per_workstream to perWorkstream, whose entity map adds
// internal.upload and internal.audit to the seeded one before any plan is
// drafted. The returned fake masons play every unit's first mason turn.
func NewParallelMasonFixture(t *testing.T, masons, perWorkstream int, drafted string) (*ShedFixture, *intakefixture.FakeMasons) {
	t.Helper()
	return NewParallelMasonFixtureOn(t, config.WorkspacesGit, masons, perWorkstream, drafted)
}

// NewParallelMasonFixtureOn is NewParallelMasonFixture whose workstreams are
// handed in on the workspace backend given.
func NewParallelMasonFixtureOn(t *testing.T, backend string, masons, perWorkstream int, drafted string) (*ShedFixture, *intakefixture.FakeMasons) {
	t.Helper()
	return NewParallelMasonFixturePrepared(t, backend, masons, perWorkstream, drafted, nil)
}

// NewParallelMasonFixturePrepared is NewParallelMasonFixtureOn with prepare,
// when set, changing the options before the service starts.
func NewParallelMasonFixturePrepared(t *testing.T, backend string, masons, perWorkstream int, drafted string, prepare func(*Options)) (*ShedFixture, *intakefixture.FakeMasons) {
	t.Helper()
	f, fake := NewMasonFixtureWith(t, backend, intakefixture.ParallelCapacity(masons, perWorkstream), drafted, prepare)
	intakefixture.ExtendParallelMasons(t, f.Clone, f.Repository(), f.Clock.Now(), f.Engine, fake, execution.MasonTurnID, intake.OwnerActor)
	return f, fake
}

// ParallelPlan has resume and dedupe in internal.trace, and upload and audit
// in internal.upload and internal.audit, disjoint from it and from each other.
// No unit depends on another.
const ParallelPlan = `{"version": 2, "units": [
  {"id": "resume", "title": "Resume from the last chunk", "task": "Resume from the last chunk", "acceptance": ["spec#1 holds, shown by TestResume (new-test)"], "criteria": ["spec#1"], "depends_on": [], "footprint": ["internal.trace"]},
  {"id": "upload", "title": "Send chunks", "task": "Send chunks", "acceptance": ["spec#1 holds, shown by TestUpload (new-test)"], "criteria": ["spec#1"], "depends_on": [], "footprint": ["internal.upload"]},
  {"id": "dedupe", "title": "Skip acknowledged chunks", "task": "Skip acknowledged chunks", "acceptance": ["spec#2 holds, shown by no chunk is sent twice (reviewer-judgement)"], "criteria": ["spec#2"], "depends_on": [], "footprint": ["internal.trace"]},
  {"id": "audit", "title": "Record acknowledged chunks", "task": "Record acknowledged chunks", "acceptance": ["spec#2 holds, shown by acknowledgements are recorded (reviewer-judgement)"], "criteria": ["spec#2"], "depends_on": [], "footprint": ["internal.audit"]}
]}
`

// RaiseFixtureContest records explicit escalation for owner-inbox fixtures.
func RaiseFixtureContest(t *testing.T, r *trace.Repository, stream config.WorkstreamID, unit string) {
	t.Helper()
	intakefixture.RaiseFixtureContest(t, r, stream, unit, func() (trace.Header, bool, error) {
		c, found, err := execution.UnitContest(r, stream, unit)
		return c.Header, found, err
	}, intake.ChiefActor, execution.EscalateContest, execution.ChiefContestPath)
}

// RecordBuild publishes the sealed documents and unit states atomically so
// running controllers only see a complete build fixture.
func RecordBuild(t *testing.T, f *ShedFixture, repository *trace.Repository, key, graph, backend, base string) config.WorkstreamID {
	t.Helper()
	w, err := InspectForTest(f.S).PrepareBuild(context.Background(), repository, backend, base)
	fixturekit.Must(t, err)
	return fixturekit.RecordBuild(t, repository, fixturekit.BuildInput{Stream: w.Stream, At: w.At, Key: key, Graph: graph, Spec: fixturekit.ValidSpec, Branch: w.Branch, Workspace: w.Directory, Base: base, BaseBranch: w.BaseBranch})
}

// RunChecks runs the checks controller's passes and reconciles each check
// run they request as the reconciliation controller does, one at a time as
// in a workstream, until none is pending, so each checking unit moves on its
// run's result.
func RunChecks(t *testing.T, s *Service, repository *trace.Repository, stream config.WorkstreamID) {
	t.Helper()
	intakefixture.RunChecks(t, repository, stream, &Checkers{Masons: NewMasonController(s, repository)}, service.CheckAction, func(op coreadapter.Operation, r coreadapter.Reconciler) (coreadapter.OperationResult, error) {
		return AttemptOperation(t, s, repository, stream, op, r)
	})
}

// RuntimeFor is the runtime of an open trace, as the service keeps it.
func RuntimeFor(repository *trace.Repository) *ActiveProject {
	return &ActiveProject{Id: repository.Project(), Repository: repository}
}

// ScriptDrafts installs the fake architect's behaviour for both members'
// own draft turns, routed by the workspace path's workstream, and for the
// workgroup's project-spec draft turn.
func (f *CrossRepoRatifyFixture) ScriptDrafts(t *testing.T) {
	t.Helper()
	intakefixture.ScriptCrossRepoDrafts(t, f.Engine, func() (config.WorkstreamID, config.WorkstreamID) { return f.Stream1, f.Stream2 })
}

// SeedBuild records a sealed plan and builds its unit graph. It returns a
// stopped service's open repository for controller tests.
func SeedBuild(t *testing.T, f *ShedFixture, key, graph, backend, base string) (config.WorkstreamID, *trace.Repository) {
	t.Helper()
	return intakefixture.SeedBuild(t, f.Stop, f.S.Current, func(t *testing.T, r *trace.Repository, k, g, b, base string) config.WorkstreamID {
		return RecordBuild(t, f, r, k, g, b, base)
	}, key, graph, backend, base)
}

// SeedChecking records a completed mason report and snapshots its
// candidate, so the unit is checking.
func SeedChecking(t *testing.T, f *ShedFixture, repository *trace.Repository, stream config.WorkstreamID, unit string, outcome string) {
	t.Helper()
	intakefixture.SeedChecking(t, f.UnitSeedDriver(repository), repository, stream, unit, outcome)
}

// SeedReview records a completed mason report, snapshots its candidate and
// passes its checks, so the unit is reviewing.
func SeedReview(t *testing.T, f *ShedFixture, repository *trace.Repository, stream config.WorkstreamID, unit string, outcome string) {
	t.Helper()
	intakefixture.SeedReview(t, repository, stream, unit, outcome, intake.UnitReviewing, func(t *testing.T, r *trace.Repository, w config.WorkstreamID, u, o string) {
		SeedChecking(t, f, r, w, u, o)
	}, func(t *testing.T, r *trace.Repository, w config.WorkstreamID) { RunChecks(t, f.S, r, w) })
}

// UnitState reads the unit's state from the running service's trace.
func (f *ShedFixture) UnitState(stream config.WorkstreamID, unit string) (string, error) {
	states, err := f.Repository().WorkflowStates(stream)
	return states[trace.UnitSubject(unit)].Value, err
}

func (f *ProductionDesignFixture) AssertBarrier(t *testing.T) {
	t.Helper()
	f.SharedDesign().AssertBarrier(t)
}

func (f *ProductionDesignFixture) AwaitApproved(t *testing.T) {
	t.Helper()
	f.SharedDesign().AwaitApproved(t)
}

func (f *ProductionDesignFixture) AwaitPacket(t *testing.T) api.PacketResponse {
	t.Helper()
	return f.SharedDesign().AwaitPacket(t)
}

func (f *ProductionDesignFixture) CheckContinuation(t *testing.T) {
	t.Helper()
	f.SharedDesign().CheckContinuation(t)
}

func (f *ProductionDesignFixture) StartOwner(t *testing.T) {
	t.Helper()
	f.SharedDesign().StartOwner(t)
}

func (f *ProductionDesignFixture) Timeout(t *testing.T, phase string) {
	t.Helper()
	f.SharedDesign().Timeout(t, phase)
}

func (f *ProductionDesignFixture) Wait(t *testing.T, phase string, done func() bool) {
	t.Helper()
	f.SharedDesign().Wait(t, phase, done)
}

func DeliveryFixture(t *testing.T) (*ShedFixture, config.WorkstreamID, *trace.Repository, api.FinalReport) {
	t.Helper()
	return DeliveryFixtureWith(t, "")
}

func NewConfiguredProductionDesignFixture(t *testing.T, configuration ProductionDesignConfiguration) *ProductionDesignFixture {
	t.Helper()
	return intakefixture.NewProductionDesign(t, func(clock *fixturekit.FixedClock) intakefixture.ProductionDesignStart[*ProductionDesignFixture] {
		opts, clone, engine, sessions, _ := NewArchitectOptions(t)
		intakefixture.ConfigureProductionDesign(t, opts.Config.Root)
		opts.Reconciliation.Now = clock.Now
		opts = Enforce(opts, service.Enforcement{Engine: engine, Hosts: opts.Architect.Hosts})
		if configuration.Options != nil {
			configuration.Options(&opts)
		}
		af := &ArchitectFixture{Opts: opts, Clone: clone, Engine: engine, Sessions: sessions}
		f := &ProductionDesignFixture{CrossRepoRatifyFixture: &CrossRepoRatifyFixture{ArchitectFixture: af}, Clock: clock, Dispatch: &intakefixture.ProductionChiefDispatch{}, Configuration: configuration}
		return intakefixture.ProductionDesignStart[*ProductionDesignFixture]{
			Fixture: f, Scenario: f.SharedDesign(), Threads: opts.Threads, Hooks: &f.Hook,
			Unwrap: func(turns coreadapter.Turns) *isolation.Turns {
				return turns.(*questions.Turns).Turns.(*execution.VerdictTurns).Turns.(*execution.ReportingTurns).Turns.(*execution.RetriedTurns).Turns.(*isolation.Turns)
			},
			Bind: func(threads func(*trace.Repository, *config.Config) (coreadapter.Reconciler, error), boundary func(context.Context, string) error) {
				af.Opts.Threads, af.Opts.Boundary = threads, boundary
			},
			Start: f.Start, Stop: f.Stop, Running: func() bool { return f.S != nil },
		}
	}, configuration.HandIn, configuration.Checkpoint)
}

type ProductionDesignConfiguration struct {
	Options    func(*Options)
	Wait       func(*testing.T, *ProductionDesignFixture, string, func() bool)
	Checkpoint func(context.Context, string) error
	HandIn     bool

	OpenPullRequestsStay bool
}

type ProductionDesignFixture struct {
	*CrossRepoRatifyFixture
	Origin        config.WorkstreamID
	Before        trace.Thread
	Design        trace.Thread
	Handed        trace.Document
	Clock         *fixturekit.FixedClock
	Hook          atomic.Pointer[intakefixture.DesignInterruption]
	Repos         []intakefixture.ProductionDesignRepo
	Owner         api.ConversationEntry
	OwnerAddress  config.WorkstreamID
	Dispatch      *intakefixture.ProductionChiefDispatch
	Configuration ProductionDesignConfiguration
}

func (f *ProductionDesignFixture) SharedDesign() *intakefixture.ProductionDesign {
	shared := &intakefixture.ProductionDesign{
		Changes: func(t *testing.T) (func(), func()) { return ServiceChanges(t, f.S) },
		Scheduled: func() (*config.Config, []intakefixture.ScheduledProject) {
			cfg, active := f.S.Runtimes()
			return cfg, intakefixture.ScheduledProjects(active, func(p *ActiveProject) intakefixture.ScheduledProject {
				interval := f.S.About(p.Repository).Project.PullRequestPollInterval()
				return intakefixture.ScheduledProject{ID: p.Id, Repository: p.Repository, Interval: interval, NextPoll: f.S.Polls.NextDue(p.Id, interval, f.Clock.Now())}
			})
		},
		LastEdgePoll: func(p config.ProjectID, w config.WorkstreamID) (time.Time, bool) {
			attempt, ok := f.S.Polls.Last(p, w, lifecycle.EdgePoll)
			return attempt.At, ok
		},
		EdgeObservationAt: func(r *trace.Repository, w config.WorkstreamID) (int, time.Time, error) {
			_, rev, at, err := lifecycle.EdgeObservationAt(r, w)
			return rev, at, err
		},

		Project: &f.Project, Project2: &f.Project2, Trace: &f.Trace,
		Origin: &f.Origin, Stream1: &f.Stream1, Stream2: &f.Stream2,
		Before: &f.Before, Design: &f.Design, Handed: &f.Handed, Owner: &f.Owner, OwnerAddress: &f.OwnerAddress, Repos: &f.Repos, ContextMode: func(p config.ProjectID) bundle.Mode { return f.S.Context().Mode(p) },
		Clone: f.Clone, Engine: f.Engine, Clock: f.Clock, Dispatch: f.Dispatch,
		Client:     func() intakefixture.ProductionDesignClient { return f.C },
		Config:     func() *config.Config { return f.S.Current() },
		Repository: func(p config.ProjectID) (*trace.Repository, error) { return f.S.Repository(p) },
		ChiefRepository: func(ctx context.Context, r *trace.Repository, w config.WorkstreamID) (*trace.Repository, config.WorkstreamID, config.WorkstreamID, func(), error) {
			return f.S.ChiefRepository(ctx, r, w)
		},
		Feed:  func(w string) (api.FeedResponse, *api.APIError) { return f.S.FeedList(w) },
		Ready: func() bool { return f.S.Ready.Load() }, Done: func() <-chan struct{} { return f.S.Done },
		Lifetime: func() context.Context { return f.S.Lifetime }, Error: func() error { return f.S.TestError() }, Runs: f.Runs,
		LogHook: func(t *testing.T) {
			if hook := f.Hook.Load(); hook != nil {
				t.Logf("injection step=%s hits=%d target=%d", hook.Step, hook.Count.Load(), hook.Nth)
			}
		},
		WriteCharter: func(t *testing.T, p config.ProjectID, text string) { intakefixture.WriteCharter(t, f.C, p, text) },
		Upstream:     f.Upstream, Script: f.Script, ScriptDrafts: f.ScriptDrafts, Workspaces: intake.Workspaces,
		ReadBuilding: func(r *trace.Repository, w config.WorkstreamID) (bool, error) {
			_, ok, err := (&Masons{S: f.S, Repository: r}).Read(w)
			return ok, err
		},
		Holds: func(p config.ProjectID, r *trace.Repository, c scheduler.Candidate) (bool, error) {
			return f.S.Holds(p, r, c)
		},
		EdgeWaiting: lifecycle.EdgeWaiting,
		Builder: func(r *trace.Repository) coreadapter.Reconciler {
			return &intake.Builder{S: f.S.Intake().BuilderDependencies(), Repository: r}
		},
		Land: func(ctx context.Context, r *trace.Repository, m config.WorkstreamID) (coreadapter.OperationResult, error) {
			_, event := execution.LandIDs("accept", 1)
			return intakefixture.TryLanding(ctx, r, m, event, service.LandAction, execution.LandInput{Unit: "accept", Review: 1, Candidate: "candidate", Base: "base"}, (&Foreman{Masons: &Masons{S: f.S, Repository: r}}).Apply)
		},
		LeastWorkstream: lifecycle.LeastWorkstream, NextReadyTurn: lifecycle.NextReadyTurn,
	}
	if f.Configuration.OpenPullRequestsStay {
		shared.IdleRefresh = func(t trace.Transition) bool { return t.Reason == lifecycle.OwnPullRequestOpenReason }
	}
	if f.Configuration.Wait != nil {
		shared.WaitMode = func(t *testing.T, phase string, done func() bool) { f.Configuration.Wait(t, f, phase, done) }
	}
	return shared
}

func (f *ArchitectFixture) SharedTurns() *intakefixture.TurnScenario {
	return &intakefixture.TurnScenario{
		Root: func() config.Root { return f.S.Current().Root }, Project: f.Project,
		Now: f.Clock.Now, Repository: f.Repository,
		Changes: func(t *testing.T) (func(), func()) { return ServiceChanges(t, f.S) },
		Done:    func() <-chan struct{} { return f.S.Done }, WaitError: f.S.Wait, ForemanActor: execution.ForemanActor,
	}
}

func (f *ShedFixture) UnitSeedDriver(repository *trace.Repository) intakefixture.UnitSeedDriver[execution.Building] {
	m := NewMasonController(f.S, repository)
	return intakefixture.UnitSeedDriver[execution.Building]{Read: m.Read, Start: m.Start, Finish: m.Finish,
		Open: NewUnitWorkspaces(f.S.Current(), repository).Open,
		Complete: func(t *testing.T, r *trace.Repository, w config.WorkstreamID, u, o string) trace.QueuedTurn {
			return CompleteMasonTurn(t, f, r, w, u, o)
		},
	}
}

func (f *ShedFixture) BuildFixture() intakefixture.BuildFixture {
	return intakefixture.BuildFixture{
		Upstream: f.Upstream,
		Seed: func(t *testing.T, key, graph, backend, base string) (config.WorkstreamID, *trace.Repository) {
			return SeedBuild(t, f, key, graph, backend, base)
		},
		Start: f.Start,
	}
}
