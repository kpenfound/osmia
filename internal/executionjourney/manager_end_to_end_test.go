package executionjourney

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/cli"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/servicetest/journey"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
)

const (
	savedManagerName      = "chief_of_staff"
	savedShedPath         = "shed/round-1/chief.json"
	savedShedID           = "shed-round-1-chief"
	savedEscalatedPath    = "units/unit-resume/chief-event_11936ce4d9d5604f2832090d42f46fee570f429c87999860000d7d6685f54023.json"
	savedEscalatedID      = "event_11936ce4d9d5604f2832090d42f46fee570f429c87999860000d7d6685f54023"
	savedRevisePath       = "units/unit-resume/chief-event_2d5f468a35926e0fbf83d71cecc03c61503ca7bd46610838335d7f35d6ec6207.json"
	savedReviseID         = "event_2d5f468a35926e0fbf83d71cecc03c61503ca7bd46610838335d7f35d6ec6207"
	savedNewContestPath   = "units/unit-resume/chief-event_088e691fbaa8448a34b3eac91d3572daab372b6009b96d764a9fa7dffe6635b6.json"
	savedDismissed        = "agent_committee_1-r1-1"
	savedEscalated        = "agent_committee_1-r1-2"
	savedShedContent      = `{"version":1,"round":1,"revision":{"spec":1,"plan":1},"decisions":[{"objection":"agent_committee_1-r1-1","decision":"dismiss","note":"The charter rule does not apply.","turn":"events_1"},{"objection":"agent_committee_1-r1-2","decision":"escalate","note":"It may break rule 2.","turn":"events_1"}]}`
	savedEscalatedBody    = `{"contest":"contest-1","decision":"escalate","note":"The reviews disagree.","turn":"events_2"}`
	savedReviseBody       = `{"contest":"contest-2","decision":"revise","note":"Retry the upload.","turn":"events_3"}`
	savedOwnerQuestion    = "Where is the upload unit?"
	savedManagerAnswer    = "The upload unit waits for its review."
	managerEndToEndConfig = "[events]\nwindow = \"50ms\"\n" +
		"[capacity]\nmanager = 2\n" +
		"[roles.manager]\nprofile = \"other\"\n"
)

