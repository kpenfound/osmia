package trace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
)

// legacyRecords returns a realistic, varied record set for stream: a
// document, a notes transition, a question and its ruling, an agent thread
// and one of its turns, and a cost entry. secret is woven into the
// document's content and the turn's prompt and response, so a test can
// confirm a migration log never carries it.
func legacyRecords(project config.ProjectID, stream config.WorkstreamID, secret string) []Record {
	h := func(kind, id string) Header {
		return Header{Schema: "osmia.trace." + kind, Version: Version, ID: id, Revision: 1, Project: project, Workstream: stream, At: at, Actor: Actor{Kind: "service", ID: "controller"}, Cause: "owner-message-1", Depth: 2}
	}
	return []Record{
		Document{Header: h("document", "spec"), Path: "spec.md", Content: "# Feature " + string(stream) + "\n" + secret + "\n"},
		Transition{Header: h("transition", "transition1"), Subject: "notes", From: "", To: "noted", Reason: "Owner note"},
		Question{Header: h("question", "question1"), AskedBy: Actor{Kind: "agent", ID: "mason1"}, Question: "Which behavior?", SentToOwner: "Choose an option"},
		Ruling{Header: h("ruling", "ruling1"), QuestionID: "question1", QuestionRevision: 1, Decision: "Use local files", OwnerResponse: "Local files", ReturnedAnswer: "Use local files", Changes: []string{"spec"}},
		Agent{Header: h("agent", "mason1"), Role: "mason", ThreadID: "thread1", Session: coreadapter.BackendSession{Backend: "fake", ID: "session1"}},
		TurnRequest{Header: h("turn-request", "request1"), AgentID: "mason1", ThreadID: "thread1", TurnID: "turn1", Profile: coreadapter.Profile{Name: "default", Backend: "fake", Model: "test", Effort: "medium", Timeout: time.Minute, MaxTurns: 4, CostLimitUSD: 1}, Resume: &coreadapter.BackendSession{Backend: "fake", ID: "session1"}, SystemPrompt: "Scoped role", Prompt: "Build the unit " + secret, History: "Earlier context"},
		TurnResponse{Header: h("turn-response", "response1"), AgentID: "mason1", ThreadID: "thread1", TurnID: "turn1", RequestID: "request1", RequestRevision: 1, Result: coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "fake", ID: "session1"}, FinalResponse: secret, StartedAt: at, Duration: time.Second, Outcome: &coreadapter.Outcome{Status: "complete", Report: "Local test passes"}, Usage: coreadapter.Usage{CostUSD: 0.25, CostKnown: true, Turns: 1}}},
		Cost{Header: h("cost", "cost1"), Entry: coreadapter.LedgerEntry{Scope: coreadapter.Scope{Project: string(project), Workstream: string(stream), Unit: "unit1", Thread: "thread1", Turn: "turn1", Role: "mason"}, AttemptID: "attempt1", At: at, Usage: coreadapter.Usage{CostUSD: 0.25, CostKnown: true, Turns: 1}}},
	}
}

