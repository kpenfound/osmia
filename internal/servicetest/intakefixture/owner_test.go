package intakefixture

import (
	"context"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
)

type settlingUnitStatus struct {
	detail api.WorkstreamStatus
	list   api.StatusResponse
}

func (c *settlingUnitStatus) Status(context.Context, config.WorkstreamID) (api.WorkstreamStatus, error) {
	return c.detail, nil
}

func (c *settlingUnitStatus) Statuses(context.Context) (api.StatusResponse, error) {
	return c.list, nil
}

func TestCheckUnitsWaitsForDetailAndCachedList(t *testing.T) {
	t.Parallel()
	const stream config.WorkstreamID = "w_status"
	building := "building"
	want := []api.UnitStatus{{Unit: "resume", State: "implementing"}}
	client := &settlingUnitStatus{
		detail: api.WorkstreamStatus{Workstream: stream, State: &building, Units: []api.UnitStatus{{Unit: "resume", State: "ready"}}},
	}
	waits := 0
	CheckUnits(t, client, func(t *testing.T, err error, _ time.Duration) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}, func() {
		waits++
		switch waits {
		case 1:
			// Detail is current before the list includes the workstream.
			client.detail.Units = want
		case 2:
			// A published list may still hold the previous unit state.
			client.list.Workstreams = []api.WorkstreamStatus{{Workstream: stream, Units: []api.UnitStatus{{Unit: "resume", State: "ready"}}}}
		case 3:
			client.list.Workstreams[0].Units = want
		default:
			t.Fatal("status fixture did not accept the converged endpoints")
		}
	}, stream, want)
	if waits != 3 {
		t.Fatalf("accepted stale status after %d waits", waits)
	}
}
