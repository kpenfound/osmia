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

	agent "github.com/kpenfound/agentcore/agent"
	config "github.com/kpenfound/osmia/internal/config"
	coreadapter "github.com/kpenfound/osmia/internal/coreadapter"
	isolation "github.com/kpenfound/osmia/internal/isolation"
	questions "github.com/kpenfound/osmia/internal/questions"
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	status "github.com/kpenfound/osmia/internal/status"
	thread "github.com/kpenfound/osmia/internal/thread"
	trace "github.com/kpenfound/osmia/internal/trace"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// newDesignFixture starts a service with an active project whose charter
// has rules and whose clone has one commit, the same shape newHandInFixture
// gives an ordinary hand-in.
func newDesignFixture(t *testing.T) (*Service, *Client, config.ProjectID) {
	t.Helper()
	f := newHandInFixture(t)
	return f.s, f.c, f.project
}

// awaitDesignConversation polls a design session's conversation until done
// accepts it.
func awaitDesignConversation(t *testing.T, c *Client, id config.WorkstreamID, done func(ConversationResponse) bool) ConversationResponse {
	t.Helper()
	return intakefixture.AwaitView(t, func() (ConversationResponse, error) {
		return c.DesignConversation(context.Background(), id)
	}, done, "design conversation did not settle: %+v")
}

// TestOpenDesignCreatesAnInertWorkstreamInItsOwnWorkgroup covers spec#6: a
// design session opens as an ordinary workstream with a chief-of-staff
// thread, wrapped in its own one-workstream workgroup like any other
// workstream, but with no kind, no feature state and no spec, plan or unit:
// none of the factory passes that act on a feature state have anything to
// act on until the owner starts a workgroup from its brief.
func TestOpenDesignCreatesAnInertWorkstreamInItsOwnWorkgroup(t *testing.T) {
	t.Parallel()
	s, c, project := newDesignFixture(t)
	opened, err := c.OpenDesign(context.Background(), project)
	must(t, err)
	stream := opened.Design.Workstream
	if opened.Design.Project != project {
		t.Fatalf("design view: %+v", opened.Design)
	}

	repository := s.sole().repository
	design, err := isDesignWorkstream(repository, stream)
	must(t, err)
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
	must(t, err)
	g, ok := trace.WorkgroupOf(groups, stream)
	if !ok || len(g.Members) != 1 || g.Members[0] != stream {
		t.Fatalf("workgroup of design session %s: %+v ok=%v", stream, g, ok)
	}
	host, err := repository.ChiefOfStaffHost(stream)
	must(t, err)
	if host != stream {
		t.Fatalf("a one-workstream workgroup's chief of staff should be its own thread, got host %s", host)
	}

	byID, err := c.Design(context.Background(), stream)
	must(t, err)
	if byID.Design.Workstream != stream {
		t.Fatalf("design read: %+v", byID.Design)
	}
	all, err := c.Designs(context.Background(), "")
	must(t, err)
	if !slices.ContainsFunc(all.Designs, func(d DesignView) bool { return d.Workstream == stream }) {
		t.Fatalf("design list does not include %s: %+v", stream, all.Designs)
	}
	status, err := c.Status(context.Background(), stream)
	must(t, err)
	if status.Kind != "design" {
		t.Fatalf("workstream status kind: %+v", status)
	}
}

// TestExploratorySessionsAreNotDesignSessions covers spec#24's "exploratory
// sessions behave exactly as before": a session workstream (design §6.8)
// never satisfies isDesignWorkstream and never appears in the design list,
// whatever state its own feature subject happens to carry.
func TestExploratorySessionsAreNotDesignSessions(t *testing.T) {
	t.Parallel()
	s, c, project := newDesignFixture(t)
	repository := s.sole().repository
	stream, err := config.NewWorkstreamID()
	must(t, err)
	now := time.Now().UTC()
	must(t, repository.CreateSessionWorkstream(context.Background(), stream, "default", "deadbeef", "osmia/session/"+string(stream), "", now, ownerActor))

	design, err := isDesignWorkstream(repository, stream)
	must(t, err)
	if design {
		t.Fatal("a session workstream must never be treated as a design session")
	}
	all, err := c.Designs(context.Background(), project)
	must(t, err)
	if slices.ContainsFunc(all.Designs, func(d DesignView) bool { return d.Workstream == stream }) {
		t.Fatalf("design list wrongly includes the session %s: %+v", stream, all.Designs)
	}
	if _, _, _, api := s.designTrace(string(stream)); api == nil {
		t.Fatal("a session workstream must be refused as a design session")
	}
}

