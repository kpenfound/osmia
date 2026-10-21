package lifecycle_test

import (
	"strings"
	"testing"
)

func useUpstreamForPush(t *testing.T, p *publicationFixture) {
	t.Helper()
	p.fork = strings.TrimSpace(demoGit(t, p.clone, "-C", p.clone, "remote", "get-url", "upstream"))
	p.s.cfg.Project.Fork = ""
	p.pulls.SyncHead = true
	p.s.options.PullRequests = p.pulls
}
