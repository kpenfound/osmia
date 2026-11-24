package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	wire "github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/projectstore"
	"github.com/kpenfound/osmia/internal/service"
)

const project = "p_0123456789abcdef0123456789abcdef"
const stream = "w_0123456789abcdef0123456789abcdef"

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func fixture(t *testing.T) service.Options {
	t.Helper()
	home, err := os.MkdirTemp("", "oc-")
	must(t, err)
	t.Cleanup(func() { os.RemoveAll(home) })
	root := filepath.Join(home, ".osmia")
	must(t, os.MkdirAll(root, 0700))
	must(t, os.WriteFile(filepath.Join(root, "config.toml"), []byte(`version=1
[profiles.default]
agent="claude"
model="test"
[profiles.other]
agent="codex"
model="other"
`), 0600))
	register(t, root, project, fmt.Sprintf("version=1\nupstream=\"upstream/repo\"\nfork=\"owner/repo\"\nclone=%q\n", filepath.Join(home, "clone")))
	return service.Options{Config: config.Options{Root: root}, Workstreams: []config.WorkstreamID{stream}}
}

func register(t *testing.T, rootDirectory, id, settings string) {
	t.Helper()
	root, err := config.ResolveRoot(rootDirectory, "")
	must(t, err)
	store := projectstore.Open(root)
	_, err = store.WriteFirst(config.ProjectID(id), []byte(settings))
	must(t, err)
	must(t, store.Add(config.ProjectID(id)))
}

func invoke(t *testing.T, root string, args ...string) (int, string, string) {
	t.Helper()
	var out, diag bytes.Buffer
	code := Run(context.Background(), append([]string{"--root", root}, args...), strings.NewReader(""), &out, &diag)
	return code, out.String(), diag.String()
}
func poll(t *testing.T, timeout time.Duration, ready func() bool, failure func() string) {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	timedOut := time.After(timeout)
	for !ready() {
		select {
		case <-ticker.C:
		case <-timedOut:
			t.Fatal(failure())
		}
	}
}

func eventually(t *testing.T, timeout time.Duration, check func() (done bool, status string)) {
	t.Helper()
	var status string
	poll(t, timeout, func() bool {
		var done bool
		done, status = check()
		return done
	}, func() string { return status })
}

func awaitHealth(t *testing.T, c *service.Client, up bool, timeout time.Duration, diagnostics func() string) {
	t.Helper()
	poll(t, timeout, func() bool {
		_, err := c.Health(context.Background())
		return (err == nil) == up
	}, func() string {
		if !up {
			return "service did not stop"
		}
		return "service did not start: " + diagnostics()
	})
}

func successful(t *testing.T, root string, args ...string) string {
	t.Helper()
	code, out, diag := invoke(t, root, args...)
	if code != 0 || diag != "" {
		t.Fatalf("%v: code=%d stdout=%s stderr=%s", args, code, out, diag)
	}
	return out
}
func startFixture(t *testing.T) (service.Options, *service.Service) {
	t.Helper()
	opts := fixture(t)
	s, err := service.Start(context.Background(), opts)
	must(t, err)
	t.Cleanup(func() { s.Close() })
	return opts, s
}

func runtimeView(t *testing.T, root string) wire.RuntimeResponse {
	t.Helper()
	var rt wire.RuntimeResponse
	must(t, json.Unmarshal([]byte(successful(t, root, "profiles", "--json")), &rt))
	return rt
}

var ownerMutations = [][]string{
	{"pause", "all", "--hard", "--reason", "travel"},
	{"pause", project},
	{"pause", stream},
	{"priority", "set", stream},
	{"profiles", "set", "mason", "other"},
}

func applyOwnerMutations(t *testing.T, root string) {
	t.Helper()
	for _, args := range ownerMutations {
		result := successful(t, root, append(args, "--json")...)
		var mutation struct {
			Mutation wire.MutationResponse
			Runtime  wire.RuntimeResponse
		}
		must(t, json.Unmarshal([]byte(result), &mutation))
		if !mutation.Mutation.Applied {
			t.Fatal(result)
		}
	}
}

