package executionjourney

import (
	context "context"
	errors "errors"
	fmt "fmt"
	fs "io/fs"
	os "os"
	exec "os/exec"
	filepath "path/filepath"
	reflect "reflect"
	slices "slices"
	strings "strings"
	sync "sync"
	testing "testing"

	agent "github.com/kpenfound/agentcore/agent"
	config "github.com/kpenfound/osmia/internal/config"
	questions "github.com/kpenfound/osmia/internal/questions"
	trace "github.com/kpenfound/osmia/internal/trace"
	workspace "github.com/kpenfound/osmia/internal/workspace"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

type forkCommit struct {
	Headers []string
	Author  string
	Message string
	Tree    string
	Parents int
	Paths   []string
}

type delivered struct {
	Refs    []string
	Commits []forkCommit
}

func TestJujutsuWorkspacesDemonstration(t *testing.T) {
	if _, err := exec.LookPath("jj"); err != nil {
		if os.Getenv("OSMIA_REQUIRE_JJ") != "" {
			t.Fatalf("jj is required but not on PATH: %v", err)
		}
		t.Skip("jj is not installed")
	}
	t.Parallel()
	var mu sync.Mutex
	runs := map[string]delivered{}
	t.Run("runs", func(t *testing.T) {
		for _, backend := range []string{config.WorkspacesGit, config.WorkspacesJujutsu} {
			t.Run(backend, func(t *testing.T) {
				t.Parallel()
				fork := deliverOn(t, backend)
				mu.Lock()
				defer mu.Unlock()
				runs[backend] = fork
			})
		}
	})
	git, jj := runs[config.WorkspacesGit], runs[config.WorkspacesJujutsu]
	if t.Failed() {
		return
	}
	if len(jj.Commits) == 0 || !reflect.DeepEqual(git, jj) {
		t.Fatalf("the fork reads differently on Jujutsu workspaces\ngit:     %+v\njujutsu: %+v", git, jj)
	}
}

func runIndependent(message string, stream config.WorkstreamID) string {
	lines := strings.Split(strings.ReplaceAll(message, string(stream), "<workstream>"), "\n")
	for i, line := range lines {
		for _, trailer := range []string{"Osmia-Candidate: ", "Osmia-Base: ", "Osmia-Operation: "} {
			if strings.HasPrefix(line, trailer) {
				lines[i] = trailer + "<id>"
			}
		}
	}
	return strings.Join(lines, "\n")
}

func deliverOn(t *testing.T, backend string) delivered {
	return deliverOnProject(t, backend, false)
}

func deliverOnProject(t *testing.T, backend string, sameRepository bool) delivered {
	t.Helper()
	ctx := context.Background()
	f, masons := newMasonFixture(t, 1, validPlan)
	defer func() { f.stop(t) }()

	f.stop(t)
	configPath := filepath.Join(f.opts.Config.Root, "config.toml")
	text, err := os.ReadFile(configPath)
	must(t, err)
	lines := strings.Split(string(text), "\n")
	for i, line := range lines {
		if strings.HasPrefix(line, "workspaces = ") {
			lines[i] = fmt.Sprintf("workspaces = %q", backend)
		}
	}
	must(t, os.WriteFile(configPath, []byte(strings.Join(lines, "\n")), 0600))

	var stream config.WorkstreamID
	home := filepath.Dir(f.clone)
	fork := filepath.Join(home, "remotes", "owner", "dagger.git")
	must(t, os.MkdirAll(filepath.Dir(fork), 0700))
	demoGit(t, home, "init", "--quiet", "--bare", fork)
	if sameRepository {
		fork = filepath.Join(home, "remotes", "dagger", "dagger.git")
		demoGit(t, home, "-C", f.clone, "remote", "rename", "upstream", "origin")
		projectPath := filepath.Join(f.opts.Config.Root, "projects", string(f.s.cfg.Project.ID), "config.toml")
		data, err := os.ReadFile(projectPath)
		must(t, err)
		var kept []string
		for _, line := range strings.Split(string(data), "\n") {
			if !strings.HasPrefix(strings.TrimSpace(line), "fork") {
				kept = append(kept, line)
			}
		}
		must(t, os.WriteFile(projectPath, []byte(strings.Join(kept, "\n")), 0600))
	} else {
		demoGit(t, home, "-C", f.clone, "remote", "add", "origin", fork)
	}
	originalBase := strings.TrimSpace(demoGit(t, home, "-C", filepath.Join(home, "remotes", "dagger", "dagger.git"), "rev-parse", "main"))
	forkBranch := func() string {
		return strings.TrimSpace(demoGit(t, home, "-C", fork, "for-each-ref", "--format=%(objectname)", "refs/heads/"+featureBranch(stream)))
	}
	prs := &fakePulls{Fork: forkBranch}
	f.opts.PullRequests = prs

	problems := &demoProblems{}
	p := &faults{}
	answered := questions.TurnID("1")
	masons.Mu.Lock()
	masons.Play[answered] = reportDone("Uploads resume")
	masons.Play[masonTurnID("dedupe")] = func(ctx context.Context, req agent.Request, tools *mcp.ClientSession) error {
		if err := os.WriteFile(filepath.Join(req.Workspace.Directory(), "internal/trace/dedupe.go"), []byte("package trace\n// Skip acknowledged chunks.\n"), 0600); err != nil {
			return err
		}
		if recorded, reason, err := done(ctx, tools, map[string]any{"outcome": "Acknowledged chunks are skipped"}); err != nil || !recorded {
			return fmt.Errorf("done refused: %q %v", reason, err)
		}
		return nil
	}
	masons.Mu.Unlock()
	f.engine.Mu.Lock()
	f.engine.Turns[masonTurnID("resume")] = masons.Asking(p, "", "1")
	f.engine.Turns[answered] = masons.Turn
	chief := f.engine.Turns["*"]
	f.engine.Turns["*"] = func(ctx context.Context, req agent.Request, turn *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		switch {
		case strings.HasPrefix(req.Name, "reviewer-") || strings.HasPrefix(req.Name, "reviewer_"):
			body, err := callTool(ctx, tools, verdictTool, map[string]any{"decision": "satisfactory", "summary": reviewSummary, "findings": []ReviewFinding{}})
			if err != nil || !strings.Contains(body, `"recorded":true`) {
				problems.report("review %s: %s: %v", req.Name, body, err)
			}
		case strings.HasPrefix(req.Name, "final-"):
			body, err := callTool(ctx, tools, FinalReportTool, map[string]any{"summary": "Resumable uploads", "findings": []any{}})
			if err != nil || !strings.Contains(body, `"recorded":true`) {
				problems.report("final report %s: %s: %v", req.Name, body, err)
			}
		default:
			return chief(ctx, req, turn, tools)
		}
		return &agent.Result{ClaudeID: "session-" + req.Name, ResultText: "Reviewed", SessionDir: req.SessionDir, NumTurns: 1}, nil
	}
	f.engine.Mu.Unlock()
	f.start(t)

	noJJ := func(when string) {
		t.Helper()
		must(t, filepath.WalkDir(f.clone, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					return nil
				}
				return err
			}
			if d.Name() == ".jj" {
				t.Fatalf("%s: the owner's clone holds %s", when, path)
			}
			return nil
		}))
	}

	all, err := f.c.Statuses(ctx)
	must(t, err)
	if all.Workspaces.Setting != backend || all.Workspaces.Backend != backend {
		t.Fatalf("service status workspaces %+v, want %s", all.Workspaces, backend)
	}
	stream = f.shedAs(t, "jujutsu-workspaces")
	st, err := f.c.Status(ctx, stream)
	must(t, err)
	if st.Workspaces != backend {
		t.Fatalf("workstream %s status reports %q workspaces, want %q", stream, st.Workspaces, backend)
	}
	noJJ("sealed")
	f.ratifiedBuild(t, stream)

	eventually(t, "question 1 was never escalated", func() bool {
		asked, err := f.repository().Questions(stream)
		must(t, err)
		return slices.ContainsFunc(asked, func(q trace.QuestionState) bool { return q.Asked.ID == "1" && q.State == trace.QuestionEscalated })
	})
	f.rule(t, "1")

	f.awaitMerged(t, stream, "resume")
	noJJ("resume landed")
	f.awaitMerged(t, stream, "dedupe")
	noJJ("dedupe landed")
	cfg := f.s.current()
	for name, w := range map[string]streamWorkspaces{
		"unit":    newUnitWorkspaces(cfg, f.repository()).streamWorkspaces,
		"feature": featureWorkspaces(cfg, f.repository()),
	} {
		provider := providerOf(t, w, stream)
		_, onJJ := provider.(*workspace.Jujutsu)
		if onJJ != (backend == config.WorkspacesJujutsu) {
			t.Fatalf("the %s workspaces of workstream %s are %T on %s", name, stream, provider, backend)
		}
		store := filepath.Join(cfg.Root.String(), w.Directory, string(cfg.Project.ID), ".jujutsu")
		if _, err := os.Stat(store); onJJ != (err == nil) {
			t.Fatalf("the %s workspaces' Jujutsu repository %s on %s: %v", name, store, backend, err)
		}
	}

	f.awaitFeature(t, stream, AssembledState)
	var presented DeliveryPresentation
	eventually(t, "final review never presented the delivery", func() bool {
		presented, err = f.c.Delivery(ctx, stream)
		return err == nil
	})
	_, err = f.c.ApproveDelivery(ctx, stream, DeliveryDecision{Review: presented.Report.Review, ReviewRevision: presented.ReviewRevision, Commit: presented.Report.Commit, DraftHash: presented.DraftHash})
	must(t, err)
	f.awaitFeature(t, stream, DeliveredState)
	problems.check(t)
	p.Check(t)
	masons.Check(t)
	prs.Mu.Lock()
	if len(prs.Prs) != 1 || prs.Creates != 1 || prs.Prs[0].Body != presented.Draft || prs.Prs[0].HeadCommit != forkBranch() {
		t.Fatalf("pull requests %+v", prs.Prs)
	}
	prs.Mu.Unlock()
	st, err = f.c.Status(ctx, stream)
	must(t, err)
	if st.Workspaces != backend {
		t.Fatalf("delivered workstream %s status reports %q workspaces, want %q", stream, st.Workspaces, backend)
	}
	noJJ("delivered")

	base := strings.TrimSpace(demoGit(t, home, "-C", filepath.Join(home, "remotes", "dagger", "dagger.git"), "rev-parse", "refs/heads/main"))
	var out delivered
	out.Refs = strings.Fields(strings.ReplaceAll(demoGit(t, home, "-C", fork, "for-each-ref", "--format=%(refname)"), string(stream), "<workstream>"))
	wantRefs := []string{"refs/heads/osmia/<workstream>"}
	if sameRepository {
		wantRefs = []string{"refs/heads/main", "refs/heads/osmia/<workstream>"}
	}
	if base != originalBase {
		t.Fatal("publication changed upstream main")
	}
	if !slices.Equal(out.Refs, wantRefs) {
		t.Fatalf("the fork's refs %v", out.Refs)
	}
	for _, commit := range strings.Fields(demoGit(t, home, "-C", fork, "rev-list", "--reverse", "--topo-order", base+".."+forkBranch())) {
		raw := demoGit(t, home, "-C", fork, "cat-file", "commit", commit)
		header, message, _ := strings.Cut(raw, "\n\n")
		c := forkCommit{Message: runIndependent(message, stream)}
		for line := range strings.SplitSeq(header, "\n") {
			if strings.HasPrefix(line, " ") {
				continue
			}
			key, value, _ := strings.Cut(line, " ")
			c.Headers = append(c.Headers, key)
			switch key {
			case "tree":
				c.Tree = value
			case "parent":
				c.Parents++
			case "author":
				c.Author, _, _ = strings.Cut(value, ">")
			}
		}
		for _, key := range c.Headers {
			if !slices.Contains([]string{"tree", "parent", "author", "committer", "gpgsig"}, key) {
				t.Fatalf("commit %s on the fork is not plain Git: header %q\n%s", commit, key, raw)
			}
		}
		if !slices.Contains(c.Headers, "gpgsig") || c.Author != "Delivery Owner <delivery@example.invalid" {
			t.Fatalf("commit lacks owner attribution or signature: %+v", c)
		}
		c.Paths = strings.Fields(demoGit(t, home, "-C", fork, "ls-tree", "-r", "--name-only", commit))
		for _, path := range c.Paths {
			if slices.Contains(strings.Split(path, "/"), ".jj") {
				t.Fatalf("commit %s on the fork holds %s", commit, path)
			}
		}
		out.Commits = append(out.Commits, c)
	}
	return out
}

func TestDeliveryWithoutForkOnBothWorkspaceBackends(t *testing.T) {
	t.Parallel()
	for _, backend := range []string{config.WorkspacesGit, config.WorkspacesJujutsu} {
		t.Run(backend, func(t *testing.T) {
			if backend == config.WorkspacesJujutsu {
				if _, err := exec.LookPath("jj"); err != nil {
					if os.Getenv("OSMIA_REQUIRE_JJ") != "" {
						t.Fatal(err)
					}
					t.Skip("jj is not installed")
				}
			}
			deliverOnProject(t, backend, true)
		})
	}
}
