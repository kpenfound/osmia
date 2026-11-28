package intake

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/ghlog"
	"github.com/kpenfound/osmia/internal/issues"
	"github.com/kpenfound/osmia/internal/kinds"
	"github.com/kpenfound/osmia/internal/trace"
)

// MaxHandedBytes bounds a handed input.
const MaxHandedBytes = 512 << 10

const (
	HandedDocument   = "handed"
	HandInTransition = "handin"
	// HandInSkipReason is why a hand-in that skipped debate recorded its
	// skip; the ratification packet carries it as its conclusion.
	HandInSkipReason = "the owner skipped debate at hand-in; the workstream still needs the owner's ratification of the spec and the plan"
	// HandedState is the feature state of a newly handed workstream.
	HandedState = "handed"
)

var HandInKey = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

// HandInWorkstream is the workstream a hand-in with the given key creates in
// project.
func HandInWorkstream(project config.ProjectID, key string) config.WorkstreamID {
	data, _ := json.Marshal([]string{"handin", string(project), key})
	sum := sha256.Sum256(data)
	return config.WorkstreamID("w_" + hex.EncodeToString(sum[:16]))
}

// HandInOrigin says a hand-in is made on the owner's behalf by someone else:
// the actor its records name in place of the owner, and the reason its
// hand-in transition records. The workstream it creates is an ordinary
// hand-in in every other respect.
type HandInOrigin struct {
	Actor trace.Actor
	// Reason is what the hand-in transition records. It ends with the path of
	// the handed document, which the service appends.
	Reason string
}

// HandIn checks that the project is active and its charter has rules, then
// copies the input into a new workstream and moves it to the handed state. The
// project may be left out while exactly one project is active.
func (s *Controller) HandIn(ctx context.Context, req HandInRequest) (HandInResponse, *APIError) {
	return s.HandInAs(ctx, req, nil)
}

