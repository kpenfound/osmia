package intakejourney

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/bundle"
	"github.com/kpenfound/osmia/internal/charter"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/scheduler"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/kpenfound/osmia/internal/workspace"
)

type Options struct {
	service.Options
	boundary       func(context.Context, string) error
	schedulePassed func()
	notifyInbox    func(context.Context) (api.InboxResponse, *api.APIError)
	checkJJ        func(context.Context) (string, error)
	sbxBin         string
	reviewChecks   service.ReviewChecks
	controls       *runtimeControls
}

func (o Options) hooks() service.TestHooks {
	h := service.TestHooks{Boundary: o.boundary, SchedulePassed: o.schedulePassed, NotifyInbox: o.notifyInbox, CheckJJ: o.checkJJ, SandboxBinary: o.sbxBin, ReviewChecks: o.reviewChecks}
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
func start(t *testing.T, o Options) (*Service, *Client) {
	t.Helper()
	s, err := Start(context.Background(), o)
	must(t, err)
	c := NewClient(s.Socket())
	t.Cleanup(func() { c.Close(); must(t, s.Close()) })
	return s, c
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
func fixtureAt(t *testing.T, home string) Options {
	return Options{Options: servicetest.FixtureAt(t, home)}
}
func fixture(t *testing.T) Options { return Options{Options: servicetest.Fixture(t)} }
func projectFixture(t *testing.T) (Options, string) {
	o, c := servicetest.ProjectFixture(t)
	return Options{Options: o}, c
}

func testOptions(c config.Options) Options { return Options{Options: servicetest.Options(c)} }
func serviceChanges(t *testing.T, s *Service) (func(), func()) {
	return servicetest.ServiceChanges(t, s.core())
}
func writeCharter(t *testing.T, c *Client, p config.ProjectID, content string) trace.Document {
	return servicetest.WriteCharter(t, c.Client, p, content)
}
func editDraft(t *testing.T, c *Client, w config.WorkstreamID, spec, plan string) map[string]trace.Document {
	return servicetest.EditDraft(t, c.Client, w, spec, plan)
}
func mutation(t *testing.T, c *Client, method, kind string, input any) {
	servicetest.Mutation(t, c.Client, method, kind, input)
}

type Service struct {
	*service.Service
	cfg      *config.Config
	options  Options
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

func (s *Service) roleExecution(c *config.Config, r string) (coreadapter.Profile, coreadapter.ExecutionSettings, error) {
	return s.intake().RoleExecution(c, r)
}
func (s *Service) refreshRuntime() error { return service.InspectForTest(s.core()).RefreshRuntime() }
func (s *Service) handle(w http.ResponseWriter, r *http.Request) {
	service.InspectForTest(s.core()).Handle(w, r)
}
func (s *Service) testError() error { return s.core().ErrForTest() }

type runtimeControls struct {
	service atomic.Pointer[Service]
	binding *service.TestEnforcementBinding
}

func (c *runtimeControls) intake() *intake.Controls {
	return &intake.Controls{Load: func() *intake.Controller {
		if c == nil || c.service.Load() == nil {
			return nil
		}
		return c.service.Load().intake()
	}}
}
func (c *runtimeControls) decideAmendment(r *trace.Repository, s coreadapter.Scope) coreadapter.Tool {
	return c.intake().DecideAmendment(r, s)
}
func (c *runtimeControls) decideCharter(r *trace.Repository, s coreadapter.Scope) coreadapter.Tool {
	return c.intake().DecideCharter(r, s)
}
func (c *runtimeControls) resolveObjection(r *trace.Repository, s coreadapter.Scope) coreadapter.Tool {
	return c.intake().ResolveObjection(r, s)
}

type Client struct {
	*service.Client
	defaultTimeout time.Duration
}

func NewClient(socket string) *Client {
	c := service.NewClient(socket)
	return &Client{Client: c, defaultTimeout: c.TimeoutForTest()}
}

type masons struct {
	s          *Service
	cfg        *config.Config
	repository *trace.Repository
}

func (m *masons) driver() *service.TestUnitDriver {
	var s *service.Service
	if m.s != nil {
		s = m.s.core()
	}
	return service.UnitsForTest(s, m.repository)
}
func (m *masons) read(w config.WorkstreamID) (service.TestBuilding, bool, error) {
	return m.driver().Read(w)
}
func (m *masons) start(ctx context.Context, b service.TestBuilding, u string) (bool, bool, error) {
	return m.driver().Start(ctx, b, u)
}
func (m *masons) finish(ctx context.Context, b service.TestBuilding, u string) (bool, bool, error) {
	return m.driver().Finish(ctx, b, u)
}
func (m *masons) bundle(ctx context.Context, w config.WorkstreamID, u string) (bundle.Mason, error) {
	return m.driver().Bundle(ctx, w, u)
}

type foreman struct{ *masons }

func (f *foreman) driver() *service.TestLandingDriver {
	return service.LandingForTest(f.masons.driver())
}
func (f *foreman) Pass(ctx context.Context) error { return f.driver().Pass(ctx) }
func (f *foreman) Apply(ctx context.Context, o coreadapter.Operation) (coreadapter.OperationResult, error) {
	return f.driver().Apply(ctx, o)
}
func (f *foreman) Inspect(ctx context.Context, o coreadapter.Operation) (coreadapter.Observation, error) {
	return f.driver().Inspect(ctx, o)
}
func (f *foreman) approval(w config.WorkstreamID, u string, v int) (trace.Document, UnitReviewResult, bool, error) {
	return f.driver().Approval(w, u, v)
}
func (f *foreman) current(ctx context.Context, w config.WorkstreamID, in landInput, r UnitReviewResult) (string, error) {
	return f.driver().Current(ctx, w, in, r)
}

type reviewers struct{ *masons }

func (r *reviewers) prepareUnitReview(ctx context.Context, w config.WorkstreamID, u string) (coreadapter.ReviewRequest, UnitReviewIdentity, error) {
	return service.ReviewsForTest(r.driver()).Prepare(ctx, w, u)
}
func (r *reviewers) one(ctx context.Context, w config.WorkstreamID, u string, st trace.WorkflowState, p bool) error {
	return service.ReviewsForTest(r.driver()).One(ctx, w, u, st, p)
}

type checkers struct{ *masons }

func (c *checkers) Pass(ctx context.Context) error {
	return service.ChecksForTest(c.driver()).Pass(ctx)
}
func (c *checkers) Apply(ctx context.Context, o coreadapter.Operation) (coreadapter.OperationResult, error) {
	return service.ChecksForTest(c.driver()).Apply(ctx, o)
}
func (c *checkers) Inspect(ctx context.Context, o coreadapter.Operation) (coreadapter.Observation, error) {
	return service.ChecksForTest(c.driver()).Inspect(ctx, o)
}

type finalReviewer struct {
	s          *Service
	repository *trace.Repository
}

func (f *finalReviewer) driver() *service.TestFinalDriver {
	return service.FinalForTest(f.s.core(), f.repository)
}
func (f *finalReviewer) Pass(ctx context.Context) error { return f.driver().Pass(ctx) }
func (f *finalReviewer) Apply(ctx context.Context, o coreadapter.Operation) (coreadapter.OperationResult, error) {
	return f.driver().Apply(ctx, o)
}
func (f *finalReviewer) Inspect(ctx context.Context, o coreadapter.Operation) (coreadapter.Observation, error) {
	return f.driver().Inspect(ctx, o)
}
func (f *finalReviewer) finalGate(ctx context.Context, w config.WorkstreamID) (FinalReport, string, error) {
	return f.driver().Gate(ctx, w)
}
func (f *finalReviewer) governing(ctx context.Context, w config.WorkstreamID) (service.TestFinalInput, seal.Seal, error) {
	return f.driver().Governing(ctx, w)
}

type publisher struct {
	s          *Service
	repository *trace.Repository
}

func (p *publisher) refuse(ctx context.Context, w config.WorkstreamID, in publishInput, reason string) (coreadapter.OperationResult, error) {
	return service.RefusePublicationForTest(ctx, p.s.core(), p.repository, w, in, reason)
}

type testUnitWorkspaces struct{ service.TestUnitWorkspaces }

func newUnitWorkspaces(c *config.Config, r *trace.Repository) testUnitWorkspaces {
	return testUnitWorkspaces{service.UnitWorkspacesForTest(c, r)}
}
func (w testUnitWorkspaces) open(ctx context.Context, s config.WorkstreamID, u string) (workspace.Worktree, string, error) {
	return w.Open(ctx, s, u)
}

func (s *Service) decideAmendment(ctx context.Context, raw string, id string, req AmendmentDecisionRequest) (AmendmentResponse, *APIError) {
	return s.intake().DecideAmendment(ctx, raw, id, req)
}
func (s *Service) recordCharterDecision(ctx context.Context, project config.ProjectID, stream config.WorkstreamID, repository *trace.Repository, question string, req CharterDecisionRequest, actor trace.Actor, cause string) (CharterProposalView, *APIError) {
	return s.intake().RecordCharterDecision(ctx, project, stream, repository, question, req, actor, cause)
}
func (s *Service) agentProfile(cfg *config.Config, role string, agent string) (coreadapter.Profile, error) {
	return s.intake().AgentProfile(cfg, role, agent)
}
func (s *Service) designTrace(raw string) (config.ProjectID, config.WorkstreamID, *trace.Repository, *APIError) {
	return s.intake().DesignTrace(raw)
}
func (s *Service) startDesignWorkgroup(ctx context.Context, raw string) (DesignStartResponse, *APIError) {
	return s.intake().StartDesignWorkgroup(ctx, raw)
}
func (s *Service) inbox(ctx context.Context) (InboxResponse, *APIError) { return s.intake().Inbox(ctx) }

func (s *Service) answer(ctx context.Context, raw string, req AnswerRequest) (AnswerResponse, *APIError) {
	return s.intake().Answer(ctx, raw, req)
}
func (s *Service) decideObjection(ctx context.Context, repository *trace.Repository, stream config.WorkstreamID, req objectionDecisionRequest) (shed.Entry, *APIError) {
	return s.intake().DecideObjection(ctx, repository, stream, req)
}
func (s *Service) answers(cfg *config.Config, repository *trace.Repository) *questions.Deliverer {
	return s.intake().Answers(cfg, repository)
}
func (s *Service) loadCharter(ctx context.Context, p config.ProjectID) (trace.Document, charter.Charter, error) {
	return s.intake().LoadCharter(ctx, p)
}
func (s *Service) chiefRepository(ctx context.Context, r *trace.Repository, w config.WorkstreamID) (*trace.Repository, config.WorkstreamID, config.WorkstreamID, func(), error) {
	return s.intake().ChiefRepository(ctx, r, w)
}
func (s *Service) newWorkspaces(ctx context.Context, c *config.Config) (WorkspacesStatus, error) {
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
func (s *Service) feedList(raw string) (FeedResponse, *APIError) {
	return service.InspectForTest(s.core()).Feed(raw)
}
func (s *Service) deliveryPresentation(ctx context.Context, raw string) (DeliveryPresentation, *APIError) {
	return service.InspectForTest(s.core()).Delivery(ctx, raw)
}
func (s *Service) approveDelivery(ctx context.Context, raw string, r DeliveryDecision) (DeliveryApproval, *APIError) {
	return service.InspectForTest(s.core()).ApproveDelivery(ctx, raw, r)
}
func (s *Service) ruleContested(ctx context.Context, w, u string, r ContestedRulingRequest) (ContestedRulingResponse, *APIError) {
	return service.InspectForTest(s.core()).RuleContested(ctx, w, u, r)
}
func (s *Service) dispatchHolds(c *config.Config, r *trace.Repository) func(scheduler.Candidate) (bool, error) {
	return service.InspectForTest(s.core()).DispatchHolds(c, r)
}
func (s *Service) holds(p config.ProjectID, w config.WorkstreamID, r *trace.Repository, c scheduler.Candidate) (bool, error) {
	return service.InspectForTest(s.core()).Holds(p, w, r, c)
}
func (s *Service) halt(p *activeProject) error { return service.InspectForTest(s.core()).Halt(p.id) }
func (m *masons) candidateEvidence(ctx context.Context, w config.WorkstreamID, u string) (coreadapter.ReviewRequest, UnitReviewIdentity, error) {
	return m.driver().CandidateEvidence(ctx, w, u)
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
