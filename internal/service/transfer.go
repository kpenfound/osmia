package service

import (
	"net/http"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/filetransport"
	"github.com/kpenfound/osmia/internal/tailnet"
)

// Transfers is where a turn's view archive routes get their turns: a turn
// registers its token and what the routes download from and upload to.
func (s *Service) Transfers() *filetransport.Registry { return s.transfers }

// transferRequest serves the read-only git route and the view archive routes,
// which only the tailnet listener answers.
func (s *Service) transferRequest(w http.ResponseWriter, r *http.Request) bool {
	project, rest, isGit := api.GitRoute(r.URL.Path)
	turn, isView := api.ViewRoute(r.URL.Path)
	relayed, _, isRelay := api.MCPRoute(r.URL.Path)
	if !isGit && !isView && !isRelay {
		return false
	}
	if tailnet.ArrivedOn(r) != tailnet.TailnetListener {
		fail(w, NotFound)
		return true
	}
	switch {
	case isRelay:
		if s.clients == nil {
			fail(w, NotFound)
			return true
		}
		if refusal := s.clients.Relay.Serve(w, r, relayed); refusal != nil {
			failWith(w, refusal)
		}
	case isGit:
		clone, ok := s.projectClone(project)
		if !ok {
			failWith(w, &APIError{Code: NotFound, Message: "no registered project has that ID"})
			return true
		}
		if refusal := filetransport.ServeGit(w, r, clone, rest); refusal != nil {
			failWith(w, refusal)
		}
	case r.Method == http.MethodGet:
		if refusal := s.transfers.Download(w, r, turn); refusal != nil {
			failWith(w, refusal)
		}
	case r.Method == http.MethodPut:
		if out, refusal := s.transfers.Upload(w, r, turn); refusal != nil {
			failWith(w, refusal)
		} else {
			respond(w, http.StatusOK, out)
		}
	default:
		fail(w, NotFound)
	}
	return true
}

func (s *Service) projectClone(id config.ProjectID) (string, bool) {
	id, err := config.ParseProjectID(string(id))
	if err != nil {
		return "", false
	}
	s.mu.Lock()
	cfg := s.cfg
	s.mu.Unlock()
	if !cfg.Active(id) {
		return "", false
	}
	clone := cfg.For(id).Project.Clone
	return clone, clone != ""
}
