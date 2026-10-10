package runtime

import (
	"path/filepath"

	"github.com/kpenfound/osmia/internal/config"
)

// modelSessionsDirectory is the directory under the root holding a session's
// model-session state, outside the worktree and every other workspace kind.
const modelSessionsDirectory = "session-models"

// ModelSessionDirectory is the directory a session's model session keeps its
// runtime state in, under the service root and outside the worktree and
// target repository: <root>/session-models/<project>/<workstream>. It is
// kept per session, reused by every turn, so a resumable backend has a
// stable location to persist to across turns and restarts.
func ModelSessionDirectory(root string, project config.ProjectID, stream config.WorkstreamID) string {
	return filepath.Join(root, modelSessionsDirectory, string(project), string(stream))
}
