package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/trace"
)

// withCharterProposal creates a project trace whose workstream has a charter
// rule proposed from the owner's ruling on question 1, as a stopped service
// left it.
func withCharterProposal(t *testing.T) service.Options {
	t.Helper()
	ctx := context.Background()
	opts := fixture(t)
	cfg, err := config.Load(opts.Config)
	must(t, err)
	owner := trace.Actor{Kind: "owner", ID: "local"}
	repo, err := trace.Create(ctx, cfg.Root, cfg.Project, written, owner)
	must(t, err)
	must(t, os.WriteFile(filepath.Join(opts.Config.Root, "projects", string(project), "charter.md"), []byte("# Charter\n\n1. Keep state in files.\n"), 0600))
	must(t, repo.CreateWorkstream(ctx, stream, written, owner))
	content, err := json.Marshal(trace.CharterProposal{Question: "1", Ruling: "workstreams/" + stream + "/questions/1/rulings.jsonl", RulingRevision: 2, OwnerResponse: "Uploads always resume.\nNo exceptions.", Rule: "Uploads resume.", Number: 2})
	must(t, err)
	chief := trace.Actor{Kind: "agent", ID: trace.ChiefOfStaff}
	h := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: trace.CharterProposalID("1"), Revision: 1, Project: project, Workstream: stream, At: written, Actor: chief, Cause: "planted"}
	tx := h
	tx.Schema, tx.ID = "osmia.trace.transition", trace.CharterSubject("1")+"_proposed"
	_, err = repo.RecordDocumentsWith(ctx, []trace.Document{{Header: h, Path: trace.CharterProposalPath("1"), Content: string(content)}},
		trace.Transaction{Transition: trace.Transition{Header: tx, Subject: trace.CharterSubject("1"), To: trace.CharterProposed, Reason: "planted"}})
	must(t, err)
	must(t, repo.Close())
	return opts
}

func TestCharterCommands(t *testing.T) {
	opts := withCharterProposal(t)
	s, err := service.Start(context.Background(), opts)
	must(t, err)
	t.Cleanup(func() { s.Close() })
	root := opts.Config.Root
	proposed := "Question 1 of " + stream + ", proposed " + written.Format(time.RFC3339) + ": proposed\n" +
		"  Rule: charter#2 would be: Uploads resume.\n" +
		"  From: workstreams/" + stream + "/questions/1/rulings.jsonl revision 2\n" +
		"  Your ruling: Uploads always resume.\n    No exceptions.\n"

	if out := successful(t, root, "charter"); out != "Charter: 1 proposed rules waiting; decide one with osmia charter <workstream-id> <question> ratify|decline\n\n"+proposed {
		t.Fatalf("charter output %q", out)
	}
	var list service.CharterProposalsResponse
	must(t, json.Unmarshal([]byte(successful(t, root, "charter", "--json")), &list))
	if len(list.Proposals) != 1 || list.Proposals[0].State != trace.CharterProposed || list.Proposals[0].Rule != "Uploads resume." {
		t.Fatalf("charter --json %+v", list)
	}
	if out := successful(t, root, "charter", stream, "1"); out != proposed {
		t.Fatalf("proposal output %q", out)
	}
	code, out, diag := invoke(t, root, "charter", stream, "2")
	if code != 4 || out != "" || !strings.Contains(diag, "has no charter proposal for question 2") {
		t.Fatalf("unknown proposal: %d %q %q", code, out, diag)
	}

	out = successful(t, root, "charter", stream, "1", "ratify", "It holds everywhere.")
	if !strings.HasPrefix(out, "Question 1 of "+stream) || !strings.Contains(out, ": ratified\n") || !strings.HasSuffix(out, "  Decision: ratify\n  Note: It holds everywhere.\nthe owner ratified the charter proposal of question 1; the rule is appended to the charter next: It holds everywhere.\n") {
		t.Fatalf("ratify output %q", out)
	}
	code, _, diag = invoke(t, root, "charter", stream, "1", "decline")
	if code != 5 || !strings.Contains(diag, "already decided: ratify") {
		t.Fatalf("declining a ratified proposal: %d %q", code, diag)
	}
	deadline := time.Now().Add(10 * time.Second)
	var view service.CharterProposalView
	for {
		must(t, json.Unmarshal([]byte(successful(t, root, "charter", stream, "1", "--json")), &view))
		if view.State == trace.CharterChartered {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("proposal stayed %s", view.State)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if out := successful(t, root, "charter", stream, "1"); !strings.Contains(out, "  Rule: charter#2 is: Uploads resume.\n") || !strings.Contains(out, "  Recorded in charter.md revision ") {
		t.Fatalf("chartered output %q", out)
	}
	if out := successful(t, root, "charter"); out != "Charter: no proposed rules are waiting for you\n" {
		t.Fatalf("charter output after the decision %q", out)
	}

	for _, args := range [][]string{
		{"charter", stream},
		{"charter", "w_x", "1"},
		{"charter", stream, "1", "accept"},
		{"charter", stream, "1", "ratify", "note", "extra"},
	} {
		if code, _, _ := invoke(t, root, args...); code != 2 {
			t.Fatalf("%v: exit %d", args, code)
		}
	}
}
