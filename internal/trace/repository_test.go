package trace

import (
	"context"
	"encoding/json"
	"errors"
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
	for _, name := range []string{".git", "charter.md", "kb/entities.json", "notes", "workstreams/" + string(streamID) + "/handed", "workstreams/" + string(streamID) + "/shed", "workstreams/" + string(streamID) + "/amendments", "workstreams/" + string(streamID) + "/questions", "workstreams/" + string(streamID) + "/units", "workstreams/" + string(streamID) + "/agents"} {
		if _, err := os.Stat(filepath.Join(r.directory, name)); err != nil {
			t.Fatal(err)
		}
	}
	for _, v := range specimens() {
		if err := r.Append(context.Background(), v); err != nil {
			t.Fatalf("append %T: %v", v, err)
		}
	}
	before := r.directory
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
	all := specimens()
	checkTyped[Document](t, reopened, all[0])
	checkTyped[Transition](t, reopened, all[1])
	checkTyped[Question](t, reopened, all[2])
	checkTyped[Ruling](t, reopened, all[3])
	checkTyped[Agent](t, reopened, all[4])
	checkTyped[TurnRequest](t, reopened, all[5])
	checkTyped[TurnResponse](t, reopened, all[6])
	checkTyped[Cost](t, reopened, all[7])
	if out, err := reopened.git(context.Background(), nil, "fsck", "--full"); err != nil {
		t.Fatalf("git fsck: %v, %s", err, out)
	}
}
func TestRevisionsAndTerminalHistory(t *testing.T) {
	r, _, _ := create(t)
	doc := specimens()[0].(Document)
	terminal := specimens()[1]
	if err := r.Append(context.Background(), doc); err != nil {
		t.Fatal(err)
	}
	if err := r.Append(context.Background(), terminal); err != nil {
		t.Fatal(err)
	}
	oldHead, err := r.git(context.Background(), nil, "rev-parse", "HEAD")
	if err != nil {
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
	oldContent, err := r.git(context.Background(), nil, "show", oldHead+":workstreams/"+string(streamID)+"/spec.md")
	if err != nil || oldContent != "# Feature" {
		t.Fatalf("Git history: %q %v", oldContent, err)
	}
	current, err := r.readFile("workstreams/" + string(streamID) + "/spec.md")
	if err != nil || string(current) != doc.Content {
		t.Fatalf("latest document: %s %v", current, err)
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
	baseline, err := r.git(context.Background(), nil, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*Document){
		func(d *Document) { d.Version = 2 }, func(d *Document) { d.Schema = "unknown" }, func(d *Document) { d.ID = "../bad" }, func(d *Document) { d.Project = "invalid" }, func(d *Document) { d.Workstream = "/tmp/escape" }, func(d *Document) { d.At = time.Time{} }, func(d *Document) { d.Actor = Actor{} }, func(d *Document) { d.Cause = "" }, func(d *Document) { d.Depth = -1 },
	} {
		d := specimens()[0].(Document)
		mutate(&d)
		if err := r.Append(context.Background(), d); err == nil {
			t.Fatalf("invalid header accepted: %#v", d)
		}
	}
	for _, p := range []string{"/tmp/trace-escape", "../outside", "handed/../../outside", "handed//file", "handed/./file", "handed/.git", ".git/config", "kb/../../../outside", "spec.md/child", "handed/a\\b", "handed/a\x00b"} {
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
	after, err := r.git(context.Background(), nil, "rev-parse", "HEAD")
	if err != nil || baseline != after {
		t.Fatalf("rejected record changed Git: %s %s %v", baseline, after, err)
	}
	docs, err := Read[Document](r, streamID)
	if err != nil || len(docs) != 0 {
		t.Fatalf("rejected record wrote files: %v %v", docs, err)
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
func TestSymlinksAndGitRedirection(t *testing.T) {
	for _, mode := range []string{"document", "agent-directory", "inside-alias", "git-directory", "git-config", "git-objects", "hardlink"} {
		t.Run(mode, func(t *testing.T) {
			r, _, p := create(t)
			sentinel := filepath.Join(p.Clone, "sentinel")
			if err := os.WriteFile(sentinel, []byte("untouched"), 0600); err != nil {
				t.Fatal(err)
			}
			name := "workstreams/" + string(streamID) + "/spec.md"
			target := sentinel
			switch mode {
			case "agent-directory":
				name = "workstreams/" + string(streamID) + "/agents"
				target = p.Clone
			case "inside-alias":
				target = filepath.Join(r.directory, "charter.md")
			case "git-directory":
				name = ".git"
				target = filepath.Join(p.Clone, ".git")
			case "git-config":
				name = ".git/config"
			case "git-objects":
				name = ".git/objects"
				target = p.Clone
			}
			full := filepath.Join(r.directory, name)
			if err := os.RemoveAll(full); err != nil {
				t.Fatal(err)
			}
			var err error
			if mode == "hardlink" {
				err = os.Link(sentinel, full)
			} else {
				err = os.Symlink(target, full)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := r.Append(context.Background(), specimens()[0]); err == nil {
				t.Fatal("alias accepted")
			}
			data, err := os.ReadFile(sentinel)
			if err != nil || string(data) != "untouched" {
				t.Fatalf("external file changed: %s %v", data, err)
			}
			if _, err := os.Stat(filepath.Join(p.Clone, "index")); !os.IsNotExist(err) {
				t.Fatalf("Git wrote outside trace: %v", err)
			}
		})
	}
}
func TestGitEnvironmentAndExistingRepositories(t *testing.T) {
	root, p := fixture(t)
	t.Setenv("GIT_DIR", filepath.Join(p.Clone, ".git"))
	t.Setenv("GIT_WORK_TREE", p.Clone)
	t.Setenv("GIT_INDEX_FILE", filepath.Join(p.Clone, "unexpected-index"))
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "core.bare")
	t.Setenv("GIT_CONFIG_VALUE_0", "true")
	r, err := Create(context.Background(), root, p, at, owner)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := os.Stat(filepath.Join(p.Clone, "unexpected-index")); !os.IsNotExist(err) {
		t.Fatalf("inherited Git index: %v", err)
	}
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
	for _, damage := range []string{"malformed", "version", "identity", "schema", "duplicate-key", "unknown-field", "partial-line", "revision-gap", "missing-file"} {
		t.Run(damage, func(t *testing.T) {
			r, root, p := create(t)
			d := specimens()[0].(Document)
			if err := r.Append(context.Background(), d); err != nil {
				t.Fatal(err)
			}
			name := filepath.Join(r.directory, recordPath(d))
			data, err := os.ReadFile(name)
			if err != nil {
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
			line, _ := json.Marshal(next)
			switch damage {
			case "malformed":
				line = []byte("{broken")
			case "duplicate-key":
				line = append([]byte(`{"id":"other",`), line[1:]...)
			case "unknown-field":
				line = append([]byte(`{"surprise":true,`), line[1:]...)
			}
			data = append(data, line...)
			if damage != "partial-line" {
				data = append(data, '\n')
			}
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}
			if damage == "missing-file" {
				err = os.Remove(name)
			} else {
				err = os.WriteFile(name, data, 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			reopened, err := Open(root, p)
			if err == nil || !strings.Contains(err.Error(), "documents.jsonl") {
				t.Fatalf("missing affected-file diagnostic: %v", err)
			}
			if reopened == nil {
				t.Fatal("corruption discarded readable handle")
			}
			defer reopened.Close()
			docs, err := Read[Document](reopened, streamID)
			expected := 1
			if damage == "missing-file" {
				expected = 0
			}
			if err == nil || len(docs) != expected {
				t.Fatalf("valid history not retained alongside error: %#v %v", docs, err)
			}
			if err := reopened.Append(context.Background(), next); err == nil {
				t.Fatal("append accepted corrupt history")
			}
			if damage != "missing-file" {
				after, _ := os.ReadFile(name)
				if string(after) != string(data) {
					t.Fatal("corrupt log silently repaired")
				}
			}
		})
	}
}
func TestConcurrentAppendAndFailedCommit(t *testing.T) {
	r, root, p := create(t)
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
	// A failed Git write is retained as explicit, uncommitted history.
	d := specimens()[0].(Document)
	d.Revision = 2
	d.Content = "pending"
	// This index lock permits read-only history checks, then refuses the commit.
	lock := filepath.Join(r.directory, ".git/index.lock")
	if err := os.WriteFile(lock, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := r.Append(context.Background(), d); err == nil {
		t.Fatal("Git failure reported success")
	}
	if err := os.Remove(lock); err != nil {
		t.Fatal(err)
	}
	r.Close()
	reopened, err := Open(root, p)
	if err == nil || !strings.Contains(err.Error(), "reconciliation required") {
		t.Fatalf("interrupted commit lost: %v", err)
	}
	if reopened == nil {
		t.Fatal("pending records unreadable")
	}
	defer reopened.Close()
	docs, err := Read[Document](reopened, streamID)
	if err != nil || len(docs) != 2 || docs[1].Content != "pending" {
		t.Fatalf("pending history: %#v %v", docs, err)
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

func TestUncommittedNewLog(t *testing.T) {
	r, root, p := create(t)
	lock := filepath.Join(r.directory, ".git/index.lock")
	if err := os.WriteFile(lock, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := r.Append(context.Background(), specimens()[4]); err == nil {
		t.Fatal("Git failure reported success")
	}
	if err := os.Remove(lock); err != nil {
		t.Fatal(err)
	}
	r.Close()
	reopened, err := Open(root, p)
	if err == nil || !strings.Contains(err.Error(), "identity.jsonl: trace file is not committed") {
		t.Fatalf("uncommitted new file went unnoticed: %v", err)
	}
	if reopened == nil {
		t.Fatal("uncommitted history lost")
	}
	defer reopened.Close()
	checkTyped[Agent](t, reopened, specimens()[4])
	if err := reopened.Append(context.Background(), specimens()[0]); err == nil {
		t.Fatal("append allowed before reconciliation")
	}
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
	files, err := r.git(context.Background(), nil, "ls-tree", "-r", "--name-only", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(files, "config.toml") {
		t.Fatal("configuration included in trace commit")
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
	data, err := os.ReadFile(filepath.Join(r.directory, EntitiesPath))
	if err != nil || string(data) != seed {
		t.Fatalf("file: %q, %v", data, err)
	}
	if err := r.checkHistory(context.Background()); err != nil {
		t.Fatal(err)
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
