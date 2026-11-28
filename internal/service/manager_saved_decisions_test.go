package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
)

const (
	savedShedPath        = "shed/round-1/chief.json"
	savedShedID          = "shed-round-1-chief"
	savedEscalatedPath   = "units/unit-resume/chief-event_11936ce4d9d5604f2832090d42f46fee570f429c87999860000d7d6685f54023.json"
	savedEscalatedID     = "event_11936ce4d9d5604f2832090d42f46fee570f429c87999860000d7d6685f54023"
	savedRevisePath      = "units/unit-resume/chief-event_2d5f468a35926e0fbf83d71cecc03c61503ca7bd46610838335d7f35d6ec6207.json"
	savedReviseID        = "event_2d5f468a35926e0fbf83d71cecc03c61503ca7bd46610838335d7f35d6ec6207"
	savedNewContestPath  = "units/unit-resume/chief-event_088e691fbaa8448a34b3eac91d3572daab372b6009b96d764a9fa7dffe6635b6.json"
	savedNewContestID    = "event_088e691fbaa8448a34b3eac91d3572daab372b6009b96d764a9fa7dffe6635b6"
	savedDismissed       = "agent_committee_1-r1-1"
	savedEscalated       = "agent_committee_1-r1-2"
	savedShedContent     = `{"version":1,"round":1,"revision":{"spec":1,"plan":1},"decisions":[{"objection":"agent_committee_1-r1-1","decision":"dismiss","note":"The charter rule does not apply.","turn":"events_1"},{"objection":"agent_committee_1-r1-2","decision":"escalate","note":"It may break rule 2.","turn":"events_1"}]}`
	savedEscalatedBody   = `{"contest":"contest-1","decision":"escalate","note":"The reviews disagree.","turn":"events_2"}`
	savedReviseBody      = `{"contest":"contest-2","decision":"revise","note":"Retry the upload.","turn":"events_3"}`
	savedManagerActorID  = "chief_of_staff"
	savedManagerActorKey = "agent"
)

func savedDocument(repo *trace.Repository, stream config.WorkstreamID, unit, id, path, content, cause string, at time.Time) trace.Document {
	h := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: id, Revision: 1, Project: repo.Project(), Workstream: stream, Unit: unit, At: at, Actor: trace.Actor{Kind: savedManagerActorKey, ID: savedManagerActorID}, Cause: cause}
	return trace.Document{Header: h, Path: path, Content: content + "\n"}
}

