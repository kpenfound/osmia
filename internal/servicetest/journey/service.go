package journey

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/activity"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/eventstream"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/jev"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/scheduler"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

type Options struct {
	ConversationTicks <-chan time.Time
	service.Options
	Boundary       func(context.Context, string) error
	SchedulePassed func()
	NotifyInbox    func(context.Context) (api.InboxResponse, *api.APIError)
	ReviewChecks   service.ReviewChecks
	Controls       *RuntimeControls

	TelemetryExporter sdktrace.SpanExporter
}

func (o Options) Hooks() service.TestHooks {
	h := service.TestHooks{ConversationTicks: o.ConversationTicks, Boundary: o.Boundary, SchedulePassed: o.SchedulePassed, NotifyInbox: o.NotifyInbox, ReviewChecks: o.ReviewChecks, TelemetryExporter: o.TelemetryExporter}
	if o.Controls != nil {
		h.Enforcement = o.Controls.Binding
	}
	return h
}

func Enforce(o Options, e service.Enforcement) Options {
	o.Options = service.Enforce(o.Options, e)
	o.Controls = &RuntimeControls{Binding: service.EnforcementBindingForTest(o.Options)}
	return o
}

func StartT(t *testing.T, o Options) (*Service, *Client) {
	t.Helper()
	s, err := Start(context.Background(), o)
	fixturekit.Must(t, err)
	c := NewClient(s.Socket())
	t.Cleanup(func() { c.Close(); fixturekit.Must(t, s.Close()) })
	return s, c
}

func Start(ctx context.Context, o Options) (*Service, error) {
	s := &Service{Options: o, BoundaryHook: o.Boundary}
	s.Polls = PollsAccess{S: s}
	h := o.Hooks()
	h.Boundary = func(ctx context.Context, name string) error {
		if s.Boundary != nil {
			return s.Boundary(name)
		}
		if s.BoundaryHook != nil {
			return s.BoundaryHook(ctx, name)
		}
		return nil
	}
	core, err := servicetest.StartContext(ctx, o.Options, h)
	if err != nil {
		return nil, err
	}
	s.bind(core)
	if o.Controls != nil {
		o.Controls.Service.Store(s)
	}
	return s, nil
}

func (s *Service) bind(core *service.Service) {
	s.Service = core
	s.Mu.S = s
	s.Cfg = service.InspectForTest(core).Config()
	for _, p := range service.InspectForTest(core).Projects() {
		s.Projects = append(s.Projects, &ActiveProject{Id: p.ID, Repository: p.Repository, Pipeline: service.InspectForTest(core).Pipeline(p.ID)})
	}
	s.Options.ReviewChecks = service.InspectForTest(core).Execution().Checks
	s.Store = service.InspectForTest(core).Intake().Store()
	s.Options.Issues = service.InspectForTest(core).Intake().Options.Issues
	s.Done = core.DoneForTest()
	s.Lifetime = core.LifetimeForTest()
	s.Ready = ReadyAccess{S: core}
}

func FixtureAt(t *testing.T, home string) Options {
	return Options{Options: servicetest.FixtureAt(t, home)}
}

func Fixture(t *testing.T) Options { return Options{Options: servicetest.Fixture(t)} }

func ProjectFixture(t *testing.T) (Options, string) {
	o, c := servicetest.ProjectFixture(t)
	return Options{Options: o}, c
}

func TestOptions(c config.Options) Options { return Options{Options: servicetest.Options(c)} }

func ServiceChanges(t *testing.T, s *Service) (func(), func()) {
	return servicetest.ServiceChanges(t, s.Core())
}

type Service struct {
	*service.Service
	Cfg          *config.Config
	Options      Options
	Jev          *jev.Judge
	Store        *runtime.Store
	Boundary     func(string) error
	BoundaryHook func(context.Context, string) error
	Done         <-chan struct{}
	Lifetime     context.Context
	Ready        ReadyAccess
	Projects     []*ActiveProject
	Polls        PollsAccess
	Mu           FixtureMutex
	Activity     *activity.Registry
	Logger       *slog.Logger
	Hub          *eventstream.Hub
}

