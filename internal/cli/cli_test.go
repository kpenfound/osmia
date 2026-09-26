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

	"github.com/kpenfound/osmia/internal/config"
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
	must(t, os.MkdirAll(filepath.Join(root, "projects", project), 0700))
	must(t, os.WriteFile(filepath.Join(root, "config.toml"), []byte(fmt.Sprintf(`version=1
active_projects=[%q]
[profiles.default]
agent="claude"
model="test"
[profiles.other]
agent="codex"
model="other"
`, project)), 0600))
	must(t, os.WriteFile(filepath.Join(root, "projects", project, "config.toml"), []byte(fmt.Sprintf(`version=1
upstream="upstream/repo"
fork="owner/repo"
clone=%q
`, filepath.Join(home, "clone"))), 0600))
	return service.Options{Config: config.Options{Root: root}, Workstreams: []config.WorkstreamID{stream}}
}
func invoke(t *testing.T, root string, args ...string) (int, string, string) {
	t.Helper()
	var out, diag bytes.Buffer
	code := Run(context.Background(), append([]string{"--root", root}, args...), strings.NewReader(""), &out, &diag)
	return code, out.String(), diag.String()
}
func successful(t *testing.T, root string, args ...string) string {
	t.Helper()
	code, out, diag := invoke(t, root, args...)
	if code != 0 || diag != "" {
		t.Fatalf("%v: code=%d stdout=%s stderr=%s", args, code, out, diag)
	}
	return out
}
func TestCommandsAndRestart(t *testing.T) {
	opts := fixture(t)
	s, err := service.Start(context.Background(), opts)
	must(t, err)
	t.Cleanup(func() { s.Close() })
	root := opts.Config.Root
	status := successful(t, root, "status", "--json")
	var st struct {
		Health        service.HealthResponse
		Configuration service.ConfigResponse
		Runtime       service.RuntimeResponse
	}
	must(t, json.Unmarshal([]byte(status), &st))
	if !st.Health.Ready || st.Configuration.Effective.Project.ID != project || len(st.Configuration.Digest) != 64 {
		t.Fatal(status)
	}
	if want := []service.ProjectRuntime{{Project: project, ContextMode: "file"}}; !reflect.DeepEqual(st.Runtime.Projects, want) {
		t.Fatal(status)
	}
	text := successful(t, root, "status")
	if !strings.Contains(text, "mason: default source=configuration") {
		t.Fatal("missing configured profile source", text)
	}
	if !strings.Contains(text, "ready=true") {
		t.Fatal("missing health")
	}
	if !strings.Contains(text, "Context: "+project+" context_mode=file\n") {
		t.Fatal(text)
	}
	cases := [][]string{
		{"pause", "all", "--hard", "--reason", "travel"},
		{"pause", project},
		{"pause", stream},
		{"priority", "set", stream},
		{"profiles", "set", "mason", "other"},
	}
	for _, args := range cases {
		result := successful(t, root, append(args, "--json")...)
		var mutation struct {
			Mutation service.MutationResponse
			Runtime  service.RuntimeResponse
		}
		must(t, json.Unmarshal([]byte(result), &mutation))
		if !mutation.Mutation.Applied {
			t.Fatal(result)
		}
	}
	must(t, s.Close())
	s, err = service.Start(context.Background(), opts)
	must(t, err)
	var rt service.RuntimeResponse
	must(t, json.Unmarshal([]byte(successful(t, root, "profiles", "--json")), &rt))
	if len(rt.Effective.Pauses) != 3 || len(rt.Effective.Priorities) != 1 || rt.Effective.Profiles["mason"] != "other" || rt.Profiles["mason"] != (service.EffectiveProfile{Name: "other", Source: "owner_override"}) {
		t.Fatalf("%+v", rt)
	}
	if out := successful(t, root, "profiles"); !strings.Contains(out, "mason: other source=owner_override") {
		t.Fatalf("profiles text: %s", out)
	}
	for _, pause := range rt.Effective.Pauses {
		if pause.Source != "owner" || pause.Reason == "" || pause.SetAt.IsZero() {
			t.Fatalf("pause attribution: %+v", pause)
		}
	}
	shown := successful(t, root, "status")
	if !strings.Contains(shown, `source=owner reason="travel" set_at=`) {
		t.Fatalf("status omits pause attribution: %s", shown)
	}
	for _, args := range [][]string{{"resume", "all"}, {"resume", project}, {"resume", stream}, {"priority", "clear"}, {"profiles", "clear", "mason"}} {
		if !strings.Contains(successful(t, root, args...), "applied=true") {
			t.Fatal(args)
		}
	}
	// Unmarshal into a fresh value because omitted JSON fields retain old values.
	rt = service.RuntimeResponse{}
	must(t, json.Unmarshal([]byte(successful(t, root, "profiles", "--json")), &rt))
	if len(rt.Effective.Pauses) != 0 || len(rt.Effective.Priorities) != 0 || rt.Effective.Profiles["mason"] != "default" || rt.Profiles["mason"] != (service.EffectiveProfile{Name: "default", Source: "configuration"}) {
		t.Fatalf("%+v", rt)
	}
	code, out, diag := invoke(t, root, "profiles", "set", "mason", "missing")
	if code != 4 || out != "" || !strings.Contains(diag, "validation") {
		t.Fatalf("%d %s %s", code, out, diag)
	}
}

