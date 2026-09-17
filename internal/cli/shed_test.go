package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
)

const (
	shedSpec = "# Uploads\n\n## Acceptance criteria\n\n1. Uploads resume after a restart.\n"
	shedPlan = `{"version":1,"units":[{"id":"resume","title":"Resume","addresses":[{"criterion":"spec#1","proof":{"kind":"new-test","name":"TestResume"}}],"depends_on":[],"footprint":["internal.store"]}]}`
	member   = "agent_committee_1"
)

// withShed creates a project trace whose workstream is in the shed after one
// round: the committee's record of it holds one objection, and debate has
// concluded, as a stopped service left it.
func withShed(t *testing.T) service.Options {
	t.Helper()
	ctx := context.Background()
	opts := fixture(t)
	cfg, err := config.Load(opts.Config)
	must(t, err)
	owner := trace.Actor{Kind: "owner", ID: "local"}
	repo, err := trace.Create(ctx, cfg.Root, cfg.Project, written, owner)
	must(t, err)
	must(t, repo.CreateWorkstream(ctx, stream, written, owner))
	document := func(id, path, content string) trace.Document {
		return trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: id, Revision: 1, Project: project, Workstream: stream, At: written, Actor: trace.Actor{Kind: "agent", ID: "agent_architect"}, Cause: "draft-1"}, Path: path, Content: content}
	}
	must(t, repo.RecordDocuments(ctx, []trace.Document{document(plan.SpecDocument, plan.SpecPath, shedSpec), document(plan.PlanDocument, plan.PlanPath, shedPlan)}))
	record := shed.Record{Version: shed.Version, Round: 1, Member: member, Revision: shed.Pin{Spec: 1, Plan: 1}, Turn: "shed-1-" + member + "-1",
		Objections: []shed.Objection{{ID: shed.ObjectionID(1, member, 1), Kind: shed.Size, Part: "plan#resume", Argument: "It does too much.", Citations: []string{"spec#1"}}}}
	content, err := shed.Encode(record)
	must(t, err)
	must(t, repo.RecordDocuments(ctx, []trace.Document{{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: shed.DocumentID(1, member), Revision: 1, Project: project, Workstream: stream, At: written, Actor: trace.Actor{Kind: "agent", ID: member}, Cause: "shed-round-1"}, Path: shed.Path(1, member), Content: string(content)}}))
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "in-shed", Revision: 1, Project: project, Workstream: stream, At: written, Actor: owner, Cause: "test"}
	_, err = repo.SetFeatureState(ctx, h, "in-shed", "planted")
	must(t, err)
	h.ID = "shed-concluded-1"
	_, err = repo.Transact(ctx, trace.Transaction{Transition: trace.Transition{Header: h, Subject: "shed", From: "", To: "concluded-1", Reason: "planted"}})
	must(t, err)
	must(t, repo.Close())
	return opts
}

func TestShedCommands(t *testing.T) {
	opts := withShed(t)
	s, err := service.Start(context.Background(), opts)
	must(t, err)
	t.Cleanup(func() { s.Close() })
	root := opts.Config.Root
	objection := shed.ObjectionID(1, member, 1)

	text := successful(t, root, "shed", "object", stream, "The plan never names the retry budget.")
	if want := "Shed of workstream " + stream + ": objected\nthe owner objected to round 1 against spec.md revision 1 and plan.json revision 1: The plan never names the retry budget.\n"; text != want {
		t.Fatalf("object output %q, want %q", text, want)
	}
	var ruled service.ShedResponse
	must(t, json.Unmarshal([]byte(successful(t, root, "shed", "rule", stream, objection, "dismiss", "I accept the risk.", "--json")), &ruled))
	if ruled.Action != string(shed.Dismissed) || ruled.Objection != objection || ruled.Round != 1 || !strings.Contains(ruled.Detail, "I accept the risk.") {
		t.Fatalf("rule --json %+v", ruled)
	}
	// The note is optional.
	if out := successful(t, root, "shed", "rule", stream, "owner-r1-1", "sustain"); !strings.Contains(out, "sustained") {
		t.Fatalf("rule without a note %q", out)
	}
	if out := successful(t, root, "shed", "more", stream, "2"); !strings.Contains(out, "2 more rounds of debate after round 1") {
		t.Fatalf("more output %q", out)
	}
	if out := successful(t, root, "shed", "skip", stream); !strings.Contains(out, "ratification") {
		t.Fatalf("skip output %q", out)
	}

	// Refusals reach the owner with the service's own message.
	code, out, diag := invoke(t, root, "shed", "skip", stream)
	if code != 5 || out != "" || !strings.Contains(diag, "already skipped") {
		t.Fatalf("skipping twice: %d %q %q", code, out, diag)
	}
	code, out, diag = invoke(t, root, "shed", "rule", stream, "agent_committee_9-r1-1", "dismiss")
	if code != 4 || out != "" || !strings.Contains(diag, "no objection") {
		t.Fatalf("unknown objection: %d %q %q", code, out, diag)
	}
	code, out, diag = invoke(t, root, "shed", "object", stream, "  ")
	if code != 4 || out != "" || !strings.Contains(diag, "an objection requires an argument") {
		t.Fatalf("empty argument: %d %q %q", code, out, diag)
	}
}

// The shed group takes exactly its own arguments.
func TestShedCommandArguments(t *testing.T) {
	opts := fixture(t)
	s, err := service.Start(context.Background(), opts)
	must(t, err)
	t.Cleanup(func() { s.Close() })
	root := opts.Config.Root
	for _, args := range [][]string{
		{"shed"},
		{"shed", "object", stream},
		{"shed", "object", stream, "a", "b"},
		{"shed", "rule", stream, "owner-r1-1"},
		{"shed", "rule", stream, "owner-r1-1", "dismiss", "note", "extra"},
		{"shed", "skip"},
		{"shed", "skip", stream, "extra"},
		{"shed", "more", stream},
		{"shed", "more", stream, "0"},
		{"shed", "more", stream, "two"},
		{"shed", "more", stream, "-1"},
		{"shed", "ratify", stream},
		{"shed", "object", "w_x", "argument"},
		{"shed", "skip", stream, "--reason", "why"},
	} {
		if code, _, _ := invoke(t, root, args...); code != 2 {
			t.Fatalf("%v: exit %d", args, code)
		}
	}
}
