package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
)

func TestReloadDrainsRemovedProjectAndAddsItAgain(t *testing.T) {
	t.Parallel()
	f := newTwoProjectFixture(t)
	entered, release := f.block(project, "first")
	s, c := start(t, f.opts)
	await(t, "the first project's turn", entered)
	path := filepath.Join(f.opts.Config.Root, "config.toml")
	original, err := os.ReadFile(path)
	must(t, err)
	must(t, os.WriteFile(path, []byte(strings.Replace(string(original), `"`+string(project)+`", `, "", 1)), 0600))
	done := make(chan error, 1)
	go func() { _, err := c.Reload(context.Background()); done <- err }()
	soon(t, "removal applied while its turn drains", func() bool { return !s.current().Active(project) })
	select {
	case err := <-done:
		t.Fatalf("reload did not wait for the active turn: %v", err)
	default:
	}
	close(release)
	must(t, <-done)
	if len(s.traces()) != 1 || s.traces()[0].Project() != otherProject {
		t.Fatal("the removed project still participates after draining")
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
