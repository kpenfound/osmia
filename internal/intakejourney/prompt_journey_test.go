package intakejourney

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

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

const promptJourneyEvents = "[events]\nwindow = \"100ms\"\nprogress_window = \"2h\"\n"

var (
	designToolNames   = []string{"draft_brief", "file_read", "read_repo", "set_title"}
	designNativeTools = []string{"Read", "Glob", "Grep", "Skill", "WebSearch", "WebFetch", "mcp__osmia_0"}
)

// journeyProblems collects what a fake agent saw wrong while it ran, which it
// cannot report by failing the test from its own goroutine.
type journeyProblems struct {
	mu   sync.Mutex
	list []string
}

func (p *journeyProblems) add(format string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.list = append(p.list, fmt.Sprintf(format, args...))
}

func (p *journeyProblems) check(t *testing.T) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.list) > 0 {
		t.Fatalf("the fake agents saw:\n%s", strings.Join(p.list, "\n"))
	}
}

func TestADesignSessionKeepsItsPromptThroughBriefChangesARestartAndStartAndTheChiefBeginsAnew(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	prepared := servicetest.PrepareDesignEnforcement(t,
		"[profiles.design]\nagent = \"claude\"\nmodel = \"design-model\"\n[kinds.hill-climb]\nskills = [\""+kindSkillRef+"\"]\n",
		"profile = \"design\"\nskills = [\""+designerSkillRef+"\"]\n"+promptJourneyEvents)
	engine := prepared.Engine
	engine.Skills = []string{t.TempDir()}
	engine.Resume = func(coreadapter.Profile, coreadapter.Profile, coreadapter.BackendSession) error { return nil }
	log := fixturekit.NewPromptLog(engine)
	s, c := journey.StartT(t, journey.Enforce(journey.Options{Options: prepared.Options}, prepared.Enforcement))
	home := servicetest.AddDesignProject(t, c.Client, prepared.Clone)
	other := addDesignProject(t, c, "client")
	opened, err := c.OpenDesign(ctx, home)
	fixturekit.Must(t, err)
	stream := opened.Design.Workstream
	repo, err := s.Repository(home)
	fixturekit.Must(t, err)
	d := &designProjects{s: s, c: c, engine: engine, repo: repo, stream: stream, home: home, other: other}

	content := "Decisions: climb the parse time.\nIntent and tradeoffs: one project, a measurable goal.\nRejected options: a feature plan."
	problems := &journeyProblems{}
	engine.Mu.Lock()
	engine.Turns["*"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		if strings.Contains(req.Prompt, intake.HandoffTitleOpening) {
			return intakefixture.QuestionResult(req, "chief-session", "Titled."), nil
		}
		if req.SystemPrompt != intake.DesignRolePrompt {
			return intakefixture.QuestionResult(req, "other-session", "Nothing to do."), nil
		}
		names, err := intakefixture.ToolNames(ctx, tools)
		if err != nil {
			return nil, err
		}
		if !slices.Equal(names, designToolNames) {
			problems.add("%s: design tools %v", req.Name, names)
		}
		for _, probe := range chiefToolProbes {
			if out, err := fixturekit.CallTool(ctx, tools, probe, map[string]any{}); err == nil {
				problems.add("%s: a designer turn called %s: %s", req.Name, probe, out)
			}
		}
		if strings.HasSuffix(req.Prompt, "message 5") {
			out, err := fixturekit.CallTool(ctx, tools, intake.DraftBriefToolName, map[string]any{"content": content, "kind": "hill-climb", "projects": []string{string(home)}, "new_project": nil})
			if err != nil || !strings.Contains(out, `"recorded":true`) {
				problems.add("%s: draft_brief: %s %v", req.Name, out, err)
			}
		}
		return &agent.Result{ClaudeID: "design-session", ResultText: "Reply to " + req.Prompt[strings.LastIndex(req.Prompt, "message"):], SessionDir: req.SessionDir, NumTurns: 1}, nil
	}
	engine.Mu.Unlock()

	otherRow := func(listed bool) string { return fmt.Sprintf("%s | client | owner/client | %v |", other, listed) }
	record := func(record intake.DesignBriefRecord) {
		recordDesignBrief(t, d, record)
	}
	steps := []struct {
		change func()
		want   []string
		not    []string
	}{
		{func() { record(intake.DesignBriefRecord{Content: content}) },
			[]string{"Current kind of the brief: feature", otherRow(false)}, []string{"New project in the current brief"}},
		{func() { record(intake.DesignBriefRecord{Content: content, Projects: []config.ProjectID{home, other}}) },
			[]string{"Current kind of the brief: feature", otherRow(true)}, []string{"New project in the current brief"}},
		{func() {
			options := s.Options
			c.Close()
			fixturekit.Must(t, s.Close())
			s, c = journey.StartT(t, options)
			repo, err = s.Repository(home)
			fixturekit.Must(t, err)
			d.s, d.c, d.repo = s, c, repo
			record(intake.DesignBriefRecord{Content: content, Projects: []config.ProjectID{home}, Kind: "hill-climb"})
		}, []string{"Current kind of the brief: hill-climb", otherRow(false)}, []string{otherRow(true), "New project in the current brief"}},
		{func() {
			record(intake.DesignBriefRecord{Content: content, Projects: []config.ProjectID{home, other}, Kind: "feature",
				NewProject: &api.NewProject{Name: "Gadget", Repo: "gadget", Visibility: "private", Description: "A gadget.", Charter: "1. Keep it small.\n"}})
		}, []string{"Current kind of the brief: feature", "New project in the current brief", "Name: Gadget", otherRow(true)}, nil},
		{func() {}, []string{"Current kind of the brief: feature", "New project in the current brief", "Name: Gadget"}, nil},
	}
	for i, step := range steps {
		step.change()
		d.say(t, fmt.Sprintf("message %d", i+1), 2*(i+1))
		turns := log.Turns()
		turn := turns[len(turns)-1]
		if !strings.HasSuffix(turn.Prompt, fmt.Sprintf("message %d", i+1)) {
			t.Fatalf("turn %d: the last turn the engine ran is %q:\n%s", i+1, turn.Name, turn.Prompt)
		}
		for _, want := range append(step.want, "## Workstream kinds", coreadapter.TurnContextHeader) {
			if !strings.Contains(turn.Prompt, want) {
				t.Fatalf("turn %d: the newest turn lacks %q:\n%s", i+1, want, turn.Prompt)
			}
		}
		for _, not := range step.not {
			if strings.Contains(turn.Prompt, not) {
				t.Fatalf("turn %d: the newest turn still carries %q:\n%s", i+1, not, turn.Prompt)
			}
		}
	}
	problems.check(t)

	designThread, err := repo.DesignerThread(stream)
	fixturekit.Must(t, err)
	if designThread.Identity.Role != trace.Designer || designThread.Identity.ThreadID != trace.Designer || len(designThread.Turns) != len(steps) {
		t.Fatalf("the design thread: %+v with %d turns, want %d", designThread.Identity, len(designThread.Turns), len(steps))
	}
	if chief, err := repo.ChiefOfStaffThread(stream); err != nil || len(chief.Turns) != 0 {
		t.Fatalf("a design turn reached the chief of staff's thread: %+v %v", chief.Turns, err)
	}
	var first fixturekit.LoggedTurn
	for i, q := range designThread.Turns {
		turn, ok := log.Turn(q.Request.TurnID)
		if !ok {
			t.Fatalf("design turn %s never reached the engine", q.Request.TurnID)
		}
		if i == 0 {
			first = turn
		}
		if q.Request.AgentID != trace.Designer || q.Request.ThreadID != trace.Designer || q.Request.Profile.Name != "design" || turn.Model != "design-model" {
			t.Errorf("design turn %d ran as agent %q on thread %q with profile %q (%q)", i+1, q.Request.AgentID, q.Request.ThreadID, q.Request.Profile.Name, turn.Model)
		}
		if !slices.Equal(turn.Skills, []string{designerSkillRef}) {
			t.Errorf("design turn %d carried skills %v, want only the designer's", i+1, turn.Skills)
		}
		if !slices.Equal(turn.Tools, designToolNames) || !slices.Equal(turn.GrantedTools, designNativeTools) || turn.VCS {
			t.Errorf("design turn %d was offered tools %v, native %v, vcs %v", i+1, turn.Tools, turn.GrantedTools, turn.VCS)
		}
		if (i == 0) != (turn.ResumeID == "") || (i > 0 && turn.ResumeID != "design-session") {
			t.Errorf("design turn %d resumed %q", i+1, turn.ResumeID)
		}
		if turn.SystemPrompt != intake.DesignRolePrompt || turn.SystemPrompt != first.SystemPrompt {
			t.Errorf("design turn %d did not keep the system prompt its session began with", i+1)
		}
		if strings.Contains(turn.SystemPrompt, "Current kind of the brief") || strings.Contains(turn.SystemPrompt, "## Workstream kinds") || strings.Contains(turn.Prompt, "## Operating policy") {
			t.Errorf("design turn %d holds changing context in its system prompt or the chief's policy in its prompt", i+1)
		}
	}

	if _, err := c.DesignStart(ctx, stream); err != nil {
		t.Fatal(err)
	}
	s.Until(t, "the chief of staff to settle the hand-off turn", func() bool {
		th, err := repo.ChiefOfStaffThread(stream)
		fixturekit.Must(t, err)
		return slices.ContainsFunc(th.Turns, func(q trace.QueuedTurn) bool {
			return strings.Contains(q.Request.Prompt, intake.HandoffTitleOpening) && q.Response != nil && !q.CompletedAt.IsZero()
		})
	})
	if kind, err := repo.Kind(stream); err != nil || kind != "hill-climb" {
		t.Fatalf("the kind Start handed in: %q %v, want the one the session set", kind, err)
	}
	problems.check(t)

	handed, err := trace.Get[trace.Document](repo, stream, intake.HandedDocument, 1)
	fixturekit.Must(t, err)
	for _, want := range []string{content, intake.DesignTranscriptHeading, "Owner: message 1", "Agent: Reply to message 1", "Owner: message 5", "Agent: Reply to message 5"} {
		if !strings.Contains(handed.Content, want) {
			t.Fatalf("the handed document lacks %q:\n%s", want, handed.Content)
		}
	}
	chief, err := repo.ChiefOfStaffThread(stream)
	fixturekit.Must(t, err)
	if chief.Identity.Role != trace.ChiefOfStaff || chief.Identity.ThreadID != trace.ChiefOfStaff || chief.Identity.ID == designThread.Identity.ID {
		t.Fatalf("the chief's thread %+v is not its own, apart from the design thread %+v", chief.Identity, designThread.Identity)
	}
	opening := chief.Turns[0].Request
	seeds := append(strings.Split(content, "\n"), intake.HandoffTitleOpening, "## Transcript", "Owner: message 1", "Agent: Reply to message 1", "Owner: message 5", "Agent: Reply to message 5")
	for _, want := range seeds {
		if !strings.Contains(opening.Prompt, want) {
			t.Fatalf("the chief's first turn is not seeded from the brief and the transcript, it lacks %q:\n%s", want, opening.Prompt)
		}
	}
	if opening.AgentID != trace.ChiefOfStaff || opening.SystemPrompt == intake.DesignRolePrompt {
		t.Fatalf("the chief's first turn: agent %q, designer prompt %v", opening.AgentID, opening.SystemPrompt == intake.DesignRolePrompt)
	}
	for _, q := range chief.Turns {
		turn, ok := log.Turn(q.Request.TurnID)
		if !ok {
			continue
		}
		if turn.ResumeID == "design-session" || turn.SystemPrompt == intake.DesignRolePrompt || turn.Model == "design-model" || slices.Contains(turn.Skills, designerSkillRef) || slices.Contains(turn.Skills, kindSkillRef) {
			t.Errorf("chief turn %s took up the design session: resume %q model %q skills %v", turn.Name, turn.ResumeID, turn.Model, turn.Skills)
		}
		if turn.ResumeID != "" && turn.ResumeID != "chief-session" {
			t.Errorf("chief turn %s resumed %q", turn.Name, turn.ResumeID)
		}
		if slices.Contains(turn.Tools, intake.DraftBriefToolName) || !slices.Contains(turn.Tools, "decide") && !slices.Contains(turn.Tools, "set_status") {
			t.Errorf("chief turn %s was offered %v, not the chief's tools", turn.Name, turn.Tools)
		}
		if !strings.Contains(turn.Prompt, "## Operating policy") || strings.Contains(turn.SystemPrompt, "## Operating policy") {
			t.Errorf("chief turn %s does not carry the current policy in its newest turn only", turn.Name)
		}
	}
	if after, err := repo.DesignerThread(stream); err != nil || len(after.Turns) != len(designThread.Turns) {
		t.Fatalf("Start changed the design thread: %d turns, %v", len(after.Turns), err)
	}

	results := fixturekit.ToolResults(t, repo, stream)
	if !slices.ContainsFunc(results, func(r fixturekit.LoggedText) bool { return strings.Contains(r.Text, `"recorded":true`) }) {
		t.Fatalf("the trace holds no result of the draft_brief call: %v", results)
	}
	texts := append(log.Texts(), results...)
	if len(texts) < 3*len(steps) {
		t.Fatalf("the journey's agents received only %d texts", len(texts))
	}
	fixturekit.AssertNamesNoSkill(t, texts)
}
