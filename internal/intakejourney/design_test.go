package intakejourney

import (
	context "context"
	json "encoding/json"
	errors "errors"
	fmt "fmt"
	maps "maps"
	os "os"
	filepath "path/filepath"
	slices "slices"
	strings "strings"
	sync "sync"
	testing "testing"
	time "time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	agent "github.com/kpenfound/agentcore/agent"
	config "github.com/kpenfound/osmia/internal/config"
	coreadapter "github.com/kpenfound/osmia/internal/coreadapter"
	intake "github.com/kpenfound/osmia/internal/intake"
	isolation "github.com/kpenfound/osmia/internal/isolation"
	kb "github.com/kpenfound/osmia/internal/kb"
	questions "github.com/kpenfound/osmia/internal/questions"
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	status "github.com/kpenfound/osmia/internal/status"
	thread "github.com/kpenfound/osmia/internal/thread"
	trace "github.com/kpenfound/osmia/internal/trace"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func awaitDesignConversation(t *testing.T, c *journey.Client, id config.WorkstreamID, done func(api.ConversationResponse) bool) api.ConversationResponse {
	t.Helper()
	return intakefixture.AwaitView(t, func() (api.ConversationResponse, error) {
		return c.DesignConversation(context.Background(), id)
	}, done, "design conversation did not settle: %+v")
}

func TestOpenDesignCreatesAnInertWorkstreamInItsOwnWorkgroup(t *testing.T) {
	t.Parallel()
	s, c, project := journey.NewDesignFixture(t)
	opened, err := c.OpenDesign(context.Background(), project)
	fixturekit.Must(t, err)
	stream := opened.Design.Workstream
	if opened.Design.Project != project {
		t.Fatalf("design view: %+v", opened.Design)
	}

	repository := s.Sole().Repository
	design, err := intake.IsDesignWorkstream(repository, stream)
	fixturekit.Must(t, err)
	if !design {
		t.Fatal("a freshly opened design session should be a design workstream")
	}
	if kind, err := repository.Kind(stream); err != nil || kind != "" {
		t.Fatalf("design session kind: %q %v", kind, err)
	}
	if state, err := repository.Workflow(stream, trace.FeatureSubject); err != nil || state.Value != "" {
		t.Fatalf("design session feature state: %+v %v", state, err)
	}
	if _, err := repository.ChiefOfStaffThread(stream); err != nil {
		t.Fatalf("design session has no chief-of-staff thread: %v", err)
	}
	groups, _, err := trace.Workgroups(repository)
	fixturekit.Must(t, err)
	g, ok := trace.WorkgroupOf(groups, stream)
	if !ok || len(g.Members) != 1 || g.Members[0] != stream {
		t.Fatalf("workgroup of design session %s: %+v ok=%v", stream, g, ok)
	}
	host, err := repository.ChiefOfStaffHost(stream)
	fixturekit.Must(t, err)
	if host != stream {
		t.Fatalf("a one-workstream workgroup's chief of staff should be its own thread, got host %s", host)
	}

	byID, err := c.Design(context.Background(), stream)
	fixturekit.Must(t, err)
	if byID.Design.Workstream != stream {
		t.Fatalf("design read: %+v", byID.Design)
	}
	all, err := c.Designs(context.Background(), "")
	fixturekit.Must(t, err)
	if !slices.ContainsFunc(all.Designs, func(d api.DesignView) bool { return d.Workstream == stream }) {
		t.Fatalf("design list does not include %s: %+v", stream, all.Designs)
	}
	status, err := c.Status(context.Background(), stream)
	fixturekit.Must(t, err)
	if status.Kind != "design" {
		t.Fatalf("workstream status kind: %+v", status)
	}
}

func TestExploratorySessionsAreNotDesignSessions(t *testing.T) {
	t.Parallel()
	s, c, project := journey.NewDesignFixture(t)
	repository := s.Sole().Repository
	stream, err := config.NewWorkstreamID()
	fixturekit.Must(t, err)
	now := time.Now().UTC()
	fixturekit.Must(t, repository.CreateSessionWorkstream(context.Background(), stream, "default", "deadbeef", "osmia/session/"+string(stream), "", now, intake.OwnerActor))

	design, err := intake.IsDesignWorkstream(repository, stream)
	fixturekit.Must(t, err)
	if design {
		t.Fatal("a session workstream must never be treated as a design session")
	}
	all, err := c.Designs(context.Background(), project)
	fixturekit.Must(t, err)
	if slices.ContainsFunc(all.Designs, func(d api.DesignView) bool { return d.Workstream == stream }) {
		t.Fatalf("design list wrongly includes the session %s: %+v", stream, all.Designs)
	}
	if _, _, _, apiErr := s.DesignTrace(string(stream)); apiErr == nil {
		t.Fatal("a session workstream must be refused as a design session")
	}
}

func TestOrdinaryConversationRefusesADesignSessionAndDesignMessageRefusesAStartedOne(t *testing.T) {
	t.Parallel()
	s, c, project := journey.NewDesignFixture(t)
	opened, err := c.OpenDesign(context.Background(), project)
	fixturekit.Must(t, err)
	stream := opened.Design.Workstream

	if _, err := c.Send(context.Background(), stream, "Hello"); err == nil {
		t.Fatal("the ordinary conversation endpoint must refuse a design session")
	}

	repository := s.Sole().Repository
	fixturekit.Must(t, intakefixture.RecordDesignDraft(t, repository, stream, "Decisions: ship it.\nIntent and tradeoffs: keep it simple.\nRejected options: none.\n"))
	if _, err := s.StartDesignWorkgroup(context.Background(), string(stream)); err != nil {
		t.Fatalf("start: %+v", err)
	}
	if state, err := repository.Workflow(stream, trace.FeatureSubject); err != nil || state.Value != intake.HandedState {
		t.Fatalf("feature state after start: %+v %v", state, err)
	}
	if _, err := c.DesignMessage(context.Background(), stream, "Still there?"); err == nil {
		t.Fatal("the design message endpoint must refuse a workstream that has already started a workgroup")
	}
}

func TestStartDesignWorkgroupHandsOffBriefOnTheSameChiefOfStaffThread(t *testing.T) {
	t.Parallel()
	s, c, project := journey.NewDesignFixture(t)
	opened, err := c.OpenDesign(context.Background(), project)
	fixturekit.Must(t, err)
	stream := opened.Design.Workstream
	repository := s.Sole().Repository

	if _, apiErr := s.StartDesignWorkgroup(context.Background(), string(stream)); apiErr == nil {
		t.Fatal("starting a workgroup before any brief is drafted must be refused")
	}

	before, err := repository.ChiefOfStaffThread(stream)
	fixturekit.Must(t, err)

	fixturekit.Must(t, intakefixture.RecordDesignDraft(t, repository, stream, "Decisions: ship the upload feature.\nIntent and tradeoffs: keep the API stable.\nRejected options: a new endpoint was rejected.\n"))

	fixturekit.Must(t, intakefixture.RecordDesignDraftRevision(t, repository, stream, 2, "Decisions: ship resumable uploads.\nIntent and tradeoffs: favor simplicity over flexibility.\nRejected options: a separate service was rejected as overkill.\n"))

	out, err := c.DesignStart(context.Background(), stream)
	fixturekit.Must(t, err)
	if out.Workstream != stream || out.State != intake.HandedState || out.Handed != intake.DesignBriefPath {
		t.Fatalf("start response: %+v", out)
	}

	docs, err := trace.Read[trace.Document](repository, stream)
	fixturekit.Must(t, err)
	var handed trace.Document
	for _, d := range docs {
		if d.ID == intake.HandedDocument {
			handed = d
		}
	}
	if handed.Revision == 0 {
		t.Fatal("no handed document was recorded")
	}
	if !strings.Contains(handed.Content, "resumable uploads") || strings.Contains(handed.Content, "upload feature") {
		t.Fatalf("handed content should carry the latest draft only: %q", handed.Content)
	}
	if !strings.Contains(handed.Content, "## Transcript") {
		t.Fatalf("handed content has no transcript: %q", handed.Content)
	}

	after, err := repository.ChiefOfStaffThread(stream)
	fixturekit.Must(t, err)
	if after.Identity.ID != before.Identity.ID || after.Identity.ThreadID != before.Identity.ThreadID {
		t.Fatalf("starting the workgroup must continue the same thread: before %+v after %+v", before.Identity, after.Identity)
	}
	host, err := repository.ChiefOfStaffHost(stream)
	fixturekit.Must(t, err)
	if host != stream {
		t.Fatalf("the started workstream's chief of staff should still be its own thread, got host %s", host)
	}

	latest := map[string]trace.Document{}
	for _, d := range docs {
		if prior, ok := latest[d.ID]; !ok || d.Revision > prior.Revision {
			latest[d.ID] = d
		}
	}
	if _, ok := latest[intake.HandedDocument]; !ok {
		t.Fatal("the architect's handed() lookup would not find this input")
	}

	if _, apiErr := s.StartDesignWorkgroup(context.Background(), string(stream)); apiErr == nil {
		t.Fatal("starting a workgroup twice must be refused")
	}
}

func TestDesignWorkgroupStartRecoversAfterRestartWithoutRewritingBrief(t *testing.T) {
	t.Parallel()
	s, c, project := journey.NewDesignFixture(t)
	ctx := context.Background()
	opened, err := c.OpenDesign(ctx, project)
	fixturekit.Must(t, err)
	stream := opened.Design.Workstream
	repository := s.Sole().Repository
	fixturekit.Must(t, intakefixture.RecordDesignDraft(t, repository, stream, "Decisions: preserve the API.\nIntent and tradeoffs: compatibility over flexibility.\nRejected options: breaking changes."))
	before, err := repository.ChiefOfStaffThread(stream)
	fixturekit.Must(t, err)
	s.Boundary = func(name string) error {
		if name == "design-brief-handed" {
			return errors.New("interrupted before feature transition")
		}
		return nil
	}
	if _, err := c.DesignStart(ctx, stream); err == nil {
		t.Fatal("expected the injected interruption")
	}
	if _, err := c.DesignMessage(ctx, stream, "Revise the brief"); !intakefixture.Failed(err, api.Conflict) {
		t.Fatalf("pending handoff allowed another turn: %v", err)
	}
	options := s.Options
	fixturekit.Must(t, s.Close())
	restarted, client := journey.StartT(t, options)
	result, err := client.DesignStart(ctx, stream)
	if err != nil && !intakefixture.Failed(err, api.Conflict) && !intakefixture.Failed(err, api.NotFound) {
		t.Fatal(err)
	}
	if err == nil && result.State != intake.HandedState {
		t.Fatalf("start after restart: %+v", result)
	}
	repository = restarted.Sole().Repository
	restarted.Until(t, "retry or recovery to complete the handoff", func() bool {
		state, err := repository.Workflow(stream, trace.FeatureSubject)
		fixturekit.Must(t, err)
		return state.Value != ""
	})
	after, err := repository.ChiefOfStaffThread(stream)
	fixturekit.Must(t, err)
	if before.Identity != after.Identity {
		t.Fatal("handoff recovery replaced the chief thread")
	}
	documents, err := trace.Read[trace.Document](repository, stream)
	fixturekit.Must(t, err)
	count := 0
	for _, document := range documents {
		if document.ID == intake.HandedDocument {
			count++
			if document.Revision != 1 || !strings.Contains(document.Content, "preserve the API") {
				t.Fatalf("handed brief changed: %+v", document)
			}
		}
	}
	if count != 1 {
		t.Fatalf("handed brief recorded %d times", count)
	}
}

func newDesignEnforcementFixture(t *testing.T, web ...bool) (*journey.Service, *journey.Client, config.ProjectID, *fixturekit.DemoEngine) {
	t.Helper()
	before := ""
	if len(web) > 0 && web[0] {
		before = servicetest.ListenWeb
	}
	return newDesignEnforcementFixtureWith(t, before, "")
}

func newDesignEnforcementFixtureWith(t *testing.T, before, after string) (*journey.Service, *journey.Client, config.ProjectID, *fixturekit.DemoEngine) {
	t.Helper()
	d := servicetest.PrepareDesignEnforcement(t, before, after)
	s, c := journey.StartT(t, journey.Enforce(journey.Options{Options: d.Options}, d.Enforcement))
	return s, c, servicetest.AddDesignProject(t, c.Client, d.Clone), d.Engine
}

func TestDesignSessionProductionEnforcementGrantsOnlyReadOnlyTools(t *testing.T) {
	t.Parallel()
	s, c, project, engine := newDesignEnforcementFixture(t)
	clone := filepath.Join(t.TempDir(), "second-repo")
	fixturekit.DemoGit(t, filepath.Dir(clone), "init", "--quiet", clone)
	fixturekit.Must(t, os.WriteFile(filepath.Join(clone, "README.md"), []byte("second repo source\n"), 0600))
	fixturekit.DemoGit(t, filepath.Dir(clone), "-C", clone, "add", "README.md")
	fixturekit.DemoGit(t, filepath.Dir(clone), "-C", clone, "-c", "user.name=Owner", "-c", "user.email=owner@example.invalid", "commit", "-qm", "base")
	added, err := c.AddProject(context.Background(), api.ProjectAddRequest{Name: "client", Upstream: "owner/second", Clone: clone})
	fixturekit.Must(t, err)
	var second config.ProjectID
	for _, repo := range added.Project.Repos {
		if repo.ID != project {
			second = repo.ID
		}
	}
	if second == "" {
		t.Fatal("project creation did not register the second project")
	}
	intakefixture.WriteCharter(t, c, second, "1. Keep the client API stable.\n")
	fixturekit.Must(t, os.WriteFile(filepath.Join(clone, "README.md"), []byte("uncommitted source\n"), 0600))
	for _, repoID := range []config.ProjectID{project, second} {
		repository, err := s.Repository(repoID)
		fixturekit.Must(t, err)
		m, err := kb.Load(repository)
		fixturekit.Must(t, err)
		m.Entities = append(m.Entities, kb.Entity{ID: "conventions", Name: "Conventions for repo " + string(repoID), Aliases: []string{}, Paths: []string{"conventions/**"}, Owners: []string{}, PartOf: []string{}})
		fixturekit.Must(t, kb.Store(context.Background(), repository, m, s.Now(), intake.OwnerActor, "fixture"))
	}
	opened, err := c.OpenDesign(context.Background(), project)
	fixturekit.Must(t, err)
	stream := opened.Design.Workstream

	var problems []string
	fail := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	engine.Turns["*"] = func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		if !strings.Contains(req.SystemPrompt, "1. Keep changes small.") {
			fail("missing project charter in design context")
		}
		for _, p := range s.Current().Projects {
			row := fmt.Sprintf("%s | %s | %s | %t |", p.ID, p.Name, p.Upstream, p.ID == project)
			if !strings.Contains(req.SystemPrompt, row) {
				fail("missing catalog row %s", row)
			}
		}
		if strings.Contains(req.SystemPrompt, "Keep the client API stable") {
			fail("non-brief charter was inlined")
		}
		if strings.Contains(req.SystemPrompt, "Conventions for repo "+string(second)) {
			fail("non-brief entity map was inlined")
		}
		for _, name := range []string{"charter.md", "entities.md"} {
			out, err := intakefixture.ReadTool(ctx, tools, "projects/"+string(second)+"/"+name)
			if err != nil || out == "" {
				fail("non-brief reference %s unreadable: %s %v", name, out, err)
			}
			if name == "charter.md" && !strings.Contains(out, "Keep the client API stable") {
				fail("reference charter missing content: %s", out)
			}
			if name == "entities.md" && !strings.Contains(out, "Conventions for repo "+string(second)) {
				fail("reference entity map missing content: %s", out)
			}
		}
		for _, repo := range []config.ProjectID{project, second} {
			if repo == project && !strings.Contains(req.SystemPrompt, "Conventions for repo "+string(repo)) {
				fail("missing repo entity map in context: %s", repo)
			}
			out, err := fixturekit.CallTool(ctx, tools, intake.DesignReadRepoToolName, map[string]any{"repo": repo, "path": "README.md"})
			if err != nil {
				fail("read repo %s: %s %v", repo, out, err)
				continue
			}
			var read struct {
				Repo    config.ProjectID `json:"repo"`
				Content string           `json:"content"`
				Commit  string           `json:"commit"`
			}
			if err := json.Unmarshal([]byte(out), &read); err != nil {
				fail("decode repo %s read: %s %v", repo, out, err)
				continue
			}
			want := "hi\n"
			if repo == second {
				want = "second repo source\n"
			}
			if read.Repo != repo || read.Content != want || read.Commit == "" {
				fail("read repo %s returned %+v, want that repo's committed content %q", repo, read, want)
			}
		}
		out, err := fixturekit.CallTool(ctx, tools, intake.DesignReadRepoToolName, map[string]any{"repo": "p_00000000000000000000000000000099", "path": "README.md"})
		if err == nil || !strings.Contains(err.Error(), "not an active project") {
			fail("read outside active projects was not refused: %s %v", out, err)
		}
		names, err := intakefixture.ToolNames(ctx, tools)
		if err != nil {
			return nil, err
		}
		if want := []string{"draft_brief", "file_read", "read_repo", "set_title"}; !slices.Equal(names, want) {
			fail("design session's MCP tools %v, want %v", names, want)
		}
		g := req.Grants
		if g == nil {
			fail("no grants recorded")
			return &agent.Result{ClaudeID: "design-session-1", ResultText: "ok"}, nil
		}
		for _, name := range g.Tools {
			if name == "Write" || name == "Edit" || name == "Bash" || name == "apply_patch" || name == "shell" {
				fail("design session granted the write or execute native tool %q", name)
			}
		}
		if want := []string{"Read", "Glob", "Grep", "WebSearch", "WebFetch", "mcp__osmia_0"}; !slices.Equal(g.Tools, want) {
			fail("design session's native tools %v, want %v", g.Tools, want)
		}
		if !strings.Contains(req.SystemPrompt, isolation.WebGuidance) || !strings.HasPrefix(req.SystemPrompt, intake.DesignRolePrompt) {
			fail("design session's system prompt lacks the web guidance after its role prompt")
		}
		for _, m := range g.Mounts {

			if m.Access != agent.ReadOnly && filepath.Base(m.Path) != "work" {
				fail("design session's mount %+v is not read-only", m)
			}
		}
		if g.VCS || verified.VCS {
			fail("design session is granted version control")
		}
		if !slices.Equal(verified.DeniedExecutables, agent.VCSExecutables) {
			fail("design session's denied executables: %+v", verified.DeniedExecutables)
		}

		if req.VCSEnv != nil || len(req.VCSContainerEnv) != 0 {
			fail("design session carries VCS credentials")
		}
		return &agent.Result{ClaudeID: "design-session-1", ResultText: "Here is the readiness pass."}, nil
	}
	_, err = c.DesignMessage(context.Background(), stream, "What should we build?")
	fixturekit.Must(t, err)
	awaitDesignConversation(t, c, stream, func(r api.ConversationResponse) bool {
		for _, e := range r.Entries {
			if e.Kind == "response" {
				return true
			}
		}
		return false
	})
	if len(problems) > 0 {
		t.Fatalf("production enforcement problems:\n%s", strings.Join(problems, "\n"))
	}
}

