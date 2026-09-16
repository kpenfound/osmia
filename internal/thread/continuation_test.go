package thread

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/trace"
)

type resumableTurns struct {
	run   fakeTurns
	check func(context.Context, coreadapter.Profile, coreadapter.Profile, coreadapter.BackendSession) error
}

func (f resumableTurns) Run(ctx context.Context, p coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
	return f.run(ctx, p)
}
func (f resumableTurns) CheckResume(ctx context.Context, old, next coreadapter.Profile, s coreadapter.BackendSession) error {
	return f.check(ctx, old, next, s)
}

func TestContinuationAcrossReopen(t *testing.T) {
	for _, mode := range []string{"resume", "profile-compatible", "profile-incompatible", "backend", "missing", "corrupt", "unsupported", "rejected", "fallback", "ambiguous"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			repo, root, projectConfig := setup(t)
			first := queue(t, repo, "first")
			runner := Runner{Store: repo, Now: func() time.Time { return timestamp.Add(time.Second) }, Turns: fakeTurns(func(context.Context, coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
				return coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "fake", ID: "saved"}, FinalResponse: "owned final"}, nil
			})}
			if _, err := runner.RunNext(ctx, stream, "agent", coreadapter.PreparedTurn{SessionDirectory: "/owned/first"}); err != nil {
				t.Fatal(err)
			}
			repo.Close()
			var err error
			repo, err = trace.Open(root, projectConfig)
			if err != nil {
				t.Fatal(err)
			}
			defer repo.Close()
			second := first
			second.ID, second.TurnID, second.Prompt = "request_second", "second", "new prompt"
			second.History = "PRIVATE TRANSCRIPT MUST NOT APPEAR"
			second.Resume = &coreadapter.BackendSession{Backend: "fake", ID: "untrusted"}
			if strings.HasPrefix(mode, "profile") {
				second.Profile.Name, second.Profile.Model = "changed", "another"
			}
			if mode == "backend" {
				second.Profile.Backend = "other"
			}
			if _, err := repo.EnqueueTurn(ctx, second); err != nil {
				t.Fatal(err)
			}
			checks, calls := 0, 0
			runner.Store = repo
			runner.MaxRetries = 1
			runner.Fallbacks = map[string]coreadapter.Profile{"default": {Name: "fallback", Backend: "other", Model: "small"}}
			runner.Turns = resumableTurns{
				check: func(_ context.Context, old, next coreadapter.Profile, s coreadapter.BackendSession) error {
					checks++
					if old != first.Profile || next != second.Profile || s.ID != "saved" {
						t.Fatalf("compatibility inputs: %#v %#v %#v", old, next, s)
					}
					switch mode {
					case "missing", "corrupt", "profile-incompatible":
						return coreadapter.ErrResumeUnavailable
					case "unsupported":
						return coreadapter.ErrUnsupported
					}
					return nil
				},
				run: func(_ context.Context, p coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
					calls++
					if p.Scope.Turn != "second" || p.Prompt != second.Prompt {
						t.Fatalf("turn identity changed: %#v", p)
					}
					wantResume := calls == 1 && (mode == "resume" || mode == "profile-compatible" || mode == "rejected" || mode == "fallback" || mode == "ambiguous")
					if (p.Resume != nil) != wantResume {
						t.Fatalf("resume=%v, want %v", p.Resume, wantResume)
					}
					if wantResume {
						if p.Resume.ID != "saved" || p.History != "" {
							t.Fatalf("bad resume: %#v", p)
						}
					} else {
						var h replayContext
						if err := json.Unmarshal([]byte(p.History), &h); err != nil {
							t.Fatal(err)
						}
						if len(h.Exchanges) != 1 || h.Exchanges[0].Prompt != first.Prompt || h.Exchanges[0].FinalResponse != "owned final" || h.Exchanges[0].Request.Cause != first.Cause || strings.Contains(p.History, "PRIVATE") {
							t.Fatalf("replay: %s", p.History)
						}
					}
					if calls == 1 {
						switch mode {
						case "rejected":
							return coreadapter.SessionResult{}, coreadapter.ErrResumeUnavailable
						case "fallback":
							return coreadapter.SessionResult{}, coreadapter.ErrNotStarted
						case "ambiguous":
							return coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "fake", ID: "partial"}, FinalResponse: "partial"}, coreadapter.ErrNotStarted
						}
					}
					if mode == "fallback" && p.Profile.Name != "fallback" {
						t.Fatalf("fallback not selected: %#v", p.Profile)
					}
					return coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: p.Profile.Backend, ID: "final"}, FinalResponse: "next final"}, nil
				},
			}
			q, err := runner.RunNext(ctx, stream, "agent", coreadapter.PreparedTurn{SessionDirectory: "/owned/second"})
			if mode == "ambiguous" {
				if !errors.Is(err, coreadapter.ErrNotStarted) {
					t.Fatal(err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			wantCalls := 1
			if mode == "rejected" || mode == "fallback" {
				wantCalls = 2
			}
			if calls != wantCalls || len(q.Attempts) != wantCalls {
				t.Fatalf("attempts: %d %#v", calls, q.Attempts)
			}
			if mode == "backend" && checks != 0 {
				t.Fatal("cross-backend capability check")
			}
			if q.Attempts[0].SourceSequence != 1 || q.Attempts[0].SourceSession.ID != "saved" {
				t.Fatalf("source: %#v", q.Attempts)
			}
			repo.Close()
			repo, err = trace.Open(root, projectConfig)
			if err != nil {
				t.Fatal(err)
			}
			defer repo.Close()
			th, err := repo.Thread(stream, "agent")
			if err != nil || !reflect.DeepEqual(th.Turns[1].Attempts, q.Attempts) {
				t.Fatalf("durable attempts: %#v %v", th, err)
			}
			responses, err := trace.Read[trace.TurnResponse](repo, stream)
			if err != nil || len(responses) != 2 {
				t.Fatalf("duplicate or missing log: %d %v", len(responses), err)
			}
		})
	}
}

