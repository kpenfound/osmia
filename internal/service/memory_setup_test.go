package service

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

type setupSource struct {
	ID         string   `json:"id"`
	Type       string   `json:"type"`
	Containers []string `json:"containers"`
	Settings   struct {
		OwnerOf     string   `json:"owner_of"`
		Posters     []string `json:"posters"`
		Attributors []string `json:"attributors"`
	} `json:"settings"`
}

type setupPrincipal struct {
	ID       string   `json:"id"`
	Kind     string   `json:"kind"`
	Class    string   `json:"class"`
	TokenEnv string   `json:"token_env"`
	Scopes   []string `json:"scopes"`
}

type setupPolicy struct {
	Scope    string `json:"scope"`
	Ratified struct {
		Principals []string `json:"principals"`
		Sources    []string `json:"sources"`
		Artifacts  []string `json:"artifacts"`
	} `json:"ratified_by"`
}

func decodeSetup[T any](t *testing.T, out MemorySetup, name string) []T {
	t.Helper()
	raw, ok := out.Files[name]
	if !ok {
		t.Fatalf("missing fragment %s in %v", name, out.Files)
	}
	var v []T
	must(t, json.Unmarshal(raw, &v))
	return v
}

func TestMemorySetupIsLocalAndRatifiesOnlyTheOwnerSource(t *testing.T) {
	t.Parallel()
	opts, clone := projectFixture(t)
	_, c := start(t, opts)
	added, err := c.AddProject(context.Background(), request(clone))
	must(t, err)
	var setup MemorySetup
	must(t, c.Do(context.Background(), http.MethodGet, Prefix+"/projects/memory/"+string(added.Project.ID), nil, &setup))
	if len(setup.Files) != 4 || setup.Scope != "work:"+setup.WorkSource+":"+string(added.Project.ID) {
		t.Fatalf("setup %+v", setup)
	}
	for name, raw := range setup.Files {
		if !json.Valid(raw) {
			t.Fatalf("invalid configuration %s", name)
		}
		if strings.Contains(string(raw), `"osmia"`) && strings.HasPrefix(name, "sources/") || strings.Contains(string(raw), "/mnt/osmia") {
			t.Fatalf("osmia connector configuration in %s: %s", name, raw)
		}
	}
	policies := decodeSetup[setupPolicy](t, setup, "authority/osmia.yaml")
	if len(policies) != 1 {
		t.Fatalf("policies %+v", policies)
	}
	rule := policies[0].Ratified
	if strings.Join(rule.Sources, ",") != setup.OwnerSource || strings.Join(rule.Artifacts, ",") != "spec,decision" || policies[0].Scope != setup.WorkSource {
		t.Fatalf("authority is not the owner source's alone: %+v", policies)
	}
	var again MemorySetup
	must(t, c.Do(context.Background(), http.MethodGet, Prefix+"/projects/memory/"+string(added.Project.ID), nil, &again))
	first, _ := json.Marshal(setup)
	second, _ := json.Marshal(again)
	if string(first) != string(second) {
		t.Fatal("setup identity changed on retry")
	}
}