type FixtureMutex struct {
	S *Service
	sync.Mutex
}

func (m *FixtureMutex) Unlock() {
	s := m.S
	var ps []service.TestProject
	for _, p := range s.Projects {
		ps = append(ps, service.TestProject{ID: p.Id, Repository: p.Repository})
	}
	service.InspectForTest(s.Core()).SetFixtureState(s.Cfg, s.Options.Options, s.Store, ps)
	m.Mutex.Unlock()
}

type ReadyAccess struct{ S *service.Service }

func (a ReadyAccess) Load() bool { return a.S.ReadyForTest() }

type ActiveProject struct {
	Id         config.ProjectID
	Repository *trace.Repository
	Pipeline   *service.TestPipeline
}

func (s *Service) Core() *service.Service {
	if s.Service == nil {
		s.Mu.S = s
		var repos []*trace.Repository
		for _, p := range s.Projects {
			repos = append(repos, p.Repository)
		}
		if s.Logger != nil {
			s.Options.Logger = s.Logger
		}
		s.Service = service.NewFixtureForTest(s.Cfg, s.Options.Options, s.Store, repos)
		s.Polls = PollsAccess{S: s}
	}
	if s.Done == nil {
		service.InspectForTest(s.Service).SetFixtureStore(s.Store)
		if s.Logger != nil {
			service.InspectForTest(s.Service).SetFixtureLogger(s.Logger)
		}
	}
	if s.Activity != nil {
		service.InspectForTest(s.Service).SetActivity(s.Activity)
	}
	if s.Hub != nil {
		service.InspectForTest(s.Service).SetHub(s.Hub)
	}
	return s.Service
}

func (s *Service) Intake() *intake.Controller {
	c := service.InspectForTest(s.Core()).Intake()
	c.Options.Architect = s.Options.Architect
	c.Options.Committee = s.Options.Committee
	if s.Boundary != nil {
		c.Step = s.Boundary
	}
	return c
}

func (s *Service) Delivery() *delivery.Controller {
	c := service.InspectForTest(s.Core()).DeliveryController()
	if s.Jev != nil {
		c.Jev = s.Jev
	}
	c.Committee = s.Options.Committee
	c.PullRequests = s.Options.PullRequests
	c.CloudStatuses, c.CloudRerun, c.CloudTrace, c.PostPushWait = s.Options.CloudStatuses, s.Options.CloudRerun, s.Options.CloudTrace, s.Options.FinalChecksWait
	if s.Options.Reconciliation.Now != nil {
		c.Now = s.Options.Reconciliation.Now
	}
	c.Execution = s.Execution
	if s.Boundary != nil {
		c.Step = s.Boundary
	}
	return c
}

func (s *Service) Current() *config.Config { return service.InspectForTest(s.Core()).Config() }

func (s *Service) About(r *trace.Repository) *config.Config { return s.Intake().About(r) }

func (s *Service) Now() time.Time { return s.Intake().Now() }

func (s *Service) Repository(p config.ProjectID) (*trace.Repository, error) {
	return service.InspectForTest(s.Core()).Repository(p)
}

func (s *Service) Sole() *ActiveProject {
	r := service.InspectForTest(s.Core()).SoleRepository()
	if r == nil {
		return nil
	}
	return &ActiveProject{Id: r.Project(), Repository: r, Pipeline: service.InspectForTest(s.Core()).Pipeline(r.Project())}
}

func (s *Service) SetSole(p *ActiveProject) {
	if p == nil {
		service.InspectForTest(s.Core()).SetTrace(nil)
		return
	}
	if p.Id == "" {
		p.Id = p.Repository.Project()
	}
	s.Projects = []*ActiveProject{p}
	service.InspectForTest(s.Core()).SetTrace(p.Repository)
}

func (s *Service) RoleExecution(c *config.Config, r string) (coreadapter.Profile, coreadapter.ExecutionSettings, error) {
	return s.Intake().RoleExecution(c, r)
}

func (s *Service) Handle(w http.ResponseWriter, r *http.Request) {
	service.InspectForTest(s.Core()).Handle(w, r)
}

