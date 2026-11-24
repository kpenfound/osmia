package lifecycle_test

import (
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
)

func useUpstreamForPush(t *testing.T, p *publicationFixture) {
	t.Helper()
	p.fork = strings.TrimSpace(fixturekit.DemoGit(t, p.clone, "-C", p.clone, "remote", "get-url", "upstream"))
	p.s.cfg.Project.Fork = ""
	p.pulls.SyncHead = true
	p.s.options.PullRequests = p.pulls
}
