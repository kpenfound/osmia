package executionjourney

import (
	"context"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/checks"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/jev"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/router"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/scheduler"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"slices"
	"sync/atomic"
	"testing"
	"time"
)

type Options struct {
	conversationTicks <-chan time.Time
	service.Options
	boundary       func(context.Context, string) error
	schedulePassed func()
	notifyInbox    func(context.Context) (api.InboxResponse, *api.APIError)
	reviewChecks   service.ReviewChecks
	controls       *runtimeControls

	telemetryExporter sdktrace.SpanExporter
}

func (o Options) hooks() service.TestHooks {
	h := service.TestHooks{ConversationTicks: o.conversationTicks, Boundary: o.boundary, SchedulePassed: o.schedulePassed, NotifyInbox: o.notifyInbox, ReviewChecks: o.reviewChecks, TelemetryExporter: o.telemetryExporter}
	if o.controls != nil {
		h.Enforcement = o.controls.binding
	}
	return h
}
func Enforce(o Options, e service.Enforcement) Options {
	o.Options = service.Enforce(o.Options, e)
	o.controls = &runtimeControls{binding: service.EnforcementBindingForTest(o.Options)}
	return o
}

func Start(ctx context.Context, o Options) (*Service, error) {
	s := &Service{options: o}
	s.polls = pollsAccess{s: s}
	h := o.hooks()
	boundary := h.Boundary
	h.Boundary = func(ctx context.Context, name string) error {
		if s.boundary != nil {
			return s.boundary(name)
		}
		if boundary != nil {
			return boundary(ctx, name)
		}
		return nil
	}
	core, err := servicetest.StartContext(ctx, o.Options, h)
	if err != nil {
		return nil, err
	}
	s.Service = core
	s.cfg = service.InspectForTest(core).Config()
	s.options.reviewChecks = service.InspectForTest(core).Execution().Checks
	s.store = service.InspectForTest(core).Intake().Store()
	s.options.Issues = service.InspectForTest(core).Intake().Options.Issues
	s.done = core.DoneForTest()
	s.lifetime = core.LifetimeForTest()
	s.ready = readyAccess{s: core}
	if o.controls != nil {
		o.controls.service.Store(s)
	}
	return s, nil
}

func projectFixture(t *testing.T) (Options, string) {
	o, c := servicetest.ProjectFixture(t)
	return Options{Options: o}, c
}

func serviceChanges(t *testing.T, s *Service) (func(), func()) {
	return servicetest.ServiceChanges(t, s.core())
}
func writeCharter(t *testing.T, c *Client, p config.ProjectID, content string) trace.Document {
	return servicetest.WriteCharter(t, c.Client, p, content)
}

func mutation(t *testing.T, c *Client, method, kind string, input any) {
	servicetest.Mutation(t, c.Client, method, kind, input)
}

type Service struct {
	*service.Service
	cfg      *config.Config
	options  Options
	jev      *jev.Judge
	store    *runtime.Store
	boundary func(string) error
	done     <-chan struct{}
	lifetime context.Context
	ready    readyAccess
	projects []*activeProject
	polls    pollsAccess
}
type readyAccess struct{ s *service.Service }

func (a readyAccess) Load() bool { return a.s.ReadyForTest() }

type activeProject struct {
	id         config.ProjectID
	repository *trace.Repository
}

func (s *Service) core() *service.Service {
	if s.Service == nil {
		var repos []*trace.Repository
		for _, p := range s.projects {
			repos = append(repos, p.repository)
		}
		s.Service = service.NewFixtureForTest(s.cfg, s.options.Options, s.store, repos)
	}
	return s.Service
}
func (s *Service) intake() *intake.Controller {
	c := service.InspectForTest(s.core()).Intake()
	c.Options.Architect = s.options.Architect
	c.Options.Committee = s.options.Committee
	if s.boundary != nil {
		c.Step = s.boundary
	}
	return c
}
func (s *Service) current() *config.Config                  { return service.InspectForTest(s.core()).Config() }
func (s *Service) about(r *trace.Repository) *config.Config { return s.intake().About(r) }
func (s *Service) now() time.Time                           { return s.intake().Now() }
func (s *Service) repository(p config.ProjectID) (*trace.Repository, error) {
	return service.InspectForTest(s.core()).Repository(p)
}
func (s *Service) sole() *activeProject {
	r := service.InspectForTest(s.core()).SoleRepository()
	if r == nil {
		return nil
	}
	return &activeProject{id: r.Project(), repository: r}
}
func (s *Service) setSole(p *activeProject) {
	if p == nil {
		service.InspectForTest(s.core()).SetTrace(nil)
		return
	}
	service.InspectForTest(s.core()).SetTrace(p.repository)
}

