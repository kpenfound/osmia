package plan

import (
	"fmt"
	"regexp"
	"strings"
)

// CheckAcceptanceProblems checks only newly submitted criteria and acceptance
// text. Prior text is exempt so revisions can carry legacy acceptance forward.
// Stored document validation deliberately does not call this admission check.
func CheckAcceptanceProblems(spec, graph, priorSpec, priorGraph string) []string {
	var out []string
	priorCriteria := map[string]int{}
	for _, c := range ParseSpec(priorSpec).Criteria {
		priorCriteria[c.Text]++
	}
	const advice = "requires the whole project check suite to pass or run; state the observable behaviour or the tests the unit adds instead (the service-owned check gate selects and runs checks)"
	for _, c := range ParseSpec(spec).Criteria {
		if priorCriteria[c.Text] > 0 {
			priorCriteria[c.Text]--
			continue
		}
		if wholeCheckObligation(c.Text) {
			out = append(out, (Problem{Kind: SpecProblem, Criterion: Cite(c.Number), Message: advice}).Error())
		}
	}
	before, _ := Parse([]byte(priorGraph))
	after, err := Parse([]byte(graph))
	if err != nil {
		return out
	}
	for _, u := range after.Units {
		priorItems := map[string]int{}
		if old, ok := before.Unit(u.ID); ok {
			for _, a := range old.Acceptance {
				priorItems[a]++
			}
		}
		for i, a := range u.Acceptance {
			if priorItems[a] > 0 {
				priorItems[a]--
				continue
			}
			if wholeCheckObligation(a) {
				out = append(out, (Problem{Kind: UnitProblem, Unit: u.ID, Message: fmt.Sprintf("acceptance item %d %s", i+1, advice)}).Error())
			}
		}
	}
	return out
}

var (
	checkExamples = regexp.MustCompile("\"([^\"]*)\"|“([^”]*)”|‘([^’]*)’|(?:^|[[:space:](])'([^'\\n]*)'|`([^`]*)`")
	// A period separates sentences only before whitespace or at the end, so
	// relative package paths and dotted package names stay with their scopes.
	checkSentences    = regexp.MustCompile(`[!?;\n]+|\.(?:[[:space:]]|$)`)
	checkNegation     = regexp.MustCompile(`\b(?:never|not|no|without|cannot|can't|mustn't|needn't|[a-z]+n['’]t|avoid\w*|disallow\w*|prohibit\w*|forbid\w*|reject\w*)\b`)
	checkMention      = regexp.MustCompile(`\b(?:wording|validator|detector|reviewer|prompt|instructions|policy|guard|invalid|defect|examples?|quotes?|describes?|mentions?)\b`)
	checkTestMention  = regexp.MustCompile(`\b(?:table\s+)?tests?\s+(?:flag|reject|cover|detect|quote|describe|prohibit)\w*\b`)
	checkSelection    = regexp.MustCompile(`--(?:go-|package|test|filter|include|exclude)|dag(?:\+check)?://`)
	wholeCheckSubject = `(?:\bdagger checks?\b|\b(?:all|every)\s+(?:(?:the|project's|project)\s+)?(?:checks?|tests?|(?:dagger\s+checks?))\b|\b(?:the\s+)?(?:full|entire|complete|whole)\s+(?:project\s+)?(?:(?:check|test)\s+suite|(?:check|test)\s+run|checks|tests)\b)`
	checkPassing      = regexp.MustCompile(wholeCheckSubject + "`?" + `\s+(?:(?:must|shall|should|will|to)\s+)?(?:(?:all|still|keep)\s+)?(?:pass(?:es)?|succeed(?:s)?|(?:is|are|be|remain|stays?)\s+green|(?:be\s+)?run|(?:is|are)\s+(?:run|required|passing)|(?:has|have)\s+(?:run|passed))\b`)
	checkRunning      = regexp.MustCompile(`\b(?:run|runs|running|execute|executes|executing)\s+` + "`?" + wholeCheckSubject)
	checkScope        = regexp.MustCompile("^`?" + `\s+(?:in|for|from|under|of)\s+(?:the\s+)?` + "`?" + `((?:\.\.?/)*[a-z0-9_][a-z0-9_./-]*)\b`)
	checkGlobalScope  = regexp.MustCompile(`^(?:all|every|full|entire|complete|whole|project|repository|repo|workspace|codebase|ci|parallel|sequence|container)$`)
)

// wholeCheckObligation is intentionally narrow: uncertain prose is left to
// planning judgment, rather than rejecting descriptions of the rule itself.
func wholeCheckObligation(text string) bool {
	text = checkExamples.ReplaceAllStringFunc(strings.ToLower(text), func(quoted string) string {
		for _, content := range checkExamples.FindStringSubmatch(quoted)[1:] {
			if content == "" {
				continue
			}
			// A complete run or pass statement is quoted content. A command
			// or subject alone remains part of the surrounding requirement:
			// Run `dagger check` and `all checks` pass are obligations.
			if checkRunning.MatchString(content) || checkPassing.MatchString(content) {
				return " "
			}
			return " " + content + " "
		}
		return " "
	})
	for _, sentence := range checkSentences.Split(text, -1) {
		if checkNegation.MatchString(sentence) || checkMention.MatchString(sentence) || checkTestMention.MatchString(sentence) || checkSelection.MatchString(sentence) {
			continue
		}
		matches := append(checkPassing.FindAllStringIndex(sentence, -1), checkRunning.FindAllStringIndex(sentence, -1)...)
		for _, match := range matches {
			// A following package or test-set scope belongs to this check's
			// subject. Project-wide and execution-location qualifiers do not
			// narrow the tests, and another unscoped check remains an obligation.
			scope := checkScope.FindStringSubmatch(sentence[match[1]:])
			if scope == nil || checkGlobalScope.MatchString(scope[1]) {
				return true
			}
		}
	}
	return false
}
