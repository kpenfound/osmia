package trace

import (
	"maps"
	"regexp"
	"strings"

	"github.com/kpenfound/osmia/internal/coreadapter"
)

// TurnClassification records the class of a clean mason response, the
// evidence for it and the inputs it was judged from. Window is the final
// 2,000 Unicode characters at most. By says what classified the response;
// Judgment is the Jev judgment record behind a ClassifiedByJev class.
type TurnClassification struct {
	Class      string         `json:"class"`
	Evidence   string         `json:"evidence"`
	Window     string         `json:"window"`
	ToolCounts map[string]int `json:"tool_counts,omitempty"`
	By         string         `json:"by,omitempty"`
	Judgment   string         `json:"judgment,omitempty"`
}

// What classified a clean mason response: the code rules, the configured
// classifier profile or a Jev judgment.
const (
	ClassifiedByRule       = "rule"
	ClassifiedByClassifier = "classifier"
	ClassifiedByJev        = "jev"
)

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
	return &TurnClassification{Class: class, Evidence: evidence, Window: window, ToolCounts: maps.Clone(result.ToolCounts), By: ClassifiedByRule}
}
