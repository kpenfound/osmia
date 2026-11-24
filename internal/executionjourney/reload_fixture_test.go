package executionjourney

import (
	"testing"

	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
)

type reloadFiles struct {
	fixturekit.ReloadFiles
	top, topText string
}

func configFiles(t *testing.T, opts Options) reloadFiles {
	f := fixturekit.ConfigFiles(t, opts.Config.Root)
	return reloadFiles{f, f.Top, f.TopText}
}
func (f reloadFiles) write(t *testing.T, top string) { f.ReloadFiles.Write(t, top) }