func TestClientDoesNotOpenState(t *testing.T) {
	opts := fixture(t)
	// A live service retains its loaded configuration when disk becomes invalid.
	s, err := service.Start(context.Background(), opts)
	must(t, err)
	defer s.Close()
	root := opts.Config.Root
	for _, p := range []string{"config.toml", filepath.Join("projects", project, "config.toml")} {
		must(t, os.Remove(filepath.Join(root, p)))
		must(t, os.Mkdir(filepath.Join(root, p), 0700))
	}
	for _, args := range [][]string{{"status"}, {"profiles"}, {"pause", "all"}, {"resume", "all"}, {"pause", stream}, {"resume", stream}, {"priority", "set", stream}, {"priority", "clear"}, {"profiles", "set", "mason", "other"}, {"profiles", "clear", "mason"}} {
		successful(t, root, args...)
	}
	// Clients can reach a custom socket through a root containing no state at all.
	alias, err := os.MkdirTemp("", "oca-")
	must(t, err)
	defer os.RemoveAll(alias)
	for _, p := range []string{"config.toml", "runtime.json", "projects", "trace"} {
		must(t, os.Mkdir(filepath.Join(alias, p), 0000))
	}
	for _, args := range [][]string{{"status"}, {"profiles"}, {"pause", "all"}, {"resume", "all"}, {"pause", project}, {"resume", project}, {"pause", stream}, {"resume", stream}, {"priority", "set", stream}, {"priority", "clear"}, {"profiles", "set", "mason", "other"}, {"profiles", "clear", "mason"}} {
		successful(t, alias, append(args, "--socket", s.Socket(), "--json")...)
	}
}

func TestUsageAndFailures(t *testing.T) {
	root := fixture(t).Config.Root
	for _, args := range [][]string{nil, {"unknown"}, {"pause"}, {"pause", "nonsense"}, {"resume", "all", "--hard"}, {"serve", "--json"}, {"priority", "set"}, {"profiles", "clear"}, {"status", "--reason", "secret"}, {"status", "--wat"}, {"status", "--json=true"}, {"status", "--root"}, {"status", "--root", "x"}, {"priority", "set", "invalid"}, {"config", "extra"}, {"config", "--hard"}} {
		code, out, diag := invoke(t, root, args...)
		if code != 2 || out != "" || !strings.Contains(diag, "invalid arguments") {
			t.Fatalf("%v: %d %s %s", args, code, out, diag)
		}
	}
	code, out, diag := invoke(t, root, "status")
	if code != 3 || out != "" || !strings.Contains(diag, "osmia serve") {
		t.Fatalf("%d %s %s", code, out, diag)
	}
	if !strings.Contains(successful(t, root, "--help"), "project add") {
		t.Fatal("help")
	}
}

func TestAPIErrorsAreSafe(t *testing.T) {
	root := fixture(t).Config.Root
	socket := filepath.Join(root, "fake.sock")
	ln, err := net.Listen("unix", socket)
	must(t, err)
	var response service.Code
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(409)
		json.NewEncoder(w).Encode(service.ErrorResponse{Error: service.APIError{Code: response, Message: "secret-do-not-print"}})
	})}
	defer server.Close()
	// Requests are sequential; each response completes before the code changes.
	go server.Serve(ln)
	for _, tc := range []struct {
		code service.Code
		exit int
		hint string
	}{
		{service.Malformed, 4, "versions"}, {service.Validation, 4, "IDs"}, {service.Conflict, 5, "restart"},
		{service.Unsupported, 5, "M1"}, {service.RestartRequired, 5, "restart required"}, {service.Unavailable, 3, "osmia serve"}, {service.Internal, 5, "status"},
	} {
		response = tc.code
		code, out, diag := invoke(t, root, "status", "--socket", "fake.sock")
		if code != tc.exit || out != "" || !strings.Contains(diag, tc.hint) || strings.Contains(diag, "secret") {
			t.Fatalf("%s: %d %s %s", tc.code, code, out, diag)
		}
	}
}

func TestServeLifecycle(t *testing.T) {
	for _, defaultRoot := range []bool{false, true} {
		t.Run(fmt.Sprint(defaultRoot), func(t *testing.T) {
			opts := fixture(t)
			root := opts.Config.Root
			if defaultRoot {
				t.Setenv("HOME", filepath.Dir(root))
			}
			socket := filepath.Join(root, "osmia.sock")
			// Leave a stale socket for serve to recover.
			ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
			must(t, err)
			ln.SetUnlinkOnClose(false)
			must(t, ln.Close())
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan int, 1)
			var diag bytes.Buffer
			args := []string{"serve"}
			if !defaultRoot {
				args = append(args, "--root", root)
			}
			go func() { result <- Run(ctx, args, strings.NewReader(""), &bytes.Buffer{}, &diag) }()
			c := service.NewClient(socket)
			defer c.Close()
			deadline := time.Now().Add(5 * time.Second)
			for {
				if _, err := c.Health(context.Background()); err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("serve did not start")
				}
				time.Sleep(10 * time.Millisecond)
			}
			code, out, d := invoke(t, root, "serve")
			if code != 6 || out != "" || !strings.Contains(d, "owner") {
				t.Fatalf("%d %s %s", code, out, d)
			}
			cancel()
			select {
			case code := <-result:
				if code != 0 || diag.Len() != 0 {
					t.Fatalf("%d %s", code, diag.String())
				}
			case <-time.After(5 * time.Second):
				t.Fatal("serve did not stop")
			}
			if _, err := os.Lstat(socket); !os.IsNotExist(err) {
				t.Fatalf("socket left behind: %v", err)
			}
			s, err := service.Start(context.Background(), opts)
			must(t, err)
			must(t, s.Close())
		})
	}
}
