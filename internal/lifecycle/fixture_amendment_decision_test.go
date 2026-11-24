package lifecycle_test

import (
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"strings"
)

var (
	amendedSpec = strings.Replace(fixturekit.ValidSpec, "1. An interrupted upload resumes from the last acknowledged chunk.", "1. An interrupted upload resumes from a durable checkpoint.", 1)
)
