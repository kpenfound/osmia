package lifecycle_test

import (
	context "context"
	config "github.com/kpenfound/osmia/internal/config"
	os "os"
	filepath "path/filepath"
	strings "strings"
	"sync"
	testing "testing"
)

func TestReloadRefusesUnresolvedProjectThenRemovesAndAddsItAgain(t *testing.T) {
	t.Parallel()
	f := newTwoProjectFixture(t)
	entered, release := f.block(project, "first")
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	s, c := start(t, f.opts)
	await(t, "the first project's turn", entered)
	path := filepath.Join(f.opts.Config.Root, "config.toml")
	original, err := os.ReadFile(path)
	must(t, err)
	must(t, os.WriteFile(path, []byte(strings.Replace(string(original), `"`+string(project)+`", `, "", 1)), 0600))
	_, err = c.Reload(context.Background())
	assertCode(t, err, Validation)
	if !strings.Contains(err.Error(), "workgroup") || !s.current().Active(project) {
		t.Fatalf("reload did not retain the unresolved project's workgroup: %v", err)
	}
	unblock()
	_, err = c.Abandon(context.Background(), f.streams[project], "Owner resolved this project's member.")
	must(t, err)
	_, err = c.Reload(context.Background())
	must(t, err)
	if len(s.traces()) != 1 || s.traces()[0].Project() != otherProject {
		t.Fatal("the removed project still participates after removal")
	}
	// A valid project-list edit starts the retained trace without registration.
	must(t, os.WriteFile(path, original, 0600))
	out, err := c.Reload(context.Background())
	must(t, err)
	if len(out.RestartRequired) != 0 || len(s.traces()) != 2 {
		t.Fatalf("re-add: %+v", out)
	}
	for _, id := range []config.ProjectID{project, otherProject} {
		if !s.current().Active(id) {
			t.Fatalf("missing project %s", id)
		}
	}
}
