package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/ghlog"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestChiefDecidesEngineeringRevisionButCannotChangeIntent(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, spec string
		allowed    bool
	}{{"engineering", validSpec, true}, {"intent", amendedSpec, false}} {
		t.Run(tc.name, func(t *testing.T) {
			f, stream := builtForAmendment(t, 1)
			defer f.stop(t)
			requester := presentAmendment(t, f, stream, masonRole, tc.spec, amendedPlan)
			out, api := f.s.recordAmendmentDecision(context.Background(), f.project, stream, f.repository(), "1", AmendmentDecisionRequest{Decision: AmendmentApprove, Packet: 1, Note: "The revised verification preserves the approved upload outcome."}, chiefActor, "engineering-decision")
			if tc.allowed {
				if api != nil {
					t.Fatal(api.Message)
				}
				if out.Decision.Actor != trace.ChiefOfStaff {
					t.Fatalf("wrong actor: %+v", out.Decision)
				}
				f.awaitAmendment(t, stream, amendmentRuled)
				f.stop(t)
				f.start(t)
				if got := f.revisions(t, stream, plan.SpecDocument); len(got) != 1 {
					t.Fatalf("engineering decision rewrote intent: %v", got)
				}
				if got := f.revisions(t, stream, plan.PlanDocument); len(got) != 2 {
					t.Fatalf("plan revision lost across restart: %v", got)
				}
				turns := f.ruling(t, stream, requester)
				if len(turns) != 1 || !strings.Contains(turns[0].Request.Prompt, "osmia:answer | chief of staff") || strings.Contains(turns[0].Request.Prompt, "osmia:owner_response") {
					t.Fatalf("misattributed engineering ruling: %+v", turns)
				}
				m, err := newMasonController(f.s, f.repository()).bundle(context.Background(), stream, "resume")
				must(t, err)
				if text := m.RenderAmendments(); !strings.Contains(text, "the chief of staff approved") || strings.Contains(text, "osmia:owner_response") {
					t.Fatalf("misattributed assignment: %s", text)
				}
			} else if api == nil || !strings.Contains(api.Message, "owner decision") {
				t.Fatalf("intent change accepted: %+v %v", out, api)
			}
		})
	}
}

func TestDiscoveryBlocksCompletionUntilAssignedWorkLands(t *testing.T) {
	t.Parallel()
	c := newContestFixture(t, "discovered-work")
	scope := coreadapter.Scope{Project: string(c.repo.Project()), Workstream: string(c.stream), Role: trace.ChiefOfStaff, Thread: trace.ChiefOfStaff, Turn: "events_1"}
	tool := recordDiscovery(c.repo, scope, c.f.s.now)
	raw, err := tool.Handle(context.Background(), json.RawMessage(`{"key":"repair-fake","description":"Update the dependent fake for the new protocol","criterion":"spec#1"}`))
	must(t, err)
	var result struct {
		ID       string `json:"id"`
		Revision int    `json:"revision"`
	}
	must(t, json.Unmarshal(raw, &result))
	if result.ID == "" {
		t.Fatal(string(raw))
	}
	pending, err := unresolvedWork(c.repo, c.stream)
	must(t, err)
	if !pending {
		t.Fatal("open necessary work did not block completion")
	}
	// Repeating a discovery key is idempotent across turns.
	again, err := tool.Handle(context.Background(), json.RawMessage(`{"key":"repair-fake","description":"Update the dependent fake for the new protocol","criterion":"spec#1"}`))
	must(t, err)
	if string(again) != string(raw) {
		t.Fatalf("duplicate discovery: %s %s", raw, again)
	}
	reused, err := tool.Handle(context.Background(), json.RawMessage(`{"key":"repair-fake","description":"Unrelated work","criterion":"spec#1"}`))
	must(t, err)
	if !strings.Contains(string(reused), "different work") {
		t.Fatalf("reused key hid new work: %s", reused)
	}
	_, previous, err := c.r.candidateEvidence(context.Background(), c.stream, "resume")
	must(t, err)
	input, _ := json.Marshal(map[string]any{"id": result.ID, "revision": result.Revision, "disposition": "assigned", "unit": "resume", "reason": "This supporting change belongs to the upload outcome."})
	assigned, err := tool.Handle(context.Background(), input)
	must(t, err)
	if !strings.Contains(string(assigned), `"recorded":true`) {
		t.Fatal(string(assigned))
	}
	pending, err = unresolvedWork(c.repo, c.stream)
	must(t, err)
	if !pending {
		t.Fatal("assignment incorrectly counted as completion")
	}
	_, current, err := c.r.candidateEvidence(context.Background(), c.stream, "resume")
	must(t, err)
	if why := staleReview(previous, current); !strings.Contains(why, "discovered work") {
		t.Fatalf("new work did not invalidate old review: %q", why)
	}
	assignment, err := assignmentContext(c.repo, coreadapter.Scope{Workstream: string(c.stream), Unit: "resume", Role: reviewerRole})
	must(t, err)
	if !strings.Contains(assignment, "Update the dependent fake") {
		t.Fatal("reviewer did not receive discovered work")
	}
	mergeDirectly(t, c.f, c.repo, c.stream, "resume", map[string]string{"discovered.txt": "fake repaired\n"})
	pending, err = unresolvedWork(c.repo, c.stream)
	must(t, err)
	if pending {
		t.Fatal("landed assignment did not resolve necessary work")
	}
	// Declining requires a recorded judgment; stale revisions cannot change it.
	input, _ = json.Marshal(map[string]any{"id": result.ID, "revision": 2, "disposition": "declined", "reason": "Inspection shows the existing fake already covers this behavior."})
	_, err = tool.Handle(context.Background(), input)
	must(t, err)
	pending, err = unresolvedWork(c.repo, c.stream)
	must(t, err)
	if pending {
		t.Fatal("reasoned disposition did not resolve the discovery")
	}
	stale, err := tool.Handle(context.Background(), input)
	must(t, err)
	if !strings.Contains(string(stale), "discovery changed") {
		t.Fatal(string(stale))
	}
}

