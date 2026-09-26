package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/kpenfound/osmia/internal/workspace"
)

// requireJJ skips the test when jj is not on PATH, and fails it there when
// OSMIA_REQUIRE_JJ says jj must be, as in the dagger test containers.
func requireJJ(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("jj"); err != nil {
		if os.Getenv("OSMIA_REQUIRE_JJ") != "" {
			t.Fatalf("jj is required but not on PATH: %v", err)
		}
		t.Skip("jj is not installed; Jujutsu workspaces need it")
	}
}

// onWorkspaces has the configuration of the fixture under opts hand new
// workstreams in on backend.
func onWorkspaces(t *testing.T, opts Options, backend string) {
	t.Helper()
	if backend == config.WorkspacesGit {
		return
	}
	if backend == config.WorkspacesJujutsu {
		requireJJ(t)
	}
	path := filepath.Join(opts.Config.Root, "config.toml")
	data, err := os.ReadFile(path)
	must(t, err)
	lines := strings.Split(string(data), "\n")
	i := slices.IndexFunc(lines, func(line string) bool { return strings.HasPrefix(line, "workspaces = ") })
	if i < 0 {
		t.Fatalf("%s sets no workspaces", path)
	}
	lines[i] = fmt.Sprintf("workspaces = %q", backend)
	must(t, os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0600))
}

// backendOf returns the workspace backend the workstream's manifest records.
func backendOf(t *testing.T, f *shedFixture, stream config.WorkstreamID) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.trace, "workstreams", string(stream), "workstream.json"))
	must(t, err)
	var manifest struct {
		Workspaces string `json:"workspaces"`
	}
	must(t, json.Unmarshal(data, &manifest))
	if manifest.Workspaces == "" {
		return config.WorkspacesGit
	}
	return manifest.Workspaces
}

var (
	uploadReport = CriterionReport{Criterion: "spec#1", Done: "send chunks", Evidence: "TestUpload passes", Proof: "internal/upload/upload_test.go TestUpload"}
	auditReport  = CriterionReport{Criterion: "spec#2", Done: "record acknowledged chunks", Evidence: "acknowledgements are recorded", Proof: "reviewer judgement"}
)

// jujutsuConflicts are the unit files newJujutsuRebaseFixture writes: resume
// and audit each add masonWrote as the landing does, differently, and upload
// adds a file of its own.
var jujutsuConflicts = map[string]map[string]string{
	"resume": {masonWrote: "package trace\n\n// resume\n"},
	"audit":  {masonWrote: "package trace\n\n// audit\n"},
	"upload": {"internal/upload/upload.go": "package upload\n"},
}

