package intake

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/kpenfound/osmia/internal/config"
)

// LibrarianWorkstream is the ID of the workstream a retired librarian kept in
// a project trace. The service layer still filters it out of its listings.
func LibrarianWorkstream(project config.ProjectID) config.WorkstreamID {
	sum := sha256.Sum256([]byte("librarian:" + string(project)))
	return config.WorkstreamID("w_" + hex.EncodeToString(sum[:16]))
}
