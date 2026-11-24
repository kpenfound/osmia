package lifecycle_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func validProjectSpecDraft(f *crossRepoRatifyFixture) map[string]any {
	return map[string]any{
		"behavior":  "Chunks the server accepts are acknowledged with their offset; the client resumes from it.",
		"contracts": []map[string]any{{"name": "acknowledgment", "text": "The acknowledgment names the stored offset."}},
		"slices": []map[string]any{
			{"workstream": string(f.stream1), "contract_criteria": []int{2}},
			{"workstream": string(f.stream2), "contract_criteria": []int{1}},
		},
	}
}

func scriptProjectSpecTurn(f *crossRepoRatifyFixture, turn string, run func(ctx context.Context, req agent.Request, tools *mcp.ClientSession) error) {
	f.engine.Mu.Lock()
	defer f.engine.Mu.Unlock()
	f.engine.Turns[turn] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		if err := run(ctx, req, tools); err != nil {
			return nil, err
		}
		return &agent.Result{ClaudeID: "session-" + req.Name, ResultText: "Turn ended", SessionDir: req.SessionDir, NumTurns: 1}, nil
	}
}

func asError(apiFault *api.APIError) error {
	if apiFault == nil {
		return nil
	}
	return apiFault
}

func projectSpecRecorded(t *testing.T, f *crossRepoRatifyFixture, id config.WorkgroupID) bool {
	t.Helper()
	wt, err := trace.OpenWorkgroupAuthority(f.s.current().Root, id)
	intakefixture.Must(t, err)
	defer wt.Close()
	_, found, err := trace.ProjectSpecFor(wt, id)
	intakefixture.Must(t, err)
	return found
}

func leaderNotices(t *testing.T, f *crossRepoRatifyFixture, text string) []string {
	t.Helper()
	leader, leaderProject := f.stream1, f.project
	if f.stream2 < f.stream1 {
		leader, leaderProject = f.stream2, f.project2
	}
	entries, err := mustRepo(t, f.s, leaderProject).Outbox(leader)
	intakefixture.Must(t, err)
	var bodies []string
	for _, e := range entries {
		if strings.Contains(e.Event.Body, text) {
			bodies = append(bodies, e.Event.Body)
		}
	}
	return bodies
}

func TestProjectSpecInitialDraftRetriesWithItsProblems(t *testing.T) {
	t.Parallel()
	f := newCrossRepoRatifyFixture(t)
	defer f.stop(t)
	writeCharter(t, f.c, f.project2, "Prose guidance with no numbered rules.\n")
	var first, second atomic.Int32
	var firstVerdict, firstPrompt, secondPrompt atomic.Value
	scriptProjectSpecTurn(f, "project-spec-draft-1", func(ctx context.Context, req agent.Request, tools *mcp.ClientSession) error {
		first.Add(1)
		firstPrompt.Store(req.Prompt)
		args := validProjectSpecDraft(f)
		args["behavior"] = "Resumable uploads, as " + string(f.project2) + "/charter#1 requires."
		out, err := intakefixture.CallTool(ctx, tools, lifecycle.ProjectSpecDraftTool, args)
		firstVerdict.Store(out)
		return err
	})
	scriptProjectSpecTurn(f, "project-spec-draft-3", func(ctx context.Context, req agent.Request, tools *mcp.ClientSession) error {
		second.Add(1)
		secondPrompt.Store(req.Prompt)
		_, err := intakefixture.CallTool(ctx, tools, lifecycle.ProjectSpecDraftTool, validProjectSpecDraft(f))
		return err
	})
	id, apiFault := f.s.DraftWorkgroup(context.Background(), []config.WorkstreamID{f.stream1, f.stream2})
	if apiFault != nil {
		t.Fatal(apiFault)
	}
	f.awaitProjectSpec(t)
	if first.Load() != 1 || second.Load() != 1 || !projectSpecRecorded(t, f, id) {
		t.Fatalf("refused first draft was not retried once: first=%d second=%d", first.Load(), second.Load())
	}
	if got := leaderNotices(t, f, "The workgroup spec is recorded"); len(got) != 0 {
		t.Fatalf("a recorded draft whose refusals were never reported told the chief: %v", got)
	}
	if prompt, _ := firstPrompt.Load().(string); !strings.Contains(prompt, string(f.project2)+" (its charter has no numbered rules") {
		t.Fatalf("prompt offers citations of a charter without rules: %s", prompt)
	}
	if verdict, _ := firstVerdict.Load().(string); !strings.Contains(verdict, "invalid") || !strings.Contains(verdict, "no rule numbered 1") {
		t.Fatalf("delivery verdict missed the unresolvable citation: %s", verdict)
	}
	if prompt, _ := secondPrompt.Load().(string); !strings.Contains(prompt, "Your previous draft was refused") || !strings.Contains(prompt, "charter#1") {
		t.Fatalf("retry prompt lacks the refusal: %s", prompt)
	}
}

