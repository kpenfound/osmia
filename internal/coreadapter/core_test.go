package coreadapter_test

import (
	"testing"

	"github.com/kpenfound/busybees/core/vcs"
)

// Compile against a public core contract without acquiring a repository.
func TestPinnedCoreDirectory(t *testing.T) {
	var workspace vcs.Workspace = vcs.Directory("/prepared/files")
	if workspace.Directory() != "/prepared/files" {
		t.Fatal("core directory contract changed")
	}
}