// newJujutsuRebaseFixture builds disjointPlan on Jujutsu workspaces with the
// service stopped once resume's mason reported done: resume is reviewing,
// and upload and audit are implementing with a done turn each. Every unit's
// workspace gets its jujutsuConflicts, a landing moves the feature branch to
// a masonWrote of its own, and one foreman pass asks to rebase all three,
// which are then rebased. It returns the workstream, the open trace, the
// foreman and the landed tip.
func newJujutsuRebaseFixture(t *testing.T, key string) (*shedFixture, config.WorkstreamID, *trace.Repository, *foreman, string) {
	t.Helper()
	ctx := context.Background()
	f, masons := newParallelMasonFixtureOn(t, config.WorkspacesJujutsu, 1, 3, disjointPlan)
	masons.play[masonTurnID("resume")] = reportDone("Built")
	stream, _ := f.builtAs(t, key)
	f.awaitUnit(t, stream, "resume", UnitReviewing)
	masons.check(t)
	f.stop(t)
	repository, err := trace.Open(f.s.cfg.Root, f.s.cfg.Project)
	must(t, err)
	t.Cleanup(func() { repository.Close() })
	if backend, err := repository.Workspaces(stream); err != nil || backend != config.WorkspacesJujutsu {
		t.Fatalf("workstream %s is on %q: %v", stream, backend, err)
	}
	m := newMasonController(f.s, repository)
	for unit, report := range map[string]CriterionReport{"upload": uploadReport, "audit": auditReport} {
		b, found, err := m.read(stream)
		must(t, err)
		if !found {
			t.Fatal("the workstream is not building")
		}
		if started, _, err := m.start(ctx, b, unit); err != nil || !started {
			t.Fatalf("%s did not start: %v", unit, err)
		}
		completeMasonTurn(t, f, repository, stream, unit, report)
	}
	units := newUnitWorkspaces(f.s.cfg, repository)
	for unit, files := range jujutsuConflicts {
		w, _, found, err := units.find(ctx, stream, unit)
		must(t, err)
		if !found {
			t.Fatalf("unit %s has no workspace", unit)
		}
		for name, content := range files {
			must(t, os.MkdirAll(filepath.Dir(filepath.Join(w.Path, name)), 0700))
			must(t, os.WriteFile(filepath.Join(w.Path, name), []byte(content), 0600))
		}
	}
	landed := moveFeature(t, f, stream, map[string]string{masonWrote: "package trace\n\n// landed\n"})
	lands := &foreman{masons: m}
	must(t, lands.Pass(ctx))
	var ops []trace.OperationRecord
	for _, unit := range []string{"resume", "upload", "audit"} {
		asked := rebaseOperations(t, repository, stream, unit)
		if len(asked) != 1 {
			t.Fatalf("one pass asked to rebase unit %s %d times", unit, len(asked))
		}
		ops = append(ops, asked...)
	}
	for _, op := range ops {
		if result, err := (rebaser{lands}).Apply(ctx, op.Operation); err != nil || result.Outcome != "succeeded" {
			t.Fatalf("rebase %s: %+v %v", op.Operation.ID, result, err)
		}
	}
	return f, stream, repository, lands, landed
}