func TestDraftBriefRefusesAnIncompleteBriefAndRecordsTheLatest(t *testing.T) {
	t.Parallel()
	s, c, project, engine := newDesignEnforcementFixture(t)
	opened, err := c.OpenDesign(context.Background(), project)
	fixturekit.Must(t, err)
	stream := opened.Design.Workstream

	var problems []string
	fail := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	engine.Turns["*"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		out, err := fixturekit.CallTool(ctx, tools, intake.DraftBriefToolName, map[string]any{"content": "We will build the thing."})
		if err != nil {
			fail("incomplete brief call failed outright: %v", err)
		} else if !strings.Contains(out, `"recorded":false`) {
			fail("an incomplete brief was recorded: %s", out)
		}
		out, err = fixturekit.CallTool(ctx, tools, intake.DraftBriefToolName, map[string]any{"content": "Decisions: ship. Intent: compatibility. Rejected options: rewrite."})
		if err != nil || !strings.Contains(out, `"recorded":false`) {
			fail("a brief without tradeoffs was accepted: %s %v", out, err)
		}
		out, err = fixturekit.CallTool(ctx, tools, intake.DraftBriefToolName, map[string]any{"content": "Decisions: ship it.\nIntent and tradeoffs: keep it simple.\nRejected options: a rewrite was rejected.\n"})
		if err != nil || !strings.Contains(out, `"recorded":true`) {
			fail("a complete brief was refused: %s %v", out, err)
		}
		return &agent.Result{ClaudeID: "design-session-1", ResultText: "Drafted the brief."}, nil
	}
	_, err = c.DesignMessage(context.Background(), stream, "I'm ready for the brief.")
	fixturekit.Must(t, err)
	awaitDesignConversation(t, c, stream, func(r api.ConversationResponse) bool {
		for _, e := range r.Entries {
			if e.Kind == "response" {
				return true
			}
		}
		return false
	})
	if len(problems) > 0 {
		t.Fatalf("draft_brief problems:\n%s", strings.Join(problems, "\n"))
	}
	docs, err := trace.Read[trace.Document](s.Sole().Repository, stream)
	fixturekit.Must(t, err)
	var draft trace.Document
	for _, d := range docs {
		if d.ID == intake.DesignDraftDocument && d.Revision >= draft.Revision {
			draft = d
		}
	}
	if draft.Revision != 1 || !strings.Contains(draft.Content, "Decisions") {
		t.Fatalf("recorded draft: %+v", draft)
	}
}

