package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	wire "github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/projectstore"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
)

const (
	shedSpec = "# Uploads\n\n## Acceptance criteria\n\n1. Uploads resume after a restart.\n"
	shedPlan = `{"version":2,"units":[{"id":"resume","title":"Resume","task":"Resume", "acceptance":["spec#1 holds, shown by TestResume (new-test)"], "criteria":["spec#1"],"depends_on":[],"footprint":["internal.store"]}]}`
	member   = "agent_committee_1"
)

func withShed(t *testing.T) service.Options {
	t.Helper()
	ctx := context.Background()
	opts := fixture(t)
	cfg, err := projectstore.LoadConfig(opts.Config)
	must(t, err)
	owner := trace.Actor{Kind: "owner", ID: "local"}
	repo, err := trace.Create(ctx, cfg.Root, cfg.Project, written, owner)
	must(t, err)
	must(t, repo.CreateWorkstream(ctx, stream, written, owner))
	document := func(id, path, content string) trace.Document {
		return trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: id, Revision: 1, Project: project, Workstream: stream, At: written, Actor: trace.Actor{Kind: "agent", ID: "agent_architect"}, Cause: "draft-1"}, Path: path, Content: content}
	}
	must(t, repo.RecordDocuments(ctx, []trace.Document{document(plan.SpecDocument, plan.SpecPath, shedSpec), document(plan.PlanDocument, plan.PlanPath, shedPlan)}))
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
	packet, err := shed.EncodePacket(shed.Present(1, shed.Pin{Spec: 1, Plan: 1}, false, "planted", shed.DissentRecord([]shed.Record{record}, nil, nil)))
	must(t, err)
	must(t, repo.RecordDocuments(ctx, []trace.Document{{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: shed.PacketDocumentID(1), Revision: 1, Project: project, Workstream: stream, At: written, Actor: trace.Actor{Kind: "service", ID: "shed"}, Cause: "ratification-packet"}, Path: shed.PacketPath(1), Content: string(packet)}}))
	must(t, repo.Close())
	return opts
}

func startShed(t *testing.T) (root, objection string) {
	t.Helper()
	opts := withShed(t)
	s, err := service.Start(context.Background(), opts)
	must(t, err)
	t.Cleanup(func() { s.Close() })
	return opts.Config.Root, shed.ObjectionID(1, member, 1)
}

func ownerObjects(t *testing.T, root string) {
	t.Helper()
	successful(t, root, "shed", "object", stream, "The plan never names the retry budget.")
}

func TestShedObjectRecordsTheOwnersObjection(t *testing.T) {
	root, _ := startShed(t)
	text := successful(t, root, "shed", "object", stream, "The plan never names the retry budget.")
	if want := "Shed of workstream " + stream + ": objected\nthe owner objected to round 1 against spec.md revision 1 and plan.json revision 1: The plan never names the retry budget.\n"; text != want {
		t.Fatalf("object output %q, want %q", text, want)
	}
}

func TestShedRuleDismissReportsTheRulingAsJSON(t *testing.T) {
	root, objection := startShed(t)
	var ruled wire.ShedResponse
	must(t, json.Unmarshal([]byte(successful(t, root, "shed", "rule", stream, objection, "dismiss", "I accept the risk.", "--json")), &ruled))
	if ruled.Action != string(shed.Dismissed) || ruled.Objection != objection || ruled.Round != 1 || !strings.Contains(ruled.Detail, "I accept the risk.") {
		t.Fatalf("rule --json %+v", ruled)
	}
}

func TestShedRuleAcceptsNoNote(t *testing.T) {
	root, _ := startShed(t)
	ownerObjects(t, root)
	if out := successful(t, root, "shed", "rule", stream, "owner-r1-1", "sustain"); !strings.Contains(out, "sustained") {
		t.Fatalf("rule without a note %q", out)
	}
}

func TestShedMoreAsksForFurtherRounds(t *testing.T) {
	root, _ := startShed(t)
	if out := successful(t, root, "shed", "more", stream, "2"); !strings.Contains(out, "2 more rounds of debate after round 1") {
		t.Fatalf("more output %q", out)
	}
}

func TestShedRedraftAsksForANewDraft(t *testing.T) {
	root, _ := startShed(t)
	if out := successful(t, root, "shed", "redraft", stream, "Split the resume unit."); !strings.Contains(out, "a redraft of spec.md revision 1 and plan.json revision 1 after round 1: Split the resume unit.") {
		t.Fatalf("redraft output %q", out)
	}
}