// HandInAs is HandIn made for origin instead of the owner. A nil origin is
// the owner's own hand-in.
func (s *Controller) HandInAs(ctx context.Context, req HandInRequest, origin *HandInOrigin) (HandInResponse, *APIError) {
	projects := req.Projects
	if projects == nil {
		project, api := s.ProjectFor(req.Project)
		if api != nil {
			return HandInResponse{}, api
		}
		projects = []config.ProjectID{project}
	} else if req.Project != "" && (len(projects) == 0 || req.Project != projects[0]) {
		return HandInResponse{}, &APIError{Code: Validation, Message: "project must match the first entry in projects"}
	}
	if api := s.ValidateProjects(ctx, projects); api != nil {
		return HandInResponse{}, api
	}
	req.Project = projects[0]
	kind, err := kinds.Normalize(req.Kind)
	if err != nil {
		return HandInResponse{}, &APIError{Code: Validation, Message: err.Error()}
	}
	req.Kind = kind
	if api := checkHandInKind(kind, len(projects), req.Base); api != nil {
		return HandInResponse{}, api
	}
	cfg := s.Current()
	baseProject := req.Project
	if len(projects) > 1 {
		selected, api := s.SelectionBase(projects, req.Base)
		if api != nil {
			return HandInResponse{}, api
		}
		if req.Base != "" {
			baseProject = selected
		}
	}
	if req.Base != "" {
		if api := s.CheckBaseKind(baseProject, req.Base); api != nil {
			return HandInResponse{}, api
		}
	}
	if !HandInKey.MatchString(req.Key) {
		return HandInResponse{}, &APIError{Code: Validation, Message: "key must be 1 to 128 letters, digits, '_' or '-', starting with a letter or digit"}
	}
	forms := 0
	for _, set := range []bool{req.Path != "", req.URL != "", req.Stdin != nil} {
		if set {
			forms++
		}
	}
	if forms != 1 {
		return HandInResponse{}, &APIError{Code: Validation, Message: "hand in exactly one input: a file path, an issue URL or stdin"}
	}
	source, name, api := HandInSource(req)
	if api != nil {
		return HandInResponse{}, api
	}

	repository, err := s.Repository(req.Project)
	if errors.Is(err, ErrNoTrace) {
		return HandInResponse{}, &APIError{Code: Internal, Message: fmt.Sprintf("project %s is configured but has no trace repository; register it with osmia project add", req.Project)}
	}
	if err != nil {
		return HandInResponse{}, &APIError{Code: NotFound, Message: fmt.Sprintf("project %s is not an active project; check the project ID with osmia status", req.Project)}
	}
	stream := HandInWorkstream(req.Project, req.Key)
	if len(projects) > 1 && req.Base != "" {
		if req.Base == stream {
			return HandInResponse{}, &APIError{Code: Validation, Message: "base must name another feature workstream"}
		}
		for _, project := range projects {
			if req.Base == HandInWorkstream(project, "design-"+string(stream)) {
				return HandInResponse{}, &APIError{Code: Validation, Message: "base cannot name a member of this hand-in"}
			}
		}
	}
	workstreamTrace, err := cfg.Root.WorkstreamTrace(req.Project, stream)
	if err != nil {
		return HandInResponse{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot resolve the trace repository of workstream %s", stream)}
	}
	h := HandIn{S: s.HandInDependencies(), Repository: repository, Req: req, Stream: stream, Source: source, Name: name, Trace: workstreamTrace, Origin: origin}
	if out, done, api := h.Record(ctx, nil); done || api != nil {
		return out, api
	}
	content, api := s.ReadHanded(ctx, req)
	if api != nil {
		return HandInResponse{}, api
	}
	out, _, api := h.Record(ctx, &content)
	return out, api
}

type HandIn struct {
	S            *HandInDependencies
	Repository   *trace.Repository
	Req          HandInRequest
	Stream       config.WorkstreamID
	Source, Name string
	Trace        string
	// Origin is who handed the work in when it is not the owner.
	Origin *HandInOrigin
}

func (h HandIn) actor() trace.Actor {
	if h.Origin != nil {
		return h.Origin.Actor
	}
	return OwnerActor
}

func (h HandIn) Failed(step string) (HandInResponse, bool, *APIError) {
	return HandInResponse{}, false, &APIError{Code: Internal, Message: fmt.Sprintf("hand-in to project %s failed while %s workstream %s; retry with the same key, or check %s", h.Req.Project, step, h.Stream, h.Trace)}
}

// Record finishes the hand-in under the hand-in lock. With the input already
// copied it checks the copy matches the request and records the transition.
// Otherwise, with content nil it reports not done; with content it creates
// the workstream, copies content and records the transition.
func (h HandIn) Record(ctx context.Context, content *string) (HandInResponse, bool, *APIError) {
	h.S.HandInMu.Lock()
	defer h.S.HandInMu.Unlock()
	repository, req, stream := h.Repository, h.Req, h.Stream
	streams, err := repository.Workstreams()
	if err != nil {
		return h.Failed("reading")
	}
	exists := slices.Contains(streams, stream)
	if _, archived := h.S.ArchivedWorkstream(string(stream)); archived {
		return HandInResponse{}, false, &APIError{Code: Conflict, Message: "workstream is permanently archived; use a new hand-in key"}
	}
	_, archivedBase := h.S.ArchivedWorkstream(string(req.Base))
	if len(req.Projects) < 2 && req.Base != "" && (archivedBase || req.Base == stream || !slices.Contains(streams, req.Base)) {
		return HandInResponse{}, false, &APIError{Code: Validation, Message: "base must name another feature workstream in the same project"}
	}
	var selected DesignSelection
	var selectionFound bool
	if exists {
		selected, selectionFound, err = DesignSelectionOf(repository, stream)
		if err != nil {
			return h.Failed("reading project selection of")
		}
		stored, err := repository.Kind(stream)
		if err != nil {
			return h.Failed("reading the kind of")
		}
		if stored != req.Kind {
			return HandInResponse{}, false, &APIError{Code: Conflict, Message: fmt.Sprintf("the hand-in key already names a %s workstream; use a new key to hand in a %s one", stored, req.Kind)}
		}
	}
	projects := req.Projects
	if projects == nil {
		projects = []config.ProjectID{req.Project}
	}
	if selectionFound {
		prior := []config.ProjectID{}
		for _, m := range selected.Members {
			prior = append(prior, m.Repo)
		}
		if !slices.Equal(prior, projects) || selected.Base != req.Base {
			return HandInResponse{}, false, &APIError{Code: Conflict, Message: "the hand-in key already names a different project selection or base"}
		}
	}
	var doc *trace.Document
	var handed, skipped bool
	if exists {
		docs, err := trace.Read[trace.Document](repository, stream)
		if err != nil {
			return h.Failed("reading")
		}
		var initial trace.WorkstreamBase
		for _, d := range docs {
			if d.ID == "workstream-base" {
				if err := json.Unmarshal([]byte(d.Content), &initial); err != nil {
					return h.Failed("reading the original base of")
				}
			}
			if d.ID == HandedDocument {
				doc = &d
				break
			}
		}
		if (doc != nil || initial.Revision > 0) && len(req.Projects) < 2 && initial.Base != req.Base {
			return HandInResponse{}, false, &APIError{Code: Conflict, Message: "the hand-in key already names a different base"}
		}
		transitions, err := trace.Read[trace.Transition](repository, stream)
		if err != nil {
			return h.Failed("reading")
		}
		handed = slices.ContainsFunc(transitions, func(t trace.Transition) bool { return t.ID == HandInTransition })
		skipped = slices.ContainsFunc(transitions, SkippedAtHandIn)
	}
	if doc != nil && len(projects) > 1 && !selectionFound {
		return HandInResponse{}, false, &APIError{Code: Conflict, Message: "the hand-in key already names a single-project hand-in"}
	}
	if doc != nil && (doc.Source != h.Source || req.Stdin != nil && doc.Content != *req.Stdin) {
		return HandInResponse{}, false, &APIError{Code: Conflict, Message: fmt.Sprintf("key %s already handed in other input as workstream %s; use a new key", req.Key, stream)}
	}
	if (handed || skipped) && skipped != req.SkipDebate {
		did := "without skipping debate"
		if skipped {
			did = "skipping debate"
		}
		return HandInResponse{}, false, &APIError{Code: Conflict, Message: fmt.Sprintf("key %s already handed in workstream %s %s; use a new key", req.Key, stream, did)}
	}
	if doc == nil && content == nil {
		return HandInResponse{}, false, nil
	}
	if doc == nil {
		now := time.Now().UTC()
		if h.S.Now != nil {
			now = h.S.Now().UTC()
		}
		if exists {
			_, err = repository.EnsureManager(ctx, stream, now, h.actor())
		} else {
			backend, checkErr := h.S.NewWorkspaces(ctx, h.S.About(h.Repository))
			if checkErr != nil {
				return HandInResponse{}, false, &APIError{Code: Unavailable, Message: fmt.Sprintf("workstream %s cannot start: %v", stream, checkErr)}
			}
			err = repository.CreateWorkstreamKind(ctx, stream, backend.Backend, req.Kind, now, h.actor())
		}
		if err != nil {
			return h.Failed("creating")
		}
		if _, err := trace.EnsureWorkgroups(ctx, repository, []config.WorkstreamID{stream}, now, h.actor(), HandInTransition); err != nil {
			return h.Failed("recording the workgroup of")
		}
		if req.Base != "" && len(projects) == 1 {
			recorded, err := repository.WorkstreamBase(stream)
			if err != nil {
				return h.Failed("reading base of")
			}
			if _, err := repository.SetWorkstreamBase(ctx, stream, req.Base, recorded.Revision, now); err != nil {
				return HandInResponse{}, false, &APIError{Code: Validation, Message: "base must form an acyclic graph of available workstreams in this project"}
			}
		}
		if err := h.S.RefreshRuntime(); err != nil {
			return h.Failed("resolving the runtime workstreams of")
		}
		if len(projects) > 1 {
			if api := h.S.RecordOwnerProjects(ctx, repository, stream, projects, req.Base); api != nil {
				return HandInResponse{}, false, api
			}
		}
		doc = &trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: HandedDocument, Revision: 1, Project: req.Project, Workstream: stream, At: now, Actor: h.actor(), Cause: HandInTransition},
			Path: "handed/" + h.Name, Content: *content, Source: h.Source}
		if err := repository.Append(ctx, *doc); err != nil {
			return h.Failed("copying the input into")
		}
		if err := RecordFactoryPolicy(ctx, repository, stream, now, false); err != nil {
			return h.Failed("recording factory authority for")
		}
	}
	if req.SkipDebate && !skipped {
		tx := trace.Transaction{Transition: trace.Transition{Header: OwnerHeader(SkipTransition, req.Project, stream, HandInTransition, doc.At), Subject: OwnerSubject, To: SkippedValue, Reason: HandInSkipReason}}
		if _, err := repository.Transact(ctx, tx); err != nil {
			return h.Failed("recording the skipped debate of")
		}
	}
	header := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: HandInTransition, Revision: 1, Project: req.Project, Workstream: stream, At: doc.At, Actor: h.actor(), Cause: HandInTransition}
	reason := "the owner handed in " + doc.Path + " from " + h.Source
	if h.Origin != nil {
		reason = h.Origin.Reason + " (" + doc.Path + ")"
	}
	if req.SkipDebate {
		reason += " and skipped debate"
	}
	state, err := repository.SetFeatureState(ctx, header, HandedState, reason)
	if err != nil {
		return h.Failed("recording the handed state of")
	}
	return HandInResponse{Project: req.Project, Workstream: stream, State: state.Value, Source: h.Source, SkipDebate: req.SkipDebate}, true, nil
}

