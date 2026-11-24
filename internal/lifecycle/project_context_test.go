package lifecycle_test

import (
	context "context"
	json "encoding/json"
	fmt "fmt"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	strings "strings"
	testing "testing"

	coreadapter "github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/service"
	trace "github.com/kpenfound/osmia/internal/trace"
)

func TestProjectContextPagesNoticesWithoutExposingPrivateRecordsOrProse(t *testing.T) {
	t.Parallel()
	f, stream, repo, _ := newFinalFixture(t, "project-context")
	ctx := context.Background()
	_, err := repo.EnsureChiefOfStaff(ctx, stream, f.s.now(), intake.OwnerActor)
	fixturekit.Must(t, err)
	h := trace.Header{Schema: "osmia.trace.turn-request", Version: 1, ID: "request_context", Revision: 1, Project: repo.Project(), Workstream: stream, At: f.s.now(), Actor: intake.OwnerActor, Cause: "owner"}
	_, err = repo.EnqueueTurn(ctx, trace.TurnRequest{Header: h, AgentID: trace.ChiefOfStaff, ThreadID: trace.ChiefOfStaff, TurnID: "context", Profile: coreadapter.Profile{Name: "default", Backend: "fake", Model: "fake"}, Prompt: "Read context"})
	fixturekit.Must(t, err)
	_, err = repo.ClaimTurn(ctx, stream, trace.ChiefOfStaff, "context-claim", "/owned/context", f.s.now())
	fixturekit.Must(t, err)
	scope := coreadapter.Scope{Project: string(repo.Project()), Workstream: string(stream), Thread: trace.ChiefOfStaff, Turn: "context", Role: trace.ChiefOfStaff}
	notice := func(text string) string {
		data, err := json.Marshal(trace.ProjectNotice{Text: text})
		fixturekit.Must(t, err)
		return string(data)
	}
	var docs []trace.Document
	for i := range 25 {
		dh := trace.Header{Schema: "osmia.trace.document", Version: 1, ID: fmt.Sprintf("topic-%02d", i), Revision: 1, Project: repo.Project(), At: f.s.now(), Actor: intake.OwnerActor, Cause: "fixture"}
		docs = append(docs, trace.Document{Header: dh, Path: fmt.Sprintf("notices/topic-%02d.json", i), Content: notice(strings.Repeat("évidence ", 4000))})
	}
	dh := docs[0].Header
	dh.ID = "private-note"
	docs = append(docs, trace.Document{Header: dh, Path: "notes/private.md", Content: "private-secret"})
	dh.ID = "subsystem-topic"
	docs = append(docs, trace.Document{Header: dh, Path: "kb/topic-00.md", Content: "old-prose-secret"})
	dh.ID = "kb-sources"
	docs = append(docs, trace.Document{Header: dh, Path: "kb/sources.json", Content: "[\"old-prose-secret\"]\n"})
	fixturekit.Must(t, repo.RecordDocuments(ctx, docs))
	tool := service.TestProjectContext(repo, scope)
	call := func(in string) map[string]any {
		t.Helper()
		raw, err := tool.Handle(ctx, json.RawMessage(in))
		fixturekit.Must(t, err)
		if len(raw) > 20000 || strings.Contains(string(raw), "private-secret") || strings.Contains(string(raw), "old-prose-secret") {
			t.Fatalf("unbounded or private output: %d bytes", len(raw))
		}
		var got map[string]any
		fixturekit.Must(t, json.Unmarshal(raw, &got))
		return got
	}
	first := call(`{"prefix":"notices/topic-"}`)
	if len(first["records"].([]any)) != 20 || first["next_offset"] != float64(20) {
		t.Fatalf("first page: %+v", first)
	}
	last := call(`{"prefix":"notices/topic-","offset":20}`)
	if len(last["records"].([]any)) != 5 || last["next_offset"] != float64(-1) {
		t.Fatalf("last page: %+v", last)
	}
	part := call(`{"path":"notices/topic-00.json","revision":1}`)
	next := int(part["next_offset"].(float64))
	if next <= 0 || len(part["content"].(string)) != next {
		t.Fatalf("invalid content continuation: %+v", part)
	}
	call(fmt.Sprintf(`{"path":"notices/topic-00.json","revision":1,"offset":%d}`, next))
	updated := docs[0]
	updated.Revision = 2
	updated.Content = notice("Current consolidated notice.")
	fixturekit.Must(t, repo.RecordDocuments(ctx, []trace.Document{updated}))
	if current := call(`{"path":"notices/topic-00.json"}`); current["content"] != updated.Content || current["revision"] != float64(2) {
		t.Fatalf("stale current notice: %+v", current)
	}
	if old := call(`{"path":"notices/topic-00.json","revision":1}`); old["content"] != part["content"] {
		t.Fatal("compaction lost historical evidence")
	}
	if found := call(`{"query":"consolidated"}`); found["total"] != float64(1) {
		t.Fatalf("search did not use the current notice: %+v", found)
	}
	if listed := call(`{"prefix":"kb/"}`); listed["total"] != float64(1) || listed["records"].([]any)[0].(map[string]any)["path"] != "kb/entities.json" {
		t.Fatalf("kb/ lists more than the entity map: %+v", listed)
	}
	if found := call(`{"query":"old-prose-secret"}`); found["total"] != float64(0) {
		t.Fatalf("search found old prose: %+v", found)
	}
	for _, path := range []string{"../config.toml", "notes/private.md", "tools/private.json", "kb/topic-00.md", "kb/sources.json"} {
		in, _ := json.Marshal(map[string]string{"path": path})
		if _, err := tool.Handle(ctx, in); err == nil {
			t.Errorf("read forbidden path %s", path)
		}
		in, _ = json.Marshal(map[string]any{"path": path, "revision": 1})
		if _, err := tool.Handle(ctx, in); err == nil {
			t.Errorf("read revision 1 of forbidden path %s", path)
		}
	}
	bad := scope
	bad.Turn = "not-active"
	if _, err := service.TestProjectContext(repo, bad).Handle(ctx, json.RawMessage(`{}`)); err == nil {
		t.Fatal("inactive turn read project context")
	}
}
