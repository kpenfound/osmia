package lifecycle_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestAChangeTheManagerRaisedIsReportedUnderItsDisplayName(t *testing.T) {
	t.Parallel()
	actor := trace.Actor{Kind: "agent", ID: trace.Manager}
	c := lifecycle.WorkgroupChange{ID: "contract-1", Requester: &trace.ProjectAmendment{ID: "1", Role: trace.Manager, Requester: actor}}

	out, err := json.Marshal(c.ToolResult())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"role":"manager"`) || !strings.Contains(string(out), `"id":"manager"`) || strings.Contains(string(out), trace.Manager) {
		t.Fatalf("result %s", out)
	}
	if c.Requester.Role != trace.Manager || c.Requester.Requester != actor {
		t.Fatalf("the recorded requester changed: %+v", c.Requester)
	}
}
