package service

import (
	"errors"
	"net/http"
	"strings"

	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/trace"
)

func documentFailure(w http.ResponseWriter, err error) {
	failWith(w, intake.DocumentError(err))
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
				failWith(w, &APIError{Code: Validation, Message: "base must form an acyclic graph of available workstreams in this project"})
			}
		} else {
			respond(w, 200, out)
		}
		return true
	}

	return s.intake().OwnerDocuments(w, r, intake.DocumentHTTP{Decode: decode, Fail: fail, FailWith: failWith, Respond: respond})
}