const designWorkerRole, designWorkerAgent, designWorkerThread = "mason", "agent_design_worker", "thread_design_worker"

func newDesignWorkgroupContinuationFixture(t *testing.T) (*journey.Service, *journey.Client, config.ProjectID, *fixturekit.DemoEngine, *fixturekit.FixedClock, chan time.Time) {
	t.Helper()
	d := servicetest.PrepareDesignEnforcement(t, "", "")
	opts, engine := journey.Options{Options: d.Options}, d.Engine
	clock := &fixturekit.FixedClock{At: fixturekit.DemoStart}
	opts.Reconciliation.Now = clock.Now
	ticks := make(chan time.Time)
	opts.Reconciliation.Ticks = ticks

	enforced := journey.Enforce(opts, d.Enforcement)
	opts.Controls = enforced.Controls
	production := enforced.Threads
	opts.Threads = func(r *trace.Repository, cfg *config.Config) (coreadapter.Reconciler, error) {
		bound, err := production(r, cfg)
		if err != nil {
			return nil, err
		}
		turns := bound.(thread.Dispatcher).Runner.Turns.(*questions.Turns).Turns.(*execution.VerdictTurns).Turns.(*execution.ReportingTurns).Turns.(*execution.RetriedTurns).Turns.(*isolation.Turns)
		turns.Grants = maps.Clone(turns.Grants)
		turns.Grants[designWorkerRole] = coreadapter.Capabilities{Tools: []string{questions.AskTool}}

		turns.Workspaces, turns.Capture = intake.StagedWorkspaces{}, nil
		selectChief := turns.Select
		turns.Select = func(ctx context.Context, scope coreadapter.Scope) (isolation.Selection, error) {
			if scope.Role == trace.ChiefOfStaff {
				return selectChief(ctx, scope)
			}
			workspace := filepath.Join(cfg.Root.String(), "workers", scope.Thread)
			return isolation.Selection{Workspace: coreadapter.WorkspaceRequest{SourceDirectory: workspace, Directory: workspace},
				Execution: coreadapter.ExecutionSettings{Mode: "container", Image: "fixture-image"}}, os.MkdirAll(workspace, 0700)
		}
		chiefScoped := turns.Scoped
		turns.Scoped = func(ctx context.Context, scope coreadapter.Scope) ([]coreadapter.Tool, error) {
			if scope.Role == trace.ChiefOfStaff {
				return chiefScoped(ctx, scope)
			}
			return questions.Tools(r, designWorkerAgent, scope, clock.Now)
		}
		return bound, nil
	}
	s, c := journey.StartT(t, opts)
	added, err := c.AddProject(context.Background(), fixturekit.Request(d.Clone))
	fixturekit.Must(t, err)
	intakefixture.WriteCharter(t, c, added.Project.ID, "1. Keep state in files under the root.\n")
	return s, c, added.Project.ID, engine, clock, ticks
}

