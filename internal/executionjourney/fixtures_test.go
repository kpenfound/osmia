package executionjourney

import (
	context "context"
	"github.com/kpenfound/osmia/internal/api"
	config "github.com/kpenfound/osmia/internal/config"
	coreadapter "github.com/kpenfound/osmia/internal/coreadapter"
	delivery "github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/intake"
	lifecycle "github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/questions"
	runtime "github.com/kpenfound/osmia/internal/runtime"
	service "github.com/kpenfound/osmia/internal/service"
	servicetest "github.com/kpenfound/osmia/internal/servicetest"
	fixturekit "github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	shed "github.com/kpenfound/osmia/internal/shed"
	trace "github.com/kpenfound/osmia/internal/trace"
	http "net/http"
	strings "strings"
	testing "testing"
	time "time"
)

func (f *architectFixture) delivered(turn string) []intake.DraftStatus {
	return f.scenario().Delivered(turn)
}

func (s *Service) saveChange(ctx context.Context, repo *trace.Repository, c *lifecycle.WorkgroupChange) error {
	return s.lifecycle().SaveChange(ctx, repo, c)
}

func draftDelivery(cfg *config.Config, r *trace.Repository, w config.WorkstreamID, report api.FinalReport) (string, string, error) {
	return delivery.New(delivery.Dependencies{PendingMergeOrderReferences: lifecycle.PendingMergeOrderReferences}).DraftDelivery(cfg, r, w, report)
}

func (f *shedFixture) ruling(t *testing.T, stream config.WorkstreamID, requester string) []trace.QueuedTurn {
	t.Helper()
	return intakefixture.AmendmentRuling(t, f.repository(), stream, requester)
}

func newCloneFixture(t *testing.T) *cloneFixture {
	t.Helper()
	data := fixturekit.NewCloneSetup(t)
	opts := testOptions(data.Config)
	opts.Workstreams = nil
	opts.reviewChecks = fixturekit.PassingChecks
	opts.Reconciliation.Now = data.Clock.Now
	return &cloneFixture{opts: opts, clone: data.Clone, engine: data.Engine, sessions: data.Sessions, clock: data.Clock}
}

func notifyFixture(t *testing.T, webhook string) (Options, *fakeInbox) {
	t.Helper()
	opts := fixture(t)
	inbox := &fakeInbox{}
	opts.notifyInbox = inbox.read
	opts.NotifyRetry = 10 * time.Millisecond
	if webhook != "" {
		setWebhook(t, opts, webhook)
	}
	return opts, inbox
}

func (f *architectFixture) documents(t *testing.T, stream config.WorkstreamID, id string) []trace.Document {
	t.Helper()
	return f.scenario().Documents(t, stream, id)
}

func (f *architectFixture) transitions(t *testing.T, stream config.WorkstreamID) []trace.Transition {
	t.Helper()
	return f.scenario().Transitions(t, stream)
}

func startNotifying(t *testing.T, opts Options, inbox *fakeInbox) (*Service, *Client) {
	t.Helper()
	s, c := start(t, opts)
	inbox.Observe(func() { service.InspectForTest(s.core()).Publish(api.Event{Kind: api.EventInbox}) })
	return s, c
}

type fakeWebhook struct{ *fixturekit.Webhook }

func (w *fakeWebhook) hook() string { return w.Hook() }

func (w *fakeWebhook) accepted() []string { return w.Accepted() }

func (w *fakeWebhook) received() []string { return w.Received() }

func (w *fakeWebhook) respond(status int) { w.Respond(status) }

func (w *fakeWebhook) await(t *testing.T, n int) []string {
	t.Helper()
	return intakefixture.AwaitWebhook(t, w.Webhook, n)
}

func eventTraceOptions(t *testing.T) Options {
	t.Helper()
	opts, cfg := conversationFixture(t, "evs-")
	fixturekit.SeedEventTrace(t, cfg)
	return opts
}

