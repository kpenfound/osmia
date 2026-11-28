package api

import (
	"encoding/json"
	"testing"

	"github.com/kpenfound/osmia/internal/trace"
)

func TestAContestReportsTheManagersRulingsLeftAsAManagerField(t *testing.T) {
	body, err := json.Marshal(ActivityContest{ID: "c1", Reason: "checks", Rulings: []string{"review"}, ManagerRulingsLeft: 2})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatal(err)
	}
	if fields["manager_rulings_left"] != float64(2) {
		t.Fatalf("contest fields %v, want manager_rulings_left 2", fields)
	}
}

func TestTheManagerArchivesUnderItsDisplayName(t *testing.T) {
	if ArchivedByManager != trace.ManagerDisplay || ArchivedByManager == trace.Manager {
		t.Fatalf("ArchivedByManager = %q, want %q", ArchivedByManager, trace.ManagerDisplay)
	}
}