type remoteFileTransport func(*http.Request) (*http.Response, error)

func (f remoteFileTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRemoteFileResolvesCommitAndKeepsCredentialsInService(t *testing.T) {
	t.Parallel()
	const commit = "0123456789012345678901234567890123456789"
	calls := 0
	client := &http.Client{Transport: remoteFileTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Host != "api.github.com" || r.Header.Get("Authorization") != "Bearer fixture-token" {
			t.Fatalf("unexpected request %s", r.URL)
		}
		body := `{"sha":"` + commit + `"}`
		if calls == 2 {
			if r.URL.Query().Get("ref") != commit {
				t.Fatal("file was not pinned to resolved commit")
			}
			body = `{"type":"file","encoding":"base64","content":"` + base64.StdEncoding.EncodeToString([]byte("# Referenced design\n")) + `"}`
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
	})}
	f, err := githubFile(context.Background(), client, "fixture-token", remoteFile{Repository: "https://github.com/example/project.git", Ref: "main", Path: "docs/design.md"})
	must(t, err)
	if calls != 2 || f.Commit != commit || f.Content != "# Referenced design\n" {
		t.Fatalf("wrong remote evidence %+v", f)
	}
	for _, repository := range []string{"http://github.com/example/project", "https://localhost/project/repo", "https://token@github.com/example/project", "https://github.com/example/project?token=secret"} {
		if _, err := githubFile(context.Background(), client, "fixture-token", remoteFile{Repository: repository, Path: "file"}); err == nil {
			t.Fatalf("unsafe repository accepted: %s", repository)
		}
	}
	if calls != 2 {
		t.Fatal("invalid input sent a credential")
	}
}

func TestAssignmentContextAndDependenciesFollowCurrentPlan(t *testing.T) {
	t.Parallel()
	f, stream, repo, _ := newFinalFixture(t, "assignment-context")
	defer repo.Close()
	held, err := assignmentHeld(repo, stream, "dedupe", masonRole)
	must(t, err)
	if !held {
		t.Fatal("dependent unit was dispatched before its prerequisite landed")
	}
	scope := coreadapter.Scope{Workstream: string(stream), Role: masonRole, Unit: "dedupe"}
	text, err := assignmentContext(repo, scope)
	must(t, err)
	for _, want := range []string{"plan revision 1", "Skip acknowledged chunks", "resume", "factory_context"} {
		if !strings.Contains(text, want) {
			t.Fatalf("assignment lacks %q: %s", want, text)
		}
	}
	mergeDirectly(t, f, repo, stream, "resume", map[string]string{"resume.txt": "implemented\n"})
	held, err = assignmentHeld(repo, stream, "dedupe", masonRole)
	must(t, err)
	if held {
		t.Fatal("landed prerequisite still holds dependent work")
	}
}

