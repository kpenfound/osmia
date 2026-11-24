package lifecycle_test

import (
	context "context"
	strings "strings"
	"sync"
	testing "testing"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
)

func TestRemovalRefusesUnresolvedProjectThenRemovesIt(t *testing.T) {
	t.Parallel()
	f := newTwoProjectFixture(t)
	entered, release := f.block(fixturekit.Project, "first")
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	s, c := start(t, f.opts)
	intakefixture.Await(t, "the first project's turn", entered)
	_, err := c.RemoveProject(context.Background(), fixturekit.Project)
	fixturekit.AssertCode(t, err, api.Validation)
	if !strings.Contains(err.Error(), "workgroup") || !s.current().Active(fixturekit.Project) {
		t.Fatalf("removal did not retain the unresolved project's workgroup: %v", err)
	}
	unblock()
	_, err = c.Abandon(context.Background(), f.streams[fixturekit.Project], "Owner resolved this project's member.")
	intakefixture.Must(t, err)
	_, err = c.RemoveProject(context.Background(), fixturekit.Project)
	intakefixture.Must(t, err)
	if len(s.traces()) != 1 || s.traces()[0].Project() != otherProject {
		t.Fatal("the removed project still participates after removal")
	}
	out, err := c.Reload(context.Background())
	intakefixture.Must(t, err)
	if len(out.RestartRequired) != 0 || len(s.traces()) != 1 || s.current().Active(fixturekit.Project) {
		t.Fatalf("a reload changed the active projects: %+v", out)
	}
}
