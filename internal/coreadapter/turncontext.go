package coreadapter

import "strings"

// TurnContextHeader and TurnContextFooter frame the block that opens a turn's
// prompt. The block carries what changes while a conversation goes on, so the
// system prompt a resumed session began with never has to change. Earlier
// turns of the same conversation hold older copies of the block; the header
// says the newest one governs.
const (
	TurnContextHeader = "Current turn context. The service rebuilds this block for every turn. Where it differs from a block in an earlier turn of this conversation, follow this one."
	TurnContextFooter = "End of current turn context."
)

// TurnContext renders the sections as one labelled block followed by a blank
// line, to place before a turn's prompt. Empty sections are dropped, and no
// section at all renders nothing.
func TurnContext(sections ...string) string {
	var kept []string
	for _, s := range sections {
		if s = strings.TrimSpace(s); s != "" {
			kept = append(kept, s)
		}
	}
	if len(kept) == 0 {
		return ""
	}
	return TurnContextHeader + "\n\n" + strings.Join(kept, "\n\n") + "\n\n" + TurnContextFooter + "\n\n"
}
