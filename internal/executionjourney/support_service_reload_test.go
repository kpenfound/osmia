package executionjourney

import (
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	testing "testing"
)

func reloadFails(t *testing.T, c *Client, path, field string) {
	t.Helper()
	intakefixture.ReloadFails(t, c.Reload, path, field)
}
