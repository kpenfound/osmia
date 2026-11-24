package executionjourney

import (
	testing "testing"

	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
)

func reloadFails(t *testing.T, c *Client, path, field string) {
	t.Helper()
	intakefixture.ReloadFails(t, c.Reload, path, field)
}
