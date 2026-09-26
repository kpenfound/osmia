package service

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/kpenfound/busybees/core/agent"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/kpenfound/osmia/internal/workspace"
)

// forkCommit is what a reader of the fork sees of one commit of the
// delivered feature branch: its headers, author, message, tree and paths.
type forkCommit struct {
	Headers []string
	Author  string
	Message string
	Tree    string
	Parents int
	Paths   []string
}

// delivered is what one run of the demonstration left on the fork.
type delivered struct {
	Refs    []string
	Commits []forkCommit
}

// TestM6JujutsuWorkspacesDemonstration takes the same handed-in feature to an
// owner-approved pull request twice through the local API, once with
// workspaces = "git" and once with workspaces = "jujutsu" and the jj on PATH.
// The fork receives the same plain Git branch and commits from both, status
// reports each run's backend, and no .jj ever appears in the owner's clone.
// See docs/m6-jujutsu-workspaces.md.
func TestM6JujutsuWorkspacesDemonstration(t *testing.T) {
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

// runIndependent masks what differs between any two runs in a commit
// message: the workstream's ID and the object and operation IDs its
// trailers name.
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

// deliverOn hands in a two-unit feature with workspaces set to backend and
// follows it through ratification, an escalated question, both units landing,
// final review and the owner's approval to one pull request on a local fork.
// It returns what the fork then holds.
func deliverOn(t *testing.T, backend string) delivered {
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

	// Delivery goes to a local bare fork and a fake pull request host.
	var stream config.WorkstreamID
	home := filepath.Dir(f.clone)
	fork := filepath.Join(home, "remotes", "owner", "dagger.git")
	must(t, os.MkdirAll(filepath.Dir(fork), 0700))
	demoGit(t, home, "init", "--quiet", "--bare", fork)
	demoGit(t, home, "-C", f.clone, "remote", "add", "origin", fork)
	forkBranch := func() string {
		return strings.TrimSpace(demoGit(t, home, "-C", fork, "for-each-ref", "--format=%(objectname)", "refs/heads/"+featureBranch(stream)))
	}
	prs := &fakePulls{fork: forkBranch}
	f.opts.PullRequests = prs

	// resume's mason asks question 1 and reports done on the answer's turn;
	// dedupe's mason reports done on its first turn.
	problems := &demoProblems{}
	p := &faults{}
	answered := questions.TurnID("1")
	masons.mu.Lock()
	masons.play[answered] = reportDone("Uploads resume")
	masons.play[masonTurnID("dedupe")] = func(ctx context.Context, req agent.Request, tools *mcp.ClientSession) error {
		if err := os.WriteFile(filepath.Join(req.Workspace.Directory(), "internal/trace/dedupe.go"), []byte("package trace\n// Skip acknowledged chunks.\n"), 0600); err != nil {
			return err
		}
		if recorded, reason, err := done(ctx, tools, map[string]any{"outcome": "Acknowledged chunks are skipped", "criteria": []any{criterionArgs(dedupeReport)}}); err != nil || !recorded {
			return fmt.Errorf("done refused: %q %v", reason, err)
		}
		return nil
	}
	masons.mu.Unlock()
	f.engine.mu.Lock()
	f.engine.turns[masonTurnID("resume")] = masons.asking(p, "", "1")
	f.engine.turns[answered] = masons.turn
	chief := f.engine.turns["*"]
	f.engine.turns["*"] = func(ctx context.Context, req agent.Request, turn *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		switch {
		case strings.HasPrefix(req.Name, "reviewer-") || strings.HasPrefix(req.Name, "reviewer_"):
			criterion := "spec#2"
			if strings.HasPrefix(req.Name, reviewerAgent("resume")) {
				criterion = "spec#1"
			}
			body, err := callTool(ctx, tools, verdictTool, map[string]any{"decision": "satisfactory", "evidence": []ReviewEvidence{{Criterion: criterion, Evidence: "The candidate contains the planned proof"}}, "findings": []ReviewFinding{}})
			if err != nil || !strings.Contains(body, `"recorded":true`) {
				problems.report("review %s: %s: %v", req.Name, body, err)
			}
		case strings.HasPrefix(req.Name, "final-"):
			body, err := callTool(ctx, tools, FinalReportTool, map[string]any{"summary": "Resumable uploads", "criteria": []any{
				map[string]any{"criterion": "spec#1", "evidence": "resume landing and TestResume"},
				map[string]any{"criterion": "spec#2", "evidence": "dedupe landing skips acknowledged chunks"},
			}})
			if err != nil || !strings.Contains(body, `"recorded":true`) {
				problems.report("final report %s: %s: %v", req.Name, body, err)
			}
		default:
			return chief(ctx, req, turn, tools)
		}
		return &agent.Result{ClaudeID: "session-" + req.Name, ResultText: "Reviewed", SessionDir: req.SessionDir, NumTurns: 1}, nil
	}
	f.engine.mu.Unlock()
	f.start(t)

	// noJJ fails the test if a .jj is anywhere in the owner's clone.
	noJJ := func(when string) {
		t.Helper()
		must(t, filepath.WalkDir(f.clone, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.Name() == ".jj" {
				t.Fatalf("%s: the owner's clone holds %s", when, path)
			}
			return nil
		}))
	}

	// 1. Service status names the backend a new workstream gets; the design
	// is handed in, sealed and waits for ratification.
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

	// 2. resume's question is escalated and the owner answers it.
	eventually(t, "question 1 was never escalated", func() bool {
		asked, err := f.repository().Questions(stream)
		must(t, err)
		return slices.ContainsFunc(asked, func(q trace.QuestionState) bool { return q.Asked.ID == "1" && q.State == trace.QuestionEscalated })
	})
	f.rule(t, "1")

	// 3. Both units are built, reviewed and land in the backend's
	// workspaces.
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
		store := filepath.Join(cfg.Root.String(), w.directory, string(cfg.Project.ID), ".jujutsu")
		if _, err := os.Stat(store); onJJ != (err == nil) {
			t.Fatalf("the %s workspaces' Jujutsu repository %s on %s: %v", name, store, backend, err)
		}
	}

	// 4. Final review presents the delivery; the owner approves it and the
	// service opens the pull request.
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
	p.check(t)
	masons.check(t)
	prs.mu.Lock()
	if len(prs.prs) != 1 || prs.creates != 1 || prs.prs[0].Body != presented.Draft || prs.prs[0].HeadCommit != forkBranch() {
		t.Fatalf("pull requests %+v", prs.prs)
	}
	prs.mu.Unlock()
	st, err = f.c.Status(ctx, stream)
	must(t, err)
	if st.Workspaces != backend {
		t.Fatalf("delivered workstream %s status reports %q workspaces, want %q", stream, st.Workspaces, backend)
	}
	noJJ("delivered")

	// What the fork holds: its refs, and each commit the feature branch adds
	// to upstream, oldest first.
	base := strings.TrimSpace(demoGit(t, home, "-C", filepath.Join(home, "remotes", "dagger", "dagger.git"), "rev-parse", "refs/heads/main"))
	var out delivered
	out.Refs = strings.Fields(strings.ReplaceAll(demoGit(t, home, "-C", fork, "for-each-ref", "--format=%(refname)"), string(stream), "<workstream>"))
	if !slices.Equal(out.Refs, []string{"refs/heads/osmia/<workstream>"}) {
		t.Fatalf("the fork's refs %v", out.Refs)
	}
	for _, commit := range strings.Fields(demoGit(t, home, "-C", fork, "rev-list", "--reverse", "--topo-order", base+".."+forkBranch())) {
		raw := demoGit(t, home, "-C", fork, "cat-file", "commit", commit)
		header, message, _ := strings.Cut(raw, "\n\n")
		c := forkCommit{Message: runIndependent(message, stream)}
		for line := range strings.SplitSeq(header, "\n") {
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
			if !slices.Contains([]string{"tree", "parent", "author", "committer"}, key) {
				t.Fatalf("commit %s on the fork is not plain Git: header %q\n%s", commit, key, raw)
			}
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
