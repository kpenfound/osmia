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

	s.handInMu.Lock()
	defer s.handInMu.Unlock()
	repository, err := s.repository(req.Project)
	if err != nil {
		return HandInResponse{}, &APIError{NotFound, fmt.Sprintf("project %s is not an active project; check the project ID with osmia status", req.Project)}
	}
	stream := HandInWorkstream(req.Project, req.Key)
	failed := func(step string) (HandInResponse, *APIError) {
		return HandInResponse{}, &APIError{Internal, fmt.Sprintf("hand-in to project %s failed while %s workstream %s; retry with the same key, or check %s", req.Project, step, stream, view.Trace)}
	}
	streams, err := repository.Workstreams()
	if err != nil {
		return failed("reading")
	}
	exists := slices.Contains(streams, stream)
	var doc *trace.Document
	if exists {
		docs, err := trace.Read[trace.Document](repository, stream)
		if err != nil {
			return failed("reading")
		}
		for _, d := range docs {
			if d.ID == handedDocument {
				doc = &d
			}
		}
	}
	if doc != nil && (doc.Source != source || req.Stdin != nil && doc.Content != *req.Stdin) {
		return HandInResponse{}, &APIError{Conflict, fmt.Sprintf("key %s already handed in other input as workstream %s; use a new key", req.Key, stream)}
	}
	if doc == nil {
		content, api := s.readHanded(ctx, req)
		if api != nil {
			return HandInResponse{}, api
		}
		now := time.Now().UTC()
		if exists {
			_, err = repository.EnsureChiefOfStaff(ctx, stream, now, ownerActor)
		} else {
			err = repository.CreateWorkstream(ctx, stream, now, ownerActor)
		}
		if err != nil {
			return failed("creating")
		}
		doc = &trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: handedDocument, Revision: 1, Project: req.Project, Workstream: stream, At: now, Actor: ownerActor, Cause: handInTransition},
			Path: "handed/" + name, Content: content, Source: source}
		if err := repository.Append(ctx, *doc); err != nil {
			return failed("copying the input into")
		}
	}
	// The transition carries the handed document's timestamp, so a retry
	// repeats the committed transition exactly.
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: handInTransition, Revision: 1, Project: req.Project, Workstream: stream, At: doc.At, Actor: ownerActor, Cause: handInTransition}
	state, err := repository.SetFeatureState(ctx, h, HandedState, "the owner handed in "+doc.Path+" from "+source)
	if err != nil {
		return failed("recording the handed state of")
	}
	return HandInResponse{Project: req.Project, Workstream: stream, State: state.Value,
		Handed: filepath.Join(view.Trace, "workstreams", string(stream), filepath.FromSlash(doc.Path)), Source: source}, nil
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
		f, err := os.Open(req.Path)
		if err != nil {
			return "", &APIError{Validation, fmt.Sprintf("cannot read %s; check that the file exists and is readable", req.Path)}
		}
		defer f.Close()
		info, err := f.Stat()
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
