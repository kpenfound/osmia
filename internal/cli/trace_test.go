package cli

import (
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/service"
)

func TestTraceUsesServiceAPI(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	socket := filepath.Join(root, "fixture.sock")
	listener, err := net.Listen("unix", socket)
	must(t, err)
	t.Cleanup(func() { listener.Close() })
	paths := []string{}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case service.Prefix + "/trace/" + stream:
			json.NewEncoder(w).Encode(service.TraceSummary{Workstream: stream, Feature: "delivered", Criteria: []service.TraceCriterion{{Criterion: "spec#1", Text: "Works"}}})
		case service.Prefix + "/trace/" + stream + "/unit/a":
			json.NewEncoder(w).Encode(service.UnitTrace{Workstream: stream, Unit: "a", Feature: "abandoned", Gaps: []service.TraceGap{{Link: "review", State: service.LinkNotCreated, Reason: "not reviewed"}}})
		case service.Prefix + "/trace/" + stream + "/criterion/spec#1":
			json.NewEncoder(w).Encode(service.CriterionTrace{Workstream: stream, Criterion: "spec#1", Text: "Works", Feature: "delivered"})
		case service.Prefix + "/trace/" + stream + "/commit/" + strings.Repeat("a", 40):
			json.NewEncoder(w).Encode(service.CommitTrace{Workstream: stream, Commit: strings.Repeat("a", 40), Feature: "delivered", Records: []service.CommitRecord{{Role: "landing", Unit: "a", Ref: service.TraceRef{ID: "landing", Revision: 2, Path: "units/a/landing.json"}}}})
		default:
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(service.ErrorResponse{Error: service.APIError{Code: service.NotFound, Message: "not in trace"}})
		}
	})}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close() })
	base := []string{"--socket", socket, "trace", stream}
	for _, test := range []struct {
		tail []string
		want string
	}{{nil, "Criterion spec#1: Works"}, {[]string{"unit", "a"}, "Gap: review [not-created]"}, {[]string{"criterion", "spec#1"}, "Criterion spec#1"}, {[]string{"commit", strings.Repeat("a", 40)}, "landing revision 2"}} {
		code, out, diag := invoke(t, root, append(base, test.tail...)...)
		if code != 0 || diag != "" || !strings.Contains(out, test.want) {
			t.Fatalf("%v: code=%d out=%s err=%s", test.tail, code, out, diag)
		}
	}
	if len(paths) != 4 {
		t.Fatalf("API requests %v", paths)
	}
	code, _, diag := invoke(t, root, append(base, "unit", "missing")...)
	if code != 4 || !strings.Contains(diag, "not in trace") {
		t.Fatalf("missing selector: %d %s", code, diag)
	}
	code, _, _ = invoke(t, root, append(base, "other", "a")...)
	if code != 2 {
		t.Fatalf("invalid selector: %d", code)
	}
}
