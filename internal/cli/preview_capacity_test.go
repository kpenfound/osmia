package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/api"
)

func TestCapacityNamesWaitingPreview(t *testing.T) {
	var out bytes.Buffer
	showCapacity(&out, &api.CapacityStatus{Roles: []api.RoleCapacity{{Role: "checks", Limit: 1, Used: 1, Waiting: []api.SlotWait{{Workstream: "w_preview", Preview: true, Reason: "capacity"}}}}})
	if !strings.Contains(out.String(), "Waiting: w_preview preview (capacity)") {
		t.Fatal(out.String())
	}
}
