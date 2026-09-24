package trace

import (
	"maps"
	"regexp"
	"strings"

	"github.com/kpenfound/osmia/internal/coreadapter"
)

// TurnClassification records the rule and inputs used to classify a clean
// mason response. Window is the final 2,000 Unicode characters at most.
type TurnClassification struct {
	Class      string         `json:"class"`
	Evidence   string         `json:"evidence"`
	Window     string         `json:"window"`
	ToolCounts map[string]int `json:"tool_counts,omitempty"`
}

var (
	proseQuestion = regexp.MustCompile(`(?i)(?:\?|\b(?:could you|can you|please clarify|i need (?:an? )?(?:answer|clarification|decision)|what should i|which (?:one|option))\b)`)
	proseDone     = regexp.MustCompile(`(?i)\b(?:i (?:have )?(?:finished|completed|implemented)|(?:the )?(?:task|unit|work) is (?:done|complete|finished)|all (?:criteria|tests) (?:are |have )?(?:met|pass(?:ed|ing)?))\b`)
	proseGiveUp   = regexp.MustCompile(`(?i)\b(?:i (?:cannot|can't|give up|am unable to) (?:complete|finish|continue|proceed)|unable to (?:complete|finish|continue|proceed)|giving up|cannot proceed)\b`)
)

// ClassifyMasonTurn returns nil unless the mason completed without an outcome
// or execution failure. A tool call count is evidence even when no phrase matches.
func ClassifyMasonTurn(result coreadapter.SessionResult, failure string) *TurnClassification {
	if failure != "" || result.Outcome != nil || result.Cancelled || result.IsError || result.TimedOut || result.ExitCode != 0 || result.Signal != 0 {
		return nil
	}
	runes := []rune(result.FinalResponse)
	if len(runes) > 2000 {
		runes = runes[len(runes)-2000:]
	}
	window := string(runes)
	class, evidence := "unclear", "no matching phrase in the final 2,000 characters"
	for _, rule := range []struct {
		class string
		re    *regexp.Regexp
	}{
		{"gave_up", proseGiveUp},
		{"asked_in_prose", proseQuestion},
		{"claims_done", proseDone},
	} {
		if match := rule.re.FindString(window); match != "" {
			class, evidence = rule.class, strings.TrimSpace(match)
			break
		}
	}
	return &TurnClassification{Class: class, Evidence: evidence, Window: window, ToolCounts: maps.Clone(result.ToolCounts)}
}