func (s *Service) refreshRuntime() error { return service.InspectForTest(s.core()).RefreshRuntime() }

func (s *Service) testError() error { return s.core().ErrForTest() }

type runtimeControls struct {
	service atomic.Pointer[Service]
	binding *service.TestEnforcementBinding
}

type Client struct {
	*service.Client
	defaultTimeout time.Duration
}

func NewClient(socket string) *Client {
	c := service.NewClient(socket)
	return &Client{Client: c, defaultTimeout: c.TimeoutForTest()}
}

func (s *Service) chiefRepository(ctx context.Context, r *trace.Repository, w config.WorkstreamID) (*trace.Repository, config.WorkstreamID, config.WorkstreamID, func(), error) {
	return s.intake().ChiefRepository(ctx, r, w)
}
func (s *Service) newWorkspaces(ctx context.Context, c *config.Config) (api.WorkspacesStatus, error) {
	return s.intake().NewWorkspaces(ctx, c)
}
func (s *Service) runtimes() (*config.Config, []*activeProject) {
	cfg, ps := s.intake().Runtimes()
	var out []*activeProject
	for _, p := range ps {
		out = append(out, &activeProject{id: p.ID, repository: p.Repository})
	}
	return cfg, out
}
func (s *Service) feedList(raw string) (api.FeedResponse, *api.APIError) {
	return service.InspectForTest(s.core()).Feed(raw)
}

func (s *Service) ruleContested(ctx context.Context, w, u string, r api.ContestedRulingRequest) (api.ContestedRulingResponse, *api.APIError) {
	return service.InspectForTest(s.core()).RuleContested(ctx, w, u, r)
}

func (s *Service) holds(p config.ProjectID, w config.WorkstreamID, r *trace.Repository, c scheduler.Candidate) (bool, error) {
	return service.InspectForTest(s.core()).Holds(p, r, c)
}

func InspectForTest(s *Service) service.TestAccess { return service.InspectForTest(s.core()) }

type pollsAccess struct{ s *Service }

func (p pollsAccess) nextDue(project config.ProjectID, interval time.Duration, now time.Time) time.Time {
	var attempts []intakefixture.PollAttempt
	for _, a := range service.InspectForTest(p.s.core()).PollAttempts() {
		attempts = append(attempts, intakefixture.PollAttempt{Project: a.Project, At: a.At})
	}
	return intakefixture.NextPollDue(attempts, project, interval, now)
}

type pollAttempt struct{ at time.Time }

func (p pollsAccess) last(project config.ProjectID, w config.WorkstreamID, kind string) (pollAttempt, bool) {
	for _, a := range service.InspectForTest(p.s.core()).PollAttempts() {
		if a.Project == project && a.Workstream == w && a.Kind == kind {
			return pollAttempt{at: a.At}, true
		}
	}
	return pollAttempt{}, false
}

func (s *Service) execution() *execution.Controller {
	c := service.InspectForTest(s.core()).Execution()
	c.Checks = s.options.reviewChecks
	if s.jev != nil {
		c.Jev = s.jev
	}
	if s.boundary != nil {
		c.Step = s.boundary
	}
	return c
}

func (s *Service) moveUnit(ctx context.Context, r *trace.Repository, w config.WorkstreamID, u string, req api.UnitMoveRequest, a trace.Actor, turn string) (api.UnitMoveResponse, *api.APIError) {
	return s.execution().MoveUnit(ctx, r, w, u, req, a, turn)
}
func (s *Service) ownerMove(ctx context.Context, w, u string, r api.UnitMoveRequest) (api.UnitMoveResponse, *api.APIError) {
	return s.execution().OwnerMove(ctx, w, u, r)
}
func (s *Service) recordContestedRuling(ctx context.Context, r *trace.Repository, w config.WorkstreamID, u string, req api.ContestedRulingRequest, a trace.Actor, ds ...trace.Document) (api.ContestedRulingResponse, *api.APIError) {
	return s.execution().RecordContestedRuling(ctx, r, w, u, req, a, ds...)
}
func (c *runtimeControls) execution() *execution.Controls {
	return &execution.Controls{Load: func() *execution.Controller {
		if c == nil || c.service.Load() == nil {
			return nil
		}
		return c.service.Load().execution()
	}}
}
func (c *runtimeControls) moveUnit(r *trace.Repository, s coreadapter.Scope) coreadapter.Tool {
	return c.execution().MoveUnit(r, s)
}
func (c *runtimeControls) resolveContested(r *trace.Repository, s coreadapter.Scope) coreadapter.Tool {
	return c.execution().ResolveContested(r, s)
}

