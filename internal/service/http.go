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
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/readview"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/tailnet"
	"github.com/kpenfound/osmia/internal/web"
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
		status, message = 501, "operation is unsupported"
	case RestartRequired:
		status, message = 409, "operation requires a service restart"
	case Unavailable:
		status, message = 503, "service is unavailable"
	case Forbidden:
		status, message = 403, "web and tailnet requests need a Host naming the listener and, except reads, Content-Type application/json"
	}
	respond(w, status, ErrorResponse{Error: APIError{Code: code, Message: message}})
}

// failWith reports a project operation error with the message the operation
// composed: field names, identities and paths the caller supplied, never raw
// file contents or parser output.
func failWith(w http.ResponseWriter, api *APIError) {
	status := http.StatusInternalServerError
	switch api.Code {
	case Validation:
		status = 422
	case NoProject, ProjectActive, CharterEmpty, Conflict:
		status = 409
	case NotFound:
		status = 404
	case Unsupported:
		status = 501
	case Unavailable:
		status = 503
	}
	respond(w, status, ErrorResponse{Error: *api})
}
func noProject(field string) Diagnostic {
	return Diagnostic{Field: field, Code: NoProject, Message: "no project is configured; add one with osmia project add"}
}
func digest(c *config.Config) string {
	b, _ := json.Marshal(c)
	return fmt.Sprintf("%x", sha256.Sum256(b))
}
func (s *Service) configuration() ConfigResponse {
	s.mu.Lock()
	cfg, pending, reloadErr := s.cfg, s.pending, s.reloadErr
	s.mu.Unlock()
	out := ConfigResponse{Root: cfg.Root.String(), Digest: digest(cfg), Effective: cfg, Diagnostics: []Diagnostic{}, LastError: reloadErr}
	// Each project has its own charter and knowledge-base extraction.
	extractions := map[config.ProjectID]*ExtractionState{}
	for _, p := range cfg.Projects {
		directory, _ := cfg.Root.ProjectTrace(p.ID)
		extraction, err := s.projectExtraction(p.ID)
		if err == nil {
			extractions[p.ID] = extraction
		} else if !errors.Is(err, errNoTrace) && !errors.Is(err, errNoActiveProject) {
			out.Diagnostics = append(out.Diagnostics, Diagnostic{Field: "extraction", Code: Internal, Message: "cannot read the knowledge-base extraction state; check the trace repository at " + directory})
		}
	}
	for _, p := range cfg.Projects {
		view := projectView(cfg.Root, p)
		view.Repos[0].Extraction = extractions[p.ID]
		view.Extraction = extractions[p.ID]
		// A project without a trace, or removed since the snapshot, has no
		// charter to report.
		state, err := s.charterState(p.ID)
		if err == nil {
			view.CharterState = &state
		} else if !errors.Is(err, errNoTrace) && !errors.Is(err, errNoActiveProject) {
			out.Diagnostics = append(out.Diagnostics, Diagnostic{Field: "charter", Code: Internal, Message: "cannot read the charter; check the trace at " + view.Trace + ", and write the charter with: osmia edit charter " + string(view.ID)})
		}
		out.Projects = append(out.Projects, view)
	}
	if len(out.Projects) == 1 {
		out.Project = &out.Projects[0]
	}
	if len(out.Projects) == 0 {
		out.Diagnostics = append(out.Diagnostics, noProject("active_projects"))
	}
	out.Diagnostics = append(out.Diagnostics, s.notifier.diagnostics()...)
	if pending != nil {
		out.Diagnostics = append(out.Diagnostics, Diagnostic{Field: "projects", Code: Internal, Message: "an interrupted project registration is incomplete; run osmia project add again to finish it, or inspect project-add.json under the root"})
	}
	next, restart, err := s.candidate(cfg)
	out.Drift = s.configDrift(cfg, err)
	if err != nil {
		out.Diagnostics = append(out.Diagnostics, Diagnostic{Field: "configuration", Code: Validation, Message: "disk configuration is invalid (" + reloadError(err, time.Time{}).Message + "); loaded configuration retained"})
		return out
	}
	if digest(next) != out.Digest {
		out.Diagnostics = append(out.Diagnostics, Diagnostic{Field: "configuration", Code: ReloadRequired, Message: "disk configuration differs; run osmia reload to apply it"})
	}
	if len(restart) > 0 {
		out.Diagnostics = append(out.Diagnostics, Diagnostic{Field: "configuration", Code: RestartRequired, Message: "disk configuration changes " + strings.Join(restart, ", ") + "; restart the service to apply it"})
	}
	return out
}
func (s *Service) runtimeView() RuntimeResponse {
	state, ds := s.effective()
	cfg := s.current()
	out := RuntimeResponse{Effective: state, Profiles: s.effectiveProfiles(state), Projects: []ProjectRuntime{}, Jev: s.jev.Status(), Diagnostics: []Diagnostic{}}
	for _, id := range cfg.ProjectIDs() {
		out.Projects = append(out.Projects, ProjectRuntime{Project: id, ContextMode: s.Context().Mode(id)})
	}
	if len(out.Projects) == 0 {
		out.Diagnostics = append(out.Diagnostics, noProject("project"))
	}
	for _, d := range ds {
		field := d.Field
		if strings.HasPrefix(field, "profiles.") {
			field = "profiles"
		}
		out.Diagnostics = append(out.Diagnostics, Diagnostic{Field: field, Code: Validation, Message: "stored reference is unavailable; excluded from effective state"})
	}
	if err := s.store.CheckDisk(); err != nil {
		code := Internal
		if errors.Is(err, runtime.ErrConflict) {
			code = Conflict
		}
		out.Diagnostics = append(out.Diagnostics, Diagnostic{Field: "runtime", Code: code, Message: "disk runtime differs or is unreadable; acknowledged state retained"})
	}
	return out
}

