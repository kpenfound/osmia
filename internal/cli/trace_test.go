package cli

import (
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	wire "github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/service"
)

type traceServer struct {
	root, socket string
	mu           sync.Mutex
	paths        []string
}

func (s *traceServer) requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.paths...)
}

func startTraceServer(t *testing.T) *traceServer {
	t.Helper()
	root := t.TempDir()
	ts := &traceServer{root: root, socket: filepath.Join(root, "fixture.sock")}
	listener, err := net.Listen("unix", ts.socket)
	must(t, err)
	t.Cleanup(func() { listener.Close() })
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ts.mu.Lock()
		ts.paths = append(ts.paths, r.URL.Path)
		ts.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case wire.Prefix + "/trace/" + stream:
			json.NewEncoder(w).Encode(wire.TraceSummary{Workstream: stream, Feature: "delivered", Criteria: []wire.TraceCriterion{{Criterion: "spec#1", Text: "Works"}}})
		case wire.Prefix + "/trace/" + stream + "/unit/a":
			json.NewEncoder(w).Encode(wire.UnitTrace{Workstream: stream, Unit: "a", Feature: "abandoned", Gaps: []wire.TraceGap{{Link: "review", State: service.LinkNotCreated, Reason: "not reviewed"}}})
		case wire.Prefix + "/trace/" + stream + "/criterion/spec#1":
			json.NewEncoder(w).Encode(wire.CriterionTrace{Workstream: stream, Criterion: "spec#1", Text: "Works", Feature: "delivered"})
		case wire.Prefix + "/trace/" + stream + "/commit/" + strings.Repeat("a", 40):
			json.NewEncoder(w).Encode(wire.CommitTrace{Workstream: stream, Commit: strings.Repeat("a", 40), Feature: "delivered", Records: []wire.CommitRecord{{Role: "landing", Unit: "a", Ref: wire.TraceRef{ID: "landing", Revision: 2, Path: "units/a/landing.json"}}}})
		default:
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(wire.ErrorResponse{Error: wire.APIError{Code: wire.NotFound, Message: "not in trace"}})
		}
	})}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close() })
	return ts
}

func TestTraceShowsEachSelectorFromOneServiceRequest(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		tail []string
		want string
	}{
		{"workstream", nil, "Criterion spec#1: Works"},
		{"unit", []string{"unit", "a"}, "Gap: review [not-created]"},
		{"criterion", []string{"criterion", "spec#1"}, "Criterion spec#1"},
		{"commit", []string{"commit", strings.Repeat("a", 40)}, "landing revision 2"},
	} {
		t.Run(test.name, func(t *testing.T) {
			ts := startTraceServer(t)
			code, out, diag := invoke(t, ts.root, append([]string{"--socket", ts.socket, "trace", stream}, test.tail...)...)
			if code != 0 || diag != "" || !strings.Contains(out, test.want) {
				t.Fatalf("%v: code=%d out=%s err=%s", test.tail, code, out, diag)
			}
			if paths := ts.requests(); len(paths) != 1 {
				t.Fatalf("API requests %v", paths)
			}
		})
	}
}

func TestTraceReportsAMissingSelector(t *testing.T) {
	t.Parallel()
	ts := startTraceServer(t)
	code, _, diag := invoke(t, ts.root, "--socket", ts.socket, "trace", stream, "unit", "missing")
	if code != 4 || !strings.Contains(diag, "not in trace") {
		t.Fatalf("missing selector: %d %s", code, diag)
	}
}

func TestTraceRefusesAnUnknownSelectorKind(t *testing.T) {
	t.Parallel()
	ts := startTraceServer(t)
	if code, _, _ := invoke(t, ts.root, "--socket", ts.socket, "trace", stream, "other", "a"); code != 2 {
		t.Fatalf("invalid selector: %d", code)
	}
}
