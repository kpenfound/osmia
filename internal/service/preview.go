package service

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
)

func (s *Service) previewHTTP(w http.ResponseWriter, r *http.Request, raw string) {
	id, err := config.ParseProjectID(raw)
	if err != nil {
		failWith(w, &APIError{Code: Validation, Message: err.Error()})
		return
	}
	var data []byte
	if r.Method == http.MethodPut {
		var body map[string]json.RawMessage
		if !decode(w, r, &body) {
			return
		}
		data, _ = json.Marshal(body)
	}
	out, apiErr := s.savePreview(id, data)
	if apiErr != nil {
		failWith(w, apiErr)
		return
	}
	respond(w, http.StatusOK, out)
}

func (s *Service) savePreview(id config.ProjectID, data []byte) (api.ProjectPreviewResponse, *APIError) {
	s.projectMu.Lock()
	defer s.projectMu.Unlock()
	cfg, projects := s.runtimes()
	if !cfg.Active(id) {
		return api.ProjectPreviewResponse{}, &APIError{Code: NotFound, Message: "project is not active"}
	}
	path, err := cfg.Root.ProjectConfig(id)
	if err != nil {
		return api.ProjectPreviewResponse{}, &APIError{Code: Internal, Message: "cannot locate project config.toml"}
	}
	// Check drift before validation or adapter preparation, even for a
	// request whose value is already loaded.
	current := cfg.For(id).Project
	if _, err := current.SavePreview(path, current.Preview); err != nil {
		return api.ProjectPreviewResponse{}, previewSaveError(err)
	}
	var preview *config.Preview
	if data != nil {
		preview, err = config.DecodePreviewJSON(data)
		if err != nil {
			return api.ProjectPreviewResponse{}, previewSaveError(err)
		}
	}
	// Prepare runtime adapters before the file is written. Existing operations
	// retain their stages while new conversations adopt the setting immediately.
	next := *cfg
	next.Projects = slices.Clone(cfg.Projects)
	for i := range next.Projects {
		if next.Projects[i].ID == id {
			next.Projects[i].Preview = preview
		}
	}
	if next.Project.ID == id {
		next.Project.Preview = preview
	}
	staged := map[*activeProject]*stages{}
	for _, active := range projects {
		if active.id == id {
			stage, err := s.stages(next.For(id), active.repository)
			if err != nil {
				return api.ProjectPreviewResponse{}, &APIError{Code: Internal, Message: "cannot apply preview configuration"}
			}
			staged[active] = stage
		}
	}
	saved, err := cfg.For(id).Project.SavePreview(path, preview)
	if err != nil {
		return api.ProjectPreviewResponse{}, previewSaveError(err)
	}
	for i := range next.Projects {
		if next.Projects[i].ID == id {
			next.Projects[i] = saved
		}
	}
	if next.Project.ID == id {
		next.Project = saved
	}
	s.mu.Lock()
	s.cfg = &next
	for active, stage := range staged {
		active.pipeline.next.Store(stage)
		active.pipeline.conversation.Store(stage)
	}
	s.mu.Unlock()
	s.hub.Publish(Event{Kind: EventConfig})
	return api.ProjectPreviewResponse{Project: id, Preview: saved.Preview}, nil
}

func previewSaveError(err error) *APIError {
	code := Internal
	var field *config.FieldError
	if errors.Is(err, config.ErrProjectDrift) {
		code = Conflict
	} else if errors.As(err, &field) {
		code = Validation
	}
	return &APIError{Code: code, Message: strings.TrimPrefix(err.Error(), ": ")}
}