func (s *Service) ShedAction(raw string, states ...string) (*intake.ShedOwner, *api.APIError) {
	return service.InspectForTest(s.Core()).ShedAction(raw, states...)
}

func (s *Service) ClearBoundary() {
	s.Boundary = nil
	s.BoundaryHook = nil
}

func (s *Service) ProjectViews() []*ActiveProject {
	_, ps := s.Runtimes()
	return ps
}

func (s *Service) TestError() error { return s.Core().ErrForTest() }

type RuntimeControls struct {
	Service atomic.Pointer[Service]
	Binding *service.TestEnforcementBinding
}

func (c *RuntimeControls) Intake() *intake.Controls {
	return &intake.Controls{Load: func() *intake.Controller {
		if c == nil || c.Service.Load() == nil {
			return nil
		}
		return c.Service.Load().Intake()
	}}
}

func (c *RuntimeControls) DecideAmendment(r *trace.Repository, s coreadapter.Scope) coreadapter.Tool {
	return c.Intake().DecideAmendment(r, s)
}

func (c *RuntimeControls) DecideCharter(r *trace.Repository, s coreadapter.Scope) coreadapter.Tool {
	return c.Intake().DecideCharter(r, s)
}

func (c *RuntimeControls) ResolveObjection(r *trace.Repository, s coreadapter.Scope) coreadapter.Tool {
	return c.Intake().ResolveObjection(r, s)
}

type Client struct {
	*service.Client
	DefaultTimeout time.Duration
}

func NewClient(socket string) *Client {
	c := service.NewClient(socket)
	return &Client{Client: c, DefaultTimeout: c.TimeoutForTest()}
}

func (s *Service) DecideAmendment(ctx context.Context, raw string, id string, req api.AmendmentDecisionRequest) (api.AmendmentResponse, *api.APIError) {
	return s.Intake().DecideAmendment(ctx, raw, id, req)
}

func (s *Service) RecordCharterDecision(ctx context.Context, project config.ProjectID, stream config.WorkstreamID, repository *trace.Repository, question string, req api.CharterDecisionRequest, actor trace.Actor, cause string) (api.CharterProposalView, *api.APIError) {
	return s.Intake().RecordCharterDecision(ctx, project, stream, repository, question, req, actor, cause)
}

func (s *Service) AgentProfile(cfg *config.Config, role string, agent string) (coreadapter.Profile, error) {
	return s.Intake().AgentProfile(cfg, role, agent)
}

func (s *Service) DesignTrace(raw string) (config.ProjectID, config.WorkstreamID, *trace.Repository, *api.APIError) {
	return s.Intake().DesignTrace(raw)
}

func (s *Service) StartDesignWorkgroup(ctx context.Context, raw string) (api.DesignStartResponse, *api.APIError) {
	return s.Intake().StartDesignWorkgroup(ctx, raw)
}

func (s *Service) Inbox(ctx context.Context) (api.InboxResponse, *api.APIError) {
	c := s.Intake()
	c.DeliveryEntry = s.Delivery().DeliveryEntry
	c.PublicationEntry = s.Delivery().PublicationEntry
	return c.Inbox(ctx)
}

func (s *Service) Answer(ctx context.Context, raw string, req api.AnswerRequest) (api.AnswerResponse, *api.APIError) {
	return s.Intake().Answer(ctx, raw, req)
}

func (s *Service) DecideObjection(ctx context.Context, repository *trace.Repository, stream config.WorkstreamID, req intake.ObjectionDecisionRequest) (shed.Entry, *api.APIError) {
	return s.Intake().DecideObjection(ctx, repository, stream, req)
}

func (s *Service) Answers(cfg *config.Config, repository *trace.Repository) *questions.Deliverer {
	return s.Intake().Answers(cfg, repository)
}

func (s *Service) ChiefRepository(ctx context.Context, r *trace.Repository, w config.WorkstreamID) (*trace.Repository, config.WorkstreamID, config.WorkstreamID, func(), error) {
	return s.Intake().ChiefRepository(ctx, r, w)
}

