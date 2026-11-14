package service

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

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
	out, err := buildMemorySetup(s.about(repo), repo)
	if err != nil {
		documentFailure(w, err)
		return
	}
	respond(w, http.StatusOK, out)
}

// Defaults for the Hearsay pair when [hearsay.publish] or the principal
// settings do not name one.
const (
	defaultWorkSource   = "osmia-work"
	defaultOwnerSource  = "osmia-work-owner"
	defaultPublisher    = "osmia-publisher"
	defaultPublisherEnv = "HEARSAY_OSMIA_PUBLISHER_TOKEN"
	defaultOwner        = "osmia-owner"
	defaultOwnerEnv     = "HEARSAY_OWNER_TOKEN"
)

// buildMemorySetup returns the Hearsay configuration for one project. The
// work source holds the project's records and the owner source holds what the
// owner decided; the publisher agent is the only poster on both and an
// attributor, so the owner source's records can name the owner as author. Only
// the owner source's documents and decisions ratify. The class agents read
// acting for the owner and are not posters.
func buildMemorySetup(cfg *config.Config, repo *trace.Repository) (MemorySetup, error) {
	id := string(repo.Project())
	out := MemorySetup{Files: map[string]json.RawMessage{}, WorkSource: defaultWorkSource, OwnerSource: defaultOwnerSource, Publisher: defaultPublisher, Principal: cfg.Hearsay.Principal}
	publisherEnv := defaultPublisherEnv
	if p := cfg.Hearsay.Publish; p != nil {
		out.WorkSource, out.OwnerSource, out.Publisher, publisherEnv = p.WorkSource, p.OwnerSource, p.Agent, p.TokenEnv
	}
	if out.Principal == "" {
		out.Principal = defaultOwner
	}
	ownerEnv := cfg.Hearsay.TokenEnv
	if ownerEnv == "" {
		ownerEnv = defaultOwnerEnv
	}
	entity := "work:" + out.WorkSource + ":" + id
	out.Scope = entity
	if cfg.Project.HearsayScope != "" {
		out.Scope = cfg.Project.HearsayScope
	}
	grants := []string{entity}
	if out.Scope != entity {
		grants = append(grants, out.Scope)
	}
	put := func(path string, value any) error {
		data, err := json.MarshalIndent(value, "", "  ")
		out.Files[path] = data
		return err
	}
	poster := map[string]any{"posters": []string{out.Publisher}, "attributors": []string{out.Publisher}}
	ownerSettings := map[string]any{"owner_of": out.WorkSource, "posters": []string{out.Publisher}, "attributors": []string{out.Publisher}}
	sources := []any{
		map[string]any{"id": out.WorkSource, "type": "work", "containers": []string{"*"}, "settings": poster},
		map[string]any{"id": out.OwnerSource, "type": "work", "containers": []string{"*"}, "settings": ownerSettings},
	}
	if err := put("sources/osmia.yaml", sources); err != nil {
		return out, err
	}
	// A work source supplies every principal's identity by its id, so none
	// needs an explicit mapping.
	principals := []any{
		map[string]any{"id": out.Principal, "kind": "human", "token_env": ownerEnv, "scopes": grants},
		map[string]any{"id": out.Publisher, "kind": "agent", "class": "observer", "token_env": publisherEnv, "scopes": grants},
	}
	for _, class := range []string{"worker", "orchestrator", "observer"} {
		agent := cfg.Hearsay.Agents[class]
		if agent.ID == "" {
			agent = config.HearsayAgent{ID: "osmia-" + class, TokenEnv: "HEARSAY_OSMIA_" + strings.ToUpper(class) + "_TOKEN"}
		}
		principals = append(principals, map[string]any{"id": agent.ID, "kind": "agent", "class": class, "token_env": agent.TokenEnv, "scopes": grants})
	}
	if err := put("principals/osmia.yaml", principals); err != nil {
		return out, err
	}
	scopes := []any{map[string]any{"id": out.WorkSource, "name": "Osmia projects", "sources": []string{out.WorkSource, out.OwnerSource}}}
	if err := put("scopes/osmia.yaml", scopes); err != nil {
		return out, err
	}
	// Authority follows the source a record arrived on: only the owner
	// source's documents and decisions ratify, whoever they name as author.
	// Hand ratification stays with the owner.
	policies := []any{map[string]any{"scope": out.WorkSource, "ratified_by": map[string]any{"principals": []string{out.Principal}, "sources": []string{out.OwnerSource}, "artifacts": []string{"spec", "decision"}}}}
	if err := put("authority/osmia.yaml", policies); err != nil {
		return out, err
	}
	return out, nil
}
