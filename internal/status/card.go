package status

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/kpenfound/osmia/internal/coreadapter"
)

// CheckCard normalises an owner-facing turn card and refuses fields that
// cannot be shown as concise prose. A refusal never returns a partial card.
func CheckCard(card coreadapter.Card, known []string) (coreadapter.Card, error) {
	fields := []struct {
		name     string
		value    *string
		limit    int
		required bool
	}{
		{"headline", &card.Headline, 64, true},
		{"happened", &card.Happened, 140, true},
		{"needs_you", &card.NeedsYou, 140, false},
	}
	for _, field := range fields {
		if strings.ContainsAny(*field.value, "\r\n\v\f\u0085\u2028\u2029") {
			return coreadapter.Card{}, fmt.Errorf("%s must be a single line", field.name)
		}
		var b strings.Builder
		for _, r := range *field.value {
			if unicode.IsSpace(r) {
				b.WriteByte(' ')
				continue
			}
			if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
				continue
			}
			b.WriteRune(r)
		}
		*field.value = strings.Join(strings.Fields(b.String()), " ")
		if field.required && *field.value == "" {
			return coreadapter.Card{}, fmt.Errorf("%s is required", field.name)
		}
		if utf8.RuneCountInString(*field.value) > field.limit {
			return coreadapter.Card{}, fmt.Errorf("%s must be at most %d characters", field.name, field.limit)
		}
		if err := identifiers(field.name, *field.value, known); err != nil {
			return coreadapter.Card{}, err
		}
	}
	return card, nil
}
