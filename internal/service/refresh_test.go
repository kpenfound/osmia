package service

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/busybees/core/agent"
	"github.com/kpenfound/osmia/internal/bundle"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/kb"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func refreshFixture(t *testing.T, output string, fail bool) (*shedFixture, *fakeMasons) {
	t.Helper()
	f, masons := newLandingFixture(t, validPlan)
	f.stop(t)
	configFile, err := os.OpenFile(filepath.Join(f.opts.Config.Root, "config.toml"), os.O_APPEND|os.O_WRONLY, 0)
	must(t, err)
	_, err = configFile.WriteString(librarianContainer)
	must(t, err)
	must(t, configFile.Close())
	f.opts.Librarian = &Librarian{Engine: f.engine, Hosts: f.opts.Architect.Hosts}
	f.start(t)
	must(t, f.repository().RecordDocuments(context.Background(), []trace.Document{{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: "subsystem-internal", Revision: 1, Project: f.project, At: f.clock.Now(), Actor: librarianActor, Cause: "fixture"}, Path: "kb/internal.md", Content: "# Internal\n\nPrevious project knowledge.\n"}}))
	masons.play[masonTurnID("resume")] = func(ctx context.Context, _ agent.Request, tools *mcp.ClientSession) error {
		ok, reason, err := done(ctx, tools, map[string]any{"outcome": "Built", "criteria": []any{criterionArgs(resumeReport)}, "learnings": []string{"Trace snapshots require a clean worktree."}})
		if err != nil || !ok {
			return fmt.Errorf("done: %s: %w", reason, err)
		}
		return nil
	}
	current, err := kb.Load(f.repository())
	must(t, err)
	encoded, err := kb.Encode(current)
	must(t, err)
	f.engine.mu.Lock()
	previous := f.engine.turns["*"]
	f.engine.turns["*"] = func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		if !strings.HasPrefix(req.Name, "refresh-") {
			return previous(ctx, req, verified, tools)
		}
		if fail {
			return nil, fmt.Errorf("fake librarian failed")
		}
		source, err := callTool(ctx, tools, "file_read", map[string]any{"path": "source/report.json"})
		if err != nil || !strings.Contains(source, "Trace snapshots require a clean worktree") {
			return nil, fmt.Errorf("missing reported learning: %s: %w", source, err)
		}
		landing, err := callTool(ctx, tools, "file_read", map[string]any{"path": "source/landing.json"})
		if err != nil || !strings.Contains(landing, "commit") {
			return nil, fmt.Errorf("missing landing source: %s: %w", landing, err)
		}
		built, err := callTool(ctx, tools, "file_read", map[string]any{"path": "repo/" + masonWrote})
		if err != nil || !strings.Contains(built, "package trace") {
			return nil, fmt.Errorf("landed files absent: %s: %v", built, err)
		}
		for path, content := range map[string]string{"output/kb/internal.md": output, "output/kb/entities.json": string(encoded)} {
			if _, err := callTool(ctx, tools, "file_write", map[string]any{"path": path, "content": content}); err != nil {
				return nil, err
			}
		}
		return &agent.Result{ClaudeID: "session-" + req.Name, ResultText: "Refreshed", SessionDir: req.SessionDir, NumTurns: 1}, nil
	}
	f.engine.mu.Unlock()
	return f, masons
}

