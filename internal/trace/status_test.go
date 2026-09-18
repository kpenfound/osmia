package trace

import (
	"context"
	"errors"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/coreadapter"
)

// chiefThread creates a chief-of-staff thread with one claimed turn and
// returns the scope of that turn.
func chiefThread(t *testing.T, r *Repository, turn string) coreadapter.Scope {
	t.Helper()
	ctx := context.Background()
	identity := Agent{Header: header("agent", "chief"), Role: StatusRole, ThreadID: "chief_thread"}
	if err := r.CreateThread(ctx, identity); err != nil {
		t.Fatal(err)
	}
	req := TurnRequest{Header: header("turn-request", "request_"+turn), AgentID: "chief", ThreadID: "chief_thread", TurnID: turn, Profile: coreadapter.Profile{Name: "default", Backend: "fake", Model: "fake-model-7"}, Prompt: "Owner message"}
	if _, err := r.EnqueueTurn(ctx, req); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ClaimTurn(ctx, streamID, "chief", "token_"+turn, "/owned/"+turn, at); err != nil {
		t.Fatal(err)
	}
	return coreadapter.Scope{Project: string(projectID), Workstream: string(streamID), Thread: "chief_thread", Turn: turn, Role: StatusRole}
}

func accept(StatusContent, []string) error { return nil }

func statusOf(t *testing.T, r *Repository) WorkstreamStatus {
	t.Helper()
	list, err := r.Statuses()
	if err != nil || len(list) != 1 {
		t.Fatalf("statuses: %+v %v", list, err)
	}
	return list[0]
}

func TestSetStatusStoresWholeRevisions(t *testing.T) {
	ctx := context.Background()
	r, root, p := create(t)
	if got := statusOf(t, r); got.Status != nil || got.State != "" || got.OpenQuestions != 0 || got.Workstream != streamID {
		t.Fatalf("fresh workstream: %+v", got)
	}
	scope := chiefThread(t, r, "turn1")
	first := StatusContent{Goal: "Ship the importer.", Attention: "Approve the plan.", Note: "The draft is ready.", Agents: []string{"The architect is drafting."}}
	var known []string
	revision, err := r.SetStatus(ctx, "chief", scope, first, at.Add(time.Minute), func(_ StatusContent, k []string) error { known = k; return nil })
	if err != nil || revision != 1 {
		t.Fatalf("first: %d %v", revision, err)
	}
	for _, id := range []string{string(projectID), string(streamID), "chief", "chief_thread", "turn1", "fake-model-7"} {
		if !slices.Contains(known, id) {
			t.Errorf("check did not receive known identifier %q: %v", id, known)
		}
	}
	second := StatusContent{Goal: "Ship the importer.", Note: "The plan was approved.", Agents: []string{}}
	if revision, err := r.SetStatus(ctx, "chief", scope, second, at.Add(2*time.Minute), accept); err != nil || revision != 2 {
		t.Fatalf("second: %d %v", revision, err)
	}
	got := statusOf(t, r).Status
	if got == nil || !reflect.DeepEqual(got.StatusContent, second) || got.Revision != 2 || got.Actor != (Actor{Kind: "agent", ID: "chief"}) || got.Cause != "request_turn1" || got.Depth != 3 || !got.At.Equal(at.Add(2*time.Minute)) {
		t.Fatalf("latest: %+v", got)
	}
	data, err := os.ReadFile(r.directory + "/workstreams/" + string(streamID) + "/status.jsonl")
	if err != nil || strings.Count(string(data), "\n") != 2 {
		t.Fatalf("status log: %q %v", data, err)
	}
	if out, err := r.git(ctx, nil, "status", "--porcelain", "--", "workstreams"); err != nil || strings.TrimSpace(out) != "" {
		t.Fatalf("status not committed: %q %v", out, err)
	}

	// A restarted service reads the same status.
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(root, p)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	after := statusOf(t, reopened).Status
	if after == nil || !reflect.DeepEqual(*after, *got) {
		t.Fatalf("after reopen: %+v", after)
	}
	all, err := Read[Status](reopened, streamID)
	if err != nil || len(all) != 2 || !reflect.DeepEqual(all[0].StatusContent, first) {
		t.Fatalf("revisions: %+v %v", all, err)
	}
}

