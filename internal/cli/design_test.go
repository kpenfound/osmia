package cli

import (
	"encoding/json"
	"strings"
	"testing"

	wire "github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
)

func openDesign(t *testing.T) (root string, project config.ProjectID, id config.WorkstreamID) {
	t.Helper()
	root, project, _ = handInFixture(t)
	var opened wire.DesignResponse
	must(t, json.Unmarshal([]byte(successful(t, root, "design", "open", string(project), "--json")), &opened))
	return root, project, opened.Design.Workstream
}

func TestDesignOpenReportsTheSessionAsJSON(t *testing.T) {
	root, project, _ := handInFixture(t)
	var opened wire.DesignResponse
	must(t, json.Unmarshal([]byte(successful(t, root, "design", "open", string(project), "--json")), &opened))
	if opened.Design.Project != project || opened.Design.Workstream == "" {
		t.Fatalf("opened: %+v", opened.Design)
	}
}

func TestDesignOpenNamesTheSessionAndProject(t *testing.T) {
	root, project, _ := handInFixture(t)
	if text := successful(t, root, "design", "open", string(project)); !strings.Contains(text, "Design session ") || !strings.Contains(text, " opened in project "+string(project)) {
		t.Fatalf("open text: %q", text)
	}
}

func TestDesignListNamesEverySessionAndItsProject(t *testing.T) {
	root, project, id := openDesign(t)
	if text := successful(t, root, "design", "list"); !strings.Contains(text, string(id)) || !strings.Contains(text, "project "+string(project)) {
		t.Fatalf("list text: %q", text)
	}
}

func TestDesignListJSONHoldsEverySession(t *testing.T) {
	root, project, _ := openDesign(t)
	successful(t, root, "design", "open", string(project))
	var listed wire.DesignsResponse
	must(t, json.Unmarshal([]byte(successful(t, root, "design", "list", string(project), "--json")), &listed))
	if len(listed.Designs) != 2 {
		t.Fatalf("list json: %+v", listed.Designs)
	}
}

func TestDesignReadBeforeAnyMessageSaysHowToSendOne(t *testing.T) {
	root, _, id := openDesign(t)
	if text := successful(t, root, "design", "read", string(id)); !strings.Contains(text, "no messages yet; send one with osmia design send "+string(id)) {
		t.Fatalf("read before any message: %q", text)
	}
}

func TestDesignStartRefusesBeforeABriefIsDrafted(t *testing.T) {
	root, _, id := openDesign(t)
	if code, out, diag := invoke(t, root, "design", "start", string(id)); code != 4 || out != "" || !strings.Contains(diag, "has not drafted a brief yet") {
		t.Fatalf("start before a draft: %d %q %q", code, out, diag)
	}
}

func TestDesignSendReportsTheMessageAsJSON(t *testing.T) {
	root, _, id := openDesign(t)
	var sent wire.ConversationEntry
	must(t, json.Unmarshal([]byte(successful(t, root, "design", "send", string(id), "What should we build?", "--json")), &sent))
	if sent.Text != "What should we build?" {
		t.Fatalf("sent: %+v", sent)
	}
}

func TestDesignReadShowsASentMessage(t *testing.T) {
	root, _, id := openDesign(t)
	successful(t, root, "design", "send", string(id), "What should we build?")
	if text := successful(t, root, "design", "read", string(id)); !strings.Contains(text, "What should we build?") {
		t.Fatalf("read after a message: %q", text)
	}
}

func TestDesignSendRefusesWhileATurnRuns(t *testing.T) {
	root, _, id := openDesign(t)
	successful(t, root, "design", "send", string(id), "What should we build?")
	if code, out, diag := invoke(t, root, "design", "send", string(id), "A second message"); code != 5 || out != "" || !strings.Contains(diag, "a turn is running for this design session") {
		t.Fatalf("send while busy: %d %q %q", code, out, diag)
	}
}

func TestDesignStartRefusesWhileATurnRuns(t *testing.T) {
	root, _, id := openDesign(t)
	successful(t, root, "design", "send", string(id), "What should we build?")
	if code, out, diag := invoke(t, root, "design", "start", string(id)); code != 5 || out != "" || !strings.Contains(diag, "a turn is running for this design session") {
		t.Fatalf("start while busy: %d %q %q", code, out, diag)
	}
}

func TestDesignCommandsRefuseAWorkstreamThatIsNotADesignSession(t *testing.T) {
	root, project, _ := handInFixture(t)
	other := handInStdin(t, root, project, "# Another feature\n")
	for _, args := range [][]string{
		{"design", "send", other, "hi"},
		{"design", "read", other},
		{"design", "start", other},
	} {
		code, _, diag := invoke(t, root, args...)
		if code != 4 || !strings.Contains(diag, "not_found") {
			t.Fatalf("%v: %d %q", args, code, diag)
		}
	}
}

func TestDesignCommandsRefuseInvalidArguments(t *testing.T) {
	root, project, id := openDesign(t)
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