func TestFactoryContextReadsPriorRevisionsAndEnforcesTurnScope(t *testing.T) {
	t.Parallel()
	c := newContestFixture(t, "factory-history")
	scope := coreadapter.Scope{Project: string(c.repo.Project()), Workstream: string(c.stream), Role: trace.ChiefOfStaff, Thread: trace.ChiefOfStaff, Turn: "events_1"}
	docs, err := trace.Read[trace.Document](c.repo, c.stream)
	must(t, err)
	var spec trace.Document
	for _, d := range docs {
		if d.ID == plan.SpecDocument {
			spec = d
		}
	}
	spec.Revision++
	spec.Content = amendedSpec
	spec.Cause = "fixture"
	must(t, c.repo.RecordDocuments(context.Background(), []trace.Document{spec}))
	tool := factoryContext(c.repo, scope)
	raw, err := tool.Handle(context.Background(), json.RawMessage(`{"path":"spec.md","revision":1}`))
	must(t, err)
	var got struct {
		Content  string `json:"content"`
		Revision int    `json:"revision"`
	}
	must(t, json.Unmarshal(raw, &got))
	if got.Revision != 1 || got.Content != validSpec {
		t.Fatalf("wrong historical evidence: %s", raw)
	}
	// Remote evidence remains citable and readable even when a JSON line exceeds the response bound.
	source := spec
	source.ID = "source-fixture"
	source.Path = "sources/source-fixture.json"
	source.Revision = 1
	source.Content = `{"content":"` + strings.Repeat("é", 40000) + `"}`
	must(t, c.repo.RecordDocuments(context.Background(), []trace.Document{source}))
	must(t, questions.Resolve(context.Background(), c.repo, c.stream, "source#source-fixture", c.f.clock.Now()))
	if err := questions.Resolve(context.Background(), c.repo, c.stream, "source#missing", c.f.clock.Now()); err == nil {
		t.Fatal("invented source resolved")
	}
	var recovered strings.Builder
	offset := 0
	for {
		input, _ := json.Marshal(map[string]any{"path": source.Path, "offset": offset})
		raw, err := tool.Handle(context.Background(), input)
		must(t, err)
		var page struct {
			Content string `json:"content"`
			Next    int    `json:"next_offset"`
		}
		must(t, json.Unmarshal(raw, &page))
		recovered.WriteString(page.Content)
		if page.Next == 0 {
			break
		}
		if page.Next <= offset {
			t.Fatal("pagination did not advance")
		}
		offset = page.Next
	}
	if recovered.String() != source.Content {
		t.Fatal("large source was truncated or corrupted")
	}
	scope.Turn = "not-claimed"
	if _, err := factoryContext(c.repo, scope).Handle(context.Background(), json.RawMessage(`{}`)); err == nil {
		t.Fatal("unclaimed turn read factory state")
	}
}

func TestFactoryContextListsFolders(t *testing.T) {
	t.Parallel()
	c := newContestFixture(t, "factory-folders")
	var recorded []trace.Document
	for _, id := range []string{"discovery-a", "discovery-b"} {
		recorded = append(recorded, trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: id, Revision: 1, Project: c.repo.Project(), Workstream: c.stream, At: c.f.clock.Now(), Actor: chiefActor, Cause: "fixture"}, Path: "discoveries/" + id + ".json", Content: "{}"})
	}
	must(t, c.repo.RecordDocuments(context.Background(), recorded))
	scope := coreadapter.Scope{Project: string(c.repo.Project()), Workstream: string(c.stream), Role: trace.ChiefOfStaff, Thread: trace.ChiefOfStaff, Turn: "events_1"}
	tool := factoryContext(c.repo, scope)
	for _, path := range []string{"discoveries", "discoveries/"} {
		input, _ := json.Marshal(map[string]any{"path": path})
		raw, err := tool.Handle(context.Background(), input)
		must(t, err)
		var listed map[string]int
		must(t, json.Unmarshal(raw, &listed))
		if len(listed) != 2 || listed["discoveries/discovery-a.json"] != 1 || listed["discoveries/discovery-b.json"] != 1 {
			t.Fatalf("folder %q listed %s", path, raw)
		}
	}
	for input, want := range map[string]string{
		`{"path":"missing"}`:                  "without a path to list documents",
		`{"path":"discoveries","revision":1}`: "is a folder",
		`{"path":"spec.md","revision":99}`:    "its latest is 1",
	} {
		_, err := tool.Handle(context.Background(), json.RawMessage(input))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: got error %v, want it to mention %q", input, err, want)
		}
	}
}