func (s *Service) requestChange(ctx context.Context, raw string, req lifecycle.ChangeRequest) (lifecycle.WorkgroupChange, *api.APIError) {
	return s.lifecycle().RequestChange(ctx, raw, req)
}

type fakeInbox struct{ fixturekit.NotificationInbox }

func (f *fakeInbox) read(ctx context.Context) (api.InboxResponse, *api.APIError) { return f.Read(ctx) }

func (f *fakeInbox) set(entries ...api.InboxEntry) { f.Set(entries...) }

func (f *fakeInbox) breakInbox(broken bool) { f.Break(broken) }

func setWebhook(t *testing.T, opts Options, webhook string) {
	t.Helper()
	fixturekit.SetWebhook(t, opts.Config.Root, webhook)
}

func (s *Service) ensureWorkgroups(ctx context.Context, cfg *config.Config, projects []*activeProject, archives []runtime.Archive) error {
	var ps []*lifecycle.Project
	for _, p := range projects {
		ps = append(ps, &lifecycle.Project{ID: p.id, Repository: p.repository})
	}
	return s.lifecycle().EnsureWorkgroups(ctx, cfg, ps, archives)
}

func setUpBusyReadsService(t *testing.T, blocking coreadapter.Reconciler) (*Service, *Client, *trace.Repository) {
	t.Helper()
	opts := testOptions(fixturekit.BusyReadsConfig(t))
	opts.Reconciliation.Adapters = map[coreadapter.OperationBoundary]coreadapter.Reconciler{coreadapter.RepositoryBoundary: blocking}
	s, c := start(t, opts)
	_, active := s.runtimeOf(fixturekit.Project)
	if active == nil {
		t.Fatal("no active project")
	}
	return s, c, active.repository
}

func readLedger(t *testing.T, opts Options) service.TestNotifyLedger {
	t.Helper()
	return fixturekit.ReadNotificationLedger[service.TestNotifyLedger](t, opts.Config.Root)
}

func builtForAmendment(t *testing.T, rounds int) (*shedFixture, config.WorkstreamID) {
	t.Helper()
	f := newDebateFixture(t, 1, rounds)
	return f, f.buildFixture().Amendment(t)
}

func (s *Service) lifecycle() *lifecycle.Controller {
	c := service.InspectForTest(s.core()).LifecycleController()
	c.Options.Architect = s.options.Architect
	c.Options.Committee = s.options.Committee
	c.Options.PullRequests = s.options.PullRequests
	c.Execution = s.execution
	c.Intake = s.intake
	if s.options.Reconciliation.Now != nil {
		c.Now = s.options.Reconciliation.Now
	}
	if s.boundary != nil {
		c.Step = s.boundary
	}
	return c
}

func (s *Service) handle(w http.ResponseWriter, r *http.Request) {
	service.InspectForTest(s.core()).Handle(w, r)
}

func fixtureAt(t *testing.T, home string) Options {
	return Options{Options: servicetest.FixtureAt(t, home)}
}

func newHandInFixture(t *testing.T) *handInFixture {
	t.Helper()
	opts, clone := projectFixture(t)
	f := &handInFixture{}
	data := intakefixture.NewHandInSetup(t, clone, func(fake *fixturekit.FakeIssues) intakefixture.ArchitectProjectClient {
		opts.Issues = fake
		f.s, f.c = start(t, opts)
		return f.c
	}, func(t *testing.T, project config.ProjectID, text string) { writeCharter(t, f.c, project, text) })
	f.issues, f.project, f.trace, f.home = data.Issues, data.Project, data.Trace, data.Home
	return f
}

type cloneFixture struct {
	opts     Options
	clone    string
	engine   *fixturekit.DemoEngine
	sessions *fixturekit.DemoSessions
	clock    *fixturekit.DemoClock
}

