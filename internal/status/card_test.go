package status

import (
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/coreadapter"
)

func TestCheckCardNormalisesAndChecksEveryField(t *testing.T) {
	t.Parallel()
	input := coreadapter.Card{Headline: "  Uploads\t resume\u200b ", Happened: "Chunks\u0000  resume\u00a0 cleanly ", NeedsYou: " Review\t the candidate. "}
	got, err := CheckCard(input, nil)
	if err != nil || got != (coreadapter.Card{Headline: "Uploads resume", Happened: "Chunks resume cleanly", NeedsYou: "Review the candidate."}) {
		t.Fatalf("card %+v, %v", got, err)
	}
	for _, tc := range []struct {
		name, reason string
		edit         func(*coreadapter.Card)
	}{
		{"headline missing", "headline is required", func(c *coreadapter.Card) { c.Headline = " \u200b " }},
		{"happened missing", "happened is required", func(c *coreadapter.Card) { c.Happened = " " }},
		{"headline limit", "headline must be at most 64 characters", func(c *coreadapter.Card) { c.Headline = strings.Repeat("é", 65) }},
		{"happened limit", "happened must be at most 140 characters", func(c *coreadapter.Card) { c.Happened = strings.Repeat("x", 141) }},
		{"needs limit", "needs_you must be at most 140 characters", func(c *coreadapter.Card) { c.NeedsYou = strings.Repeat("x", 141) }},
		{"headline line", "headline must be a single line", func(c *coreadapter.Card) { c.Headline = "One\nTwo" }},
		{"happened line", "happened must be a single line", func(c *coreadapter.Card) { c.Happened = "One\rTwo" }},
		{"needs line", "needs_you must be a single line", func(c *coreadapter.Card) { c.NeedsYou = "One\u2028Two" }},
		{"vertical line", "happened must be a single line", func(c *coreadapter.Card) { c.Happened = "One\vTwo" }},
		{"headline identifier", "headline contains a file name", func(c *coreadapter.Card) { c.Headline = "Review parser.go" }},
		{"happened identifier", "happened contains an Osmia or backend identifier", func(c *coreadapter.Card) { c.Happened = "Completed turn-9" }},
		{"needs identifier", "needs_you contains a URL", func(c *coreadapter.Card) { c.NeedsYou = "Read https://example.com" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			card := coreadapter.Card{Headline: "Uploads resume", Happened: "Chunks resume cleanly", NeedsYou: "Review the candidate."}
			tc.edit(&card)
			got, err := CheckCard(card, []string{"turn-9"})
			if err == nil || !strings.Contains(err.Error(), tc.reason) || got != (coreadapter.Card{}) {
				t.Fatalf("card %+v, err %v; want %s", got, err, tc.reason)
			}
		})
	}
	if got, err := CheckCard(coreadapter.Card{Headline: "Done", Happened: "The upload resumed."}, nil); err != nil || got.NeedsYou != "" {
		t.Fatalf("empty needs_you: %+v, %v", got, err)
	}
	boundary := coreadapter.Card{Headline: strings.Repeat("h", 64), Happened: strings.Repeat("d", 140), NeedsYou: strings.Repeat("n", 140)}
	if got, err := CheckCard(boundary, nil); err != nil || got != boundary {
		t.Fatalf("boundary card: %+v, %v", got, err)
	}
}
