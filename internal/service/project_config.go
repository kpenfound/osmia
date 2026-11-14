package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"reflect"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/redact"
)

func (s *Service) projectConfigRequest(w http.ResponseWriter, r *http.Request, id string) {
	switch r.Method {
	case http.MethodGet:
		if out, api := s.readProjectConfig(config.ProjectID(id)); api != nil {
			failWith(w, api)
		} else {
			respond(w, 200, out)
		}
	case http.MethodPut:
		var edit ProjectConfigEdit
		if !decode(w, r, &edit) {
			return
		}
		if out, api := s.saveProjectConfig(r.Context(), config.ProjectID(id), edit); api != nil {
			failWith(w, api)
		} else {
			respond(w, 200, out)
		}
	default:
		fail(w, Unsupported)
	}
}

// readProjectConfig returns the editable snapshot of an active project's
// config.toml: the bytes on disk decoded once, so the values, the markers and
// the pin describe the same file. The loaded configuration supplies the
// values in effect. A file that does not load yields the loader's error, the
// values in effect and no pin.
func (s *Service) readProjectConfig(raw config.ProjectID) (ProjectConfigResponse, *APIError) {
	id, api := s.projectFor(raw)
	if api != nil {
		return ProjectConfigResponse{}, api
	}
	cfg := s.current()
	if !cfg.Active(id) {
		return ProjectConfigResponse{}, &APIError{Code: NotFound, Message: fmt.Sprintf("project %s is not an active project", id)}
	}
	path, err := cfg.Root.ProjectConfig(id)
	if err != nil {
		return ProjectConfigResponse{}, &APIError{Code: Internal, Message: "cannot locate the project configuration file"}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		err = &config.FieldError{Path: path, Reason: "cannot be read", Err: err}
	}
	return s.projectConfigSnapshot(cfg, id, path, data, err), nil
}

// projectConfigSnapshot decodes data, the bytes of the project's file (or
// readErr when they could not be read), against the loaded configuration cfg.
func (s *Service) projectConfigSnapshot(cfg *config.Config, id config.ProjectID, path string, data []byte, readErr error) ProjectConfigResponse {
	out := ProjectConfigResponse{Project: id, Path: path, Settings: []ProjectConfigSetting{}}
	loaded := cfg.For(id).Project
	var file config.ProjectFile
	err := readErr
	if err == nil {
		file, err = cfg.DecodeProject(id, s.options.Config.Home, data)
	}
	if err != nil {
		failed := reloadError(err, time.Time{})
		out.Error = &ProjectConfigError{Field: failed.Field, Message: failed.Message}
	} else {
		out.Pin = config.ProjectPin(data)
	}
	for _, name := range config.ProjectSettings {
		setting := ProjectConfigSetting{Name: name, Effective: settingValue(loaded, name)}
		if err == nil {
			setting.Value = settingValue(file.Project, name)
			isDefault := !file.Set(name)
			setting.Default = &isDefault
			setting.Pending = !reflect.DeepEqual(setting.Value, setting.Effective)
		}
		out.Settings = append(out.Settings, setting)
	}
	return out
}

// settingValue is the value a project holds for a setting of its file. The
// hearsay_entities mapping is never nil, so an empty one encodes as {}.
func settingValue(p config.Project, name string) any {
	switch name {
	case "name":
		return p.Name
	case "upstream":
		return p.Upstream
	case "fork":
		return p.Fork
	case "clone":
		return p.Clone
	case "base_branch":
		return p.BaseBranch
	case "landing":
		return p.Landing
	case "classifier":
		return p.Classifier
	case "upstream_rebase":
		return p.UpstreamRebase
	case "pull_request_poll":
		return p.PullRequestPoll
	case "checks_timeout":
		return p.ChecksTimeout
	case "capacity.per_workstream":
		return p.Capacity.PerWorkstream
	case "capacity.git_operations":
		return p.Capacity.GitOperations
	case "hearsay_scope":
		return p.HearsayScope
	case "hearsay_declare_repo":
		return p.HearsayDeclareRepo
	case "hearsay_entities":
		entities := map[string]string{}
		maps.Copy(entities, p.HearsayEntities)
		return entities
	}
	return nil
}