func awaitRefresh(t *testing.T, f *shedFixture) trace.OperationRecord {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		ops, err := f.repository().Operations(librarianWorkstream(f.project))
		must(t, err)
		for _, op := range ops {
			if op.Operation.Action == RefreshAction && op.Result != nil {
				return op
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	ops, _ := f.repository().Operations(librarianWorkstream(f.project))
	t.Fatalf("refresh did not complete: %+v", ops)
	return trace.OperationRecord{}
}

func TestLandedLearningsRefreshKnowledgeAndSurviveRestart(t *testing.T) {
	t.Parallel()
	f, masons := refreshFixture(t, "# Internal\n\nPrevious project knowledge.\n\nTrace snapshots require a clean worktree.\n", false)
	defer f.stop(t)
	stream, _ := f.builtAs(t, "refresh-success")
	f.awaitMerged(t, stream, "resume")
	op := awaitRefresh(t, f)
	if op.Result.Outcome != "succeeded" {
		t.Fatalf("refresh: %+v", op.Result)
	}
	masons.check(t)
	var in refreshInput
	must(t, json.Unmarshal(op.Operation.Input, &in))
	if in.Unit != "resume" || in.Workstream != stream {
		t.Fatalf("source: %+v", in)
	}
	docs, err := trace.Read[trace.Document](f.repository(), "")
	must(t, err)
	var ledger trace.Document
	var prose trace.Document
	for _, d := range docs {
		if d.ID == "kb-sources" {
			ledger = d
		}
		if d.ID == "subsystem-internal" {
			prose = d
		}
	}
	var sources []KnowledgeSource
	must(t, json.Unmarshal([]byte(ledger.Content), &sources))
	if len(sources) != 1 || sources[0].Unit != in.Unit || sources[0].Commit != in.Commit || sources[0].Landing != in.Landing || sources[0].Report != in.Report || sources[0].Operation != op.Operation.ID || sources[0].Revision != 2 || prose.Revision != 2 || prose.Cause != op.Operation.ID || ledger.Revision != 1 {
		t.Fatalf("provenance: %+v; prose %+v", sources, prose)
	}
	files := bundle.Files{Repository: func(_ config.ProjectID) (*trace.Repository, error) { return f.repository(), nil }}
	visible, err := files.Mason(context.Background(), f.project, stream, "dedupe")
	must(t, err)
	if visible.Context.Mode != bundle.ModeFile || len(visible.Context.Knowledge) == 0 || !strings.Contains(visible.Context.Knowledge[0].Content, "clean worktree") {
		t.Fatalf("later unit did not see refreshed local context: %+v", visible.Context.Knowledge)
	}
	f.stop(t)
	f.start(t)
	second := awaitRefresh(t, f)
	if second.Operation.ID != op.Operation.ID {
		t.Fatalf("duplicate operation: %+v", second)
	}
	again := &refresher{extractor: &extractor{s: f.s, repository: f.repository()}}
	result, err := again.Apply(context.Background(), op.Operation)
	must(t, err)
	if result.Outcome != "succeeded" {
		t.Fatalf("retry: %+v", result)
	}
	all, err := trace.Read[trace.Document](f.repository(), "")
	must(t, err)
	count := 0
	for _, d := range all {
		if d.ID == "kb-sources" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("source ledger has %d revisions", count)
	}
}

func TestRefreshRequiresRecordedLanding(t *testing.T) {
	t.Parallel()
	f, masons := newMasonFixture(t, 1, validPlan)
	defer f.stop(t)
	masons.play[masonTurnID("resume")] = reportDone("Built")
	stream, _ := f.builtAs(t, "refresh-before-landing")
	f.awaitUnit(t, stream, "resume", UnitReviewing)
	r := &refresher{extractor: &extractor{s: f.s, repository: f.repository()}}
	must(t, r.Pass(context.Background()))
	ops, err := f.repository().Operations(librarianWorkstream(f.project))
	must(t, err)
	for _, op := range ops {
		if op.Operation.Action == RefreshAction {
			t.Fatalf("refresh before landing: %+v", op)
		}
	}
}

func TestFailedRefreshPreservesKnowledge(t *testing.T) {
	for _, tc := range []struct {
		name, output string
		fail         bool
	}{
		{"invalid", "", false}, {"turn-failure", "# Trace\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := refreshFixture(t, tc.output, tc.fail)
			defer f.stop(t)
			stream, _ := f.builtAs(t, "refresh-"+tc.name)
			f.awaitMerged(t, stream, "resume")
			op := awaitRefresh(t, f)
			if op.Result.Outcome != "failed" {
				t.Fatalf("refresh: %+v", op.Result)
			}
			docs, err := trace.Read[trace.Document](f.repository(), "")
			must(t, err)
			for _, d := range docs {
				if d.Cause == op.Operation.ID {
					t.Fatalf("failed refresh recorded %+v", d)
				}
			}
			prose, err := f.repository().Prose("internal")
			must(t, err)
			if prose != "# Internal\n\nPrevious project knowledge.\n" {
				t.Fatalf("previous knowledge changed: %q", prose)
			}
		})
	}
}
