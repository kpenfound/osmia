package cli

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	wire "github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
)

func TestDesignReadAndStartPinBriefRevision(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		t.Run(map[bool]string{false: "start", true: "conflict"}[conflict], func(t *testing.T) {
			home := config.ProjectID("p_11111111111111111111111111111111")
			other := config.ProjectID("p_22222222222222222222222222222222")
			design := wire.DesignView{Workstream: config.WorkstreamID(stream), Project: home, BriefRevision: 7, Brief: "Decisions and intent", Projects: []config.ProjectID{other, home}}
			requests := make(chan wire.DesignStartRequest, 2)
			root, socket := ratifyServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.Method + " " + r.URL.Path {
				case "GET /v1/design/" + stream:
					json.NewEncoder(w).Encode(wire.DesignResponse{Design: design})
				case "GET /v1/design/" + stream + "/conversation":
					json.NewEncoder(w).Encode(wire.ConversationResponse{Workstream: config.WorkstreamID(stream)})
				case "POST /v1/design/" + stream + "/start":
					var req wire.DesignStartRequest
					if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
						t.Error(err)
					}
					requests <- req
					if conflict {
						w.WriteHeader(http.StatusConflict)
						json.NewEncoder(w).Encode(map[string]any{"error": wire.APIError{Code: wire.Conflict, Message: "the brief has changed; read the latest brief before starting"}})
					} else {
						json.NewEncoder(w).Encode(wire.DesignStartResponse{Workstream: config.WorkstreamID(stream), State: "handed"})
					}
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			read := successful(t, root, "design", "read", stream, "--socket", socket)
			for _, want := range []string{"Brief revision: 7", string(other), string(home) + " (home)", design.Brief} {
				if !strings.Contains(read, want) {
					t.Fatalf("read missing %q: %s", want, read)
				}
			}
			var readJSON struct {
				Design wire.DesignView `json:"design"`
			}
			must(t, json.Unmarshal([]byte(successful(t, root, "design", "read", stream, "--socket", socket, "--json")), &readJSON))
			if readJSON.Design.BriefRevision != 7 || len(readJSON.Design.Projects) != 2 {
				t.Fatalf("read JSON: %+v", readJSON)
			}
			for _, jsonOutput := range []bool{false, true} {
				args := []string{"design", "start", stream, "--socket", socket}
				if jsonOutput {
					args = append(args, "--json")
				}
				code, out, diag := invoke(t, root, args...)
				printed := out
				if jsonOutput {
					printed = diag
				}
				if !strings.Contains(printed, "Starting with:\nBrief revision: 7\nProjects:\n  "+string(other)+"\n  "+string(home)+" (home)") {
					t.Fatalf("start list: %q %q", out, diag)
				}
				req := <-requests
				if req.BriefRevision == nil || *req.BriefRevision != 7 {
					t.Fatalf("start pin: %+v", req)
				}
				if conflict {
					if code != 5 || !strings.Contains(diag, "conflict") || !strings.Contains(diag, "brief has changed") || strings.Contains(out, "Started a workgroup") {
						t.Fatalf("conflict: %d %q %q", code, out, diag)
					}
				} else if code != 0 {
					t.Fatalf("start: %d %q %q", code, out, diag)
				} else if jsonOutput {
					var result wire.DesignStartResponse
					must(t, json.Unmarshal([]byte(out), &result))
				}
			}
		})
	}
}