func TestReplayBoundsAndOrder(t *testing.T) {
	th := trace.Thread{}
	for i := uint64(1); i <= 5; i++ {
		req := trace.TurnRequest{Header: trace.Header{ID: trace.EventID("request", string(rune('a'+i))), Cause: "owner", Depth: int(i)}, Prompt: strings.Repeat("x", int(i)*10), History: "private", SystemPrompt: "private", TurnID: string(rune('a' + i))}
		res := trace.TurnResponse{Header: trace.Header{ID: "response"}, Result: coreadapter.SessionResult{FinalResponse: "answer"}}
		th.Turns = append(th.Turns, trace.QueuedTurn{Sequence: i, Request: req, Response: &res, CompletedAt: timestamp})
	}
	// An in-flight pair and a queued request are never replayed.
	th.Turns = append(th.Turns, trace.QueuedTurn{Sequence: 6, Request: trace.TurnRequest{Prompt: "inflight"}, Response: &trace.TurnResponse{Result: coreadapter.SessionResult{FinalResponse: "partial"}}}, trace.QueuedTurn{Sequence: 7})
	raw, from, omitted, err := Replay(th, 8, ReplayLimits{MaxTurns: 2})
	if err != nil || from != 4 || omitted != 3 {
		t.Fatalf("bounds: %s %d %d %v", raw, from, omitted, err)
	}
	var got replayContext
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Exchanges) != 2 || got.Exchanges[0].Sequence != 4 || got.Exchanges[1].Sequence != 5 || strings.Contains(raw, "private") || strings.Contains(raw, "partial") {
		t.Fatal(raw)
	}
	again, _, _, _ := Replay(th, 8, ReplayLimits{MaxTurns: 2})
	if again != raw {
		t.Fatal("nondeterministic replay")
	}
	smaller, from, omitted, err := Replay(th, 8, ReplayLimits{MaxBytes: len(raw) - 1})
	if err != nil || len(smaller) >= len(raw) || from != 5 || omitted != 4 {
		t.Fatalf("byte bound: %s %d %d %v", smaller, from, omitted, err)
	}
	tiny, from, omitted, err := Replay(th, 8, ReplayLimits{MaxBytes: 100})
	if err != nil || len(tiny) > 100 || from != 0 || omitted != 5 {
		t.Fatalf("oversized newest: %s %d %d %v", tiny, from, omitted, err)
	}
	if _, _, _, err := Replay(th, 8, ReplayLimits{MaxBytes: 1}); err == nil {
		t.Fatal("marker overflow accepted")
	}
}

