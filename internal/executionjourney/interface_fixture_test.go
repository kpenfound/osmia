package executionjourney

import (
	context "context"
	config "github.com/kpenfound/osmia/internal/config"
	coreadapter "github.com/kpenfound/osmia/internal/coreadapter"
	delivery "github.com/kpenfound/osmia/internal/delivery"
	lifecycle "github.com/kpenfound/osmia/internal/lifecycle"
	runtime "github.com/kpenfound/osmia/internal/runtime"
	service "github.com/kpenfound/osmia/internal/service"
	servicetest "github.com/kpenfound/osmia/internal/servicetest"
	fixturekit "github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	shed "github.com/kpenfound/osmia/internal/shed"
	trace "github.com/kpenfound/osmia/internal/trace"
	http "net/http"
	testing "testing"
	time "time"
)

// delivered returns what each draft_write the scripted turn made returned.
func (f *architectFixture) delivered(turn string) []draftStatus {
	return f.scenario().Delivered(turn)
}

func (s *Service) saveChange(ctx context.Context, repo *trace.Repository, c *WorkgroupChange) error {
	return s.lifecycle().SaveChange(ctx, repo, c)
}

func draftDelivery(cfg *config.Config, r *trace.Repository, w config.WorkstreamID, report FinalReport) (string, string, error) {
	return delivery.New(delivery.Dependencies{PendingMergeOrderReferences: pendingMergeOrderReferences}).DraftDelivery(cfg, r, w, report)
}

// ruling returns the turns of the requester's thread that deliver the
// ruling on amendment 1.
func (f *shedFixture) ruling(t *testing.T, stream config.WorkstreamID, requester string) []trace.QueuedTurn {
	t.Helper()
	return intakefixture.AmendmentRuling(t, f.repository(), stream, requester)
}

func newLibrarianFixture(t *testing.T) *librarianFixture {
	t.Helper()
	data := fixturekit.NewLibrarianSetup(t)
	opts := testOptions(data.Config)
	opts.Workstreams = nil
	opts.reviewChecks = fixturekit.PassingChecks
	opts.Reconciliation.Now = data.Clock.Now
	opts.Librarian = &Librarian{Engine: data.Engine, Hosts: &coreadapter.MCPHost{Transport: &demoTransport{Sessions: data.Sessions}}}
	return &librarianFixture{opts: opts, clone: data.Clone, engine: data.Engine, sessions: data.Sessions, clock: data.Clock}
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
	inbox.Observe(func() { service.InspectForTest(s.core()).Publish(Event{Kind: EventInbox}) })
	return s, c
}

// fakeWebhook records the bodies posted to it and answers with status. It
// stands in for the owner's real webhook endpoint, leaving that endpoint's
// actual network behaviour and availability unverified.
type fakeWebhook struct{ *fixturekit.Webhook }

func (w *fakeWebhook) hook() string { return w.Hook() }

func (w *fakeWebhook) accepted() []string { return w.Accepted() }

func (w *fakeWebhook) received() []string { return w.Received() }

func (w *fakeWebhook) respond(status int) { w.Respond(status) }

func (w *fakeWebhook) await(t *testing.T, n int) []string {
	t.Helper()
	return intakefixture.AwaitWebhook(t, w.Webhook, n)
}

// eventTraceOptions plants a workstream with one escalated question in the
// trace and returns the options of a service that has not started.
func eventTraceOptions(t *testing.T) Options {
	t.Helper()
	opts, cfg := conversationFixture(t, "evs-")
	fixturekit.SeedEventTrace(t, cfg)
	return opts
}

func (s *Service) requestChange(ctx context.Context, raw string, req ChangeRequest) (WorkgroupChange, *APIError) {
	return s.lifecycle().RequestChange(ctx, raw, req)
}

type fakeInbox struct{ fixturekit.NotificationInbox }

func (f *fakeInbox) read(ctx context.Context) (InboxResponse, *APIError) { return f.Read(ctx) }