func TestSetStatusRefusals(t *testing.T) {
	ctx := context.Background()
	r, _, _ := create(t)
	scope := chiefThread(t, r, "turn1")
	valid := StatusContent{Goal: "Ship it.", Note: "Going well.", Agents: []string{}}
	_, err := r.SetStatus(ctx, "chief", scope, valid, at, func(StatusContent, []string) error { return errors.New("goal names a commit") })
	var rejected *StatusRejected
	if !errors.As(err, &rejected) || rejected.Reason != "goal names a commit" {
		t.Fatalf("check rejection: %v", err)
	}
	for name, content := range map[string]StatusContent{
		"no goal":     {Note: "n", Agents: []string{}},
		"blank goal":  {Goal: "  ", Note: "n", Agents: []string{}},
		"no note":     {Goal: "g", Agents: []string{}},
		"nil agents":  {Goal: "g", Note: "n"},
		"blank agent": {Goal: "g", Note: "n", Agents: []string{" "}},
	} {
		if _, err := r.SetStatus(ctx, "chief", scope, content, at, accept); err == nil {
			t.Errorf("%s: stored", name)
		}
	}
	altered := func(f func(*coreadapter.Scope)) coreadapter.Scope { s := scope; f(&s); return s }
	for name, s := range map[string]coreadapter.Scope{
		"mason role":   altered(func(s *coreadapter.Scope) { s.Role = "mason" }),
		"unit":         altered(func(s *coreadapter.Scope) { s.Unit = "unit1" }),
		"other thread": altered(func(s *coreadapter.Scope) { s.Thread = "other" }),
		"other turn":   altered(func(s *coreadapter.Scope) { s.Turn = "other" }),
		"project":      altered(func(s *coreadapter.Scope) { s.Project = "p_00000000000000000000000000000002" }),
	} {
		if _, err := r.SetStatus(ctx, "chief", s, valid, at, accept); err == nil {
			t.Errorf("%s: stored", name)
		}
	}
	if _, err := r.SetStatus(ctx, "mason", scope, valid, at, accept); err == nil {
		t.Error("unknown agent stored")
	}
	if _, err := r.SetStatus(ctx, "chief", scope, valid, time.Time{}, accept); err == nil {
		t.Error("zero time stored")
	}
	if _, err := r.SetStatus(ctx, "chief", scope, valid, at, nil); err == nil {
		t.Error("stored without a check")
	}
	// A mason thread cannot write a status even with a claimed turn.
	if err := r.CreateThread(ctx, threadAgent()); err != nil {
		t.Fatal(err)
	}
	enqueue(t, r, "work")
	claimTurn(t, r, "mason_token")
	mason := coreadapter.Scope{Project: string(projectID), Workstream: string(streamID), Thread: "thread", Turn: "work", Role: "mason"}
	if _, err := r.SetStatus(ctx, "mason", mason, valid, at, accept); err == nil {
		t.Error("mason stored a status")
	}
	direct := Status{Header: header("status", StatusID), StatusContent: valid}
	if err := r.Append(ctx, direct); !errors.Is(err, ErrConflict) {
		t.Errorf("Append wrote a status: %v", err)
	}
	if records, err := Read[Status](r, streamID); err != nil || len(records) != 0 {
		t.Fatalf("refused status stored: %+v %v", records, err)
	}
}

func TestSetStatusRequiresActiveTurn(t *testing.T) {
	ctx := context.Background()
	r, _, _ := create(t)
	identity := Agent{Header: header("agent", "chief"), Role: StatusRole, ThreadID: "chief_thread"}
	if err := r.CreateThread(ctx, identity); err != nil {
		t.Fatal(err)
	}
	req := TurnRequest{Header: header("turn-request", "request_queued"), AgentID: "chief", ThreadID: "chief_thread", TurnID: "queued", Profile: coreadapter.Profile{Name: "default", Backend: "fake", Model: "m"}, Prompt: "p"}
	if _, err := r.EnqueueTurn(ctx, req); err != nil {
		t.Fatal(err)
	}
	scope := coreadapter.Scope{Project: string(projectID), Workstream: string(streamID), Thread: "chief_thread", Turn: "queued", Role: StatusRole}
	valid := StatusContent{Goal: "Ship it.", Note: "Going well.", Agents: []string{}}
	if _, err := r.SetStatus(ctx, "chief", scope, valid, at, accept); err == nil {
		t.Fatal("queued turn stored a status")
	}
}

func TestStatusesReportsServiceFacts(t *testing.T) {
	ctx := context.Background()
	r, _, _ := create(t)
	tx := Transaction{Transition: Transition{Header: header("transition", "handed"), Subject: FeatureSubject, To: "handed", Reason: "Owner handed in a design"}}
	if _, err := r.Transact(ctx, tx); err != nil {
		t.Fatal(err)
	}
	all := specimens()
	answered := all[2].(Question)
	open := answered
	open.ID = "question2"
	for _, v := range []Record{answered, all[3], open} {
		if err := r.Append(ctx, v); err != nil {
			t.Fatal(err)
		}
	}
	got := statusOf(t, r)
	if got.State != "handed" || got.OpenQuestions != 1 || got.Status != nil || len(got.Subjects) != 1 || got.Subjects[FeatureSubject] != (WorkflowState{Version: 1, Value: "handed"}) {
		t.Fatalf("facts: %+v", got)
	}
}

func TestStatusRecordValidation(t *testing.T) {
	good := Status{Header: header("status", StatusID), StatusContent: StatusContent{Goal: "g", Note: "n", Agents: []string{}}}
	if err := validate(good); err != nil {
		t.Fatal(err)
	}
	for name, edit := range map[string]func(*Status){
		"other ID":     func(s *Status) { s.ID = "other" },
		"unit":         func(s *Status) { s.Unit = "unit1" },
		"project":      func(s *Status) { s.Workstream = "" },
		"schema":       func(s *Status) { s.Schema = "osmia.trace.document" },
		"nil agents":   func(s *Status) { s.Agents = nil },
		"blank note":   func(s *Status) { s.Note = "\n" },
		"blank agent":  func(s *Status) { s.Agents = []string{""} },
		"missing goal": func(s *Status) { s.Goal = "" },
	} {
		s := good
		edit(&s)
		if err := validate(s); err == nil {
			t.Errorf("%s: valid", name)
		}
	}
	if recordPath(good) != "workstreams/"+string(streamID)+"/status.jsonl" {
		t.Fatal(recordPath(good))
	}
}
