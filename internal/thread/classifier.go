package thread

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/trace"
)

type classificationJSON struct {
	Class    string `json:"class"`
	Evidence string `json:"evidence"`
}

func parseClassification(s string) (classificationJSON, bool) {
	var v classificationJSON
	if len(s) > 8192 {
		return v, false
	}
	d := json.NewDecoder(strings.NewReader(s))
	start, err := d.Token()
	if err != nil || start != json.Delim('{') {
		return v, false
	}
	seen := map[string]bool{}
	for d.More() {
		token, err := d.Token()
		if err != nil {
			return v, false
		}
		name, ok := token.(string)
		if !ok || seen[name] {
			return v, false
		}
		seen[name] = true
		var value string
		if err := d.Decode(&value); err != nil {
			return v, false
		}
		switch name {
		case "class":
			v.Class = value
		case "evidence":
			v.Evidence = value
		default:
			return v, false
		}
	}
	end, err := d.Token()
	if err != nil || end != json.Delim('}') || len(seen) != 2 {
		return v, false
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return v, false
	}
	switch v.Class {
	case "asked_in_prose", "claims_done", "gave_up", "unclear":
	default:
		return v, false
	}
	return v, strings.TrimSpace(v.Evidence) != "" && len([]rune(v.Evidence)) <= 500
}

func (r Runner) classify(ctx context.Context, original coreadapter.PreparedTurn, response *trace.TurnResponse) {
	profile := *r.ClassifierProfile
	if profile.Timeout <= 0 || profile.Timeout > 30*time.Second {
		profile.Timeout = 30 * time.Second
	}
	if profile.Backend == "claude" {
		profile.MaxTurns = 1
	}
	for i := 1; i <= 2; i++ {
		if ctx.Err() != nil {
			return
		}
		input := coreadapter.PreparedTurn{
			Scope: original.Scope, Profile: profile,
			SessionDirectory: fmt.Sprintf("%s/classifier-%d", original.SessionDirectory, i),
			SystemPrompt:     `Classify a completed mason response. Return only a JSON object with exactly two string properties: "class" (one of "asked_in_prose", "claims_done", "gave_up", "unclear") and "evidence" (a brief reason grounded in the response). Do not take any action.`,
			Prompt:           fmt.Sprintf("Final response:\n%s\n\nService tool counts: %v", response.Classification.Window, response.Classification.ToolCounts),
		}
		input.Scope.Role = "classifier"
		input.Scope.Turn = fmt.Sprintf("%s-classifier-%d", original.Scope.Turn, i)
		attempt, cancel := context.WithTimeout(ctx, profile.Timeout)
		result, err := r.Classifier(attempt, input)
		cancel()
		if !result.StartedAt.IsZero() || result.Usage.Turns > 0 || result.Usage.CostKnown || result.Usage.CostUSD > 0 {
			response.ClassifierUsage = append(response.ClassifierUsage, result.Usage)
		}
		if err != nil || result.IsError || result.TimedOut || result.Cancelled || result.ExitCode != 0 || result.Outcome != nil {
			continue
		}
		if v, ok := parseClassification(result.FinalResponse); ok {
			response.Classification.Class = v.Class
			response.Classification.Evidence = v.Evidence
			return
		}
	}
}
