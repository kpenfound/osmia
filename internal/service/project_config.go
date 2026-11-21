package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/projectstore"
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
// stored settings: the current revision decoded once, so the values, the
// markers and the pin describe the same revision. The loaded configuration
// supplies the values in effect. Settings that do not load yield the loader's
// error, the values in effect and no pin.
func (s *Service) readProjectConfig(raw config.ProjectID) (ProjectConfigResponse, *APIError) {
	id, api := s.projectFor(raw)
	if api != nil {
		return ProjectConfigResponse{}, api
	}
	cfg := s.current()
	if !cfg.Active(id) {
		return ProjectConfigResponse{}, &APIError{Code: NotFound, Message: fmt.Sprintf("project %s is not an active project", id)}
	}
	revision, err := s.settingsStore().Current(id)
	if err != nil {
		return ProjectConfigResponse{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot read the stored settings of project %s; check the root's project-store directory and its permissions", id)}
	}
	return s.projectConfigSnapshot(cfg, id, revision), nil
}

// projectConfigSnapshot decodes revision, the project's stored settings,
// against the loaded configuration cfg.
func (s *Service) projectConfigSnapshot(cfg *config.Config, id config.ProjectID, revision projectstore.Revision) ProjectConfigResponse {
	out := ProjectConfigResponse{Project: id, Revision: revision.Number, Settings: []ProjectConfigSetting{}}
	loaded := cfg.For(id).Project
	file, err := cfg.DecodeProject(id, s.options.Config.Home, revision.Data)
	if err != nil {
		failed := reloadError(projectstore.Relabel(cfg.Root, id, err), time.Time{})
		out.Error = &ProjectConfigError{Field: failed.Field, Message: failed.Message}
	} else {
		out.Pin = revision.Pin
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

// settingValue is the value a project holds for a setting of its file.
func settingValue(p config.Project, name string) any { return p.SettingValue(name) }

// saveProjectConfig applies a pinned edit to an active project's stored
// settings.
//
// Under projectMu it checks the pin against the current stored revision, edits
// the pinned revision's text, validates the result as loading does, and
// prepares the running service's adoption of it before anything is written.
// Any failure until then refuses the save and leaves the stored settings and
// the loaded configuration as they were. Only the saved project's settings are
// adopted; a revision saved earlier and not adopted stays pending until a save
// builds on it. Once the revision is recorded the outcome is never a refusal: a
// failure then is reported as saved but not applied.
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
	store := s.settingsStore()
	current, err := store.Current(id)
	if err != nil {
		return ProjectConfigSaveResponse{}, refusedUnchanged(&APIError{Code: Internal, Message: fmt.Sprintf("the stored settings of project %s cannot be read", id)})
	}
	stale := &APIError{Code: Conflict, Message: fmt.Sprintf("the settings of project %s changed since they were read; read them again before saving. Nothing was written", id)}
	if current.Pin != edit.Pin {
		return ProjectConfigSaveResponse{}, stale
	}
	// Stored settings that do not load are repaired offline: no save applies
	// to them.
	if _, err := cfg.DecodeProject(id, s.options.Config.Home, current.Data); err != nil {
		err = projectstore.Relabel(cfg.Root, id, err)
		return ProjectConfigSaveResponse{}, refusedUnchanged(&APIError{Code: Validation, Message: "the stored settings do not load, so they cannot be saved here: " + reloadError(err, time.Time{}).Message + "; " + projectstore.RepairHint(id, err)})
	}
	edited, file, err := cfg.EditProject(id, s.options.Config.Home, current.Data, projectEdit(edit))
	if err != nil {
		err = projectstore.Relabel(cfg.Root, id, err)
		var field *config.FieldError
		if errors.As(err, &field) {
			return ProjectConfigSaveResponse{}, refusedUnchanged(&APIError{Code: Validation, Field: field.Field, Message: field.Error()})
		}
		return ProjectConfigSaveResponse{}, refusedUnchanged(&APIError{Code: Internal, Message: fmt.Sprintf("the stored settings of project %s cannot be edited", id)})
	}
	adoption, err := s.prepareProjectAdoption(ctx, cfg, projects, id, file.Project)
	if err != nil {
		return ProjectConfigSaveResponse{}, refusedUnchanged(&APIError{Code: Internal, Message: "cannot apply the saved settings to the running service: " + redact.Secrets(err.Error(), redact.KnownSecrets()...)})
	}
	werr := s.step("project-config-write")
	var saved projectstore.Revision
	if werr == nil {
		saved, werr = store.Replace(id, current.Pin, edited)
	}
	if werr != nil {
		if errors.Is(werr, projectstore.ErrStale) {
			return ProjectConfigSaveResponse{}, stale
		}
		// Only a recorded revision that holds the edited bytes is a save; a
		// failure that leaves anything else is a refusal.
		now, err := store.Current(id)
		if err != nil || now.Number != current.Number+1 || !bytes.Equal(now.Data, edited) {
			return ProjectConfigSaveResponse{}, refusedUnchanged(&APIError{Code: Internal, Message: fmt.Sprintf("the settings of project %s cannot be written: %s", id, redact.Secrets(werr.Error(), redact.KnownSecrets()...))})
		}
		return s.notApplied(cfg, id, now, "the revision was recorded but writing it did not finish cleanly: "+redact.Secrets(werr.Error(), redact.KnownSecrets()...)), nil
	}
	if err := s.step("project-config-written"); err != nil {
		return s.notApplied(cfg, id, saved, redact.Secrets(err.Error(), redact.KnownSecrets()...)), nil
	}
	s.adoptProject(adoption, id, saved.Number)
	s.log().Info("project configuration saved", "project", string(id), "revision", saved.Number, "pin", saved.Pin)
	return ProjectConfigSaveResponse{ProjectConfigResponse: s.projectConfigSnapshot(adoption.next, id, saved), Outcome: ProjectConfigApplied}, nil
}

// refusedUnchanged finishes the message of a refusal with what it guarantees.
func refusedUnchanged(e *APIError) *APIError {
	e.Message += "; the stored settings and the loaded configuration are unchanged"
	return e
}

// notApplied reports a save whose revision was recorded but not adopted. The
// snapshot is of the saved revision against the configuration still in
// effect, so the saved values read as not yet applied. They stay pending until
// the project's next save, which builds on the revision, or a restart; a reload
// does not adopt them.
func (s *Service) notApplied(cfg *config.Config, id config.ProjectID, saved projectstore.Revision, reason string) ProjectConfigSaveResponse {
	s.log().Error("project configuration saved but not applied", "project", string(id), "reason", reason)
	s.hub.Publish(Event{Kind: EventConfig})
	return ProjectConfigSaveResponse{ProjectConfigResponse: s.projectConfigSnapshot(cfg, id, saved), Outcome: ProjectConfigSavedNotApplied, Reason: reason}
}

// projectEdit translates a request into the settings the file edit changes.
func projectEdit(e ProjectConfigEdit) config.ProjectEdit {
	out := config.ProjectEdit{Strings: map[string]string{}, Ints: map[string]int{}, Bools: map[string]bool{}, Entities: e.Set.HearsayEntities, Clear: e.Clear, ClearEntities: e.ClearHearsayEntities}
	for name, value := range map[string]*string{"name": e.Set.Name, "upstream": e.Set.Upstream, "fork": e.Set.Fork, "clone": e.Set.Clone, "base_branch": e.Set.BaseBranch, "landing": e.Set.Landing, "delivery": e.Set.Delivery, "upstream_rebase": e.Set.UpstreamRebase, "pull_request_poll": e.Set.PullRequestPoll, "checks_timeout": e.Set.ChecksTimeout, "hearsay_scope": e.Set.HearsayScope} {
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
	if e.Set.FlakeFixes != nil {
		out.Bools["flake_fixes"] = *e.Set.FlakeFixes
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
// next step, and clients are told. revision is the stored revision the adopted
// settings come from. A reload failure recorded for this project's settings is
// stale once they are applied.
func (s *Service) adoptProject(a projectAdoption, id config.ProjectID, revision int) {
	s.mu.Lock()
	s.cfg = a.next
	s.setEffective(id, revision)
	a.resolve()
	if a.runtime != nil {
		a.runtime.pipeline.next.Store(a.stage)
		a.runtime.pipeline.conversation.Store(a.stage)
	}
	if s.reloadErr != nil && s.reloadErr.Path == config.ProjectSettingsLabel(id) {
		s.reloadErr = nil
	}
	s.mu.Unlock()
	s.hub.Publish(Event{Kind: EventResync}, Event{Kind: EventConfig})
}