func TestStatusReportsHealthConfigurationAndRuntime(t *testing.T) {
	opts, _ := startFixture(t)
	root := opts.Config.Root
	status := successful(t, root, "status", "--json")
	var st struct {
		Health        wire.HealthResponse
		Configuration wire.ConfigResponse
		Runtime       wire.RuntimeResponse
	}
	must(t, json.Unmarshal([]byte(status), &st))
	if !st.Health.Ready || st.Configuration.Effective.Project.ID != project || len(st.Configuration.Digest) != 64 {
		t.Fatal(status)
	}
	if want := []wire.ProjectRuntime{{Project: project, ContextMode: "file"}}; !reflect.DeepEqual(st.Runtime.Projects, want) {
		t.Fatal(status)
	}
}

func TestStatusTextNamesTheProfileSourceHealthAndContext(t *testing.T) {
	opts, _ := startFixture(t)
	text := successful(t, opts.Config.Root, "status")
	for _, want := range []string{"mason: default source=configuration", "ready=true", "Context: " + project + " context_mode=file\n"} {
		if !strings.Contains(text, want) {
			t.Fatalf("status lacks %q:\n%s", want, text)
		}
	}
}

func TestOwnerMutationsSurviveARestart(t *testing.T) {
	opts, s := startFixture(t)
	root := opts.Config.Root
	applyOwnerMutations(t, root)
	must(t, s.Close())
	s, err := service.Start(context.Background(), opts)
	must(t, err)
	t.Cleanup(func() { s.Close() })
	rt := runtimeView(t, root)
	if len(rt.Effective.Pauses) != 3 || len(rt.Effective.Priorities) != 1 || rt.Effective.Profiles["mason"] != "other" || rt.Profiles["mason"] != (wire.EffectiveProfile{Name: "other", Source: "owner_override"}) {
		t.Fatalf("%+v", rt)
	}
	if out := successful(t, root, "profiles"); !strings.Contains(out, "mason: other source=owner_override") {
		t.Fatalf("profiles text: %s", out)
	}
}

func TestPauseAttributionSurvivesARestart(t *testing.T) {
	opts, s := startFixture(t)
	root := opts.Config.Root
	applyOwnerMutations(t, root)
	must(t, s.Close())
	s, err := service.Start(context.Background(), opts)
	must(t, err)
	t.Cleanup(func() { s.Close() })
	for _, pause := range runtimeView(t, root).Effective.Pauses {
		if pause.Source != "owner" || pause.Reason == "" || pause.SetAt.IsZero() {
			t.Fatalf("pause attribution: %+v", pause)
		}
	}
	if shown := successful(t, root, "status"); !strings.Contains(shown, `source=owner reason="travel" set_at=`) {
		t.Fatalf("status omits pause attribution: %s", shown)
	}
}

func TestClearingOwnerMutationsRestoresTheConfiguration(t *testing.T) {
	opts, _ := startFixture(t)
	root := opts.Config.Root
	applyOwnerMutations(t, root)
	for _, args := range [][]string{{"resume", "all"}, {"resume", project}, {"resume", stream}, {"priority", "clear"}, {"profiles", "clear", "mason"}} {
		if !strings.Contains(successful(t, root, args...), "applied=true") {
			t.Fatal(args)
		}
	}
	rt := runtimeView(t, root)
	if len(rt.Effective.Pauses) != 0 || len(rt.Effective.Priorities) != 0 || rt.Effective.Profiles["mason"] != "default" || rt.Profiles["mason"] != (wire.EffectiveProfile{Name: "default", Source: "configuration"}) {
		t.Fatalf("%+v", rt)
	}
}

func TestProfilesSetRefusesAnUnknownProfile(t *testing.T) {
	opts, _ := startFixture(t)
	code, out, diag := invoke(t, opts.Config.Root, "profiles", "set", "mason", "missing")
	if code != 4 || out != "" || !strings.Contains(diag, "validation") {
		t.Fatalf("%d %s %s", code, out, diag)
	}
}

var clientCommands = [][]string{{"status"}, {"profiles"}, {"pause", "all"}, {"resume", "all"}, {"pause", stream}, {"resume", stream}, {"priority", "set", stream}, {"priority", "clear"}, {"profiles", "set", "mason", "other"}, {"profiles", "clear", "mason"}}

