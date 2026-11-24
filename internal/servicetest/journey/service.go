package journey

import (
	"context"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/bundle"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/scheduler"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

type Options struct {
	service.Options
	Boundary       func(context.Context, string) error
	SchedulePassed func()
	NotifyInbox    func(context.Context) (api.InboxResponse, *api.APIError)
	ReviewChecks   service.ReviewChecks
	Controls       *RuntimeControls
}

func (o Options) Hooks() service.TestHooks {
	h := service.TestHooks{Boundary: o.Boundary, SchedulePassed: o.SchedulePassed, NotifyInbox: o.NotifyInbox, ReviewChecks: o.ReviewChecks}
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
	s := &Service{Options: o}
	s.Polls = PollsAccess{S: s}
	h := o.Hooks()
	boundary := h.Boundary
	h.Boundary = func(ctx context.Context, name string) error {
		if s.Boundary != nil {
			return s.Boundary(name)
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
	s.Store = service.InspectForTest(core).Intake().Store()
	s.Options.Issues = service.InspectForTest(core).Intake().Options.Issues
	s.Done = core.DoneForTest()
	s.Lifetime = core.LifetimeForTest()
	s.Ready = ReadyAccess{S: core}
	if o.Controls != nil {
		o.Controls.Service.Store(s)
	}
	return s, nil
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

func WriteCharter(t *testing.T, c *Client, p config.ProjectID, content string) trace.Document {
	return servicetest.WriteCharter(t, c.Client, p, content)
}

func Mutation(t *testing.T, c *Client, method, kind string, input any) {
	servicetest.Mutation(t, c.Client, method, kind, input)
}

type Service struct {
	*service.Service
	Cfg      *config.Config
	Options  Options
	Store    *runtime.Store
	Boundary func(string) error
	Done     <-chan struct{}
	Lifetime context.Context
	Ready    ReadyAccess
	Projects []*ActiveProject
	Polls    PollsAccess
}

type ReadyAccess struct{ S *service.Service }

func (a ReadyAccess) Load() bool { return a.S.ReadyForTest() }

type ActiveProject struct {
	Id         config.ProjectID
	Repository *trace.Repository
}

func (s *Service) Core() *service.Service {
	if s.Service == nil {
		var repos []*trace.Repository
		for _, p := range s.Projects {
			repos = append(repos, p.Repository)
		}
		s.Service = service.NewFixtureForTest(s.Cfg, s.Options.Options, s.Store, repos)
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
	return &ActiveProject{Id: r.Project(), Repository: r}
}

func (s *Service) SetSole(p *ActiveProject) {
	if p == nil {
		service.InspectForTest(s.Core()).SetTrace(nil)
		return
	}
	service.InspectForTest(s.Core()).SetTrace(p.Repository)
}

func (s *Service) RoleExecution(c *config.Config, r string) (coreadapter.Profile, coreadapter.ExecutionSettings, error) {
	return s.Intake().RoleExecution(c, r)
}

func (s *Service) Handle(w http.ResponseWriter, r *http.Request) {
	service.InspectForTest(s.Core()).Handle(w, r)
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

type Masons struct {
	S          *Service
	Cfg        *config.Config
	Repository *trace.Repository
}

func (m *Masons) Driver() *service.TestUnitDriver {
	var s *service.Service
	if m.S != nil {
		s = m.S.Core()
	}
	return service.UnitsForTest(s, m.Repository)
}

func (m *Masons) Read(w config.WorkstreamID) (service.TestBuilding, bool, error) {
	return m.Driver().Read(w)
}

func (m *Masons) Start(ctx context.Context, b service.TestBuilding, u string) (bool, bool, error) {
	return m.Driver().Start(ctx, b, u)
}

func (m *Masons) Finish(ctx context.Context, b service.TestBuilding, u string) (bool, bool, error) {
	return m.Driver().Finish(ctx, b, u)
}

func (m *Masons) Bundle(ctx context.Context, w config.WorkstreamID, u string) (bundle.Mason, error) {
	return m.Driver().Bundle(ctx, w, u)
}

type Foreman struct{ *Masons }

func (f *Foreman) Driver() *service.TestLandingDriver {
	return service.LandingForTest(f.Masons.Driver())
}

func (f *Foreman) Pass(ctx context.Context) error { return f.Driver().Pass(ctx) }

func (f *Foreman) Apply(ctx context.Context, o coreadapter.Operation) (coreadapter.OperationResult, error) {
	return f.Driver().Apply(ctx, o)
}

func (f *Foreman) Inspect(ctx context.Context, o coreadapter.Operation) (coreadapter.Observation, error) {
	return f.Driver().Inspect(ctx, o)
}

func (f *Foreman) Approval(w config.WorkstreamID, u string, v int) (trace.Document, service.UnitReviewResult, bool, error) {
	return f.Driver().Approval(w, u, v)
}

func (f *Foreman) Current(ctx context.Context, w config.WorkstreamID, in service.TestLandInput, r service.UnitReviewResult) (string, error) {
	return f.Driver().Current(ctx, w, in, r)
}

type Reviewers struct{ *Masons }

func (r *Reviewers) PrepareUnitReview(ctx context.Context, w config.WorkstreamID, u string) (coreadapter.ReviewRequest, service.UnitReviewIdentity, error) {
	return service.ReviewsForTest(r.Driver()).Prepare(ctx, w, u)
}

func (r *Reviewers) One(ctx context.Context, w config.WorkstreamID, u string, st trace.WorkflowState, p bool) error {
	return service.ReviewsForTest(r.Driver()).One(ctx, w, u, st, p)
}

type Checkers struct{ *Masons }

func (c *Checkers) Pass(ctx context.Context) error {
	return service.ChecksForTest(c.Driver()).Pass(ctx)
}

func (c *Checkers) Apply(ctx context.Context, o coreadapter.Operation) (coreadapter.OperationResult, error) {
	return service.ChecksForTest(c.Driver()).Apply(ctx, o)
}

func (c *Checkers) Inspect(ctx context.Context, o coreadapter.Operation) (coreadapter.Observation, error) {
	return service.ChecksForTest(c.Driver()).Inspect(ctx, o)
}

type FinalReviewer struct {
	S          *Service
	Repository *trace.Repository
}

func (f *FinalReviewer) Driver() *service.TestFinalDriver {
	return service.FinalForTest(f.S.Core(), f.Repository)
}

func (f *FinalReviewer) Pass(ctx context.Context) error { return f.Driver().Pass(ctx) }

func (f *FinalReviewer) Apply(ctx context.Context, o coreadapter.Operation) (coreadapter.OperationResult, error) {
	return f.Driver().Apply(ctx, o)
}

func (f *FinalReviewer) Inspect(ctx context.Context, o coreadapter.Operation) (coreadapter.Observation, error) {
	return f.Driver().Inspect(ctx, o)
}

func (f *FinalReviewer) FinalGate(ctx context.Context, w config.WorkstreamID) (api.FinalReport, string, error) {
	return f.Driver().Gate(ctx, w)
}

func (f *FinalReviewer) Governing(ctx context.Context, w config.WorkstreamID) (service.TestFinalInput, seal.Seal, error) {
	return f.Driver().Governing(ctx, w)
}

type Publisher struct {
	S          *Service
	Repository *trace.Repository
}

func (p *Publisher) Refuse(ctx context.Context, w config.WorkstreamID, in service.TestPublishInput, reason string) (coreadapter.OperationResult, error) {
	return service.RefusePublicationForTest(ctx, p.S.Core(), p.Repository, w, in, reason)
}

func NewUnitWorkspaces(c *config.Config, r *trace.Repository) service.TestUnitWorkspaces {
	return service.UnitWorkspacesForTest(c, r)
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
	return s.Intake().Inbox(ctx)
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
		out = append(out, &ActiveProject{Id: p.ID, Repository: p.Repository})
	}
	return cfg, out
}

func (s *Service) FeedList(raw string) (api.FeedResponse, *api.APIError) {
	return service.InspectForTest(s.Core()).Feed(raw)
}

func (s *Service) DeliveryPresentation(ctx context.Context, raw string) (api.DeliveryPresentation, *api.APIError) {
	return service.InspectForTest(s.Core()).Delivery(ctx, raw)
}

func (s *Service) ApproveDelivery(ctx context.Context, raw string, r api.DeliveryDecision) (api.DeliveryApproval, *api.APIError) {
	return service.InspectForTest(s.Core()).ApproveDelivery(ctx, raw, r)
}

func (s *Service) RuleContested(ctx context.Context, w, u string, r api.ContestedRulingRequest) (api.ContestedRulingResponse, *api.APIError) {
	return service.InspectForTest(s.Core()).RuleContested(ctx, w, u, r)
}

func (s *Service) DispatchHolds(c *config.Config, r *trace.Repository) func(scheduler.Candidate) (bool, error) {
	return service.InspectForTest(s.Core()).DispatchHolds(c, r)
}

func (s *Service) Holds(p config.ProjectID, w config.WorkstreamID, r *trace.Repository, c scheduler.Candidate) (bool, error) {
	return service.InspectForTest(s.Core()).Holds(p, r, c)
}

func (s *Service) Halt(p *ActiveProject) error { return service.InspectForTest(s.Core()).Halt(p.Id) }

func (m *Masons) CandidateEvidence(ctx context.Context, w config.WorkstreamID, u string) (coreadapter.ReviewRequest, service.UnitReviewIdentity, error) {
	return m.Driver().CandidateEvidence(ctx, w, u)
}

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
	return &PageFixture{S: &Service{Service: f.S}, C: &Client{Client: f.C, DefaultTimeout: f.C.TimeoutForTest()}, Chief: f.Chief}
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
