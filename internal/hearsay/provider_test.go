package hearsay

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kpenfound/osmia/internal/bundle"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
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

func publishing(h config.Hearsay) config.Hearsay {
	h.Publish = &config.HearsayPublish{WorkSource: "osmia-work", OwnerSource: "osmia-owner", HooksURL: "https://hooks.example", Agent: "osmia-publisher", TokenEnv: "PUBLISHER_TOKEN"}
	return h
}

// A scripted Hearsay that echoes the requested entity and records the order of
// the entities asked for.
func echoServer(t *testing.T, requested *[]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in map[string]string
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Error(err)
		}
		*requested = append(*requested, in["scope"])
		fmt.Fprintf(w, `{"scope":{"id":%q}}`, in["scope"])
	}))
}

func TestProviderReadsWorkEntitiesByDefault(t *testing.T) {
	base := "work:osmia-work:" + string(project)
	for _, tc := range []struct {
		name     string
		project  config.Project
		publish  bool
		scope    bundle.Scope
		want     []string
		wantMode bundle.Mode
	}{
		{"project-wide turn", config.Project{ID: project}, true, bundle.Scope{Role: "mason"}, []string{base}, Mode},
		{"workstream-tied turn", config.Project{ID: project}, true, bundle.Scope{Role: "mason", Workstream: "w_0123"}, []string{base, base + "#w_0123"}, Mode},
		{"override replaces the project default", config.Project{ID: project, HearsayScope: "code:owner/repo"}, true, bundle.Scope{Role: "mason"}, []string{"code:owner/repo"}, Mode},
		{"mapped footprint keeps the workstream entity", config.Project{ID: project, HearsayEntities: map[string]string{"storage": "code:storage"}}, true, bundle.Scope{Role: "mason", Entities: []string{"storage"}, Workstream: "w_0123"}, []string{"code:storage", base + "#w_0123"}, Mode},
		{"no work source and no override stays in file mode", config.Project{ID: project}, false, bundle.Scope{Role: "mason", Workstream: "w_0123"}, nil, bundle.ModeFile},
	} {
		var requested []string
		server := echoServer(t, &requested)
		hearsayConfig := settings(server.URL)
		if tc.publish {
			hearsayConfig = publishing(hearsayConfig)
		}
		cfg := &config.Config{Hearsay: hearsayConfig, Projects: []config.Project{tc.project}}
		p := Provider{Local: local{}, Config: func() *config.Config { return cfg }, Health: &Health{}, Client: func(h config.Hearsay) Client { return Client{Config: h, LookupEnv: secret} }}
		if tc.wantMode == bundle.ModeFile && p.Mode(project) != bundle.ModeFile {
			t.Errorf("%s: mode %v", tc.name, p.Mode(project))
		}
		got, err := p.Assemble(context.Background(), project, tc.scope)
		server.Close()
		if err != nil || got.Mode != tc.wantMode || !slices.Equal(requested, tc.want) || len(got.Memory) != len(tc.want) {
			t.Errorf("%s: requested %v mode %v memory %d: %v", tc.name, requested, got.Mode, len(got.Memory), err)
		}
	}
}

// A traverse call reaches Hearsay's /v1/traverse as the role's class agent
// acting for the owner, and never carries the publisher's token.
func TestTraverseUsesTheRolesClassCredentialsNotThePublishers(t *testing.T) {
	var seen []http.Header
	var bodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/traverse" {
			t.Errorf("path %s", r.URL.Path)
		}
		seen = append(seen, r.Header.Clone())
		raw, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(raw))
		fmt.Fprint(w, `{"nodes":[],"edges":[],"truncated":false}`)
	}))
	defer server.Close()
	c := Client{Config: publishing(settings(server.URL)), LookupEnv: secret}
	input := json.RawMessage(`{"start":[{"type":"entity","id":"work:osmia-work:p"}],"steps":[{"edge":"documents"}]}`)
	for role, agent := range map[string]string{"mason": "worker", "chief_of_staff": "orchestrator", "librarian": "observer", "session": "observer"} {
		var tool *coreadapter.Tool
		for _, candidate := range c.Tools(role, "work:osmia-work:p", func() error { return nil }) {
			if candidate.Name == "traverse" {
				tool = &candidate
			}
		}
		if tool == nil {
			t.Fatalf("%s has no traverse tool", role)
		}
		if _, err := tool.Handle(context.Background(), input); err != nil {
			t.Fatalf("%s: %v", role, err)
		}
		h := seen[len(seen)-1]
		if h.Get("Hearsay-Agent") != agent || h.Get("Hearsay-Agent-Token") != secret(map[string]string{"worker": "WORKER_TOKEN", "orchestrator": "CHIEF_TOKEN", "observer": "OBSERVER_TOKEN"}[agent]) || h.Get("Authorization") != "Bearer private-OWNER_TOKEN" || h.Get("Hearsay-Principal") != "human" {
			t.Errorf("%s: credentials %v", role, h)
		}
		for name, values := range h {
			for _, value := range values {
				if strings.Contains(value, "PUBLISHER_TOKEN") || value == "osmia-publisher" {
					t.Errorf("%s: header %s carries the publisher", role, name)
				}
			}
		}
		var sent, want any
		if json.Unmarshal([]byte(bodies[len(bodies)-1]), &sent) != nil || json.Unmarshal(input, &want) != nil || !reflect.DeepEqual(sent, want) {
			t.Errorf("%s: body %s", role, bodies[len(bodies)-1])
		}
	}
}

func TestEveryRoleWithReadToolsGetsTraverseAndOnlyWorkersAndTheChiefAssert(t *testing.T) {
	c := Client{Config: settings("https://memory.example")}
	for _, role := range []string{"chief_of_staff", "architect", "committee", "mason", "reviewer", "librarian", "session"} {
		names := map[string]bool{}
		for _, tool := range c.Tools(role, "work:s:p", nil) {
			names[tool.Name] = true
		}
		for _, read := range []string{"get_bundle", "search", "traverse", "resolve", "stance_history", "get_l1", "get_l0"} {
			if !names[read] {
				t.Errorf("%s lacks %s", role, read)
			}
		}
		if names["assert"] != (Class(role) != "observer") || names["watch"] {
			t.Errorf("%s: write tools %v", role, names)
		}
	}
}

// Hearsay's traverse takes its start nodes as a list of {type, id} references
// and refuses a single object, so the tool schema must offer the list.
func TestTraverseSchemaOffersStartAsAList(t *testing.T) {
	c := Client{Config: settings("https://memory.example")}
	for _, tool := range c.Tools("mason", "work:s:p", nil) {
		if tool.Name != "traverse" {
			continue
		}
		var schema struct {
			Properties map[string]struct {
				Type string `json:"type"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
			t.Fatal(err)
		}
		if schema.Properties["start"].Type != "array" || schema.Properties["steps"].Type != "array" {
			t.Fatalf("traverse schema: %s", tool.InputSchema)
		}
		return
	}
	t.Fatal("no traverse tool")
}
