package fixturekit

import (
	"github.com/kpenfound/agentcore/agent"
	"path/filepath"
	"slices"
	"strings"
)

// CheckGrants reports a request whose grants or verified turn reach beyond a
// private writable view outside clone: another mount, VCS, an ungranted tool, a
// VCS executable on PATH, or a variable beyond the request's own.
func CheckGrants(req agent.Request, verified *agent.Turn, clone string, fail func(string, ...any)) {
	view := req.Workspace.Directory()
	g := req.Grants
	if g == nil || len(g.Mounts) != 2 || g.Mounts[0] != (agent.Mount{Path: view, Access: agent.ReadWrite}) || g.Mounts[1] != (agent.Mount{Path: req.SessionDir, Access: agent.ReadOnly}) || view == clone || strings.HasPrefix(view, clone+string(filepath.Separator)) {
		fail("grants %+v expose more than the private view", g)
		return
	}
	if g.VCS || verified.VCS || !slices.Equal(verified.DeniedExecutables, agent.VCSExecutables) {
		fail("VCS is not denied: %+v", verified)
	}
	if !slices.Equal(g.Tools, []string{"Read", "Glob", "Grep", "Write", "Edit", "mcp__osmia_0"}) || !slices.Equal(verified.Tools, []string{"Read", "Glob", "Grep", "Write", "Edit"}) {
		fail("tools %v %v exceed the role grant", g.Tools, verified.Tools)
	}
	for _, kv := range verified.Env {
		key, value, _ := strings.Cut(kv, "=")
		if key != "HOME" && req.Env[key] != value {
			fail("verified environment exposes %s", key)
		}
	}
}
