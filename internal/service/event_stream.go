package service

import (
	"net/http"
)

func (s *Service) streamEvents(w http.ResponseWriter, r *http.Request) {
	if !s.hub.Serve(w, r, s.options.WriteTimeout) {
		fail(w, Unavailable)
	}
}