func newArchitectFixture(t *testing.T, configure ...func(*Options)) *architectFixture {
	t.Helper()
	opts, clone, engine, sessions, clock := newArchitectOptions(t)
	for _, apply := range configure {
		apply(&opts)
	}
	f := &architectFixture{opts: opts, clone: clone, engine: engine, sessions: sessions, clock: clock}
	f.start(t)
	f.project, f.trace = intakefixture.AddArchitectProject(t, f.c, clone, "1. Keep changes small.\n2. Every change has a test.\n", func(t *testing.T, p config.ProjectID, text string) { writeCharter(t, f.c, p, text) })
	f.s.cfg = f.s.current()
	return f
}

func start(t *testing.T, o Options) (*Service, *Client) {
	t.Helper()
	s, err := Start(context.Background(), o)
	fixturekit.Must(t, err)
	c := NewClient(s.Socket())
	t.Cleanup(func() { c.Close(); fixturekit.Must(t, s.Close()) })
	return s, c
}

func (w *fakeWebhook) records(opts Options) *fakeWebhook {
	w.Webhook.Records(opts.Config.Root)
	return w
}

func (s *Service) deltaTool(wt *trace.Repository, c lifecycle.WorkgroupChange, agent, name string) coreadapter.Tool {
	return s.lifecycle().DeltaTool(wt, c, agent, name)
}

func (f *shedFixture) ownerMoves(t *testing.T, stream config.WorkstreamID) []string {
	t.Helper()
	return intakefixture.OwnerMoves(t, f.transitions(t, stream))
}

func eventTraceFixture(t *testing.T) (*Service, *Client) {
	t.Helper()
	return start(t, eventTraceOptions(t))
}

func (f *shedFixture) awaitAmendment(t *testing.T, stream config.WorkstreamID, want string) {
	t.Helper()
	intakefixture.AwaitAmendment(t, f.repository(), stream, want)
}

func notifyDiagnostics(t *testing.T, c *Client) (status, cfg []api.Diagnostic) {
	t.Helper()
	st, err := c.Statuses(context.Background())
	fixturekit.Must(t, err)
	cr, err := c.Configuration(context.Background())
	fixturekit.Must(t, err)
	pick := func(ds []api.Diagnostic) []api.Diagnostic {
		var out []api.Diagnostic
		for _, d := range ds {
			if d.Field == "notify" {
				out = append(out, d)
			}
		}
		return out
	}
	return pick(st.Diagnostics), pick(cr.Diagnostics)
}

func (s *Service) workstreamStatus(raw string) (api.WorkstreamStatus, *api.APIError) {
	return service.InspectForTest(s.core()).WorkstreamStatus(raw)
}

func testOptions(c config.Options) Options { return Options{Options: servicetest.Options(c)} }

func (s *Service) runtimeOf(id config.ProjectID) (*config.Config, *activeProject) {
	cfg, ps := s.runtimes()
	for _, p := range ps {
		if p.id == id {
			return cfg, p
		}
	}
	return cfg, nil
}

func fixture(t *testing.T) Options { return Options{Options: servicetest.Fixture(t)} }

type handInFixture struct {
	s       *Service
	c       *Client
	issues  *fixturekit.FakeIssues
	project config.ProjectID
	trace   string
	home    string
}

func (s *Service) statusList() api.StatusResponse {
	return service.InspectForTest(s.core()).StatusList()
}

func (f *architectFixture) handIn(t *testing.T, key, content string) config.WorkstreamID {
	t.Helper()
	return f.scenario().HandIn(t, f.project, key, content)
}

func (s *Service) changeContext(ctx context.Context, raw string) (*trace.Repository, trace.Workgroup, config.WorkstreamID, func(), error) {
	return s.lifecycle().ChangeContext(ctx, raw)
}

func presentAmendment(t *testing.T, f *shedFixture, stream config.WorkstreamID, role, spec, graph string, records ...shed.Record) string {
	t.Helper()
	return intakefixture.PresentAmendment(t, f.repository(), f.project, f.clock.Now(), stream, role, spec, graph, records...)
}

func (s *Service) halt(p *activeProject) error { return service.InspectForTest(s.core()).Halt(p.id) }

