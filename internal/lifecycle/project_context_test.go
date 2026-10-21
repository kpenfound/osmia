package lifecycle_test

import (
	context "context"
	json "encoding/json"
	fmt "fmt"
	coreadapter "github.com/kpenfound/osmia/internal/coreadapter"
	trace "github.com/kpenfound/osmia/internal/trace"
	strings "strings"
	testing "testing"
)

func TestProjectContextPagesKnowledgeWithoutExposingPrivateRecords(t *testing.T) {
	t.Parallel()
	f, stream, repo, _ := newFinalFixture(t, "project-context")
	ctx := context.Background()
	_, err := repo.EnsureChiefOfStaff(ctx, stream, f.s.now(), ownerActor)
	must(t, err)
	h := trace.Header{Schema: "osmia.trace.turn-request", Version: 1, ID: "request_context", Revision: 1, Project: repo.Project(), Workstream: stream, At: f.s.now(), Actor: ownerActor, Cause: "owner"}
	_, err = repo.EnqueueTurn(ctx, trace.TurnRequest{Header: h, AgentID: trace.ChiefOfStaff, ThreadID: trace.ChiefOfStaff, TurnID: "context", Profile: coreadapter.Profile{Name: "default", Backend: "fake", Model: "fake"}, Prompt: "Read context"})
	must(t, err)
	_, err = repo.ClaimTurn(ctx, stream, trace.ChiefOfStaff, "context-claim", "/owned/context", f.s.now())
	must(t, err)
	scope := coreadapter.Scope{Project: string(repo.Project()), Workstream: string(stream), Thread: trace.ChiefOfStaff, Turn: "context", Role: trace.ChiefOfStaff}
	var docs []trace.Document
	for i := range 25 {
		dh := trace.Header{Schema: "osmia.trace.document", Version: 1, ID: fmt.Sprintf("knowledge-%d", i), Revision: 1, Project: repo.Project(), At: f.s.now(), Actor: librarianActor, Cause: "fixture"}
		docs = append(docs, trace.Document{Header: dh, Path: fmt.Sprintf("kb/topic-%02d.md", i), Content: strings.Repeat("évidence ", 4000)})
	}
	dh := docs[0].Header
	dh.ID = "private-note"
	docs = append(docs, trace.Document{Header: dh, Path: "notes/private.md", Content: "private-secret"})
	must(t, repo.RecordDocuments(ctx, docs))
	tool := projectContext(repo, scope)
	call := func(in string) map[string]any {
		t.Helper()
		raw, err := tool.Handle(ctx, json.RawMessage(in))
		must(t, err)
		if len(raw) > 20000 || strings.Contains(string(raw), "private-secret") {
			t.Fatalf("unbounded or private output: %d bytes", len(raw))
		}
		var got map[string]any
		must(t, json.Unmarshal(raw, &got))
		return got
	}
	first := call(`{"prefix":"kb/topic-"}`)
	if len(first["records"].([]any)) != 20 || first["next_offset"] != float64(20) {
		t.Fatalf("first page: %+v", first)
	}
	last := call(`{"prefix":"kb/topic-","offset":20}`)
	if len(last["records"].([]any)) != 5 || last["next_offset"] != float64(-1) {
		t.Fatalf("last page: %+v", last)
	}
	part := call(`{"path":"kb/topic-00.md","revision":1}`)
	next := int(part["next_offset"].(float64))
	if next <= 0 || len(part["content"].(string)) != next {
		t.Fatalf("invalid content continuation: %+v", part)
	}
	call(fmt.Sprintf(`{"path":"kb/topic-00.md","revision":1,"offset":%d}`, next))
	updated := docs[0]
	updated.Revision = 2
	updated.Content = "Current consolidated knowledge."
	must(t, repo.RecordDocuments(ctx, []trace.Document{updated}))
	if current := call(`{"path":"kb/topic-00.md"}`); current["content"] != updated.Content || current["revision"] != float64(2) {
		t.Fatalf("stale current knowledge: %+v", current)
	}
	if old := call(`{"path":"kb/topic-00.md","revision":1}`); old["content"] != part["content"] {
		t.Fatal("compaction lost historical evidence")
	}
	if found := call(`{"query":"consolidated"}`); found["total"] != float64(1) {
		t.Fatalf("search did not use current knowledge: %+v", found)
	}
	for _, path := range []string{"../config.toml", "notes/private.md", "tools/private.json"} {
		in, _ := json.Marshal(map[string]string{"path": path})
		if _, err := tool.Handle(ctx, in); err == nil {
			t.Errorf("read forbidden path %s", path)
		}
	}
	bad := scope
	bad.Turn = "not-active"
	if _, err := projectContext(repo, bad).Handle(ctx, json.RawMessage(`{}`)); err == nil {
		t.Fatal("inactive turn read project context")
	}
}
