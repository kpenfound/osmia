package web

import (
	"testing"
)

func TestBrowserChecksChoiceSendsTheChosenAnswerWithItsPush(t *testing.T) {
	t.Parallel()
	pin := `{"push":2,"pull_request":7,"pushed_commit":"abc123def456","operation":"review-2","offer":3}`
	entry := `{"kind":"checks-choice","project":"p_fake","workstream":"w_000","options":["keep_waiting","continue"],"question":"No check results are available for push 2.","asked":[],"answer":{"method":"POST","path":"/v1/checks-choice/w_000","body":` + pin + `}}`
	f := newDecisionButtonFixture(t, entry)
	card := `[data-decision][data-kind="checks-choice"] `
	assertDecisionButtons(f.p, card, map[string]string{"keep_waiting": "Keep waiting", "continue": "Continue without check results"})
	f.p.awaitText(card+"[data-field=pins]", "push 2")
	f.p.click(card + `[data-decision-choice="continue"]`)
	f.p.awaitGone("the answered card", card)
	assertDecisionRequest(t, f, "/v1/checks-choice/w_000", map[string]any{"push": float64(2), "pull_request": float64(7), "pushed_commit": "abc123def456", "operation": "review-2", "offer": float64(3), "decision": "continue"})
}