func (f *fakeInbox) set(entries ...InboxEntry) { f.Set(entries...) }

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

// setUpBusyReadsService starts a service whose only active project has
// stream already created, wired so a RepositoryBoundary operation reaches
// blocking instead of a real sealer, builder or foreman.
func setUpBusyReadsService(t *testing.T, blocking coreadapter.Reconciler) (*Service, *Client, *trace.Repository) {
	t.Helper()
	opts := testOptions(fixturekit.BusyReadsConfig(t))
	opts.Reconciliation.Adapters = map[coreadapter.OperationBoundary]coreadapter.Reconciler{coreadapter.RepositoryBoundary: blocking}
	s, c := start(t, opts)
	_, active := s.runtimeOf(project)
	if active == nil {
		t.Fatal("no active project")
	}
	return s, c, active.repository
}

func readLedger(t *testing.T, opts Options) notifyLedger {
	t.Helper()
	return fixturekit.ReadNotificationLedger[notifyLedger](t, opts.Config.Root)
}

// builtForAmendment returns a fixture with a building workstream on seal 1,
// a committee of one and the given shed.max_rounds.
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

// newHandInFixture starts a service with an active project whose charter has
// rules.
func newHandInFixture(t *testing.T) *handInFixture {
	t.Helper()
	opts, clone := projectFixture(t)
	f := &handInFixture{}
	data := intakefixture.NewHandInSetup(t, clone, func(fake *fakeIssues) intakefixture.ArchitectProjectClient {
		opts.Issues = fake
		f.s, f.c = start(t, opts)
		return f.c
	}, func(t *testing.T, project config.ProjectID, text string) { writeCharter(t, f.c, project, text) })
	f.issues, f.project, f.trace, f.home = data.Issues, data.Project, data.Trace, data.Home
	return f
}

// librarianFixture is a project root, a clone with tracked and untracked files
// and the fake engine the librarian's turns run in.
type librarianFixture struct {
	opts     Options
	clone    string
	engine   *demoEngine
	sessions *demoSessions
	clock    *demoClock
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
	must(t, err)
	c := NewClient(s.Socket())
	t.Cleanup(func() { c.Close(); must(t, s.Close()) })
	return s, c
}

func (w *fakeWebhook) records(opts Options) *fakeWebhook {
	w.Webhook.Records(opts.Config.Root)
	return w
}

// awaitExtraction polls status until the latest extraction is terminal.
func awaitExtraction(t *testing.T, c *Client) ExtractionState {
	t.Helper()
	return intakefixture.AwaitExtraction(t, c.Configuration)
}

func (s *Service) deltaTool(wt *trace.Repository, c WorkgroupChange, agent, name string) coreadapter.Tool {
	return s.lifecycle().DeltaTool(wt, c, agent, name)
}

// ownerMoves lists the states the workstream's owner subject went through.
func (f *shedFixture) ownerMoves(t *testing.T, stream config.WorkstreamID) []string {
	t.Helper()
	return intakefixture.OwnerMoves(t, f.transitions(t, stream))
}

// eventTraceFixture is a service whose trace holds stream and quiet, with
// quiet's question escalated to the owner as inbox entry 1.
func eventTraceFixture(t *testing.T) (*Service, *Client) {
	t.Helper()
	return start(t, eventTraceOptions(t))
}

// awaitAmendment waits until amendment 1 reaches the wanted state.
func (f *shedFixture) awaitAmendment(t *testing.T, stream config.WorkstreamID, want string) {
	t.Helper()
	intakefixture.AwaitAmendment(t, f.repository(), stream, want)
}

func notifyDiagnostics(t *testing.T, c *Client) (status, cfg []Diagnostic) {
	t.Helper()
	st, err := c.Statuses(context.Background())
	must(t, err)
	cr, err := c.Configuration(context.Background())
	must(t, err)
	pick := func(ds []Diagnostic) []Diagnostic {
		var out []Diagnostic
		for _, d := range ds {
			if d.Field == "notify" {
				out = append(out, d)
			}
		}
		return out
	}
	return pick(st.Diagnostics), pick(cr.Diagnostics)
}