func withTailnet(t *testing.T, opts *Options, names ...string) **fixturekit.TailnetNode {
	t.Helper()
	join, joined := fixturekit.TailnetJoiner(names...)
	opts.JoinTailnet = func(hostname, dir string) (service.TailnetListener, error) { return join(hostname, dir) }
	return joined
}

func newDesignFixture(t *testing.T) (*Service, *Client, config.ProjectID) {
	t.Helper()
	f := newHandInFixture(t)
	return f.s, f.c, f.project
}

func newFakeWebhook(t *testing.T) *fakeWebhook { return &fakeWebhook{fixturekit.NewWebhook(t)} }

func (f *shedFixture) revisions(t *testing.T, stream config.WorkstreamID, id string) []int {
	t.Helper()
	return f.scenario().DocumentRevisions(t, stream, id)
}

func (f *shedFixture) ratification(t *testing.T, stream config.WorkstreamID, round int) shed.Ratification {
	t.Helper()
	return f.scenario().Ratification(t, stream, round)
}

func currentChange(t *testing.T, s *Service, address config.WorkstreamID, id string) lifecycle.WorkgroupChange {
	t.Helper()
	wt, g, _, close, err := s.changeContext(context.Background(), string(address))
	fixturekit.Must(t, err)
	defer close()
	changes, err := lifecycle.ReadChanges(wt, g.Members[0])
	fixturekit.Must(t, err)
	for _, c := range changes {
		if c.ID == id {
			return c
		}
	}
	return lifecycle.WorkgroupChange{}
}

func withListen(t *testing.T, opts Options, body string) {
	t.Helper()
	fixturekit.WithListen(t, opts.Config.Root, body)
}

func (f *architectFixture) acknowledgedDraftOperations(t *testing.T, stream config.WorkstreamID) []trace.OperationRecord {
	t.Helper()
	return f.scenario().AcknowledgedDraftOperations(t, stream)
}

var (
	amendedSpec = strings.Replace(fixturekit.ValidSpec, "1. An interrupted upload resumes from the last acknowledged chunk.", "1. An interrupted upload resumes from a durable checkpoint.", 1)
)

func (f *architectFixture) runs() []string {
	return f.scenario().Runs()
}

func (f *crossRepoRatifyFixture) scriptDrafts(t *testing.T) {
	t.Helper()
	intakefixture.ScriptCrossRepoDrafts(t, f.engine, func() (config.WorkstreamID, config.WorkstreamID) { return f.stream1, f.stream2 })
}

func completeClaimedTurn(t *testing.T, f *shedFixture, r *trace.Repository, w config.WorkstreamID, a string, q trace.QueuedTurn, o *coreadapter.Outcome) {
	t.Helper()
	f.sharedTurns().CompleteClaimedTurn(t, r, w, a, q, o)
}
func chiefRecordsAnswer(t *testing.T, f *shedFixture, r *trace.Repository, w config.WorkstreamID, id, text, cite string) {
	t.Helper()
	f.sharedTurns().ChiefRecordsAnswer(t, r, w, id, text, cite)
}

func claimOldestTurn(t *testing.T, f *shedFixture, r *trace.Repository, w config.WorkstreamID, a string) trace.QueuedTurn {
	t.Helper()
	return intakefixture.ClaimOldestTurn(t, f.sharedTurns(), r, w, a)
}

func (s *Service) answers(cfg *config.Config, r *trace.Repository) *questions.Deliverer {
	return s.intake().Answers(cfg, r)
}

func chiefAnswers(t *testing.T, f *shedFixture, r *trace.Repository, w config.WorkstreamID, id, text, cite string) {
	t.Helper()
	chiefRecordsAnswer(t, f, r, w, id, text, cite)
	fixturekit.Must(t, f.s.answers(f.s.cfg, r).Pass(context.Background()))
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

func sealDependent(t *testing.T, f *shedFixture, repo *trace.Repository, backend string, base config.WorkstreamID, files map[string]string) (config.WorkstreamID, string) {
	t.Helper()
	return intakefixture.SealDependent(t, f.s.intake(), repo, backend, base, files)
}