func TestShedSkipMovesTheWorkstreamToRatification(t *testing.T) {
	root, _ := startShed(t)
	if out := successful(t, root, "shed", "skip", stream); !strings.Contains(out, "ratification") {
		t.Fatalf("skip output %q", out)
	}
}

func TestShedSkipTwiceIsAConflict(t *testing.T) {
	root, _ := startShed(t)
	successful(t, root, "shed", "skip", stream)
	code, out, diag := invoke(t, root, "shed", "skip", stream)
	if code != 5 || out != "" || !strings.Contains(diag, "already skipped") {
		t.Fatalf("skipping twice: %d %q %q", code, out, diag)
	}
}

func TestShedOverruleReportsTheRulingAsJSON(t *testing.T) {
	root, _ := startShed(t)
	ownerObjects(t, root)
	successful(t, root, "shed", "rule", stream, "owner-r1-1", "sustain")
	var overruled wire.ShedResponse
	must(t, json.Unmarshal([]byte(successful(t, root, "shed", "overrule", stream, "owner-r1-1", "I accept the risk.", "--json")), &overruled))
	if overruled.Action != string(shed.Overruled) || overruled.Objection != "owner-r1-1" || !strings.Contains(overruled.Detail, "I accept the risk.") {
		t.Fatalf("overrule --json %+v", overruled)
	}
}

func TestShedOverruleAcceptsNoReason(t *testing.T) {
	root, objection := startShed(t)
	successful(t, root, "shed", "rule", stream, objection, "dismiss", "I accept the risk.")
	if out := successful(t, root, "shed", "overrule", stream, objection); !strings.Contains(out, "overruled") {
		t.Fatalf("overrule without a reason %q", out)
	}
}

func TestRatifyRefusesASustainedObjection(t *testing.T) {
	root, _ := startShed(t)
	ownerObjects(t, root)
	successful(t, root, "shed", "rule", stream, "owner-r1-1", "sustain")
	code, out, diag := invoke(t, root, "ratify", stream)
	if code != 5 || out != "" || !strings.Contains(diag, "blocks and is sustained and is not conceded") {
		t.Fatalf("ratifying over a sustained objection: %d %q %q", code, out, diag)
	}
}

func TestRatifyReportsTheRatificationAsJSON(t *testing.T) {
	root, objection := startShed(t)
	successful(t, root, "shed", "rule", stream, objection, "dismiss", "I accept the risk.")
	var ratified wire.RatifyResponse
	must(t, json.Unmarshal([]byte(successful(t, root, "ratify", stream, "--json")), &ratified))
	if ratified.Spec != 1 || ratified.Plan != 1 || ratified.Round != 1 || ratified.Sealing != "requested" || ratified.Workstream != stream || !strings.HasSuffix(ratified.Detail, "; the sealing is asked for") {
		t.Fatalf("ratify --json %+v", ratified)
	}
}

func TestRatifyTwiceReportsTheExistingRatification(t *testing.T) {
	root, objection := startShed(t)
	successful(t, root, "shed", "rule", stream, objection, "dismiss", "I accept the risk.")
	successful(t, root, "ratify", stream)
	want := "Workstream " + stream + " ratified: spec.md revision 1 and plan.json revision 1\nworkstream " + stream +
		" is ratified at spec.md revision 1 and plan.json revision 1 already; "
	if out := successful(t, root, "ratify", stream); !strings.HasPrefix(out, want) || !strings.HasSuffix(out, "\n") {
		t.Fatalf("ratify output %q, want a prefix %q", out, want)
	}
}

func TestShedCommandsRefuseInvalidRequests(t *testing.T) {
	root, _ := startShed(t)
	for _, c := range []struct {
		name string
		args []string
		diag string
	}{
		{"no rounds", []string{"shed", "more", stream, "0"}, "shed.max_rounds"},
		{"unknown objection", []string{"shed", "rule", stream, "agent_committee_9-r1-1", "dismiss"}, "no objection"},
		{"empty argument", []string{"shed", "object", stream, "  "}, "an objection requires an argument"},
	} {
		t.Run(c.name, func(t *testing.T) {
			code, out, diag := invoke(t, root, c.args...)
			if code != 4 || out != "" || !strings.Contains(diag, c.diag) {
				t.Fatalf("%d %q %q", code, out, diag)
			}
		})
	}
}

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