// On Jujutsu workspaces, one landing rebases every unit in flight: the two
// whose change conflicts with it hold the conflict stored in their rebased
// commit and materialized with markers in their workspace, and the third is
// rebased cleanly. One pass then routes each conflicted unit, reviewing or
// implementing, to its own mason, whose turns are queued side by side, with
// the markers explained. A resolved unit goes to review with a candidate
// that holds no conflict and a diff with no marker; one whose mason left the
// conflict is never offered for review and gets a reminder. Nothing lands.
func TestJujutsuRebaseStoresEachUnitsConflictAndRoutesItToItsMason(t *testing.T) {
	t.Parallel()
	f, stream, repository, lands, landed := newJujutsuRebaseFixture(t, "jj-conflicts")
	ctx := context.Background()
	m := lands.masons
	units := newUnitWorkspaces(f.s.cfg, repository)
	g := providerOf(t, units.streamWorkspaces, stream)
	if _, ok := g.(*workspace.Jujutsu); !ok {
		t.Fatalf("the unit workspaces are %T", g)
	}

	for _, unit := range []string{"resume", "audit"} {
		rebases := unitRebases(t, repository, stream, unit)
		if len(rebases) != 1 || !slices.Equal(rebases[0].Conflicts, []string{masonWrote}) || rebases[0].Onto != landed {
			t.Fatalf("unit %s's rebases %+v", unit, rebases)
		}
		if stored, err := g.StoredConflicts(ctx, rebases[0].Commit); err != nil || !slices.Equal(stored, []string{masonWrote}) {
			t.Fatalf("unit %s's rebased commit %s stores %v: %v", unit, rebases[0].Commit, stored, err)
		}
		if c, err := g.Commit(ctx, rebases[0].Commit); err != nil || !slices.Equal(c.Parents, []string{landed}) {
			t.Fatalf("unit %s's rebased commit %+v: %v", unit, c, err)
		}
		w, _, _, err := units.find(ctx, stream, unit)
		must(t, err)
		data, err := os.ReadFile(filepath.Join(w.Path, masonWrote))
		must(t, err)
		lines := strings.Split(string(data), "\n")
		for _, want := range []string{"<<<<<<< ", "// landed", "||||||| ", "=======", "// " + unit, ">>>>>>> "} {
			if !slices.ContainsFunc(lines, func(line string) bool { return strings.HasPrefix(line, want) }) {
				t.Fatalf("unit %s's conflicted file lacks a line %q:\n%s", unit, want, data)
			}
		}
	}
	rebases := unitRebases(t, repository, stream, "upload")
	if len(rebases) != 1 || len(rebases[0].Conflicts) != 0 || rebases[0].Onto != landed {
		t.Fatalf("upload's rebases %+v", rebases)
	}
	if stored, err := g.StoredConflicts(ctx, rebases[0].Commit); err != nil || len(stored) != 0 {
		t.Fatalf("upload's rebased commit stores %v: %v", stored, err)
	}
	w, _, _, err := units.find(ctx, stream, "upload")
	must(t, err)
	for name, want := range map[string]string{masonWrote: "package trace\n\n// landed\n", "internal/upload/upload.go": "package upload\n"} {
		if data, err := os.ReadFile(filepath.Join(w.Path, name)); err != nil || string(data) != want {
			t.Fatalf("upload's %s after its rebase: %q %v", name, data, err)
		}
	}

	must(t, lands.Pass(ctx))
	if ops := landOperations(t, repository, stream); len(ops) != 0 {
		t.Fatalf("a landing was asked for with conflicted units: %+v", ops)
	}
	for _, unit := range []string{"resume", "audit"} {
		if state, err := repository.Workflow(stream, trace.UnitSubject(unit)); err != nil || state.Value != UnitImplementing {
			t.Fatalf("the conflicted unit %s is %+v: %v", unit, state, err)
		}
		th, err := repository.Thread(stream, masonAgent(unit))
		must(t, err)
		resolve := th.Turns[len(th.Turns)-1]
		if resolve.Request.TurnID != resolveTurnID(unit, 1) || !resolve.CompletedAt.IsZero() {
			t.Fatalf("unit %s's mason's last turn is %s", unit, resolve.Request.TurnID)
		}
		for _, want := range []string{"- " + masonWrote + "\n", `the lines between "<<<<<<<" and "|||||||" are the feature branch's`, `those between "=======" and ">>>>>>>" are your unit's`, "## end of spec.md"} {
			if !strings.Contains(resolve.Request.Prompt, want) {
				t.Fatalf("unit %s's resolve turn lacks %q:\n%s", unit, want, resolve.Request.Prompt)
			}
		}
	}
	if th, err := repository.Thread(stream, masonAgent("upload")); err != nil || th.Turns[len(th.Turns)-1].Request.TurnID == resolveTurnID("upload", 1) {
		t.Fatalf("the cleanly rebased unit's mason got a resolve turn: %v", err)
	}

	resume, _, _, err := units.find(ctx, stream, "resume")
	must(t, err)
	must(t, os.WriteFile(filepath.Join(resume.Path, masonWrote), []byte("package trace\n\n// landed and resumed\n"), 0600))
	completeMasonTurn(t, f, repository, stream, "resume", resumeReport)
	done := completeMasonTurn(t, f, repository, stream, "audit", auditReport)
	must(t, m.Pass(ctx))

	for unit, want := range map[string]string{"resume": "+// landed and resumed", "upload": "+package upload"} {
		if state, err := repository.Workflow(stream, trace.UnitSubject(unit)); err != nil || state.Value != UnitReviewing {
			t.Fatalf("unit %s is %+v: %v", unit, state, err)
		}
		_, report := latestReport(t, repository, stream, unit)
		if report.Base != landed {
			t.Fatalf("unit %s's report %+v, want a candidate from %s", unit, report, landed)
		}
		if stored, err := g.StoredConflicts(ctx, report.Candidate); err != nil || len(stored) != 0 {
			t.Fatalf("unit %s's candidate stores %v: %v", unit, stored, err)
		}
		req, _, err := m.candidateEvidence(ctx, stream, unit)
		must(t, err)
		if strings.Contains(req.Diff, "<<<<<<<") || strings.Contains(req.Diff, ">>>>>>>") || strings.Contains(req.Diff, "jjconflict") || !strings.Contains(req.Diff, want) {
			t.Fatalf("unit %s's reviewer diff:\n%s", unit, req.Diff)
		}
	}
	if state, err := repository.Workflow(stream, trace.UnitSubject("audit")); err != nil || state.Value != UnitImplementing {
		t.Fatalf("a candidate holding its conflict left implementing: %+v %v", state, err)
	}
	if doc, _ := latestReport(t, repository, stream, "audit"); doc.Revision != 0 {
		t.Fatalf("a candidate holding its conflict was reported as revision %d", doc.Revision)
	}
	th, err := repository.Thread(stream, masonAgent("audit"))
	must(t, err)
	remind := th.Turns[len(th.Turns)-1]
	if remind.Request.TurnID != fmt.Sprintf("%s-markers-%d", masonAgent("audit"), done.Sequence) || !strings.Contains(remind.Request.Prompt, "still carry conflict markers") || !strings.Contains(remind.Request.Prompt, masonWrote) {
		t.Fatalf("the reminder %s:\n%s", remind.Request.TurnID, remind.Request.Prompt)
	}
}

