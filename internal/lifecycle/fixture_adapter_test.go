package lifecycle_test

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/activity"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/eventstream"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/jev"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/scheduler"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
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
func Start(ctx context.Context, o Options) (*Service, error) {
	s := &Service{options: o, boundaryHook: o.boundary}
	s.polls = pollsAccess{s: s}
	h := o.hooks()
	h.Boundary = func(ctx context.Context, name string) error {
		if s.boundary != nil {
			return s.boundary(name)
		}
		if s.boundaryHook != nil {
			return s.boundaryHook(ctx, name)
		}
		return nil
	}
	core, err := servicetest.StartContext(ctx, o.Options, h)
	if err != nil {
		return nil, err
	}
	s.Service = core
	s.mu.s = s
	s.cfg = service.InspectForTest(core).Config()
	for _, p := range service.InspectForTest(core).Projects() {
		s.projects = append(s.projects, &activeProject{id: p.ID, repository: p.Repository, pipeline: service.InspectForTest(core).Pipeline(p.ID)})
	}
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

type Service struct {
	*service.Service
	cfg          *config.Config
	options      Options
	jev          *jev.Judge
	store        *runtime.Store
	boundary     func(string) error
	boundaryHook func(context.Context, string) error
	done         <-chan struct{}
	lifetime     context.Context
	ready        readyAccess
	projects     []*activeProject
	polls        pollsAccess
	mu           fixtureMutex
	activity     *activity.Registry
	logger       *slog.Logger
	hub          *eventstream.Hub
}
type readyAccess struct{ s *service.Service }

func (a readyAccess) Load() bool { return a.s.ReadyForTest() }

type activeProject struct {
	id         config.ProjectID
	repository *trace.Repository
	pipeline   *service.TestPipeline
}

func (s *Service) core() *service.Service {
	if s.Service == nil {
		s.mu.s = s
		var repos []*trace.Repository
		for _, p := range s.projects {
			repos = append(repos, p.repository)
		}
		if s.logger != nil {
			s.options.Logger = s.logger
		}
		s.Service = service.NewFixtureForTest(s.cfg, s.options.Options, s.store, repos)
		s.polls = pollsAccess{s: s}
	}
	if s.done == nil {
		service.InspectForTest(s.Service).SetFixtureStore(s.store)
		if s.logger != nil {
			service.InspectForTest(s.Service).SetFixtureLogger(s.logger)
		}
	}
	if s.activity != nil {
		service.InspectForTest(s.Service).SetActivity(s.activity)
	}
	if s.hub != nil {
		service.InspectForTest(s.Service).SetHub(s.hub)
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
	return &activeProject{id: r.Project(), repository: r, pipeline: service.InspectForTest(s.core()).Pipeline(r.Project())}
}
func (s *Service) setSole(p *activeProject) {
	if p == nil {
		service.InspectForTest(s.core()).SetTrace(nil)
		return
	}
	s.projects = []*activeProject{p}
	service.InspectForTest(s.core()).SetTrace(p.repository)
}
func (s *Service) refreshRuntime() error { return service.InspectForTest(s.core()).RefreshRuntime() }
func (s *Service) testError() error      { return s.core().ErrForTest() }

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
func (s *Service) runtimes() (*config.Config, []*activeProject) {
	cfg, ps := s.intake().Runtimes()
	var out []*activeProject
	for _, p := range ps {
		var view *activeProject
		for _, old := range s.projects {
			if old.id == p.ID {
				view = old
				break
			}
		}
		if view == nil {
			view = &activeProject{id: p.ID}
		}
		view.repository = p.Repository
		view.pipeline = service.InspectForTest(s.core()).Pipeline(p.ID)
		out = append(out, view)
	}
	s.projects = out
	return cfg, out
}
func (s *Service) feedList(raw string) (FeedResponse, *APIError) {
	return service.InspectForTest(s.core()).Feed(raw)
}
func (s *Service) holds(p config.ProjectID, w config.WorkstreamID, r *trace.Repository, c scheduler.Candidate) (bool, error) {
	return service.InspectForTest(s.core()).Holds(p, w, r, c)
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

func (s *Service) delivery() *delivery.Controller {
	c := service.InspectForTest(s.core()).DeliveryController()
	c.Committee = s.options.Committee
	c.Checks = s.options.reviewChecks
	c.PullRequests = s.options.PullRequests
	if s.options.Reconciliation.Now != nil {
		c.Now = s.options.Reconciliation.Now
	}
	c.Execution = s.execution
	if s.boundary != nil {
		c.Step = s.boundary
	}
	return c
}

type fixtureMutex struct {
	s *Service
	sync.Mutex
}

func (m *fixtureMutex) Unlock() {
	s := m.s
	var ps []service.TestProject
	for _, p := range s.projects {
		ps = append(ps, service.TestProject{ID: p.id, Repository: p.repository})
	}
	service.InspectForTest(s.core()).SetFixtureState(s.cfg, s.options.Options, s.store, ps)
	m.Mutex.Unlock()
}
func (s *Service) runtimeOf(id config.ProjectID) (*config.Config, *activeProject) {
	cfg, ps := s.runtimes()
	for _, p := range ps {
		if p.id == id {
			return cfg, p
		}
	}
	return cfg, nil
}
func (s *Service) installPipeline(p *service.TestPipeline) {
	service.InspectForTest(s.core()).SetPipeline(s.cfg.Project.ID, p)
}
func (s *Service) stages(cfg *config.Config, r *trace.Repository) (*service.TestStages, error) {
	return service.InspectForTest(s.core()).Stages(cfg, r)
}

type scheduleHook = service.TestScheduleHook

var newTestPipeline = service.NewPipelineForTest

func (s *Service) projectViews() []*activeProject { _, ps := s.runtimes(); return ps }

func (s *Service) clearBoundary() {
	s.boundary = nil
	s.boundaryHook = nil
}
