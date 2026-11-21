package lifecycle_test

import (
	context "context"
	strings "strings"
	"sync"
	testing "testing"
)

// A project with an unresolved workgroup member is not removed until the member
// is resolved. A reload never adds or removes a project, so the removed project
// stays removed.
func TestRemovalRefusesUnresolvedProjectThenRemovesIt(t *testing.T) {
	t.Parallel()
	f := newTwoProjectFixture(t)
	entered, release := f.block(project, "first")
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	s, c := start(t, f.opts)
	await(t, "the first project's turn", entered)
	_, err := c.RemoveProject(context.Background(), project)
	assertCode(t, err, Validation)
	if !strings.Contains(err.Error(), "workgroup") || !s.current().Active(project) {
		t.Fatalf("removal did not retain the unresolved project's workgroup: %v", err)
	}
	unblock()
	_, err = c.Abandon(context.Background(), f.streams[project], "Owner resolved this project's member.")
	must(t, err)
	_, err = c.RemoveProject(context.Background(), project)
	must(t, err)
	if len(s.traces()) != 1 || s.traces()[0].Project() != otherProject {
		t.Fatal("the removed project still participates after removal")
	}
	out, err := c.Reload(context.Background())
	must(t, err)
	if len(out.RestartRequired) != 0 || len(s.traces()) != 1 || s.current().Active(project) {
		t.Fatalf("a reload changed the active projects: %+v", out)
	}
}
