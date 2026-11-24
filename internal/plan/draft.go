package plan

import (
	"context"

	"github.com/kpenfound/osmia/internal/charter"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/kb"
	"github.com/kpenfound/osmia/internal/trace"
)

// DraftProblems lists what keeps spec and graph, one workstream's draft,
// from being recorded: the spec's own problems, and the plan's when it
// parses, checked against entities; or the plan's parse error.
func DraftProblems(spec, graph string, entities kb.Map) []string {
	parsed, parseErr := Parse([]byte(graph))
	if parseErr != nil {
		parsed = Plan{Version: Version}
	}
	var out []string
	for _, p := range Validate(ParseSpec(spec), parsed, entities) {
		if parseErr == nil || p.Kind == SpecProblem {
			out = append(out, p.Error())
		}
	}
	if parseErr != nil {
		out = append(out, PlanPath+": "+parseErr.Error())
	}
	return out
}

// CharterProblems checks embedded charter citations against the citing member's
// current workgroup charters. It supplements structural draft validation.
func CharterProblems(ctx context.Context, repo *trace.Repository, stream config.WorkstreamID, documents ...string) []string {
	var out []string
	seen := map[string]bool{}
	for _, text := range documents {
		for _, c := range charter.References(text) {
			if seen[c] {
				continue
			}
			seen[c] = true
			if _, _, err := repo.ResolveCharter(ctx, stream, c); err != nil {
				out = append(out, "citation "+c+": "+err.Error())
			}
		}
	}
	return out
}
