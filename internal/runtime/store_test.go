package runtime

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
)

const pid config.ProjectID = "p_0123456789abcdef0123456789abcdef"
const w1 config.WorkstreamID = "w_0123456789abcdef0123456789abcdef"
const w2 config.WorkstreamID = "w_1123456789abcdef0123456789abcdef"

func fixture(t *testing.T) Inputs {
	t.Helper()
	// Short roots keep Unix socket paths within the loader's portable limit.
	home, err := os.MkdirTemp("", "osmia-runtime-")
	must(t, err)
	t.Cleanup(func() { os.RemoveAll(home) })
	root := filepath.Join(home, "root")
	must(t, os.MkdirAll(filepath.Join(root, "projects", string(pid)), 0700))
	must(t, os.WriteFile(filepath.Join(root, "config.toml"), []byte(fmt.Sprintf(`version = 1
active_projects = [%q]
[profiles.default]
agent = "claude"
model = "test"
[profiles.other]
agent = "codex"
model = "test-other"
`, pid)), 0600))
	must(t, os.WriteFile(filepath.Join(root, "projects", string(pid), "config.toml"), []byte(`version = 1
upstream = "upstream/repo"
fork = "owner/repo"
clone = "`+filepath.Join(home, "clone")+`"
`), 0600))
	c, err := config.Load(config.Options{Root: root})
	must(t, err)
	return Inputs{c, []config.WorkstreamID{w1, w2}}
}
func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func open(t *testing.T, in Inputs) *Store {
	t.Helper()
	s, _, err := Open(in)
	must(t, err)
	t.Cleanup(func() { s.Close() })
	return s
}
func disk(t *testing.T, in Inputs) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(in.Config.Root.String(), "runtime.json"))
	must(t, err)
	return b
}
func pauses() []Pause {
	return []Pause{
		{Target: Target{Scope: "factory"}, Mode: "soft", Source: "operator", Reason: "travelling"},
		{Target: Target{Scope: "project", Project: pid}, Mode: "hard", Source: "operator"},
		{Target: Target{Scope: "workstream", Project: pid, Workstream: w1}, Mode: "soft", Source: "operator"},
	}
}
func TestRoundTripsAndChangedDefaults(t *testing.T) {
	in := fixture(t)
	s := open(t, in)
	configPath, _ := in.Config.Root.Config()
	original, err := os.ReadFile(configPath)
	must(t, err)
	effective, ds := s.Effective()
	if len(ds) != 0 || len(effective.Pauses) != 0 || len(effective.Priorities) != 0 || effective.Profiles["mason"] != "default" {
		t.Fatal(effective, ds)
	}
	if _, err := os.Stat(filepath.Join(in.Config.Root.String(), "runtime.json")); !os.IsNotExist(err) {
		t.Fatal("load created runtime file")
	}
	for _, p := range pauses() {
		must(t, s.SetPause(p))
	}
	must(t, s.SetPriority(Priority{pid, []config.WorkstreamID{w2, w1}}))
	must(t, s.SetProfile("mason", "other"))
	before, _ := s.Snapshot()
	restarted := open(t, in)
	after, ds := restarted.Snapshot()
	if !reflect.DeepEqual(before, after) || len(ds) != 0 {
		t.Fatal(before, after, ds)
	}
	effective, _ = restarted.Effective()
	if effective.Profiles["mason"] != "other" || !reflect.DeepEqual(effective.Priorities[0].Workstreams, []config.WorkstreamID{w2, w1}) || len(effective.Pauses) != 3 {
		t.Fatal(effective)
	}
	unchanged, err := os.ReadFile(configPath)
	must(t, err)
	if !bytes.Equal(original, unchanged) {
		t.Fatal("mutation rewrote config")
	}
	// Load changed declarative input through the actual loader.
	must(t, os.WriteFile(configPath, append(original, []byte("\n[roles.mason]\nprofile = 'other'\n")...), 0600))
	changed, err := config.Load(config.Options{Root: in.Config.Root.String()})
	must(t, err)
	in.Config = changed
	must(t, restarted.SetProfile("mason", "default"))
	must(t, restarted.Resolve(in))
	effective, _ = restarted.Effective()
	if effective.Profiles["mason"] != "default" {
		t.Fatal("resolve erased override")
	}
	must(t, restarted.ClearProfile("mason"))
	must(t, restarted.ClearPriority(pid))
	for _, p := range pauses() {
		must(t, restarted.ClearPause(p.Target))
	}
	cleared := open(t, in)
	effective, ds = cleared.Effective()
	if effective.Profiles["mason"] != "other" || len(effective.Pauses) != 0 || len(effective.Priorities) != 0 || len(ds) != 0 {
		t.Fatal(effective, ds)
	}
}
func TestStaleReferencesAreRetainedAndNeverRetargeted(t *testing.T) {
	in := fixture(t)
	s := open(t, in)
	must(t, s.SetPause(pauses()[0]))
	must(t, s.SetPause(pauses()[2]))
	must(t, s.SetPriority(Priority{pid, []config.WorkstreamID{w1, w2}}))
	must(t, s.SetProfile("mason", "other"))
	saved := disk(t, in)
	changed, _ := copyInputs(in)
	delete(changed.Config.Profiles, "other")
	changed.Workstreams = []config.WorkstreamID{w2}
	must(t, s.Resolve(changed))
	effective, ds := s.Effective()
	if len(ds) != 3 || len(effective.Pauses) != 1 || effective.Profiles["mason"] != "default" || !reflect.DeepEqual(effective.Priorities[0].Workstreams, []config.WorkstreamID{w2}) {
		t.Fatal(effective, ds)
	}
	restarted := open(t, changed)
	_, ds = restarted.Snapshot()
	if len(ds) != 3 {
		t.Fatal(ds)
	}
	must(t, restarted.SetProfile("reviewer", "default"))
	raw, _ := restarted.Snapshot()
	if len(raw.Pauses) != 2 || raw.Profiles["mason"] != "other" || len(raw.Priorities[0].Workstreams) != 2 {
		t.Fatal("stale entries lost", raw)
	}
	must(t, restarted.Resolve(in))
	_, ds = restarted.Effective()
	if len(ds) != 0 {
		t.Fatal(ds)
	}
	// A different project must not inherit any project-scoped records.
	changed, _ = copyInputs(in)
	changed.Config.Project.ID = "p_2123456789abcdef0123456789abcdef"
	changed.Config.ActiveProjects = []string{string(changed.Config.Project.ID)}
	must(t, s.Resolve(changed))
	effective, ds = s.Effective()
	if len(ds) != 2 || len(effective.Pauses) != 1 || len(effective.Priorities) != 0 {
		t.Fatal(effective, ds)
	}
	if bytes.Equal(saved, disk(t, in)) {
		t.Fatal("expected reviewer mutation to persist")
	}
	must(t, s.Resolve(in))
}
func TestInvalidFiles(t *testing.T) {
	for _, body := range []string{
		`null`, `{`, `{"version":2,"version":1}`, `{"version":1,"profiles":{"mason":"default","mason":"other"}}`, `{"version":2}`, `{"version":1} {}`, `{"version":1,"unknown":true}`,
		`{"version":1,"pauses":[{"target":{"scope":"factory"},"mode":"nope","source":"operator"}]}`,
		`{"version":1,"pauses":[{"target":{"scope":"factory","project":"` + string(pid) + `"},"mode":"soft","source":"operator"}]}`,
		`{"version":1,"pauses":[{"target":{"scope":"factory"},"mode":"soft","source":"budget"}]}`,
		`{"version":1,"priorities":[{"project":"` + string(pid) + `","workstreams":["` + string(w1) + `","` + string(w1) + `"]}]}`,
		`{"version":1,"priorities":[{"project":"../bad","workstreams":[]}]}`,
		`{"version":1,"profiles":{"mason":""}}`,
	} {
		t.Run(body, func(t *testing.T) {
			in := fixture(t)
			must(t, os.WriteFile(filepath.Join(in.Config.Root.String(), "runtime.json"), []byte(body), 0600))
			s, _, err := Open(in)
			if err == nil || s != nil {
				t.Fatalf("accepted %s", body)
			}
			if string(disk(t, in)) != body {
				t.Fatal("failed load modified file")
			}
		})
	}
}
func TestRejectedMutationsPreserveState(t *testing.T) {
	in := fixture(t)
	s := open(t, in)
	must(t, s.SetProfile("mason", "other"))
	saved := disk(t, in)
	before, _ := s.Snapshot()
	bads := []func() error{
		func() error { return s.SetProfile("fake", "default") }, func() error { return s.SetProfile("mason", "missing") }, func() error { return s.SetProfile("mason", "") },
		func() error {
			return s.SetPause(Pause{Target: Target{Scope: "factory"}, Mode: "unknown", Source: "operator"})
		},
		func() error {
			return s.SetPause(Pause{Target: Target{Scope: "workstream", Project: pid, Workstream: "w_3123456789abcdef0123456789abcdef"}, Mode: "soft", Source: "operator"})
		},
		func() error { return s.SetPriority(Priority{pid, []config.WorkstreamID{w1, w1}}) },
		func() error {
			return s.SetPriority(Priority{pid, []config.WorkstreamID{"w_3123456789abcdef0123456789abcdef"}})
		},
		func() error { return s.SetPriority(Priority{pid, nil}) },
	}
	for i, bad := range bads {
		if bad() == nil {
			t.Fatalf("accepted mutation %d", i)
		}
		after, _ := s.Snapshot()
		if !reflect.DeepEqual(before, after) || !bytes.Equal(saved, disk(t, in)) {
			t.Fatal("failure changed state")
		}
	}
	in.Config.Roles["mason"] = config.Role{Profile: "default", Sandbox: "claude"}
	must(t, s.Resolve(in))
	if s.SetProfile("mason", "other") == nil {
		t.Fatal("incompatible sandbox accepted")
	}
}
func TestPersistenceFailures(t *testing.T) {
	injected := errors.New("injected")
	for _, absent := range []bool{false, true} {
		for _, stage := range []string{"encode", "write", "short write", "flush", "rename", "directory sync"} {
			t.Run(fmt.Sprint(absent)+stage, func(t *testing.T) {
				in := fixture(t)
				s := open(t, in)
				var saved []byte
				if !absent {
					must(t, s.SetProfile("mason", "other"))
					saved = disk(t, in)
				}
				before, _ := s.Snapshot()
				switch stage {
				case "encode":
					s.ops.encode = func(State) ([]byte, error) { return nil, injected }
				case "write":
					s.ops.write = func(f *os.File, b []byte) (int, error) { n, _ := f.Write(b[:len(b)/2]); return n, injected }
				case "short write":
					s.ops.write = func(f *os.File, b []byte) (int, error) { return f.Write(b[:len(b)/2]) }
				case "flush":
					s.ops.sync = func(*os.File) error { return injected }
				case "rename":
					s.ops.rename = func(*os.Root, string, string) error { return injected }
				case "directory sync":
					s.ops.syncDir = func(*os.File) error { return injected }
				}
				if err := s.SetPause(pauses()[0]); err == nil {
					t.Fatal("failure acknowledged")
				}
				after, _ := s.Snapshot()
				if !reflect.DeepEqual(before, after) {
					t.Fatal("failure installed state")
				}
				if absent {
					if _, err := os.Stat(filepath.Join(in.Config.Root.String(), "runtime.json")); !os.IsNotExist(err) {
						t.Fatal("failed first mutation created file")
					}
				} else if !bytes.Equal(saved, disk(t, in)) {
					t.Fatal("previous file not restored")
				}
				reopened := open(t, in)
				after, _ = reopened.Snapshot()
				if !reflect.DeepEqual(before, after) {
					t.Fatal("restart changed state")
				}
				entries, err := filepath.Glob(filepath.Join(in.Config.Root.String(), ".runtime-*"))
				must(t, err)
				if len(entries) != 0 {
					t.Fatal("temporary files leaked", entries)
				}
				s.ops = defaultFileOps()
				must(t, s.SetPause(pauses()[0]))
			})
		}
	}
}
func TestConcurrentMutationsAndReaderIsolation(t *testing.T) {
	in := fixture(t)
	for i := 0; i < 30; i++ {
		in.Workstreams = append(in.Workstreams, config.WorkstreamID(fmt.Sprintf("w_%032x", i)))
	}
	s := open(t, in)
	var wg sync.WaitGroup
	for _, w := range in.Workstreams {
		wg.Go(func() {
			err := s.SetPause(Pause{Target: Target{Scope: "workstream", Project: pid, Workstream: w}, Mode: "soft", Source: "operator"})
			if err != nil {
				t.Error(err)
			}
			raw, _ := s.Snapshot()
			if err := validate(raw); err != nil {
				t.Error(err)
			}
			raw.Pauses[0].Mode = "corrupted"
			effective, _ := s.Effective()
			effective.Profiles["mason"] = "corrupted"
		})
	}
	wg.Wait()
	restarted := open(t, in)
	raw, _ := restarted.Snapshot()
	if len(raw.Pauses) != len(in.Workstreams) {
		t.Fatalf("lost updates: %d", len(raw.Pauses))
	}
	for _, p := range raw.Pauses {
		if p.Mode != "soft" {
			t.Fatal(p)
		}
	}
	effective, _ := s.Effective()
	if effective.Profiles["mason"] != "default" {
		t.Fatal("reader mutated state")
	}
	// Identical state encodes byte-for-byte identically.
	before := disk(t, in)
	must(t, s.SetPause(raw.Pauses[0]))
	if !bytes.Equal(before, disk(t, in)) {
		t.Fatal("nondeterministic output")
	}
}
func TestUnknownReferencesDiagnosedOnLoad(t *testing.T) {
	in := fixture(t)
	st := State{Version: Version, Profiles: map[string]string{"fake": "default", "mason": "missing"}, Pauses: []Pause{{Target: Target{Scope: "workstream", Project: pid, Workstream: "w_3123456789abcdef0123456789abcdef"}, Mode: "soft", Source: "operator"}}}
	data, err := json.Marshal(st)
	must(t, err)
	must(t, os.WriteFile(filepath.Join(in.Config.Root.String(), "runtime.json"), data, 0600))
	s, loadDiagnostics, err := Open(in)
	must(t, err)
	t.Cleanup(func() { s.Close() })
	if len(loadDiagnostics) != 3 {
		t.Fatal(loadDiagnostics)
	}
	effective, ds := s.Effective()
	if len(ds) != 3 || len(effective.Pauses) != 0 || effective.Profiles["mason"] != "default" {
		t.Fatal(effective, ds)
	}
	must(t, s.ClearProfile("fake"))
	must(t, s.ClearProfile("mason"))
	must(t, s.ClearPause(st.Pauses[0].Target))
	_, ds = s.Snapshot()
	if len(ds) != 0 {
		t.Fatal(ds)
	}
}
func TestFilesystemBoundariesAndInputIsolation(t *testing.T) {
	in := fixture(t)
	s := open(t, in)
	in.Workstreams[0] = "invalid"
	in.Config.Roles["mason"] = config.Role{Profile: "invalid"}
	must(t, s.SetPause(pauses()[2]))
	effective, _ := s.Effective()
	if effective.Profiles["mason"] != "default" {
		t.Fatal("input aliased")
	}
	path := filepath.Join(in.Config.Root.String(), "runtime.json")
	saved := disk(t, in)
	must(t, os.Remove(path))
	must(t, os.Symlink("config.toml", path))
	if s.SetProfile("reviewer", "default") == nil {
		t.Fatal("symlink mutation accepted")
	}
	if st, _, err := Open(in); err == nil || st != nil {
		t.Fatal("symlink load accepted")
	}
	must(t, os.Remove(path))
	must(t, os.WriteFile(path, append(saved, ' '), 0600))
	if err := s.SetProfile("reviewer", "default"); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatal(err)
	}
}
