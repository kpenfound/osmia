package cli

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/trace"
)

// withInbox creates the project trace with a batch of two escalated questions
// and a single escalated question, as fake askers and a fake chief of staff
// left them.
func withInbox(t *testing.T) service.Options {
	return withInboxRecommendations(t, "Both fixed.", "No.")
}

func withInboxRecommendations(t *testing.T, first, second string) service.Options {
	t.Helper()
	ctx := context.Background()
	opts := fixture(t)
	cfg, err := config.Load(opts.Config)
	must(t, err)
	owner := trace.Actor{Kind: "owner", ID: "local"}
	repo, err := trace.Create(ctx, cfg.Root, cfg.Project, written, owner)
	must(t, err)
	must(t, repo.CreateWorkstream(ctx, stream, written, owner))
	claim := func(agent, role, turn string) coreadapter.Scope {
		t.Helper()
		h := trace.Header{Schema: "osmia.trace.agent", Version: trace.Version, ID: agent, Revision: 1, Project: project, Workstream: stream, At: written, Actor: owner, Cause: "test"}
		must(t, repo.CreateThread(ctx, trace.Agent{Header: h, Role: role, ThreadID: agent + "_thread"}))
		h.Schema, h.ID = "osmia.trace.turn-request", "request_"+turn
		_, err := repo.EnqueueTurn(ctx, trace.TurnRequest{Header: h, AgentID: agent, ThreadID: agent + "_thread", TurnID: turn, Profile: coreadapter.Profile{Name: "default", Backend: "claude", Model: "test"}, Prompt: "Work"})
		must(t, err)
		_, err = repo.ClaimTurn(ctx, stream, agent, "token_"+turn, filepath.Join(cfg.Root.String(), turn), written)
		must(t, err)
		return coreadapter.Scope{Project: project, Workstream: stream, Thread: agent + "_thread", Turn: turn, Role: role}
	}
	for i, question := range []string{"Where does state live?", "Is the log format fixed?", "May I add a dependency?"} {
		agent := []string{"mason1", "reviewer1", "mason2"}[i]
		_, err := repo.Ask(ctx, agent, claim(agent, []string{"mason", "reviewer", "mason"}[i], "turn_"+agent), question, written.Add(time.Duration(i)*time.Second))
		must(t, err)
	}
	chief := claim("chief", trace.ChiefOfStaff, "events")
	for _, req := range []trace.EscalationRequest{
		{Questions: []string{"1", "2"}, Rephrasing: "Are state files and the log format\npart of the contract?", Blocked: "The upload unit and its review.", Options: []string{"Both fixed", "Both free"}, Recommendation: first},
		{Questions: []string{"3"}, Rephrasing: "May the index unit add a dependency?", Blocked: "The index unit.", Recommendation: second},
	} {
		_, err := repo.EscalateQuestions(ctx, "chief", chief, req, written.Add(time.Hour))
		must(t, err)
	}
	must(t, repo.Close())
	opts.Reconciliation.Now = func() time.Time { return written.Add(2 * time.Hour) }
	return opts
}