func TestDesignWorkgroupThreadCarriesALaterStatusAndEscalation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, c, project, engine, clock, ticks := newDesignWorkgroupContinuationFixture(t)
	settle := func() {
		t.Helper()
		for range 5 {
			select {
			case ticks <- clock.Now():
			case <-time.After(fixturekit.DemoTimeout):
				s.Close()
				t.Fatal("service did not finish its pass")
			}
		}
	}

	opened, err := c.OpenDesign(ctx, project)
	fixturekit.Must(t, err)
	stream := opened.Design.Workstream
	repository := s.Sole().Repository
	var mu sync.Mutex
	var problems []string
	var acted bool
	problem := func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		problems = append(problems, fmt.Sprintf(format, args...))
	}
	fatalf := func(format string, args ...any) {
		t.Helper()
		mu.Lock()
		collected := strings.Join(problems, "; ")
		mu.Unlock()
		t.Fatalf("%s; tool problems: [%s]", fmt.Sprintf(format, args...), collected)
	}
	turnSummary := func(th trace.Thread) string {
		var turns []string
		for _, q := range th.Turns {
			response := "none"
			failure := ""
			if q.Response != nil {
				response = q.Response.ID
				failure = q.Response.Failure
			}
			turns = append(turns, fmt.Sprintf("%s(request=%s response=%s status=%s completed=%s attempts=%d failure=%q)", q.Request.TurnID, q.Request.ID, response, q.Status(), q.CompletedAt.Format(time.RFC3339Nano), len(q.Attempts), failure))
		}
		return fmt.Sprintf("%s status=%s active=%q turns=[%s]", th.Identity.ThreadID, th.Status, th.Active, strings.Join(turns, ", "))
	}

	waitCompleted := func(label string, chiefCount, workerCount int, workerStatus string) {
		t.Helper()
		deadline := time.Now().Add(fixturekit.DemoTimeout)
		for {
			chief, err := repository.ChiefOfStaffThread(stream)
			fixturekit.Must(t, err)
			complete := func(th trace.Thread) bool {
				return th.Active == "" && !slices.ContainsFunc(th.Turns, func(q trace.QueuedTurn) bool {
					return q.CompletedAt.IsZero() || q.Response == nil || (q.Status() != "idle" && q.Status() != "waiting")
				})
			}
			ready := len(chief.Turns) >= chiefCount && complete(chief)
			var worker trace.Thread
			if workerCount > 0 {
				worker, err = repository.Thread(stream, designWorkerAgent)
				fixturekit.Must(t, err)
				ready = ready && len(worker.Turns) == workerCount && complete(worker) && worker.Status == workerStatus
			}
			outbox, err := repository.Outbox(stream)
			fixturekit.Must(t, err)
			var pending []string
			for _, e := range outbox {
				window := s.Current().EventWindow()
				if e.Event.Kind == trace.ProgressKind {
					window = s.Current().EventProgressWindow()
				}
				if e.Event.Operation == nil && !e.Acknowledged && (e.Claim != nil || !clock.Now().Before(e.At.Add(window))) {
					pending = append(pending, e.Event.ID)
				}
			}
			if ready && len(pending) == 0 {
				t.Logf("%s completed: chief %s; worker %s", label, turnSummary(chief), turnSummary(worker))
				return
			}
			if time.Now().After(deadline) {
				mu.Lock()
				toolProblems := strings.Join(problems, "; ")
				mu.Unlock()
				t.Fatalf("%s: want completed chief >=%d and worker %d/%s; actual chief %s; worker %s; pending events %v; tool problems %s", label, chiefCount, workerCount, workerStatus, turnSummary(chief), turnSummary(worker), pending, toolProblems)
			}
			select {
			case ticks <- clock.Now():
			case <-time.After(20 * time.Millisecond):
			}
		}
	}

	const designMessage = "Keep the module name stable."
	const designReply = "Decisions: retain the module name.\nIntent and tradeoffs: compatibility over flexibility.\nRejected options: renaming the module.\n"
	engine.Mu.Lock()
	engine.Turns["*"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		if out, err := fixturekit.CallTool(ctx, tools, intake.DraftBriefToolName, map[string]any{"content": designReply}); err != nil || !strings.Contains(out, `"recorded":true`) {
			return nil, fmt.Errorf("draft brief: %s %v", out, err)
		}
		return intakefixture.QuestionResult(req, "session-design", designReply), nil
	}
	engine.Mu.Unlock()
	sent, err := c.DesignMessage(ctx, stream, designMessage)
	fixturekit.Must(t, err)
	waitCompleted("design reply", 1, 0, "")
	designConversation := awaitDesignConversation(t, c, stream, func(r api.ConversationResponse) bool {
		return len(r.Entries) == 2 && r.Entries[1].Kind == "response"
	})
	if designConversation.Entries[0].Text != designMessage || designConversation.Entries[1].Text != designReply || designConversation.Entries[1].State != api.TurnDone {
		t.Fatalf("design conversation: %+v", designConversation.Entries)
	}
	before, err := repository.ChiefOfStaffThread(stream)
	fixturekit.Must(t, err)
	if len(before.Turns) != 1 || before.Turns[0].Request.TurnID != sent.Turn || before.Turns[0].Response == nil || before.Turns[0].CompletedAt.IsZero() {
		t.Fatalf("design exchange did not complete on its actual thread: %+v", before)
	}

	started, err := c.DesignStart(ctx, stream)
	fixturekit.Must(t, err)
	if started.Workstream != stream || started.State != intake.HandedState {
		t.Fatalf("owner start: %+v", started)
	}
	groups, _, err := trace.Workgroups(repository)
	fixturekit.Must(t, err)
	group, ok := trace.WorkgroupOf(groups, stream)
	if !ok || !slices.Equal(group.Members, []config.WorkstreamID{stream}) {
		t.Fatalf("started single-member workgroup: %+v", group)
	}
	handed, err := trace.Get[trace.Document](repository, stream, intake.HandedDocument, 1)
	fixturekit.Must(t, err)
	if !strings.HasPrefix(handed.Content, designReply) || handed.Path != intake.DesignBriefPath {
		t.Fatalf("handed brief: got %q at %s, want drafted brief followed by transcript at %s", handed.Content, handed.Path, intake.DesignBriefPath)
	}

	const question = "May the module be renamed?"
	const statusGoal = "Scoping the rename."
	const escalationQuestion = "Should the module be renamed?"

	engine.Mu.Lock()
	engine.Turns["*"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		prompt := intakefixture.OwnPrompt(req.Prompt)
		if strings.Contains(prompt, intake.HandoffTitleOpening) && !strings.Contains(prompt, "asked by the mason: "+question) {
			return intakefixture.QuestionResult(req, "session-chief", "Titled."), nil
		}
		if !strings.Contains(prompt, "asked by the mason: "+question) {
			problem("unexpected chief-of-staff turn: %s", req.Prompt)
			return intakefixture.QuestionResult(req, "session-chief", "Nothing to do"), nil
		}
		names, err := intakefixture.ToolNames(ctx, tools)
		want := slices.Clone(service.ChiefOrdinaryTools)
		slices.Sort(want)
		if err != nil || !slices.Equal(names, want) {
			problem("ordinary chief tools: %v %v, want %v", names, err, want)
		}
		if out, err := fixturekit.CallTool(ctx, tools, service.ProjectContextTool, map[string]any{"path": "charter.md"}); err != nil || !strings.Contains(out, "Keep state in files under the root.") {
			problem("project context through production enforcement: %s %v", out, err)
		}
		if out, err := fixturekit.CallTool(ctx, tools, service.ProjectContextTool, map[string]any{"path": "tools/design-brief.json"}); err == nil || !strings.Contains(err.Error(), "project document revision not found") {
			problem("project context did not refuse private workstream records: %s %v", out, err)
		}
		if out, err := fixturekit.CallTool(ctx, tools, status.ToolName, map[string]any{"goal": statusGoal, "note": "Looking into the rename.", "agents": []string{"Scoping the rename."}}); err != nil || !strings.Contains(out, `"stored":true`) {
			problem("set_status: %s %v", out, err)
		}
		if out, err := fixturekit.CallTool(ctx, tools, questions.EscalateTool, map[string]any{"questions": []string{"1"}, "rephrasing": escalationQuestion,
			"blocked": "The worker's work.", "options": []string{"Rename", "Keep"}, "recommendation": "Keep."}); err != nil || !strings.Contains(out, `"recorded":true`) {
			problem("escalate: %s %v", out, err)
		}
		mu.Lock()
		acted = true
		mu.Unlock()
		return intakefixture.QuestionResult(req, "session-chief", "Handled"), nil
	}
	engine.Mu.Unlock()

	clock.Advance(time.Minute)
	waitCompleted("hand-off title", 2, 0, "")
	titled, err := repository.ChiefOfStaffThread(stream)
	fixturekit.Must(t, err)
	if len(titled.Turns) != 2 || !strings.Contains(titled.Turns[1].Request.Prompt, intake.HandoffTitleOpening) {
		t.Fatalf("the start's hand-off turn is not the second chief turn: %s", turnSummary(titled))
	}

	now := clock.Now()
	h := trace.Header{Schema: "osmia.trace.agent", Version: 1, Revision: 1, ID: designWorkerAgent, Project: project, Workstream: stream, At: now, Actor: intake.OwnerActor, Cause: "workstream_created"}
	fixturekit.Must(t, repository.CreateThread(ctx, trace.Agent{Header: h, Role: designWorkerRole, ThreadID: designWorkerThread}))
	h.Schema, h.ID, h.Cause, h.Depth = "osmia.trace.turn-request", "request_ask", "message_ask", 1
	_, err = repository.EnqueueTurn(ctx, trace.TurnRequest{Header: h, AgentID: designWorkerAgent, ThreadID: designWorkerThread, TurnID: "ask",
		Profile: coreadapter.Profile{Name: "default", Backend: "claude", Model: "test"}, SystemPrompt: "You are the mason.", Prompt: "Work: ask"})
	fixturekit.Must(t, err)

	engine.Mu.Lock()
	engine.Turns["ask"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		if out, err := fixturekit.CallTool(ctx, tools, questions.AskTool, map[string]any{"question": question}); err != nil || !strings.Contains(out, `"recorded":true`) {
			problem("ask: %s %v", out, err)
		}
		return intakefixture.QuestionResult(req, "session-ask", "Asked"), nil
	}
	engine.Mu.Unlock()

	settle()
	clock.Advance(time.Minute)
	waitCompleted("worker ask and chief escalation", 3, 1, "waiting")

	mu.Lock()
	ranChief := acted
	mu.Unlock()
	if !ranChief {
		chief, err := repository.ChiefOfStaffThread(stream)
		fixturekit.Must(t, err)
		fatalf("the chief-of-staff turn that answers the worker's question never ran; chief %s", turnSummary(chief))
	}

	after, err := repository.ChiefOfStaffThread(stream)
	fixturekit.Must(t, err)
	if after.Identity.ID != before.Identity.ID || after.Identity.ThreadID != before.Identity.ThreadID {
		fatalf("the started workgroup's chief of staff is not the design session's own thread: before %+v after %+v", before.Identity, after.Identity)
	}
	if len(after.Turns) <= len(before.Turns) {
		fatalf("no later turn landed on the continued thread: before %d turns, after %+v", len(before.Turns), after.Turns)
	}
	originalTurns, err := json.Marshal(before.Turns)
	fixturekit.Must(t, err)
	continuedTurns, err := json.Marshal(after.Turns[:len(before.Turns)])
	fixturekit.Must(t, err)
	if string(originalTurns) != string(continuedTurns) {
		fatalf("continuation changed the design turn's request, response or recovery state")
	}

	st, err := c.Status(ctx, stream)
	fixturekit.Must(t, err)
	if st.Status == nil || st.Status.Goal != statusGoal {
		fatalf("a later status did not appear on the continued thread: %+v", st.Status)
	}

	inbox, err := c.Inbox(ctx)
	fixturekit.Must(t, err)
	if len(inbox.Entries) != 1 || inbox.Entries[0].Workstream != stream || inbox.Entries[0].Question != escalationQuestion {
		fatalf("a later escalation did not appear on the continued thread's inbox: %+v", inbox.Entries)
	}
	conversation, err := c.Conversation(ctx, stream)
	fixturekit.Must(t, err)
	if len(conversation.Entries) < len(designConversation.Entries) {
		fatalf("continuation lost the design exchange: %+v", conversation.Entries)
	}
	originalEntries, err := json.Marshal(designConversation.Entries)
	fixturekit.Must(t, err)
	continuedEntries, err := json.Marshal(conversation.Entries[:len(designConversation.Entries)])
	fixturekit.Must(t, err)
	if string(originalEntries) != string(continuedEntries) {
		fatalf("continuation changed the original chat entries: %+v", conversation.Entries)
	}
	wantEscalation := api.ConversationEntry{Turn: inbox.Entries[0].Batch, Kind: "action", Text: "Question for you: " + escalationQuestion + "\nBlocked: The worker's work.\nOptions: Rename; Keep\nRecommendation: Keep.", At: clock.Now(), State: api.TurnDone}
	if len(conversation.Entries) != 3 || conversation.Entries[2] != wantEscalation {
		fatalf("continued chat: want original exchange then exactly one escalation %+v; actual %+v; chief %s", wantEscalation, conversation.Entries, turnSummary(after))
	}
	feed, apiErr := s.FeedList(string(stream))
	if apiErr != nil {
		fatalf("%v", apiErr)
	}
	if !slices.ContainsFunc(feed.Entries, func(entry api.FeedEntry) bool {
		return entry.Kind == "status" && entry.Status != nil && entry.Status.Goal == statusGoal
	}) {
		fatalf("the later status is absent from the continued chat's feed: %+v", feed.Entries)
	}

	restart := func() {
		t.Helper()
		worker, err := repository.Thread(stream, designWorkerAgent)
		fixturekit.Must(t, err)
		waitCompleted("before restart", 2, len(worker.Turns), worker.Status)
		previous, err := c.Conversation(ctx, stream)
		fixturekit.Must(t, err)
		chief, err := repository.ChiefOfStaffThread(stream)
		fixturekit.Must(t, err)
		options := s.Options
		c.Close()
		fixturekit.Must(t, s.Close())
		s, c = journey.StartT(t, options)
		repository = s.Sole().Repository
		settle()
		waitCompleted("after restart", len(chief.Turns), len(worker.Turns), worker.Status)
		continued, err := c.Conversation(ctx, stream)
		fixturekit.Must(t, err)
		previousJSON, err := json.Marshal(previous)
		fixturekit.Must(t, err)
		continuedJSON, err := json.Marshal(continued)
		fixturekit.Must(t, err)
		if string(previousJSON) != string(continuedJSON) {
			fatalf("restart changed conversation: want %s; actual %s", previousJSON, continuedJSON)
		}
		recoveredChief, err := repository.ChiefOfStaffThread(stream)
		fixturekit.Must(t, err)
		chiefJSON, err := json.Marshal(chief)
		fixturekit.Must(t, err)
		recoveredJSON, err := json.Marshal(recoveredChief)
		fixturekit.Must(t, err)
		if string(chiefJSON) != string(recoveredJSON) {
			fatalf("restart changed chief records: want %s; actual %s", turnSummary(chief), turnSummary(recoveredChief))
		}
		recoveredWorker, err := repository.Thread(stream, designWorkerAgent)
		fixturekit.Must(t, err)
		workerJSON, err := json.Marshal(worker)
		fixturekit.Must(t, err)
		recoveredWorkerJSON, err := json.Marshal(recoveredWorker)
		fixturekit.Must(t, err)
		if string(workerJSON) != string(recoveredWorkerJSON) {
			fatalf("restart changed worker records: want %s; actual %s", turnSummary(worker), turnSummary(recoveredWorker))
		}
		recoveredBrief, err := trace.Get[trace.Document](repository, stream, handed.ID, 1)
		fixturekit.Must(t, err)
		briefJSON, err := json.Marshal(handed)
		fixturekit.Must(t, err)
		recoveredBriefJSON, err := json.Marshal(recoveredBrief)
		fixturekit.Must(t, err)
		if string(briefJSON) != string(recoveredBriefJSON) {
			fatalf("restart changed immutable handed brief: got %+v, want %+v", recoveredBrief, handed)
		}
	}
	restart()
	recoveredInbox, err := c.Inbox(ctx)
	fixturekit.Must(t, err)
	if len(recoveredInbox.Entries) != 1 || recoveredInbox.Entries[0].Question != escalationQuestion {
		fatalf("continued design escalation after restart: %+v", recoveredInbox.Entries)
	}
	const ownerRuling = "Keep the module name stable."
	var resumed int
	engine.Mu.Lock()
	engine.Turns["*"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		if !strings.Contains(req.Prompt, ownerRuling) {
			problem("chief ruling prompt lacks the owner's decision: %s", req.Prompt)
		}
		if out, err := fixturekit.CallTool(ctx, tools, questions.RelayRulingTool, map[string]any{"question": "1", "text": ownerRuling, "scope": "notify"}); err != nil || !strings.Contains(out, `"recorded":true`) {
			problem("relay ruling: %s %v", out, err)
		}
		return intakefixture.QuestionResult(req, "session-chief", ownerRuling), nil
	}
	engine.Turns["answer_1"] = func(_ context.Context, req agent.Request, _ *agent.Turn, _ *mcp.ClientSession) (*agent.Result, error) {
		if !strings.Contains(req.Prompt, ownerRuling) {
			problem("resumed worker lacks the owner's ruling: %s", req.Prompt)
		}
		mu.Lock()
		resumed++
		mu.Unlock()
		return intakefixture.QuestionResult(req, "session-ask", "Continuing with the stable module name."), nil
	}
	engine.Mu.Unlock()
	answer, err := c.Answer(ctx, recoveredInbox.Entries[0].Number, ownerRuling, "")
	fixturekit.Must(t, err)
	wantEntries := append(slices.Clone(conversation.Entries), api.ConversationEntry{Turn: "ruling_" + answer.Batch, Kind: "action", Text: "You ruled: " + ownerRuling, At: answer.At, State: api.TurnDone})
	ownerConversation, err := c.Conversation(ctx, stream)
	fixturekit.Must(t, err)
	if !slices.Equal(ownerConversation.Entries, wantEntries) {
		fatalf("owner answer before relay: want %+v; actual %+v", wantEntries, ownerConversation.Entries)
	}
	clock.Advance(time.Minute)
	waitCompleted("owner ruling relay and worker continuation", len(after.Turns)+1, 2, "idle")
	worker, err := repository.Thread(stream, designWorkerAgent)
	fixturekit.Must(t, err)
	if worker.Parked() || len(worker.Turns) != 2 || worker.Turns[1].Request.TurnID != "answer_1" || worker.Turns[1].Response == nil || worker.Turns[1].Status() != "idle" || worker.Turns[1].CompletedAt.IsZero() {
		fatalf("ruling: want exactly one completed answer_1 on asking worker; actual %s", turnSummary(worker))
	}
	answeredInbox, err := c.Inbox(ctx)
	fixturekit.Must(t, err)
	if len(answeredInbox.Entries) != 0 {
		fatalf("answered escalation remains in the inbox: %+v", answeredInbox.Entries)
	}
	ruledConversation, err := c.Conversation(ctx, stream)
	fixturekit.Must(t, err)
	ruledChief, err := repository.ChiefOfStaffThread(stream)
	fixturekit.Must(t, err)
	ruling, err := trace.Get[trace.Ruling](repository, stream, "1", 2)
	fixturekit.Must(t, err)
	if ruling.ReturnedAnswer != ownerRuling || ruling.OwnerResponse != ownerRuling || !slices.ContainsFunc(ruledChief.Turns, func(q trace.QueuedTurn) bool {
		return q.Request.ID == ruling.Cause && q.Status() == "idle" && !q.CompletedAt.IsZero() && q.Response != nil && q.Response.Result.FinalResponse == ownerRuling
	}) {
		fatalf("relay: want ruling linked to a completed chief turn on the actual design thread; ruling %+v; chief %s", ruling, turnSummary(ruledChief))
	}
	continuedTurns, err = json.Marshal(ruledChief.Turns[:len(before.Turns)])
	fixturekit.Must(t, err)
	if string(originalTurns) != string(continuedTurns) {
		fatalf("owner ruling changed the original design request, response or recovery state")
	}
	if !slices.Equal(ruledConversation.Entries, wantEntries) {
		fatalf("owner ruling chat: want %+v; actual %+v; chief %s; worker %s", wantEntries, ruledConversation.Entries, turnSummary(ruledChief), turnSummary(worker))
	}
	restart()
	mu.Lock()
	if resumed != 1 {
		problems = append(problems, fmt.Sprintf("worker resumed %d times across restart, want 1", resumed))
	}
	mu.Unlock()

	mu.Lock()
	defer mu.Unlock()
	if len(problems) != 0 {
		t.Fatal(strings.Join(problems, "\n"))
	}
}
