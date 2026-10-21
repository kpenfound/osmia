package fixturekit

import (
	"encoding/json"
	"fmt"
	"github.com/kpenfound/osmia/internal/api"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// NotificationEntry is an open inbox entry of kind, told apart by n.
func NotificationEntry(kind string, n int) api.InboxEntry {
	e := api.InboxEntry{Kind: kind, Workstream: Stream, OpenedAt: time.Date(2026, 9, 1, 12, 0, n, 0, time.UTC), Question: fmt.Sprintf("Decide %s %d?\nIt matters.", kind, n), Recommendation: fmt.Sprintf("Recommend %d.", n)}
	switch kind {
	case api.InboxEscalation:
		e.Number = n
	case api.InboxContested:
		e.Unit, e.Recommendation = fmt.Sprintf("unit-%d", n), ""
	case api.InboxAmendment:
		e.Amendment, e.Revision = fmt.Sprint(n), 1
	default:
		e.Revision = n
	}
	return e
}
func SetWebhook(t *testing.T, root, webhook string) {
	t.Helper()
	path := filepath.Join(root, "config.toml")
	top, err := os.ReadFile(path)
	Must(t, err)
	text, _, _ := strings.Cut(string(top), "[notify]\n")
	if webhook != "" {
		text += "[notify]\nwebhook = " + fmt.Sprintf("%q", webhook) + "\n"
	}
	Must(t, os.WriteFile(path, []byte(text), 0600))
}
func ReadNotificationLedger[T any](t *testing.T, root string) T {
	t.Helper()
	var ledger T
	data, err := os.ReadFile(filepath.Join(root, "notifications.json"))
	if os.IsNotExist(err) {
		return ledger
	}
	Must(t, err)
	Must(t, json.Unmarshal(data, &ledger))
	return ledger
}
