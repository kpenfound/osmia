package intake

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/trace"
)

func TestAnAmendmentTheManagerFiledAndDecidedIsShownAsTheManagers(t *testing.T) {
	t.Parallel()
	request := trace.Amendment{Header: trace.Header{ID: "1", Actor: ManagerActor}, Requester: ManagerActor, Role: trace.Manager, Thread: trace.Manager, Citations: []string{"spec#1"}}
	content, err := json.MarshalIndent(amendmentPacket{Round: 1, Request: request, Affected: json.RawMessage(`[]`), Reply: json.RawMessage(`{}`)}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	c := AmendmentCase{Request: request, Packet: trace.Document{Header: trace.Header{Revision: 1}, Content: string(content)}, Decisions: []AmendmentDecision{{Decision: AmendmentApprove, Actor: trace.Manager}}}

	out := c.Response("project", "stream", "")
	if out.Decision == nil || out.Decision.Actor != trace.ManagerDisplay {
		t.Fatalf("decision %+v", out.Decision)
	}
	var packet amendmentPacket
	if err := json.Unmarshal(out.Packet, &packet); err != nil {
		t.Fatal(err)
	}
	if r := packet.Request; r.Role != trace.ManagerDisplay || r.Requester.ID != trace.ManagerDisplay || r.Actor.ID != trace.ManagerDisplay || strings.Contains(string(out.Packet), trace.Manager) {
		t.Fatalf("packet %s", out.Packet)
	}
	if c.Decisions[0].Actor != trace.Manager || c.Packet.Content != string(content) {
		t.Fatalf("the recorded decision and packet changed: %+v %s", c.Decisions[0], c.Packet.Content)
	}
	if prompt := AmendmentRulingPrompt(request, c.Decisions[0], trace.Manager, "applied"); !strings.Contains(prompt, "Actor: manager.") || strings.Contains(prompt, trace.Manager) {
		t.Fatalf("prompt %s", prompt)
	}

	owned := `{"round":1}` + "\n"
	c.Packet.Content = owned
	if got := c.Response("project", "stream", "").Packet; string(got) != owned {
		t.Fatalf("a packet without the manager was rewritten: %s", got)
	}
}
