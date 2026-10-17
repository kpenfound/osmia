package trace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
)

const projectID config.ProjectID = "p_00000000000000000000000000000001"
const streamID config.WorkstreamID = "w_00000000000000000000000000000001"

var at = time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
var owner = Actor{Kind: "owner", ID: "local"}

func fixture(t *testing.T) (config.Root, config.Project) {
	t.Helper()
	base := t.TempDir()
	root, err := config.ResolveRoot(filepath.Join(base, "osmia"), "")
	if err != nil {
		t.Fatal(err)
	}
	clone := filepath.Join(base, "target")
	if err := os.Mkdir(clone, 0700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "init", "--quiet", clone)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TEMPLATE_DIR="}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	return root, config.Project{ID: projectID, Clone: clone}
}
func create(t *testing.T) (*Repository, config.Root, config.Project) {
	t.Helper()
	root, p := fixture(t)
	r, err := Create(context.Background(), root, p, at, owner)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	if err := r.CreateWorkstream(context.Background(), streamID, at, owner); err != nil {
		t.Fatal(err)
	}
	return r, root, p
}

// mustScoped returns the workstream's own trace instance, the way every
// workstream-scoped write and read resolves it: its own directory, lock and
// database, independent of the project's.
func mustScoped(t *testing.T, r *Repository, stream config.WorkstreamID) *Repository {
	t.Helper()
	s, err := r.scoped(stream)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func header(kind, id string) Header {
	return Header{Schema: "osmia.trace." + kind, Version: Version, ID: id, Revision: 1, Project: projectID, Workstream: streamID, At: at, Actor: Actor{Kind: "service", ID: "controller"}, Cause: "owner-message-1", Depth: 2}
}
func specimens() []Record {
	return []Record{
		Document{Header: header("document", "spec"), Path: "spec.md", Content: "# Feature\n"},
		Transition{Header: header("transition", "transition1"), Subject: "workstream", From: "active", To: "abandoned", Reason: "Owner ended work"},
		Question{Header: header("question", "question1"), AskedBy: Actor{Kind: "agent", ID: "mason1"}, Question: "Which behavior?", SentToOwner: "Choose an option"},
		Ruling{Header: header("ruling", "ruling1"), QuestionID: "question1", QuestionRevision: 1, Decision: "Use local files", OwnerResponse: "Local files", ReturnedAnswer: "Use local files", Changes: []string{"spec"}},
		Agent{Header: header("agent", "mason1"), Role: "mason", ThreadID: "thread1", Session: coreadapter.BackendSession{Backend: "fake", ID: "session1"}},
		TurnRequest{Header: header("turn-request", "request1"), AgentID: "mason1", ThreadID: "thread1", TurnID: "turn1", Profile: coreadapter.Profile{Name: "default", Backend: "fake", Model: "test", Effort: "medium", Timeout: time.Minute, MaxTurns: 4, CostLimitUSD: 1}, Resume: &coreadapter.BackendSession{Backend: "fake", ID: "session1"}, SystemPrompt: "Scoped role", Prompt: "Build the unit", History: "Earlier context"},
		TurnResponse{Header: header("turn-response", "response1"), AgentID: "mason1", ThreadID: "thread1", TurnID: "turn1", RequestID: "request1", RequestRevision: 1, Result: coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "fake", ID: "session1"}, FinalResponse: "Finished", StartedAt: at, Duration: time.Second, Outcome: &coreadapter.Outcome{Status: "complete", Report: "Local test passes"}, Usage: coreadapter.Usage{CostUSD: 0.25, CostKnown: true, Turns: 1}}},
		Cost{Header: header("cost", "cost1"), Entry: coreadapter.LedgerEntry{Scope: coreadapter.Scope{Project: string(projectID), Workstream: string(streamID), Unit: "unit1", Thread: "thread1", Turn: "turn1", Role: "mason"}, AttemptID: "attempt1", At: at, Usage: coreadapter.Usage{CostUSD: 0.25, CostKnown: true, Turns: 1}}},
	}
}
func checkTyped[T Record](t *testing.T, r *Repository, want Record) {
	t.Helper()
	list, err := Read[T](r, streamID)
	if err != nil {
		t.Fatal(err)
	}
	// Every workstream carries its chief-of-staff identity alongside the specimen.
	list = slices.DeleteFunc(list, func(v T) bool {
		a, ok := any(v).(Agent)
		return ok && a.ID == ChiefOfStaff
	})
	if len(list) != 1 || !reflect.DeepEqual(list[0], want) {
		t.Fatalf("read: got %#v, want %#v", list, want)
	}
	got, err := Get[T](r, streamID, want.header().ID, 1)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("get: got %#v, %v", got, err)
	}
}
func TestCreateRoundTripReopen(t *testing.T) {
	r, root, p := create(t)
	s := mustScoped(t, r, streamID)
	// Each trace directory holds its database and lock, and nothing else.
	for _, dir := range []string{r.directory, s.directory} {
		for _, name := range []string{databaseFile, lockFile} {
			if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
				t.Fatal(err)
			}
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if !slices.Contains(storeFiles, e.Name()) {
				t.Fatalf("%s holds %s", dir, e.Name())
			}
		}
	}
	for _, v := range specimens() {
		if err := r.Append(context.Background(), v); err != nil {
			t.Fatalf("append %T: %v", v, err)
		}
	}
	before, beforeStream := r.directory, s.directory
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	// Typed reads do not need the target clone or a backend transcript.
	if err := os.RemoveAll(p.Clone); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(root, p)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.directory != before || reopened.Project() != p.ID {
		t.Fatal("reopened identity changed")
	}
	streams, err := reopened.Workstreams()
	if err != nil || !reflect.DeepEqual(streams, []config.WorkstreamID{streamID}) {
		t.Fatalf("streams: %v, %v", streams, err)
	}
	reopenedStream := mustScoped(t, reopened, streamID)
	if reopenedStream.directory != beforeStream {
		t.Fatal("reopened workstream identity changed")
	}
	all := specimens()
	checkTyped[Document](t, reopened, all[0])
	checkTyped[Transition](t, reopened, all[1])
	checkTyped[Question](t, reopened, all[2])
	checkTyped[Ruling](t, reopened, all[3])
	checkTyped[Agent](t, reopened, all[4])
	checkTyped[TurnRequest](t, reopened, all[5])
	checkTyped[TurnResponse](t, reopened, all[6])
	checkTyped[Cost](t, reopened, all[7])
}
func TestRevisionsAndTerminalHistory(t *testing.T) {
	r, _, _ := create(t)
	s := mustScoped(t, r, streamID)
	doc := specimens()[0].(Document)
	terminal := specimens()[1]
	if err := r.Append(context.Background(), doc); err != nil {
		t.Fatal(err)
	}
	if err := r.Append(context.Background(), terminal); err != nil {
		t.Fatal(err)
	}
	doc.Revision = 2
	doc.Content = "# Revised feature\n"
	doc.Cause = "owner-message-2"
	if err := r.Append(context.Background(), doc); err != nil {
		t.Fatal(err)
	}
	docs, err := Read[Document](r, streamID)
	if err != nil || len(docs) != 2 || docs[0].Content != "# Feature\n" || docs[1].Content != doc.Content {
		t.Fatalf("history: %#v %v", docs, err)
	}
	if current := fileOf(t, s, "workstreams/"+string(streamID)+"/spec.md"); current != doc.Content {
		t.Fatalf("latest document: %s", current)
	}
	checkTyped[Transition](t, r, terminal)
	doc.Revision = 4
	if err := r.Append(context.Background(), doc); !errors.Is(err, ErrConflict) {
		t.Fatalf("revision gap accepted: %v", err)
	}
	doc.Revision = 2
	if err := r.Append(context.Background(), doc); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate revision accepted: %v", err)
	}
	doc.Revision = 3
	doc.Path = "plan.json"
	if err := r.Append(context.Background(), doc); !errors.Is(err, ErrConflict) {
		t.Fatalf("document identity moved: %v", err)
	}
}
func TestProjectDocumentsAndImmutableInput(t *testing.T) {
	r, _, _ := create(t)
	d := Document{Header: header("document", "charter"), Path: "charter.md", Content: "Owner's charter"}
	d.Workstream = ""
	d.Revision = 2
	if err := r.Append(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	got, err := Get[Document](r, "", "charter", 2)
	if err != nil || !reflect.DeepEqual(got, d) {
		t.Fatalf("project doc: %#v %v", got, err)
	}
	d.Revision = 3
	d.Source = "stdin"
	if err := r.Append(context.Background(), d); err == nil {
		t.Fatal("source accepted on a document that was not handed in")
	}
	d.Header = header("document", "input1")
	d.Revision = 1
	d.Path = "handed/design.jsonl"
	for _, source := range []string{" ", "file:/a\nb", "file:/a\rb", "file:/a\x00b"} {
		d.Source = source
		if err := r.Append(context.Background(), d); err == nil {
			t.Fatalf("source %q accepted", source)
		}
	}
	d.Source = "file:/home/owner/design.jsonl"
	if err := r.Append(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if got, err := Get[Document](r, streamID, "input1", 1); err != nil || !reflect.DeepEqual(got, d) {
		t.Fatalf("handed doc: %#v %v", got, err)
	}
	d.Revision = 2
	if err := r.Append(context.Background(), d); !errors.Is(err, ErrConflict) {
		t.Fatalf("handed content changed: %v", err)
	}
	d.ID = "input2"
	d.Revision = 1
	if err := r.Append(context.Background(), d); !errors.Is(err, ErrConflict) {
		t.Fatalf("second input identity accepted: %v", err)
	}
}
func TestInvalidRecordsDoNotWrite(t *testing.T) {
	r, _, _ := create(t)
	s := mustScoped(t, r, streamID)
	baseline, streamBaseline := commits(t, r), commits(t, s)
	for _, mutate := range []func(*Document){
		func(d *Document) { d.Version = 2 }, func(d *Document) { d.Schema = "unknown" }, func(d *Document) { d.ID = "../bad" }, func(d *Document) { d.Project = "invalid" }, func(d *Document) { d.Workstream = "/tmp/escape" }, func(d *Document) { d.At = time.Time{} }, func(d *Document) { d.Actor = Actor{} }, func(d *Document) { d.Cause = "" }, func(d *Document) { d.Depth = -1 },
	} {
		d := specimens()[0].(Document)
		mutate(&d)
		if err := r.Append(context.Background(), d); err == nil {
			t.Fatalf("invalid header accepted: %#v", d)
		}
	}
	for _, p := range []string{"/tmp/trace-escape", "../outside", "handed/../../outside", "handed//file", "handed/./file", "handed/.git", ".git/config", "kb/../../../outside", "spec.md/child", "handed/a\\b", "handed/a\x00b",
		"shed/round-1/../../../outside.json", "shed/round-1/../spec.md", "shed/../round-1/m.json", "shed/round-1/.git.json", "shed/round-1/m/../n.json", "shed/round-1/nested/m.json", "shed/round-1/m.md", "shed/round-1/m",
		"shed/round-0/m.json", "shed/round-01/m.json", "shed/round-x/m.json", "shed/round-1", "shed/m.json", "shed/round-1/ m.json", "shed/round-1/a b.json", "shed/round-1/m.json/child", "shed/round-1/a\\b.json"} {
		d := specimens()[0].(Document)
		d.Path = p
		if err := r.Append(context.Background(), d); err == nil {
			t.Fatalf("invalid path %q accepted", p)
		}
	}
	bad := specimens()
	q := bad[2].(Question)
	q.AskedBy = Actor{}
	bad[2] = q
	ruling := bad[3].(Ruling)
	ruling.QuestionRevision = 0
	bad[3] = ruling
	agent := bad[4].(Agent)
	agent.ThreadID = ""
	bad[4] = agent
	request := bad[5].(TurnRequest)
	request.Profile.Backend = ""
	bad[5] = request
	response := bad[6].(TurnResponse)
	response.RequestID = ""
	bad[6] = response
	cost := bad[7].(Cost)
	cost.Entry.Scope.Project = "other"
	bad[7] = cost
	for _, v := range bad[2:] {
		if err := r.Append(context.Background(), v); err == nil {
			t.Fatalf("invalid %T accepted", v)
		}
	}
	if after, streamAfter := commits(t, r), commits(t, s); after != baseline || streamAfter != streamBaseline {
		t.Fatalf("rejected record committed: %d/%d, %d/%d", after, baseline, streamAfter, streamBaseline)
	}
	docs, err := Read[Document](r, streamID)
	if err != nil || len(docs) != 0 {
		t.Fatalf("rejected record wrote records: %v %v", docs, err)
	}
}
func TestSeparationAndBoundaryRejection(t *testing.T) {
	for _, mode := range []string{"same", "root-in-clone", "clone-in-root", "trace-symlink", "clone-alias", "invalid-project"} {
		t.Run(mode, func(t *testing.T) {
			root, p := fixture(t)
			trace, _ := root.ProjectTrace(p.ID)
			outside := t.TempDir()
			switch mode {
			case "same":
				p.Clone = trace
			case "root-in-clone":
				p.Clone = filepath.Dir(root.String())
			case "clone-in-root":
				p.Clone = filepath.Join(root.String(), "clone")
			case "trace-symlink":
				if err := os.MkdirAll(filepath.Dir(trace), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, trace); err != nil {
					t.Fatal(err)
				}
			case "clone-alias":
				if err := os.MkdirAll(root.String(), 0700); err != nil {
					t.Fatal(err)
				}
				alias := filepath.Join(outside, "clone")
				if err := os.Symlink(root.String(), alias); err != nil {
					t.Fatal(err)
				}
				p.Clone = alias
			case "invalid-project":
				p.ID = "../outside"
			}
			if r, err := Create(context.Background(), root, p, at, owner); err == nil {
				r.Close()
				t.Fatal("unsafe trace accepted")
			}
			entries, err := os.ReadDir(outside)
			if err != nil {
				t.Fatal(err)
			}
			expected := 0
			if mode == "clone-alias" {
				expected = 1
			}
			if len(entries) != expected {
				t.Fatalf("writes outside root: %v", entries)
			}
		})
	}
}
func TestExistingTraceAndSecondWriter(t *testing.T) {
	root, p := fixture(t)
	r, err := Create(context.Background(), root, p, at, owner)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if second, err := Create(context.Background(), root, p, at, owner); err == nil {
		second.Close()
		t.Fatal("existing trace initialized again")
	}
	if second, err := Open(root, p); !errors.Is(err, ErrLocked) {
		if second != nil {
			second.Close()
		}
		t.Fatalf("second writer: %v", err)
	}
}
func TestCorruptionPreservesValidHistory(t *testing.T) {
	for _, damage := range []string{"malformed", "version", "identity", "schema", "duplicate-key", "unknown-field", "partial-body", "revision-gap", "path"} {
		t.Run(damage, func(t *testing.T) {
			r, root, p := create(t)
			s := mustScoped(t, r, streamID)
			d := specimens()[0].(Document)
			if err := r.Append(context.Background(), d); err != nil {
				t.Fatal(err)
			}
			next := d
			next.Revision = 2
			switch damage {
			case "version":
				next.Version = 99
			case "identity":
				next.Project = "p_00000000000000000000000000000002"
			case "schema":
				next.Schema = "osmia.trace.future"
			case "revision-gap":
				next.Revision = 4
			}
			body, _ := json.Marshal(next)
			switch damage {
			case "malformed":
				body = []byte("{broken")
			case "duplicate-key":
				body = append([]byte(`{"id":"other",`), body[1:]...)
			case "unknown-field":
				body = append([]byte(`{"surprise":true,`), body[1:]...)
			case "partial-body":
				body = body[:len(body)-1]
			}
			name := recordPath(d)
			if damage == "path" {
				// A valid record stored under another record's path.
				name = "workstreams/" + string(streamID) + "/events.jsonl"
			}
			storeExec(t, s, "INSERT INTO records (commit_seq, path, schema, id, revision, body) VALUES ((SELECT max(seq) FROM commits), ?, ?, ?, ?, ?)", name, next.Schema, next.ID, next.Revision, string(body))
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := Open(root, p)
			if err == nil || !strings.Contains(err.Error(), name+"#") {
				t.Fatalf("missing damaged-row diagnostic: %v", err)
			}
			if reopened == nil {
				t.Fatal("corruption discarded readable handle")
			}
			defer reopened.Close()
			docs, err := Read[Document](reopened, streamID)
			if err == nil || len(docs) != 1 || !reflect.DeepEqual(docs[0], d) {
				t.Fatalf("valid history not retained alongside error: %#v %v", docs, err)
			}
			if damage != "path" {
				if err := reopened.Append(context.Background(), next); err == nil {
					t.Fatal("append accepted over corrupt history")
				}
			}
			stored := storedBodies(t, mustScoped(t, reopened, streamID), name)
			if stored[len(stored)-1] != string(body) {
				t.Fatal("corrupt row silently repaired")
			}
		})
	}
}

func TestConcurrentAppendAndFailedCommit(t *testing.T) {
	r, root, p := create(t)
	s := mustScoped(t, r, streamID)
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wg.Go(func() { results <- r.Append(context.Background(), specimens()[0]) })
	}
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		} else if !errors.Is(err, ErrConflict) {
			t.Fatal(err)
		}
	}
	if successes != 1 {
		t.Fatalf("duplicate append successes: %d", successes)
	}
	// A commit that fails before the database commits records nothing.
	d := specimens()[0].(Document)
	d.Revision = 2
	d.Content = "pending"
	injected := errors.New("injected commit failure")
	before := commits(t, s)
	s.failPublication = func(step string) error {
		if step == boundaryBeforeCommit {
			return injected
		}
		return nil
	}
	if err := r.Append(context.Background(), d); !errors.Is(err, injected) {
		t.Fatalf("failed commit: %v", err)
	}
	s.failPublication = nil
	if after := commits(t, s); after != before {
		t.Fatalf("a failed commit landed: %d commits, %d before", after, before)
	}
	r.Close()
	reopened, err := Open(root, p)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	docs, err := Read[Document](reopened, streamID)
	if err != nil || len(docs) != 1 || docs[0].Content != "# Feature\n" {
		t.Fatalf("history after a failed commit: %#v %v", docs, err)
	}
	if err := reopened.Append(context.Background(), d); err != nil {
		t.Fatalf("retry of the failed commit: %v", err)
	}
}

