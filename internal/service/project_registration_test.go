package service

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/runtime"
)

func TestProjectRegistrationRecoversAlongsideAnActiveProject(t *testing.T) {
	for _, step := range []string{"journal-written", "project-config-written", "trace-created", "active-project-listed", "journal-removed"} {
		t.Run(step, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			opts, clone := projectFixture(t)
			s, c := start(t, opts)
			first, err := c.AddProject(ctx, request(clone))
			must(t, err)
			mutation(t, c, "PUT", "pause", PauseRequest{Target: runtime.Target{Scope: "project", Project: first.Project.ID}, Mode: "soft", Reason: "Owner review"})
			secondClone := filepath.Join(filepath.Dir(clone), "other")
			demoGit(t, filepath.Dir(clone), "init", "--quiet", secondClone)
			req := ProjectAddRequest{Name: "other", Upstream: "upstream/other", Fork: "owner/other", Clone: secondClone}
			s.boundary = func(name string) error {
				if name == step {
					return errors.New("interrupted registration")
				}
				return nil
			}
			_, err = c.AddProject(ctx, req)
			assertCode(t, err, Internal)
			must(t, s.Close())
			s, c = start(t, opts)
			second, err := c.AddProject(ctx, req)
			must(t, err)
			if first.Project.ID == second.Project.ID || len(projectDirectories(t, opts.Config.Root)) != 2 {
				t.Fatal("recovery duplicated or replaced a project")
			}
			want := []config.ProjectID{first.Project.ID, second.Project.ID}
			if !slices.Equal(s.current().ProjectIDs(), want) || len(s.traces()) != 2 {
				t.Fatalf("active projects: %v", s.current().ProjectIDs())
			}
			rt, err := c.Runtime(ctx)
			must(t, err)
			if len(rt.Effective.Pauses) != 1 || rt.Effective.Pauses[0].Target.Project != first.Project.ID {
				t.Fatalf("lost another project's controls: %+v", rt.Effective.Pauses)
			}
			_, err = c.RemoveProject(ctx, second.Project.ID)
			must(t, err)
			if !slices.Equal(s.current().ProjectIDs(), want[:1]) {
				t.Fatal("removing the new project removed the existing project")
			}
		})
	}
}
