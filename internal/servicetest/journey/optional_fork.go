package journey

import (
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
)

func UseUpstreamForPush(t *testing.T, p *PublicationFixture) {
	t.Helper()
	p.Fork = strings.TrimSpace(fixturekit.DemoGit(t, p.Clone, "-C", p.Clone, "remote", "get-url", "upstream"))
	p.S.Cfg.Project.Fork = ""
	p.Pulls.SyncHead = true
	p.S.Options.PullRequests = p.Pulls
}