// A cancelled caller commits nothing.
func TestAppendWithACancelledContextCommitsNothing(t *testing.T) {
	r, _, _ := create(t)
	s := mustScoped(t, r, streamID)
	before := commits(t, s)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := r.Append(ctx, specimens()[0]); !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled append: %v", err)
	}
	if after := commits(t, s); after != before {
		t.Fatalf("a cancelled append committed: %d commits, %d before", after, before)
	}
	if docs, err := Read[Document](r, streamID); err != nil || len(docs) != 0 {
		t.Fatalf("a cancelled append recorded: %#v %v", docs, err)
	}
}

// TestWorkstreamLockDoesNotSerializeAnotherWorkstreamOrTheProject is the
// isolation separate traces give: each workstream's own trace is its own
// database with its own lock, so a write that happens to hold one
// workstream's own lock for a long time never serializes a write to another
// workstream or a read of the project's own trace, since neither shares a
// lock or a database with it.
func TestWorkstreamLockDoesNotSerializeAnotherWorkstreamOrTheProject(t *testing.T) {
	r, _, _ := create(t)
	second := config.WorkstreamID("w_00000000000000000000000000000002")
	if err := r.CreateWorkstream(context.Background(), second, at, owner); err != nil {
		t.Fatal(err)
	}
	busy := mustScoped(t, r, second)
	holding := make(chan struct{})
	release := make(chan struct{})
	go func() {
		busy.mu.Lock()
		close(holding)
		<-release
		busy.mu.Unlock()
	}()
	<-holding
	defer close(release)

	done := make(chan error, 1)
	go func() {
		started := time.Now()
		if err := r.Append(context.Background(), specimens()[0]); err != nil {
			done <- err
			return
		}
		if _, err := r.Charter(context.Background()); err != nil {
			done <- err
			return
		}
		if elapsed := time.Since(started); elapsed > time.Second {
			done <- fmt.Errorf("waited %s on another workstream's own lock", elapsed)
			return
		}
		done <- nil
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a write to one workstream and a read of the project's own trace waited on another workstream's own lock")
	}
}