func TestProjectSpecDraftStopsRetryingAndTheChiefAsksAgain(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newCrossRepoRatifyFixture(t)
	defer f.stop(t)
	var refused atomic.Int32
	for _, turn := range []string{"project-spec-draft-1", "project-spec-draft-3", "project-spec-draft-5"} {
		scriptProjectSpecTurn(f, turn, func(context.Context, agent.Request, *mcp.ClientSession) error {
			refused.Add(1)
			return nil
		})
	}
	id, apiFault := f.s.DraftWorkgroup(ctx, []config.WorkstreamID{f.stream1, f.stream2})
	if apiFault != nil {
		t.Fatal(apiFault)
	}
	leader, leaderProject := f.stream1, f.project
	if f.stream2 < f.stream1 {
		leader, leaderProject = f.stream2, f.project2
	}
	reported := func() string {
		entries, err := mustRepo(t, f.s, leaderProject).Outbox(leader)
		intakefixture.Must(t, err)
		for _, e := range entries {
			if strings.Contains(e.Event.Body, "The workgroup spec could not be drafted") {
				return e.Event.Body
			}
		}
		return ""
	}
	intakefixture.Eventually(t, "the chief of staff is told drafting stopped", func() bool { return reported() != "" })
	haltGuardControllers(t, f)
	for range 3 {
		intakefixture.Must(t, f.s.draftProjectSpecIfReady(ctx, mustRepo(t, f.s, f.project), id))
	}
	if refused.Load() != lifecycle.ProjectSpecDraftAttempts || projectSpecRecorded(t, f, id) {
		t.Fatalf("drafting did not stop after %d refusals: calls=%d", lifecycle.ProjectSpecDraftAttempts, refused.Load())
	}
	if body := reported(); !strings.Contains(body, "last 3 drafts were refused") || !strings.Contains(body, "move_workstream to sketched") {
		t.Fatalf("chief notice does not say what happened or what to do: %s", body)
	}
	entries, err := mustRepo(t, f.s, leaderProject).Outbox(leader)
	intakefixture.Must(t, err)
	notices := 0
	for _, e := range entries {
		if strings.Contains(e.Event.Body, "The workgroup spec could not be drafted") {
			notices++
		}
	}
	if notices != 1 {
		t.Fatalf("refusal reported %d times", notices)
	}

	chief := trace.Actor{Kind: "agent", ID: trace.ChiefOfStaff}
	repo := mustRepo(t, f.s, f.project)
	_, apiFault = f.s.lifecycle().MoveWorkstream(ctx, repo, f.stream1, api.WorkstreamMoveRequest{To: intake.InShedState, Note: "Start the review."}, chief, "chief-turn-1")
	if err := asError(apiFault); !intakefixture.Failed(err, api.Conflict) || !containsMessage(err, "stays sketched") || !containsMessage(err, "last 3 drafts were refused") {
		t.Fatalf("in-shed move does not explain the wait: %v", apiFault)
	}
	note := "Cite no charter rule; neither charter numbers any."
	var requested, prompt atomic.Value
	scriptProjectSpecTurn(f, "project-spec-draft-7", func(_ context.Context, req agent.Request, _ *mcp.ClientSession) error {
		requested.Store(req.Prompt)
		return nil
	})
	scriptProjectSpecTurn(f, "project-spec-draft-9", func(ctx context.Context, req agent.Request, tools *mcp.ClientSession) error {
		prompt.Store(req.Prompt)
		_, err := intakefixture.CallTool(ctx, tools, lifecycle.ProjectSpecDraftTool, validProjectSpecDraft(f))
		return err
	})
	out, apiFault := f.s.lifecycle().MoveWorkstream(ctx, repo, f.stream1, api.WorkstreamMoveRequest{To: intake.SketchedState, Note: note}, chief, "chief-turn-2")
	if apiFault != nil {
		t.Fatal(apiFault)
	}
	if out.State != intake.SketchedState || !strings.Contains(out.Detail, "drafts the workgroup spec again") {
		t.Fatalf("chief's request not taken: %+v", out)
	}
	if _, apiFault := f.s.lifecycle().MoveWorkstream(ctx, repo, f.stream1, api.WorkstreamMoveRequest{To: intake.SketchedState, Note: note}, chief, "chief-turn-3"); !intakefixture.Failed(asError(apiFault), api.Conflict) || !containsMessage(asError(apiFault), "drafting the workgroup spec") {
		t.Fatalf("second request while drafting: %v", apiFault)
	}
	intakefixture.Must(t, f.s.draftProjectSpecIfReady(ctx, mustRepo(t, f.s, f.project), id))
	if projectSpecRecorded(t, f, id) {
		t.Fatal("a refused requested draft was recorded")
	}
	if p, _ := requested.Load().(string); !strings.Contains(p, note) || !strings.Contains(p, "Your previous draft was refused") {
		t.Fatalf("requested draft prompt lacks the note or the refusal: %s", p)
	}
	intakefixture.Must(t, f.s.draftProjectSpecIfReady(ctx, mustRepo(t, f.s, f.project), id))
	if !projectSpecRecorded(t, f, id) {
		t.Fatal("retry of the requested draft not recorded")
	}
	if p, _ := prompt.Load().(string); !strings.Contains(p, note) || !strings.Contains(p, "Your previous draft was refused") {
		t.Fatalf("retry prompt lacks the note or the refusal: %s", p)
	}
	recorded := "The workgroup spec is recorded; each member's committee review starts now."
	intakefixture.Must(t, f.s.draftProjectSpecIfReady(ctx, mustRepo(t, f.s, f.project), id))
	if got := leaderNotices(t, f, recorded); len(got) != 1 {
		t.Fatalf("recorded workgroup spec reported %d times after the reported refusals", len(got))
	}
	if _, apiFault := f.s.lifecycle().MoveWorkstream(ctx, repo, f.stream1, api.WorkstreamMoveRequest{To: intake.SketchedState, Note: note}, chief, "chief-turn-4"); apiFault == nil || containsMessage(asError(apiFault), "drafts the workgroup spec again") {
		t.Fatalf("a recorded workgroup spec was drafted again: %v", apiFault)
	}
}
