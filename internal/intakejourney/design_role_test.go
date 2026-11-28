package intakejourney

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/servicetest"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/servicetest/journey"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	designerSkillRef = "https://github.com/kpenfound/skills#design-sketch"
	kindSkillRef     = "https://github.com/kpenfound/skills#hill-climb-evaluator"
)

var managerToolProbes = []string{"file_write", "shell", "git_push", "set_status", "decide", "decide_amendment", "decide_change", "request_change", "prioritise", "pause", "resume", "move_workstream", "route_amendment", "inspect_code", "record_discovery", "factory_context", "project_context", "ask", "preview", "notify"}

func designedFixture(t *testing.T) (*journey.Service, *journey.Client, config.ProjectID, *fixturekit.DemoEngine) {
	t.Helper()
	d := servicetest.PrepareDesignEnforcement(t,
		"[profiles.design]\nagent = \"claude\"\nmodel = \"design-model\"\n[kinds.hill-climb]\nskills = [\""+kindSkillRef+"\"]\n",
		"profile = \"design\"\nskills = [\""+designerSkillRef+"\"]\n")
	d.Engine.Skills = []string{t.TempDir()}
	s, c := journey.StartT(t, journey.Enforce(journey.Options{Options: d.Options}, d.Enforcement))
	return s, c, servicetest.AddDesignProject(t, c.Client, d.Clone), d.Engine
}

func settled(t *testing.T, c *journey.Client, stream config.WorkstreamID, responses int) api.ConversationResponse {
	t.Helper()
	return awaitDesignConversation(t, c, stream, func(r api.ConversationResponse) bool {
		n := 0
		for _, e := range r.Entries {
			if e.Kind == "response" {
				n++
			}
		}
		return n >= responses
	})
}

func TestADesignTurnRunsOnTheDesignerRoleAndItsOwnThreadWithOnlyTheDesignTools(t *testing.T) {
	t.Parallel()
	s, c, home, engine := designedFixture(t)
	ctx := context.Background()
	opened, err := c.OpenDesign(ctx, home)
	fixturekit.Must(t, err)
	stream := opened.Design.Workstream
	repository := s.Sole().Repository
	if manager, err := repository.ManagerThread(stream); err != nil || len(manager.Turns) != 0 {
		t.Fatalf("a new design session's chief-of-staff thread holds turns: %+v %v", manager.Turns, err)
	}
	if th, err := repository.DesignerThread(stream); err != nil || th.Identity.Role != trace.Designer || th.Identity.ThreadID != trace.Designer {
		t.Fatalf("design thread: %+v %v", th.Identity, err)
	}

	var problems []string
	fail := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	engine.Turns["*"] = func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		names, err := intakefixture.ToolNames(ctx, tools)
		if err != nil {
			return nil, err
		}
		if want := []string{"draft_brief", "file_read", "read_repo", "set_title"}; !slices.Equal(names, want) {
			fail("tools %v", names)
		}
		for _, probe := range managerToolProbes {
			if out, err := fixturekit.CallTool(ctx, tools, probe, map[string]any{}); err == nil {
				fail("a designer turn called %s: %s", probe, out)
			}
		}
		if g := req.Grants; g == nil || g.VCS || verified.VCS || !slices.Equal(g.Tools, []string{"Read", "Glob", "Grep", "Skill", "WebSearch", "WebFetch", "mcp__osmia_0"}) {
			fail("native grants %+v", req.Grants)
		}
		if req.SystemPrompt != intake.DesignRolePrompt || strings.Contains(req.Prompt, "Current factory operating policy") {
			fail("the designer's turn carries the chief of staff's policy or a changed system prompt")
		}
		if !slices.Equal(req.Profile.Skills, []string{designerSkillRef}) {
			fail("skills %v, want only the designer's %s", req.Profile.Skills, designerSkillRef)
		}
		if !strings.Contains(req.Prompt, "## Workstream kinds") {
			fail("the designer's turn lacks the workstream kind catalog")
		}
		return &agent.Result{ClaudeID: "design-session", ResultText: "Understood."}, nil
	}
	_, err = c.DesignMessage(ctx, stream, "What should we build?")
	fixturekit.Must(t, err)
	settled(t, c, stream, 1)
	if len(problems) > 0 {
		t.Fatalf("designer turn problems:\n%s", strings.Join(problems, "\n"))
	}

	th, err := repository.DesignerThread(stream)
	fixturekit.Must(t, err)
	if len(th.Turns) != 1 || th.Turns[0].Request.AgentID != trace.Designer || th.Turns[0].Request.Profile.Name != "design" || th.Turns[0].Request.Profile.Model != "design-model" {
		t.Fatalf("design thread turns: %+v", th.Turns)
	}
	if manager, err := repository.ManagerThread(stream); err != nil || len(manager.Turns) != 0 {
		t.Fatalf("a design turn reached the chief-of-staff thread: %+v %v", manager.Turns, err)
	}
}

