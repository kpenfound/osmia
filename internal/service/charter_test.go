package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/bundle"
	"github.com/kpenfound/osmia/internal/charter"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

func handInError(t *testing.T, c *Client, req HandInRequest) *APIError {
	t.Helper()
	err := c.HandIn(context.Background(), req)
	var api *APIError
	if !errors.As(err, &api) {
		t.Fatalf("hand-in: %v", err)
	}
	return api
}

func handInStatus(t *testing.T, s *Service, body string) int {
	t.Helper()
	w := httptest.NewRecorder()
	s.handle(w, httptest.NewRequest(http.MethodPost, Prefix+"/handin", strings.NewReader(body)))
	return w.Code
}

// documentRevisions returns the active project's revisions of one document.
func documentRevisions(t *testing.T, s *Service, id string) []trace.Document {
	t.Helper()
	docs, err := trace.Read[trace.Document](s.active.repository, "")
	must(t, err)
	var out []trace.Document
	for _, d := range docs {
		if d.ID == id {
			out = append(out, d)
		}
	}
	return out
}

func charterRevisionCount(t *testing.T, s *Service) int {
	t.Helper()
	return len(documentRevisions(t, s, "charter"))
}

func TestHandInCharterGate(t *testing.T) {
	opts, clone := projectFixture(t)
	s, c := start(t, opts)
	ctx := context.Background()
	unknown := config.ProjectID("p_0123456789abcdef0123456789abcdef")

	if api := handInError(t, c, HandInRequest{Project: unknown}); api.Code != NotFound || !strings.Contains(api.Message, string(unknown)) {
		t.Fatalf("no active project: %+v", api)
	}
	added, err := c.AddProject(ctx, request(clone))
	must(t, err)
	id, path := added.Project.ID, added.Project.Charter

	cfg, err := c.Configuration(ctx)
	must(t, err)
	if want := (&CharterState{Ready: false, Rules: 0, Revision: 1, Diagnostics: []charter.Diagnostic{}}); cfg.Project == nil || !reflect.DeepEqual(cfg.Project.CharterState, want) || len(cfg.Diagnostics) != 0 {
		t.Fatalf("template status: %+v %+v", cfg.Project, cfg.Diagnostics)
	}
	if added.Project.CharterState != nil {
		t.Fatal("project add reports charter state")
	}

	api := handInError(t, c, HandInRequest{Project: id, Paths: []string{"/tmp/design.md"}})
	if api.Code != CharterEmpty || !strings.Contains(api.Message, string(id)) || !strings.Contains(api.Message, "charter has no rules") || !strings.Contains(api.Message, path) {
		t.Fatalf("empty charter: %+v", api)
	}
	if code := handInStatus(t, s, `{"project":"`+string(id)+`"}`); code != http.StatusConflict {
		t.Fatalf("empty charter status %d", code)
	}
	if api := handInError(t, c, HandInRequest{Project: unknown}); api.Code != NotFound || !strings.Contains(api.Message, string(unknown)) {
		t.Fatalf("unknown project: %+v", api)
	}
	if code := handInStatus(t, s, `{"project":"`+string(unknown)+`"}`); code != http.StatusNotFound {
		t.Fatalf("unknown project status %d", code)
	}
	if api := handInError(t, c, HandInRequest{Project: "dagger"}); api.Code != Validation {
		t.Fatalf("malformed project: %+v", api)
	}
	if n := charterRevisionCount(t, s); n != 1 {
		t.Fatalf("reads without an edit recorded %d revisions", n)
	}

	must(t, os.WriteFile(path, []byte(trace.CharterTemplate+"\n## Rules\n\n1. Keep changes small.\n3. Test every fix.\n3. Cite the charter.\n"), 0600))
	cfg, err = c.Configuration(ctx)
	must(t, err)
	state := cfg.Project.CharterState
	if state == nil || !state.Ready || state.Rules != 3 || state.Revision != 2 || len(state.Diagnostics) != 2 || !strings.Contains(state.Diagnostics[0].Message, "gap") || !strings.Contains(state.Diagnostics[1].Message, "cannot be cited") {
		t.Fatalf("edited status: %+v", state)
	}
	api = handInError(t, c, HandInRequest{Project: id})
	if api.Code != Unsupported || !strings.Contains(api.Message, string(id)) {
		t.Fatalf("ready charter: %+v", api)
	}
	if code := handInStatus(t, s, `{"project":"`+string(id)+`","paths":[]}`); code != http.StatusNotImplemented {
		t.Fatalf("ready charter status %d", code)
	}
	if n := charterRevisionCount(t, s); n != 2 {
		t.Fatalf("one edit recorded %d revisions in total", n)
	}
	docs := documentRevisions(t, s, "charter")
	if docs[1].Actor != (trace.Actor{Kind: "owner", ID: "local"}) || docs[1].Cause != "owner-edit" {
		t.Fatalf("edit provenance: %+v", docs[1].Header)
	}

	// Emptying the charter closes the gate again.
	must(t, os.WriteFile(path, []byte("# Charter\n"), 0600))
	if api := handInError(t, c, HandInRequest{Project: id}); api.Code != CharterEmpty {
		t.Fatalf("emptied charter: %+v", api)
	}
	if n := charterRevisionCount(t, s); n != 3 {
		t.Fatalf("hand-in did not record the edit: %d", n)
	}

	// A trace failure is a diagnostic, not a status failure.
	must(t, os.Remove(path))
	cfg, err = c.Configuration(ctx)
	must(t, err)
	if cfg.Project == nil || cfg.Project.CharterState != nil || !hasDiagnostic(cfg.Diagnostics, Internal) {
		t.Fatalf("unreadable charter: %+v %+v", cfg.Project, cfg.Diagnostics)
	}
	if api := handInError(t, c, HandInRequest{Project: id}); api.Code != Internal || !strings.Contains(api.Message, path) {
		t.Fatalf("unreadable charter hand-in: %+v", api)
	}

	_, err = c.RemoveProject(ctx, id)
	must(t, err)
	if api := handInError(t, c, HandInRequest{Project: id}); api.Code != NotFound {
		t.Fatalf("inactive project: %+v", api)
	}
}

