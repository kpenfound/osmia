package intake

import (
	"errors"
)

var (
	ErrNoActiveProject = errors.New("no active project")
	ErrNoTrace         = errors.New("project has no trace")
)