// Observe is called once, on the project instance, before or after a
// workstream instance exists. Either way it must still hear that
// workstream's own commits: service.go relies on this to publish the event
// stream every workstream write feeds, and a missed call here previously
// left API and test subscribers waiting on a slow fallback poll instead of
// the commit's own wake.
func TestObserveReportsWorkstreamCommitsOnTheProjectInstance(t *testing.T) {
	r, _, _ := create(t)
	// CreateWorkstream, inside create, already opened and cached the
	// workstream's own instance before Observe is called below.
	var commits []Commit
	r.Observe(func(c Commit) { commits = append(commits, c) })
	if err := r.Append(context.Background(), specimens()[0]); err != nil {
		t.Fatal(err)
	}
	if len(commits) != 1 || !slices.Contains(commits[0].Paths, "workstreams/"+string(streamID)+"/documents.jsonl") {
		t.Fatalf("observed commits: %+v", commits)
	}
	// A workstream opened after Observe was called delegates just the same.
	second := config.WorkstreamID("w_00000000000000000000000000000002")
	if err := r.CreateWorkstream(context.Background(), second, at, owner); err != nil {
		t.Fatal(err)
	}
	commits = nil
	h := header("document", "spec")
	h.Workstream = second
	if err := r.Append(context.Background(), Document{Header: h, Path: "spec.md", Content: "# Feature\n"}); err != nil {
		t.Fatal(err)
	}
	if len(commits) != 1 || !slices.Contains(commits[0].Paths, "workstreams/"+string(second)+"/documents.jsonl") {
		t.Fatalf("observed commits from the second workstream instance: %+v", commits)
	}
}