func TestInterruptedAttemptIsNeverRelaunched(t *testing.T) {
	ctx := context.Background()
	repo, root, p := setup(t)
	req := queue(t, repo, "first")
	queue(t, repo, "second")
	q, err := repo.ClaimTurn(ctx, stream, "agent", "token", "/owned", timestamp.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	a := trace.TurnAttempt{Number: 1, Profile: req.Profile, Path: "replay", Reason: "fresh", At: q.Claim.At}
	if err := repo.RecordAttempt(ctx, stream, "agent", "first", q.Claim.Token, a); err != nil {
		t.Fatal(err)
	}
	repo.Close()
	repo, err = trace.Open(root, p)
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	runner := Runner{Store: repo, Now: func() time.Time { return timestamp.Add(2 * time.Second) }, Turns: fakeTurns(func(context.Context, coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
		t.Fatal("relaunched interrupted attempt")
		return coreadapter.SessionResult{}, nil
	})}
	if _, err := runner.RunNext(ctx, stream, "agent", coreadapter.PreparedTurn{SessionDirectory: "/new"}); !errors.Is(err, trace.ErrClaimed) {
		t.Fatal(err)
	}
	th, err := repo.Thread(stream, "agent")
	if err != nil || th.Status != "interrupted" || len(th.Turns) != 2 || th.Turns[0].Attempts[0].Result != nil {
		t.Fatalf("%#v %v", th, err)
	}
}

func TestNotesIsolationAndReopen(t *testing.T) {
	ctx := context.Background()
	repo, root, p := setup(t)
	req := queue(t, repo, "first")
	scope := coreadapter.Scope{Project: string(project), Workstream: string(stream), Thread: req.ThreadID, Turn: req.TurnID, Role: "mason"}
	tools, err := repo.NotesTools("agent", scope)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tools[0].Handle(ctx, json.RawMessage(`{}`)); err == nil {
		t.Fatal("unclaimed turn read notes")
	}
	q, err := repo.ClaimTurn(ctx, stream, "agent", "token", "/owned", timestamp.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"Project", "Role", "Thread", "Turn", "Workstream"} {
		altered := scope
		switch field {
		case "Project":
			altered.Project = "p_00000000000000000000000000000002"
		case "Role":
			altered.Role = "reviewer"
		case "Thread":
			altered.Thread = "another"
		case "Turn":
			altered.Turn = "another"
		case "Workstream":
			altered.Workstream = "../escape"
		}
		if _, err := repo.NotesTools("agent", altered); err == nil {
			t.Fatalf("accepted %s", field)
		}
	}
	for _, input := range []string{`{"path":"../escape","text":"bad"}`, `{"role":"reviewer","text":"bad"}`, `{"project":"other","text":"bad"}`, `{"text":"a","text":"b"}`, `{}`} {
		if _, err := tools[1].Handle(ctx, json.RawMessage(input)); err == nil {
			t.Fatalf("accepted %s", input)
		}
	}
	if _, err := tools[1].Handle(ctx, json.RawMessage(`{"text":"private craft"}`)); err != nil {
		t.Fatal(err)
	}
	data, err := tools[0].Handle(ctx, json.RawMessage(`{}`))
	if err != nil || !strings.Contains(string(data), "private craft") {
		t.Fatalf("%s %v", data, err)
	}
	// Captured turns cannot retain access to their old tool closures.
	h := req.Header
	h.Schema, h.ID, h.Actor = "osmia.trace.turn-response", "response", trace.Actor{Kind: "service", ID: "test"}
	h.At = q.Claim.At
	res := trace.TurnResponse{Header: h, AgentID: "agent", ThreadID: req.ThreadID, TurnID: req.TurnID, RequestID: req.ID, RequestRevision: 1, Result: coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "fake", ID: "s"}, SessionDirectory: "/owned", StartedAt: h.At}}
	if err := repo.CaptureTurn(ctx, q.Claim.Token, res); err != nil {
		t.Fatal(err)
	}
	if err := repo.CompleteTurn(ctx, stream, "agent", "first", q.Claim.Token, h.At); err != nil {
		t.Fatal(err)
	}
	if _, err := tools[0].Handle(ctx, json.RawMessage(`{}`)); err == nil {
		t.Fatal("completed turn read notes")
	}
	repo.Close()
	repo, err = trace.Open(root, p)
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	queue(t, repo, "next")
	scope.Turn = "next"
	tools, err = repo.NotesTools("agent", scope)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ClaimTurn(ctx, stream, "agent", "next", "/next", timestamp.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	data, err = tools[0].Handle(ctx, json.RawMessage(`{}`))
	if err != nil || !strings.Contains(string(data), "private craft") {
		t.Fatalf("reopen: %s %v", data, err)
	}
	directory, _ := root.ProjectTrace(project)
	if err := os.Rename(directory+"/notes/mason.md", directory+"/notes/reviewer.md"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("reviewer.md", directory+"/notes/mason.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := tools[0].Handle(ctx, json.RawMessage(`{}`)); err == nil {
		t.Fatal("symlink read accepted")
	}
	if _, err := tools[1].Handle(ctx, json.RawMessage(`{"text":"escape"}`)); err == nil {
		t.Fatal("symlink write accepted")
	}
}
