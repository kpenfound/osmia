package intakefixture

import (
	"fmt"
	"testing"

	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
)

func AwaitWebhook(t *testing.T, w *fixturekit.Webhook, n int) []string {
	t.Helper()
	Soon(t, fmt.Sprintf("%d posts", n), func() bool { return len(w.Received()) >= n })
	return w.Received()
}
