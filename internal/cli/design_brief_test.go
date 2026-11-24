package cli

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	wire "github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
)

type briefServer struct {
	root, socket string
	design       wire.DesignView
	requests     chan wire.DesignStartRequest
}

func startBriefServer(t *testing.T, conflict bool) briefServer {
	t.Helper()
	home := config.ProjectID("p_11111111111111111111111111111111")
	other := config.ProjectID("p_22222222222222222222222222222222")
	b := briefServer{
		design:   wire.DesignView{Workstream: config.WorkstreamID(stream), Project: home, BriefRevision: 7, Brief: "Decisions and intent", Projects: []config.ProjectID{other, home}},
		requests: make(chan wire.DesignStartRequest, 2),
	}
	b.root, b.socket = ratifyServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/design/" + stream:
			json.NewEncoder(w).Encode(wire.DesignResponse{Design: b.design})
		case "GET /v1/design/" + stream + "/conversation":
			json.NewEncoder(w).Encode(wire.ConversationResponse{Workstream: config.WorkstreamID(stream)})
		case "POST /v1/design/" + stream + "/start":
			var req wire.DesignStartRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Error(err)
			}
			b.requests <- req
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
	return b
}

const startedWith = "Starting with:\nBrief revision: 7\nProjects:\n  p_22222222222222222222222222222222\n  p_11111111111111111111111111111111 (home)"

func TestDesignReadShowsTheBriefRevisionAndProjects(t *testing.T) {
	b := startBriefServer(t, false)
	read := successful(t, b.root, "design", "read", stream, "--socket", b.socket)
	for _, want := range []string{"Brief revision: 7", "p_22222222222222222222222222222222", "p_11111111111111111111111111111111 (home)", b.design.Brief} {
		if !strings.Contains(read, want) {
			t.Fatalf("read missing %q: %s", want, read)
		}
	}
}

func TestDesignReadJSONHoldsTheBriefRevisionAndProjects(t *testing.T) {
	b := startBriefServer(t, false)
	var read struct {
		Design wire.DesignView `json:"design"`
	}
	must(t, json.Unmarshal([]byte(successful(t, b.root, "design", "read", stream, "--socket", b.socket, "--json")), &read))
	if read.Design.BriefRevision != 7 || len(read.Design.Projects) != 2 {
		t.Fatalf("read JSON: %+v", read)
	}
}

func TestDesignStartPinsTheBriefRevisionItListed(t *testing.T) {
	b := startBriefServer(t, false)
	code, out, diag := invoke(t, b.root, "design", "start", stream, "--socket", b.socket)
	if code != 0 || !strings.Contains(out, startedWith) {
		t.Fatalf("start: %d %q %q", code, out, diag)
	}
	if req := <-b.requests; req.BriefRevision == nil || *req.BriefRevision != 7 {
		t.Fatalf("start pin: %+v", req)
	}
}

func TestDesignStartJSONListsOnStandardErrorAndPrintsTheResult(t *testing.T) {
	b := startBriefServer(t, false)
	code, out, diag := invoke(t, b.root, "design", "start", stream, "--socket", b.socket, "--json")
	if code != 0 || !strings.Contains(diag, startedWith) {
		t.Fatalf("start: %d %q %q", code, out, diag)
	}
	var result wire.DesignStartResponse
	must(t, json.Unmarshal([]byte(out), &result))
	if req := <-b.requests; req.BriefRevision == nil || *req.BriefRevision != 7 {
		t.Fatalf("start pin: %+v", req)
	}
}

func TestDesignStartRefusesWhenTheBriefHasChanged(t *testing.T) {
	b := startBriefServer(t, true)
	code, out, diag := invoke(t, b.root, "design", "start", stream, "--socket", b.socket)
	if code != 5 || !strings.Contains(diag, "conflict") || !strings.Contains(diag, "brief has changed") || strings.Contains(out, "Started a workgroup") {
		t.Fatalf("conflict: %d %q %q", code, out, diag)
	}
	if req := <-b.requests; req.BriefRevision == nil || *req.BriefRevision != 7 {
		t.Fatalf("start pin: %+v", req)
	}
}

func TestDesignReadPrintsTheTitleOnlyWhenSet(t *testing.T) {
	for _, title := range []string{"", "Plan the billing export"} {
		design := wire.DesignView{Workstream: config.WorkstreamID(stream), Project: "p_11111111111111111111111111111111", BriefRevision: 1, Brief: "Decisions and intent", Title: title}
		root, socket := ratifyServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.Method + " " + r.URL.Path {
			case "GET /v1/design/" + stream:
				json.NewEncoder(w).Encode(wire.DesignResponse{Design: design})
			case "GET /v1/design/" + stream + "/conversation":
				json.NewEncoder(w).Encode(wire.ConversationResponse{Workstream: config.WorkstreamID(stream)})
			default:
				t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				http.NotFound(w, r)
			}
		}))
		read := successful(t, root, "design", "read", stream, "--socket", socket)
		if got := strings.Contains(read, "Title: "); got != (title != "") || (title != "" && !strings.Contains(read, "Title: "+title+"\n")) {
			t.Fatalf("title %q printed as: %s", title, read)
		}
	}
}