func (s *Service) effectiveProfiles(state runtime.State) map[string]EffectiveProfile {
	stored, _ := s.store.Snapshot()
	profiles := make(map[string]EffectiveProfile)
	cfg := s.current()
	for role, binding := range cfg.Roles {
		name, source := binding.Profile, "configuration"
		if override := stored.Profiles[role]; override != "" && state.Profiles[role] == override {
			name, source = override, "owner_override"
		} else if state.Profiles[role] != binding.Profile {
			name, source = state.Profiles[role], "provider_fallback"
		}
		reason := ""
		if source == "provider_fallback" {
			for _, limit := range state.ProviderLimits {
				if limit.Backend == cfg.Profiles[binding.Profile].Agent {
					reason = "Provider " + limit.Backend + " usage limit (" + limit.Status + ")"
					break
				}
			}
		}
		for _, p := range state.Pauses {
			if p.Target.Scope == "role" && p.Target.Role == role {
				name, source, reason = "", "provider_pause", p.Reason
			}
		}
		profiles[role] = EffectiveProfile{Name: name, Source: source, Reason: reason}
	}
	return profiles
}

// sessionCheckWriteMargin pads the write deadline a session check's HTTP
// response gets beyond the project's own checks_timeout, the budget the
// check itself is bounded by, so recording the result after the check
// finishes is never cut off by the deadline that bounds the check.
const sessionCheckWriteMargin = time.Minute

