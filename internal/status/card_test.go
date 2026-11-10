package status

import (
	"strings"
	"testing"
	"unicode/utf8"

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

func TestFitCardShortensAtAWordBoundaryWithAnEllipsis(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("Chunks resume cleanly ", 20)
	for _, tc := range []struct {
		name  string
		card  coreadapter.Card
		field func(coreadapter.Card) string
		limit int
	}{
		{"headline", coreadapter.Card{Headline: long, Happened: "Done."}, func(c coreadapter.Card) string { return c.Headline }, 64},
		{"happened", coreadapter.Card{Headline: "Done", Happened: long}, func(c coreadapter.Card) string { return c.Happened }, 140},
		{"needs_you", coreadapter.Card{Headline: "Done", Happened: "Done.", NeedsYou: long}, func(c coreadapter.Card) string { return c.NeedsYou }, 140},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := FitCard(tc.card, nil)
			if got.Card == nil || len(got.Adjustments) != 1 || !strings.Contains(got.Adjustments[0], tc.name+" shortened from") {
				t.Fatalf("fitted %+v", got)
			}
			field := tc.field(*got.Card)
			body := strings.TrimSuffix(field, "…")
			n := utf8.RuneCountInString(field)
			if n > tc.limit || n < tc.limit-len("Chunks resume cleanly") || body == field || !strings.HasSuffix(body, "cleanly") && !strings.HasSuffix(body, "resume") && !strings.HasSuffix(body, "Chunks") {
				t.Fatalf("%s %q (%d characters)", tc.name, field, n)
			}
			if _, err := CheckCard(*got.Card, nil); err != nil {
				t.Fatalf("the fitted card is refused: %v", err)
			}
		})
	}
}

func TestFitCardCutsAWordWithNoBoundaryAndCountsCharacters(t *testing.T) {
	t.Parallel()
	got := FitCard(coreadapter.Card{Headline: strings.Repeat("é", 100), Happened: "Done."}, nil)
	if got.Card == nil || got.Card.Headline != strings.Repeat("é", 63)+"…" {
		t.Fatalf("fitted %+v", got)
	}
	exact := coreadapter.Card{Headline: strings.Repeat("h", 64), Happened: strings.Repeat("d", 140), NeedsYou: strings.Repeat("n", 140)}
	if got := FitCard(exact, nil); got.Card == nil || *got.Card != exact || len(got.Adjustments) != 0 {
		t.Fatalf("a card at its limits changed: %+v", got)
	}
	// The text is cut back to the last word boundary inside the limit.
	got = FitCard(coreadapter.Card{Headline: strings.Repeat("a", 62) + " bbbbbb", Happened: "Done."}, nil)
	if got.Card == nil || got.Card.Headline != strings.Repeat("a", 62)+"…" {
		t.Fatalf("fitted %+v", got)
	}
	// A word that ends exactly where the text is cut is kept whole.
	got = FitCard(coreadapter.Card{Headline: strings.Repeat("a", 63) + " bbb", Happened: "Done."}, nil)
	if got.Card == nil || got.Card.Headline != strings.Repeat("a", 63)+"…" {
		t.Fatalf("fitted %+v", got)
	}
}

func TestFitCardJoinsLinesAndLeavesOutWhatCannotBeShown(t *testing.T) {
	t.Parallel()
	got := FitCard(coreadapter.Card{Headline: "Uploads\nresume", Happened: "One\r\nTwo\u2028Three", NeedsYou: "Read https://example.com"}, nil)
	if got.Card == nil || *got.Card != (coreadapter.Card{Headline: "Uploads resume", Happened: "One Two Three"}) || got.NeedsYou != "" || !got.OwnerAction || len(got.Adjustments) != 3 {
		t.Fatalf("fitted %+v", got)
	}
	for _, field := range []string{"headline", "happened"} {
		card := coreadapter.Card{Headline: "Uploads resume", Happened: "Chunks resume.", NeedsYou: "Review the candidate."}
		switch field {
		case "headline":
			card.Headline = "See /etc/hosts"
		case "happened":
			card.Happened = ""
		}
		got := FitCard(card, nil)
		if got.Card != nil || got.NeedsYou != "Review the candidate." || !got.OwnerAction {
			t.Fatalf("%s unusable: %+v", field, got)
		}
		if last := got.Adjustments[len(got.Adjustments)-1]; !strings.HasPrefix(last, "no card recorded: "+field) {
			t.Fatalf("%s unusable: adjustments %v", field, got.Adjustments)
		}
	}
	for _, card := range []coreadapter.Card{{}, {NeedsYou: " \u200b "}} {
		if got := FitCard(card, nil); got.Card != nil || got.OwnerAction || got.NeedsYou != "" {
			t.Fatalf("empty card %+v: %+v", card, got)
		}
	}
	if got := FitCard(coreadapter.Card{Headline: "Done", Happened: "Completed turn-9"}, []string{"turn-9"}); got.Card != nil {
		t.Fatalf("a known identifier was kept: %+v", got)
	}
}
