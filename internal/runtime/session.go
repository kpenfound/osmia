package runtime

import (
	"path/filepath"
	"slices"

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

// ForgetSession removes owner overrides naming a permanently removed session.
func (s *Store) ForgetSession(project config.ProjectID, stream config.WorkstreamID) error {
	return s.mutate(func(st *State, _ Inputs) error {
		s.input.Workstreams[project] = slices.DeleteFunc(slices.Clone(s.input.Workstreams[project]), func(w config.WorkstreamID) bool { return w == stream })
		st.Pauses = slices.DeleteFunc(st.Pauses, func(p Pause) bool { return p.Target.Project == project && p.Target.Workstream == stream })
		for i := range st.Priorities {
			if st.Priorities[i].Project == project {
				st.Priorities[i].Workstreams = slices.DeleteFunc(st.Priorities[i].Workstreams, func(w config.WorkstreamID) bool { return w == stream })
			}
		}
		return nil
	})
}
