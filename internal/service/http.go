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
func digest(c *config.Config) string {
	b, _ := json.Marshal(c)
	return fmt.Sprintf("%x", sha256.Sum256(b))
}
func (s *Service) configuration() ConfigResponse {
	out := ConfigResponse{Root: s.cfg.Root.String(), Digest: digest(s.cfg), Effective: s.cfg, Diagnostics: []Diagnostic{}}
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
	out := RuntimeResponse{Effective: state, Diagnostics: []Diagnostic{}}
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
		}
	}
	if r.Method == http.MethodPut && (r.URL.Path == Prefix+"/config/root" || r.URL.Path == Prefix+"/config/listen") {
		fail(w, RestartRequired)
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
