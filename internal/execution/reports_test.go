package execution

import (
	testing "testing"

	coreadapter "github.com/kpenfound/osmia/internal/coreadapter"
)

func TestFinishNoticeWithOptionalOwnerAction(t *testing.T) {
	t.Parallel()
	base := "Unit parser is checking: its mason reported done on turn first; its report is units/parser/report.json revision 2."
	for _, tc := range []struct {
		card *coreadapter.Card
		want string
	}{
		{nil, base},
		{&coreadapter.Card{Headline: "Parser is ready"}, base + " Headline: Parser is ready"},
		{&coreadapter.Card{Headline: "Parser is ready", NeedsYou: "Review the candidate."}, base + " Headline: Parser is ready; Needs you: Review the candidate."},
	} {
		if got := finishNotice("parser", "first", "units/parser/report.json", 2, tc.card); got != tc.want {
			t.Fatalf("notice %q, want %q", got, tc.want)
		}
	}
}
