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

	// codexReviewerRoles runs the unit reviewer on the Codex profile of the
	// fixture's configuration, with the one fake skill.
	codexReviewerRoles = chiefRole + "[roles.mason]\nsandbox = \"container\"\nimage = \"fixture-image\"\n[roles.reviewer]\nprofile = \"other\"\nsandbox = \"container\"\nimage = \"fixture-image\"\nskills = [\"" + fakeSkillRef + "\"]\n"
)

// skillClones stands in for git: it clones a repository by copying the fixture
// directory its URL names, so preparing skills reaches no network.
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

// skillEngine is the fixture's fake engine with the skill preparation of the
// service's real skill manager, which clones from skillClones.
type skillEngine struct {
	*demoEngine
	manager *skills.Manager
}

var _ coreadapter.SkillPreparing = skillEngine{}

func (e skillEngine) SkillDirs() []string { return []string{e.manager.Dir} }

func (e skillEngine) PrepareSkills(ctx context.Context, refs []string) ([]string, error) {
	return e.manager.Prepare(ctx, refs)
}

func putFile(t *testing.T, path, content string) {
	t.Helper()
	must(t, os.MkdirAll(filepath.Dir(path), 0700))
	must(t, os.WriteFile(path, []byte(content), 0600))
}

// newSkillReviewFixture is newReviewFixture for a reviewer role that runs on
// Codex with fakeSkillRef. Its skill cache holds the clone of a repository with
// two skills, only one of which the role is given.
func newSkillReviewFixture(t *testing.T, key string) (*shedFixture, config.WorkstreamID, *trace.Repository, *skills.Manager) {
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
	f := newDebateFixtureWith(t, 1, 1, codexReviewerRoles, func(opts *Options) {
		opts.WriteTimeout = time.Minute
		engine := skillEngine{opts.Committee.Engine.(*demoEngine), manager}
		enforced := Enforce(*opts, Enforcement{Engine: engine, Hosts: opts.Committee.Hosts})
		opts.Threads, opts.controls = enforced.Threads, enforced.controls
		opts.Threads = intakefixture.ConfigureMasonThreads(t, opts.Config.Root, config.WorkspacesGit, "masons = 1\n", opts.Threads)
	})
	f.script("draft-1-1", map[string]string{plan.SpecPath: validSpec, plan.PlanPath: independentPlan}, nil)
	f.upstream(t)
	intakefixture.ScriptMasons(f.engine, masonTurnID)
	base := strings.TrimSpace(demoGit(t, f.clone, "-C", f.clone, "rev-parse", "HEAD"))
	stream, repository := seedBuild(t, f, key, independentPlan, config.WorkspacesGit, base)
	seedReview(t, f, repository, stream, "resume", resumeReport)
	return f, stream, repository, manager
}

// skillFile returns the path of name in the one skill a prepared plugin holds.
func skillFile(t *testing.T, plugin, name string) string {
	t.Helper()
	for _, pattern := range []string{filepath.Join(plugin, "skills", name), filepath.Join(plugin, "skills", "*", name)} {
		matches, err := filepath.Glob(pattern)
		must(t, err)
		if len(matches) == 1 {
			return matches[0]
		}
	}
	t.Fatalf("plugin %s holds no %s", plugin, name)
	return ""
}

// reviewerReads is what the reviewer's turn saw through its own tools.
type reviewerReads struct {
	request  agent.Request
	skillMD  string
	checked  string
	view     string
	refusals map[string]string
}

