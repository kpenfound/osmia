package service

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/kb"
	"github.com/kpenfound/osmia/internal/trace"
)

// MemorySetup contains reviewable Hearsay configuration fragments, not applied
// configuration. JSON contents are valid YAML for Hearsay's directory loader.
type MemorySetup struct {
	Files    map[string]json.RawMessage `json:"files"`
	Entities map[string]string          `json:"hearsay_entities"`
	Scope    string                     `json:"hearsay_scope"`
	Anchors  []MemoryAnchor             `json:"anchors"`
}
type MemoryAnchor struct {
	Source   string `json:"source"`
	Artifact string `json:"artifact"`
	Scope    string `json:"scope"`
}

func (s *Service) memorySetup(w http.ResponseWriter, r *http.Request, raw string) {
	if r.Method != http.MethodGet {
		fail(w, Unsupported)
		return
	}
	id, api := s.projectFor(config.ProjectID(raw))
	if api != nil {
		failWith(w, api)
		return
	}
	repo, err := s.repository(id)
	if err != nil {
		documentFailure(w, err)
		return
	}
	cfg := s.about(repo)
	entities, err := kb.Load(repo)
	if err != nil {
		documentFailure(w, err)
		return
	}
	out, err := buildMemorySetup(cfg, repo, entities)
	if err != nil {
		documentFailure(w, err)
		return
	}
	respond(w, http.StatusOK, out)
}

func buildMemorySetup(cfg *config.Config, repo *trace.Repository, local kb.Map) (MemorySetup, error) {
	id := string(repo.Project())
	ownerSource := "osmia-owner-" + id[2:]
	workSource := "osmia-work-" + id[2:]
	principal := cfg.Hearsay.Principal
	if principal == "" {
		principal = "osmia-owner"
	}
	token := cfg.Hearsay.TokenEnv
	if token == "" {
		token = "HEARSAY_OWNER_TOKEN"
	}
	scope := cfg.Project.HearsayScope
	if scope == "" {
		scope = "osmia-" + id[2:]
	}
	entityRoot := "code:" + cfg.Project.Upstream
	out := MemorySetup{Files: map[string]json.RawMessage{}, Entities: map[string]string{}, Scope: scope, Anchors: []MemoryAnchor{}}
	put := func(path string, value any) error {
		data, err := json.MarshalIndent(value, "", "  ")
		out.Files[path] = data
		return err
	}
	identity := map[string]string{"source": ownerSource, "kind": "user", "native_id": "local"}
	sources := []any{}
	for _, channel := range []string{"owner", "work"} {
		source := ownerSource
		if channel == "work" {
			source = workSource
		}
		sources = append(sources, map[string]any{"id": source, "type": "osmia", "containers": []string{id}, "read_only": true, "settings": map[string]any{"root": "/mnt/osmia", "owner": identity, "channel": channel}})
	}
	if err := put("sources/osmia-"+id+".yaml", sources); err != nil {
		return out, err
	}
	principals := []any{map[string]any{"id": principal, "kind": "human", "token_env": token, "scopes": []string{entityRoot}, "identities": []any{map[string]string{"source": ownerSource, "native_id": "local"}}}}
	for _, class := range []string{"worker", "orchestrator", "observer"} {
		agent := cfg.Hearsay.Agents[class]
		if agent.ID == "" {
			agent = config.HearsayAgent{ID: "osmia-" + class, TokenEnv: "HEARSAY_OSMIA_" + strings.ToUpper(class) + "_TOKEN"}
		}
		principals = append(principals, map[string]any{"id": agent.ID, "kind": "agent", "class": class, "token_env": agent.TokenEnv, "scopes": []string{entityRoot}, "identities": []any{map[string]string{"source": workSource, "native_id": class}}})
	}
	if err := put("principals/osmia-"+id+".yaml", principals); err != nil {
		return out, err
	}
	code := []any{map[string]any{"id": entityRoot, "type": "project", "name": cfg.Project.Name}}
	projectScope := map[string]any{"id": scope, "name": cfg.Project.Name, "sources": []string{ownerSource, workSource}, "entities": []string{entityRoot}}
	scopes := []any{projectScope}
	scopeByID := map[string]map[string]any{scope: projectScope}
	for _, entity := range local.Entities {
		mapped := cfg.Project.HearsayEntities[entity.ID]
		if mapped == "" {
			mapped = fmt.Sprintf("osmia-%s-%x", id[2:18], sha256.Sum256([]byte(entity.ID)))[:55]
		}
		out.Entities[entity.ID] = mapped
	}
	for _, entity := range local.Entities {
		parents := []string{}
		for _, parent := range entity.PartOf {
			parents = append(parents, entityRoot+":"+parent)
		}
		if len(parents) == 0 {
			parents = []string{entityRoot}
		}
		entry := map[string]any{"id": entityRoot + ":" + entity.ID, "type": "module", "name": entity.Name, "aliases": entity.Aliases, "path_patterns": entity.Paths, "part_of": parents, "repo": map[string]string{"source": workSource, "project": id}}
		// Local CODEOWNERS handles need principal resolution by the operator. Never
		// grant an unmapped handle authority by treating it as a Hearsay principal.
		code = append(code, entry)
		mapped := out.Entities[entity.ID]
		if existing, ok := scopeByID[mapped]; ok {
			existing["entities"] = append(existing["entities"].([]string), entityRoot+":"+entity.ID)
		} else {
			entry := map[string]any{"id": mapped, "name": entity.Name, "sources": []string{ownerSource, workSource}, "entities": []string{entityRoot + ":" + entity.ID}}
			scopeByID[mapped] = entry
			scopes = append(scopes, entry)
		}
	}
	if err := put("code/osmia-"+id+".yaml", code); err != nil {
		return out, err
	}
	if err := put("scopes/osmia-"+id+".yaml", scopes); err != nil {
		return out, err
	}
	policies := []any{}
	seen := map[string]bool{}
	for _, sc := range append([]string{scope}, sortedMappingValues(out.Entities)...) {
		if seen[sc] {
			continue
		}
		seen[sc] = true
		policies = append(policies, map[string]any{"scope": sc, "ratified_by": map[string]any{"principals": []string{principal}, "sources": []string{ownerSource}, "artifacts": []string{"spec"}}})
	}
	if err := put("authority/osmia-"+id+".yaml", policies); err != nil {
		return out, err
	}
	streams, err := repo.Workstreams()
	if err != nil {
		return out, err
	}
	for _, stream := range append([]config.WorkstreamID{""}, streams...) {
		docs, err := trace.Read[trace.Document](repo, stream)
		if err != nil {
			return out, err
		}
		for _, doc := range latestDocuments(docs) {
			if doc.Content == "" {
				continue
			}
			source := workSource
			anchor := false
			if doc.Path == "charter.md" {
				source = ownerSource
				anchor = true
			}
			if doc.Path == "spec.md" || strings.HasPrefix(doc.Path, "kb/") && strings.HasSuffix(doc.Path, ".md") {
				anchor = true
			}
			if anchor {
				out.Anchors = append(out.Anchors, MemoryAnchor{Source: source, Artifact: id + "/" + string(stream) + "/document/" + doc.ID, Scope: scope})
			}
		}
	}
	slices.SortFunc(out.Anchors, func(a, b MemoryAnchor) int { return strings.Compare(a.Artifact, b.Artifact) })
	return out, nil
}
func sortedMappingValues(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	slices.Sort(out)
	return out
}