func (s *Service) handle(w http.ResponseWriter, r *http.Request) {
	if s.engineHTTP(w, r) {
		return
	}
	if web.Serve(w, r) {
		return
	}
	if path, ok := strings.CutPrefix(r.URL.Path, Prefix+"/workgroups/"); ok {
		id, action, valid := strings.Cut(path, "/")
		if valid && action == "feed" && r.Method == http.MethodGet {
			if out, api := s.workgroupFeed(r.Context(), id); api != nil {
				failWith(w, api)
			} else {
				respond(w, 200, out)
			}
			return
		}
		if valid && action == "conversation" && r.Method == http.MethodPost {
			var req SendRequest
			if !decode(w, r, &req) {
				return
			}
			if out, api := s.workgroupSend(r.Context(), id, req); api != nil {
				failWith(w, api)
			} else {
				respond(w, 200, out)
			}
			return
		}
	}
	if r.Method == http.MethodPost && r.URL.Path == Prefix+"/stop" {
		if tailnet.ArrivedOn(r) != 0 {
			fail(w, Forbidden)
			return
		}
		respond(w, http.StatusAccepted, StopResponse{Stopping: true})
		s.cancel()
		return
	}
	if s.documentRequest(w, r) {
		return
	}
	if r.Method == http.MethodGet {
		switch r.URL.Path {
		case Prefix + "/health":
			respond(w, 200, s.healthView(r.Context()))
			return
		case Prefix + "/activity":
			respond(w, 200, s.activityView())
			return
		case Prefix + "/config":
			out := s.configuration()
			out.Tailnet = s.tailnetStatus(r.Context())
			respond(w, 200, out)
			return
		case Prefix + "/runtime":
			respond(w, 200, s.runtimeView())
			return
		case Prefix + "/status":
			// statusList assembles the workstream list from the trace and
			// the runtime store (status.go); a request whose view cannot
			// refresh within coreReadBound answers with the last one
			// instead of blocking past it (bounded_read.go, boundedView).
			out, stale := readview.Bounded(r.Context(), &s.statusViews, coreReadBound, s.statusList)
			out.Tailnet = s.tailnetStatus(r.Context())
			if stale {
				w.Header().Set(StaleHeader, "true")
			}
			respond(w, 200, out)
			return
		case Prefix + "/events":
			s.streamEvents(w, r)
			return
		case Prefix + "/inbox":
			if out, api := s.inbox(r.Context()); api != nil {
				failWith(w, api)
			} else {
				respond(w, 200, out)
			}
			return
		}
		if path, ok := strings.CutPrefix(r.URL.Path, Prefix+"/trace/"); ok {
			stream, kind, selector, valid := splitTracePath(path)
			if !valid {
				failWith(w, &APIError{Code: Validation, Message: "expected trace/<workstream>[/unit|criterion|commit/<selector>]"})
			} else if out, api := s.traceView(r.Context(), stream, kind, selector); api != nil {
				failWith(w, api)
			} else {
				respond(w, 200, out)
			}
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
		if id, ok := strings.CutPrefix(r.URL.Path, Prefix+"/feed/"); ok {
			if out, api := s.feedList(id); api != nil {
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
		if r.URL.Path == Prefix+"/design" {
			if out, api := s.designList(r.URL.Query().Get("project")); api != nil {
				failWith(w, api)
			} else {
				respond(w, 200, out)
			}
			return
		}
		if id, ok := strings.CutSuffix(r.URL.Path, "/conversation"); ok {
			if id, ok = strings.CutPrefix(id, Prefix+"/design/"); ok {
				if out, api := s.conversationList(id); api != nil {
					if _, err := config.ParseWorkstreamID(id); err == nil && api.Code == Validation {
						api = &APIError{Code: NotFound, Message: "design session not found"}
					}
					failWith(w, api)
				} else {
					respond(w, 200, out)
				}
				return
			}
		}
		if id, ok := strings.CutPrefix(r.URL.Path, Prefix+"/design/"); ok {
			if out, api := s.designRead(id); api != nil {
				failWith(w, api)
			} else {
				respond(w, 200, DesignResponse{Design: out})
			}
			return
		}
		if r.URL.Path == Prefix+"/sessions" {
			if out, api := s.sessionList(r.URL.Query().Get("project")); api != nil {
				failWith(w, api)
			} else {
				respond(w, 200, out)
			}
			return
		}
		if id, ok := strings.CutSuffix(r.URL.Path, "/changes"); ok {
			if id, ok = strings.CutPrefix(id, Prefix+"/sessions/"); ok {
				if out, api := s.sessionChanges(r.Context(), id); api != nil {
					failWith(w, api)
				} else {
					respond(w, 200, out)
				}
				return
			}
		}
		if id, ok := strings.CutPrefix(r.URL.Path, Prefix+"/sessions/"); ok {
			if out, api := s.sessionRead(id); api != nil {
				failWith(w, api)
			} else {
				respond(w, 200, SessionResponse{Session: out})
			}
			return
		}
	}
	if r.Method == http.MethodPost && r.URL.Path == Prefix+"/sessions" {
		var v SessionOpenRequest
		if !decode(w, r, &v) {
			return
		}
		out, api := s.openSession(r.Context(), v)
		if api != nil {
			failWith(w, api)
		} else {
			respond(w, 200, SessionResponse{Session: out})
		}
		return
	}
	if id, ok := strings.CutSuffix(r.URL.Path, "/message"); ok && r.Method == http.MethodPost {
		if id, ok = strings.CutPrefix(id, Prefix+"/sessions/"); ok {
			var v SessionMessageRequest
			if !decode(w, r, &v) {
				return
			}
			if out, api := s.sessionMessage(r.Context(), id, v); api != nil {
				failWith(w, api)
			} else {
				respond(w, 200, out)
			}
			return
		}
	}
	if id, ok := strings.CutSuffix(r.URL.Path, "/stop"); ok && r.Method == http.MethodPost {
		if id, ok = strings.CutPrefix(id, Prefix+"/sessions/"); ok {
			if out, api := s.sessionStop(r.Context(), id); api != nil {
				failWith(w, api)
			} else {
				respond(w, 200, SessionResponse{Session: out})
			}
			return
		}
	}

	if id, ok := strings.CutSuffix(r.URL.Path, "/destroy"); ok && r.Method == http.MethodPost {
		for _, route := range []struct {
			prefix string
			design bool
		}{{Prefix + "/sessions/", false}, {Prefix + "/design/", true}} {
			if raw, ok := strings.CutPrefix(id, route.prefix); ok {
				if out, api := s.destroySession(r.Context(), raw, route.design); api != nil {
					failWith(w, api)
				} else {
					respond(w, 200, out)
				}
				return
			}
		}
	}
	if id, ok := strings.CutSuffix(r.URL.Path, "/commit"); ok && r.Method == http.MethodPost {
		if id, ok = strings.CutPrefix(id, Prefix+"/sessions/"); ok {
			var v SessionCommitRequest
			if !decode(w, r, &v) {
				return
			}
			if out, api := s.sessionCommit(r.Context(), id, v); api != nil {
				failWith(w, api)
			} else {
				respond(w, 200, out)
			}
			return
		}
	}
	if id, ok := strings.CutSuffix(r.URL.Path, "/check"); ok && r.Method == http.MethodPost {
		if id, ok = strings.CutPrefix(id, Prefix+"/sessions/"); ok {
			// A session check runs the project's own check command inline
			// (session_controls.go, sessionCheck), which routinely outlives
			// the server's default WriteTimeout; extend the write deadline
			// to the project's own checks_timeout, the same budget the
			// check itself is bounded by, plus a margin for recording the
			// result, the same way the project-add route extends it to
			// addProjectTimeout.
			if _, _, repository, api := s.sessionTrace(id); api != nil {
				failWith(w, api)
				return
			} else if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(s.about(repository).Project.CheckTimeout() + sessionCheckWriteMargin)); err != nil {
				fail(w, Internal)
				return
			}
			if out, api := s.sessionCheck(r.Context(), id); api != nil {
				failWith(w, api)
			} else {
				respond(w, 200, out)
			}
			return
		}
	}
	if id, ok := strings.CutSuffix(r.URL.Path, "/push"); ok && r.Method == http.MethodPost {
		if id, ok = strings.CutPrefix(id, Prefix+"/sessions/"); ok {
			if out, api := s.sessionPush(r.Context(), id); api != nil {
				failWith(w, api)
			} else {
				respond(w, 200, out)
			}
			return
		}
	}
	if id, ok := strings.CutSuffix(r.URL.Path, "/end"); ok && r.Method == http.MethodPost {
		if id, ok = strings.CutPrefix(id, Prefix+"/sessions/"); ok {
			if out, api := s.sessionEnd(r.Context(), id); api != nil {
				failWith(w, api)
			} else {
				respond(w, 200, SessionResponse{Session: out})
			}
			return
		}
	}
	if r.Method == http.MethodPost && r.URL.Path == Prefix+"/design" {
		var v DesignOpenRequest
		if !decode(w, r, &v) {
			return
		}
		out, api := s.openDesign(r.Context(), v)
		if api != nil {
			failWith(w, api)
		} else {
			respond(w, 200, DesignResponse{Design: out})
		}
		return
	}
	if id, ok := strings.CutSuffix(r.URL.Path, "/message"); ok && r.Method == http.MethodPost {
		if id, ok = strings.CutPrefix(id, Prefix+"/design/"); ok {
			var v DesignMessageRequest
			if !decode(w, r, &v) {
				return
			}
			if out, api := s.designMessage(r.Context(), id, v); api != nil {
				failWith(w, api)
			} else {
				respond(w, 200, out)
			}
			return
		}
	}
	if id, ok := strings.CutSuffix(r.URL.Path, "/projects"); ok && r.Method == http.MethodPost {
		if id, ok = strings.CutPrefix(id, Prefix+"/design/"); ok {
			var v api.ProjectSelectionRequest
			if !decode(w, r, &v) {
				return
			}
			if out, failure := s.intake().ChooseDesignProjects(r.Context(), id, v.Projects); failure != nil {
				failWith(w, failure)
			} else {
				respond(w, 200, out)
			}
			return
		}
	}
	if id, ok := strings.CutSuffix(r.URL.Path, "/start"); ok && r.Method == http.MethodPost {
		if id, ok = strings.CutPrefix(id, Prefix+"/design/"); ok {
			var req DesignStartRequest
			if r.Body != nil && r.ContentLength != 0 {
				if !decode(w, r, &req) {
					return
				}
			}
			if out, api := s.startDesignWorkgroup(r.Context(), id, req); api != nil {
				failWith(w, api)
			} else {
				respond(w, 200, out)
			}
			return
		}
	}
	if r.Method == http.MethodPost && r.URL.Path == Prefix+"/reload" {
		if out, api := s.reload(); api != nil {
			failWith(w, api)
		} else {
			respond(w, 200, out)
		}
		return
	}
	if r.Method == http.MethodPut && (r.URL.Path == Prefix+"/config/root" || r.URL.Path == Prefix+"/config/listen") {
		fail(w, RestartRequired)
		return
	}
	if id, ok := strings.CutPrefix(r.URL.Path, Prefix+"/projects/preview/"); ok && (r.Method == http.MethodPut || r.Method == http.MethodDelete) {
		s.previewHTTP(w, r, id)
		return
	}
	if r.URL.Path == Prefix+"/projects" && (r.Method == http.MethodPost || r.Method == http.MethodDelete) {
		if r.Method == http.MethodPost {
			if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(addProjectTimeout)); err != nil {
				fail(w, Internal)
				return
			}
		}
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
	if r.Method == http.MethodPost && r.URL.Path == Prefix+"/projects/extract" {
		var v ProjectExtractRequest
		if !decode(w, r, &v) {
			return
		}
		result, api := s.extractProject(r.Context(), v)
		if api != nil {
			failWith(w, api)
			return
		}
		respond(w, 200, result)
		return
	}
	if r.Method == http.MethodPost && r.URL.Path == Prefix+"/projects/rebase" {
		var v ProjectRebaseRequest
		if !decode(w, r, &v) {
			return
		}
		if out, api := s.rebaseProject(r.Context(), v); api != nil {
			failWith(w, api)
		} else {
			respond(w, 200, out)
		}
		return
	}
	if id, ok := strings.CutPrefix(r.URL.Path, Prefix+"/rebase/"); ok {
		if r.Method != http.MethodPost {
			fail(w, Unsupported)
		} else if out, api := s.rebaseWorkstream(r.Context(), id); api != nil {
			failWith(w, api)
		} else {
			respond(w, 200, out)
		}
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
	if n, ok := strings.CutPrefix(r.URL.Path, Prefix+"/inbox/"); ok && r.Method == http.MethodPost {
		var v AnswerRequest
		if !decode(w, r, &v) {
			return
		}
		if out, api := s.answer(r.Context(), n, v); api != nil {
			failWith(w, api)
		} else {
			respond(w, 200, out)
		}
		return
	}
	if target, ok := strings.CutPrefix(r.URL.Path, Prefix+"/contested/"); ok && r.Method == http.MethodPost {
		stream, unit, found := strings.Cut(target, "/")
		if !found || unit == "" {
			failWith(w, &APIError{Code: Validation, Message: "a contested ruling requires a workstream and unit"})
			return
		}
		var v ContestedRulingRequest
		if !decode(w, r, &v) {
			return
		}
		if out, api := s.ruleContested(r.Context(), stream, unit, v); api != nil {
			failWith(w, api)
		} else {
			respond(w, 200, out)
		}
		return
	}
	if target, ok := strings.CutPrefix(r.URL.Path, Prefix+"/move/"); ok && r.Method == http.MethodPost {
		stream, unit, found := strings.Cut(target, "/")
		if !found {
			var v WorkstreamMoveRequest
			if !decode(w, r, &v) {
				return
			}
			if out, api := s.ownerMoveWorkstream(r.Context(), stream, v); api != nil {
				failWith(w, api)
			} else {
				respond(w, 200, out)
			}
			return
		}
		if unit == "" {
			failWith(w, &APIError{Code: Validation, Message: "a move requires a workstream and unit"})
			return
		}
		var v UnitMoveRequest
		if !decode(w, r, &v) {
			return
		}
		if out, api := s.ownerMove(r.Context(), stream, unit, v); api != nil {
			failWith(w, api)
		} else {
			respond(w, 200, out)
		}
		return
	}
	if r.URL.Path == Prefix+"/charter" && r.Method == http.MethodGet {
		if out, api := s.charterProposals(); api != nil {
			failWith(w, api)
		} else {
			respond(w, 200, out)
		}
		return
	}
	if target, ok := strings.CutPrefix(r.URL.Path, Prefix+"/charter/"); ok {
		stream, question, found := strings.Cut(target, "/")
		if !found || question == "" || strings.Contains(question, "/") {
			failWith(w, &APIError{Code: Validation, Message: "expected charter/<workstream>/<question>"})
			return
		}
		var (
			out CharterProposalView
			api *APIError
		)
		switch r.Method {
		case http.MethodGet:
			out, api = s.charterProposal(stream, question)
		case http.MethodPost:
			var v CharterDecisionRequest
			if !decode(w, r, &v) {
				return
			}
			out, api = s.decideCharter(r.Context(), stream, question, v)
		default:
			fail(w, Unsupported)
			return
		}
		if api != nil {
			failWith(w, api)
		} else {
			respond(w, 200, out)
		}
		return
	}
	if target, ok := strings.CutPrefix(r.URL.Path, Prefix+"/amendment/"); ok {
		stream, id, found := strings.Cut(target, "/")
		if !found || id == "" || strings.Contains(id, "/") {
			failWith(w, &APIError{Code: Validation, Message: "expected amendment/<workstream>/<amendment>"})
			return
		}
		var (
			out AmendmentResponse
			api *APIError
		)
		switch r.Method {
		case http.MethodGet:
			out, api = s.amendmentView(stream, id)
		case http.MethodPost:
			var v AmendmentDecisionRequest
			if !decode(w, r, &v) {
				return
			}
			out, api = s.decideAmendment(r.Context(), stream, id, v)
		default:
			fail(w, Unsupported)
			return
		}
		if api != nil {
			failWith(w, api)
		} else {
			respond(w, 200, out)
		}
		return
	}
	if rest, ok := strings.CutPrefix(r.URL.Path, Prefix+"/shed/"); ok && r.Method == http.MethodPost {
		action, id, _ := strings.Cut(rest, "/")
		var (
			out ShedResponse
			api *APIError
		)
		switch action {
		case "object":
			var v ShedObjectRequest
			if !decode(w, r, &v) {
				return
			}
			out, api = s.shedObject(r.Context(), id, v)
		case "rule":
			var v ShedRuleRequest
			if !decode(w, r, &v) {
				return
			}
			out, api = s.shedRule(r.Context(), id, v)
		case "skip":
			out, api = s.shedSkip(r.Context(), id)
		case "overrule":
			var v ShedOverruleRequest
			if !decode(w, r, &v) {
				return
			}
			out, api = s.shedOverrule(r.Context(), id, v)
		case "more":
			var v ShedMoreRequest
			if !decode(w, r, &v) {
				return
			}
			out, api = s.shedMore(r.Context(), id, v)
		case "redraft":
			var v ShedRedraftRequest
			if !decode(w, r, &v) {
				return
			}
			out, api = s.shedRedraft(r.Context(), id, v)
		default:
			fail(w, Unsupported)
			return
		}
		if api != nil {
			failWith(w, api)
		} else {
			respond(w, 200, out)
		}
		return
	}
	if id, ok := strings.CutPrefix(r.URL.Path, Prefix+"/packet/"); ok && r.Method == http.MethodGet {
		if out, api := s.packet(id); api != nil {
			failWith(w, api)
		} else {
			respond(w, 200, out)
		}
		return
	}
	if id, ok := strings.CutPrefix(r.URL.Path, Prefix+"/ratify/"); ok && r.Method == http.MethodPost {
		var v RatifyRequest
		if !decode(w, r, &v) {
			return
		}
		if out, api := s.ratify(r.Context(), id, v); api != nil {
			failWith(w, api)
		} else {
			respond(w, 200, out)
		}
		return
	}
	if id, ok := strings.CutPrefix(r.URL.Path, Prefix+"/delivery/"); ok {
		if id, ok := strings.CutSuffix(id, "/merged"); ok {
			if r.Method != http.MethodPost {
				fail(w, Unsupported)
			} else if out, api := s.recordMerge(r.Context(), id); api != nil {
				failWith(w, api)
			} else {
				respond(w, 200, out)
			}
			return
		}
		switch r.Method {
		case http.MethodGet:
			if out, api := s.deliveryPresentation(r.Context(), id); api != nil {
				failWith(w, api)
			} else {
				respond(w, 200, out)
			}
		case http.MethodPost:
			var v DeliveryDecision
			if !decode(w, r, &v) {
				return
			}
			if out, api := s.approveDelivery(r.Context(), id, v); api != nil {
				failWith(w, api)
			} else {
				respond(w, 200, out)
			}
		default:
			fail(w, Unsupported)
		}
		return
	}
	if id, ok := strings.CutPrefix(r.URL.Path, Prefix+"/archive/"); ok && (r.Method == http.MethodPost || r.Method == http.MethodDelete) {
		out, api := s.archive(id)
		if r.Method == http.MethodDelete {
			out, api = s.unarchive(id)
		}
		if api != nil {
			failWith(w, api)
		} else {
			respond(w, 200, out)
		}
		return
	}
	if id, ok := strings.CutPrefix(r.URL.Path, Prefix+"/abandon/"); ok && r.Method == http.MethodPost {
		var v AbandonRequest
		if !decode(w, r, &v) {
			return
		}
		if out, api := s.abandon(r.Context(), id, v); api != nil {
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
		if out, api := s.handIn(r.Context(), v); api != nil {
			failWith(w, api)
		} else {
			respond(w, 200, out)
		}
		return
	}
	var err error
	switch {
	case r.Method == http.MethodPut && r.URL.Path == Prefix+"/runtime/pause":
		var v PauseRequest
		if !decode(w, r, &v) {
			return
		}
		if v.Source != "" && v.Source != runtime.PauseOwner {
			fail(w, Validation)
			return
		}
		v.Source = runtime.PauseOwner
		v.SetAt = s.now()
		if strings.TrimSpace(v.Reason) == "" {
			v.Reason = "Owner requested pause"
		}
		err = s.setPause(v)
	case r.Method == http.MethodDelete && r.URL.Path == Prefix+"/runtime/pause":
		var v ClearPauseRequest
		if !decode(w, r, &v) {
			return
		}
		err = s.clearPause(v)
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
	case r.Method == http.MethodDelete && r.URL.Path == Prefix+"/runtime/provider-limit":
		var v ClearProviderLimitRequest
		if !decode(w, r, &v) {
			return
		}
		err = s.store.ClearProviderLimit(v.Backend)
	default:
		if !routePathExists(r.URL.Path) {
			failWith(w, &APIError{Code: NotFound, Message: "route not found"})
		} else {
			fail(w, Unsupported)
		}
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
	respond(w, 200, MutationResponse{Applied: true})
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

// routePathExists distinguishes an unsupported method on a known endpoint
// from a path that has no API route.
func routePathExists(path string) bool {
	actual := strings.Split(path, "/")
	for _, route := range Routes {
		parts := strings.Split(route.Path, "/")
		if len(parts) != len(actual) {
			continue
		}
		matches := true
		for i, part := range parts {
			if strings.HasPrefix(part, "{") && strings.HasSuffix(part, "}") && actual[i] != "" {
				continue
			}
			if part != actual[i] {
				matches = false
				break
			}
		}
		if matches {
			return true
		}
	}
	return false
}
