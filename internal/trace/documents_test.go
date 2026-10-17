package trace

import (
	"context"
	"errors"
	"io/fs"
	"reflect"
	"slices"
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

func TestRecordDocumentsIsOneCommit(t *testing.T) {
	r, root, p := create(t)
	ctx := context.Background()
	first := []Document{projectDocument("subsystem-trace", "kb/trace.md", "# trace\n", 1), projectDocument("subsystem-service", "kb/service.md", "# service\n", 1), projectDocument(EntitiesDocument, EntitiesPath, "{\"version\":1,\"entities\":[]}\n", 1)}
	created := commits(t, r)
	if err := r.RecordDocuments(ctx, first); err != nil {
		t.Fatal(err)
	}
	before := commits(t, r)
	if before != created+1 {
		t.Fatalf("commits after one record: %d, %d before it", before, created)
	}
	if got := kbFiles(t, r); !reflect.DeepEqual(got, map[string]string{"trace.md": "# trace\n", "service.md": "# service\n", "entities.json": "{\"version\":1,\"entities\":[]}\n"}) {
		t.Fatalf("knowledge base: %v", got)
	}
	// Every revision in the batch is checked before anything is written.
	invalid := [][]Document{
		{projectDocument("subsystem-trace", "kb/trace.md", "x\n", 2), projectDocument("subsystem-service", "kb/service.md", "x\n", 3)},
		{projectDocument("subsystem-trace", "kb/trace.md", "x\n", 2), projectDocument("subsystem-other", "kb/trace.md", "x\n", 1)},
		{projectDocument("subsystem-trace", "kb/trace.md", "x\n", 2), projectDocument("subsystem-trace", "kb/trace.md", "y\n", 3)},
		{projectDocument("subsystem-other", "kb/trace.md", "x\n", 1)},
		{projectDocument("charter", "charter.md", "1. Rule\n", 2)},
		{projectDocument("spec", "spec.md", "x\n", 1)},
		{},
	}
	for i, docs := range invalid {
		if err := r.RecordDocuments(ctx, docs); err == nil {
			t.Fatalf("batch %d accepted", i)
		}
		if got := commits(t, r); got != before {
			t.Fatalf("batch %d committed: %d", i, got)
		}
		if got := projectDocuments(t, r, "subsystem-trace"); len(got) != 1 {
			t.Fatalf("batch %d wrote revisions: %+v", i, got)
		}
	}
	if got := kbFiles(t, r); len(got) != 3 || got["trace.md"] != "# trace\n" {
		t.Fatalf("knowledge base after refused batches: %v", got)
	}
	// An empty revision removes the subsystem; its history stays readable.
	second := []Document{projectDocument("subsystem-trace", "kb/trace.md", "# trace 2\n", 2), projectDocument("subsystem-service", "kb/service.md", "", 2), projectDocument(EntitiesDocument, EntitiesPath, "{\"version\":1,\"entities\":[]}\n", 2)}
	if err := r.RecordDocuments(ctx, second); err != nil {
		t.Fatal(err)
	}
	if got := kbFiles(t, r); !reflect.DeepEqual(got, map[string]string{"trace.md": "# trace 2\n", "entities.json": "{\"version\":1,\"entities\":[]}\n"}) {
		t.Fatalf("knowledge base after removal: %v", got)
	}
	if _, err := r.Prose("service"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("removed prose read: %v", err)
	}
	got := projectDocuments(t, r, "subsystem-service")
	if len(got) != 2 || got[0].Content != "# service\n" || got[1].Content != "" {
		t.Fatalf("removal revision: %+v", got)
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

func TestRecordDocumentsPublicationFailures(t *testing.T) {
	for _, step := range []string{boundaryBeforeCommit, boundaryCommitted} {
		t.Run(step, func(t *testing.T) {
			r, root, p := create(t)
			ctx := context.Background()
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
			r, err := Open(root, p)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			committed := step == boundaryCommitted
			want := map[string]string{"trace.md": "# trace\n", "service.md": "# service\n"}
			revisions := 1
			if committed {
				want, revisions = map[string]string{"trace.md": "# trace 2\n"}, 2
			}
			if got := kbFiles(t, r); !reflect.DeepEqual(got, want) {
				t.Fatalf("knowledge base: %v", got)
			}
			if got := projectDocuments(t, r, "subsystem-service"); len(got) != revisions {
				t.Fatalf("revisions: %+v", got)
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

// A crash before or after the database commit leaves a document and the
// transition and outbox entries recorded with it all visible, or none.
func TestRecordDocumentsWithPublicationFailures(t *testing.T) {
	for _, step := range []string{boundaryBeforeCommit, boundaryCommitted} {
		t.Run(step, func(t *testing.T) {
			r, root, p := create(t)
			ctx := context.Background()
			s := mustScoped(t, r, streamID)
			report := unitReport("{\"turn\":1}\n", 1)
			tx := transaction("reviewing", 0, "", "reviewing", 2)
			injected := errors.New("injected publication failure")
			hit := false
			s.failPublication = func(got string) error {
				if got == step {
					hit = true
					return injected
				}
				return nil
			}
			if _, err := r.RecordDocumentsWith(ctx, []Document{report}, tx); !errors.Is(err, injected) || !hit {
				t.Fatalf("injection not reached: %v", err)
			}
			s.failPublication = nil
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}
			r, err := Open(root, p)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			committed := step == boundaryCommitted
			_, docErr := Get[Document](r, streamID, "unit-parser-report", 1)
			state, err := r.Workflow(streamID, "feature")
			if err != nil {
				t.Fatal(err)
			}
			events := ready(t, r, at)
			transitions, err := Read[Transition](r, streamID)
			if err != nil {
				t.Fatal(err)
			}
			if committed {
				if docErr != nil || state.Value != "reviewing" || len(events) != 2 || len(transitions) != 1 {
					t.Fatalf("partly committed: %v %+v %d events %d transitions", docErr, state, len(events), len(transitions))
				}
				return
			}
			if docErr == nil || state.Version != 0 || len(events) != 0 || len(transitions) != 0 {
				t.Fatalf("partly committed: %v %+v %d events %d transitions", docErr, state, len(events), len(transitions))
			}
			if _, err := r.RecordDocumentsWith(ctx, []Document{report}, tx); err != nil {
				t.Fatal(err)
			}
			if got := ready(t, r, at); len(got) != 2 {
				t.Fatalf("retry: %d events", len(got))
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
	if err := r.RecordDocuments(ctx, []Document{projectDocument("subsystem-trace", "kb/trace.md", "# trace\n", 1)}); err != nil {
		t.Fatal(err)
	}
	before := commits(t, r)
	for _, path := range []string{"kb/foo.bar.md", "kb/nested/foo.md", "kb/foo.txt", "docs/foo.md", "charter.md"} {
		batch := []Document{projectDocument("subsystem-trace", "kb/trace.md", "# trace 2\n", 2), projectDocument("subsystem-foo", path, "# foo\n", 1)}
		if err := r.RecordDocuments(ctx, batch); err == nil {
			t.Fatalf("%s recorded", path)
		}
		if got := commits(t, r); got != before {
			t.Fatalf("%s: commits %d, want %d", path, got, before)
		}
		if got := projectDocuments(t, r, "subsystem-trace"); len(got) != 1 {
			t.Fatalf("%s: revisions recorded: %+v", path, got)
		}
		if got := kbFiles(t, r); !reflect.DeepEqual(got, map[string]string{"trace.md": "# trace\n"}) {
			t.Fatalf("%s: knowledge base: %v", path, got)
		}
	}
	// The trace reopens cleanly: nothing partial was committed.
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

// streamFile is the logical path of a workstream document of streamID.
func streamFile(path string) string { return "workstreams/" + string(streamID) + "/" + path }

func TestRecordDocumentsRecordsWorkstreamDocumentsAsOneCommit(t *testing.T) {
	r, root, p := create(t)
	s := mustScoped(t, r, streamID)
	ctx := context.Background()
	handed := streamDocument("handed", "handed/design.md", "# Design\n", 1)
	handed.Source = "stdin"
	if err := r.Append(ctx, handed); err != nil {
		t.Fatal(err)
	}
	created := commits(t, s)
	first := []Document{streamDocument("spec", "spec.md", "# Spec\n", 1), streamDocument("plan", "plan.json", "{\"version\":1,\"units\":[]}\n", 1), streamDocument("seal", "seal.json", "{\"seal\":1}\n", 1)}
	if err := r.RecordDocuments(ctx, first); err != nil {
		t.Fatal(err)
	}
	if got := commits(t, s); got != created+1 {
		t.Fatalf("commits after one record: %d, %d before it", got, created)
	}
	for name, want := range map[string]string{"spec.md": "# Spec\n", "plan.json": "{\"version\":1,\"units\":[]}\n", "seal.json": "{\"seal\":1}\n"} {
		if got := fileOf(t, s, streamFile(name)); got != want {
			t.Fatalf("%s: %q", name, got)
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
	other.Workstream = threadlessStream
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
	after := commits(t, s)
	for i, docs := range invalid {
		if err := r.RecordDocuments(ctx, docs); err == nil {
			t.Fatalf("batch %d accepted", i)
		}
		if got := commits(t, s); got != after {
			t.Fatalf("batch %d committed: %d", i, got)
		}
	}
	second := []Document{streamDocument("spec", "spec.md", "# Spec 2\n", 2), streamDocument("plan", "plan.json", "{\"version\":1,\"units\":[]}\n", 2)}
	if err := r.RecordDocuments(ctx, second); err != nil {
		t.Fatal(err)
	}
	if got := fileOf(t, s, streamFile("spec.md")); got != "# Spec 2\n" {
		t.Fatalf("spec.md after revision 2: %q", got)
	}
	if first, err := Get[Document](r, streamID, "spec", 1); err != nil || first.Content != "# Spec\n" {
		t.Fatalf("history lost revision 1: %+v %v", first, err)
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

// acceptedPath records content at path of streamID as a workstream document
// and refuses an unlisted name beside it.
func acceptedPath(t *testing.T, id, path, content, unlisted string) {
	t.Helper()
	r, _, _ := create(t)
	ctx := context.Background()
	if err := r.RecordDocuments(ctx, []Document{streamDocument(id, path, content, 1)}); err != nil {
		t.Fatal(err)
	}
	if got := fileOf(t, mustScoped(t, r, streamID), streamFile(path)); got != content {
		t.Fatalf("%s: %q", path, got)
	}
	if err := r.RecordDocuments(ctx, []Document{streamDocument(id+"-extra", unlisted, "x\n", 1)}); err == nil {
		t.Fatalf("unlisted path %s accepted", unlisted)
	}
}

func TestOwnPullRequestOutcomeDocumentPathIsAccepted(t *testing.T) {
	acceptedPath(t, "workstream-own-pull-request-outcome", "own-pull-request-outcome.json", "{\"number\":1,\"outcome\":\"merged\"}\n", "own-pull-request-outcome-extra.json")
}

func TestAssumptionsDocumentPathIsAccepted(t *testing.T) {
	acceptedPath(t, "workstream-assumptions", AssumptionsPath, "{\"version\":1,\"assumptions\":[]}\n", "assumptions-extra.json")
}

func TestEdgeObservationDocumentPathIsAccepted(t *testing.T) {
	acceptedPath(t, "workstream-edge-observation", "edge-observation.json", "{\"edges\":[]}\n", "edge-observation-extra.json")
}

// A session's check run and its captured output are recorded under
// session-checks/, mirroring a workstream's own final check runs under
// final/; an unlisted name in the same directory is refused.
func TestSessionCheckDocumentPathIsAccepted(t *testing.T) {
	r, _, _ := create(t)
	ctx := context.Background()
	run := streamDocument("session-check-1", "session-checks/checks-1.json", "{\"run\":1}\n", 1)
	output := streamDocument("session-check-1-output", "session-checks/checks-1-output.txt", "CHECKS\n", 1)
	if err := r.RecordDocuments(ctx, []Document{run, output}); err != nil {
		t.Fatal(err)
	}
	s := mustScoped(t, r, streamID)
	if got := fileOf(t, s, streamFile("session-checks/checks-1.json")); got != "{\"run\":1}\n" {
		t.Fatalf("session-checks/checks-1.json: %q", got)
	}
	if got := fileOf(t, s, streamFile("session-checks/checks-1-output.txt")); got != "CHECKS\n" {
		t.Fatalf("session-checks/checks-1-output.txt: %q", got)
	}
	unlisted := streamDocument("session-check-other", "session-checks/other.json", "x\n", 1)
	if err := r.RecordDocuments(ctx, []Document{unlisted}); err == nil {
		t.Fatal("unlisted name under session-checks/ accepted")
	}
}

// A workstream move is a workstream document at moves/move-<n>.json,
// recorded together with the transitions it makes; other names under moves/
// are refused.
func TestWorkstreamMoveDocumentPathIsAccepted(t *testing.T) {
	r, _, _ := create(t)
	ctx := context.Background()
	move := streamDocument("workstream-move-1", "moves/move-1.json", "{\"move\":1}\n", 1)
	if _, err := r.RecordDocumentsWith(ctx, []Document{move}, transaction("assembled", 0, "", "assembled", 1)); err != nil {
		t.Fatal(err)
	}
	if got := fileOf(t, mustScoped(t, r, streamID), streamFile("moves/move-1.json")); got != "{\"move\":1}\n" {
		t.Fatalf("moves/move-1.json: %q", got)
	}
	for _, path := range []string{"moves/move-0.json", "moves/other.json", "moves/move-1.txt"} {
		if err := r.RecordDocuments(ctx, []Document{streamDocument("workstream-move-other", path, "x\n", 1)}); err == nil {
			t.Fatalf("%s accepted", path)
		}
	}
}

// A shed record is a workstream document under shed/round-<n>/: a batch is one
// commit through RecordDocuments, Append records one too, a project-scoped
// shed path is refused, and the trace reopens with every record readable.
func TestShedRecordsAreWorkstreamDocuments(t *testing.T) {
	r, root, p := create(t)
	s := mustScoped(t, r, streamID)
	ctx := context.Background()
	before := commits(t, s)
	batch := []Document{streamDocument("shed-round-1-agent_committee_1", "shed/round-1/agent_committee_1.json", "{\"round\":1}\n", 1), streamDocument("shed-round-1-agent_committee_2", "shed/round-1/agent_committee_2.json", "{\"round\":1}\n", 1)}
	if err := r.RecordDocuments(ctx, batch); err != nil {
		t.Fatal(err)
	}
	if got := commits(t, s); got != before+1 {
		t.Fatalf("commits %d after one batch, %d before it", got, before)
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
	reopenedStream := mustScoped(t, reopened, streamID)
	for _, d := range docs {
		if got := fileOf(t, reopenedStream, streamFile(d.Path)); got != d.Content {
			t.Fatalf("%s reads %q", d.Path, got)
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
	r, _, _ := create(t)
	s := mustScoped(t, r, streamID)
	ctx := context.Background()
	var observed []Commit
	r.Observe(func(c Commit) { observed = append(observed, c) })
	before := commits(t, s)
	refused := [][]Transaction{
		{transaction("stale", 3, "", "reviewing", 0)},
		{func() Transaction {
			tx := transaction("elsewhere", 0, "", "reviewing", 0)
			tx.Transition.Workstream = threadlessStream
			return tx
		}()},
	}
	for i, txs := range refused {
		if _, err := r.RecordDocumentsWith(ctx, []Document{unitReport("{}\n", 1)}, txs...); err == nil {
			t.Fatalf("batch %d accepted", i)
		}
		if got := commits(t, s); got != before {
			t.Fatalf("batch %d committed: %d", i, got)
		}
	}
	if _, err := r.RecordDocumentsWith(ctx, []Document{projectDocument("subsystem-trace", "kb/trace.md", "# trace\n", 1)}); err == nil {
		t.Fatal("project documents recorded with a workflow")
	}
	if len(observed) != 0 {
		t.Fatalf("refused batches announced commits: %+v", observed)
	}
	states, err := r.RecordDocumentsWith(ctx, []Document{unitReport("{\"turn\":1}\n", 1)}, transaction("reviewing", 0, "", "reviewing", 1))
	if err != nil {
		t.Fatal(err)
	}
	if want := []WorkflowState{{Version: 1, Value: "reviewing"}}; !reflect.DeepEqual(states, want) {
		t.Fatalf("states %+v", states)
	}
	if got := commits(t, s); got != before+1 {
		t.Fatalf("commits %d, %d before", got, before)
	}
	if len(observed) != 1 || observed[0].Workflow == nil {
		t.Fatalf("announced commits: %+v", observed)
	}
	for _, want := range []string{streamFile("units/parser/report.json"), streamFile("events.jsonl"), streamFile("documents.jsonl"), streamFile("workflow.json")} {
		if !slices.Contains(observed[0].Paths, want) {
			t.Fatalf("the commit lacks %s: %v", want, observed[0].Paths)
		}
	}
	if got := fileOf(t, s, streamFile("units/parser/report.json")); got != "{\"turn\":1}\n" {
		t.Fatalf("report %q", got)
	}
	if got, err := r.Workflow(streamID, "feature"); err != nil || got.Value != "reviewing" {
		t.Fatalf("workflow %+v %v", got, err)
	}
	if _, err := Get[Document](r, streamID, "unit-parser-report", 1); err != nil {
		t.Fatal(err)
	}
	// A unit's landing, rebase, change, dispatch decision, check runs, moves
	// and the chief of staff's contest decisions are unit documents; other
	// paths under units/ are not.
	for _, path := range []string{"units/parser/other.json", "units/parser", "units/../report.json", "units/a/b/report.json", "units/parser/chief-.json", "units/parser/chief-a b.json", "units/parser/checks-0.json", "units/parser/checks-x.json", "units/parser/checks-0-output.txt", "units/parser/checks-x-output.txt", "final/checks-0-output.txt", "final/checks-x-output.txt", "units/parser/move-0.json", "units/parser/move-x.json"} {
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
	checks := unitReport("{\"status\":\"passed\"}\n", 1)
	checks.ID, checks.Path = "unit-parser-checks-1", "units/parser/checks-1.json"
	if err := r.RecordDocuments(ctx, []Document{checks}); err != nil {
		t.Fatal(err)
	}
	finalChecks := unitReport("{\"status\":\"passed\"}\n", 1)
	finalChecks.ID, finalChecks.Path = "final-checks-1", "final/checks-1.json"
	if err := r.RecordDocuments(ctx, []Document{finalChecks}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"units/parser/checks-1-output.txt", "final/checks-1-output.txt"} {
		d := unitReport("complete output\n", 1)
		d.ID, d.Path = strings.ReplaceAll(strings.TrimSuffix(path, ".txt"), "/", "-"), path
		if err := r.RecordDocuments(ctx, []Document{d}); err != nil {
			t.Fatal(err)
		}
	}
	finalChecks.ID, finalChecks.Path = "final-checks-x", "final/checks-x.json"
	if err := r.RecordDocuments(ctx, []Document{finalChecks}); err == nil {
		t.Fatal("recorded a final check run without a run number")
	}
	handback := unitReport("{\"drift\":2,\"held\":1}\n", 1)
	handback.ID, handback.Path = "drift-handback-2", "drift/handback-2.json"
	if err := r.RecordDocuments(ctx, []Document{handback}); err != nil {
		t.Fatal(err)
	}
	handback.ID, handback.Path = "drift-handback-x", "drift/handback-x.json"
	if err := r.RecordDocuments(ctx, []Document{handback}); err == nil {
		t.Fatal("recorded a drift handback without a drift rebase number")
	}
	move := unitReport("{\"to\":\"implementing\"}\n", 1)
	move.ID, move.Path = "unit-parser-move-3", "units/parser/move-3.json"
	if err := r.RecordDocuments(ctx, []Document{move}); err != nil {
		t.Fatal(err)
	}
	chief := unitReport("{\"decision\":\"escalate\"}\n", 1)
	chief.ID, chief.Path = "unit-parser-chief-contest-1", "units/parser/chief-contest-1.json"
	if err := r.RecordDocuments(ctx, []Document{chief}); err != nil {
		t.Fatal(err)
	}
}

// draft records the workstream's spec and plan and moves it to state.
func draft(t *testing.T, r *Repository, state string) {
	t.Helper()
	ctx := context.Background()
	if err := r.RecordDocuments(ctx, []Document{streamDocument("spec", "spec.md", "# Spec\n", 1), streamDocument("plan", "plan.json", "{}\n", 1)}); err != nil {
		t.Fatal(err)
	}
	transact(t, r, transaction(state, 0, "", state, 0))
}

func TestEditDraftComparesRevisionsAndChecksContent(t *testing.T) {
	r, root, p := create(t)
	s := mustScoped(t, r, streamID)
	ctx := context.Background()
	draft(t, r, "sketched")
	accept := func(map[string]string) error { return nil }
	later := at.Add(time.Hour)
	before := commits(t, s)
	for _, revisions := range [][2]int{{2, 1}, {1, 2}, {0, 1}} {
		if _, err := r.EditDraft(ctx, streamID, revisions[0], revisions[1], "# Spec 2\n", "{}\n", later, accept); !errors.Is(err, ErrConflict) {
			t.Fatalf("revisions %v: %v", revisions, err)
		}
	}
	refused := errors.New("cyclic plan")
	var checked map[string]string
	if _, err := r.EditDraft(ctx, streamID, 1, 1, "# Spec 2\n", "{\"cycle\":true}\n", later, func(content map[string]string) error {
		checked = content
		return refused
	}); !errors.Is(err, ErrInvalidEdit) || !errors.Is(err, refused) {
		t.Fatalf("invalid draft: %v", err)
	}
	if checked["spec"] != "# Spec 2\n" || checked["plan"] != "{\"cycle\":true}\n" {
		t.Fatalf("checked content: %v", checked)
	}
	// Unchanged content records nothing.
	if got, err := r.EditDraft(ctx, streamID, 1, 1, "# Spec\n", "{}\n", later, accept); err != nil || got["spec"].Revision != 1 || got["plan"].Revision != 1 {
		t.Fatalf("unchanged draft: %+v %v", got, err)
	}
	if got := commits(t, s); got != before {
		t.Fatalf("refused or unchanged edits committed: %d, %d before", got, before)
	}
	// Only the changed document gets a new revision, as the owner's.
	got, err := r.EditDraft(ctx, streamID, 1, 1, "# Spec 2\n", "{}\n", later, accept)
	if err != nil || got["spec"].Revision != 2 || got["spec"].Content != "# Spec 2\n" || got["spec"].Actor != owner || got["spec"].Cause != "owner-edit" || got["plan"].Revision != 1 {
		t.Fatalf("edit: %+v %v", got, err)
	}
	if got := commits(t, s); got != before+1 {
		t.Fatalf("commits %d, %d before", got, before)
	}
	if _, err := r.EditDraft(ctx, streamID, 1, 1, "# Spec 3\n", "{}\n", later, accept); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale edit: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(root, p)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got := fileOf(t, mustScoped(t, reopened, streamID), streamFile("spec.md")); got != "# Spec 2\n" {
		t.Fatalf("spec after reopen: %q", got)
	}
}

func TestEditDraftOnlyBeforeRatification(t *testing.T) {
	accept := func(map[string]string) error { return nil }
	ctx := context.Background()
	t.Run("state", func(t *testing.T) {
		r, _, _ := create(t)
		draft(t, r, "building")
		if _, err := r.EditDraft(ctx, streamID, 1, 1, "# Spec 2\n", "{}\n", at, accept); !errors.Is(err, ErrConflict) {
			t.Fatalf("edit while building: %v", err)
		}
	})
	t.Run("ratified", func(t *testing.T) {
		r, _, _ := create(t)
		draft(t, r, "in-shed")
		if err := r.Append(ctx, streamDocument("shed-round-1-ratification", "shed/round-1/ratification.json", "{}\n", 1)); err != nil {
			t.Fatal(err)
		}
		if _, err := r.EditDraft(ctx, streamID, 1, 1, "# Spec 2\n", "{}\n", at, accept); !errors.Is(err, ErrConflict) {
			t.Fatalf("edit after ratification: %v", err)
		}
	})
}
