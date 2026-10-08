package hearsay

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kpenfound/osmia/internal/bundle"
	"github.com/kpenfound/osmia/internal/config"
)

const project config.ProjectID = "p_0123456789abcdef0123456789abcdef"

// local is a fake bundle.Provider standing in for the real file-based bundle
// assembler that backs Osmia when Hearsay is absent (charter#2). It leaves
// unverified whether the real assembler reads project state correctly; this
// package only has to show that Provider layers Hearsay's memory on top of
// whatever the authoritative local bundle already contains.
type local struct{}

func (local) Mode(config.ProjectID) bundle.Mode { return bundle.ModeFile }
func (local) Assemble(_ context.Context, id config.ProjectID, scope bundle.Scope) (bundle.Bundle, error) {
	return bundle.Bundle{Project: id, Mode: bundle.ModeFile, Scope: scope, Charter: bundle.Charter{Revision: 7}}, nil
}
func settings(endpoint string) config.Hearsay {
	return config.Hearsay{URL: endpoint, Principal: "human", TokenEnv: "OWNER_TOKEN", Agents: map[string]config.HearsayAgent{"worker": {ID: "worker", TokenEnv: "WORKER_TOKEN"}, "observer": {ID: "observer", TokenEnv: "OBSERVER_TOKEN"}, "orchestrator": {ID: "orchestrator", TokenEnv: "CHIEF_TOKEN"}}}
}
func secret(name string) string { return "private-" + name }

// server stands in for a live Hearsay instance (charter#4 forbids one in
// tests): it checks the shape of the request our Client sends and scripts a
// response, but it never exercises Hearsay's own authentication, storage or
// stance-merging logic, so this test cannot catch a real Hearsay API change.
func TestProviderAuthenticatesRoleScopesAndFallsBackLocally(t *testing.T) {
	var failure atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/get_bundle" || r.Header.Get("Authorization") != "Bearer private-OWNER_TOKEN" || r.Header.Get("Hearsay-Principal") != "human" || r.Header.Get("Hearsay-Agent") != "worker" || r.Header.Get("Hearsay-Agent-Token") != "private-WORKER_TOKEN" {
			t.Error("wrong call or authentication")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if failure.Load() {
			http.Error(w, "private-OWNER_TOKEN upstream details", http.StatusServiceUnavailable)
			return
		}
		var in map[string]string
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Error(err)
		}
		if in["scope"] != "code:storage" {
			t.Errorf("scope %v", in)
		}
		fmt.Fprint(w, `{"scope":{"id":"code:storage"},"stances":[{"current":"Use a durable outbox","topic_id":"outbox"}]}`)
	}))
	defer server.Close()
	cfg := &config.Config{Hearsay: settings(server.URL), Projects: []config.Project{{ID: project, HearsayScope: "root", HearsayEntities: map[string]string{"storage": "code:storage"}}}}
	p := Provider{Local: local{}, Config: func() *config.Config { return cfg }, Health: &Health{}, Client: func(h config.Hearsay) Client { return Client{Config: h, LookupEnv: secret} }}
	if p.Mode(project) != Degraded {
		t.Fatal("unobserved service reported healthy")
	}
	got, err := p.Assemble(context.Background(), project, bundle.Scope{Role: "mason", Entities: []string{"storage"}})
	if err != nil || got.Mode != Mode || got.Charter.Revision != 7 || len(got.Memory) != 1 || p.Mode(project) != Mode {
		t.Fatalf("bundle %+v %v", got, err)
	}
	failure.Store(true)
	got, err = p.Assemble(context.Background(), project, bundle.Scope{Role: "mason", Entities: []string{"storage"}})
	if err != nil || got.Mode != Degraded || got.Charter.Revision != 7 || len(got.Memory) != 0 || strings.Contains(got.MemoryProblem, "private") {
		t.Fatalf("fallback %+v %v", got, err)
	}
	cfg.Hearsay = config.Hearsay{}
	got, err = p.Assemble(context.Background(), project, bundle.Scope{})
	if err != nil || got.Mode != bundle.ModeFile || p.Mode(project) != bundle.ModeFile {
		t.Fatalf("disabled %+v %v", got, err)
	}
}