// plantUnit moves the unit to state to, as the test decides.
func plantUnit(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, unit, to string) {
	t.Helper()
	subject := trace.UnitSubject(unit)
	state, err := repository.Workflow(stream, subject)
	must(t, err)
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: subject + "-planted-" + to, Revision: 1, Project: repository.Project(), Workstream: stream, Unit: unit, At: time.Now().UTC(), Actor: foremanActor, Cause: "test"}
	_, err = repository.Transact(context.Background(), trace.Transaction{ExpectedVersion: state.Version,
		Transition: trace.Transition{Header: h, Subject: subject, From: state.Value, To: to, Reason: "the test moves it"}})
	must(t, err)
}

// plantDocument records the next revision of the unit's document id at path
// holding value.
func plantDocument(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, unit, id, path string, value any) trace.Document {
	t.Helper()
	docs, err := trace.Read[trace.Document](repository, stream)
	must(t, err)
	revision := 1
	for _, d := range docs {
		if d.ID == id {
			revision = d.Revision + 1
		}
	}
	content, err := json.MarshalIndent(value, "", "  ")
	must(t, err)
	doc := trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: id, Revision: revision, Project: repository.Project(), Workstream: stream, Unit: unit, At: time.Now().UTC(), Actor: foremanActor, Cause: "test"},
		Path: path, Content: string(content) + "\n"}
	must(t, repository.RecordDocuments(context.Background(), []trace.Document{doc}))
	return doc
}

