package intakefixture

import (
	"fmt"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"testing"
)

func AwaitWebhook(t *testing.T, w *fixturekit.Webhook, n int) []string {
	t.Helper()
	Soon(t, fmt.Sprintf("%d posts", n), func() bool { return len(w.Received()) >= n })
	return w.Received()
}
