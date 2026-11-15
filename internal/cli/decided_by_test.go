package cli

import (
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	wire "github.com/kpenfound/osmia/internal/api"
)

// The trace names a decision Jev made, and leaves the owner's and the chief
// of staff's decisions as they were.
func TestTraceLabelsDecisionsJevMade(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	socket := filepath.Join(root, "fixture.sock")
	listener, err := net.Listen("unix", socket)
	must(t, err)
	t.Cleanup(func() { listener.Close() })
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != wire.Prefix+"/trace/"+stream+"/unit/a" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		json.NewEncoder(w).Encode(wire.UnitTrace{Workstream: stream, Unit: "a", Feature: "building", Rulings: []wire.TraceRuling{
			{Kind: "contested", Decision: "review", DecidedBy: "jev", Judgment: "judgment_ab12"},
			{Kind: "contested", Decision: "revise", DecidedBy: "owner"},
			{Kind: "question", Decision: "answer", DecidedBy: "chief_of_staff"},
		}})
	})}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close() })
	code, out, diag := invoke(t, root, "--socket", socket, "trace", stream, "unit", "a")
	if code != 0 || diag != "" {
		t.Fatalf("code=%d out=%s err=%s", code, out, diag)
	}
	for _, want := range []string{"contested: review (decided by Jev)\n", "contested: revise\n", "question: answer\n"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Count(out, "decided by Jev") != 1 {
		t.Fatalf("output labels more than the Jev decision:\n%s", out)
	}
}
