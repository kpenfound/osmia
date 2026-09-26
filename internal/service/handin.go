package service

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
	"github.com/kpenfound/osmia/internal/issues"
	"github.com/kpenfound/osmia/internal/trace"
)

// MaxHandedBytes bounds a handed input.
const MaxHandedBytes = 512 << 10

const (
	handedDocument   = "handed"
	handInTransition = "handin"
	// handInSkipReason is why a hand-in that skipped debate recorded its
	// skip; the ratification packet carries it as its conclusion.
	handInSkipReason = "the owner skipped debate at hand-in; the workstream still needs the owner's ratification of the spec and the plan"
	// HandedState is the feature state of a newly handed workstream.
	HandedState = "handed"
)

var handInKey = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

// HandInWorkstream is the workstream a hand-in with the given key creates in
// project.
func HandInWorkstream(project config.ProjectID, key string) config.WorkstreamID {
	data, _ := json.Marshal([]string{"handin", string(project), key})
	sum := sha256.Sum256(data)
	return config.WorkstreamID("w_" + hex.EncodeToString(sum[:16]))
}

// handIn checks that the project is active and its charter has rules, then
// copies the input into a new workstream and moves it to the handed state.
func (s *Service) handIn(ctx context.Context, req HandInRequest) (HandInResponse, *APIError) {
	if err := config.CheckProjectIDs(req.Project); err != nil {
		return HandInResponse{}, &APIError{Validation, "project must be a project ID: p_ followed by 32 lowercase hexadecimal digits"}
	}
	cfg := s.current()
	_, c, err := s.loadCharter(ctx, req.Project)
	if errors.Is(err, errNoActiveProject) {
		return HandInResponse{}, &APIError{NotFound, fmt.Sprintf("project %s is not an active project; check the project ID with osmia status", req.Project)}
	}
	view := projectView(cfg.Root, config.Project{ID: req.Project})
	if errors.Is(err, errNoTrace) {
		return HandInResponse{}, &APIError{Internal, fmt.Sprintf("project %s is configured but has no trace repository; register it with osmia project add", req.Project)}
	}
	if err != nil {
		return HandInResponse{}, &APIError{Internal, fmt.Sprintf("cannot read or record the charter of project %s; check %s and the trace repository", req.Project, view.Charter)}
	}
	if c.Empty() {
		return HandInResponse{}, &APIError{CharterEmpty, fmt.Sprintf("project %s cannot take work: its charter has no rules; write numbered rules (\"1. ...\") in %s", req.Project, view.Charter)}
	}
	if !handInKey.MatchString(req.Key) {
		return HandInResponse{}, &APIError{Validation, "key must be 1 to 128 letters, digits, '_' or '-', starting with a letter or digit"}
	}
	forms := 0
	for _, set := range []bool{req.Path != "", req.URL != "", req.Stdin != nil} {
		if set {
			forms++
		}
	}
	if forms != 1 {
		return HandInResponse{}, &APIError{Validation, "hand in exactly one input: a file path, an issue URL or stdin"}
	}
	source, name, api := handInSource(req)
	if api != nil {
		return HandInResponse{}, api
	}

	repository, err := s.repository(req.Project)
	if errors.Is(err, errNoTrace) {
		return HandInResponse{}, &APIError{Internal, fmt.Sprintf("project %s is configured but has no trace repository; register it with osmia project add", req.Project)}
	}
	if err != nil {
		return HandInResponse{}, &APIError{NotFound, fmt.Sprintf("project %s is not an active project; check the project ID with osmia status", req.Project)}
	}
	h := handIn{s: s, repository: repository, req: req, stream: HandInWorkstream(req.Project, req.Key), source: source, name: name, trace: view.Trace}
	// A key whose input is already copied finishes without reading it again.
	if out, done, api := h.record(ctx, nil); done || api != nil {
		return out, api
	}
	// The input is read without holding the hand-in lock, so a slow fetch does
	// not hold up other hand-ins.
	content, api := s.readHanded(ctx, req)
	if api != nil {
		return HandInResponse{}, api
	}
	out, _, api := h.record(ctx, &content)
	return out, api
}

type handIn struct {
	s            *Service
	repository   *trace.Repository
	req          HandInRequest
	stream       config.WorkstreamID
	source, name string
	trace        string
}

