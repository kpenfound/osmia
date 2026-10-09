package service

import (
	"errors"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/kb"
	"github.com/kpenfound/osmia/internal/trace"
)

type DraftEdit struct {
	SpecRevision int    `json:"spec_revision"`
	PlanRevision int    `json:"plan_revision"`
	Spec         string `json:"spec"`
	Plan         string `json:"plan"`
}

type CharterEdit struct {
	Revision int    `json:"revision"`
	Content  string `json:"content"`
}

type BaseEdit struct {
	Base     config.WorkstreamID `json:"base"`
	Revision int                 `json:"revision"`
}

func validDocument(content string) bool {
	return len(content) <= MaxHandedBytes && utf8.ValidString(content) && !strings.ContainsRune(content, 0)
}

func documentFailure(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, trace.ErrConflict):
		failWith(w, &APIError{Conflict, "the document changed or is no longer editable; read it again before saving"})
	case errors.Is(err, trace.ErrOwnerEdit):
		failWith(w, &APIError{Validation, err.Error()})
	default:
		failWith(w, &APIError{Internal, "cannot read or save the documents; check the project trace"})
	}
}

func (s *Service) documentRequest(w http.ResponseWriter, r *http.Request) bool {
	if raw, ok := strings.CutPrefix(r.URL.Path, Prefix+"/projects/memory/"); ok {
		s.memorySetup(w, r, raw)
		return true
	}
	if raw, ok := strings.CutPrefix(r.URL.Path, Prefix+"/base/"); ok {
		if raw, ok := strings.CutSuffix(raw, "/upstream"); ok {
			if r.Method != http.MethodPost {
				fail(w, Unsupported)
				return true
			}
			var in BaseUpstreamRequest
			if !decode(w, r, &in) {
				return true
			}
			if out, api := s.baseUpstream(r.Context(), raw, in); api != nil {
				failWith(w, api)
			} else {
				respond(w, 200, out)
			}
			return true
		}
		_, stream, repo, api := s.conversationTrace(raw)
		if api != nil {
			failWith(w, api)
			return true
		}
		var out trace.WorkstreamBase
		var err error
		switch r.Method {
		case http.MethodGet:
			out, err = repo.WorkstreamBase(stream)
		case http.MethodPut:
			var in BaseEdit
			if !decode(w, r, &in) {
				return true
			}
			_, archivedBase := s.archivedWorkstream(string(in.Base))
			if archivedBase || in.Base == librarianWorkstream(repo.Project()) {
				fail(w, Validation)
				return true
			}
			out, err = repo.SetWorkstreamBase(r.Context(), stream, in.Base, in.Revision, s.now())
		default:
			fail(w, Unsupported)
			return true
		}
		if err != nil {
			if errors.Is(err, trace.ErrConflict) {
				documentFailure(w, err)
			} else {
				failWith(w, &APIError{Validation, "base must form an acyclic graph of available workstreams in this project"})
			}
		} else {
			respond(w, 200, out)
		}
		return true
	}

	if raw, ok := strings.CutPrefix(r.URL.Path, Prefix+"/projects/charter/"); ok {
		id, api := s.projectFor(config.ProjectID(raw))
		if api != nil {
			failWith(w, api)
			return true
		}
		repo, err := s.repository(id)
		if err != nil {
			documentFailure(w, err)
			return true
		}
		var doc trace.Document
		switch r.Method {
		case http.MethodGet:
			doc, err = repo.Charter(r.Context(), s.now())
		case http.MethodPut:
			var edit CharterEdit
			if !decode(w, r, &edit) {
				return true
			}
			if !validDocument(edit.Content) || edit.Revision < 1 {
				fail(w, Validation)
				return true
			}
			doc, err = repo.EditCharter(r.Context(), edit.Revision, edit.Content, s.now())
		default:
			fail(w, Unsupported)
			return true
		}
		if err != nil {
			documentFailure(w, err)
		} else {
			respond(w, 200, doc)
		}
		return true
	}
	if raw, ok := strings.CutPrefix(r.URL.Path, Prefix+"/documents/"); ok {
		_, stream, repo, api := s.conversationTrace(raw)
		if api != nil {
			failWith(w, api)
			return true
		}
		var out map[string]trace.Document
		var err error
		switch r.Method {
		case http.MethodGet:
			var docs []trace.Document
			docs, err = trace.Read[trace.Document](repo, stream)
			out = map[string]trace.Document{}
			for _, d := range docs {
				if d.ID == "spec" || d.ID == "plan" || d.ID == "handed" {
					out[d.ID] = d
				}
			}
		case http.MethodPut:
			var edit DraftEdit
			if !decode(w, r, &edit) {
				return true
			}
			if !validDocument(edit.Spec) || !validDocument(edit.Plan) {
				fail(w, Validation)
				return true
			}
			entities, loadErr := kb.Load(repo)
			if loadErr != nil {
				documentFailure(w, loadErr)
				return true
			}
			out, err = repo.EditDraft(r.Context(), stream, edit.SpecRevision, edit.PlanRevision, edit.Spec, edit.Plan, s.now(), func(content map[string]string) error {
				if problems := validateDraft(content["spec"], content["plan"], entities); len(problems) > 0 {
					return errors.New(strings.Join(problems, "; "))
				}
				return nil
			})
		default:
			fail(w, Unsupported)
			return true
		}
		if err != nil {
			documentFailure(w, err)
		} else {
			respond(w, 200, out)
		}
		return true
	}
	return false
}
