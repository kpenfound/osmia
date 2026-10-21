package intakefixture

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
)

type InboxClient interface {
	Inbox(context.Context) (api.InboxResponse, error)
}

// decisionCard is the selector of the page's card of an inbox entry.
func DecisionCard(e api.InboxEntry) string {
	return fmt.Sprintf(`[data-decision="%s"] `, strings.Join([]string{e.Kind, string(e.Project), string(e.Workstream), strconv.Itoa(e.Number), e.Unit, e.Amendment}, ":"))
}

// entriesOf returns the listed inbox entries of one kind.
func EntriesOf(t *testing.T, c InboxClient, kind string) []api.InboxEntry {
	t.Helper()
	inbox, err := c.Inbox(context.Background())
	fixturekit.Must(t, err)
	var out []api.InboxEntry
	for _, e := range inbox.Entries {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}