func TestReopenAfterProcessExit(t *testing.T) {
	if os.Getenv("OSMIA_TRACE_TEST_CHILD") == "1" {
		root, err := config.ResolveRoot(os.Getenv("OSMIA_TRACE_TEST_ROOT"), "")
		if err != nil {
			t.Fatal(err)
		}
		p := config.Project{ID: projectID, Clone: os.Getenv("OSMIA_TRACE_TEST_CLONE")}
		r, err := Create(context.Background(), root, p, at, owner)
		if err != nil {
			t.Fatal(err)
		}
		if err := r.CreateWorkstream(context.Background(), streamID, at, owner); err != nil {
			t.Fatal(err)
		}
		if err := r.Append(context.Background(), specimens()[1]); err != nil {
			t.Fatal(err)
		}
		os.Exit(0) // The OS must release the trace lock without Close.
	}
	root, p := fixture(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestReopenAfterProcessExit$")
	cmd.Env = append(os.Environ(), "OSMIA_TRACE_TEST_CHILD=1", "OSMIA_TRACE_TEST_ROOT="+root.String(), "OSMIA_TRACE_TEST_CLONE="+p.Clone)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("child: %v %s", err, out)
	}
	r, err := Open(root, p)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	checkTyped[Transition](t, r, specimens()[1])
}