// A candidate that holds a stored conflict is refused at both gates even
// when the trace names it: the reviewer's bundle is not assembled, so no
// reviewer turn is queued and why is recorded, and its approval does not
// land, so the feature branch stays.
func TestJujutsuCandidateHoldingAConflictIsNeitherReviewedNorLanded(t *testing.T) {
	t.Parallel()
	f, stream, repository, lands, landed := newJujutsuRebaseFixture(t, "jj-gates")
	ctx := context.Background()
	rebases := unitRebases(t, repository, stream, "audit")
	if len(rebases) != 1 || len(rebases[0].Conflicts) == 0 {
		t.Fatalf("audit's rebases %+v", rebases)
	}
	candidate := rebases[0].Commit
	if tip, _, err := providerOf(t, newUnitWorkspaces(f.s.cfg, repository).streamWorkspaces, stream).Branch(ctx, unitBranch(stream, "audit")); err != nil || tip != candidate {
		t.Fatalf("audit's branch is at %s, %v; want %s", tip, err, candidate)
	}
	latest, _, _, err := seal.Latest(repository, stream)
	must(t, err)
	report := plantDocument(t, repository, stream, "audit", reportDocument("audit"), fmt.Sprintf(reportPath, "audit"),
		UnitReport{Unit: "audit", Turn: "test", Seal: latest.Seal, Outcome: "Built", Criteria: []CriterionReport{auditReport}, Branch: unitBranch(stream, "audit"), Base: landed, Candidate: candidate})
	plantUnit(t, repository, stream, "audit", UnitReviewing)
	refused := fmt.Sprintf("candidate %s holds unresolved conflicts in %s", candidate, masonWrote)

	r := &reviewers{masons: lands.masons}
	state, err := repository.Workflow(stream, trace.UnitSubject("audit"))
	must(t, err)
	must(t, r.one(ctx, stream, "audit", state, false))
	if th, err := repository.Thread(stream, reviewerAgent("audit")); err == nil && len(th.Turns) != 0 || err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the candidate holding a conflict was offered for review: %+v %v", th.Turns, err)
	}
	blocked := slices.IndexFunc(allTransitions(t, f.trace, stream), func(tr trace.Transition) bool {
		return tr.Subject == reviewBlockedSubject("audit") && tr.Reason == "unit audit stays reviewing: its reviewer bundle cannot be assembled: "+refused
	})
	if blocked < 0 {
		t.Fatal("the refused review is not recorded")
	}

	review := plantDocument(t, repository, stream, "audit", reviewDocument("audit"), "units/audit/review.json", UnitReviewResult{
		Identity: UnitReviewIdentity{Subject: string(stream) + "/audit", Candidate: coreadapter.Candidate{Revision: candidate, BaseRevision: landed, SpecRevision: fmt.Sprint(latest.Revision.Spec), PlanRevision: fmt.Sprint(latest.Revision.Plan)}, Report: fmt.Sprintf("%s revision %d", report.Path, report.Revision), Seal: latest.Seal},
		Turn:     "test", Verdict: UnitVerdict{Decision: "satisfactory", Evidence: []ReviewEvidence{{Criterion: "spec#2", Evidence: "acknowledgements are recorded"}}}})
	plantUnit(t, repository, stream, "audit", UnitApproved)
	_, result, satisfactory, err := lands.approval(stream, "audit", review.Revision)
	if err != nil || !satisfactory {
		t.Fatalf("the planted approval does not hold: %v", err)
	}
	must(t, lands.request(ctx, stream, "audit", review, result))
	ops := landOperations(t, repository, stream)
	if len(ops) != 1 {
		t.Fatalf("landings %+v", ops)
	}
	landing, err := lands.Apply(ctx, ops[0].Operation)
	if err != nil || landing.Outcome != "failed" || landing.Evidence != "unit audit did not land: "+refused {
		t.Fatalf("the landing of a candidate holding a conflict: %+v %v", landing, err)
	}
	if tip := featureTip(t, f, stream); tip != landed {
		t.Fatalf("the feature branch moved to %s", tip)
	}
	if state, err := repository.Workflow(stream, trace.UnitSubject("audit")); err != nil || state.Value != UnitApproved {
		t.Fatalf("the refused unit is %+v: %v", state, err)
	}
}

