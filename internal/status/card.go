package status

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/kpenfound/osmia/internal/coreadapter"
)

// Card field limits, in characters.
const (
	headlineLimit = 64
	happenedLimit = 140
	needsYouLimit = 140
)

// ellipsis ends a card field the service shortened. It counts toward the
// field's limit.
const ellipsis = "…"

const lineBreaks = "\r\n\v\f\u0085\u2028\u2029"

// normalise returns text as one line of single-spaced characters without
// control or format characters.
func normalise(text string) string {
	var b strings.Builder
	for _, r := range text {
		if unicode.IsSpace(r) {
			b.WriteByte(' ')
			continue
		}
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			continue
		}
		b.WriteRune(r)
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// CheckCard normalises an owner-facing turn card and refuses fields that
// cannot be shown as concise prose. A refusal never returns a partial card.
func CheckCard(card coreadapter.Card, known []string) (coreadapter.Card, error) {
	fields := []struct {
		name     string
		value    *string
		limit    int
		required bool
	}{
		{"headline", &card.Headline, headlineLimit, true},
		{"happened", &card.Happened, happenedLimit, true},
		{"needs_you", &card.NeedsYou, needsYouLimit, false},
	}
	for _, field := range fields {
		if strings.ContainsAny(*field.value, lineBreaks) {
			return coreadapter.Card{}, fmt.Errorf("%s must be a single line", field.name)
		}
		*field.value = normalise(*field.value)
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

// FittedCard is a mason's card made to fit the rules of CheckCard.
type FittedCard struct {
	// Card is the card to store and show. It is nil when the headline or
	// happened field is missing or cannot be shown.
	Card *coreadapter.Card
	// NeedsYou is the needs_you field after fitting, whether or not the card
	// survived. It is empty when the mason gave none or it cannot be shown.
	NeedsYou string
	// OwnerAction reports whether the mason gave a non-empty needs_you,
	// whatever survived of it.
	OwnerAction bool
	// Adjustments says, one entry per change, what the service did to make
	// the card fit.
	Adjustments []string
}

// FitCard makes a mason's card fit instead of refusing it. A field over its
// limit is shortened, ending at a word boundary when one falls inside the
// limit and with an ellipsis that counts toward the limit. A field over
// several lines is joined into one line. A field that holds an identifier, a
// path, a URL or a model name is left out, and so is the whole card when its
// headline or happened field is missing or left out. The card it returns
// always meets the rules of CheckCard.
func FitCard(card coreadapter.Card, known []string) FittedCard {
	var fitted FittedCard
	headline, notes := fitField("headline", card.Headline, headlineLimit, known)
	fitted.Adjustments = append(fitted.Adjustments, notes...)
	happened, notes := fitField("happened", card.Happened, happenedLimit, known)
	fitted.Adjustments = append(fitted.Adjustments, notes...)
	needsYou, notes := fitField("needs_you", card.NeedsYou, needsYouLimit, known)
	fitted.Adjustments = append(fitted.Adjustments, notes...)
	fitted.NeedsYou = needsYou
	fitted.OwnerAction = normalise(card.NeedsYou) != ""

	var missing []string
	if headline == "" {
		missing = append(missing, "headline")
	}
	if happened == "" {
		missing = append(missing, "happened")
	}
	if len(missing) != 0 {
		fitted.Adjustments = append(fitted.Adjustments, fmt.Sprintf("no card recorded: %s cannot be shown", strings.Join(missing, " and ")))
		return fitted
	}
	checked, err := CheckCard(coreadapter.Card{Headline: headline, Happened: happened, NeedsYou: needsYou}, known)
	if err != nil {
		fitted.Adjustments = append(fitted.Adjustments, fmt.Sprintf("no card recorded: %v", err))
		return fitted
	}
	fitted.Card = &checked
	return fitted
}

// fitField fits one card field to its limit. It returns "" for a field that
// is empty or cannot be shown, and what it changed.
func fitField(name, value string, limit int, known []string) (string, []string) {
	var notes []string
	if strings.ContainsAny(value, lineBreaks) {
		notes = append(notes, name+" joined into one line")
	}
	text := normalise(value)
	if text == "" {
		if strings.TrimSpace(value) != "" {
			notes = append(notes, name+" left out: it holds no printable text")
		}
		return "", notes
	}
	if err := identifiers(name, text, known); err != nil {
		return "", append(notes, fmt.Sprintf("%s left out: %s", name, strings.TrimPrefix(err.Error(), name+" ")))
	}
	if n := utf8.RuneCountInString(text); n > limit {
		text = shorten(text, limit)
		if err := identifiers(name, text, known); err != nil {
			return "", append(notes, fmt.Sprintf("%s left out: %s", name, strings.TrimPrefix(err.Error(), name+" ")))
		}
		notes = append(notes, fmt.Sprintf("%s shortened from %d to %d characters", name, n, utf8.RuneCountInString(text)))
	}
	return text, notes
}

// shorten returns text cut to at most limit characters, the last of which is
// an ellipsis. It cuts at a word boundary when one falls inside the limit.
func shorten(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	keep := runes[:limit-1]
	cut := len(keep)
	if !unicode.IsSpace(runes[limit-1]) {
		for i := len(keep) - 1; i > 0; i-- {
			if keep[i] == ' ' {
				cut = i
				break
			}
		}
	}
	return strings.TrimRight(string(keep[:cut]), " ,;:") + ellipsis
}
