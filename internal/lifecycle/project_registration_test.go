package lifecycle_test

import (
	context "context"
	errors "errors"
	filepath "path/filepath"
	slices "slices"
	testing "testing"

	"github.com/kpenfound/osmia/internal/api"
	config "github.com/kpenfound/osmia/internal/config"
	runtime "github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
)

func TestProjectRegistrationRecoversAlongsideAnActiveProject(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	opts, clone := projectFixture(t)
	s, c := start(t, opts)
	first, err := c.AddProject(ctx, fixturekit.Request(clone))
	fixturekit.Must(t, err)
	mutation(t, c, "PUT", "pause", api.PauseRequest{Target: runtime.Target{Scope: "project", Project: first.Project.ID}, Mode: "soft", Reason: "Owner review"})
	secondClone := filepath.Join(filepath.Dir(clone), "other")
	fixturekit.DemoGit(t, filepath.Dir(clone), "init", "--quiet", secondClone)
	req := api.ProjectAddRequest{Name: "other", Upstream: "upstream/other", Fork: "owner/other", Clone: secondClone}
	s.boundary = func(name string) error {
		if name == "trace-created" {
			return errors.New("interrupted registration")
		}
		return nil
	}
	_, err = c.AddProject(ctx, req)
	fixturekit.AssertCode(t, err, api.Internal)
	fixturekit.Must(t, s.Close())
	s, c = start(t, opts)
	second, err := c.AddProject(ctx, req)
	fixturekit.Must(t, err)
	if first.Project.ID == second.Project.ID || len(projectDirectories(t, opts.Config.Root)) != 2 {
		t.Fatal("recovery duplicated or replaced a project")
	}
	want := []config.ProjectID{first.Project.ID, second.Project.ID}
	if !slices.Equal(s.current().ProjectIDs(), want) || len(s.traces()) != 2 {
		t.Fatalf("active projects: %v", s.current().ProjectIDs())
	}
	rt, err := c.Runtime(ctx)
	fixturekit.Must(t, err)
	if len(rt.Effective.Pauses) != 1 || rt.Effective.Pauses[0].Target.Project != first.Project.ID {
		t.Fatalf("lost another project's controls: %+v", rt.Effective.Pauses)
	}
	_, err = c.RemoveProject(ctx, second.Project.ID)
	fixturekit.Must(t, err)
	if !slices.Equal(s.current().ProjectIDs(), want[:1]) {
		t.Fatal("removing the new project removed the existing project")
	}
}