func (s *Service) delivery() *delivery.Controller {
	c := service.InspectForTest(s.core()).DeliveryController()
	if s.jev != nil {
		c.Jev = s.jev
	}
	c.Committee = s.options.Committee
	c.PullRequests = s.options.PullRequests
	c.CloudStatuses, c.CloudRerun, c.CloudTrace, c.PostPushWait = s.options.CloudStatuses, s.options.CloudRerun, s.options.CloudTrace, s.options.FinalChecksWait
	if s.options.Reconciliation.Now != nil {
		c.Now = s.options.Reconciliation.Now
	}
	c.Execution = s.execution
	if s.boundary != nil {
		c.Step = s.boundary
	}
	return c
}

type drifter struct{ *foreman }

func (d drifter) driver() *service.TestDriftDriver {
	return service.DriftForTest(d.s.core(), d.repository)
}
func (d drifter) requestDrifts(ctx context.Context, due func(config.WorkstreamID) (string, error)) ([]config.WorkstreamID, error) {
	return d.driver().Request(ctx, due)
}
func (d drifter) Inspect(ctx context.Context, op coreadapter.Operation) (coreadapter.Observation, error) {
	return d.driver().Inspect(ctx, op)
}
func (d drifter) Apply(ctx context.Context, op coreadapter.Operation) (coreadapter.OperationResult, error) {
	return d.driver().Apply(ctx, op)
}

var factoryTarget = runtime.Target{Scope: "factory"}

func (s *Service) admit(cfg *config.Config, r *trace.Repository) func(context.Context, scheduler.Candidate) (bool, error) {
	return service.InspectForTest(s.core()).Admit(cfg, r)
}
func (s *Service) traceView(ctx context.Context, w, kind, sel string) (any, *api.APIError) {
	return service.InspectForTest(s.core()).TraceView(ctx, w, kind, sel)
}
func (s *Service) capacityStatus(list []api.WorkstreamStatus) *api.CapacityStatus {
	return service.InspectForTest(s.core()).Capacity(list)
}
func (s *Service) contestedEntry(r *trace.Repository, w config.WorkstreamID, u string) (api.InboxEntry, bool, error) {
	return service.InspectForTest(s.core()).ContestedEntry(r, w, u)
}

func (s *Service) decideAmendment(ctx context.Context, raw string, id string, req api.AmendmentDecisionRequest) (api.AmendmentResponse, *api.APIError) {
	return s.intake().DecideAmendment(ctx, raw, id, req)
}

type reloadFiles struct {
	fixturekit.ReloadFiles
	top, topText string
}

func configFiles(t *testing.T, opts Options) reloadFiles {
	f := fixturekit.ConfigFiles(t, opts.Config.Root)
	return reloadFiles{f, f.Top, f.TopText}
}
func (f reloadFiles) write(t *testing.T, top string) { f.ReloadFiles.Write(t, top) }

func workTransitions(t *testing.T, repo *trace.Repository, stream config.WorkstreamID) []trace.Transition {
	t.Helper()
	all, err := trace.Read[trace.Transition](repo, stream)
	fixturekit.Must(t, err)
	return slices.DeleteFunc(all, func(tr trace.Transition) bool { return router.IsJournal(tr.Subject) })
}

func driftOperations(t *testing.T, repo *trace.Repository, stream config.WorkstreamID) []trace.OperationRecord {
	return intakefixture.DriftOperations(t, repo, stream, service.DriftAction)
}

const sibling config.WorkstreamID = "w_0123456789abcdef0123456789abcdee"

func (s *Service) setReviewChecks(checker checks.ReviewChecks) {
	s.options.reviewChecks = checker
	InspectForTest(s).SetReviewChecks(checker)
}
func (s *Service) decideChange(ctx context.Context, raw, id string, d service.ChangeDecision) (lifecycle.WorkgroupChange, *api.APIError) {
	return s.lifecycle().DecideChange(ctx, raw, id, d)
}
func recordUpstreamOwnPullRequest(t *testing.T, s *Service, repo *trace.Repository, stream config.WorkstreamID, p api.DeliveryPublication) {
	t.Helper()
	fixturekit.UpstreamOwnPullRequest(t, repo, stream, p, s.now(), service.TestForemanActor, delivery.PublicationDocument, delivery.PublicationPath)
}
func (c *checkers) incompleteSinceEntry(stream config.WorkstreamID, unit string, latest execution.UnitCheckRun, runs []execution.UnitCheckRun) (int, error) {
	return c.driver().IncompleteSinceEntry(stream, unit, latest, runs)
}
func (f *shedFixture) awaitShed(t *testing.T, stream config.WorkstreamID, want string, never ...string) {
	t.Helper()
	f.scenario().AwaitShed(t, stream, want, never...)
}