// TestOrdinaryConversationRefusesADesignSessionAndDesignMessageRefusesAStartedOne
// covers spec#8's thread continuity from the other side: before a workgroup
// starts, the ordinary chief-of-staff conversation endpoint must not be used
// on a design session, so the owner is never shown its ordinary, far wider
// toolset and prompt by mistake; and once a workgroup has started, the
// design message endpoint must not be used either, since the workstream is
// no longer a design session.
func TestOrdinaryConversationRefusesADesignSessionAndDesignMessageRefusesAStartedOne(t *testing.T) {
	t.Parallel()
	s, c, project := newDesignFixture(t)
	opened, err := c.OpenDesign(context.Background(), project)
	must(t, err)
	stream := opened.Design.Workstream

	if _, err := c.Send(context.Background(), stream, "Hello"); err == nil {
		t.Fatal("the ordinary conversation endpoint must refuse a design session")
	}

	repository := s.sole().repository
	must(t, recordDesignDraft(t, repository, stream, "Decisions: ship it.\nIntent and tradeoffs: keep it simple.\nRejected options: none.\n"))
	if _, err := s.startDesignWorkgroup(context.Background(), string(stream)); err != nil {
		t.Fatalf("start: %+v", err)
	}
	if state, err := repository.Workflow(stream, trace.FeatureSubject); err != nil || state.Value != HandedState {
		t.Fatalf("feature state after start: %+v %v", state, err)
	}
	if _, err := c.DesignMessage(context.Background(), stream, "Still there?"); err == nil {
		t.Fatal("the design message endpoint must refuse a workstream that has already started a workgroup")
	}
}

var recordDesignDraft = intakefixture.RecordDesignDraft

