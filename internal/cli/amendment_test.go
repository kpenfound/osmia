package cli

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
)

// withAmendment creates a project trace whose building workstream has
// amendment 1 presented to the owner, as a stopped service left it.
func withAmendment(t *testing.T) service.Options {
	t.Helper()
	ctx := context.Background()
	opts := fixture(t)
	cfg, err := config.Load(opts.Config)
	must(t, err)
	cmd := exec.Command("git", "init", "--quiet", cfg.Project.Clone)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	owner := trace.Actor{Kind: "owner", ID: "local"}
	repo, err := trace.Create(ctx, cfg.Root, cfg.Project, written, owner)
	must(t, err)
	must(t, repo.CreateWorkstream(ctx, stream, written, owner))
	architect := trace.Actor{Kind: "agent", ID: "agent_architect"}
	document := func(id, path, content string) trace.Document {
		return trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: id, Revision: 1, Project: project, Workstream: stream, At: written, Actor: architect, Cause: "planted"}, Path: path, Content: content}
	}
	sealed, err := seal.Encode(seal.Seal{Version: seal.Version, Seal: 1, Round: 1, Revision: shed.Pin{Spec: 1, Plan: 1}, SpecHash: seal.SpecHash(shedSpec),
		Base: seal.Base{Remote: "upstream", Branch: "main", Commit: "0123456789abcdef0123456789abcdef01234567"}, Branch: "osmia/" + stream, Workspace: "/nowhere",
		Footprints: []seal.Footprint{{Unit: "resume", Entities: []string{"internal.store"}, Paths: []string{"internal/store/**"}}}})
	must(t, err)
	must(t, repo.RecordDocuments(ctx, []trace.Document{document(plan.SpecDocument, plan.SpecPath, shedSpec), document(plan.PlanDocument, plan.PlanPath, shedPlan), document(seal.DocumentID, seal.Path, string(sealed))}))
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "building", Revision: 1, Project: project, Workstream: stream, At: written, Actor: owner, Cause: "planted"}
	_, err = repo.SetFeatureState(ctx, h, "building", "planted")
	must(t, err)
	mason := trace.Actor{Kind: "agent", ID: "mason-resume"}
	must(t, repo.Append(ctx, trace.Amendment{Header: trace.Header{Schema: "osmia.trace.amendment", Version: trace.Version, ID: "1", Revision: 1, Project: project, Workstream: stream, At: written, Actor: mason, Cause: "planted"},
		Requester: mason, Role: "mason", Thread: "mason-resume", Turn: "turn", Citations: []string{"spec#1"}, Change: "Resume from a checkpoint", Reason: "Restarts lose progress", Seal: 1, SealRevision: 1, SpecHash: seal.SpecHash(shedSpec)}))
	h.ID, h.Actor = "amendment-1-presented", trace.Actor{Kind: "service", ID: "shed"}
	_, err = repo.RecordDocumentsWith(ctx, []trace.Document{document("amendment_1_spec", "amendments/1/spec.md", strings.Replace(shedSpec, "after a restart", "from a checkpoint", 1)), document("amendment_1_plan", "amendments/1/plan.json", shedPlan),
		document("amendment-1-presented-packet", "amendments/1/packet.json", `{"round":1,"recommendation":"ratify: no objection stands"}`+"\n")},
		trace.Transaction{Transition: trace.Transition{Header: h, Subject: "amendment_1", To: "presented", Reason: "planted"}})
	must(t, err)
	must(t, repo.Close())
	return opts
}

func TestAmendmentCommands(t *testing.T) {
	opts := withAmendment(t)
	s, err := service.Start(context.Background(), opts)
	must(t, err)
	t.Cleanup(func() { s.Close() })
	root := opts.Config.Root

	out := successful(t, root, "amendment", stream, "1")
	if !strings.HasPrefix(out, "Amendment 1 of "+stream+": presented after round 1\nPacket revision 1:\n") || !strings.Contains(out, "ratify: no objection stands") {
		t.Fatalf("amendment output %q", out)
	}
	var view service.AmendmentResponse
	must(t, json.Unmarshal([]byte(successful(t, root, "amendment", stream, "1", "--json")), &view))
	if view.State != "presented" || view.Revision != 1 || view.Decision != nil {
		t.Fatalf("amendment --json %+v", view)
	}
	code, out, diag := invoke(t, root, "amendment", stream, "2")
	if code == 0 || out != "" || !strings.Contains(diag, "has no amendment 2") {
		t.Fatalf("unknown amendment: %d %q %q", code, out, diag)
	}

	// The decision is on the packet revision the command read.
	out = successful(t, root, "amendment", stream, "1", "reject", "Keep restarts as they are.")
	if want := "Amendment 1 of " + stream + ": rejected\nthe owner rejected amendment 1 on packet revision 1; the sealed spec and plan stay in force: Keep restarts as they are.\n"; out != want {
		t.Fatalf("reject output %q, want %q", out, want)
	}
	code, out, diag = invoke(t, root, "amendment", stream, "1", "approve")
	if code != 5 || out != "" || !strings.Contains(diag, "already decided: reject") {
		t.Fatalf("approving a rejected packet: %d %q %q", code, out, diag)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		must(t, json.Unmarshal([]byte(successful(t, root, "amendment", stream, "1", "--json")), &view))
		if view.State == "ruled" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("amendment stayed %s", view.State)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if view.Decision == nil || view.Decision.Decision != "reject" || view.Decision.Packet != 1 {
		t.Fatalf("decided amendment %+v", view)
	}

	for _, args := range [][]string{
		{"amendment"},
		{"amendment", stream},
		{"amendment", "w_x", "1"},
		{"amendment", stream, "1", "accept"},
		{"amendment", stream, "1", "approve", "note", "extra"},
	} {
		if code, _, _ := invoke(t, root, args...); code != 2 {
			t.Fatalf("%v: exit %d", args, code)
		}
	}
}