// A drift rebase that conflicts on Jujutsu workspaces stops its replay with
// the conflict stored and the markers, explained to the drift mason, in the
// resolution workspace. A resolution that leaves them is not reviewed; the
// resolved candidate a reviewer reads holds no conflict and shows no marker,
// and only its approval moves the feature branch.
func TestJujutsuDriftConflictIsResolvedInItsWorkspaceBeforeReview(t *testing.T) {
	t.Parallel()
	f := newDebateFixtureWith(t, 1, 1, "", func(opts *Options) { onWorkspaces(t, *opts, config.WorkspacesJujutsu) })
	f.upstream(t)
	stream, _ := f.builtAs(t, "jj-drift")
	f.stop(t)
	repository, err := trace.Open(f.s.cfg.Root, f.s.cfg.Project)
	must(t, err)
	t.Cleanup(func() { repository.Close() })
	d := drifter{&foreman{masons: newMasonController(f.s, repository)}}
	ctx := context.Background()
	before, upstream, op := conflictedDrift(t, f, d, stream)

	awaitResolution(t, f.s, d, stream, op, "its mason's resolution of CODEOWNERS")
	w := resolutionWorkspace(t, f, stream)
	g := providerOf(t, driftWorkspaces(f.s.cfg, repository), stream)
	if _, ok := g.(*workspace.Jujutsu); !ok {
		t.Fatalf("the drift workspaces are %T", g)
	}
	if stop, conflicts, replaying, err := g.Replaying(ctx, w); err != nil || !replaying || stop != before || !slices.Equal(conflicts, []string{"CODEOWNERS"}) {
		t.Fatalf("the replay stopped at %s with %v (%t): %v", stop, conflicts, replaying, err)
	}
	data, err := os.ReadFile(filepath.Join(w.Path, "CODEOWNERS"))
	must(t, err)
	for _, want := range []string{"<<<<<<< ", "@upstream", "||||||| ", "=======", "@feature", ">>>>>>> "} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("the resolution workspace's CODEOWNERS lacks %q:\n%s", want, data)
		}
	}
	th, err := repository.Thread(stream, driftMasonAgent)
	must(t, err)
	for _, want := range []string{"- CODEOWNERS\n", `the lines between "<<<<<<<" and "|||||||" are the branch as rebased onto upstream so far`, `those between "=======" and ">>>>>>>" are the commit being replayed`} {
		if !strings.Contains(th.Turns[0].Request.Prompt, want) {
			t.Fatalf("the resolve turn lacks %q:\n%s", want, th.Turns[0].Request.Prompt)
		}
	}

	left := completeDriftTurn(t, f, repository, stream, driftMasonAgent, resolvedDone("Left it"))
	awaitResolution(t, f.s, d, stream, op, "its mason's resolution of CODEOWNERS")
	if ids := turnIDs(t, repository, stream, driftMasonAgent); !slices.Equal(ids, []string{driftResolveTurnID(1, 1), fmt.Sprintf("%s-markers-%d", driftMasonAgent, left.Sequence)}) {
		t.Fatalf("drift mason turns %v", ids)
	}
	if _, err := repository.Thread(stream, driftReviewerAgent); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a resolution holding its conflict went to review: %v", err)
	}

	must(t, os.WriteFile(filepath.Join(w.Path, "CODEOWNERS"), []byte("/internal/ @upstream @feature\n"), 0600))
	completeDriftTurn(t, f, repository, stream, driftMasonAgent, resolvedDone("Kept both owners"))
	awaitResolution(t, f.s, d, stream, op, "review 1 of candidate")
	records := driftRecords(t, repository, stream)
	candidate := records[len(records)-1].Candidate
	if stored, err := g.StoredConflicts(ctx, candidate); err != nil || len(stored) != 0 || parentOf(t, f, candidate) != upstream {
		t.Fatalf("the resolved candidate %s stores %v: %v", candidate, stored, err)
	}
	review, err := repository.Thread(stream, driftReviewerAgent)
	must(t, err)
	prompt := review.Turns[0].Request.Prompt
	if strings.Contains(prompt, "<<<<<<<") || strings.Contains(prompt, ">>>>>>>") || strings.Contains(prompt, "jjconflict") || !strings.Contains(prompt, "+/internal/ @upstream @feature") {
		t.Fatalf("the drift reviewer reads:\n%s", prompt)
	}
	if tip := featureTip(t, f, stream); tip != before {
		t.Fatalf("the feature branch moved to %s before review", tip)
	}

	completeDriftTurn(t, f, repository, stream, driftReviewerAgent, driftVerdict(t, approvedResolution))
	if result, err := attemptOperation(t, f.s, repository, stream, op, d); err != nil || result.Outcome != "succeeded" {
		t.Fatalf("the approved drift rebase %+v %v", result, err)
	}
	if tip := featureTip(t, f, stream); tip != candidate || fileAt(t, f, tip, "CODEOWNERS") != "/internal/ @upstream @feature\n" {
		t.Fatalf("the feature branch is at %s, not the approved candidate %s", tip, candidate)
	}
}
