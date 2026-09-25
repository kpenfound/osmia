package cli

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
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
		{Kind: service.InboxEscalation, Number: 1, Workstream: stream, Batch: "escalation_1", Question: "Are state files and the log format\npart of the contract?", Blocked: "The upload unit and its review.", Options: []string{"Both fixed", "Both free"}, Recommendation: "Both fixed.", QuickReply: "Both fixed.", OpenedAt: escalated, Answer: service.InboxAnswer{Method: "POST", Path: "/v1/inbox/1", Body: map[string]any{}},
			Asked: []service.InboxQuestion{{ID: "1", AskedBy: "mason1", Question: "Where does state live?"}, {ID: "2", AskedBy: "reviewer1", Question: "Is the log format fixed?"}}},
		{Kind: service.InboxEscalation, Number: 2, Workstream: stream, Batch: "escalation_3", Question: "May the index unit add a dependency?", Blocked: "The index unit.", Options: []string{}, Recommendation: "No.", QuickReply: "No.", OpenedAt: escalated, Answer: service.InboxAnswer{Method: "POST", Path: "/v1/inbox/2", Body: map[string]any{}},
			Asked: []service.InboxQuestion{{ID: "3", AskedBy: "mason2", Question: "May I add a dependency?"}}},
	}}
	if !reflect.DeepEqual(list, want) {
		t.Fatalf("inbox --json:\n%+v\nwant\n%+v", list, want)
	}
	stamp := escalated.Format(time.RFC3339)
	if want := "Inbox: 2 waiting; each entry names the command that answers it\n" +
		"\n[1] " + stamp + " " + stream + " (escalation_1)\n  Question: Are state files and the log format\n    part of the contract?\n  Blocked: The upload unit and its review.\n  Option 1: Both fixed\n  Option 2: Both free\n  Recommendation: Both fixed.\n" +
		"  Asked by mason1 as question 1: Where does state live?\n  Asked by reviewer1 as question 2: Is the log format fixed?\n" +
		"  Answer: osmia answer 1 \"...\"\n  Accept the recommendation: osmia answer 1 --accept\n" +
		"\n[2] " + stamp + " " + stream + " (escalation_3)\n  Question: May the index unit add a dependency?\n  Blocked: The index unit.\n  Recommendation: No.\n  Asked by mason2 as question 3: May I add a dependency?\n" +
		"  Answer: osmia answer 2 \"...\"\n  Accept the recommendation: osmia answer 2 --accept\n"; successful(t, root, "inbox") != want {
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
	for _, args := range [][]string{{"answer", "2"}, {"answer", "two", "No."}, {"answer", "0", "No."}, {"answer", "2", "No.", "more"}, {"inbox", "2"}, {"inbox", "--hard"},
		{"answer", "2", "No.", "--accept"}, {"answer", "--accept"}, {"answer", "2", "--accept=true"}, {"inbox", "--accept"}} {
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
	if got := successful(t, root, "inbox"); got != "Inbox: no decisions are waiting for you\n" {
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
	for _, number := range []string{"1", "9"} {
		code, out, diag := invoke(t, root, "answer", number, "--accept")
		if code != 4 || out != "" || diag != "validation: inbox entry "+number+" has no eligible quick reply; give a ruling explicitly\n" {
			t.Fatalf("accept of unlisted entry %s: %d %q %q", number, code, out, diag)
		}
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

func TestInboxPrintsEveryDecisionKind(t *testing.T) {
	opened := written.Add(time.Hour)
	stamp := opened.Format(time.RFC3339)
	list := service.InboxResponse{Entries: []service.InboxEntry{
		{Kind: service.InboxRatification, Workstream: stream, Revision: 2, Question: "Ratify spec.md revision 1 and plan.json revision 3? Debate ended after round 1: settled", Blocked: "Sealing.",
			Options: []string{}, Recommendation: "do not ratify yet: ratification is blocked by 1 objection", OpenedAt: opened, Answer: service.InboxAnswer{Method: "POST", Path: "/v1/ratify/" + stream, Body: map[string]any{"spec": 1.0, "plan": 3.0}}},
		{Kind: service.InboxContested, Workstream: stream, Unit: "resume", Question: "Unit resume is contested: two bounces", Blocked: "Unit resume.", Options: []string{"review", "revise"}, OpenedAt: opened},
		{Kind: service.InboxAmendment, Workstream: stream, Amendment: "2", Unit: "resume", Revision: 4, Question: "Amend it?", Blocked: "Unit resume waits for the decision.",
			Options: []string{"approve", "reject"}, Recommendation: "approve: no objection stands", OpenedAt: opened},
		{Kind: service.InboxDelivery, Workstream: stream, Revision: 5, Question: "Deliver it?", Blocked: "Publishing the pull request.", Options: []string{"approve"}, OpenedAt: opened,
			Answer: service.InboxAnswer{Method: "POST", Path: "/v1/delivery/" + stream, Body: map[string]any{"review": 2.0, "review_revision": 5.0, "commit": "abc123", "draft_hash": "f00"}}},
		{Kind: service.InboxEscalation, Workstream: stream, Number: 3, Batch: "escalation_4", Question: "Which format?", Blocked: "The unit.", Options: []string{}, Recommendation: "Merge them.", OpenedAt: opened},
	}}
	var out strings.Builder
	showInbox(&out, list)
	want := "Inbox: 5 waiting; each entry names the command that answers it\n" +
		"\n[ratification] " + stamp + " " + stream + "\n  Question: Ratify spec.md revision 1 and plan.json revision 3? Debate ended after round 1: settled\n  Blocked: Sealing.\n  Options: none until what blocks it is resolved\n  Recommendation: do not ratify yet: ratification is blocked by 1 objection\n" +
		"  Decided on: spec.md revision 1 and plan.json revision 3 in packet revision 2\n  Answer: osmia ratify " + stream + "\n" +
		"\n[contested] " + stamp + " " + stream + " unit resume\n  Question: Unit resume is contested: two bounces\n  Blocked: Unit resume.\n  Options: review, revise\n" +
		"  Answer: osmia contested " + stream + " resume <review|revise> \"...\"\n" +
		"\n[amendment] " + stamp + " " + stream + " amendment 2\n  Question: Amend it?\n  Blocked: Unit resume waits for the decision.\n  Options: approve, reject\n  Recommendation: approve: no objection stands\n" +
		"  Decided on: packet revision 4\n  Answer: osmia amendment " + stream + " 2 <approve|reject> [note]\n" +
		"\n[delivery] " + stamp + " " + stream + "\n  Question: Deliver it?\n  Blocked: Publishing the pull request.\n  Options: approve\n" +
		"  Decided on: final review 2 (report revision 5) of commit abc123\n  Answer: osmia delivery " + stream + ", then osmia approve " + stream + " [description-file]\n" +
		"\n[3] " + stamp + " " + stream + " (escalation_4)\n  Question: Which format?\n  Blocked: The unit.\n  Recommendation: Merge them.\n  Answer: osmia answer 3 \"...\"\n"
	if out.String() != want {
		t.Fatalf("inbox:\n%s\nwant:\n%s", out.String(), want)
	}
}