func TestClientCommandsIgnoreTheRootsConfigurationFile(t *testing.T) {
	opts, _ := startFixture(t)
	root := opts.Config.Root
	must(t, os.Remove(filepath.Join(root, "config.toml")))
	must(t, os.Mkdir(filepath.Join(root, "config.toml"), 0700))
	for _, args := range clientCommands {
		successful(t, root, args...)
	}
}

func TestClientCommandsNeedNoReadableRootWhenGivenASocket(t *testing.T) {
	_, s := startFixture(t)
	unreadable, err := os.MkdirTemp("", "oca-")
	must(t, err)
	defer os.RemoveAll(unreadable)
	for _, p := range []string{"config.toml", "runtime.json", "projects", "trace"} {
		must(t, os.Mkdir(filepath.Join(unreadable, p), 0000))
	}
	for _, args := range append(clientCommands, []string{"pause", project}, []string{"resume", project}) {
		successful(t, unreadable, append(args, "--socket", s.Socket(), "--json")...)
	}
}

func TestInvalidArgumentsExitWithUsage(t *testing.T) {
	root := fixture(t).Config.Root
	for _, args := range [][]string{nil, {"unknown"}, {"pause"}, {"pause", "nonsense"}, {"resume", "all", "--hard"}, {"serve", "--json"}, {"priority", "set"}, {"profiles", "clear"}, {"status", "--reason", "secret"}, {"status", "--wat"}, {"status", "--json=true"}, {"status", "--root"}, {"status", "--root", "x"}, {"priority", "set", "invalid"}, {"config", "extra"}, {"config", "--hard"}} {
		code, out, diag := invoke(t, root, args...)
		if code != 2 || out != "" || !strings.Contains(diag, "invalid arguments") {
			t.Fatalf("%v: %d %s %s", args, code, out, diag)
		}
	}
}

func TestCommandsWithoutAServiceNameServe(t *testing.T) {
	root := fixture(t).Config.Root
	code, out, diag := invoke(t, root, "status")
	if code != 3 || out != "" || !strings.Contains(diag, "osmia serve") {
		t.Fatalf("%d %s %s", code, out, diag)
	}
}

func TestHelpListsTheCommands(t *testing.T) {
	root := fixture(t).Config.Root
	if !strings.Contains(successful(t, root, "--help"), "project add") {
		t.Fatal("help")
	}
}

func TestAPIErrorsAreSafe(t *testing.T) {
	root := fixture(t).Config.Root
	socket := filepath.Join(root, "fake.sock")
	ln, err := net.Listen("unix", socket)
	must(t, err)
	var response wire.Code
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(409)
		json.NewEncoder(w).Encode(wire.ErrorResponse{Error: wire.APIError{Code: response, Message: "secret-do-not-print"}})
	})}
	defer server.Close()
	go server.Serve(ln)
	for _, tc := range []struct {
		code wire.Code
		exit int
		hint string
	}{
		{wire.Malformed, 4, "versions"}, {wire.Validation, 4, "IDs"}, {wire.Conflict, 5, "restart"},
		{wire.Unsupported, 5, "supported commands"}, {wire.RestartRequired, 5, "restart required"}, {wire.Unavailable, 3, "osmia serve"}, {wire.Internal, 5, "status"},
	} {
		response = tc.code
		code, out, diag := invoke(t, root, "status", "--socket", "fake.sock")
		if code != tc.exit || out != "" || !strings.Contains(diag, tc.hint) || strings.Contains(diag, "secret") {
			t.Fatalf("%s: %d %s %s", tc.code, code, out, diag)
		}
	}
}

type serveRun struct {
	opts   service.Options
	root   string
	socket string
	cancel context.CancelFunc
	result chan int
	diag   *bytes.Buffer
}