func TestFactoryContextReadsCompleteCheckOutput(t *testing.T) {
	t.Parallel()
	c := newContestFixture(t, "check-output")
	captured := "failure at start\n" + strings.Repeat("é", 70000) + "\nfailure at end\n"
	path := "units/resume/checks-99-output.txt"
	doc := trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: "unit-resume-checks-99-output", Revision: 1, Project: c.repo.Project(), Workstream: c.stream, At: c.f.clock.Now(), Actor: checksActor, Cause: "fixture"}, Path: path, Content: captured}
	must(t, c.repo.RecordDocuments(context.Background(), []trace.Document{doc}))
	scope := coreadapter.Scope{Project: string(c.repo.Project()), Workstream: string(c.stream), Role: trace.ChiefOfStaff, Thread: trace.ChiefOfStaff, Turn: "events_1"}
	tool := factoryContext(c.repo, scope)
	var recovered strings.Builder
	offset := 0
	for {
		input, _ := json.Marshal(map[string]any{"path": path, "start": 1, "lines": 1000, "offset": offset})
		raw, err := tool.Handle(context.Background(), input)
		must(t, err)
		var page struct {
			Content string `json:"content"`
			Next    int    `json:"next_offset"`
		}
		must(t, json.Unmarshal(raw, &page))
		recovered.WriteString(page.Content)
		if page.Next == 0 {
			break
		}
		if page.Next <= offset {
			t.Fatal("check output pagination did not advance")
		}
		offset = page.Next
	}
	if recovered.String() != captured {
		t.Fatal("captured check output was truncated or changed")
	}
}

func TestChiefInspectsMasonCandidateAndCommissionsWork(t *testing.T) {
	t.Parallel()
	c := newContestFixture(t, "chief-engineering-tools")
	scope := coreadapter.Scope{Project: string(c.repo.Project()), Workstream: string(c.stream), Role: trace.ChiefOfStaff, Thread: trace.ChiefOfStaff, Turn: "events_1"}
	raw, err := inspectCode(c.f.s.about(c.repo), c.repo, scope, c.f.clock.Now).Handle(context.Background(), json.RawMessage(`{"unit":"resume","path":"`+masonWrote+`"}`))
	must(t, err)
	var evidence struct {
		trace.CodeInspection
		Citation string `json:"citation"`
	}
	must(t, json.Unmarshal(raw, &evidence))
	if evidence.Commit == "" || evidence.Content != "package trace\n" || evidence.Citation == "" {
		t.Fatalf("missing candidate evidence: %s", raw)
	}
	w, _, found, err := newUnitWorkspaces(c.f.s.cfg, c.repo).find(context.Background(), c.stream, "resume")
	must(t, err)
	if !found {
		t.Fatal("unit workspace missing")
	}
	must(t, os.WriteFile(filepath.Join(w.Path, "unfinished.txt"), []byte("captured after the report\n"), 0600))
	latest, err := inspectCode(c.f.s.cfg, c.repo, scope, c.f.clock.Now).Handle(context.Background(), json.RawMessage(`{"unit":"resume","captured":true,"path":"unfinished.txt"}`))
	must(t, err)
	var captured trace.CodeInspection
	must(t, json.Unmarshal(latest, &captured))
	if captured.Commit == evidence.Commit || captured.Content != "captured after the report\n" {
		t.Fatalf("captured inspection reused stale report: %s", latest)
	}
	tools, err := questions.Tools(c.repo, trace.ChiefOfStaff, scope, c.f.clock.Now)
	must(t, err)
	for _, tool := range tools {
		if tool.Name != questions.RouteAmendmentTool {
			continue
		}
		input := json.RawMessage(`{"citations":["spec#1"],"change":"Add a bounded investigation of interrupted transfers to the execution plan","reason":"The implementation revealed an unverified assumption; preserve approved intent"}`)
		first, err := tool.Handle(context.Background(), input)
		must(t, err)
		if !strings.Contains(string(first), `"recorded":true`) {
			t.Fatal(string(first))
		}
		second, err := tool.Handle(context.Background(), input)
		must(t, err)
		if string(first) != string(second) {
			t.Fatalf("repeated request duplicated work: %s %s", first, second)
		}
		state, err := c.repo.Workflow(c.stream, trace.UnitSubject("resume"))
		must(t, err)
		if state.Value != UnitReviewing {
			t.Fatalf("chief's own request parked unrelated unit: %s", state.Value)
		}
		return
	}
	t.Fatal("chief lacks replanning tool")
}

