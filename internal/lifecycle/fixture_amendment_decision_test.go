package lifecycle_test

import (
	"strings"

	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
)

var (
	amendedSpec = strings.Replace(intakefixture.ValidSpec, "1. An interrupted upload resumes from the last acknowledged chunk.", "1. An interrupted upload resumes from a durable checkpoint.", 1)
)