// saveProjectConfig applies a pinned edit to an active project's config.toml.
//
// Under projectMu it checks the pin against the file on disk, edits the
// pinned bytes as text, validates the result as loading does, and prepares the
// running service's adoption of it before anything is written. Any failure
// until then refuses the save and leaves the file and the loaded
// configuration as they were. Only the saved project's settings are adopted;
// other files with unapplied edits stay unapplied. Once the file is replaced
// the outcome is never a refusal: a failure then is reported as saved but not
// applied.
func (s *Service) saveProjectConfig(ctx context.Context, raw config.ProjectID, edit ProjectConfigEdit) (ProjectConfigSaveResponse, *APIError) {
	s.projectMu.Lock()
	defer s.projectMu.Unlock()
	id, refusal := s.projectFor(raw)
	if refusal != nil {
		return ProjectConfigSaveResponse{}, refusal
	}
	if edit.Pin == "" {
		return ProjectConfigSaveResponse{}, &APIError{Code: Validation, Field: "pin", Message: "pin: read the project configuration first and send its pin"}
	}
	cfg, projects := s.runtimes()
	path, err := cfg.Root.ProjectConfig(id)
	if err != nil {
		return ProjectConfigSaveResponse{}, &APIError{Code: Internal, Message: "cannot locate the project configuration file"}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ProjectConfigSaveResponse{}, refusedUnchanged(&APIError{Code: Internal, Message: path + ": cannot be read"})
	}
	if config.ProjectPin(data) != edit.Pin {
		return ProjectConfigSaveResponse{}, &APIError{Code: Conflict, Message: path + " changed since it was read; read it again before saving. Nothing was written"}
	}
	// A file that does not load is fixed by hand: no save applies to it.
	if _, err := cfg.DecodeProject(id, s.options.Config.Home, data); err != nil {
		return ProjectConfigSaveResponse{}, refusedUnchanged(&APIError{Code: Validation, Message: "the file does not load, so it cannot be saved until it is fixed by hand: " + reloadError(err, time.Time{}).Message})
	}
	edited, file, err := cfg.EditProject(id, s.options.Config.Home, data, projectEdit(edit))
	if err != nil {
		var field *config.FieldError
		if errors.As(err, &field) {
			return ProjectConfigSaveResponse{}, refusedUnchanged(&APIError{Code: Validation, Field: field.Field, Message: field.Error()})
		}
		return ProjectConfigSaveResponse{}, refusedUnchanged(&APIError{Code: Internal, Message: path + ": cannot be edited"})
	}
	adoption, err := s.prepareProjectAdoption(ctx, cfg, projects, id, file.Project)
	if err != nil {
		return ProjectConfigSaveResponse{}, refusedUnchanged(&APIError{Code: Internal, Message: "cannot apply the saved settings to the running service: " + redact.Secrets(err.Error(), redact.KnownSecrets()...)})
	}
	// Preparation may have taken a while: the pin must still describe the file.
	if again, err := os.ReadFile(path); err != nil || !bytes.Equal(again, data) {
		return ProjectConfigSaveResponse{}, &APIError{Code: Conflict, Message: path + " changed since it was read; read it again before saving. Nothing was written"}
	}
	werr := s.step("project-config-write")
	if werr == nil {
		werr = s.replaceProjectConfig(path, edited)
	}
	if werr != nil {
		// Only a file that holds the edited bytes was replaced; a failure that
		// leaves anything else is a refusal.
		if now, err := os.ReadFile(path); err != nil || !bytes.Equal(now, edited) || bytes.Equal(data, edited) {
			return ProjectConfigSaveResponse{}, refusedUnchanged(&APIError{Code: Internal, Message: path + ": cannot be written: " + redact.Secrets(werr.Error(), redact.KnownSecrets()...)})
		}
		return s.notApplied(cfg, id, path, edited, "the file was replaced but writing it did not finish cleanly: "+redact.Secrets(werr.Error(), redact.KnownSecrets()...)), nil
	}
	if err := s.step("project-config-written"); err != nil {
		return s.notApplied(cfg, id, path, edited, redact.Secrets(err.Error(), redact.KnownSecrets()...)), nil
	}
	s.adoptProject(adoption, path)
	s.log().Info("project configuration saved", "project", string(id), "pin", config.ProjectPin(edited))
	return ProjectConfigSaveResponse{ProjectConfigResponse: s.projectConfigSnapshot(adoption.next, id, path, edited, nil), Outcome: ProjectConfigApplied}, nil
}

// refusedUnchanged finishes the message of a refusal with what it guarantees.
func refusedUnchanged(e *APIError) *APIError {
	e.Message += "; the file and the loaded configuration are unchanged"
	return e
}