// legacyFixture builds a project trace in the shared layout: every
// workstream's records inside the project's own trace repository, under
// "workstreams/<id>", as every trace was laid out before each workstream had
// its own repository. Each workstream is first built through the ordinary,
// validated write path in its own dedicated repository, then, once extra has
// made any further writes while the trace still opens as a per-workstream
// one, its files are moved into the project's own repository and its
// dedicated repository is discarded, so the fixture holds exactly what
// those calls would have produced under the shared layout. A shared-layout
// project does not open cleanly through the ordinary Open, so every write
// extra needs must happen here, before that collapse.
func legacyFixture(t *testing.T, root config.Root, p config.Project, streams []config.WorkstreamID, secret string, extra func(ctx context.Context, r *Repository)) {
	t.Helper()
	ctx := context.Background()
	r, err := Create(ctx, root, p, at, owner)
	if err != nil {
		t.Fatal(err)
	}
	for _, stream := range streams {
		if err := r.CreateWorkstream(ctx, stream, at, owner); err != nil {
			t.Fatal(err)
		}
		for _, v := range legacyRecords(p.ID, stream, secret) {
			if err := r.Append(ctx, v); err != nil {
				t.Fatalf("workstream %s: append %T: %v", stream, v, err)
			}
		}
		// A second revision exercises that revision history, not only the
		// latest content, survives migration.
		d := legacyRecords(p.ID, stream, secret)[0].(Document)
		d.Revision, d.Content = 2, d.Content+"revision 2\n"
		if err := r.Append(ctx, d); err != nil {
			t.Fatalf("workstream %s: append revision 2: %v", stream, err)
		}
	}
	if extra != nil {
		extra(ctx, r)
	}
	collapseToSharedLayout(t, r, root, p.ID, streams)
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	base, err := root.WorkstreamTraces(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	// A shared-layout project has no destination directory for its traces.
	if err := os.Remove(base); err != nil {
		t.Fatal(err)
	}
}

// collapseToSharedLayout moves every one of streams' dedicated trace
// repositories into r's own trace repository, under "workstreams/<id>", and
// discards the dedicated repositories, reproducing the shared layout from a
// per-workstream one built through the ordinary write path.
func collapseToSharedLayout(t *testing.T, r *Repository, root config.Root, project config.ProjectID, streams []config.WorkstreamID) {
	t.Helper()
	ctx := context.Background()
	for _, stream := range streams {
		s := mustScoped(t, r, stream)
		var paths []string
		err := s.walk(func(parent *os.Root, name string, entry fs.DirEntry) error {
			if entry.IsDir() {
				return nil
			}
			data, readErr := readEntry(parent, name, entry)
			if readErr != nil {
				return readErr
			}
			if writeErr := r.writeFile(name, data); writeErr != nil {
				return writeErr
			}
			paths = append(paths, name)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		sort.Strings(paths)
		if err := r.commit(ctx, paths, "Seed legacy workstream "+string(stream)); err != nil {
			t.Fatal(err)
		}
	}
	r.workstreamsMu.Lock()
	children := r.workstreams
	r.workstreams = nil
	r.workstreamsMu.Unlock()
	for _, s := range children {
		s.Close()
	}
	for _, stream := range streams {
		dir, err := root.WorkstreamTrace(project, stream)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.RemoveAll(dir); err != nil {
			t.Fatal(err)
		}
	}
}

// fileTree reads every file under directory outside ".git", by path relative
// to directory.
func fileTree(t *testing.T, directory string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	err := filepath.WalkDir(directory, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(directory, path)
		if relErr != nil {
			return relErr
		}
		if d.IsDir() {
			if rel == ".git" {
				return fs.SkipDir
			}
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		out[filepath.ToSlash(rel)] = data
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// logCapture collects the lines Migrate reports, safe for concurrent calls.
type logCapture struct {
	mu    sync.Mutex
	lines []string
}

func (c *logCapture) logf(format string, args ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = append(c.lines, fmt.Sprintf(format, args...))
}
func (c *logCapture) all() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string{}, c.lines...)
}

func TestMigrateMovesSharedLayoutWorkstreamsToTheirOwnRepositories(t *testing.T) {
	root, p := fixture(t)
	ctx := context.Background()
	active := config.WorkstreamID("w_a0000000000000000000000000000001")
	delivered := config.WorkstreamID("w_a0000000000000000000000000000002")
	abandoned := config.WorkstreamID("w_a0000000000000000000000000000003")
	archived := config.WorkstreamID("w_a0000000000000000000000000000004")
	dependent := config.WorkstreamID("w_a0000000000000000000000000000005")
	streams := []config.WorkstreamID{active, delivered, abandoned, archived, dependent}
	const secret = "credential-ghp_FAKETOKEN0123456789-and-transcript-DO-NOT-LOG"
	// Give each workstream a feature state and declare one workstream's
	// base on another, exactly as the shared layout would have recorded
	// them, while the trace still opens as a per-workstream one.
	legacyFixture(t, root, p, streams, secret, func(ctx context.Context, r *Repository) {
		if _, err := r.SetWorkstreamBase(ctx, dependent, active, 0, at); err != nil {
			t.Fatal(err)
		}
		for stream, state := range map[config.WorkstreamID]string{active: "building", delivered: "delivered", abandoned: "abandoned", archived: "delivered", dependent: "building"} {
			h := Header{Schema: "osmia.trace.transition", Version: Version, ID: "feature-state", Revision: 1, Project: projectID, Workstream: stream, At: at, Actor: owner, Cause: "test-setup"}
			if _, err := r.SetFeatureState(ctx, h, state, "test setup"); err != nil {
				t.Fatal(err)
			}
		}
	})

	directory, err := root.ProjectTrace(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	before := fileTree(t, directory)

	capture := &logCapture{}
	if err := Migrate(ctx, root, p.ID, capture.logf); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// The shared "workstreams" directory and its Git objects are gone from
	// the project repository; durable project knowledge is unchanged.
	if _, err := os.Lstat(filepath.Join(directory, "workstreams")); !os.IsNotExist(err) {
		t.Fatalf("shared workstreams directory survived migration: %v", err)
	}
	for _, name := range []string{"charter.md", "project.json", "kb/entities.json"} {
		if !bytes.Equal(before[name], fileTree(t, directory)[name]) {
			t.Fatalf("%s changed by migration", name)
		}
	}
	gitCheck, err := openStoreDirectory(root, p.ID, "", directory)
	if err != nil {
		t.Fatal(err)
	}
	defer gitCheck.Close()
	names, err := gitCheck.git(ctx, nil, "rev-list", "--objects", "--all")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(names, "workstreams") {
		t.Fatalf("a Git object of the project repository still names a workstream path:\n%s", names)
	}

	// A second call finds nothing left to do.
	if err := Migrate(ctx, root, p.ID, capture.logf); err != nil {
		t.Fatalf("second migrate: %v", err)
	}

	r, err := Open(root, p)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	got, err := r.Workstreams()
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
	if !reflect.DeepEqual(got, streams) {
		t.Fatalf("workstreams after migration = %v, want %v", got, streams)
	}
	for _, stream := range streams {
		docs, err := Read[Document](r, stream)
		if err != nil {
			t.Fatalf("workstream %s: %v", stream, err)
		}
		var specs []Document
		for _, d := range docs {
			if d.Path == "spec.md" {
				specs = append(specs, d)
			}
		}
		if len(specs) != 2 || specs[0].Revision != 1 || specs[1].Revision != 2 || !strings.Contains(specs[1].Content, secret) || !strings.Contains(specs[1].Content, "revision 2") {
			t.Fatalf("workstream %s: revision history not preserved: %+v", stream, specs)
		}
		questions, err := Read[Question](r, stream)
		if err != nil || len(questions) != 1 {
			t.Fatalf("workstream %s: questions = %v, %v", stream, questions, err)
		}
		rulings, err := Read[Ruling](r, stream)
		if err != nil || len(rulings) != 1 || rulings[0].Decision != "Use local files" {
			t.Fatalf("workstream %s: rulings = %v, %v", stream, rulings, err)
		}
		requests, err := Read[TurnRequest](r, stream)
		if err != nil || len(requests) != 1 {
			t.Fatalf("workstream %s: turn requests = %v, %v", stream, requests, err)
		}
		responses, err := Read[TurnResponse](r, stream)
		if err != nil || len(responses) != 1 {
			t.Fatalf("workstream %s: turn responses = %v, %v", stream, responses, err)
		}
		state, err := r.Workflow(stream, FeatureSubject)
		if err != nil {
			t.Fatalf("workstream %s: feature state: %v", stream, err)
		}
		if state.Value == "" {
			t.Fatalf("workstream %s: no recovered feature state", stream)
		}
		ops, err := r.Operations(stream)
		if err != nil {
			t.Fatalf("workstream %s: operations: %v", stream, err)
		}
		_ = ops // operations and attempts, if any, must at least read cleanly
	}
	base, err := r.WorkstreamBase(dependent)
	if err != nil || base.Base != active {
		t.Fatalf("dependent workstream's base = %+v, %v", base, err)
	}

	// Logs name the project and workstreams but never the secret content.
	lines := capture.all()
	if len(lines) == 0 {
		t.Fatal("migration logged nothing")
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, string(p.ID)) {
		t.Fatalf("log does not name the project: %v", lines)
	}
	if !strings.Contains(joined, "starting migration") || !strings.Contains(joined, "migration complete") {
		t.Fatalf("log does not report start and completion: %v", lines)
	}
	for _, stream := range streams {
		if !strings.Contains(joined, string(stream)) {
			t.Fatalf("log does not name workstream %s: %v", stream, lines)
		}
	}
	if strings.Contains(joined, secret) || strings.Contains(joined, "ghp_") {
		t.Fatalf("log carries transcript content or a credential-shaped value: %v", lines)
	}
}

// TestMigrateResumesAfterInterruptionAtEachStep interrupts a fresh migration
// right after each journaled step, copy, verify, switch and each step of
// reclamation, and confirms that resuming after the simulated crash
// completes migration with every record intact and none duplicated.
func TestMigrateResumesAfterInterruptionAtEachStep(t *testing.T) {
	t.Cleanup(func() { migrationInterrupt = nil })
	stream := config.WorkstreamID("w_b0000000000000000000000000000001")
	steps := []string{
		"migration-workstream-copied:" + string(stream),
		"migration-workstream-verified:" + string(stream),
		"migration-workstream-switched:" + string(stream),
		"migration-reclaim-journal-written",
		"migration-reclaim-ref-published",
		"migration-reclaim-files-removed",
		"migration-reclaim-objects-pruned",
	}
	for _, step := range steps {
		t.Run(step, func(t *testing.T) {
			root, p := fixture(t)
			ctx := context.Background()
			legacyFixture(t, root, p, []config.WorkstreamID{stream}, "no-secret-in-this-test", nil)

			migrationInterrupt = func(name string) error {
				if name == step {
					return fmt.Errorf("simulated crash at %s", name)
				}
				return nil
			}
			if err := Migrate(ctx, root, p.ID, nil); err == nil {
				t.Fatalf("migration did not stop at %s", step)
			}
			migrationInterrupt = nil

			if err := Migrate(ctx, root, p.ID, nil); err != nil {
				t.Fatalf("resume after %s: %v", step, err)
			}
			// Migration is idempotent once complete; calling it again finds
			// nothing left to do and changes nothing.
			if err := Migrate(ctx, root, p.ID, nil); err != nil {
				t.Fatalf("migrate once more after %s: %v", step, err)
			}

			r, err := Open(root, p)
			if err != nil {
				t.Fatalf("open after resuming past %s: %v", step, err)
			}
			defer r.Close()
			got, err := r.Workstreams()
			if err != nil || len(got) != 1 || got[0] != stream {
				t.Fatalf("workstreams after resuming past %s = %v, %v", step, got, err)
			}
			docs, err := Read[Document](r, stream)
			if err != nil {
				t.Fatalf("read after resuming past %s: %v", step, err)
			}
			specs := 0
			for _, d := range docs {
				if d.Path == "spec.md" {
					specs++
				}
			}
			if specs != 2 {
				t.Fatalf("resuming past %s lost or duplicated records: %d spec.md revisions, want 2", step, specs)
			}
			questions, err := Read[Question](r, stream)
			if err != nil || len(questions) != 1 {
				t.Fatalf("resuming past %s: questions = %v, %v", step, questions, err)
			}
			directory, err := root.ProjectTrace(p.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(filepath.Join(directory, "workstreams")); !os.IsNotExist(err) {
				t.Fatalf("resuming past %s left the shared workstreams directory behind: %v", step, err)
			}
		})
	}
}

// TestMigrateVerificationFailureLeavesTheSharedTraceAuthoritative corrupts a
// workstream's already-copied repository on disk, so verification fails,
// and confirms Migrate leaves the shared trace exactly as it was: nothing
// is reclaimed, and the shared copy stays the one Open reads.
func TestMigrateVerificationFailureLeavesTheSharedTraceAuthoritative(t *testing.T) {
	root, p := fixture(t)
	ctx := context.Background()
	stream := config.WorkstreamID("w_c0000000000000000000000000000001")
	legacyFixture(t, root, p, []config.WorkstreamID{stream}, "no-secret-in-this-test", nil)

	directory, err := root.ProjectTrace(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	before := fileTree(t, directory)

	// Run the copy step on its own, as a resumed migration would find it
	// already done, then corrupt the copy's working tree so it no longer
	// matches the shared trace it was copied from.
	pr, err := openStoreDirectory(root, p.ID, "", directory)
	if err != nil {
		t.Fatal(err)
	}
	if err := pr.checkGit(); err != nil {
		t.Fatal(err)
	}
	if err := pr.acquire(); err != nil {
		t.Fatal(err)
	}
	if err := copyWorkstreamTrace(ctx, root, p.ID, stream, pr); err != nil {
		t.Fatal(err)
	}
	if err := pr.Close(); err != nil {
		t.Fatal(err)
	}
	newDirectory, err := root.WorkstreamTrace(p.ID, stream)
	if err != nil {
		t.Fatal(err)
	}
	corrupted := filepath.Join(newDirectory, "workstreams", string(stream), "documents.jsonl")
	data, err := os.ReadFile(corrupted)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(corrupted, append(data, []byte(`{"schema":"osmia.trace.document"}`+"\n")...), 0600); err != nil {
		t.Fatal(err)
	}

	capture := &logCapture{}
	err = Migrate(ctx, root, p.ID, capture.logf)
	if err == nil || !errors.Is(err, ErrMigrationIncomplete) {
		t.Fatalf("migrate over a verification failure = %v, want ErrMigrationIncomplete", err)
	}

	after := fileTree(t, directory)
	for name, data := range before {
		if !bytes.Equal(data, after[name]) {
			t.Fatalf("%s changed although verification failed", name)
		}
	}
	if _, err := os.Lstat(filepath.Join(directory, "workstreams", string(stream), "workstream.json")); err != nil {
		t.Fatalf("shared trace is no longer intact after a verification failure: %v", err)
	}
	// The project's trace still uses the shared layout, so the caller must
	// not rely on it through Open until a later Migrate succeeds; it is not
	// expected to open cleanly in the meantime.

	joined := strings.Join(capture.all(), "\n")
	if !strings.Contains(joined, string(stream)) || !strings.Contains(joined, "verification failed") {
		t.Fatalf("the verification failure was not logged with its reason: %v", capture.all())
	}
}
