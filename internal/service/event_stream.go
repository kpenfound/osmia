package service

import (
	"net/http"
)

// streamEvents serves change notifications until the request or service ends.
func (s *Service) streamEvents(w http.ResponseWriter, r *http.Request) {
	if !s.hub.Serve(w, r, s.options.WriteTimeout) {
		fail(w, Unavailable)
	}
}
