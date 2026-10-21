package executionjourney

import (
	"testing"

	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
)

type reloadFiles struct {
	fixturekit.ReloadFiles
	top, project, topText, projectText string
}

func configFiles(t *testing.T, opts Options) reloadFiles {
	f := fixturekit.ConfigFiles(t, opts.Config.Root, project)
	return reloadFiles{f, f.Top, f.Project, f.TopText, f.ProjectText}
}
func (f reloadFiles) write(t *testing.T, top, project string) { f.ReloadFiles.Write(t, top, project) }
