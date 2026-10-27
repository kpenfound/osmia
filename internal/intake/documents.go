package intake

import (
	"errors"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/kb"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/trace"
)

// DocumentHTTP supplies the service's local API encoding boundaries.
type DocumentHTTP struct {
	Decode   func(http.ResponseWriter, *http.Request, any) bool
	Fail     func(http.ResponseWriter, Code)
	FailWith func(http.ResponseWriter, *APIError)
	Respond  func(http.ResponseWriter, int, any)
}

func validDocument(content string) bool {
	return len(content) <= MaxHandedBytes && utf8.ValidString(content) && !strings.ContainsRune(content, 0)
}

// DocumentError maps durable edit failures to the local API's document responses.
func DocumentError(err error) *APIError {
	switch {
	case errors.Is(err, trace.ErrConflict):
		return &APIError{Code: Conflict, Message: "the document changed or is no longer editable; read it again before saving"}
	case errors.Is(err, trace.ErrInvalidEdit):
		return &APIError{Code: Validation, Message: err.Error()}
	default:
		return &APIError{Code: Internal, Message: "cannot read or save the documents; check the project trace"}
	}
}

// OwnerDocuments handles charter and draft revision reads and owner edits.
func (s *Controller) OwnerDocuments(w http.ResponseWriter, r *http.Request, h DocumentHTTP) bool {
	if raw, ok := strings.CutPrefix(r.URL.Path, Prefix+"/projects/charter/"); ok {
		id, api := s.ProjectFor(config.ProjectID(raw))
		if api != nil {
			h.FailWith(w, api)
			return true
		}
		repo, err := s.Repository(id)
		if err != nil {
			h.FailWith(w, DocumentError(err))
			return true
		}
		var doc trace.Document
		switch r.Method {
		case http.MethodGet:
			doc, err = repo.Charter(r.Context())
		case http.MethodPut:
			var edit CharterEdit
			if !h.Decode(w, r, &edit) {
				return true
			}
			if !validDocument(edit.Content) || edit.Revision < 1 {
				h.Fail(w, Validation)
				return true
			}
			doc, err = repo.EditCharter(r.Context(), edit.Revision, edit.Content, s.Now())
		default:
			h.Fail(w, Unsupported)
			return true
		}
		if err != nil {
			h.FailWith(w, DocumentError(err))
		} else {
			h.Respond(w, 200, doc)
		}
		return true
	}
	if raw, ok := strings.CutPrefix(r.URL.Path, Prefix+"/documents/"); ok {
		_, stream, repo, api := s.ConversationTrace(raw)
		if api != nil {
			h.FailWith(w, api)
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
			if !h.Decode(w, r, &edit) {
				return true
			}
			if !validDocument(edit.Spec) || !validDocument(edit.Plan) {
				h.Fail(w, Validation)
				return true
			}
			entities, loadErr := kb.Load(repo)
			if loadErr != nil {
				h.FailWith(w, DocumentError(loadErr))
				return true
			}
			if problems := plan.CharterProblems(r.Context(), repo, stream, edit.Spec, edit.Plan); len(problems) > 0 {
				h.FailWith(w, &APIError{Code: Validation, Message: strings.Join(problems, "; ")})
				return true
			}
			out, err = repo.EditDraft(r.Context(), stream, edit.SpecRevision, edit.PlanRevision, edit.Spec, edit.Plan, s.Now(), func(content map[string]string) error {
				if problems := ValidateDraft(content["spec"], content["plan"], entities); len(problems) > 0 {
					return errors.New(strings.Join(problems, "; "))
				}
				return nil
			})
		default:
			h.Fail(w, Unsupported)
			return true
		}
		if err != nil {
			h.FailWith(w, DocumentError(err))
		} else {
			h.Respond(w, 200, out)
		}
		return true
	}
	return false
}
