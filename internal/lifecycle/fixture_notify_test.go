package lifecycle_test

import (
	testing "testing"

	"github.com/kpenfound/osmia/internal/service"
	fixturekit "github.com/kpenfound/osmia/internal/servicetest/fixturekit"
)

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

func readLedger(t *testing.T, opts Options) service.TestNotifyLedger {
	t.Helper()
	return fixturekit.ReadNotificationLedger[service.TestNotifyLedger](t, opts.Config.Root)
}
