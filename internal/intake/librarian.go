package intake

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/kpenfound/osmia/internal/config"
)

// LibrarianWorkstream is the ID of the librarian workstream an older project
// trace can hold. Nothing creates a workstream under it; status, the page and
// every controller pass skip one that exists.
func LibrarianWorkstream(project config.ProjectID) config.WorkstreamID {
	sum := sha256.Sum256([]byte("librarian:" + string(project)))
	return config.WorkstreamID("w_" + hex.EncodeToString(sum[:16]))
}