// notApplied reports a save whose file was replaced but not adopted. The
// snapshot is of the saved bytes against the configuration still in effect, so
// the saved values read as not yet applied; a reload or a save with its pin
// applies them.
func (s *Service) notApplied(cfg *config.Config, id config.ProjectID, path string, saved []byte, reason string) ProjectConfigSaveResponse {
	s.log().Error("project configuration saved but not applied", "project", string(id), "reason", reason)
	s.hub.Publish(Event{Kind: EventConfig})
	return ProjectConfigSaveResponse{ProjectConfigResponse: s.projectConfigSnapshot(cfg, id, path, saved, nil), Outcome: ProjectConfigSavedNotApplied, Reason: reason}
}

// projectEdit translates a request into the settings the file edit changes.
func projectEdit(e ProjectConfigEdit) config.ProjectEdit {
	out := config.ProjectEdit{Strings: map[string]string{}, Ints: map[string]int{}, Bools: map[string]bool{}, Entities: e.Set.HearsayEntities, Clear: e.Clear, ClearEntities: e.ClearHearsayEntities}
	for name, value := range map[string]*string{"name": e.Set.Name, "upstream": e.Set.Upstream, "fork": e.Set.Fork, "clone": e.Set.Clone, "base_branch": e.Set.BaseBranch, "landing": e.Set.Landing, "classifier": e.Set.Classifier, "upstream_rebase": e.Set.UpstreamRebase, "pull_request_poll": e.Set.PullRequestPoll, "checks_timeout": e.Set.ChecksTimeout, "hearsay_scope": e.Set.HearsayScope} {
		if value != nil {
			out.Strings[name] = *value
		}
	}
	for name, value := range map[string]*int{"capacity.per_workstream": e.Set.CapacityPerWorkstream, "capacity.git_operations": e.Set.CapacityGitOperations} {
		if value != nil {
			out.Ints[name] = *value
		}
	}
	if e.Set.HearsayDeclareRepo != nil {
		out.Bools["hearsay_declare_repo"] = *e.Set.HearsayDeclareRepo
	}
	return out
}

// projectAdoption is everything the running service needs to adopt one
// project's saved settings, prepared so that adopting it cannot fail.
type projectAdoption struct {
	next    *config.Config
	runtime *activeProject
	stage   *stages
	resolve func()
}

// prepareProjectAdoption does the fallible work of adopting project p over
// cfg, as a reload does: stages for the project's passes, the engine, and the
// runtime state resolved against the candidate. It changes nothing.
func (s *Service) prepareProjectAdoption(ctx context.Context, cfg *config.Config, projects []*activeProject, id config.ProjectID, p config.Project) (projectAdoption, error) {
	if err := s.step("project-config-prepare"); err != nil {
		return projectAdoption{}, err
	}
	a := projectAdoption{next: cfg.WithLoadedProject(p)}
	for _, running := range projects {
		if running.id != id {
			continue
		}
		var err error
		if a.stage, err = s.stages(a.next.For(id), running.repository); err != nil {
			return projectAdoption{}, err
		}
		a.runtime = running
	}
	if err := s.ensureEngine(ctx, a.next); err != nil {
		return projectAdoption{}, err
	}
	in, err := s.runtimeInputs(a.next, projects)
	if err != nil {
		return projectAdoption{}, err
	}
	if a.resolve, err = s.store.PrepareResolve(in); err != nil {
		return projectAdoption{}, err
	}
	return a, nil
}

// adoptProject publishes a prepared adoption: the loaded configuration
// changes, the project's passes and conversations take its stages from their
// next step, and clients are told. A reload failure recorded for this file is
// stale once the file is applied.
func (s *Service) adoptProject(a projectAdoption, path string) {
	s.mu.Lock()
	s.cfg = a.next
	a.resolve()
	if a.runtime != nil {
		a.runtime.pipeline.next.Store(a.stage)
		a.runtime.pipeline.conversation.Store(a.stage)
	}
	if s.reloadErr != nil && s.reloadErr.Path == path {
		s.reloadErr = nil
	}
	s.mu.Unlock()
	s.hub.Publish(Event{Kind: EventResync}, Event{Kind: EventConfig})
}

// replaceProjectConfig replaces the file, through the test seam when set.
func (s *Service) replaceProjectConfig(path string, data []byte) error {
	s.boundaryMu.RLock()
	write := s.configWrite
	s.boundaryMu.RUnlock()
	if write == nil {
		write = config.ReplaceFile
	}
	return write(path, data)
}

func (s *Service) setConfigWriter(write func(path string, data []byte) error) {
	s.boundaryMu.Lock()
	s.configWrite = write
	s.boundaryMu.Unlock()
}