func TestChiefRecoveryCannotApproveItsOwnCandidate(t *testing.T) {
	t.Parallel()
	m := newMoveFixture(t, "independent-review")
	out := m.moveUnit(t, `{"unit":"resume","to":"approved","note":"The change appears correct"}`)
	if !strings.Contains(out, `"recorded":false`) || !strings.Contains(out, "independent review") {
		t.Fatal(out)
	}
	if m.state(t).Value != UnitReviewing {
		t.Fatal("chief bypassed independent review")
	}
}

func TestRecoveryDecisionDoesNotResetLoopGuard(t *testing.T) {
	t.Parallel()
	c := newContestFixture(t, "recovery-progress")
	before, _, err := lastProgress(c.repo, c.stream)
	must(t, err)
	c.contest(t)
	out := c.resolve(t, `{"unit":"resume","decision":"review","note":"Use the existing verification evidence"}`)
	if !strings.Contains(out, `"recorded":true`) {
		t.Fatal(out)
	}
	after, _, err := lastProgress(c.repo, c.stream)
	must(t, err)
	if !after.Equal(before) {
		t.Fatal("a recovery instruction reset the no-progress budget")
	}
}

// raiseFixtureContest records explicit escalation for owner-inbox fixtures.
func raiseFixtureContest(t *testing.T, r *trace.Repository, stream config.WorkstreamID, unit string) {
	t.Helper()
	contest, found, err := unitContest(r, stream, unit)
	must(t, err)
	if !found {
		t.Fatal("fixture unit has no contest")
	}
	data, err := json.Marshal(ChiefContestDecision{Contest: contest.ID, Decision: escalateContest, Turn: "fixture"})
	must(t, err)
	h := contest.Header
	h.Schema = "osmia.trace.document"
	h.ID = trace.UnitSubject(unit) + "-chief-" + contest.ID
	h.Revision = 1
	h.Actor = chiefActor
	h.Cause = contest.ID
	must(t, r.RecordDocuments(context.Background(), []trace.Document{{Header: h, Path: chiefContestPath(unit, contest.ID), Content: string(data)}}))
}

