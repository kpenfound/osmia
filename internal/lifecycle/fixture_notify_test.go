package lifecycle_test

import (
	fixturekit "github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	testing "testing"
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

func setWebhook(t *testing.T, opts Options, webhook string) {
	t.Helper()
	fixturekit.SetWebhook(t, opts.Config.Root, webhook)
}

func readLedger(t *testing.T, opts Options) notifyLedger {
	t.Helper()
	return fixturekit.ReadNotificationLedger[notifyLedger](t, opts.Config.Root)
}