var committedPaths = fixturekit.CommittedPaths

func (s *Service) workstreamStatus(raw string) (WorkstreamStatus, *APIError) {
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
	issues  *fakeIssues
	project config.ProjectID
	trace   string
	home    string
}

var entryOf = fixturekit.NotificationEntry

func (s *Service) statusList() StatusResponse { return service.InspectForTest(s.core()).StatusList() }

func (f *architectFixture) handIn(t *testing.T, key, content string) config.WorkstreamID {
	t.Helper()
	return f.scenario().HandIn(t, f.project, key, content)
}

func (s *Service) changeContext(ctx context.Context, raw string) (*trace.Repository, trace.Workgroup, config.WorkstreamID, func(), error) {
	return s.lifecycle().ChangeContext(ctx, raw)
}

// presentAmendment plants amendment 1 as the chief of staff presents it: the
// requester of the given role asked for it from its turn filing on unit
// resume, which waits for it, the architect proposed spec and graph, the
// given committee records make its first round, and packet.json revision 1
// is presented. It returns the requester's agent.
func presentAmendment(t *testing.T, f *shedFixture, stream config.WorkstreamID, role, spec, graph string, records ...shed.Record) string {
	t.Helper()
	return intakefixture.PresentAmendment(t, f.repository(), f.project, f.clock.Now(), stream, role, spec, graph, records...)
}

func (s *Service) halt(p *activeProject) error { return service.InspectForTest(s.core()).Halt(p.id) }

// withTailnet sets opts to join a fake tailnet that reports names, and
// returns a pointer to the node the service joins.
func withTailnet(t *testing.T, opts *Options, names ...string) **fakeTailnet {
	t.Helper()
	join, joined := fixturekit.TailnetJoiner(names...)
	opts.JoinTailnet = func(hostname, dir string) (TailnetListener, error) { return join(hostname, dir) }
	return joined
}

// newDesignFixture starts a service with an active project whose charter
// has rules and whose clone has one commit, the same shape newHandInFixture
// gives an ordinary hand-in.
func newDesignFixture(t *testing.T) (*Service, *Client, config.ProjectID) {
	t.Helper()
	f := newHandInFixture(t)
	return f.s, f.c, f.project
}

var feedDocument = fixturekit.FeedDocument

func newFakeWebhook(t *testing.T) *fakeWebhook { return &fakeWebhook{fixturekit.NewWebhook(t)} }

// revisions lists the recorded revisions of one document.
func (f *shedFixture) revisions(t *testing.T, stream config.WorkstreamID, id string) []int {
	t.Helper()
	return f.scenario().DocumentRevisions(t, stream, id)
}

func (f *shedFixture) ratification(t *testing.T, stream config.WorkstreamID, round int) shed.Ratification {
	t.Helper()
	return f.scenario().Ratification(t, stream, round)
}

func currentChange(t *testing.T, s *Service, address config.WorkstreamID, id string) WorkgroupChange {
	t.Helper()
	wt, g, _, close, err := s.changeContext(context.Background(), string(address))
	must(t, err)
	defer close()
	changes, err := readChanges(wt, g.Members[0])
	must(t, err)
	for _, c := range changes {
		if c.ID == id {
			return c
		}
	}
	return WorkgroupChange{}
}

// withListen appends a [listen] table with the given body to the fixture's
// top-level file.
func withListen(t *testing.T, opts Options, body string) {
	t.Helper()
	fixturekit.WithListen(t, opts.Config.Root, body)
}

// acknowledgedDraftOperations returns the draft operations once every one of
// them is acknowledged, for assertions reached through an awaited state.
func (f *architectFixture) acknowledgedDraftOperations(t *testing.T, stream config.WorkstreamID) []trace.OperationRecord {
	t.Helper()
	return f.scenario().AcknowledgedDraftOperations(t, stream)
}
