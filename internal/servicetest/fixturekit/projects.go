package fixturekit

import (
	"github.com/kpenfound/osmia/internal/api"
)

const CommentedConfig = `# Owner's notes stay at the top.
version = 1
active_projects = [] # none yet
workspaces = "git" # the tests run on Git worktrees whether or not jj is installed

[profiles.default] # keep this profile
agent = "claude"
model = "test"
[profiles.other]
agent = "codex"
model = "other"
`

func Request(clone string) api.ProjectAddRequest {
	return api.ProjectAddRequest{Name: "dagger", Upstream: "dagger/dagger", Fork: "owner/dagger", Clone: clone}
}
