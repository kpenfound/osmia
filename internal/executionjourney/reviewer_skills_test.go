package executionjourney

import (
	context "context"
	json "encoding/json"
	fmt "fmt"
	io "io"
	slog "log/slog"
	os "os"
	filepath "path/filepath"
	regexp "regexp"
	slices "slices"
	strings "strings"
	testing "testing"
	time "time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	agent "github.com/kpenfound/agentcore/agent"
	config "github.com/kpenfound/osmia/internal/config"
	coreadapter "github.com/kpenfound/osmia/internal/coreadapter"
	plan "github.com/kpenfound/osmia/internal/plan"
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	skills "github.com/kpenfound/osmia/internal/skills"
	thread "github.com/kpenfound/osmia/internal/thread"
	trace "github.com/kpenfound/osmia/internal/trace"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	fakeSkillRef      = "https://github.com/kpenfound/skills#fake-skill"
	fakeSkillRepo     = "https://github.com/kpenfound/skills"
	fakeSkillMarkdown = "---\nname: fake-skill\ndescription: Reviews a candidate the fake way.\n---\nBefore you review, read references/checklist.md.\n"
	fakeChecklist     = "1. The candidate keeps its promises.\n"
	notGivenMarkdown  = "---\nname: other-skill\ndescription: A skill the reviewer was not given.\n---\n"

	codexReviewerRoles = intakefixture.ChiefRole + "[roles.mason]\nsandbox = \"container\"\nimage = \"fixture-image\"\n[roles.reviewer]\nprofile = \"other\"\nsandbox = \"container\"\nimage = \"fixture-image\"\nskills = [\"" + fakeSkillRef + "\"]\n"
)

type skillClones map[string]string

func (c skillClones) run(_ context.Context, _ string, args ...string) (string, error) {
	if args[0] != "clone" {
		return "", nil
	}
	url, dest := args[len(args)-2], args[len(args)-1]
	source, ok := c[url]
	if !ok {
		return "", fmt.Errorf("no skill fixture for %s", url)
	}
	return "", os.CopyFS(dest, os.DirFS(source))
}

type skillEngine struct {
	*fixturekit.DemoEngine
	manager *skills.Manager
}

var _ coreadapter.SkillPreparing = skillEngine{}

func (e skillEngine) SkillDirs() []string { return []string{e.manager.Dir} }

func (e skillEngine) PrepareSkills(ctx context.Context, refs []string) ([]string, error) {
	return e.manager.Prepare(ctx, refs)
}

func putFile(t *testing.T, path, content string) {
	t.Helper()
	fixturekit.Must(t, os.MkdirAll(filepath.Dir(path), 0700))
	fixturekit.Must(t, os.WriteFile(path, []byte(content), 0600))
}

func newSkillReviewFixture(t *testing.T, key string) (*journey.ShedFixture, config.WorkstreamID, *trace.Repository, *skills.Manager) {
	t.Helper()
	repo := t.TempDir()
	putFile(t, filepath.Join(repo, "fake-skill", "SKILL.md"), fakeSkillMarkdown)
	putFile(t, filepath.Join(repo, "fake-skill", "references", "checklist.md"), fakeChecklist)
	putFile(t, filepath.Join(repo, "other-skill", "SKILL.md"), notGivenMarkdown)
	putFile(t, filepath.Join(repo, ".git", "HEAD"), "")
	manager := skills.NewManager(t.TempDir())
	manager.Git = skillClones{fakeSkillRepo: repo}.run
	manager.Refresh = func() string { return "never" }
	manager.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	f := journey.NewDebateFixtureWith(t, 1, 1, codexReviewerRoles, func(opts *journey.Options) {
		opts.WriteTimeout = time.Minute
		engine := skillEngine{opts.Committee.Engine.(*fixturekit.DemoEngine), manager}
		enforced := journey.Enforce(*opts, service.Enforcement{Engine: engine, Hosts: opts.Committee.Hosts})
		opts.Threads, opts.Controls = enforced.Threads, enforced.Controls
		opts.Threads = intakefixture.ConfigureMasonThreads(t, opts.Config.Root, config.WorkspacesGit, "masons = 1\n", opts.Threads)
	})
	f.Script("draft-1-1", map[string]string{plan.SpecPath: fixturekit.ValidSpec, plan.PlanPath: intakefixture.IndependentPlan}, nil)
	f.Upstream(t)
	intakefixture.ScriptMasons(f.Engine, execution.MasonTurnID)
	base := strings.TrimSpace(fixturekit.DemoGit(t, f.Clone, "-C", f.Clone, "rev-parse", "HEAD"))
	stream, repository := journey.SeedBuild(t, f, key, intakefixture.IndependentPlan, config.WorkspacesGit, base)
	journey.SeedReview(t, f, repository, stream, "resume", journey.ResumeReport)
	return f, stream, repository, manager
}