func TestADesignSessionOpenedBeforeTheDesignerRoleKeepsItsMessagesAndMovesItsNextTurnToTheDesignThread(t *testing.T) {
	t.Parallel()
	s, c, home, engine := designedFixture(t)
	ctx := context.Background()
	opened, err := c.OpenDesign(ctx, home)
	fixturekit.Must(t, err)
	stream := opened.Design.Workstream
	repository := s.Sole().Repository
	now := func() time.Time { return time.Now().UTC() }
	legacy := intakefixture.ClaimManagerTurn(t, repository, home, stream, now, intakefixture.ManagerTurnInput{
		Turn: "message_legacy", Prompt: "Earlier question from the owner", Cause: "owner-message", Token: "legacy-token", Directory: "legacy", Actor: intake.OwnerActor})
	fixturekit.AnswerManager(t, repository, stream, legacy.Turn, "Earlier answer from the agent")
	fixturekit.Must(t, intakefixture.RecordDesignDraft(t, repository, stream, "Decisions: ship it.\nIntent and tradeoffs: keep it simple.\nRejected options: a rewrite."))

	var mu sync.Mutex
	var seen []agent.Request
	var problems []string
	engine.Turns["*"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		mu.Lock()
		seen = append(seen, req)
		mu.Unlock()
		names, err := intakefixture.ToolNames(ctx, tools)
		if err != nil {
			return nil, err
		}
		if want := []string{"draft_brief", "file_read", "read_repo", "set_title"}; !slices.Equal(names, want) {
			mu.Lock()
			problems = append(problems, "tools "+strings.Join(names, ","))
			mu.Unlock()
		}
		for _, probe := range managerToolProbes {
			if _, err := fixturekit.CallTool(ctx, tools, probe, map[string]any{}); err == nil {
				mu.Lock()
				problems = append(problems, "called "+probe)
				mu.Unlock()
			}
		}
		return &agent.Result{ClaudeID: "design-session", ResultText: "Carrying on."}, nil
	}

	before, err := c.DesignConversation(ctx, stream)
	fixturekit.Must(t, err)
	if len(before.Entries) != 2 || before.Entries[0].Text != "Earlier question from the owner" || before.Entries[1].Text != "Earlier answer from the agent" {
		t.Fatalf("the earlier messages are not shown: %+v", before.Entries)
	}
	_, err = c.DesignMessage(ctx, stream, "Carry on")
	fixturekit.Must(t, err)
	conversation := settled(t, c, stream, 2)
	var texts []string
	for _, e := range conversation.Entries {
		texts = append(texts, e.Text)
	}
	if want := []string{"Earlier question from the owner", "Earlier answer from the agent", "Carry on", "Carrying on."}; !slices.Equal(texts, want) {
		t.Fatalf("conversation %q, want %q", texts, want)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(problems) > 0 {
		t.Fatalf("migrated designer turn problems: %v", problems)
	}
	if len(seen) != 1 || seen[0].ResumeID != "" || seen[0].SystemPrompt != intake.DesignRolePrompt {
		t.Fatalf("the first turn on the design thread: %d requests, first %+v", len(seen), seen)
	}
	for _, want := range []string{"Earlier question from the owner", "Earlier answer from the agent", "Decisions: ship it."} {
		if !strings.Contains(seen[0].Prompt, want) {
			t.Fatalf("the first designer turn of a migrated session lacks %q:\n%s", want, seen[0].Prompt)
		}
	}
	design, err := repository.DesignerThread(stream)
	fixturekit.Must(t, err)
	manager, err := repository.ManagerThread(stream)
	fixturekit.Must(t, err)
	if len(design.Turns) != 1 || len(manager.Turns) != 1 {
		t.Fatalf("the new turn is on thread %d turns, the chief's has %d, want 1 and 1", len(design.Turns), len(manager.Turns))
	}
}

func TestStartingAfterDesignGivesTheManagerANewThreadAndCarriesTheKindThrough(t *testing.T) {
	t.Parallel()
	s, c, home, engine := designedFixture(t)
	ctx := context.Background()
	opened, err := c.OpenDesign(ctx, home)
	fixturekit.Must(t, err)
	stream := opened.Design.Workstream
	repository := s.Sole().Repository
	engine.Turns["*"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		out, err := fixturekit.CallTool(ctx, tools, intake.DraftBriefToolName, map[string]any{"content": "Decisions: climb the number.\nIntent and tradeoffs: one project.\nRejected options: a feature plan.", "kind": "hill-climb"})
		if err != nil || !strings.Contains(out, `"recorded":true`) {
			return nil, errors.New("the kind input was refused: " + out)
		}
		return &agent.Result{ClaudeID: "design-session", ResultText: "Drafted a hill-climb brief."}, nil
	}
	_, err = c.DesignMessage(ctx, stream, "Draft it")
	fixturekit.Must(t, err)
	settled(t, c, stream, 1)
	if brief, _, err := intake.LatestDesignBrief(repository, stream); err != nil || brief.BriefKind() != "hill-climb" {
		t.Fatalf("the kind set in the session: %+v %v", brief, err)
	}
	if _, err := c.DesignStart(ctx, stream); err != nil {
		t.Fatal(err)
	}
	if kind, err := repository.Kind(stream); err != nil || kind != "hill-climb" {
		t.Fatalf("the kind Start handed in: %q %v", kind, err)
	}
	manager, err := repository.ManagerThread(stream)
	fixturekit.Must(t, err)
	if manager.Identity.Role != trace.Manager {
		t.Fatalf("chief thread: %+v", manager.Identity)
	}
	for _, q := range manager.Turns {
		if q.Request.AgentID != trace.Manager || q.Request.Prompt == "Draft it" || q.Request.SystemPrompt == intake.DesignRolePrompt {
			t.Fatalf("the design conversation reached the chief's thread: %+v", q.Request)
		}
	}
	design, err := repository.DesignerThread(stream)
	fixturekit.Must(t, err)
	if len(design.Turns) != 1 {
		t.Fatalf("design thread turns: %d", len(design.Turns))
	}
}

func TestADesignTurnWithoutADesignerRoleRunsOnTheDefaultProfileWhicheverManagerKeyIsSet(t *testing.T) {
	t.Parallel()
	managerSettings := "profile = \"chief\"\nsandbox = \"container\"\nimage = \"chief-image\"\nskills = [\"" + designerSkillRef + "\"]\n"
	for name, roles := range map[string]string{
		"canonical key":    "[roles.manager]\n" + managerSettings,
		"deprecated alias": "[roles.chief_of_staff]\n" + managerSettings,
		"no manager role":  "",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			d := servicetest.PrepareDesignEnforcementRoles(t, "[profiles.chief]\nagent = \"claude\"\nmodel = \"chief-model\"\n", roles)
			s, c := journey.StartT(t, journey.Enforce(journey.Options{Options: d.Options}, d.Enforcement))
			home := servicetest.AddDesignProject(t, c.Client, d.Clone)
			ctx := context.Background()
			opened, err := c.OpenDesign(ctx, home)
			fixturekit.Must(t, err)
			stream := opened.Design.Workstream
			_, err = c.DesignMessage(ctx, stream, "What should we build?")
			fixturekit.Must(t, err)
			th, err := s.Sole().Repository.DesignerThread(stream)
			fixturekit.Must(t, err)
			if len(th.Turns) != 1 || th.Turns[0].Request.Profile.Name != "default" || th.Turns[0].Request.Profile.Model == "chief-model" {
				t.Fatalf("the design turn's profile: %+v", th.Turns)
			}
			if manager, err := s.Sole().Repository.ManagerThread(stream); err != nil || len(manager.Turns) != 0 {
				t.Fatalf("the design turn reached the manager's thread: %+v %v", manager.Turns, err)
			}
		})
	}
}