func saveManagerTrace(t *testing.T, cfg *config.Config) {
	t.Helper()
	ctx := context.Background()
	repo, err := trace.Open(cfg.Root, cfg.Project)
	fixturekit.Must(t, err)
	defer repo.Close()
	owner, serviceActor := trace.Actor{Kind: "owner", ID: "local"}, trace.Actor{Kind: "service", ID: "osmia"}
	stream, at := fixturekit.Stream, fixturekit.DemoStart.Add(time.Minute)
	header := func(schema, id, cause string, actor trace.Actor, at time.Time) trace.Header {
		return trace.Header{Schema: schema, Version: trace.Version, ID: id, Revision: 1, Project: cfg.Project.ID, Workstream: stream, At: at, Actor: actor, Cause: cause}
	}
	saved, err := repo.ManagerThread(stream)
	fixturekit.Must(t, err)
	if id := saved.Identity; id.ID != savedManagerName || id.Role != savedManagerName || id.ThreadID != savedManagerName || id.Cause != "manager-create" {
		t.Fatalf("the manager thread a workstream starts with: %+v", id)
	}
	profile := coreadapter.Profile{Name: "other", Backend: "codex", Model: "other"}
	request := trace.TurnRequest{Header: header("osmia.trace.turn-request", "request_saved", "owner-message-1", owner, at), AgentID: savedManagerName, ThreadID: savedManagerName, TurnID: "saved", Profile: profile, Prompt: savedOwnerQuestion}
	_, err = repo.EnqueueTurn(ctx, request)
	fixturekit.Must(t, err)
	claimed, err := repo.ClaimTurn(ctx, stream, savedManagerName, "claim-saved", t.TempDir(), at)
	fixturekit.Must(t, err)
	response := trace.TurnResponse{Header: header("osmia.trace.turn-response", "response_saved", "owner-message-1", owner, at.Add(time.Second)), AgentID: savedManagerName, ThreadID: savedManagerName, TurnID: "saved", RequestID: request.ID, RequestRevision: 1,
		Result: coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "codex", ID: "saved-session"}, SessionDirectory: claimed.Claim.SessionDirectory, StartedAt: at, FinalResponse: savedManagerAnswer}}
	fixturekit.Must(t, repo.CaptureTurn(ctx, "claim-saved", response))
	fixturekit.Must(t, repo.CompleteTurn(ctx, stream, savedManagerName, "saved", "claim-saved", at.Add(2*time.Second)))

	manager := trace.Actor{Kind: "agent", ID: savedManagerName}
	document := func(unit, id, path, content, cause string, at time.Time) trace.Document {
		h := header("osmia.trace.document", id, cause, manager, at)
		h.Unit = unit
		return trace.Document{Header: h, Path: path, Content: content + "\n"}
	}
	later := at.Add(time.Hour)
	contest := header("osmia.trace.transition", "contest-1", "test", trace.Actor{Kind: "service", ID: "reviewer"}, later)
	contest.Unit = "resume"
	_, err = repo.Transact(ctx, trace.Transaction{Transition: trace.Transition{Header: contest, Subject: trace.UnitSubject("resume"), To: "contested", Reason: "The reviews disagree."}})
	fixturekit.Must(t, err)
	fixturekit.Must(t, repo.RecordDocuments(ctx, []trace.Document{
		document("", savedShedID, savedShedPath, savedShedContent, savedDismissed, later.Add(time.Minute)),
		document("resume", savedEscalatedID, savedEscalatedPath, savedEscalatedBody, "contest-1", later.Add(2*time.Minute)),
		document("resume", savedReviseID, savedRevisePath, savedReviseBody, "contest-2", later.Add(3*time.Minute)),
	}))
	notice := header("osmia.trace.transition", "saved-notice", "test", serviceActor, later.Add(4*time.Minute))
	_, err = repo.Transact(ctx, trace.Transaction{Transition: trace.Transition{Header: notice, Subject: "saved-notice", To: "changed", Reason: "seeded"},
		Events: []trace.Event{trace.Notice("saved-notice", trace.ManagerKey, "Unit resume is contested.")}})
	fixturekit.Must(t, err)
}

func appendManagerConfig(t *testing.T, root, text string) {
	t.Helper()
	file := filepath.Join(root, "config.toml")
	data, err := os.ReadFile(file)
	fixturekit.Must(t, err)
	fixturekit.Must(t, os.WriteFile(file, append(data, text...), 0600))
}

func runCLI(t *testing.T, root string, args ...string) string {
	t.Helper()
	var out, diag bytes.Buffer
	code := cli.Run(context.Background(), append([]string{"--root", root}, args...), strings.NewReader(""), &out, &diag)
	if code != 0 || diag.Len() != 0 {
		t.Fatalf("osmia %s: code=%d stderr=%s\n%s", strings.Join(args, " "), code, diag.String(), out.String())
	}
	return out.String()
}

func neverNamesTheStoredRole(t *testing.T, what, text string) {
	t.Helper()
	lower := strings.ToLower(text)
	if strings.Contains(lower, savedManagerName) || strings.Contains(lower, trace.ManagerKey) {
		t.Fatalf("%s names the manager by its stored name:\n%s", what, text)
	}
}

