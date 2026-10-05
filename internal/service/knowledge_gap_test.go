package service

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/kb"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestCodeAnswerQueuesPinnedKnowledgeRefreshAcrossRestart(t *testing.T) {
	t.Parallel()
	f, stream, repo, _ := newFinalFixture(t, "code-answer")
	ctx := context.Background()
	commit := moveFeature(t, f, stream, map[string]string{"answer.go": "package answer\n// Committed knowledge.\n"})
	_, err := repo.EnsureChiefOfStaff(ctx, stream, f.s.now(), ownerActor)
	must(t, err)
	h := trace.Header{Schema: "osmia.trace.turn-request", Version: 1, ID: "request_inspect", Revision: 1, Project: repo.Project(), Workstream: stream, At: f.s.now(), Actor: ownerActor, Cause: "question"}
	_, err = repo.EnqueueTurn(ctx, trace.TurnRequest{Header: h, AgentID: trace.ChiefOfStaff, ThreadID: trace.ChiefOfStaff, TurnID: "inspect", Profile: coreadapter.Profile{Name: "default", Backend: "fake", Model: "fake"}, Prompt: "Answer from code"})
	must(t, err)
	_, err = repo.ClaimTurn(ctx, stream, trace.ChiefOfStaff, "inspect-claim", "/owned/inspect", f.s.now())
	must(t, err)
	scope := coreadapter.Scope{Project: string(repo.Project()), Workstream: string(stream), Thread: trace.ChiefOfStaff, Turn: "inspect", Role: trace.ChiefOfStaff}
	tool := inspectCode(f.s.about(repo), repo, scope, f.s.now)
	raw, err := tool.Handle(ctx, json.RawMessage(`{"path":"answer.go"}`))
	must(t, err)
	var result struct {
		trace.CodeInspection
		Citation string `json:"citation"`
	}
	must(t, json.Unmarshal(raw, &result))
	if result.Commit != commit || !strings.Contains(result.Content, "Committed knowledge") || result.Citation == "" {
		t.Fatalf("inspection %s", raw)
	}
	if _, err := tool.Handle(ctx, json.RawMessage(`{"path":"../config.toml"}`)); err == nil {
		t.Fatal("escaped code view")
	}
	if again, err := tool.Handle(ctx, json.RawMessage(`{"path":"answer.go"}`)); err != nil || string(again) != string(raw) {
		t.Fatalf("retry %s %v", again, err)
	}
	h.Schema, h.ID, h.Actor = "osmia.trace.ruling", "code-answer", trace.Actor{Kind: "agent", ID: trace.ChiefOfStaff}
	must(t, repo.Append(ctx, trace.Ruling{Header: h, QuestionID: "code-question", QuestionRevision: 1, Decision: trace.DecisionAnswer, ReturnedAnswer: "The committed code specifies the behavior.", Citations: []string{result.Citation}}))
	// Restart between the answer and its derived operation cannot lose the gap.
	repo = restartTrace(t, f, repo)
	r := &refresher{extractor: &extractor{s: f.s, repository: repo}}
	must(t, r.Pass(ctx))
	ops, err := repo.Operations(librarianWorkstream(repo.Project()))
	must(t, err)
	for _, pending := range ops {
		if pending.Operation.Action == ExtractAction && pending.Result == nil {
			settleOperation(t, f.s, repo, librarianWorkstream(repo.Project()), pending.Operation, r.extractor)
		}
	}
	must(t, r.Pass(ctx))
	ops, err = repo.Operations(librarianWorkstream(repo.Project()))
	must(t, err)
	var op trace.OperationRecord
	for _, candidate := range ops {
		if candidate.Operation.Action == RefreshAction {
			op = candidate
		}
	}
	if op.Operation.ID == "" {
		t.Fatalf("no gap operation: %+v", ops)
	}
	var in refreshInput
	must(t, json.Unmarshal(op.Operation.Input, &in))
	if in.Commit != commit || in.Question != "code-question" || in.Inspection != strings.TrimPrefix(result.Citation, "inspection#") {
		t.Fatalf("source %+v", in)
	}
	// A later branch movement does not change the librarian's evidence.
	moveFeature(t, f, stream, map[string]string{"answer.go": "package answer\n// Later change.\n"})
	view := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(view, "seed"), 0700))
	must(t, stageRefreshSource(ctx, repo, f.clone, in.key()+"-1", view))
	code, err := os.ReadFile(filepath.Join(view, "repo", "answer.go"))
	must(t, err)
	if !strings.Contains(string(code), "Committed knowledge") {
		t.Fatalf("unpinned view: %s", code)
	}
	source, err := os.ReadFile(filepath.Join(view, "source", "ruling.json"))
	must(t, err)
	if !strings.Contains(string(source), result.Citation) {
		t.Fatal("missing citation provenance")
	}
	// Accept only validated librarian output and retain its source in the ledger.
	turn := in.key() + "-1"
	output := filepath.Join(r.turnDirectory(turn), kb.OutputDirectory, "kb")
	must(t, os.MkdirAll(output, 0700))
	entities, err := kb.Load(repo)
	must(t, err)
	encoded, err := kb.Encode(entities)
	must(t, err)
	must(t, os.WriteFile(filepath.Join(output, "entities.json"), encoded, 0600))
	must(t, os.WriteFile(filepath.Join(output, "internal.md"), []byte("# Internal\n\nCommitted knowledge.\n"), 0600))
	accepted, err := r.recordRefresh(ctx, op.Operation.ID, in, trace.QueuedTurn{Request: trace.TurnRequest{TurnID: turn}})
	must(t, err)
	if accepted.Outcome != "succeeded" {
		t.Fatal(accepted)
	}
	observed, err := r.Inspect(ctx, op.Operation)
	must(t, err)
	if observed.Result == nil || observed.Result.Outcome != "succeeded" {
		t.Fatalf("reconcile %+v", observed)
	}
	must(t, r.Pass(ctx))
	again, err := repo.Operations(librarianWorkstream(repo.Project()))
	must(t, err)
	if len(again) != len(ops) {
		t.Fatal("duplicate refresh on retry")
	}
	docs, err := trace.Read[trace.Document](repo, "")
	must(t, err)
	found := false
	for _, d := range docs {
		if d.ID == "kb-sources" && strings.Contains(d.Content, in.Inspection) && strings.Contains(d.Content, commit) {
			found = true
		}
	}
	if !found {
		t.Fatal("knowledge provenance missing")
	}
	if _, err := tool.Handle(ctx, json.RawMessage(`{"path":"answer.go"}`)); err == nil {
		t.Fatal("stale turn read code")
	}
}