func (h handIn) failed(step string) (HandInResponse, bool, *APIError) {
	return HandInResponse{}, false, &APIError{Internal, fmt.Sprintf("hand-in to project %s failed while %s workstream %s; retry with the same key, or check %s", h.req.Project, step, h.stream, h.trace)}
}

// record finishes the hand-in under the hand-in lock. With the input already
// copied it checks the copy matches the request and records the transition.
// Otherwise, with content nil it reports not done; with content it creates
// the workstream, copies content and records the transition.
func (h handIn) record(ctx context.Context, content *string) (HandInResponse, bool, *APIError) {
	h.s.handInMu.Lock()
	defer h.s.handInMu.Unlock()
	repository, req, stream := h.repository, h.req, h.stream
	streams, err := repository.Workstreams()
	if err != nil {
		return h.failed("reading")
	}
	exists := slices.Contains(streams, stream)
	var doc *trace.Document
	var handed, skipped bool
	if exists {
		docs, err := trace.Read[trace.Document](repository, stream)
		if err != nil {
			return h.failed("reading")
		}
		for _, d := range docs {
			if d.ID == handedDocument {
				doc = &d
			}
		}
		transitions, err := trace.Read[trace.Transition](repository, stream)
		if err != nil {
			return h.failed("reading")
		}
		handed = slices.ContainsFunc(transitions, func(t trace.Transition) bool { return t.ID == handInTransition })
		// Only the skip the hand-in recorded counts: a later skip through the
		// shed is the owner's action on the workstream, not part of the request.
		skipped = slices.ContainsFunc(transitions, skippedAtHandIn)
	}
	if doc != nil && (doc.Source != h.source || req.Stdin != nil && doc.Content != *req.Stdin) {
		return HandInResponse{}, false, &APIError{Conflict, fmt.Sprintf("key %s already handed in other input as workstream %s; use a new key", req.Key, stream)}
	}
	// Whether debate is skipped is part of the request: a retry asks for what
	// the recorded hand-in did, and a hand-in interrupted before recording
	// its skip records it on the retry.
	if (handed || skipped) && skipped != req.SkipDebate {
		did := "without skipping debate"
		if skipped {
			did = "skipping debate"
		}
		return HandInResponse{}, false, &APIError{Conflict, fmt.Sprintf("key %s already handed in workstream %s %s; use a new key", req.Key, stream, did)}
	}
	if doc == nil && content == nil {
		return HandInResponse{}, false, nil
	}
	if doc == nil {
		now := time.Now().UTC()
		if exists {
			_, err = repository.EnsureChiefOfStaff(ctx, stream, now, ownerActor)
		} else {
			// A new workstream records the backend its workspaces use; it
			// keeps it until it is delivered or abandoned.
			backend, checkErr := h.s.newWorkspaces(ctx, h.s.current())
			if checkErr != nil {
				return HandInResponse{}, false, &APIError{Unavailable, fmt.Sprintf("workstream %s cannot start: %v", stream, checkErr)}
			}
			err = repository.CreateWorkstreamOn(ctx, stream, backend.Backend, now, ownerActor)
		}
		if err != nil {
			return h.failed("creating")
		}
		// The workstream may be new: the runtime store learns it before the
		// input is copied, so a retried hand-in resolves it again and a
		// priority or pause may name it as soon as this hand-in finishes.
		if err := h.s.resolveRuntime(h.s.current(), repository); err != nil {
			return h.failed("resolving the runtime workstreams of")
		}
		doc = &trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: handedDocument, Revision: 1, Project: req.Project, Workstream: stream, At: now, Actor: ownerActor, Cause: handInTransition},
			Path: "handed/" + h.name, Content: *content, Source: h.source}
		if err := repository.Append(ctx, *doc); err != nil {
			return h.failed("copying the input into")
		}
	}
	// The skip is recorded as the owner's skip of debate before the handed
	// state, so the debate controller never finds the workstream without it.
	// It carries the handed document's timestamp like the transition below.
	if req.SkipDebate && !skipped {
		tx := trace.Transaction{Transition: trace.Transition{Header: ownerHeader(skipTransition, req.Project, stream, handInTransition, doc.At), Subject: ownerSubject, To: skippedValue, Reason: handInSkipReason}}
		if _, err := repository.Transact(ctx, tx); err != nil {
			return h.failed("recording the skipped debate of")
		}
	}
	// The transition carries the handed document's timestamp, so a retry
	// repeats the committed transition exactly.
	header := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: handInTransition, Revision: 1, Project: req.Project, Workstream: stream, At: doc.At, Actor: ownerActor, Cause: handInTransition}
	reason := "the owner handed in " + doc.Path + " from " + h.source
	if req.SkipDebate {
		reason += " and skipped debate"
	}
	state, err := repository.SetFeatureState(ctx, header, HandedState, reason)
	if err != nil {
		return h.failed("recording the handed state of")
	}
	return HandInResponse{Project: req.Project, Workstream: stream, State: state.Value,
		Handed: filepath.Join(h.trace, "workstreams", string(stream), filepath.FromSlash(doc.Path)), Source: h.source, SkipDebate: req.SkipDebate}, true, nil
}

