package checks

import (
	"errors"

	"github.com/kpenfound/osmia/internal/callargs"
)

// ValidatePreviewCall checks agent-supplied arguments before an operation is pinned.
func ValidatePreviewCall(call []string) error {
	if len(call) == 0 {
		return errors.New("preview call must be a non-empty argument list returning a Service")
	}
	return callargs.Validate("preview call", call)
}

// ValidatePreviewSource checks the same values through the exported source's
// root. Root.Stat follows symlinks while refusing any escape, even through a
// nonexistent descendant. It does not open files, sockets or devices.
func ValidatePreviewSource(dir string, call []string) error {
	return callargs.ValidateSource("preview call", dir, call)
}
