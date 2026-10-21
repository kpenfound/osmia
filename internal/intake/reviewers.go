package intake

import (
	"strings"

	"github.com/kpenfound/osmia/internal/trace"
)

const ReviewerRole = "reviewer"

func ReviewerAgent(unit string) string {
	return ReviewerRole + strings.TrimPrefix(trace.UnitSubject(unit), "unit")
}