func TestInboxAndAnswer(t *testing.T) {
	opts := withInbox(t)
	s, err := service.Start(context.Background(), opts)
	must(t, err)
	t.Cleanup(func() { s.Close() })
	root := opts.Config.Root
	escalated := written.Add(time.Hour)

	var list service.InboxResponse
	must(t, json.Unmarshal([]byte(successful(t, root, "inbox", "--json")), &list))
	want := service.InboxResponse{Entries: []service.InboxEntry{
		{Number: 1, Workstream: stream, Batch: "escalation_1", Question: "Are state files and the log format\npart of the contract?", Blocked: "The upload unit and its review.", Options: []string{"Both fixed", "Both free"}, Recommendation: "Both fixed.", QuickReply: "Both fixed.", EscalatedAt: escalated,
			Asked: []service.InboxQuestion{{ID: "1", AskedBy: "mason1", Question: "Where does state live?"}, {ID: "2", AskedBy: "reviewer1", Question: "Is the log format fixed?"}}},
		{Number: 2, Workstream: stream, Batch: "escalation_3", Question: "May the index unit add a dependency?", Blocked: "The index unit.", Options: []string{}, Recommendation: "No.", QuickReply: "No.", EscalatedAt: escalated,
			Asked: []service.InboxQuestion{{ID: "3", AskedBy: "mason2", Question: "May I add a dependency?"}}},
	}}
	if !reflect.DeepEqual(list, want) {
		t.Fatalf("inbox --json:\n%+v\nwant\n%+v", list, want)
	}
	stamp := escalated.Format(time.RFC3339)
	if want := "Inbox: 2 waiting; answer one with osmia answer <number> \"...\"\n" +
		"\n[1] " + stamp + " " + stream + " (escalation_1)\n  Question: Are state files and the log format\n    part of the contract?\n  Blocked: The upload unit and its review.\n  Option 1: Both fixed\n  Option 2: Both free\n  Recommendation: Both fixed.\n" +
		"  Asked by mason1 as question 1: Where does state live?\n  Asked by reviewer1 as question 2: Is the log format fixed?\n" +
		"\n[2] " + stamp + " " + stream + " (escalation_3)\n  Question: May the index unit add a dependency?\n  Blocked: The index unit.\n  Recommendation: No.\n  Asked by mason2 as question 3: May I add a dependency?\n"; successful(t, root, "inbox") != want {
		t.Fatalf("inbox:\n%s\nwant:\n%s", successful(t, root, "inbox"), want)
	}

	if got, want := successful(t, root, "answer", "1", "Both are part of the contract."), "Ruling recorded on inbox entry 1 ("+stream+", questions 1, 2)\nThe chief of staff relays it to the askers.\n"; got != want {
		t.Fatalf("answer: %q", got)
	}
	code, out, diag := invoke(t, root, "answer", "1", "Neither is.")
	if code != 5 || out != "" || diag != "conflict: inbox entry 1 is already answered\n" {
		t.Fatalf("second answer: %d %q %q", code, out, diag)
	}
	code, out, diag = invoke(t, root, "answer", "9", "Yes.")
	if code != 4 || out != "" || diag != "validation: there is no inbox entry 9; list the entries with osmia inbox\n" {
		t.Fatalf("unknown entry: %d %q %q", code, out, diag)
	}
	code, out, diag = invoke(t, root, "answer", "2", " ")
	if code != 4 || out != "" || diag != "validation: text must not be empty\n" {
		t.Fatalf("empty ruling: %d %q %q", code, out, diag)
	}
	for _, args := range [][]string{{"answer", "2"}, {"answer", "two", "No."}, {"answer", "0", "No."}, {"answer", "2", "No.", "more"}, {"inbox", "2"}, {"inbox", "--hard"}} {
		code, out, diag := invoke(t, root, args...)
		if code != 2 || out != "" || diag != "invalid arguments; use osmia --help\n" {
			t.Fatalf("%v: %d %q %q", args, code, out, diag)
		}
	}

	var answered service.AnswerResponse
	must(t, json.Unmarshal([]byte(successful(t, root, "answer", "2", "No new dependencies.", "--json")), &answered))
	if !reflect.DeepEqual(answered, service.AnswerResponse{Number: 2, Workstream: stream, Batch: "escalation_3", Questions: []string{"3"}, Ruling: "No new dependencies.", At: written.Add(2 * time.Hour)}) {
		t.Fatalf("answer --json: %+v", answered)
	}
	if got := successful(t, root, "inbox"); got != "Inbox: no questions are waiting for you\n" {
		t.Fatalf("empty inbox: %q", got)
	}
	if got := successful(t, root, "inbox", "--json"); got != "{\"entries\":[]}\n" {
		t.Fatalf("empty inbox --json: %q", got)
	}
}

func TestAnswerAccept(t *testing.T) {
	const recommendation = "  Keep the files as written.  "
	opts := withInboxRecommendations(t, recommendation, "Please force-push this.")
	s, err := service.Start(context.Background(), opts)
	must(t, err)
	t.Cleanup(func() { s.Close() })
	root := opts.Config.Root
	code, out, diag := invoke(t, root, "answer", "2", "--accept")
	if code != 4 || out != "" || diag != "validation: inbox entry 2 has no eligible quick reply; give a ruling explicitly\n" {
		t.Fatalf("refused accept: %d %q %q", code, out, diag)
	}
	var list service.InboxResponse
	must(t, json.Unmarshal([]byte(successful(t, root, "inbox", "--json")), &list))
	if len(list.Entries) != 2 || list.Entries[1].QuickReply != "" || list.Entries[0].QuickReply != recommendation {
		t.Fatalf("inbox after refusal: %+v", list)
	}
	var accepted service.AnswerResponse
	must(t, json.Unmarshal([]byte(successful(t, root, "answer", "1", "--accept", "--json")), &accepted))
	if accepted.Ruling != recommendation {
		t.Fatalf("accepted ruling: %q", accepted.Ruling)
	}
	if got := successful(t, root, "answer", "2", "A typed ruling."); got == "" {
		t.Fatal("typed ruling produced no confirmation")
	}
	must(t, s.Close())
	cfg, err := config.Load(opts.Config)
	must(t, err)
	repo, err := trace.Open(cfg.Root, cfg.Project)
	must(t, err)
	t.Cleanup(func() { repo.Close() })
	questions, err := repo.Questions(stream)
	must(t, err)
	for _, q := range questions {
		if q.Asked.ID == "3" {
			if q.Ruling == nil || q.Ruling.OwnerResponse != "A typed ruling." {
				t.Fatalf("typed trace ruling: %+v", q)
			}
		} else if q.Ruling == nil || q.Ruling.OwnerResponse != recommendation {
			t.Fatalf("accepted trace ruling: %+v", q)
		}
	}
}