// checkHandInKind checks a hand-in against the flags of its kind, refusing with
// a plain reason.
func checkHandInKind(name string, projects int, base config.WorkstreamID) *APIError {
	kind, _ := kinds.Lookup(name)
	if err := kind.CheckBrief(projects, false); err != nil {
		return &APIError{Code: Validation, Message: err.Error()}
	}
	if base != "" {
		if err := kind.CheckDependencies(); err != nil {
			return &APIError{Code: Validation, Message: err.Error()}
		}
	}
	return nil
}

// HandInSource returns the recorded source of the request's input and the
// name of its copy under handed/.
func HandInSource(req HandInRequest) (source, name string, api *APIError) {
	switch {
	case req.Path != "":
		if !filepath.IsAbs(req.Path) || filepath.Clean(req.Path) != req.Path || strings.ContainsAny(req.Path, "\x00\r\n") {
			return "", "", &APIError{Code: Validation, Message: "path must be a clean absolute file path"}
		}
		name = filepath.Base(req.Path)
		if !ValidHandedName(name) {
			name = "input"
		}
		return "file:" + req.Path, name, nil
	case req.URL != "":
		ref, err := issues.Parse(req.URL)
		if err != nil {
			return "", "", &APIError{Code: Validation, Message: err.Error()}
		}
		return ref.URL(), fmt.Sprintf("issue-%d.md", ref.Number), nil
	default:
		return "stdin", "stdin", nil
	}
}

