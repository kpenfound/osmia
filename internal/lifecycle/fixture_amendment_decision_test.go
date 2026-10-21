package lifecycle_test

import (
	"strings"
)

var (
	// amendedSpec changes the meaning of criterion 1 of validSpec.
	amendedSpec = strings.Replace(validSpec, "1. An interrupted upload resumes from the last acknowledged chunk.", "1. An interrupted upload resumes from a durable checkpoint.", 1)
)
