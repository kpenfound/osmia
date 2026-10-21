package cli

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	wire "github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/trace"
)

func ratifyServer(t *testing.T, handler http.Handler) (string, string) {
	t.Helper()
	// Unix sockets have a short path limit, including the test name.
	root, err := os.MkdirTemp("", "osmia-cli-")
	must(t, err)
	t.Cleanup(func() { os.RemoveAll(root) })
	socket := filepath.Join(root, "fake.sock")
	listener, err := net.Listen("unix", socket)
	must(t, err)
	server := &http.Server{Handler: handler}
	t.Cleanup(func() { server.Close() })
	go server.Serve(listener)
	return root, socket
}

func TestRatifySendsOwnerReadPinsWithoutFetchingReplacements(t *testing.T) {
	t.Parallel()
	requests := make(chan wire.RatifyRequest, 1)
	root, socket := ratifyServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/v1/ratify/"+stream {
			t.Errorf("explicit decision fetched replacement pins: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		var req wire.RatifyRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		requests <- req
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]any{"error": wire.APIError{Code: wire.Conflict, Message: "the project spec is catching up with the latest member revisions; read the packet again"}})
	}))
	code, out, diag := invoke(t, root, "ratify", stream, "--project-spec-revision", "7", "--spec-revision", "2", "--plan-revision", "3", "--socket", socket)
	if code != 5 || out != "" || !strings.Contains(diag, "read the packet again") {
		t.Fatalf("stale decision: %d %q %q", code, out, diag)
	}
	if req := <-requests; req.Spec != 2 || req.Plan != 3 || req.ProjectSpecRevision != 7 {
		t.Fatalf("ratify replaced owner-read pins: %+v", req)
	}
}

func TestRatifyWithoutOwnerReadProjectRevisionRecordsNothing(t *testing.T) {
	t.Parallel()
	for _, drafted := range []bool{false, true} {
		t.Run(map[bool]string{false: "drafting", true: "drafted"}[drafted], func(t *testing.T) {
			packet := wire.PacketResponse{ProjectSpecRevision: 7}
			packet.Packet.Revision.Spec, packet.Packet.Revision.Plan = 2, 3
			if drafted {
				packet.ProjectSpec = &trace.ProjectSpec{Behavior: "Shared behavior"}
			} else {
				packet.ProjectSpecRevision = 0
				packet.ProjectSpec = &trace.ProjectSpec{}
			}
			root, socket := ratifyServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.URL.Path != "/v1/packet/"+stream {
					t.Errorf("decision sent without owner-read pins: %s %s", r.Method, r.URL.Path)
					http.Error(w, "unexpected request", http.StatusBadRequest)
					return
				}
				json.NewEncoder(w).Encode(packet)
			}))
			code, out, diag := invoke(t, root, "ratify", stream, "--socket", socket)
			if code != 5 || out != "" || !strings.Contains(diag, "Read the packet again") || !strings.Contains(diag, "spec.md revision 2; plan.json revision 3") {
				t.Fatalf("missing owner revision: %d %q %q", code, out, diag)
			}
		})
	}
}

func TestRatifySingleWorkstreamRequestOmitsProjectRevision(t *testing.T) {
	t.Parallel()
	requests := make(chan map[string]int, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/packet/"+stream, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"packet": map[string]any{"revision": map[string]int{"spec": 2, "plan": 3}}})
	})
	mux.HandleFunc("/v1/ratify/"+stream, func(w http.ResponseWriter, r *http.Request) {
		var req map[string]int
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		requests <- req
		json.NewEncoder(w).Encode(wire.RatifyResponse{Workstream: stream, Spec: req["spec"], Plan: req["plan"]})
	})
	root, socket := ratifyServer(t, mux)
	successful(t, root, "ratify", stream, "--socket", socket)
	if req := <-requests; !reflect.DeepEqual(req, map[string]int{"spec": 2, "plan": 3}) {
		t.Fatalf("single-workstream request changed: %+v", req)
	}
}

func TestRatifyRequiresAllExplicitPins(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{"ratify", stream, "--project-spec-revision", "1"},
		{"ratify", stream, "--spec-revision", "1", "--plan-revision", "1"},
		{"ratify", stream, "--project-spec-revision", "0", "--spec-revision", "1", "--plan-revision", "1"},
		{"ratify", stream, "--project-spec-revision", "one", "--spec-revision", "1", "--plan-revision", "1"},
		{"ratify", stream, "--project-spec-revision", "1", "--spec-revision", "1", "--plan-revision", "-1"},
		{"status", stream, "--project-spec-revision", "1", "--spec-revision", "1", "--plan-revision", "1"},
		{"packet", stream, "--project-spec-revision", "1", "--spec-revision", "1", "--plan-revision", "1"},
	} {
		if code, _, _ := invoke(t, t.TempDir(), args...); code != 2 {
			t.Fatalf("%v: exit %d", args, code)
		}
	}
}