func skillFile(t *testing.T, plugin, name string) string {
	t.Helper()
	for _, pattern := range []string{filepath.Join(plugin, "skills", name), filepath.Join(plugin, "skills", "*", name)} {
		matches, err := filepath.Glob(pattern)
		fixturekit.Must(t, err)
		if len(matches) == 1 {
			return matches[0]
		}
	}
	t.Fatalf("plugin %s holds no %s", plugin, name)
	return ""
}

type reviewerReads struct {
	request  agent.Request
	skillMD  string
	checked  string
	view     string
	refusals map[string]string
}

func TestCodexUnitReviewerGetsAndReadsItsSkills(t *testing.T) {
	t.Parallel()
	f, stream, repository, manager := newSkillReviewFixture(t, "codex-skills")
	ctx := context.Background()
	plugins, err := manager.Prepare(ctx, []string{fakeSkillRef})
	fixturekit.Must(t, err)
	if len(plugins) != 1 {
		t.Fatalf("prepared skill directories %v", plugins)
	}
	skillMD := skillFile(t, plugins[0], "SKILL.md")
	mentioned := regexp.MustCompile(`references/[\w/-]+\.md`).FindString(fakeSkillMarkdown)
	checklist := filepath.Join(filepath.Dir(skillMD), filepath.FromSlash(mentioned))
	notGiven, err := filepath.Glob(filepath.Join(manager.Dir, "repos", "*", "other-skill", "SKILL.md"))
	fixturekit.Must(t, err)
	if len(notGiven) != 1 {
		t.Fatalf("the clone holds %v for the skill the role was not given", notGiven)
	}
	secret := filepath.Join(t.TempDir(), "secret.txt")
	putFile(t, secret, "outside every directory the turn may read")
	refused := map[string]string{
		"a skill the role was not given":    notGiven[0],
		"an absolute path outside the view": secret,
		"the skill cache":                   manager.Dir,
		"the plugin beside its skills":      filepath.Join(plugins[0], ".claude-plugin", "plugin.json"),
		"traversal out of the skill":        filepath.Dir(skillMD) + strings.Repeat("/..", 16) + secret,
		"traversal out of the view":         "../secret.txt",
	}

	r := &journey.Reviewers{Masons: journey.NewMasonController(f.S, repository)}
	state, err := repository.Workflow(stream, trace.UnitSubject("resume"))
	fixturekit.Must(t, err)
	fixturekit.Must(t, r.One(ctx, stream, "resume", state, false))
	reviewer := intake.ReviewerAgent("resume")
	th, err := repository.Thread(stream, reviewer)
	fixturekit.Must(t, err)
	if len(th.Turns) != 1 {
		t.Fatalf("review turns %+v", th.Turns)
	}
	queued := th.Turns[0].Request

	read := func(ctx context.Context, tools *mcp.ClientSession, path string) (string, error) {
		body, err := fixturekit.CallTool(ctx, tools, "file_read", map[string]any{"path": path})
		if err != nil {
			return "", err
		}
		var text string
		if err := json.Unmarshal([]byte(body), &text); err != nil {
			return "", fmt.Errorf("file_read of %s returned %q: %w", path, body, err)
		}
		return text, nil
	}
	seen := &reviewerReads{refusals: map[string]string{}}
	f.Engine.Mu.Lock()
	f.Engine.Turns[queued.TurnID] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		seen.request = req
		var err error
		if seen.skillMD, err = read(ctx, tools, skillMD); err != nil {
			return nil, err
		}
		if seen.checked, err = read(ctx, tools, checklist); err != nil {
			return nil, err
		}
		if seen.view, err = read(ctx, tools, fixturekit.MasonWrote); err != nil {
			return nil, err
		}
		for name, path := range refused {
			text, err := read(ctx, tools, path)
			if err == nil {
				seen.refusals[name] = "read: " + text
				continue
			}
			seen.refusals[name] = ""
		}
		v := service.UnitVerdict{Decision: "satisfactory", Summary: fixturekit.ReviewSummary, Findings: []api.ReviewFinding{}}
		body, err := fixturekit.CallTool(ctx, tools, execution.VerdictTool, map[string]any{"decision": v.Decision, "summary": v.Summary, "findings": v.Findings})
		if err != nil || !strings.Contains(body, `"recorded":true`) {
			return nil, fmt.Errorf("verdict %s: %v", body, err)
		}
		return &agent.Result{ClaudeID: req.Name, ResultText: "Reviewed", SessionDir: req.SessionDir, NumTurns: 1}, nil
	}
	f.Engine.Mu.Unlock()

	bound, err := f.S.Options.Threads(repository, f.S.Cfg)
	fixturekit.Must(t, err)
	op, err := thread.TurnOperation(f.Project, "codex-skills-review", thread.TurnInput{Workstream: stream, Agent: reviewer, Turn: queued.TurnID})
	fixturekit.Must(t, err)
	if _, err := bound.Apply(ctx, op); err != nil {
		t.Fatalf("the review turn: %v", err)
	}
	req := seen.request
	if req.Name != queued.TurnID {
		t.Fatalf("the engine ran %q, not the review turn %q", req.Name, queued.TurnID)
	}

	if req.Profile.Agent != agent.AgentCodex {
		t.Errorf("the reviewer ran on %q, not Codex", req.Profile.Agent)
	}
	if !slices.Equal(req.Profile.Skills, []string{fakeSkillRef}) {
		t.Errorf("request skills %v, want the role's %q", req.Profile.Skills, fakeSkillRef)
	}
	if req.Grants == nil || !slices.Contains(req.Grants.Mounts, agent.Mount{Path: manager.Dir, Access: agent.ReadOnly}) {
		t.Errorf("the skill cache is not mounted read-only: %+v", req.Grants)
	}
	if req.Grants != nil {
		for _, m := range req.Grants.Mounts {
			if m.Path == manager.Dir && m.Access != agent.ReadOnly {
				t.Errorf("the skill cache is mounted %v", m.Access)
			}
		}
		if slices.Contains(req.Grants.Tools, "Skill") {
			t.Errorf("a Codex turn holds Claude's Skill tool: %v", req.Grants.Tools)
		}
		for _, tool := range []string{"Write", "Edit", "Bash", "apply_patch", "shell", "edit", "bash"} {
			if slices.Contains(req.Grants.Tools, tool) {
				t.Errorf("the reviewer holds %s: %v", tool, req.Grants.Tools)
			}
		}
	}
	if slices.Contains(req.Profile.AllowedTools, "Skill") {
		t.Errorf("a Codex turn is allowed Claude's Skill tool: %v", req.Profile.AllowedTools)
	}

	if !strings.HasPrefix(req.SystemPrompt, queued.SystemPrompt) {
		t.Errorf("the engine's system prompt is not the enqueued one:\n%s", req.SystemPrompt)
	}
	intakefixture.AssertCheckGateReviewInstructions(t, req.SystemPrompt)
	for _, want := range []string{
		"You are the unit reviewer",
		"Review the exact candidate identity your prompt gives",
		"workstream_diff", "ask puts a question to the chief of staff when a decision is needed", "verdict records your decision",
		"A finding is material, and the verdict is material_findings, when the task or an acceptance item does not hold",
		"when it breaks a written rule", "never blocks", "Formatting and lint belong to the checks",
		"Instructions loaded for your role add to this prompt",
		"this prompt wins",
	} {
		if !strings.Contains(req.SystemPrompt, want) {
			t.Errorf("system prompt lacks %q:\n%s", want, req.SystemPrompt)
		}
	}
	for _, gone := range []string{"Code style, formatting and lint belong to the checks, not", "A finding is material only when"} {
		if strings.Contains(req.SystemPrompt, gone) {
			t.Errorf("system prompt keeps the old wording %q", gone)
		}
	}

	if seen.skillMD != fakeSkillMarkdown {
		t.Errorf("file_read of %s returned %q", skillMD, seen.skillMD)
	}
	if seen.checked != fakeChecklist {
		t.Errorf("file_read of %s returned %q", checklist, seen.checked)
	}
	if !strings.Contains(seen.view, "package trace") {
		t.Errorf("file_read of the candidate's %s in the view returned %q", fixturekit.MasonWrote, seen.view)
	}
	for name := range refused {
		if got, ok := seen.refusals[name]; !ok || got != "" {
			t.Errorf("file_read of %s was not refused: %q", name, got)
		}
	}
	if strings.Contains(fmt.Sprint(seen.refusals), "outside every directory") {
		t.Errorf("a refused read leaked a file: %v", seen.refusals)
	}

	done, err := repository.Thread(stream, reviewer)
	fixturekit.Must(t, err)
	if out := done.Turns[0].Response; out == nil || out.Result.Outcome == nil || out.Result.Outcome.Status != execution.VerdictOutcome {
		t.Fatalf("the review turn recorded %+v, not a verdict", out)
	}
}
