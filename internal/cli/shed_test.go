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
	// The plan's footprint names an entity of the project's map, which
	// ratification validates it against.
	entities := trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: trace.EntitiesDocument, Revision: 1, Project: project, At: written, Actor: owner, Cause: "test"},
		Path: trace.EntitiesPath, Content: `{"version":1,"entities":[{"id":"internal.store","name":"store","paths":["internal/store/**"]}]}` + "\n"}
	must(t, repo.RecordDocuments(ctx, []trace.Document{entities}))
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
	// The chief of staff presented the packet of the conclusion, which is
	// what osmia ratify reads the revisions from.
	packet, err := shed.EncodePacket(shed.Present(1, shed.Pin{Spec: 1, Plan: 1}, false, "planted", shed.DissentRecord([]shed.Record{record}, nil)))
	must(t, err)
	must(t, repo.RecordDocuments(ctx, []trace.Document{{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: shed.PacketDocumentID(1), Revision: 1, Project: project, Workstream: stream, At: written, Actor: trace.Actor{Kind: "service", ID: "shed"}, Cause: "ratification-packet"}, Path: shed.PacketPath(1), Content: string(packet)}}))
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
	if out := successful(t, root, "shed", "redraft", stream, "Split the resume unit."); !strings.Contains(out, "a redraft of spec.md revision 1 and plan.json revision 1 after round 1: Split the resume unit.") {
		t.Fatalf("redraft output %q", out)
	}
	if out := successful(t, root, "shed", "skip", stream); !strings.Contains(out, "ratification") {
		t.Fatalf("skip output %q", out)
	}
	// Ratification is refused while the owner's own sustained objection
	// blocks, and passes once the owner has overruled it instead.
	code, out, diag := invoke(t, root, "ratify", stream)
	if code != 5 || out != "" || !strings.Contains(diag, "blocks and is sustained and is not conceded") {
		t.Fatalf("ratifying over a sustained objection: %d %q %q", code, out, diag)
	}
	var overruled service.ShedResponse
	must(t, json.Unmarshal([]byte(successful(t, root, "shed", "overrule", stream, "owner-r1-1", "I accept the risk.", "--json")), &overruled))
	if overruled.Action != string(shed.Overruled) || overruled.Objection != "owner-r1-1" || !strings.Contains(overruled.Detail, "I accept the risk.") {
		t.Fatalf("overrule --json %+v", overruled)
	}
	// The reason is optional.
	if out := successful(t, root, "shed", "overrule", stream, objection); !strings.Contains(out, "overruled") {
		t.Fatalf("overrule without a reason %q", out)
	}
	// ratify pins the revisions it read from the packet and asks for the
	// sealing. This fixture's clone is no Git repository, so the sealing
	// never completes and the workstream stays in the shed.
	var ratified service.RatifyResponse
	must(t, json.Unmarshal([]byte(successful(t, root, "ratify", stream, "--json")), &ratified))
	if ratified.Spec != 1 || ratified.Plan != 1 || ratified.Round != 1 || ratified.Sealing != "requested" || ratified.Workstream != stream || !strings.HasSuffix(ratified.Detail, "; sealing 1 is requested") {
		t.Fatalf("ratify --json %+v", ratified)
	}
	// Ratifying the same revisions again reports the sealing asked for,
	// pending or running as the loop has it at the time.
	want := "Workstream " + stream + " ratified: spec.md revision 1 and plan.json revision 1\nworkstream " + stream +
		" is ratified at spec.md revision 1 and plan.json revision 1 already; sealing 1 is "
	if out := successful(t, root, "ratify", stream); !strings.HasPrefix(out, want) || !strings.HasSuffix(out, "\n") {
		t.Fatalf("ratify output %q, want a prefix %q", out, want)
	}

	// Refusals reach the owner with the service's own message.
	code, out, diag = invoke(t, root, "shed", "skip", stream)
	if code != 5 || out != "" || !strings.Contains(diag, "already skipped") {
		t.Fatalf("skipping twice: %d %q %q", code, out, diag)
	}
	code, out, diag = invoke(t, root, "shed", "more", stream, "0")
	if code != 4 || out != "" || !strings.Contains(diag, "shed.max_rounds") {
		t.Fatalf("no rounds: %d %q %q", code, out, diag)
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
		{"shed", "more", stream, "two"},
		{"shed", "more", stream, "-1"},
		{"shed", "ratify", stream},
		{"shed", "overrule", stream},
		{"shed", "overrule", stream, "owner-r1-1", "reason", "extra"},
		{"shed", "redraft", stream},
		{"shed", "redraft", stream, "note", "extra"},
		{"ratify"},
		{"ratify", stream, "extra"},
		{"ratify", "w_x"},
		{"shed", "object", "w_x", "argument"},
		{"shed", "skip", stream, "--reason", "why"},
	} {
		if code, _, _ := invoke(t, root, args...); code != 2 {
			t.Fatalf("%v: exit %d", args, code)
		}
	}
}