func TestAServiceWithTheManagerKeysResumesATraceSavedBeforeTheRename(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	opts, cfg := journey.ConversationFixture(t, "manager-end-to-end-")
	fixturekit.WithListen(t, opts.Config.Root, "web = \"127.0.0.1:0\"\n")
	appendManagerConfig(t, opts.Config.Root, managerEndToEndConfig)
	saveManagerTrace(t, cfg)

	turns := &fixturekit.ManagerTurns{Hold: make(chan struct{}), Entered: make(chan struct{})}
	opts.Reconciliation.Now = func() time.Time { return time.Now().UTC() }
	journey.RunManager(&opts, cfg, turns)
	s, c := journey.StartT(t, opts)
	repo := s.Sole().Repository
	stream := fixturekit.Stream
	root, err := config.ResolveRoot(opts.Config.Root, "")
	fixturekit.Must(t, err)

	select {
	case <-turns.Entered:
	case <-time.After(fixturekit.DemoTimeout):
		t.Fatal("the saved notice did not start a turn on the saved manager thread")
	}
	turns.Mu.Lock()
	first := turns.Prepared[0]
	turns.Mu.Unlock()
	if first.Profile.Name != "other" || first.Profile.Backend != "codex" {
		t.Fatalf("the notice turn ran on profile %+v, want the manager's configured profile", first.Profile)
	}
	if first.Scope.Role != trace.Manager || first.Scope.Thread != savedManagerName {
		t.Fatalf("the notice turn ran in scope %+v, want the saved manager thread", first.Scope)
	}
	if slots := s.ManagerSlots(); slots.Limit != 2 || slots.Used != 1 || len(slots.Active) != 1 {
		t.Fatalf("manager slots while the turn runs: %+v", slots)
	}
	statuses, err := c.Statuses(ctx)
	fixturekit.Must(t, err)
	if statuses.Capacity == nil {
		t.Fatal("the API reports no capacity")
	}
	var managerCapacity *api.RoleCapacity
	for i := range statuses.Capacity.Roles {
		if statuses.Capacity.Roles[i].Role == "manager" {
			managerCapacity = &statuses.Capacity.Roles[i]
		}
	}
	if managerCapacity == nil || managerCapacity.Limit == nil || *managerCapacity.Limit != 2 || managerCapacity.Used != 1 {
		t.Fatalf("API manager capacity: %+v", managerCapacity)
	}
	if got := statuses.Profiles["manager"]; got.Name != "other" || got.Source != "configuration" {
		t.Fatalf("API manager profile: %+v", statuses.Profiles)
	}
	raw, err := json.Marshal(statuses)
	fixturekit.Must(t, err)
	neverNamesTheStoredRole(t, "the API status", string(raw))
	cliStatus := runCLI(t, root.String(), "status")
	if !strings.Contains(cliStatus, "manager: 1 of 2 slot(s) in use") {
		t.Fatalf("the CLI reports no manager capacity:\n%s", cliStatus)
	}
	neverNamesTheStoredRole(t, "the CLI status", cliStatus)

	p := openBrowser(t)
	p.Run(chromedp.EmulateViewport(1280, 800), chromedp.Navigate("http://"+s.WebAddr()+"/"))
	p.Await("the live connection", `document.body.dataset.connection === 'live'`)
	p.AwaitText(`#capacity [data-role=manager] [data-field=slots]`, "1 / 2 slots")
	var shown string
	p.Eval(`document.body.textContent`, &shown)
	neverNamesTheStoredRole(t, "the web page", shown)

	close(turns.Hold)
	intakefixture.Soon(t, "the notice turn to complete on the saved thread", func() bool {
		th, err := repo.Thread(stream, savedManagerName)
		if err != nil || len(th.Turns) != 2 {
			return false
		}
		return !th.Turns[1].CompletedAt.IsZero()
	})
	intakefixture.Soon(t, "the manager slot to be released", func() bool { return s.ManagerSlots().Used == 0 })
	th, err := repo.ManagerThread(stream)
	fixturekit.Must(t, err)
	if id := th.Identity; id.ID != savedManagerName || id.Role != savedManagerName || id.ThreadID != savedManagerName || th.Turns[1].Request.AgentID != savedManagerName || th.Turns[1].Request.Profile.Name != "other" {
		t.Fatalf("the resumed manager thread: %+v, notice turn %+v", id, th.Turns[1].Request)
	}
	if all, err := repo.Threads(stream); err != nil || len(all) != 1 {
		t.Fatalf("threads of the workstream: %d, %v", len(all), err)
	}

	sent, err := c.Send(ctx, stream, "How is the unit?")
	fixturekit.Must(t, err)
	var conversation api.ConversationResponse
	intakefixture.Soon(t, "the manager to answer the owner on the saved thread", func() bool {
		conversation, err = c.Conversation(ctx, stream)
		fixturekit.Must(t, err)
		for _, e := range conversation.Entries {
			if e.Turn == sent.Turn && e.Kind == "response" && e.State == api.TurnDone {
				return true
			}
		}
		return false
	})
	turns.Mu.Lock()
	second := turns.Prepared[len(turns.Prepared)-1]
	turns.Mu.Unlock()
	if second.Profile.Name != "other" || second.Profile.Backend != "codex" || second.Scope.Thread != savedManagerName {
		t.Fatalf("the owner's message ran on %+v in scope %+v, want the configured profile on the saved thread", second.Profile, second.Scope)
	}
	var history []string
	for _, e := range conversation.Entries {
		history = append(history, e.Text)
	}
	text := strings.Join(history, "\n")
	for _, want := range []string{
		savedOwnerQuestion,
		savedManagerAnswer,
		"Dismissed objection agent_committee_1-r1-1 on your behalf: The charter rule does not apply.",
		"Raised objection agent_committee_1-r1-2 to you: It may break rule 2.",
		"Raised contested unit resume to you: The reviews disagree.",
		"Resolved contested unit resume: its mason revises the unit. Retry the upload.",
		"Answer to How is the unit?",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("the conversation lacks %q:\n%s", want, text)
		}
	}
	neverNamesTheStoredRole(t, "the conversation", text)
	cliConversation := runCLI(t, root.String(), "conversation", string(stream))
	for _, want := range []string{"Dismissed objection agent_committee_1-r1-1 on your behalf", "Raised contested unit resume to you: The reviews disagree."} {
		if !strings.Contains(cliConversation, want) {
			t.Fatalf("the CLI conversation lacks %q:\n%s", want, cliConversation)
		}
	}
	neverNamesTheStoredRole(t, "the CLI conversation", cliConversation)

	status, err := c.Status(ctx, stream)
	fixturekit.Must(t, err)
	if len(status.Gates) != 1 || status.Gates[0].Kind != "contested" || status.Gates[0].Reference != "resume" {
		t.Fatalf("the saved escalation does not hold the owner's gate: %+v", status.Gates)
	}

	rounds, err := shed.AllManagerDecisions(repo, stream)
	if err != nil || len(rounds) != 1 || rounds[0].Round != 1 || len(rounds[0].Decisions) != 2 {
		t.Fatalf("saved shed rulings: %+v, %v", rounds, err)
	}
	fit := func(k int) shed.Objection {
		return shed.Objection{ID: shed.ObjectionID(1, "agent_committee_1", k), Kind: shed.Charter, Part: "spec#1", Argument: "It works against the store.", Citations: []string{"kb/entities.json#internal.store"}}
	}
	records := []shed.Record{{Version: shed.Version, Round: 1, Member: "agent_committee_1", Revision: shed.Pin{Spec: 1, Plan: 1}, Objections: []shed.Objection{fit(1), fit(2)}}}
	dissent, err := intake.DissentRecord(repo, stream, records)
	fixturekit.Must(t, err)
	byID := map[string]shed.Entry{}
	for _, e := range dissent {
		byID[e.ID] = e
	}
	if e := byID[savedDismissed]; e.Disposition != shed.Dismissed || e.Blocking {
		t.Fatalf("the dismissed objection came back as %+v", e)
	}
	if e := byID[savedEscalated]; !e.Escalated || !e.Blocking {
		t.Fatalf("the escalated objection came back as %+v", e)
	}
	if shown := shed.ForOwner(dissent); len(shown) != 1 || shown[0].ID != savedEscalated {
		t.Fatalf("the owner is shown %+v", shown)
	}
	if _, escalated, err := execution.ContestEscalated(repo, stream, "resume", trace.Transition{Header: trace.Header{ID: "contest-1"}}); err != nil || !escalated {
		t.Fatalf("the saved escalation of contest-1: %v, %v", escalated, err)
	}
	if _, escalated, err := execution.ContestEscalated(repo, stream, "resume", trace.Transition{Header: trace.Header{ID: "contest-2"}}); err != nil || escalated {
		t.Fatalf("the saved revise ruling of contest-2 is an escalation: %v, %v", escalated, err)
	}
	doc := execution.ManagerDecisionDocument(repo, stream, "resume", "contest-3", "escalate", "Needs the owner.", "events_4", time.Now())
	if doc.Path != savedNewContestPath || doc.Actor.ID != savedManagerName || shed.ManagerPath(1) != savedShedPath {
		t.Fatalf("a new manager decision is written at %q by %+v, and a new shed ruling at %q", doc.Path, doc.Actor, shed.ManagerPath(1))
	}
}
