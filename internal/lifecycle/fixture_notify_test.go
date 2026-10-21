package lifecycle_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
)

// fakeWebhook records the bodies posted to it and answers with status. It
// stands in for the owner's real webhook endpoint, leaving that endpoint's
// actual network behaviour and availability unverified.
type fakeWebhook struct{ *fixturekit.Webhook }

func newFakeWebhook(t *testing.T) *fakeWebhook { return &fakeWebhook{fixturekit.NewWebhook(t)} }
func (w *fakeWebhook) records(opts Options) *fakeWebhook {
	w.Webhook.Records(opts.Config.Root)
	return w
}
func (w *fakeWebhook) hook() string { return w.Hook() }

func (w *fakeWebhook) received() []string { return w.Received() }

// setWebhook rewrites the fixture's top-level file with notify.webhook set,
// or without a [notify] table when webhook is empty.
func setWebhook(t *testing.T, opts Options, webhook string) {
	t.Helper()
	path := filepath.Join(opts.Config.Root, "config.toml")
	top, err := os.ReadFile(path)
	must(t, err)
	text, _, _ := strings.Cut(string(top), "[notify]\n")
	if webhook != "" {
		text += "[notify]\nwebhook = " + fmt.Sprintf("%q", webhook) + "\n"
	}
	must(t, os.WriteFile(path, []byte(text), 0600))
}
func readLedger(t *testing.T, opts Options) notifyLedger {
	t.Helper()
	var l notifyLedger
	data, err := os.ReadFile(filepath.Join(opts.Config.Root, "notifications.json"))
	if os.IsNotExist(err) {
		return l
	}
	must(t, err)
	must(t, json.Unmarshal(data, &l))
	return l
}