func TestStartingFromASessionOpenedBeforeTheDesignerRoleNeverResumesItsBackendSession(t *testing.T) {
	t.Parallel()
	s, c, home, engine := designedFixture(t)
	ctx := context.Background()
	engine.Resume = func(coreadapter.Profile, coreadapter.Profile, coreadapter.BackendSession) error { return nil }
	opened, err := c.OpenDesign(ctx, home)
	fixturekit.Must(t, err)
	stream := opened.Design.Workstream
	repository := s.Sole().Repository
	now := func() time.Time { return time.Now().UTC() }
	legacy := intakefixture.ClaimManagerTurn(t, repository, home, stream, now, intakefixture.ManagerTurnInput{
		Turn: "message_legacy", Prompt: "Earlier question from the owner", Cause: "owner-message", Token: "legacy-token", Directory: "legacy", Actor: intake.OwnerActor})
	fixturekit.AnswerManager(t, repository, stream, legacy.Turn, "Earlier answer from the agent")
	fixturekit.Must(t, intakefixture.RecordDesignDraft(t, repository, stream, "Decisions: ship it.\nIntent and tradeoffs: keep it simple.\nRejected options: a rewrite."))

	var mu sync.Mutex
	var resumed []string
	var replayed []string
	engine.Turns["*"] = func(_ context.Context, req agent.Request, _ *agent.Turn, _ *mcp.ClientSession) (*agent.Result, error) {
		mu.Lock()
		defer mu.Unlock()
		if req.ResumeID == "legacy-token" {
			resumed = append(resumed, req.Name)
		}
		if strings.Contains(req.Prompt, "osmia-owned-log") && strings.Contains(req.Prompt, "Earlier question from the owner") {
			replayed = append(replayed, req.Name)
		}
		return &agent.Result{ClaudeID: "chief-session", ResultText: "Carrying on."}, nil
	}
	if _, err := c.DesignStart(ctx, stream); err != nil {
		t.Fatal(err)
	}
	s.Until(t, "the chief of staff accepts a message once Start has settled", func() bool {
		_, err := c.Send(ctx, stream, "What is next?")
		return err == nil
	})
	s.Until(t, "the chief of staff answered after Start", func() bool {
		manager, err := repository.ManagerThread(stream)
		fixturekit.Must(t, err)
		return slices.ContainsFunc(manager.Turns, func(q trace.QueuedTurn) bool {
			return q.Request.Prompt == "What is next?" && q.Response != nil && !q.CompletedAt.IsZero()
		})
	})
	mu.Lock()
	defer mu.Unlock()
	if len(resumed) > 0 || len(replayed) > 0 {
		t.Fatalf("a chief turn after Start resumed the design session's backend session %v or replayed its conversation %v", resumed, replayed)
	}
	manager, err := repository.ManagerThread(stream)
	fixturekit.Must(t, err)
	if manager.Epoch == 0 {
		t.Fatalf("the chief's thread keeps the design conversation in its current run: %+v", manager.Identity)
	}
}
