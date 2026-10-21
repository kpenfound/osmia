package intake

import (
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

// isSession reports whether a workstream is a session, so a scheduler role
// or pass can leave it alone.
func IsSession(repository *trace.Repository, stream config.WorkstreamID) (bool, error) {
	kind, err := repository.Kind(stream)
	return kind == trace.SessionKind, err
}

// chiefOfStaffSkip reports a workstream whose events stay undelivered to its
// chief of staff: an abandoned workstream, as before, and now also a
// session, which has no chief of staff to deliver them to.
func ChiefOfStaffSkip(repository *trace.Repository) func(config.WorkstreamID) (bool, error) {
	return func(stream config.WorkstreamID) (bool, error) {
		if session, err := IsSession(repository, stream); err != nil || session {
			return session, err
		}
		return Abandoned(repository, stream)
	}
}
