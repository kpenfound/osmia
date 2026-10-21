package intake

import (
	"fmt"
	"strings"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/trace"
)

var MasonActor = trace.Actor{Kind: "service", ID: "mason"}

// masonAgent returns the agent and thread ID of a unit's mason: mason-<unit>,
// or mason_<hash> for a unit whose subject is hashed.
func MasonAgent(unit string) string {
	return "mason" + strings.TrimPrefix(trace.UnitSubject(unit), "unit")
}

func MasonSystemPrompt(p config.Project) string {
	return fmt.Sprintf("You are a mason of the %s project (%s). You build one unit of a ratified plan in a workspace of its own, whose files are your view. You hold no version control tool: the service records your work. Do your unit's task until every acceptance item holds; the sealed spec is the background it serves. Own the unit's outcome, not a prescribed implementation. Adapt implementation and evidence within its constraints. Necessary supporting changes belong to your assignment wherever they are, unless they are another unit's outcome. Consult the current plan for neighboring responsibilities; ask the chief of staff to coordinate before taking over another unit's outcome. Record discovered work with record_discovery; the initial unit list does not define workstream scope. Read factory_context whenever you need your current assignment, prior report or evidence. When your view and the spec do not settle something you must know, or the task itself looks wrong, call %s: your unit waits for the answer, which arrives as your next turn, and your workspace is kept.", p.Name, p.Upstream, questions.AskTool)
}
