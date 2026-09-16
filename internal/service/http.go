package service

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/runtime"
)

func respond(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(value)
}
func fail(w http.ResponseWriter, code Code) {
	status, message := http.StatusInternalServerError, "operation failed"
	switch code {
	case NoProject:
		status, message = 409, "no project is configured; add one with osmia project add"
	case Malformed:
		status, message = 400, "expected one JSON object with known, unique fields"
	case Validation:
		status, message = 422, "invalid override; check target, mode, role and profile references"
	case Conflict:
		status, message = 409, "runtime file changed externally; restore it or restart the service"
	case Unsupported:
		status, message = 501, "operation is unsupported in M1"
	case RestartRequired:
		status, message = 409, "operation requires a service restart"
	case Unavailable:
		status, message = 503, "service is unavailable"
	}
	respond(w, status, ErrorResponse{APIError{code, message}})
}

// failWith reports a project operation error with the message the operation
// composed: field names, identities and paths the caller supplied, never raw
// file contents or parser output.
func failWith(w http.ResponseWriter, api *APIError) {
	status := http.StatusInternalServerError
	switch api.Code {
	case Validation:
		status = 422
	case NoProject, ProjectActive, CharterEmpty:
		status = 409
	case NotFound:
		status = 404
	case Unsupported:
		status = 501
	}
	respond(w, status, ErrorResponse{*api})
}
func noProject(field string) Diagnostic {
	return Diagnostic{field, NoProject, "no project is configured; add one with osmia project add"}
}
func digest(c *config.Config) string {
	b, _ := json.Marshal(c)
	return fmt.Sprintf("%x", sha256.Sum256(b))
}
func (s *Service) configuration() ConfigResponse {
	s.mu.Lock()
	cfg, pending := s.cfg, s.pending
	s.mu.Unlock()
	out := ConfigResponse{Root: cfg.Root.String(), Digest: digest(cfg), Effective: cfg, Diagnostics: []Diagnostic{}}
	if cfg.HasProject() {
		view := projectView(cfg.Root, cfg.Project)
		// A project without a trace, or removed since the snapshot, has no
		// charter to report.
		state, err := s.charterState(cfg.Project.ID)
		if err == nil {
			view.CharterState = &state
		} else if !errors.Is(err, errNoTrace) && !errors.Is(err, errNoActiveProject) {
			out.Diagnostics = append(out.Diagnostics, Diagnostic{"charter", Internal, "cannot read or record the charter; check " + view.Charter + " and the trace repository"})
		}
		out.Project = &view
	} else {
		out.Diagnostics = append(out.Diagnostics, noProject("active_projects"))
	}
	if pending != nil {
		out.Diagnostics = append(out.Diagnostics, Diagnostic{"projects", Internal, "an interrupted project registration is incomplete; run osmia project add again to finish it, or inspect project-add.json under the root"})
	}
	current, err := config.Load(s.options.Config)
	if err != nil {
		out.Diagnostics = append(out.Diagnostics, Diagnostic{"configuration", Validation, "disk configuration is invalid or unreadable; loaded configuration retained"})
	} else if digest(current) != out.Digest {
		out.Diagnostics = append(out.Diagnostics, Diagnostic{"configuration", RestartRequired, "disk configuration differs; restart to apply it"})
	}
	return out
}
func (s *Service) runtimeView() RuntimeResponse {
	state, ds := s.store.Effective()
	out := RuntimeResponse{Effective: state, Projects: []ProjectRuntime{}, Diagnostics: []Diagnostic{}}
	if cfg := s.current(); cfg.HasProject() {
		out.Projects = append(out.Projects, ProjectRuntime{cfg.Project.ID, s.Context().Mode(cfg.Project.ID)})
	} else {
		out.Diagnostics = append(out.Diagnostics, noProject("project"))
	}
	for _, d := range ds {
		field := d.Field
		if strings.HasPrefix(field, "profiles.") {
			field = "profiles"
		}
		out.Diagnostics = append(out.Diagnostics, Diagnostic{field, Validation, "stored reference is unavailable; excluded from effective state"})
	}
	if err := s.store.CheckDisk(); err != nil {
		code := Internal
		if errors.Is(err, runtime.ErrConflict) {
			code = Conflict
		}
		out.Diagnostics = append(out.Diagnostics, Diagnostic{"runtime", code, "disk runtime differs or is unreadable; acknowledged state retained"})
	}
	return out
}
func (s *Service) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		switch r.URL.Path {
		case Prefix + "/health":
			respond(w, 200, HealthResponse{true, "osmia", 1, s.options.Build})
			return
		case Prefix + "/config":
			respond(w, 200, s.configuration())
			return
		case Prefix + "/runtime":
			respond(w, 200, s.runtimeView())
			return
		case Prefix + "/status":
			respond(w, 200, s.statusList())
			return
		}
		if id, ok := strings.CutPrefix(r.URL.Path, Prefix+"/conversation/"); ok {
			if out, api := s.conversationList(id); api != nil {
				failWith(w, api)
			} else {
				respond(w, 200, out)
			}
			return
		}
		if id, ok := strings.CutPrefix(r.URL.Path, Prefix+"/status/"); ok {
			if out, api := s.workstreamStatus(id); api != nil {
				failWith(w, api)
			} else {
				respond(w, 200, out)
			}
			return
		}
	}
	if r.Method == http.MethodPut && (r.URL.Path == Prefix+"/config/root" || r.URL.Path == Prefix+"/config/listen") {
		fail(w, RestartRequired)
		return
	}
	if r.URL.Path == Prefix+"/projects" && (r.Method == http.MethodPost || r.Method == http.MethodDelete) {
		var (
			result ProjectResponse
			api    *APIError
		)
		if r.Method == http.MethodPost {
			var v ProjectAddRequest
			if !decode(w, r, &v) {
				return
			}
			result, api = s.addProject(r.Context(), v)
		} else {
			var v ProjectRemoveRequest
			if !decode(w, r, &v) {
				return
			}
			result, api = s.removeProject(v)
		}
		if api != nil {
			failWith(w, api)
			return
		}
		respond(w, 200, result)
		return
	}
	if id, ok := strings.CutPrefix(r.URL.Path, Prefix+"/conversation/"); ok && r.Method == http.MethodPost {
		var v SendRequest
		if !decode(w, r, &v) {
			return
		}
		if out, api := s.send(r.Context(), id, v); api != nil {
			failWith(w, api)
		} else {
			respond(w, 200, out)
		}
		return
	}
	if r.Method == http.MethodPost && r.URL.Path == Prefix+"/handin" {
		var v HandInRequest
		if !decode(w, r, &v) {
			return
		}
		failWith(w, s.handIn(r.Context(), v))
		return
	}
	var err error
	switch {
	case r.Method == http.MethodPut && r.URL.Path == Prefix+"/runtime/pause":
		var v PauseRequest
		if !decode(w, r, &v) {
			return
		}
		err = s.store.SetPause(v)
	case r.Method == http.MethodDelete && r.URL.Path == Prefix+"/runtime/pause":
		var v ClearPauseRequest
		if !decode(w, r, &v) {
			return
		}
		err = s.store.ClearPause(v)
	case r.Method == http.MethodPut && r.URL.Path == Prefix+"/runtime/priority":
		var v PriorityRequest
		if !decode(w, r, &v) {
			return
		}
		err = s.store.SetPriority(v)
	case r.Method == http.MethodDelete && r.URL.Path == Prefix+"/runtime/priority":
		var v ClearPriorityRequest
		if !decode(w, r, &v) {
			return
		}
		err = s.store.ClearPriority(v.Project)
	case r.Method == http.MethodPut && r.URL.Path == Prefix+"/runtime/profile":
		var v ProfileRequest
		if !decode(w, r, &v) {
			return
		}
		err = s.store.SetProfile(v.Role, v.Profile)
	case r.Method == http.MethodDelete && r.URL.Path == Prefix+"/runtime/profile":
		var v ClearProfileRequest
		if !decode(w, r, &v) {
			return
		}
		if strings.TrimSpace(v.Role) == "" {
			fail(w, Validation)
			return
		}
		err = s.store.ClearProfile(v.Role)
	default:
		fail(w, Unsupported)
		return
	}
	if err != nil {
		code := Internal
		if errors.Is(err, runtime.ErrValidation) {
			code = Validation
		}
		if errors.Is(err, runtime.ErrConflict) {
			code = Conflict
		}
		fail(w, code)
		return
	}
	respond(w, 200, MutationResponse{true})
}
func decode(w http.ResponseWriter, r *http.Request, out any) bool {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	data = bytes.TrimSpace(data)
	if err != nil || len(data) == 0 || data[0] != '{' {
		fail(w, Malformed)
		return false
	}
	if err = uniqueJSON(json.NewDecoder(bytes.NewReader(data))); err != nil {
		fail(w, Malformed)
		return false
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err = d.Decode(out); err != nil {
		fail(w, Malformed)
		return false
	}
	if err = d.Decode(new(any)); err != io.EOF {
		fail(w, Malformed)
		return false
	}
	return true
}
func uniqueJSON(d *json.Decoder) error {
	token, err := d.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	seen := map[string]bool{}
	for d.More() {
		if delim == '{' {
			key, err := d.Token()
			if err != nil {
				return err
			}
			// encoding/json matches struct fields case-insensitively.
			name := strings.ToLower(fmt.Sprint(key))
			if seen[name] {
				return fmt.Errorf("duplicate key")
			}
			seen[name] = true
		}
		if err := uniqueJSON(d); err != nil {
			return err
		}
	}
	_, err = d.Token()
	return err
}