// handInSource returns the recorded source of the request's input and the
// name of its copy under handed/.
func handInSource(req HandInRequest) (source, name string, api *APIError) {
	switch {
	case req.Path != "":
		if !filepath.IsAbs(req.Path) || filepath.Clean(req.Path) != req.Path || strings.ContainsAny(req.Path, "\x00\r\n") {
			return "", "", &APIError{Validation, "path must be a clean absolute file path"}
		}
		name = filepath.Base(req.Path)
		if !validHandedName(name) {
			name = "input"
		}
		return "file:" + req.Path, name, nil
	case req.URL != "":
		ref, err := issues.Parse(req.URL)
		if err != nil {
			return "", "", &APIError{Validation, err.Error()}
		}
		return ref.URL(), fmt.Sprintf("issue-%d.md", ref.Number), nil
	default:
		return "stdin", "stdin", nil
	}
}

// validHandedName reports whether name is accepted as a file name under
// handed/ by the trace.
func validHandedName(name string) bool {
	return name != "" && path.Clean(name) == name && !strings.HasPrefix(name, ".") && strings.TrimSpace(name) == name &&
		!strings.ContainsAny(name, "/\\\x00\r\n:")
}

// readHanded reads the request's input: the file, the fetched issue or the
// stdin text.
func (s *Service) readHanded(ctx context.Context, req HandInRequest) (string, *APIError) {
	var content string
	switch {
	case req.Path != "":
		// Only a regular file is opened, and without blocking: opening a FIFO
		// would wait for a writer.
		info, err := os.Stat(req.Path)
		if err != nil {
			return "", &APIError{Validation, fmt.Sprintf("cannot read %s; check that the file exists and is readable", req.Path)}
		}
		if !info.Mode().IsRegular() {
			return "", &APIError{Validation, fmt.Sprintf("%s is not a regular file", req.Path)}
		}
		f, err := os.OpenFile(req.Path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			return "", &APIError{Validation, fmt.Sprintf("cannot read %s; check that the file exists and is readable", req.Path)}
		}
		defer f.Close()
		info, err = f.Stat()
		if err != nil || !info.Mode().IsRegular() {
			return "", &APIError{Validation, fmt.Sprintf("%s is not a regular file", req.Path)}
		}
		data, err := io.ReadAll(io.LimitReader(f, MaxHandedBytes+1))
		if err != nil {
			return "", &APIError{Validation, fmt.Sprintf("cannot read %s; check that the file exists and is readable", req.Path)}
		}
		content = string(data)
	case req.URL != "":
		ref, _ := issues.Parse(req.URL)
		text, err := s.options.Issues.Fetch(ctx, ref)
		if err != nil {
			return "", &APIError{Internal, fmt.Sprintf("cannot fetch %s; check the URL and the service's GitHub access", ref.URL())}
		}
		content = text
	default:
		content = *req.Stdin
	}
	if len(content) > MaxHandedBytes {
		return "", &APIError{Validation, fmt.Sprintf("handed input is larger than %d bytes", MaxHandedBytes)}
	}
	if strings.TrimSpace(content) == "" {
		return "", &APIError{Validation, "handed input is empty"}
	}
	if !utf8.ValidString(content) {
		return "", &APIError{Validation, "handed input must be UTF-8 text"}
	}
	return content, nil
}
