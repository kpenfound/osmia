package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/service"
)

// TestDesignCommands covers the command-line surface of spec#6 to spec#8:
// opening, listing, reading and messaging a design session, and the
// "not drafted yet" and "busy" refusals of starting a workgroup before the
// agent has recorded a brief, or while a turn is running. The agent itself
// never runs in this test, since the fixture wires no engine; the brief and
// its handoff are covered end to end in internal/service, this unit's other
// surface.
func TestDesignCommands(t *testing.T) {
	root, project, _ := handInFixture(t)

	var opened service.DesignResponse
	must(t, json.Unmarshal([]byte(successful(t, root, "design", "open", string(project), "--json")), &opened))
	id := opened.Design.Workstream
	if opened.Design.Project != project {
		t.Fatalf("opened: %+v", opened.Design)
	}

	if text := successful(t, root, "design", "open", string(project)); !strings.Contains(text, "Design session ") || !strings.Contains(text, " opened in project "+string(project)) {
		t.Fatalf("open text: %q", text)
	}

	if text := successful(t, root, "design", "list"); !strings.Contains(text, string(id)) || !strings.Contains(text, "project "+string(project)) {
		t.Fatalf("list text: %q", text)
	}
	var listed service.DesignsResponse
	must(t, json.Unmarshal([]byte(successful(t, root, "design", "list", string(project), "--json")), &listed))
	if len(listed.Designs) != 2 {
		t.Fatalf("list json: %+v", listed.Designs)
	}

	if text := successful(t, root, "design", "read", string(id)); !strings.Contains(text, "no messages yet; send one with osmia design send "+string(id)) {
		t.Fatalf("read before any message: %q", text)
	}

	// No brief has been drafted yet: starting a workgroup is refused, with
	// nothing recorded for the architect.
	if code, out, diag := invoke(t, root, "design", "start", string(id)); code != 4 || out != "" || !strings.Contains(diag, "has not drafted a brief yet") {
		t.Fatalf("start before a draft: %d %q %q", code, out, diag)
	}

	var sent service.ConversationEntry
	must(t, json.Unmarshal([]byte(successful(t, root, "design", "send", string(id), "What should we build?", "--json")), &sent))
	if sent.Text != "What should we build?" {
		t.Fatalf("sent: %+v", sent)
	}
	if text := successful(t, root, "design", "read", string(id)); !strings.Contains(text, "What should we build?") {
		t.Fatalf("read after a message: %q", text)
	}

	// Nothing runs this fixture's agent, so the first message's turn is
	// still queued: a second message, and starting a workgroup, are both
	// refused as busy while it is outstanding, not queued behind it.
	if code, out, diag := invoke(t, root, "design", "send", string(id), "A second message"); code != 5 || out != "" || !strings.Contains(diag, "a turn is running for this design session") {
		t.Fatalf("send while busy: %d %q %q", code, out, diag)
	}
	if code, out, diag := invoke(t, root, "design", "start", string(id)); code != 5 || out != "" || !strings.Contains(diag, "a turn is running for this design session") {
		t.Fatalf("start while busy: %d %q %q", code, out, diag)
	}

	// An ordinary workstream, never a design session, is refused by the two
	// commands that only ever act on one: sending it a message and starting
	// a workgroup from it (design read shares the ordinary conversation
	// endpoint and so is not itself restricted to design sessions).
	other := handInStdin(t, root, project, "# Another feature\n")
	for _, args := range [][]string{
		{"design", "send", other, "hi"},
		{"design", "start", other},
	} {
		code, _, diag := invoke(t, root, args...)
		if code != 4 || !strings.Contains(diag, "not_found") {
			t.Fatalf("%v: %d %q", args, code, diag)
		}
	}

	for _, args := range [][]string{
		{"design"}, {"design", "open"}, {"design", "open", string(project), "x"}, {"design", "send", string(id)},
		{"design", "read"}, {"design", "start"}, {"design", "list", "a", "b"},
		{"design", "destroy"}, {"design", "destroy", string(id), "extra"}, {"design", "destroy", "not-an-id"},
	} {
		if code, _, _ := invoke(t, root, args...); code != 2 {
			t.Fatalf("%v: exit %d", args, code)
		}
	}
}