func TestExistingConfigurationIsNotCommitted(t *testing.T) {
	root, p := fixture(t)
	name, err := root.ProjectConfig(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(name), 0700); err != nil {
		t.Fatal(err)
	}
	data := []byte("version = 1\n")
	if err := os.WriteFile(name, data, 0600); err != nil {
		t.Fatal(err)
	}
	r, err := Create(context.Background(), root, p, at, owner)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	got, err := os.ReadFile(name)
	if err != nil || string(got) != string(data) {
		t.Fatalf("configuration changed: %s %v", got, err)
	}
	var stored int
	if err := r.db.QueryRow("SELECT count(*) FROM files WHERE path LIKE '%config.toml%'").Scan(&stored); err != nil || stored != 0 {
		t.Fatalf("configuration included in the trace: %d %v", stored, err)
	}
}

func TestCreateSeededRecordsEntityMap(t *testing.T) {
	root, p := fixture(t)
	if _, err := CreateSeeded(context.Background(), root, p, at, owner, nil); err == nil {
		t.Fatal("empty seed accepted")
	}
	if _, err := os.Lstat(filepath.Join(root.String(), "projects", string(p.ID))); !os.IsNotExist(err) {
		t.Fatalf("empty seed created a trace: %v", err)
	}
	seed := "{\"version\": 1, \"entities\": []}\n"
	r, err := CreateSeeded(context.Background(), root, p, at, owner, []byte(seed))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	docs, err := Read[Document](r, "")
	want := Document{Header: Header{Schema: "osmia.trace.document", Version: Version, ID: EntitiesDocument, Revision: 1, Project: p.ID, At: at, Actor: owner, Cause: "project-create"}, Path: EntitiesPath, Content: seed}
	if err != nil || len(docs) != 2 || docs[0].ID != "charter" || !reflect.DeepEqual(docs[1], want) {
		t.Fatalf("documents: %#v, %v", docs, err)
	}
	if got := fileOf(t, r, EntitiesPath); got != seed {
		t.Fatalf("latest entity map: %q", got)
	}
}

func TestCloseReleasesALockItsDescriptorsShare(t *testing.T) {
	r, root, p := create(t)
	// A duplicate descriptor shares the lock the way a child process forked
	// but not yet executed does.
	dup, err := syscall.Dup(int(r.lock.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(dup)
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := Open(root, p)
	if err != nil {
		t.Fatalf("reopen after close: %v", err)
	}
	if err := again.Close(); err != nil {
		t.Fatal(err)
	}
}