// TestStartDesignWorkgroupHandsOffBriefOnTheSameChiefOfStaffThread covers
// spec#7 and spec#8: starting a workgroup is refused until a brief is
// drafted; once it is, the drafted content (not a rejected or an earlier
// draft) reaches the architect's handed input with the transcript attached
// for reference, and the same chief-of-staff thread the design
// conversation used continues, with no new one created, as the workgroup's.
func TestStartDesignWorkgroupHandsOffBriefOnTheSameChiefOfStaffThread(t *testing.T) {
	t.Parallel()
	s, c, project := newDesignFixture(t)
	opened, err := c.OpenDesign(context.Background(), project)
	must(t, err)
	stream := opened.Design.Workstream
	repository := s.sole().repository

	if _, api := s.startDesignWorkgroup(context.Background(), string(stream)); api == nil {
		t.Fatal("starting a workgroup before any brief is drafted must be refused")
	}

	before, err := repository.ChiefOfStaffThread(stream)
	must(t, err)

	must(t, recordDesignDraft(t, repository, stream, "Decisions: ship the upload feature.\nIntent and tradeoffs: keep the API stable.\nRejected options: a new endpoint was rejected.\n"))

	must(t, recordDesignDraft2(t, repository, stream, "Decisions: ship resumable uploads.\nIntent and tradeoffs: favor simplicity over flexibility.\nRejected options: a separate service was rejected as overkill.\n"))

	out, err := c.DesignStart(context.Background(), stream)
	must(t, err)
	if out.Workstream != stream || out.State != HandedState || out.Handed != designBriefPath {
		t.Fatalf("start response: %+v", out)
	}

	docs, err := trace.Read[trace.Document](repository, stream)
	must(t, err)
	var handed trace.Document
	for _, d := range docs {
		if d.ID == handedDocument {
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
	must(t, err)
	if after.Identity.ID != before.Identity.ID || after.Identity.ThreadID != before.Identity.ThreadID {
		t.Fatalf("starting the workgroup must continue the same thread: before %+v after %+v", before.Identity, after.Identity)
	}
	host, err := repository.ChiefOfStaffHost(stream)
	must(t, err)
	if host != stream {
		t.Fatalf("the started workstream's chief of staff should still be its own thread, got host %s", host)
	}

	latest := map[string]trace.Document{}
	for _, d := range docs {
		if prior, ok := latest[d.ID]; !ok || d.Revision > prior.Revision {
			latest[d.ID] = d
		}
	}
	if _, ok := latest[handedDocument]; !ok {
		t.Fatal("the architect's handed() lookup would not find this input")
	}

	if _, api := s.startDesignWorkgroup(context.Background(), string(stream)); api == nil {
		t.Fatal("starting a workgroup twice must be refused")
	}
}

func TestDesignWorkgroupStartRecoversAfterRestartWithoutRewritingBrief(t *testing.T) {
	t.Parallel()
	s, c, project := newDesignFixture(t)
	ctx := context.Background()
	opened, err := c.OpenDesign(ctx, project)
	must(t, err)
	stream := opened.Design.Workstream
	repository := s.sole().repository
	must(t, recordDesignDraft(t, repository, stream, "Decisions: preserve the API.\nIntent and tradeoffs: compatibility over flexibility.\nRejected options: breaking changes."))
	before, err := repository.ChiefOfStaffThread(stream)
	must(t, err)
	s.boundary = func(name string) error {
		if name == "design-brief-handed" {
			return errors.New("interrupted before feature transition")
		}
		return nil
	}
	if _, err := c.DesignStart(ctx, stream); err == nil {
		t.Fatal("expected the injected interruption")
	}
	if _, err := c.DesignMessage(ctx, stream, "Revise the brief"); !failed(err, Conflict) {
		t.Fatalf("pending handoff allowed another turn: %v", err)
	}
	options := s.options
	must(t, s.Close())
	restarted, client := start(t, options)
	result, err := client.DesignStart(ctx, stream)
	must(t, err)
	if result.State != HandedState {
		t.Fatalf("start after restart: %+v", result)
	}
	repository = restarted.sole().repository
	after, err := repository.ChiefOfStaffThread(stream)
	must(t, err)
	if before.Identity != after.Identity {
		t.Fatal("handoff recovery replaced the chief thread")
	}
	documents, err := trace.Read[trace.Document](repository, stream)
	must(t, err)
	count := 0
	for _, document := range documents {
		if document.ID == handedDocument {
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

var recordDesignDraft2 = intakefixture.RecordDesignDraft2

// newDesignEnforcementFixture starts a service with the real production
// enforcement (internal/service/enforcement.go), a fake execution engine
// and a fake in-memory MCP transport, on an onboarded project with a
// committed clone, so a design session's turn exercises the same grant,
// sandbox and tool-registry machinery a production service would. The
// chief-of-staff role runs in a container, the only sandbox mode every
// platform's confiner can enforce (compare chiefDemoRoles): the host
// sandbox every role defaults to needs Landlock or its Seatbelt equivalent
// to confine a session at all, which not every kernel has.
func newDesignEnforcementFixture(t *testing.T, web ...bool) (*Service, *Client, config.ProjectID, *demoEngine) {
	t.Helper()
	opts, clone := projectFixture(t)
	configFile, err := os.OpenFile(filepath.Join(opts.Config.Root, "config.toml"), os.O_APPEND|os.O_WRONLY, 0)
	must(t, err)
	if len(web) > 0 && web[0] {
		_, err = configFile.WriteString(listenWeb)
		must(t, err)
	}
	_, err = configFile.WriteString("[roles.chief_of_staff]\nsandbox = \"container\"\nimage = \"fixture-image\"\n")
	must(t, errors.Join(err, configFile.Close()))
	home := filepath.Dir(clone)
	must(t, os.WriteFile(filepath.Join(clone, "README.md"), []byte("hi\n"), 0600))
	demoGit(t, home, "-C", clone, "add", "README.md")
	demoGit(t, home, "-C", clone, "-c", "user.name=Owner", "-c", "user.email=owner@example.invalid", "commit", "-qm", "base")
	sessions := &demoSessions{ByKey: map[string]*mcp.ClientSession{}}
	engine := &demoEngine{Sessions: sessions, Turns: map[string]demoTurn{}}
	engine.Resume = func(coreadapter.Profile, coreadapter.Profile, coreadapter.BackendSession) error {
		return coreadapter.ErrResumeUnavailable
	}
	enforced := Enforce(opts, Enforcement{Engine: engine, Hosts: &coreadapter.MCPHost{Transport: &demoTransport{Sessions: sessions}}})
	opts.Threads, opts.controls = enforced.Threads, enforced.controls
	s, c := start(t, opts)
	added, err := c.AddProject(context.Background(), request(clone))
	must(t, err)
	writeCharter(t, c, added.Project.ID, "1. Keep changes small.\n")
	return s, c, added.Project.ID, engine
}

// TestDesignSessionProductionEnforcementGrantsOnlyReadOnlyTools covers
// spec#6, checked through production enforcement: a design session's turn
// is scoped to file_read, read_repo and draft_brief alone, with no write,
// version-control, credential or owner-gate tool reachable, whatever the
// chief-of-staff role's full grant otherwise allows.
func TestDesignSessionProductionEnforcementGrantsOnlyReadOnlyTools(t *testing.T) {
	t.Parallel()
	s, c, project, engine := newDesignEnforcementFixture(t)
	clone := filepath.Join(t.TempDir(), "second-repo")
	demoGit(t, filepath.Dir(clone), "init", "--quiet", clone)
	must(t, os.WriteFile(filepath.Join(clone, "README.md"), []byte("second repo source\n"), 0600))
	demoGit(t, filepath.Dir(clone), "-C", clone, "add", "README.md")
	demoGit(t, filepath.Dir(clone), "-C", clone, "-c", "user.name=Owner", "-c", "user.email=owner@example.invalid", "commit", "-qm", "base")
	added, err := c.AddProject(context.Background(), ProjectAddRequest{Name: "client", Upstream: "owner/second", Clone: clone})
	must(t, err)
	var second config.ProjectID
	for _, repo := range added.Project.Repos {
		if repo.ID != project {
			second = repo.ID
		}
	}
	if second == "" {
		t.Fatal("project creation did not register the second project")
	}
	must(t, os.WriteFile(filepath.Join(clone, "README.md"), []byte("uncommitted source\n"), 0600))
	for _, repoID := range []config.ProjectID{project, second} {
		repository, err := s.repository(repoID)
		must(t, err)
		header := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: "subsystem-design", Revision: 1, Project: repoID, At: s.now(), Actor: ownerActor, Cause: "knowledge-base"}
		must(t, repository.RecordDocuments(context.Background(), []trace.Document{{Header: header, Path: "kb/design.md", Content: "Conventions for repo " + string(repoID)}}))
	}
	opened, err := c.OpenDesign(context.Background(), project)
	must(t, err)
	stream := opened.Design.Workstream

	var problems []string
	fail := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	engine.Turns["*"] = func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		if !strings.Contains(req.SystemPrompt, "1. Keep changes small.") {
			fail("missing project charter in design context")
		}
		for _, repo := range []config.ProjectID{project} {
			if !strings.Contains(req.SystemPrompt, "Conventions for repo "+string(repo)) {
				fail("missing repo knowledge base in context: %s", repo)
			}
			out, err := callTool(ctx, tools, designReadRepoToolName, map[string]any{"repo": repo, "path": "README.md"})
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
		out, err := callTool(ctx, tools, designReadRepoToolName, map[string]any{"repo": "p_00000000000000000000000000000099", "path": "README.md"})
		if err == nil || !strings.Contains(err.Error(), "not this project's repository") {
			fail("read outside project was not refused by membership: %s %v", out, err)
		}
		names, err := toolNames(ctx, tools)
		if err != nil {
			return nil, err
		}
		if want := []string{"draft_brief", "file_read", "read_repo"}; !slices.Equal(names, want) {
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
	must(t, err)
	awaitDesignConversation(t, c, stream, func(r ConversationResponse) bool {
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
	_ = s
}

// TestDraftBriefRefusesAnIncompleteBriefAndRecordsTheLatest covers spec#7:
// the brief the owner is shown is refused, recording nothing, until it
// states decisions, intent and tradeoffs, and rejected options, and only
// the latest drafted call is used.
func TestDraftBriefRefusesAnIncompleteBriefAndRecordsTheLatest(t *testing.T) {
	t.Parallel()
	s, c, project, engine := newDesignEnforcementFixture(t)
	opened, err := c.OpenDesign(context.Background(), project)
	must(t, err)
	stream := opened.Design.Workstream

	var problems []string
	fail := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	engine.Turns["*"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		out, err := callTool(ctx, tools, draftBriefToolName, map[string]any{"content": "We will build the thing."})
		if err != nil {
			fail("incomplete brief call failed outright: %v", err)
		} else if !strings.Contains(out, `"recorded":false`) {
			fail("an incomplete brief was recorded: %s", out)
		}
		out, err = callTool(ctx, tools, draftBriefToolName, map[string]any{"content": "Decisions: ship. Intent: compatibility. Rejected options: rewrite."})
		if err != nil || !strings.Contains(out, `"recorded":false`) {
			fail("a brief without tradeoffs was accepted: %s %v", out, err)
		}
		out, err = callTool(ctx, tools, draftBriefToolName, map[string]any{"content": "Decisions: ship it.\nIntent and tradeoffs: keep it simple.\nRejected options: a rewrite was rejected.\n"})
		if err != nil || !strings.Contains(out, `"recorded":true`) {
			fail("a complete brief was refused: %s %v", out, err)
		}
		return &agent.Result{ClaudeID: "design-session-1", ResultText: "Drafted the brief."}, nil
	}
	_, err = c.DesignMessage(context.Background(), stream, "I'm ready for the brief.")
	must(t, err)
	awaitDesignConversation(t, c, stream, func(r ConversationResponse) bool {
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
	docs, err := trace.Read[trace.Document](s.sole().repository, stream)
	must(t, err)
	var draft trace.Document
	for _, d := range docs {
		if d.ID == designDraftDocument && d.Revision >= draft.Revision {
			draft = d
		}
	}
	if draft.Revision != 1 || !strings.Contains(draft.Content, "Decisions") {
		t.Fatalf("recorded draft: %+v", draft)
	}
	_ = c
}

// designWorkerRole, designWorkerAgent and designWorkerThread name the one
// fake worker newDesignWorkgroupContinuationFixture wires to the plain
// question tools, standing in for a real mason's turn without needing a
// unit workspace.
const designWorkerRole, designWorkerAgent, designWorkerThread = "mason", "agent_design_worker", "thread_design_worker"

// newDesignWorkgroupContinuationFixture is newDesignEnforcementFixture with
// a controllable clock and one worker role wired directly to the plain
// question tools (questions.Tools), so a test can drive a worker's question
// and the chief of staff's status and escalation on the thread that
// continues from a design session, without standing up a real mason's unit
// workspace. The chief-of-staff role keeps the production selection and
// tools Enforce builds, including the design/ordinary narrowing in
// enforcement.go.
func newDesignWorkgroupContinuationFixture(t *testing.T) (*Service, *Client, config.ProjectID, *demoEngine, *fixedClock, chan time.Time) {
	t.Helper()
	opts, clone := projectFixture(t)
	configFile, err := os.OpenFile(filepath.Join(opts.Config.Root, "config.toml"), os.O_APPEND|os.O_WRONLY, 0)
	must(t, err)
	_, err = configFile.WriteString("[roles.chief_of_staff]\nsandbox = \"container\"\nimage = \"fixture-image\"\n")
	must(t, errors.Join(err, configFile.Close()))
	home := filepath.Dir(clone)
	must(t, os.WriteFile(filepath.Join(clone, "README.md"), []byte("hi\n"), 0600))
	demoGit(t, home, "-C", clone, "add", "README.md")
	demoGit(t, home, "-C", clone, "-c", "user.name=Owner", "-c", "user.email=owner@example.invalid", "commit", "-qm", "base")

	sessions := &demoSessions{ByKey: map[string]*mcp.ClientSession{}}
	engine := &demoEngine{Sessions: sessions, Turns: map[string]demoTurn{}}
	engine.Resume = func(coreadapter.Profile, coreadapter.Profile, coreadapter.BackendSession) error {
		return coreadapter.ErrResumeUnavailable
	}
	clock := &fixedClock{At: demoStart}
	opts.Reconciliation.Now = clock.Now
	ticks := make(chan time.Time)
	opts.Reconciliation.Ticks = ticks

	enforced := Enforce(opts, Enforcement{Engine: engine, Hosts: &coreadapter.MCPHost{Transport: &demoTransport{Sessions: sessions}}})
	opts.controls = enforced.controls
	production := enforced.Threads
	opts.Threads = func(r *trace.Repository, cfg *config.Config) (coreadapter.Reconciler, error) {
		bound, err := production(r, cfg)
		if err != nil {
			return nil, err
		}
		turns := bound.(thread.Dispatcher).Runner.Turns.(*questions.Turns).Turns.(*verdictTurns).Turns.(*reportingTurns).Turns.(*retriedTurns).Turns.(*isolation.Turns)
		turns.Grants = maps.Clone(turns.Grants)
		turns.Grants[designWorkerRole] = coreadapter.Capabilities{Tools: []string{questions.AskTool}}

		turns.Workspaces, turns.Capture = stagedWorkspaces{}, nil
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
	s, c := start(t, opts)
	added, err := c.AddProject(context.Background(), request(clone))
	must(t, err)
	writeCharter(t, c, added.Project.ID, "1. Keep state in files under the root.\n")
	return s, c, added.Project.ID, engine, clock, ticks
}

// TestDesignWorkgroupThreadCarriesALaterStatusAndEscalation covers
// single-member continuation: one owner start hands off the brief drafted
// in a real design turn. A worker's question reaches that same thread, and
// the chief's later status and escalation preserve the original exchange
// and its durable turn records. The owner's ruling resumes the worker on
// its own thread, with the continued conversation preserved across restarts.
func TestDesignWorkgroupThreadCarriesALaterStatusAndEscalation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, c, project, engine, clock, ticks := newDesignWorkgroupContinuationFixture(t)
	settle := func() {
		t.Helper()
		for range 5 {
			select {
			case ticks <- clock.Now():
			case <-time.After(demoTimeout):
				s.Close()
				t.Fatal("service did not finish its pass")
			}
		}
	}

	opened, err := c.OpenDesign(ctx, project)
	must(t, err)
	stream := opened.Design.Workstream
	repository := s.sole().repository
	var mu sync.Mutex
	var problems []string
	var acted bool
	problem := func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		problems = append(problems, fmt.Sprintf(format, args...))
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
		deadline := time.Now().Add(demoTimeout)
		for {
			chief, err := repository.ChiefOfStaffThread(stream)
			must(t, err)
			complete := func(th trace.Thread) bool {
				return th.Active == "" && !slices.ContainsFunc(th.Turns, func(q trace.QueuedTurn) bool {
					return q.CompletedAt.IsZero() || q.Response == nil || (q.Status() != "idle" && q.Status() != "waiting")
				})
			}
			ready := len(chief.Turns) >= chiefCount && complete(chief)
			var worker trace.Thread
			if workerCount > 0 {
				worker, err = repository.Thread(stream, designWorkerAgent)
				must(t, err)
				ready = ready && len(worker.Turns) == workerCount && complete(worker) && worker.Status == workerStatus
			}
			outbox, err := repository.Outbox(stream)
			must(t, err)
			var pending []string
			for _, e := range outbox {
				window := s.current().EventWindow()
				if e.Event.Kind == trace.ProgressKind {
					window = s.current().EventProgressWindow()
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
		if out, err := callTool(ctx, tools, draftBriefToolName, map[string]any{"content": designReply}); err != nil || !strings.Contains(out, `"recorded":true`) {
			return nil, fmt.Errorf("draft brief: %s %v", out, err)
		}
		return questionResult(req, "session-design", designReply), nil
	}
	engine.Mu.Unlock()
	sent, err := c.DesignMessage(ctx, stream, designMessage)
	must(t, err)
	waitCompleted("design reply", 1, 0, "")
	designConversation := awaitDesignConversation(t, c, stream, func(r ConversationResponse) bool {
		return len(r.Entries) == 2 && r.Entries[1].Kind == "response"
	})
	if designConversation.Entries[0].Text != designMessage || designConversation.Entries[1].Text != designReply || designConversation.Entries[1].State != TurnDone {
		t.Fatalf("design conversation: %+v", designConversation.Entries)
	}
	before, err := repository.ChiefOfStaffThread(stream)
	must(t, err)
	if len(before.Turns) != 1 || before.Turns[0].Request.TurnID != sent.Turn || before.Turns[0].Response == nil || before.Turns[0].CompletedAt.IsZero() {
		t.Fatalf("design exchange did not complete on its actual thread: %+v", before)
	}

	started, err := c.DesignStart(ctx, stream)
	must(t, err)
	if started.Workstream != stream || started.State != HandedState {
		t.Fatalf("owner start: %+v", started)
	}
	groups, _, err := trace.Workgroups(repository)
	must(t, err)
	group, ok := trace.WorkgroupOf(groups, stream)
	if !ok || !slices.Equal(group.Members, []config.WorkstreamID{stream}) {
		t.Fatalf("started single-member workgroup: %+v", group)
	}
	handed, err := trace.Get[trace.Document](repository, stream, handedDocument, 1)
	must(t, err)
	if !strings.HasPrefix(handed.Content, designReply) || handed.Path != designBriefPath {
		t.Fatalf("handed brief: got %q at %s, want drafted brief followed by transcript at %s", handed.Content, handed.Path, designBriefPath)
	}

	now := clock.Now()
	h := trace.Header{Schema: "osmia.trace.agent", Version: 1, Revision: 1, ID: designWorkerAgent, Project: project, Workstream: stream, At: now, Actor: ownerActor, Cause: "workstream_created"}
	must(t, repository.CreateThread(ctx, trace.Agent{Header: h, Role: designWorkerRole, ThreadID: designWorkerThread}))
	h.Schema, h.ID, h.Cause, h.Depth = "osmia.trace.turn-request", "request_ask", "message_ask", 1
	_, err = repository.EnqueueTurn(ctx, trace.TurnRequest{Header: h, AgentID: designWorkerAgent, ThreadID: designWorkerThread, TurnID: "ask",
		Profile: coreadapter.Profile{Name: "default", Backend: "claude", Model: "test"}, SystemPrompt: "You are the mason.", Prompt: "Work: ask"})
	must(t, err)

	const question = "May the module be renamed?"
	const statusGoal = "Scoping the rename."
	const escalationQuestion = "Should the module be renamed?"

	engine.Mu.Lock()
	engine.Turns["ask"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		if out, err := callTool(ctx, tools, questions.AskTool, map[string]any{"question": question}); err != nil || !strings.Contains(out, `"recorded":true`) {
			problem("ask: %s %v", out, err)
		}
		return questionResult(req, "session-ask", "Asked"), nil
	}
	engine.Turns["*"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		if !strings.Contains(req.Prompt, "asked by the mason: "+question) {
			problem("unexpected chief-of-staff turn: %s", req.Prompt)
			return questionResult(req, "session-chief", "Nothing to do"), nil
		}
		names, err := toolNames(ctx, tools)
		want := slices.Clone(chiefOrdinaryTools)
		slices.Sort(want)
		if err != nil || !slices.Equal(names, want) {
			problem("ordinary chief tools: %v %v, want %v", names, err, want)
		}
		if out, err := callTool(ctx, tools, projectContextTool, map[string]any{"path": "charter.md"}); err != nil || !strings.Contains(out, "Keep state in files under the root.") {
			problem("project context through production enforcement: %s %v", out, err)
		}
		if out, err := callTool(ctx, tools, projectContextTool, map[string]any{"path": "tools/design-brief.json"}); err == nil || !strings.Contains(err.Error(), "project document revision not found") {
			problem("project context did not refuse private workstream records: %s %v", out, err)
		}
		if out, err := callTool(ctx, tools, status.ToolName, map[string]any{"goal": statusGoal, "note": "Looking into the rename.", "agents": []string{"Scoping the rename."}}); err != nil || !strings.Contains(out, `"stored":true`) {
			problem("set_status: %s %v", out, err)
		}
		if out, err := callTool(ctx, tools, questions.EscalateTool, map[string]any{"questions": []string{"1"}, "rephrasing": escalationQuestion,
			"blocked": "The worker's work.", "options": []string{"Rename", "Keep"}, "recommendation": "Keep."}); err != nil || !strings.Contains(out, `"recorded":true`) {
			problem("escalate: %s %v", out, err)
		}
		mu.Lock()
		acted = true
		mu.Unlock()
		return questionResult(req, "session-chief", "Handled"), nil
	}
	engine.Mu.Unlock()

	settle()
	clock.Advance(time.Minute)
	waitCompleted("worker ask and chief escalation", 2, 1, "waiting")

	mu.Lock()
	ranChief := acted
	mu.Unlock()
	if !ranChief {
		t.Fatal("the chief-of-staff turn that answers the worker's question never ran")
	}

	after, err := repository.ChiefOfStaffThread(stream)
	must(t, err)
	if after.Identity.ID != before.Identity.ID || after.Identity.ThreadID != before.Identity.ThreadID {
		t.Fatalf("the started workgroup's chief of staff is not the design session's own thread: before %+v after %+v", before.Identity, after.Identity)
	}
	if len(after.Turns) <= len(before.Turns) {
		t.Fatalf("no later turn landed on the continued thread: before %d turns, after %+v", len(before.Turns), after.Turns)
	}
	originalTurns, err := json.Marshal(before.Turns)
	must(t, err)
	continuedTurns, err := json.Marshal(after.Turns[:len(before.Turns)])
	must(t, err)
	if string(originalTurns) != string(continuedTurns) {
		t.Fatal("continuation changed the design turn's request, response or recovery state")
	}

	st, err := c.Status(ctx, stream)
	must(t, err)
	if st.Status == nil || st.Status.Goal != statusGoal {
		t.Fatalf("a later status did not appear on the continued thread: %+v", st.Status)
	}

	inbox, err := c.Inbox(ctx)
	must(t, err)
	if len(inbox.Entries) != 1 || inbox.Entries[0].Workstream != stream || inbox.Entries[0].Question != escalationQuestion {
		t.Fatalf("a later escalation did not appear on the continued thread's inbox: %+v", inbox.Entries)
	}
	conversation, err := c.Conversation(ctx, stream)
	must(t, err)
	if len(conversation.Entries) < len(designConversation.Entries) {
		t.Fatalf("continuation lost the design exchange: %+v", conversation.Entries)
	}
	originalEntries, err := json.Marshal(designConversation.Entries)
	must(t, err)
	continuedEntries, err := json.Marshal(conversation.Entries[:len(designConversation.Entries)])
	must(t, err)
	if string(originalEntries) != string(continuedEntries) {
		t.Fatalf("continuation changed the original chat entries: %+v", conversation.Entries)
	}
	wantEscalation := ConversationEntry{Turn: inbox.Entries[0].Batch, Kind: "action", Text: "Question for you: " + escalationQuestion + "\nBlocked: The worker's work.\nOptions: Rename; Keep\nRecommendation: Keep.", At: clock.Now(), State: TurnDone}
	if len(conversation.Entries) != 3 || conversation.Entries[2] != wantEscalation {
		t.Fatalf("continued chat: want original exchange then exactly one escalation %+v; actual %+v; chief %s", wantEscalation, conversation.Entries, turnSummary(after))
	}
	feed, api := s.feedList(string(stream))
	if api != nil {
		t.Fatal(api)
	}
	if !slices.ContainsFunc(feed.Entries, func(entry FeedEntry) bool {
		return entry.Kind == "status" && entry.Status != nil && entry.Status.Goal == statusGoal
	}) {
		t.Fatalf("the later status is absent from the continued chat's feed: %+v", feed.Entries)
	}

	restart := func() {
		t.Helper()
		worker, err := repository.Thread(stream, designWorkerAgent)
		must(t, err)
		waitCompleted("before restart", 2, len(worker.Turns), worker.Status)
		previous, err := c.Conversation(ctx, stream)
		must(t, err)
		chief, err := repository.ChiefOfStaffThread(stream)
		must(t, err)
		options := s.options
		c.Close()
		must(t, s.Close())
		s, c = start(t, options)
		repository = s.sole().repository
		settle()
		waitCompleted("after restart", len(chief.Turns), len(worker.Turns), worker.Status)
		continued, err := c.Conversation(ctx, stream)
		must(t, err)
		previousJSON, err := json.Marshal(previous)
		must(t, err)
		continuedJSON, err := json.Marshal(continued)
		must(t, err)
		if string(previousJSON) != string(continuedJSON) {
			t.Fatalf("restart changed conversation: want %s; actual %s", previousJSON, continuedJSON)
		}
		recoveredChief, err := repository.ChiefOfStaffThread(stream)
		must(t, err)
		chiefJSON, err := json.Marshal(chief)
		must(t, err)
		recoveredJSON, err := json.Marshal(recoveredChief)
		must(t, err)
		if string(chiefJSON) != string(recoveredJSON) {
			t.Fatalf("restart changed chief records: want %s; actual %s", turnSummary(chief), turnSummary(recoveredChief))
		}
		recoveredWorker, err := repository.Thread(stream, designWorkerAgent)
		must(t, err)
		workerJSON, err := json.Marshal(worker)
		must(t, err)
		recoveredWorkerJSON, err := json.Marshal(recoveredWorker)
		must(t, err)
		if string(workerJSON) != string(recoveredWorkerJSON) {
			t.Fatalf("restart changed worker records: want %s; actual %s", turnSummary(worker), turnSummary(recoveredWorker))
		}
		recoveredBrief, err := trace.Get[trace.Document](repository, stream, handed.ID, 1)
		must(t, err)
		briefJSON, err := json.Marshal(handed)
		must(t, err)
		recoveredBriefJSON, err := json.Marshal(recoveredBrief)
		must(t, err)
		if string(briefJSON) != string(recoveredBriefJSON) {
			t.Fatalf("restart changed immutable handed brief: got %+v, want %+v", recoveredBrief, handed)
		}
	}
	restart()
	recoveredInbox, err := c.Inbox(ctx)
	must(t, err)
	if len(recoveredInbox.Entries) != 1 || recoveredInbox.Entries[0].Question != escalationQuestion {
		t.Fatalf("continued design escalation after restart: %+v", recoveredInbox.Entries)
	}
	const ownerRuling = "Keep the module name stable."
	var resumed int
	engine.Mu.Lock()
	engine.Turns["*"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		if !strings.Contains(req.Prompt, ownerRuling) {
			problem("chief ruling prompt lacks the owner's decision: %s", req.Prompt)
		}
		if out, err := callTool(ctx, tools, questions.RelayRulingTool, map[string]any{"question": "1", "text": ownerRuling, "scope": "notify"}); err != nil || !strings.Contains(out, `"recorded":true`) {
			problem("relay ruling: %s %v", out, err)
		}
		return questionResult(req, "session-chief", ownerRuling), nil
	}
	engine.Turns["answer_1"] = func(_ context.Context, req agent.Request, _ *agent.Turn, _ *mcp.ClientSession) (*agent.Result, error) {
		if !strings.Contains(req.Prompt, ownerRuling) {
			problem("resumed worker lacks the owner's ruling: %s", req.Prompt)
		}
		mu.Lock()
		resumed++
		mu.Unlock()
		return questionResult(req, "session-ask", "Continuing with the stable module name."), nil
	}
	engine.Mu.Unlock()
	answer, err := c.Answer(ctx, recoveredInbox.Entries[0].Number, ownerRuling, "")
	must(t, err)
	wantEntries := append(slices.Clone(conversation.Entries), ConversationEntry{Turn: "ruling_" + answer.Batch, Kind: "action", Text: "You ruled: " + ownerRuling, At: answer.At, State: TurnDone})
	ownerConversation, err := c.Conversation(ctx, stream)
	must(t, err)
	if !slices.Equal(ownerConversation.Entries, wantEntries) {
		t.Fatalf("owner answer before relay: want %+v; actual %+v", wantEntries, ownerConversation.Entries)
	}
	clock.Advance(time.Minute)
	waitCompleted("owner ruling relay and worker continuation", len(after.Turns)+1, 2, "idle")
	worker, err := repository.Thread(stream, designWorkerAgent)
	must(t, err)
	if worker.Parked() || len(worker.Turns) != 2 || worker.Turns[1].Request.TurnID != "answer_1" || worker.Turns[1].Response == nil || worker.Turns[1].Status() != "idle" || worker.Turns[1].CompletedAt.IsZero() {
		t.Fatalf("ruling: want exactly one completed answer_1 on asking worker; actual %s", turnSummary(worker))
	}
	answeredInbox, err := c.Inbox(ctx)
	must(t, err)
	if len(answeredInbox.Entries) != 0 {
		t.Fatalf("answered escalation remains in the inbox: %+v", answeredInbox.Entries)
	}
	ruledConversation, err := c.Conversation(ctx, stream)
	must(t, err)
	ruledChief, err := repository.ChiefOfStaffThread(stream)
	must(t, err)
	ruling, err := trace.Get[trace.Ruling](repository, stream, "1", 2)
	must(t, err)
	if ruling.ReturnedAnswer != ownerRuling || ruling.OwnerResponse != ownerRuling || !slices.ContainsFunc(ruledChief.Turns, func(q trace.QueuedTurn) bool {
		return q.Request.ID == ruling.Cause && q.Status() == "idle" && !q.CompletedAt.IsZero() && q.Response != nil && q.Response.Result.FinalResponse == ownerRuling
	}) {
		t.Fatalf("relay: want ruling linked to a completed chief turn on the actual design thread; ruling %+v; chief %s", ruling, turnSummary(ruledChief))
	}
	continuedTurns, err = json.Marshal(ruledChief.Turns[:len(before.Turns)])
	must(t, err)
	if string(originalTurns) != string(continuedTurns) {
		t.Fatal("owner ruling changed the original design request, response or recovery state")
	}
	if !slices.Equal(ruledConversation.Entries, wantEntries) {
		t.Fatalf("owner ruling chat: want %+v; actual %+v; chief %s; worker %s", wantEntries, ruledConversation.Entries, turnSummary(ruledChief), turnSummary(worker))
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
