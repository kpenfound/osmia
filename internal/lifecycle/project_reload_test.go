package lifecycle_test

import (
	context "context"
	strings "strings"
	"sync"
	testing "testing"

	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
)

func TestRemovalRefusesUnresolvedProjectThenRemovesIt(t *testing.T) {
	t.Parallel()
	f := journey.NewTwoProjectFixture(t)
	entered, release := f.Turns.Block(intakefixture.Turn(fixturekit.Project, "first"))
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	s, c := journey.StartT(t, f.Opts)
	intakefixture.Await(t, "the first project's turn", entered)
	_, err := c.RemoveProject(context.Background(), fixturekit.Project)
	fixturekit.AssertCode(t, err, api.Validation)
	if !strings.Contains(err.Error(), "workgroup") || !s.Current().Active(fixturekit.Project) {
		t.Fatalf("removal did not retain the unresolved project's workgroup: %v", err)
	}
	unblock()
	_, err = c.Abandon(context.Background(), f.Streams[fixturekit.Project], "Owner resolved this project's member.")
	fixturekit.Must(t, err)
	_, err = c.RemoveProject(context.Background(), fixturekit.Project)
	fixturekit.Must(t, err)
	if len(traces(s)) != 1 || traces(s)[0].Project() != fixturekit.OtherProject {
		t.Fatal("the removed project still participates after removal")
	}
	out, err := c.Reload(context.Background())
	fixturekit.Must(t, err)
	if len(out.RestartRequired) != 0 || len(traces(s)) != 1 || s.Current().Active(fixturekit.Project) {
		t.Fatalf("a reload changed the active projects: %+v", out)
	}
}