func TestReplanningWaitsForWritersWithoutHoldingReadOnlyReview(t *testing.T) {
	t.Parallel()
	f, stream, repo := newReviewFixture(t, "active-ownership")
	m := newMasonController(f.s, repo)
	ctx := context.Background()
	b, _, err := m.read(stream)
	must(t, err)
	started, blocked, err := m.start(ctx, b, "dedupe")
	must(t, err)
	if !started || blocked {
		t.Fatal("fixture did not start neighboring assignment")
	}
	directory := t.TempDir()
	q, err := repo.ClaimTurn(ctx, stream, masonAgent("dedupe"), "writer", directory, f.clock.Now())
	must(t, err)
	idle, err := revisionWorkersIdle(repo, stream, []string{"dedupe"})
	must(t, err)
	if idle {
		t.Fatal("assignment revised underneath an active writer")
	}
	held, err := assignmentHeld(repo, stream, "resume", masonRole)
	must(t, err)
	if !held {
		t.Fatal("overlapping implementation was dispatched")
	}
	held, err = assignmentHeld(repo, stream, "resume", reviewerRole)
	must(t, err)
	if held {
		t.Fatal("read-only review waited for an unrelated writer")
	}
	h := q.Request.Header
	h.Schema = "osmia.trace.turn-response"
	h.ID = trace.EventID(q.Request.ID, "response")
	h.At = f.clock.Now()
	response := trace.TurnResponse{Header: h, AgentID: masonAgent("dedupe"), ThreadID: masonAgent("dedupe"), TurnID: q.Request.TurnID, RequestID: q.Request.ID, RequestRevision: q.Request.Revision, Result: coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: q.Request.Profile.Backend, ID: "writer-session"}, SessionDirectory: directory, StartedAt: q.Claim.At, FinalResponse: "Investigation recorded"}}
	must(t, repo.CaptureTurn(ctx, q.Claim.Token, response))
	must(t, repo.CompleteTurn(ctx, stream, masonAgent("dedupe"), q.Request.TurnID, q.Claim.Token, f.clock.Now()))
	idle, err = revisionWorkersIdle(repo, stream, []string{"dedupe"})
	must(t, err)
	if !idle {
		t.Fatal("completed writer permanently blocked replanning")
	}
	w, _, found, err := newUnitWorkspaces(f.s.cfg, repo).find(ctx, stream, "dedupe")
	must(t, err)
	if !found {
		t.Fatal("captured unit workspace disappeared")
	}
	must(t, os.WriteFile(filepath.Join(w.Path, "investigation.txt"), []byte("work captured before done\n"), 0600))
	scope := chiefTurn(t, f, repo, stream, "inspect_unfinished")
	raw, err := inspectCode(f.s.cfg, repo, scope, f.clock.Now).Handle(ctx, json.RawMessage(`{"unit":"dedupe","path":"investigation.txt"}`))
	must(t, err)
	var evidence trace.CodeInspection
	must(t, json.Unmarshal(raw, &evidence))
	if evidence.Commit == "" || evidence.Content != "work captured before done\n" {
		t.Fatalf("chief cannot inspect unfinished captured work: %s", raw)
	}
}

// TestRemoteFileLogsOneGitHubActivityRecordPerRequest shows one record for
// the commit resolution and one for the content read, each with the
// marker, a read access, the repository and a status, whether the overall
// call succeeds or a later request fails.
func TestRemoteFileLogsOneGitHubActivityRecordPerRequest(t *testing.T) {
	t.Parallel()
	const commit = "0123456789012345678901234567890123456789"
	calls := 0
	status := http.StatusOK
	client := &http.Client{Transport: remoteFileTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		body := `{"sha":"` + commit + `"}`
		if calls == 2 {
			if status != http.StatusOK {
				return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(`{}`)), Header: http.Header{}}, nil
			}
			body = `{"type":"file","encoding":"base64","content":"` + base64.StdEncoding.EncodeToString([]byte("# Referenced design\n")) + `"}`
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
	})}
	var buf bytes.Buffer
	ctx := ghlog.WithLogger(context.Background(), slog.New(slog.NewJSONHandler(&buf, nil)))
	ctx = ghlog.WithScope(ctx, ghlog.Scope{Project: "p_1", Workstream: "w_1", Unit: "u_1"})

	if _, err := githubFile(ctx, client, "fixture-token", remoteFile{Repository: "https://github.com/example/project.git", Ref: "main", Path: "docs/design.md"}); err != nil {
		t.Fatal(err)
	}
	status = http.StatusForbidden
	calls = 0
	if _, err := githubFile(ctx, client, "fixture-token", remoteFile{Repository: "https://github.com/example/project.git", Ref: "main", Path: "docs/design.md"}); err == nil {
		t.Fatal("expected the contents request to fail")
	}

	var records []struct {
		Activity, Access, Action, Repository, Method, Project, Workstream, Unit string
		Status                                                                  int
	}
	for _, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		var r struct {
			Activity, Access, Action, Repository, Method, Project, Workstream, Unit string
			Status                                                                  int
		}
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("line does not parse as JSON: %s: %v", line, err)
		}
		records = append(records, r)
	}
	if len(records) != 4 {
		t.Fatalf("records %+v", records)
	}
	wantActions := []string{"remote_file.commit", "remote_file.contents", "remote_file.commit", "remote_file.contents"}
	wantStatus := []int{http.StatusOK, http.StatusOK, http.StatusOK, http.StatusForbidden}
	for i, r := range records {
		if r.Activity != ghlog.MarkerValue || r.Access != ghlog.Read || r.Action != wantActions[i] || r.Method != http.MethodGet ||
			r.Repository != "example/project" || r.Status != wantStatus[i] || r.Project != "p_1" || r.Workstream != "w_1" || r.Unit != "u_1" {
			t.Fatalf("record %d: %+v", i, r)
		}
	}
}