func TestManagerDecisionsSavedBeforeTheChangeAreStillFoundAndStillTakeEffect(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, cfg := conversationFixture(t, "saved-manager-decisions-")
	repo, err := trace.Open(cfg.Root, cfg.Project)
	fixturekit.Must(t, err)
	t.Cleanup(func() { repo.Close() })
	stream, at := fixturekit.Stream, fixturekit.DemoStart.Add(time.Hour)
	fixturekit.Must(t, repo.RecordDocuments(ctx, []trace.Document{
		savedDocument(repo, stream, "", savedShedID, savedShedPath, savedShedContent, savedDismissed, at),
		savedDocument(repo, stream, "resume", savedEscalatedID, savedEscalatedPath, savedEscalatedBody, "contest-1", at.Add(time.Minute)),
		savedDocument(repo, stream, "resume", savedReviseID, savedRevisePath, savedReviseBody, "contest-2", at.Add(2*time.Minute)),
	}))

	rounds, err := shed.AllManagerDecisions(repo, stream)
	if err != nil || len(rounds) != 1 || rounds[0].Round != 1 || len(rounds[0].Decisions) != 2 {
		t.Fatalf("shed decisions = %+v, %v", rounds, err)
	}
	docs, decisions, err := execution.ManagerDecisions(repo, stream, "resume")
	if err != nil || len(docs) != 2 || len(decisions) != 2 || decisions[0].Contest != "contest-1" || decisions[1].Contest != "contest-2" {
		t.Fatalf("contest decisions = %+v, %v", decisions, err)
	}

	fit := func(k int) shed.Objection {
		return shed.Objection{ID: shed.ObjectionID(1, "agent_committee_1", k), Kind: shed.Charter, Part: "spec#1", Argument: "It works against the store.", Citations: []string{"kb/entities.json#internal.store"}}
	}
	records := []shed.Record{{Version: shed.Version, Round: 1, Member: "agent_committee_1", Revision: shed.Pin{Spec: 1, Plan: 1}, Objections: []shed.Objection{fit(1), fit(2)}}}
	dissent, err := intake.DissentRecord(repo, stream, records)
	if err != nil || len(dissent) != 2 {
		t.Fatalf("dissent = %+v, %v", dissent, err)
	}
	byID := map[string]shed.Entry{}
	for _, e := range dissent {
		byID[e.ID] = e
	}
	if e := byID[savedDismissed]; e.Disposition != shed.Dismissed || e.By != "chief_of_staff" || e.Blocking || e.Note != "The charter rule does not apply." {
		t.Fatalf("the dismissed objection came back as %+v", e)
	}
	if e := byID[savedEscalated]; !e.Escalated || !e.Blocking || e.Disposition != "" || e.Escalation != "It may break rule 2." {
		t.Fatalf("the escalated objection came back as %+v", e)
	}
	if shown := shed.ForOwner(dissent); len(shown) != 1 || shown[0].ID != savedEscalated {
		t.Fatalf("the owner is shown %+v", shown)
	}
	if undecided := shed.Undecided(dissent); len(undecided) != 0 {
		t.Fatalf("objections still waiting for the manager: %+v", undecided)
	}

	escalated := trace.Transition{Header: trace.Header{ID: "contest-1"}}
	if note, ok, err := execution.ContestEscalated(repo, stream, "resume", escalated); err != nil || !ok || note != "The reviews disagree." {
		t.Fatalf("escalated contest = %q, %v, %v", note, ok, err)
	}
	ruled := trace.Transition{Header: trace.Header{ID: "contest-2"}}
	if _, ok, err := execution.ContestEscalated(repo, stream, "resume", ruled); err != nil || ok {
		t.Fatalf("a ruled contest was escalated: %v, %v", ok, err)
	}
	for _, contest := range []trace.Transition{escalated, ruled} {
		if raised, _, err := execution.ContestRaised(repo, stream, "resume", contest); err != nil || !raised {
			t.Fatalf("contest %s raised = %v, %v", contest.ID, raised, err)
		}
	}

	actions, err := ManagerActions(repo, stream)
	fixturekit.Must(t, err)
	var history []string
	for _, a := range actions {
		history = append(history, a.Text)
	}
	text := strings.Join(history, "\n")
	for _, want := range []string{
		"Dismissed objection agent_committee_1-r1-1 on your behalf: The charter rule does not apply.",
		"Raised objection agent_committee_1-r1-2 to you: It may break rule 2.",
		"Raised contested unit resume to you: The reviews disagree.",
		"Resolved contested unit resume: its mason revises the unit. Retry the upload.",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("conversation history lacks %q:\n%s", want, text)
		}
	}
}

func TestANewManagerDecisionIsWrittenUnderTheKeysSavedDecisionsUse(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, cfg := conversationFixture(t, "new-manager-decision-")
	repo, err := trace.Open(cfg.Root, cfg.Project)
	fixturekit.Must(t, err)
	t.Cleanup(func() { repo.Close() })
	stream, at := fixturekit.Stream, fixturekit.DemoStart.Add(time.Hour)

	if shed.ManagerPath(1) != savedShedPath || shed.ManagerDocumentID(1) != savedShedID || !shed.IsManagerPath(savedShedPath) {
		t.Fatalf("shed keys = %q %q", shed.ManagerPath(1), shed.ManagerDocumentID(1))
	}
	doc := execution.ManagerDecisionDocument(repo, stream, "resume", "contest-3", "escalate", "Needs the owner.", "events_4", at)
	if doc.Path != savedNewContestPath || doc.ID != savedNewContestID || execution.ManagerContestPath("resume", "contest-3") != savedNewContestPath {
		t.Fatalf("contest decision keys = %q %q", doc.Path, doc.ID)
	}
	if doc.Actor.Kind != savedManagerActorKey || doc.Actor.ID != savedManagerActorID {
		t.Fatalf("contest decision actor = %+v", doc.Actor)
	}
	fixturekit.Must(t, repo.RecordDocuments(ctx, []trace.Document{doc}))
	docs, decisions, err := execution.ManagerDecisions(repo, stream, "resume")
	if err != nil || len(docs) != 1 || docs[0].Path != savedNewContestPath || decisions[0].Contest != "contest-3" {
		t.Fatalf("recorded decision found as %+v, %v", decisions, err)
	}
}