// A unit review turn of a reviewer role that runs on Codex and configures a
// skill is run through the service's own turn path: the role's configuration,
// the review enqueued for the candidate, the thread dispatcher, the isolation
// that selects the turn's view and tools, and the execution boundary, into a
// fake engine. The request that reaches the engine carries the role's skill,
// the skill cache mounted read-only and the unit reviewer's system prompt, and
// the turn's own file_read reads the skill's SKILL.md and the file it references
// at their mounted paths, and refuses every path outside the view and the
// skill's directory.
func TestCodexUnitReviewerGetsAndReadsItsSkills(t *testing.T) {
	t.Parallel()
	f, stream, repository, manager := newSkillReviewFixture(t, "codex-skills")
	ctx := context.Background()
	plugins, err := manager.Prepare(ctx, []string{fakeSkillRef})
	must(t, err)
	if len(plugins) != 1 {
		t.Fatalf("prepared skill directories %v", plugins)
	}
	skillMD := skillFile(t, plugins[0], "SKILL.md")
	mentioned := regexp.MustCompile(`references/[\w/-]+\.md`).FindString(fakeSkillMarkdown)
	checklist := filepath.Join(filepath.Dir(skillMD), filepath.FromSlash(mentioned))
	notGiven, err := filepath.Glob(filepath.Join(manager.Dir, "repos", "*", "other-skill", "SKILL.md"))
	must(t, err)
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

	r := &reviewers{masons: newMasonController(f.s, repository)}
	state, err := repository.Workflow(stream, trace.UnitSubject("resume"))
	must(t, err)
	must(t, r.one(ctx, stream, "resume", state, false))
	reviewer := reviewerAgent("resume")
	th, err := repository.Thread(stream, reviewer)
	must(t, err)
	if len(th.Turns) != 1 {
		t.Fatalf("review turns %+v", th.Turns)
	}
	queued := th.Turns[0].Request

	read := func(ctx context.Context, tools *mcp.ClientSession, path string) (string, error) {
		body, err := callTool(ctx, tools, "file_read", map[string]any{"path": path})
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
	f.engine.Mu.Lock()
	f.engine.Turns[queued.TurnID] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		seen.request = req
		var err error
		if seen.skillMD, err = read(ctx, tools, skillMD); err != nil {
			return nil, err
		}
		if seen.checked, err = read(ctx, tools, checklist); err != nil {
			return nil, err
		}
		if seen.view, err = read(ctx, tools, masonWrote); err != nil {
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
		v := UnitVerdict{Decision: "satisfactory", Summary: reviewSummary, Findings: []ReviewFinding{}}
		body, err := callTool(ctx, tools, verdictTool, map[string]any{"decision": v.Decision, "summary": v.Summary, "findings": v.Findings})
		if err != nil || !strings.Contains(body, `"recorded":true`) {
			return nil, fmt.Errorf("verdict %s: %v", body, err)
		}
		return &agent.Result{ClaudeID: req.Name, ResultText: "Reviewed", SessionDir: req.SessionDir, NumTurns: 1}, nil
	}
	f.engine.Mu.Unlock()

	bound, err := f.s.options.Threads(repository, f.s.cfg)
	must(t, err)
	op, err := thread.TurnOperation(f.project, "codex-skills-review", thread.TurnInput{Workstream: stream, Agent: reviewer, Turn: queued.TurnID})
	must(t, err)
	if _, err := bound.Apply(ctx, op); err != nil {
		t.Fatalf("the review turn: %v", err)
	}
	req := seen.request
	if req.Name != queued.TurnID {
		t.Fatalf("the engine ran %q, not the review turn %q", req.Name, queued.TurnID)
	}

	// The request carries the role's skill and the skill cache, read-only, on a
	// Codex turn that keeps Codex's own tools and no write or shell grant.
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

	// Its system prompt is the unit reviewer's: the protocol and the materiality
	// rule, with how to review left to the role's skills.
	if !strings.HasPrefix(req.SystemPrompt, queued.SystemPrompt) {
		t.Errorf("the engine's system prompt is not the enqueued one:\n%s", req.SystemPrompt)
	}
	intakefixture.AssertCheckGateReviewInstructions(t, req.SystemPrompt)
	for _, want := range []string{
		"You are the unit reviewer",
		"Review the exact candidate identity your prompt gives",
		"workstream_diff", "Call ask when a decision is needed", "Otherwise call verdict",
		"A finding is material, and the verdict is material_findings, when the task or an acceptance item does not hold",
		"when it breaks a written rule", "never blocks", "Formatting and lint belong to the checks",
		"How to review is left to the skills configured on your role",
		"Read a skill's SKILL.md, and the files it references, with file_read at the path the skill list gives",
		"Osmia's protocol overrides a skill's own output format and any instruction never to stop and ask",
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

	// The turn's own file_read reads the skill and what it references at their
	// mounted paths, and the candidate's files in the view as before.
	if seen.skillMD != fakeSkillMarkdown {
		t.Errorf("file_read of %s returned %q", skillMD, seen.skillMD)
	}
	if seen.checked != fakeChecklist {
		t.Errorf("file_read of %s returned %q", checklist, seen.checked)
	}
	if !strings.Contains(seen.view, "package trace") {
		t.Errorf("file_read of the candidate's %s in the view returned %q", masonWrote, seen.view)
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
	must(t, err)
	if out := done.Turns[0].Response; out == nil || out.Result.Outcome == nil || out.Result.Outcome.Status != verdictOutcome {
		t.Fatalf("the review turn recorded %+v, not a verdict", out)
	}
}