func startServeRun(t *testing.T, defaultRoot bool) *serveRun {
	t.Helper()
	opts := fixture(t)
	root := opts.Config.Root
	if defaultRoot {
		home := filepath.Dir(root)
		t.Setenv("HOME", home)
		t.Setenv("XDG_DATA_HOME", "")
		t.Setenv("XDG_CONFIG_HOME", "")
		data := filepath.Join(home, ".local", "share", "osmia")
		top := filepath.Join(home, ".config", "osmia", "config.toml")
		must(t, os.MkdirAll(filepath.Dir(data), 0700))
		must(t, os.MkdirAll(filepath.Dir(top), 0700))
		must(t, os.Rename(root, data))
		must(t, os.Rename(filepath.Join(data, "config.toml"), top))
		root = data
		opts.Config = config.Options{}
	}
	socket := filepath.Join(root, "osmia.sock")
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	must(t, err)
	ln.SetUnlinkOnClose(false)
	must(t, ln.Close())
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	r := &serveRun{opts: opts, root: root, socket: socket, cancel: cancel, result: make(chan int, 1), diag: &bytes.Buffer{}}
	args := []string{"serve"}
	if !defaultRoot {
		args = append(args, "--root", root)
	}
	go func() { r.result <- Run(ctx, args, strings.NewReader(""), &bytes.Buffer{}, r.diag) }()
	c := service.NewClient(socket)
	t.Cleanup(c.Close)
	awaitHealth(t, c, true, 5*time.Second, r.diag.String)
	return r
}

func (r *serveRun) stop(t *testing.T) int {
	t.Helper()
	r.cancel()
	select {
	case code := <-r.result:
		return code
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not stop")
		return 0
	}
}

func forEachServeRoot(t *testing.T, check func(t *testing.T, r *serveRun)) {
	t.Helper()
	for _, defaultRoot := range []bool{false, true} {
		t.Run(fmt.Sprint(defaultRoot), func(t *testing.T) {
			check(t, startServeRun(t, defaultRoot))
		})
	}
}

func TestServeRefusesASecondServiceOnTheRoot(t *testing.T) {
	forEachServeRoot(t, func(t *testing.T, r *serveRun) {
		code, out, d := invoke(t, r.root, "serve")
		if code != 6 || out != "" || !strings.Contains(d, "owner") {
			t.Fatalf("%d %s %s", code, out, d)
		}
	})
}

func TestServeStopsCleanlyWhenCancelled(t *testing.T) {
	forEachServeRoot(t, func(t *testing.T, r *serveRun) {
		if code := r.stop(t); code != 0 {
			t.Fatalf("%d %s", code, r.diag.String())
		}
	})
}

func TestServeLogsStructuredRecordsToStandardError(t *testing.T) {
	forEachServeRoot(t, func(t *testing.T, r *serveRun) {
		r.stop(t)
		assertStructuredServiceLog(t, r.diag.String())
	})
}

func TestServeRemovesItsSocketWhenItStops(t *testing.T) {
	forEachServeRoot(t, func(t *testing.T, r *serveRun) {
		r.stop(t)
		if _, err := os.Lstat(r.socket); !os.IsNotExist(err) {
			t.Fatalf("socket left behind: %v", err)
		}
	})
}

func TestServeReleasesTheRootWhenItStops(t *testing.T) {
	forEachServeRoot(t, func(t *testing.T, r *serveRun) {
		r.stop(t)
		s, err := service.Start(context.Background(), r.opts)
		must(t, err)
		must(t, s.Close())
	})
}

func assertStructuredServiceLog(t *testing.T, diag string) {
	t.Helper()
	sawStart, sawStop := false, false
	for _, line := range strings.Split(diag, "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("not a JSON object: %q: %v", line, err)
		}
		if _, ok := rec["time"]; !ok {
			t.Fatalf("record missing time: %q", line)
		}
		level, _ := rec["level"].(string)
		if level == "" {
			t.Fatalf("record missing level: %q", line)
		}
		if strings.HasPrefix(level, "ERROR") {
			t.Fatalf("record at ERROR level or above: %q", line)
		}
		msg, _ := rec["msg"].(string)
		if msg == "" {
			t.Fatalf("record missing msg: %q", line)
		}
		switch msg {
		case "service started":
			sawStart = true
		case "service stopped":
			sawStop = true
		}
	}
	if !sawStart || !sawStop {
		t.Fatalf("missing service start/stop records (start=%v stop=%v): %s", sawStart, sawStop, diag)
	}
}