// ValidHandedName reports whether name is accepted as a file name under
// handed/ by the trace.
func ValidHandedName(name string) bool {
	return name != "" && path.Clean(name) == name && !strings.HasPrefix(name, ".") && strings.TrimSpace(name) == name &&
		!strings.ContainsAny(name, "/\\\x00\r\n:")
}

// ReadHanded reads the request's input: the file, the fetched issue or the
// stdin text.
func (s *Controller) ReadHanded(ctx context.Context, req HandInRequest) (string, *APIError) {
	ctx = ghlog.WithScope(ctx, ghlog.Scope{Project: string(req.Project)})
	var content string
	switch {
	case req.Path != "":
		info, err := os.Stat(req.Path)
		if err != nil {
			return "", &APIError{Code: Validation, Message: fmt.Sprintf("cannot read %s; check that the file exists and is readable", req.Path)}
		}
		if !info.Mode().IsRegular() {
			return "", &APIError{Code: Validation, Message: fmt.Sprintf("%s is not a regular file", req.Path)}
		}
		f, err := os.OpenFile(req.Path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			return "", &APIError{Code: Validation, Message: fmt.Sprintf("cannot read %s; check that the file exists and is readable", req.Path)}
		}
		defer f.Close()
		info, err = f.Stat()
		if err != nil || !info.Mode().IsRegular() {
			return "", &APIError{Code: Validation, Message: fmt.Sprintf("%s is not a regular file", req.Path)}
		}
		data, err := io.ReadAll(io.LimitReader(f, MaxHandedBytes+1))
		if err != nil {
			return "", &APIError{Code: Validation, Message: fmt.Sprintf("cannot read %s; check that the file exists and is readable", req.Path)}
		}
		content = string(data)
	case req.URL != "":
		ref, _ := issues.Parse(req.URL)
		text, err := s.Options.Issues.Fetch(ctx, ref)
		if err != nil {
			return "", &APIError{Code: Internal, Message: fmt.Sprintf("cannot fetch %s; check the URL and the service's GitHub access", ref.URL())}
		}
		content = text
	default:
		content = *req.Stdin
	}
	if len(content) > MaxHandedBytes {
		return "", &APIError{Code: Validation, Message: fmt.Sprintf("handed input is larger than %d bytes", MaxHandedBytes)}
	}
	if strings.TrimSpace(content) == "" {
		return "", &APIError{Code: Validation, Message: "handed input is empty"}
	}
	if !utf8.ValidString(content) {
		return "", &APIError{Code: Validation, Message: "handed input must be UTF-8 text"}
	}
	return content, nil
}
