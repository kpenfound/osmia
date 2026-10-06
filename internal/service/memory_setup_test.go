package service

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/kb"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestMemorySetupIsLocalAndSeparatesOwnerAuthority(t *testing.T) {
	t.Parallel()
	opts, clone := projectFixture(t)
	_, c := start(t, opts)
	added, err := c.AddProject(context.Background(), request(clone))
	must(t, err)
	var setup MemorySetup
	must(t, c.Do(context.Background(), http.MethodGet, Prefix+"/projects/memory/"+string(added.Project.ID), nil, &setup))
	if len(setup.Files) != 5 || len(setup.Anchors) != 1 || !strings.HasPrefix(setup.Scope, "osmia-") {
		t.Fatalf("setup %+v", setup)
	}
	for name, raw := range setup.Files {
		if !json.Valid(raw) {
			t.Fatalf("invalid configuration %s", name)
		}
		if strings.HasPrefix(name, "authority/") {
			var rules []struct {
				Scope    string `json:"scope"`
				Ratified struct {
					Sources   []string `json:"sources"`
					Artifacts []string `json:"artifacts"`
				} `json:"ratified_by"`
			}
			must(t, json.Unmarshal(raw, &rules))
			for _, rule := range rules {
				if len(rule.Ratified.Sources) != 1 || !strings.HasPrefix(rule.Ratified.Sources[0], "osmia-owner-") || strings.Join(rule.Ratified.Artifacts, ",") != "spec" {
					t.Fatalf("agent source can ratify: %s", raw)
				}
			}
		}
	}
	var again MemorySetup
	must(t, c.Do(context.Background(), http.MethodGet, Prefix+"/projects/memory/"+string(added.Project.ID), nil, &again))
	first, _ := json.Marshal(setup)
	second, _ := json.Marshal(again)
	if string(first) != string(second) {
		t.Fatal("setup identity changed on retry")
	}
}

func TestMemorySetupCombinesEntitiesMappedToOneScope(t *testing.T) {
	t.Parallel()
	_, cfg := conversationFixture(t, "setup-shared-")
	repo, err := trace.Open(cfg.Root, cfg.Project)
	must(t, err)
	defer repo.Close()
	cfg.Project.HearsayScope = "shared"
	cfg.Project.HearsayEntities = map[string]string{"storage": "shared", "api": "shared"}
	local := kb.Map{Version: 1, Entities: []kb.Entity{{ID: "storage", Name: "Storage"}, {ID: "api", Name: "API"}}}
	out, err := buildMemorySetup(cfg, repo, local)
	must(t, err)
	for name, raw := range out.Files {
		if strings.HasPrefix(name, "scopes/") {
			var scopes []struct {
				ID       string   `json:"id"`
				Entities []string `json:"entities"`
			}
			must(t, json.Unmarshal(raw, &scopes))
			if len(scopes) != 1 || scopes[0].ID != "shared" || len(scopes[0].Entities) != 3 {
				t.Fatalf("shared scope: %s", raw)
			}
		}
		if strings.HasPrefix(name, "authority/") {
			var policies []map[string]any
			must(t, json.Unmarshal(raw, &policies))
			if len(policies) != 1 || policies[0]["scope"] != "shared" {
				t.Fatalf("shared authority: %s", raw)
			}
		}
	}
}

// The same configuration fixture is validated by Hearsay's directory loader.
func TestMemorySetupMatchesConnectorConfigurationContract(t *testing.T) {
	t.Parallel()
	_, cfg := conversationFixture(t, "setup-contract-")
	repo, err := trace.Open(cfg.Root, cfg.Project)
	must(t, err)
	defer repo.Close()
	cfg.Project.Name = "Example"
	cfg.Project.Upstream = "owner/example"
	local := kb.Map{Version: 1, Entities: []kb.Entity{{ID: "storage", Name: "Storage", Aliases: []string{"storage"}, Paths: []string{"internal/storage/**"}}}}
	out, err := buildMemorySetup(cfg, repo, local)
	must(t, err)
	fixture, err := os.ReadFile("testdata/memory-setup.json")
	must(t, err)
	var expected MemorySetup
	must(t, json.Unmarshal(fixture, &expected))
	out.Anchors = nil
	actual, _ := json.Marshal(out)
	want, _ := json.Marshal(expected)
	var a, b any
	must(t, json.Unmarshal(actual, &a))
	must(t, json.Unmarshal(want, &b))
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("setup contract differs:\n%s\nwant:\n%s", actual, want)
	}
}