func TestContextProviderReadsActiveProject(t *testing.T) {
	opts, clone := projectFixture(t)
	s, c := start(t, opts)
	ctx := context.Background()
	provider := s.Context()
	if provider.Mode("") != bundle.ModeFile {
		t.Fatal(provider.Mode(""))
	}
	unknown := config.ProjectID("p_0123456789abcdef0123456789abcdef")
	if _, err := provider.Assemble(ctx, unknown, bundle.Scope{}); !errors.Is(err, errNoActiveProject) {
		t.Fatalf("idle service: %v", err)
	}
	added, err := c.AddProject(ctx, request(clone))
	must(t, err)
	id, path := added.Project.ID, added.Project.Charter
	must(t, os.WriteFile(path, []byte("1. Keep changes small.\n"), 0600))
	must(t, os.WriteFile(filepath.Join(added.Project.Trace, "kb", "service.md"), []byte("Service notes\n"), 0600))
	b, err := provider.Assemble(ctx, id, bundle.Scope{})
	must(t, err)
	if b.Charter.Revision != 2 || len(b.Charter.Rules) != 1 || len(b.Knowledge) != 1 || b.Knowledge[0].Content != "Service notes\n" || b.Entities.Revision != 1 {
		t.Fatalf("bundle: %+v", b)
	}
	if n := charterRevisionCount(t, s); n != 2 {
		t.Fatalf("assembly recorded %d charter revisions", n)
	}
	if _, err := provider.Assemble(ctx, unknown, bundle.Scope{}); !errors.Is(err, errNoActiveProject) {
		t.Fatalf("other project: %v", err)
	}
	_, err = c.RemoveProject(ctx, id)
	must(t, err)
	if _, err := provider.Assemble(ctx, id, bundle.Scope{}); !errors.Is(err, errNoActiveProject) {
		t.Fatalf("removed project: %v", err)
	}
}