// source and target stand in for a live Hearsay instance and an attacker's
// endpoint it might redirect to; they verify our Client withholds credentials
// on a redirect, not any behavior of a real Hearsay deployment.
func TestClientRejectsRedirectsMissingCredentialsAndUngrantableCalls(t *testing.T) {
	var leaked atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Store(true) }))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	c := Client{Config: settings(source.URL), LookupEnv: secret}
	for _, test := range []struct{ role, name string }{{"mason", "get_bundle"}, {"librarian", "assert"}, {"mason", "watch"}, {"chief_of_staff", "delete"}} {
		if _, err := c.Call(context.Background(), test.role, test.name, map[string]string{}); err == nil || strings.Contains(err.Error(), "private") {
			t.Fatalf("%+v: %v", test, err)
		}
	}
	if leaked.Load() {
		t.Fatal("credentials forwarded through redirect")
	}
	c.LookupEnv = func(string) string { return "" }
	if _, err := c.Call(context.Background(), "mason", "get_bundle", map[string]string{}); err == nil {
		t.Fatal("missing credentials accepted")
	}
}

func TestProviderRejectsWrongScopeAndOversizedBundles(t *testing.T) {
	for _, response := range []string{`{"scope":{"id":"other"}}`, `{"scope":{"id":"root"},"text":"` + strings.Repeat("x", 8000) + `"}`} {
		// server scripts a malformed reply in place of a live Hearsay, so it
		// leaves unverified whether real Hearsay would ever actually return
		// a mismatched scope or an oversized bundle.
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, response) }))
		cfg := &config.Config{Hearsay: settings(server.URL), Projects: []config.Project{{ID: project, HearsayScope: "root"}}}
		p := Provider{Local: local{}, Config: func() *config.Config { return cfg }, Client: func(h config.Hearsay) Client { return Client{Config: h, LookupEnv: secret} }}
		got, err := p.Assemble(context.Background(), project, bundle.Scope{Role: "chief_of_staff"})
		server.Close()
		if err != nil || got.Mode != Degraded || len(got.Memory) != 0 || got.Charter.Revision != 7 {
			t.Fatalf("invalid bundle: %+v %v", got, err)
		}
	}
}

// server stands in for a live Hearsay instance, scripted to echo the scope it
// was asked for; it leaves unverified how a real Hearsay enforces agent
// grants, and tests only that our Tools wiring withholds the call until the
// turn-scope closure allows it.
func TestToolsBindRoleAndValidateActiveTurnBeforeCalling(t *testing.T) {
	var called atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called.Add(1)
		if r.Header.Get("Hearsay-Agent") != "orchestrator" {
			t.Error("wrong agent grant")
		}
		var input map[string]string
		json.NewDecoder(r.Body).Decode(&input)
		if input["scope"] != "project-scope" {
			t.Errorf("scope: %v", input)
		}
		fmt.Fprint(w, `{"scope":{"id":"project-scope"}}`)
	}))
	defer server.Close()
	c := Client{Config: settings(server.URL), LookupEnv: secret}
	active := false
	tools := c.Tools("chief_of_staff", "project-scope", func() error {
		if !active {
			return fmt.Errorf("turn scope denied")
		}
		return nil
	})
	if _, err := tools[0].Handle(context.Background(), json.RawMessage(`{}`)); err == nil || called.Load() != 0 {
		t.Fatal("unclaimed tool reached memory")
	}
	active = true
	if _, err := tools[0].Handle(context.Background(), json.RawMessage(`{}`)); err != nil || called.Load() != 1 {
		t.Fatalf("active call: %v", err)
	}
	for _, tool := range c.Tools("librarian", "project-scope", func() error { return nil }) {
		if tool.Name == "assert" || tool.Name == "watch" {
			t.Fatal("observer received mutating or watch tool")
		}
	}
}