func TestMemorySetupFollowsPublishSettings(t *testing.T) {
	t.Parallel()
	_, cfg := conversationFixture(t, "setup-publish-")
	repo, err := trace.Open(cfg.Root, cfg.Project)
	must(t, err)
	defer repo.Close()
	cfg.Hearsay.Principal = "kyle"
	cfg.Hearsay.TokenEnv = "KYLE_TOKEN"
	cfg.Hearsay.Agents = map[string]config.HearsayAgent{
		"worker":       {ID: "w", TokenEnv: "W_TOKEN"},
		"orchestrator": {ID: "o", TokenEnv: "O_TOKEN"},
		"observer":     {ID: "ob", TokenEnv: "OB_TOKEN"},
	}
	cfg.Hearsay.Publish = &config.HearsayPublish{WorkSource: "projects", OwnerSource: "projects-owner", HooksURL: "https://hooks.example", Agent: "publisher", TokenEnv: "PUB_TOKEN"}
	out, err := buildMemorySetup(cfg, repo)
	must(t, err)
	project := string(repo.Project())
	if out.WorkSource != "projects" || out.OwnerSource != "projects-owner" || out.Publisher != "publisher" || out.Principal != "kyle" || out.Scope != "work:projects:"+project {
		t.Fatalf("setup %+v", out)
	}
	sources := decodeSetup[setupSource](t, out, "sources/osmia.yaml")
	if len(sources) != 2 {
		t.Fatalf("sources %+v", sources)
	}
	for _, s := range sources {
		if s.Type != "work" || strings.Join(s.Containers, ",") != "*" ||
			strings.Join(s.Settings.Posters, ",") != "publisher" || strings.Join(s.Settings.Attributors, ",") != "publisher" {
			t.Fatalf("source %+v: only the publisher may post", s)
		}
	}
	if sources[0].ID != "projects" || sources[0].Settings.OwnerOf != "" || sources[1].ID != "projects-owner" || sources[1].Settings.OwnerOf != "projects" {
		t.Fatalf("pairing %+v", sources)
	}
	principals := decodeSetup[setupPrincipal](t, out, "principals/osmia.yaml")
	byID := map[string]setupPrincipal{}
	for _, p := range principals {
		byID[p.ID] = p
	}
	for id, want := range map[string]struct{ kind, class, env string }{
		"kyle": {"human", "", "KYLE_TOKEN"}, "publisher": {"agent", "observer", "PUB_TOKEN"},
		"w": {"agent", "worker", "W_TOKEN"}, "o": {"agent", "orchestrator", "O_TOKEN"}, "ob": {"agent", "observer", "OB_TOKEN"},
	} {
		p, ok := byID[id]
		if !ok || p.Kind != want.kind || p.Class != want.class || p.TokenEnv != want.env || !reflect.DeepEqual(p.Scopes, []string{"work:projects:" + project}) {
			t.Fatalf("principal %s: %+v", id, p)
		}
	}
	if len(principals) != 5 {
		t.Fatalf("principals %+v", principals)
	}
	policy := decodeSetup[setupPolicy](t, out, "authority/osmia.yaml")[0]
	if policy.Scope != "projects" || !reflect.DeepEqual(policy.Ratified.Sources, []string{"projects-owner"}) {
		t.Fatalf("authority %+v", policy)
	}
	scopes := decodeSetup[struct {
		ID      string   `json:"id"`
		Sources []string `json:"sources"`
	}](t, out, "scopes/osmia.yaml")
	if len(scopes) != 1 || scopes[0].ID != "projects" || !reflect.DeepEqual(scopes[0].Sources, []string{"projects", "projects-owner"}) {
		t.Fatalf("scopes %+v", scopes)
	}
}

func TestMemorySetupScopeOverrideIsGranted(t *testing.T) {
	t.Parallel()
	_, cfg := conversationFixture(t, "setup-override-")
	repo, err := trace.Open(cfg.Root, cfg.Project)
	must(t, err)
	defer repo.Close()
	cfg.Project.HearsayScope = "work:other:shared"
	out, err := buildMemorySetup(cfg, repo)
	must(t, err)
	if out.Scope != "work:other:shared" {
		t.Fatalf("scope %q", out.Scope)
	}
	for _, p := range decodeSetup[setupPrincipal](t, out, "principals/osmia.yaml") {
		if len(p.Scopes) != 2 || p.Scopes[1] != "work:other:shared" {
			t.Fatalf("principal %s is not granted the override: %+v", p.ID, p.Scopes)
		}
	}
}

// The fixture is the generated configuration for Hearsay's directory loader;
// `hearsay config validate` accepts it.
func TestMemorySetupMatchesConnectorConfigurationContract(t *testing.T) {
	t.Parallel()
	_, cfg := conversationFixture(t, "setup-contract-")
	repo, err := trace.Open(cfg.Root, cfg.Project)
	must(t, err)
	defer repo.Close()
	out, err := buildMemorySetup(cfg, repo)
	must(t, err)
	fixture, err := os.ReadFile("testdata/memory-setup.json")
	must(t, err)
	var expected MemorySetup
	must(t, json.Unmarshal(fixture, &expected))
	actual, _ := json.Marshal(out)
	want, _ := json.Marshal(expected)
	var a, b any
	must(t, json.Unmarshal(actual, &a))
	must(t, json.Unmarshal(want, &b))
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("setup contract differs:\n%s\nwant:\n%s", actual, want)
	}
}
