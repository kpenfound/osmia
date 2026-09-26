package trace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/coreadapter"
)

func projectDocument(id, path, content string, revision int) Document {
	return Document{Header: Header{Schema: "osmia.trace.document", Version: Version, ID: id, Revision: revision, Project: projectID, At: at, Actor: Actor{Kind: "agent", ID: "agent_librarian"}, Cause: "operation_1", Depth: 1}, Path: path, Content: content}
}
func projectDocuments(t *testing.T, r *Repository, id string) []Document {
	t.Helper()
	docs, err := Read[Document](r, "")
	if err != nil {
		t.Fatal(err)
	}
	var out []Document
	for _, d := range docs {
		if d.ID == id {
			out = append(out, d)
		}
	}
	return out
}
func kbFiles(t *testing.T, root string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(root, "kb"))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(root, "kb", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out[e.Name()] = string(data)
	}
	return out
}

func TestRecordDocumentsIsOneCommit(t *testing.T) {
	r, root, p := create(t)
	ctx := context.Background()
	dir, err := root.ProjectTrace(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	first := []Document{projectDocument("subsystem-trace", "kb/trace.md", "# trace\n", 1), projectDocument("subsystem-service", "kb/service.md", "# service\n", 1), projectDocument(EntitiesDocument, EntitiesPath, "{\"version\":1,\"entities\":[]}\n", 1)}
	created, err := strconv.Atoi(gitOutput(t, r, "rev-list", "--count", "HEAD"))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.RecordDocuments(ctx, first); err != nil {
		t.Fatal(err)
	}
	before := gitOutput(t, r, "rev-list", "--count", "HEAD")
	if before != strconv.Itoa(created+1) {
		t.Fatalf("commits after one record: %s, %d before it", before, created)
	}
	if got := kbFiles(t, dir); !reflect.DeepEqual(got, map[string]string{"trace.md": "# trace\n", "service.md": "# service\n", "entities.json": "{\"version\":1,\"entities\":[]}\n"}) {
		t.Fatalf("files: %v", got)
	}
	// Every revision in the batch is checked before anything is written.
	invalid := [][]Document{
		{projectDocument("subsystem-trace", "kb/trace.md", "x\n", 2), projectDocument("subsystem-service", "kb/service.md", "x\n", 3)},
		{projectDocument("subsystem-trace", "kb/trace.md", "x\n", 2), projectDocument("subsystem-other", "kb/trace.md", "x\n", 1)},
		{projectDocument("subsystem-trace", "kb/trace.md", "x\n", 2), projectDocument("subsystem-trace", "kb/trace.md", "y\n", 3)},
		{projectDocument("charter", "charter.md", "1. Rule\n", 2)},
		{projectDocument("spec", "spec.md", "x\n", 1)},
		{},
	}
	for i, docs := range invalid {
		if err := r.RecordDocuments(ctx, docs); err == nil {
			t.Fatalf("batch %d accepted", i)
		}
		if got := gitOutput(t, r, "rev-list", "--count", "HEAD"); got != before {
			t.Fatalf("batch %d committed: %s", i, got)
		}
		if got := projectDocuments(t, r, "subsystem-trace"); len(got) != 1 {
			t.Fatalf("batch %d wrote revisions: %+v", i, got)
		}
	}
	if got := kbFiles(t, dir); len(got) != 3 || got["trace.md"] != "# trace\n" {
		t.Fatalf("files after refused batches: %v", got)
	}
	// A removal revision deletes the file and the tree entry; history keeps it.
	second := []Document{projectDocument("subsystem-trace", "kb/trace.md", "# trace 2\n", 2), projectDocument("subsystem-service", "kb/service.md", "", 2), projectDocument(EntitiesDocument, EntitiesPath, "{\"version\":1,\"entities\":[]}\n", 2)}
	if err := r.RecordDocuments(ctx, second); err != nil {
		t.Fatal(err)
	}
	if got := kbFiles(t, dir); !reflect.DeepEqual(got, map[string]string{"trace.md": "# trace 2\n", "entities.json": "{\"version\":1,\"entities\":[]}\n"}) {
		t.Fatalf("files after removal: %v", got)
	}
	if tree := gitOutput(t, r, "ls-tree", "--name-only", "HEAD", "kb/"); strings.Contains(tree, "service.md") {
		t.Fatalf("removed file still tracked:\n%s", tree)
	}
	if got := projectDocuments(t, r, "subsystem-service"); len(got) != 2 || got[1].Content != "" {
		t.Fatalf("removal revision: %+v", got)
	}
	if committed := gitOutput(t, r, "show", "HEAD~1:kb/service.md"); committed != "# service" {
		t.Fatalf("history lost the removed file: %q", committed)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(root, p)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got := projectDocuments(t, reopened, "subsystem-trace"); len(got) != 2 {
		t.Fatalf("reopen: %+v", got)
	}
}

func gitOutput(t *testing.T, r *Repository, args ...string) string {
	t.Helper()
	out, err := r.git(context.Background(), nil, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestRecordDocumentsPublicationFailures(t *testing.T) {
	pre := []string{"objects-written", "journal-written", "before-ref"}
	post := []string{"ref-published", "materialized:documents.jsonl", "materialized:trace.md", "removed:service.md", "before-journal-removal"}
	for _, step := range append(pre, post...) {
		t.Run(step, func(t *testing.T) {
			r, root, p := create(t)
			ctx := context.Background()
			dir, err := root.ProjectTrace(p.ID)
			if err != nil {
				t.Fatal(err)
			}
			first := []Document{projectDocument("subsystem-trace", "kb/trace.md", "# trace\n", 1), projectDocument("subsystem-service", "kb/service.md", "# service\n", 1)}
			if err := r.RecordDocuments(ctx, first); err != nil {
				t.Fatal(err)
			}
			second := []Document{projectDocument("subsystem-trace", "kb/trace.md", "# trace 2\n", 2), projectDocument("subsystem-service", "kb/service.md", "", 2)}
			injected := errors.New("injected publication failure")
			hit := false
			r.failPublication = func(got string) error {
				if got == step {
					hit = true
					return injected
				}
				return nil
			}
			if err := r.RecordDocuments(ctx, second); !errors.Is(err, injected) || !hit {
				t.Fatalf("injection not reached: %v", err)
			}
			r.failPublication = nil
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}
			r, err = Open(root, p)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			committed := false
			for _, s := range post {
				committed = committed || step == s
			}
			want := map[string]string{"trace.md": "# trace\n", "service.md": "# service\n", "entities.json": "{}\n"}
			revisions := 1
			if committed {
				want, revisions = map[string]string{"trace.md": "# trace 2\n", "entities.json": "{}\n"}, 2
			}
			if got := kbFiles(t, dir); !reflect.DeepEqual(got, want) {
				t.Fatalf("files: %v", got)
			}
			if got := projectDocuments(t, r, "subsystem-service"); len(got) != revisions {
				t.Fatalf("revisions: %+v", got)
			}
			if _, err := r.dir.Stat(publicationFile); !os.IsNotExist(err) {
				t.Fatalf("journal retained: %v", err)
			}
			// The same batch is recorded once whether or not it committed.
			if err := r.RecordDocuments(ctx, second); committed != (err != nil) {
				t.Fatalf("retry after committed=%t: %v", committed, err)
			}
			if got := projectDocuments(t, r, "subsystem-service"); len(got) != 2 || got[1].Content != "" {
				t.Fatalf("after retry: %+v", got)
			}
		})
	}
}

func TestAbandonTurnRecordsInterruption(t *testing.T) {
	r, root, p := create(t)
	ctx := context.Background()
	if err := r.CreateThread(ctx, threadAgent()); err != nil {
		t.Fatal(err)
	}
	enqueue(t, r, "one")
	enqueue(t, r, "two")
	claimed := claimTurn(t, r, "token")
	// The reserving session cannot abandon its own running turn.
	if err := r.AbandonTurn(ctx, streamID, "mason", "one", at.Add(time.Second)); !errors.Is(err, ErrClaim) {
		t.Fatalf("own session: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := Open(root, p)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := r.AbandonTurn(ctx, streamID, "mason", "two", at.Add(time.Second)); !errors.Is(err, ErrClaim) {
		t.Fatalf("unreserved turn: %v", err)
	}
	if err := r.AbandonTurn(ctx, streamID, "mason", "one", time.Time{}); err == nil {
		t.Fatal("zero timestamp accepted")
	}
	// A timestamp before the claim is raised to it.
	if err := r.AbandonTurn(ctx, streamID, "mason", "one", at.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	th := mustThread(t, r)
	q := th.Turns[0]
	if th.Active != "" || th.Status != "interrupted" || q.Status() != "interrupted" || q.Response == nil || !q.CompletedAt.Equal(claimed.Claim.At) {
		t.Fatalf("abandoned turn: %+v", th)
	}
	res := q.Response
	if res.Failure == "" || !res.Result.Cancelled || !res.Result.IsError || res.Result.SessionDirectory != claimed.Claim.SessionDirectory || !res.Result.StartedAt.Equal(claimed.Claim.At) || res.Result.Session != (coreadapter.BackendSession{}) || res.Cause != q.Request.Cause || res.Actor.ID != "thread-recovery" {
		t.Fatalf("interrupted response: %+v", res)
	}
	responses, err := Read[TurnResponse](r, streamID)
	if err != nil || len(responses) != 1 || !reflect.DeepEqual(responses[0], *res) {
		t.Fatalf("owned log: %+v %v", responses, err)
	}
	if err := r.AbandonTurn(ctx, streamID, "mason", "one", at.Add(time.Second)); !errors.Is(err, ErrClaim) {
		t.Fatalf("abandoning twice: %v", err)
	}
	// The successor is eligible, and the abandoned turn cannot be resumed from.
	if next := claimTurn(t, r, "next"); next.Request.TurnID != "two" {
		t.Fatalf("successor: %+v", next)
	}
	if th := mustThread(t, r); th.Session != threadAgent().Session {
		t.Fatalf("session changed by the interruption: %+v", th.Session)
	}
}

func TestAbandonTurnRefusesCapturedTurn(t *testing.T) {
	r, root, p := create(t)
	ctx := context.Background()
	if err := r.CreateThread(ctx, threadAgent()); err != nil {
		t.Fatal(err)
	}
	enqueue(t, r, "one")
	claimed := claimTurn(t, r, "token")
	captured := threadResponse(claimed)
	if err := r.CaptureTurn(ctx, "token", captured); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := Open(root, p)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	// The captured result outlives the session that reserved the turn: only
	// completion may close the turn.
	if err := r.AbandonTurn(ctx, streamID, "mason", "one", at.Add(time.Minute)); !errors.Is(err, ErrClaim) {
		t.Fatalf("captured turn abandoned: %v", err)
	}
	th := mustThread(t, r)
	q := th.Turns[0]
	if th.Active != "one" || th.Status != "captured" || q.Status() != "captured" || !q.CompletedAt.IsZero() || q.Response == nil || !reflect.DeepEqual(*q.Response, captured) {
		t.Fatalf("captured turn changed: %+v", th)
	}
	if responses, err := Read[TurnResponse](r, streamID); err != nil || len(responses) != 1 || !reflect.DeepEqual(responses[0], captured) {
		t.Fatalf("owned log: %+v %v", responses, err)
	}
}

func TestRecordDocumentsRefusesUnpublishablePaths(t *testing.T) {
	r, root, p := create(t)
	ctx := context.Background()
	dir, err := root.ProjectTrace(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.RecordDocuments(ctx, []Document{projectDocument("subsystem-trace", "kb/trace.md", "# trace\n", 1)}); err != nil {
		t.Fatal(err)
	}
	commits := gitOutput(t, r, "rev-list", "--count", "HEAD")
	for _, path := range []string{"kb/foo.bar.md", "kb/nested/foo.md", "kb/foo.txt", "docs/foo.md", "charter.md"} {
		batch := []Document{projectDocument("subsystem-trace", "kb/trace.md", "# trace 2\n", 2), projectDocument("subsystem-foo", path, "# foo\n", 1)}
		if err := r.RecordDocuments(ctx, batch); err == nil {
			t.Fatalf("%s recorded", path)
		}
		if got := gitOutput(t, r, "rev-list", "--count", "HEAD"); got != commits {
			t.Fatalf("%s: commits %s, want %s", path, got, commits)
		}
		if _, err := r.dir.Stat(publicationFile); !os.IsNotExist(err) {
			t.Fatalf("%s: journal left behind: %v", path, err)
		}
		if got := projectDocuments(t, r, "subsystem-trace"); len(got) != 1 {
			t.Fatalf("%s: revisions recorded: %+v", path, got)
		}
		if got := kbFiles(t, dir); !reflect.DeepEqual(got, map[string]string{"trace.md": "# trace\n", "entities.json": "{}\n"}) {
			t.Fatalf("%s: files: %v", path, got)
		}
	}
	// The trace reopens cleanly: nothing partial was published.
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(root, p)
	if err != nil {
		t.Fatal(err)
	}
	reopened.Close()
}

func streamDocument(id, path, content string, revision int) Document {
	d := projectDocument(id, path, content, revision)
	d.Workstream = streamID
	return d
}

func TestRecordDocumentsRecordsWorkstreamDocumentsAsOneCommit(t *testing.T) {
	r, root, p := create(t)
	ctx := context.Background()
	dir, err := root.ProjectTrace(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	handed := streamDocument("handed", "handed/design.md", "# Design\n", 1)
	handed.Source = "stdin"
	if err := r.Append(ctx, handed); err != nil {
		t.Fatal(err)
	}
	before := gitOutput(t, r, "rev-list", "--count", "HEAD")
	commits := func() int {
		n, err := strconv.Atoi(gitOutput(t, r, "rev-list", "--count", "HEAD"))
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	created := commits()
	first := []Document{streamDocument("spec", "spec.md", "# Spec\n", 1), streamDocument("plan", "plan.json", "{\"version\":1,\"units\":[]}\n", 1), streamDocument("seal", "seal.json", "{\"seal\":1}\n", 1)}
	if err := r.RecordDocuments(ctx, first); err != nil {
		t.Fatal(err)
	}
	if got := commits(); got != created+1 {
		t.Fatalf("commits after one record: %d, %d before it", got, created)
	}
	streamDir := filepath.Join(dir, "workstreams", string(streamID))
	for name, want := range map[string]string{"spec.md": "# Spec\n", "plan.json": "{\"version\":1,\"units\":[]}\n", "seal.json": "{\"seal\":1}\n"} {
		data, err := os.ReadFile(filepath.Join(streamDir, name))
		if err != nil || string(data) != want {
			t.Fatalf("%s on disk: %q %v", name, data, err)
		}
	}
	docs, err := Read[Document](r, streamID)
	if err != nil || len(docs) != 4 || docs[1].ID != "spec" || docs[2].ID != "plan" || docs[3].ID != "seal" {
		t.Fatalf("revisions: %+v %v", docs, err)
	}
	if project, err := Read[Document](r, ""); err != nil || len(project) != 1 {
		t.Fatalf("project documents changed: %+v %v", project, err)
	}
	// Every revision in the batch is checked before anything is written: mixed
	// scopes, a handed revision, a project path in a workstream, an unknown
	// workstream and a repeated revision are all refused whole.
	other := streamDocument("spec", "spec.md", "x\n", 1)
	other.Workstream = legacyStream
	handed2 := streamDocument("handed", "handed/design.md", "changed\n", 2)
	handed2.Source = "stdin"
	invalid := [][]Document{
		{streamDocument("spec", "spec.md", "x\n", 2), projectDocument("subsystem-trace", "kb/trace.md", "x\n", 1)},
		{streamDocument("spec", "spec.md", "x\n", 2), handed2},
		{streamDocument("spec", "spec.md", "x\n", 2), streamDocument("subsystem-trace", "kb/trace.md", "x\n", 1)},
		{other},
		{streamDocument("spec", "spec.md", "x\n", 1)},
		{streamDocument("spec", "spec.md", "x\n", 2), streamDocument("spec", "spec.md", "y\n", 3)},
	}
	after := gitOutput(t, r, "rev-list", "--count", "HEAD")
	for i, docs := range invalid {
		if err := r.RecordDocuments(ctx, docs); err == nil {
			t.Fatalf("batch %d accepted", i)
		}
		if got := gitOutput(t, r, "rev-list", "--count", "HEAD"); got != after {
			t.Fatalf("batch %d committed: %s (was %s before the first record)", i, got, before)
		}
		if _, err := r.dir.Stat(publicationFile); !os.IsNotExist(err) {
			t.Fatalf("batch %d: journal left behind: %v", i, err)
		}
	}
	second := []Document{streamDocument("spec", "spec.md", "# Spec 2\n", 2), streamDocument("plan", "plan.json", "{\"version\":1,\"units\":[]}\n", 2)}
	if err := r.RecordDocuments(ctx, second); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(streamDir, "spec.md")); err != nil || string(data) != "# Spec 2\n" {
		t.Fatalf("spec.md after revision 2: %q %v", data, err)
	}
	if committed := gitOutput(t, r, "show", "HEAD~1:workstreams/"+string(streamID)+"/spec.md"); committed != "# Spec" {
		t.Fatalf("history lost revision 1: %q", committed)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(root, p)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if docs, err := Read[Document](reopened, streamID); err != nil || len(docs) != 6 {
		t.Fatalf("reopen: %d %v", len(docs), err)
	}
}

// A shed record is a workstream document under shed/round-<n>/: a batch is one
// commit through RecordDocuments, Append records one too, a project-scoped
// shed path is refused, and the trace reopens with the files on disk.
func TestShedRecordsAreWorkstreamDocuments(t *testing.T) {
	r, root, p := create(t)
	ctx := context.Background()
	dir, err := root.ProjectTrace(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	before := gitOutput(t, r, "rev-list", "--count", "HEAD")
	batch := []Document{streamDocument("shed-round-1-agent_committee_1", "shed/round-1/agent_committee_1.json", "{\"round\":1}\n", 1), streamDocument("shed-round-1-agent_committee_2", "shed/round-1/agent_committee_2.json", "{\"round\":1}\n", 1)}
	if err := r.RecordDocuments(ctx, batch); err != nil {
		t.Fatal(err)
	}
	n, err := strconv.Atoi(before)
	if err != nil {
		t.Fatal(err)
	}
	if got := gitOutput(t, r, "rev-list", "--count", "HEAD"); got != strconv.Itoa(n+1) {
		t.Fatalf("commits %s after one batch, %s before it", got, before)
	}
	if err := r.Append(ctx, streamDocument("shed-round-12-agent_committee_1", "shed/round-12/agent_committee_1.json", "{\"round\":12}\n", 1)); err != nil {
		t.Fatal(err)
	}
	project := projectDocument("shed-round-1-m", "shed/round-1/m.json", "{}\n", 1)
	if err := r.RecordDocuments(ctx, []Document{project}); err == nil {
		t.Fatal("project-scoped shed record accepted")
	}
	if err := r.Append(ctx, project); err == nil {
		t.Fatal("project-scoped shed record appended")
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(root, p)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	docs, err := Read[Document](reopened, streamID)
	if err != nil || len(docs) != 3 {
		t.Fatalf("documents: %+v %v", docs, err)
	}
	for _, d := range docs {
		data, err := os.ReadFile(filepath.Join(dir, "workstreams", string(streamID), filepath.FromSlash(d.Path)))
		if err != nil || string(data) != d.Content {
			t.Fatalf("%s on disk: %q %v", d.Path, data, err)
		}
		if got := gitOutput(t, reopened, "cat-file", "blob", "HEAD:workstreams/"+string(streamID)+"/"+d.Path); got != strings.TrimSpace(d.Content) {
			t.Fatalf("%s committed as %q", d.Path, got)
		}
	}
}

func unitReport(content string, revision int) Document {
	h := header("document", "unit-parser-report")
	h.Revision, h.Unit = revision, "parser"
	return Document{Header: h, Path: "units/parser/report.json", Content: content}
}

// A unit report and the transitions recorded with it are one commit, and a
// refused transition records neither.
func TestRecordDocumentsWithTransitionsIsOneCommit(t *testing.T) {
	r, root, p := create(t)
	ctx := context.Background()
	dir, err := root.ProjectTrace(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	before := gitOutput(t, r, "rev-list", "--count", "HEAD")
	refused := [][]Transaction{
		{transaction("stale", 3, "", "reviewing", 0)},
		{func() Transaction {
			tx := transaction("elsewhere", 0, "", "reviewing", 0)
			tx.Transition.Workstream = legacyStream
			return tx
		}()},
	}
	for i, txs := range refused {
		if _, err := r.RecordDocumentsWith(ctx, []Document{unitReport("{}\n", 1)}, txs...); err == nil {
			t.Fatalf("batch %d accepted", i)
		}
		if got := gitOutput(t, r, "rev-list", "--count", "HEAD"); got != before {
			t.Fatalf("batch %d committed: %s", i, got)
		}
	}
	if _, err := r.RecordDocumentsWith(ctx, []Document{projectDocument("subsystem-trace", "kb/trace.md", "# trace\n", 1)}); err == nil {
		t.Fatal("project documents recorded with a workflow")
	}
	states, err := r.RecordDocumentsWith(ctx, []Document{unitReport("{\"turn\":1}\n", 1)}, transaction("reviewing", 0, "", "reviewing", 1))
	if err != nil {
		t.Fatal(err)
	}
	if want := []WorkflowState{{Version: 1, Value: "reviewing"}}; !reflect.DeepEqual(states, want) {
		t.Fatalf("states %+v", states)
	}
	if got := gitOutput(t, r, "rev-list", "--count", "HEAD"); got != fmt.Sprint(mustAtoi(t, before)+1) {
		t.Fatalf("commits %s, %s before", got, before)
	}
	files := gitOutput(t, r, "show", "--name-only", "--format=", "HEAD")
	for _, want := range []string{"workstreams/" + string(streamID) + "/units/parser/report.json", "workstreams/" + string(streamID) + "/events.jsonl", "workstreams/" + string(streamID) + "/documents.jsonl"} {
		if !strings.Contains(files, want) {
			t.Fatalf("the commit lacks %s:\n%s", want, files)
		}
	}
	if data, err := os.ReadFile(filepath.Join(dir, "workstreams", string(streamID), "units", "parser", "report.json")); err != nil || string(data) != "{\"turn\":1}\n" {
		t.Fatalf("report file %q %v", data, err)
	}
	if got, err := r.Workflow(streamID, "feature"); err != nil || got.Value != "reviewing" {
		t.Fatalf("workflow %+v %v", got, err)
	}
	if _, err := Get[Document](r, streamID, "unit-parser-report", 1); err != nil {
		t.Fatal(err)
	}
	// A unit's landing, rebase, change and dispatch decision are unit
	// documents; other paths under units/ are not.
	for _, path := range []string{"units/parser/other.json", "units/parser", "units/../report.json", "units/a/b/report.json"} {
		d := unitReport("{}\n", 2)
		d.ID, d.Path = "unit-other", path
		if err := r.RecordDocuments(ctx, []Document{d}); err == nil {
			t.Fatalf("path %s accepted", path)
		}
	}
	landing := unitReport("{\"commit\":\"c\"}\n", 1)
	landing.ID, landing.Path = "unit-parser-landing", "units/parser/landing.json"
	if err := r.RecordDocuments(ctx, []Document{landing}); err != nil {
		t.Fatal(err)
	}
	rebase := unitReport("{\"commit\":\"c\"}\n", 1)
	rebase.ID, rebase.Path = "unit-parser-rebase", "units/parser/rebase.json"
	if err := r.RecordDocuments(ctx, []Document{rebase}); err != nil {
		t.Fatal(err)
	}
	change := unitReport("{\"change\":\"c\"}\n", 1)
	change.ID, change.Path = "unit-parser-change", "units/parser/change.json"
	if err := r.RecordDocuments(ctx, []Document{change}); err != nil {
		t.Fatal(err)
	}
	dispatch := unitReport("{\"decision\":\"deferred\"}\n", 1)
	dispatch.ID, dispatch.Path = "unit-parser-dispatch", "units/parser/dispatch.json"
	if err := r.RecordDocuments(ctx, []Document{dispatch}); err != nil {
		t.Fatal(err)
	}
}

func mustAtoi(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatal(err)
	}
	return n
}