func (s *Service) Runtimes() (*config.Config, []*ActiveProject) {
	cfg, ps := s.Intake().Runtimes()
	var out []*ActiveProject
	for _, p := range ps {
		var view *ActiveProject
		for _, old := range s.Projects {
			if old.Id == p.ID {
				view = old
				break
			}
		}
		if view == nil {
			view = &ActiveProject{Id: p.ID}
		}
		view.Repository = p.Repository
		view.Pipeline = service.InspectForTest(s.Core()).Pipeline(p.ID)
		out = append(out, view)
	}
	s.Projects = out
	return cfg, out
}

func (s *Service) FeedList(raw string) (api.FeedResponse, *api.APIError) {
	return service.InspectForTest(s.Core()).Feed(raw)
}

func (s *Service) DeliveryPresentation(ctx context.Context, raw string) (api.DeliveryPresentation, *api.APIError) {
	return s.Delivery().DeliveryPresentation(ctx, raw)
}

func (s *Service) ApproveDelivery(ctx context.Context, raw string, r api.DeliveryDecision) (api.DeliveryApproval, *api.APIError) {
	return s.Delivery().ApproveDelivery(ctx, raw, r)
}

func (s *Service) RuleContested(ctx context.Context, w, u string, r api.ContestedRulingRequest) (api.ContestedRulingResponse, *api.APIError) {
	return service.InspectForTest(s.Core()).RuleContested(ctx, w, u, r)
}

func (s *Service) DispatchHolds(c *config.Config, r *trace.Repository) func(scheduler.Candidate) (bool, error) {
	return service.InspectForTest(s.Core()).DispatchHolds(c, r)
}

func (s *Service) Holds(p config.ProjectID, r *trace.Repository, c scheduler.Candidate) (bool, error) {
	return service.InspectForTest(s.Core()).Holds(p, r, c)
}

func (s *Service) Halt(p *ActiveProject) error { return service.InspectForTest(s.Core()).Halt(p.Id) }

func InspectForTest(s *Service) service.TestAccess { return service.InspectForTest(s.Core()) }

type PollsAccess struct{ S *Service }

func (p PollsAccess) NextDue(project config.ProjectID, interval time.Duration, now time.Time) time.Time {
	var attempts []intakefixture.PollAttempt
	for _, a := range service.InspectForTest(p.S.Core()).PollAttempts() {
		attempts = append(attempts, intakefixture.PollAttempt{Project: a.Project, At: a.At})
	}
	return intakefixture.NextPollDue(attempts, project, interval, now)
}

type PollAttempt struct{ At time.Time }

func (p PollsAccess) Last(project config.ProjectID, w config.WorkstreamID, kind string) (PollAttempt, bool) {
	for _, a := range service.InspectForTest(p.S.Core()).PollAttempts() {
		if a.Project == project && a.Workstream == w && a.Kind == kind {
			return PollAttempt{At: a.At}, true
		}
	}
	return PollAttempt{}, false
}

type PageFixture struct {
	S     *Service
	C     *Client
	Chief coreadapter.Scope
}

func NewPageFixture(t *testing.T) *PageFixture {
	t.Helper()
	f := servicetest.NewPageFixture(t)
	s := &Service{}
	s.Polls = PollsAccess{S: s}
	s.bind(f.S)
	return &PageFixture{S: s, C: &Client{Client: f.C, DefaultTimeout: f.C.TimeoutForTest()}, Chief: f.Chief}
}

// Until re-evaluates cond whenever the service announces a change, and on a
// short timer for state it does not announce, until it holds or DemoTimeout
// passes.
func (s *Service) Until(t testing.TB, what string, cond func() bool) {
	t.Helper()
	wake, release, ok := service.InspectForTest(s.Core()).ChangesEvery(50 * time.Millisecond)
	if !ok {
		wake, release = fixturekit.Ticks(50 * time.Millisecond)
	}
	defer release()
	fixturekit.Until(t, what, wake, cond)
}

func (c *RuntimeControls) Execution() *execution.Controls {
	return &execution.Controls{Load: func() *execution.Controller {
		if c == nil || c.Service.Load() == nil {
			return nil
		}
		return c.Service.Load().Execution()
	}}
}
