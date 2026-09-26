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
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/busybees/core/agent"
	"github.com/modelcontextprotocol/go-sdk/mcp"

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

// trackedFile is a file of the clone the landing of newJujutsuLandedFixture
// changes and audit deletes.
const trackedFile = "internal/trace/git.go"

// jujutsuConflicts are the unit files newJujutsuLandedFixture writes, an
// empty one deleted: resume and audit each add masonWrote as the landing
// does, differently, audit also deletes trackedFile, which the landing
// changes, and upload adds a file of its own.
var jujutsuConflicts = map[string]map[string]string{
	"resume": {masonWrote: "package trace\n\n// resume\n"},
	"audit":  {masonWrote: "package trace\n\n// audit\n", trackedFile: ""},
	"upload": {"internal/upload/upload.go": "package upload\n"},
}

// jujutsuConflicted are the paths each unit's rebase onto the landing of
// newJujutsuLandedFixture leaves conflicted.
var jujutsuConflicted = map[string][]string{"resume": {masonWrote}, "audit": {masonWrote, trackedFile}, "upload": nil}

// newJujutsuLandedFixture builds disjointPlan on Jujutsu workspaces with the
// service stopped once resume's mason reported done: resume is reviewing,
// and upload and audit are implementing with a done turn each. Every unit's
// workspace gets its jujutsuConflicts, a landing moves the feature branch to
// a masonWrote of its own and changes trackedFile, and one foreman pass asks
// to rebase all three. It returns the workstream, the open trace, the
// foreman, the landed tip and each unit's rebase operation.
func newJujutsuLandedFixture(t *testing.T, key string) (*shedFixture, config.WorkstreamID, *trace.Repository, *foreman, string, map[string]coreadapter.Operation) {
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
			if content == "" {
				must(t, os.Remove(filepath.Join(w.Path, name)))
				continue
			}
			must(t, os.MkdirAll(filepath.Dir(filepath.Join(w.Path, name)), 0700))
			must(t, os.WriteFile(filepath.Join(w.Path, name), []byte(content), 0600))
		}
	}
	landed := moveFeature(t, f, stream, map[string]string{masonWrote: "package trace\n\n// landed\n", trackedFile: "package trace\n\n// landed git\n"})
	lands := &foreman{masons: m}
	must(t, lands.Pass(ctx))
	ops := map[string]coreadapter.Operation{}
	for _, unit := range []string{"resume", "upload", "audit"} {
		asked := rebaseOperations(t, repository, stream, unit)
		if len(asked) != 1 {
			t.Fatalf("one pass asked to rebase unit %s %d times", unit, len(asked))
		}
		ops[unit] = asked[0].Operation
	}
	return f, stream, repository, lands, landed, ops
}

// newJujutsuRebaseFixture is newJujutsuLandedFixture with every unit's
// rebase applied.
func newJujutsuRebaseFixture(t *testing.T, key string) (*shedFixture, config.WorkstreamID, *trace.Repository, *foreman, string) {
	t.Helper()
	f, stream, repository, lands, landed, ops := newJujutsuLandedFixture(t, key)
	for _, unit := range []string{"resume", "upload", "audit"} {
		if result, err := (rebaser{lands}).Apply(context.Background(), ops[unit]); err != nil || result.Outcome != "succeeded" {
			t.Fatalf("rebase of unit %s: %+v %v", unit, result, err)
		}
	}
	return f, stream, repository, lands, landed
}

// conflictSides returns the lines of each side of the one conflict the file
// holds: the side rebased onto, the base and the side rebased.
func conflictSides(t *testing.T, data []byte) (onto, base, change []string) {
	t.Helper()
	var section *[]string
	markers := 0
	for _, line := range strings.Split(string(data), "\n") {
		switch {
		case strings.HasPrefix(line, "<<<<<<< "):
			section = &onto
		case strings.HasPrefix(line, "||||||| "):
			section = &base
		case line == "=======":
			section = &change
		case strings.HasPrefix(line, ">>>>>>> "):
			section = nil
		default:
			if section != nil {
				*section = append(*section, line)
			}
			continue
		}
		markers++
	}
	if markers != 4 {
		t.Fatalf("the file holds %d marker lines, not one conflict's four:\n%s", markers, data)
	}
	return onto, base, change
}

// On Jujutsu workspaces, one landing rebases every unit in flight: the two
// whose change conflicts with it hold the conflict stored in their rebased
// commit and materialized with markers in their workspace, a file one side
// deleted with no lines on that side, and the third is rebased cleanly. One pass then routes each conflicted unit, reviewing or
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
		if len(rebases) != 1 || !slices.Equal(rebases[0].Conflicts, jujutsuConflicted[unit]) || rebases[0].Onto != landed {
			t.Fatalf("unit %s's rebases %+v", unit, rebases)
		}
		if stored, err := g.StoredConflicts(ctx, rebases[0].Commit); err != nil || !slices.Equal(stored, jujutsuConflicted[unit]) {
			t.Fatalf("unit %s's rebased commit %s stores %v: %v", unit, rebases[0].Commit, stored, err)
		}
		if c, err := g.Commit(ctx, rebases[0].Commit); err != nil || !slices.Equal(c.Parents, []string{landed}) {
			t.Fatalf("unit %s's rebased commit %+v: %v", unit, c, err)
		}
		w, _, _, err := units.find(ctx, stream, unit)
		must(t, err)
		data, err := os.ReadFile(filepath.Join(w.Path, masonWrote))
		must(t, err)
		onto, base, change := conflictSides(t, data)
		if !slices.Contains(onto, "// landed") || len(base) != 0 || !slices.Contains(change, "// "+unit) {
			t.Fatalf("unit %s's conflicted %s reads %q | %q | %q", unit, masonWrote, onto, base, change)
		}
	}
	audit, _, _, err := units.find(ctx, stream, "audit")
	must(t, err)
	data, err := os.ReadFile(filepath.Join(audit.Path, trackedFile))
	must(t, err)
	if onto, base, change := conflictSides(t, data); !slices.Equal(onto, []string{"package trace", "", "// landed git"}) || !slices.Equal(base, []string{"package trace"}) || len(change) != 0 {
		t.Fatalf("audit's deleted %s reads %q | %q | %q", trackedFile, onto, base, change)
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
	for name, want := range map[string]string{masonWrote: "package trace\n\n// landed\n", trackedFile: "package trace\n\n// landed git\n", "internal/upload/upload.go": "package upload\n"} {
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
		for _, want := range []string{"- " + strings.Join(jujutsuConflicted[unit], "\n- ") + "\n", `the lines between "<<<<<<<" and "|||||||" are the feature branch's`, `those between "=======" and ">>>>>>>" are your unit's`, "A side that deleted the file holds no lines.", "## end of spec.md"} {
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
	if remind.Request.TurnID != fmt.Sprintf("%s-markers-%d", masonAgent("audit"), done.Sequence) || !strings.Contains(remind.Request.Prompt, "still carry conflict markers") || !strings.Contains(remind.Request.Prompt, masonWrote+", "+trackedFile) {
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

// A candidate that holds a stored conflict, of a file both sides changed and
// of one a side deleted, is refused at both gates even when the trace names
// it: the reviewer's bundle is not assembled, so no
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
	refused := fmt.Sprintf("candidate %s holds unresolved conflicts in %s, %s", candidate, masonWrote, trackedFile)

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

// crashedRebase applies the rebase operation with the service cut off once
// the unit's branch and files moved, and checks that the retry completes it
// from the commit the branch moved to: the operation reads as this
// operation's rebase, nothing is rebased again, and the rebase is recorded
// once, on onto, with conflicts, which that commit stores, conflicted or
// else rebased. It records the operation's result and returns the commit.
func crashedRebase(t *testing.T, f *shedFixture, repository *trace.Repository, r rebaser, stream config.WorkstreamID, unit string, op coreadapter.Operation, onto string, conflicts []string) string {
	t.Helper()
	ctx := context.Background()
	crashed := false
	f.s.boundary = func(name string) error {
		if name == "rebase-moved" && !crashed {
			crashed = true
			return errors.New("crash")
		}
		return nil
	}
	defer func() { f.s.boundary = nil }()
	if result, err := r.Apply(ctx, op); err == nil || !strings.Contains(err.Error(), "crash") {
		t.Fatalf("unit %s's rebase never moved the workspace: %+v %v", unit, result, err)
	}
	g := providerOf(t, newUnitWorkspaces(f.s.cfg, repository).streamWorkspaces, stream)
	moved, _, err := g.Branch(ctx, unitBranch(stream, unit))
	must(t, err)
	if observed, err := r.Inspect(ctx, op); err != nil || observed.State != coreadapter.EffectAbsent || !strings.Contains(observed.Evidence, "this operation's rebase of snapshot") {
		t.Fatalf("inspection of unit %s's interrupted rebase %+v %v", unit, observed, err)
	}
	if result, err := r.Apply(ctx, op); err != nil || result.Outcome != "succeeded" {
		t.Fatalf("unit %s's retried rebase %+v %v", unit, result, err)
	}
	if tip, _, err := g.Branch(ctx, unitBranch(stream, unit)); err != nil || tip != moved {
		t.Fatalf("the retry moved unit %s's branch from %s to %s: %v", unit, moved, tip, err)
	}
	if c, err := g.Commit(ctx, moved); err != nil || !slices.Equal(c.Parents, []string{onto}) {
		t.Fatalf("unit %s's rebased commit %+v: %v", unit, c, err)
	}
	if stored, err := g.StoredConflicts(ctx, moved); err != nil || !slices.Equal(stored, conflicts) {
		t.Fatalf("unit %s's rebased commit stores %v: %v", unit, stored, err)
	}
	rebases := unitRebases(t, repository, stream, unit)
	last := rebases[len(rebases)-1]
	if last.Commit != moved || last.Onto != onto || !slices.Equal(last.Conflicts, conflicts) {
		t.Fatalf("unit %s's rebase records %+v", unit, rebases)
	}
	transition, _ := rebaseIDs(unit, last.Rebase)
	var outcomes []string
	for _, tr := range allTransitions(t, f.trace, stream) {
		if tr.Subject == rebaseSubject(unit) && strings.HasPrefix(tr.ID, transition+"-") {
			outcomes = append(outcomes, tr.ID)
		}
	}
	outcome := transition + "-rebased"
	if len(conflicts) != 0 {
		outcome = transition + "-conflicted"
	}
	if !slices.Equal(outcomes, []string{outcome}) {
		t.Fatalf("unit %s's rebase outcomes %v", unit, outcomes)
	}
	settleOperation(t, f.s, repository, stream, op, r)
	return moved
}

// rebaseNumber returns the unit's rebase operation number k.
func rebaseNumber(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, unit string, k int) coreadapter.Operation {
	t.Helper()
	for _, o := range rebaseOperations(t, repository, stream, unit) {
		if in, err := decodeRebase(o.Operation); err == nil && in.Rebase == k {
			return o.Operation
		}
	}
	t.Fatalf("unit %s has no rebase %d", unit, k)
	return coreadapter.Operation{}
}

// A conflicted rebase on Jujutsu workspaces cut off after the unit's branch
// and files moved completes on retry from the commit the branch holds,
// though a conflicted rebase makes a new commit each time it runs. So does
// the next rebase of the unit while its conflict is unresolved, and the one
// after, onto a landing that makes the unit's change itself, which resolves
// the conflict.
func TestInterruptedJujutsuConflictedRebaseCompletesOnRetry(t *testing.T) {
	t.Parallel()
	f, stream, repository, lands, landed, ops := newJujutsuLandedFixture(t, "jj-interrupted")
	ctx := context.Background()
	r := rebaser{lands}
	for _, unit := range []string{"resume", "upload"} {
		if result := settleOperation(t, f.s, repository, stream, ops[unit], r); result.Outcome != "succeeded" {
			t.Fatalf("rebase of unit %s: %+v", unit, result)
		}
	}
	commits := []string{crashedRebase(t, f, repository, r, stream, "audit", ops["audit"], landed, jujutsuConflicted["audit"])}
	for k, landing := range []struct {
		files     map[string]string
		conflicts []string
	}{
		{map[string]string{"LANDED.md": "landed again\n"}, jujutsuConflicted["audit"]},
		{map[string]string{masonWrote: jujutsuConflicts["audit"][masonWrote], trackedFile: ""}, nil},
	} {
		onto := moveFeature(t, f, stream, landing.files)
		must(t, lands.Pass(ctx))
		for _, unit := range []string{"resume", "upload"} {
			if result := settleOperation(t, f.s, repository, stream, rebaseNumber(t, repository, stream, unit, k+2), r); result.Outcome != "succeeded" {
				t.Fatalf("rebase %d of unit %s: %+v", k+2, unit, result)
			}
		}
		commits = append(commits, crashedRebase(t, f, repository, r, stream, "audit", rebaseNumber(t, repository, stream, "audit", k+2), onto, landing.conflicts))
	}
	rebases := unitRebases(t, repository, stream, "audit")
	if len(rebases) != 3 || rebases[1].Snapshot != commits[0] || rebases[2].Snapshot != commits[1] {
		t.Fatalf("audit's rebase records %+v, rebased commits %v", rebases, commits)
	}
	w, _, _, err := newUnitWorkspaces(f.s.cfg, repository).find(ctx, stream, "audit")
	must(t, err)
	if data, err := os.ReadFile(filepath.Join(w.Path, masonWrote)); err != nil || string(data) != jujutsuConflicts["audit"][masonWrote] {
		t.Fatalf("audit's resolved %s holds %q: %v", masonWrote, data, err)
	}
	if _, err := os.Stat(filepath.Join(w.Path, trackedFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("audit's %s after both sides deleted it: %v", trackedFile, err)
	}
}

// Several conflicted units on Jujutsu workspaces are resolved at once within
// mason capacity: with two mason slots, the one conflict-free unit reviewing
// and the other two routed to their masons, the resolve turns of both
// conflicted units are in flight together, taking both slots with none
// waiting, each on a view of its own unit's workspace with its markers, and
// both units go to review once resolved.
func TestJujutsuConflictedUnitsAreResolvedInParallel(t *testing.T) {
	t.Parallel()
	f, stream, repository, lands, _ := newJujutsuRebaseFixture(t, "jj-parallel")
	must(t, lands.Pass(context.Background()))
	for _, unit := range []string{"resume", "audit"} {
		th, err := repository.Thread(stream, masonAgent(unit))
		must(t, err)
		if last := th.Turns[len(th.Turns)-1]; last.Request.TurnID != resolveTurnID(unit, 1) {
			t.Fatalf("unit %s's mason's last turn is %s", unit, last.Request.TurnID)
		}
	}
	must(t, repository.Close())
	path := filepath.Join(f.opts.Config.Root, "config.toml")
	data, err := os.ReadFile(path)
	must(t, err)
	// Two mason slots, and room in the workstream for both and the
	// reviewers' two, so only capacity.masons bounds the resolve turns.
	raised := strings.Replace(string(data), "\nmasons = 1\nper_workstream = 3\n", "\nmasons = 2\nper_workstream = 4\n", 1)
	if raised == string(data) {
		t.Fatalf("%s sets no capacity of one mason", path)
	}
	must(t, os.WriteFile(path, []byte(raised), 0600))

	resolutions := map[string]map[string]string{
		"resume": {masonWrote: "package trace\n\n// landed and resumed\n"},
		"audit":  {masonWrote: "package trace\n\n// landed and audited\n", trackedFile: "package trace\n\n// landed git\n"},
	}
	reports := map[string]CriterionReport{"resume": resumeReport, "audit": auditReport}
	var mu sync.Mutex
	var problems []string
	release := make(chan struct{})
	f.engine.mu.Lock()
	for unit, files := range resolutions {
		f.engine.turns[resolveTurnID(unit, 1)] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
			view := req.Workspace.Directory()
			for name := range files {
				if data, err := os.ReadFile(filepath.Join(view, name)); err != nil || !strings.Contains(string(data), "\n||||||| ") {
					mu.Lock()
					problems = append(problems, fmt.Sprintf("unit %s's view holds %s as %q: %v", unit, name, data, err))
					mu.Unlock()
				}
			}
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			for name, content := range files {
				if err := os.WriteFile(filepath.Join(view, name), []byte(content), 0600); err != nil {
					return nil, err
				}
			}
			if recorded, reason, err := done(ctx, tools, map[string]any{"outcome": "Resolved", "criteria": []any{criterionArgs(reports[unit])}}); err != nil || !recorded {
				mu.Lock()
				problems = append(problems, fmt.Sprintf("unit %s's done refused: %q %v", unit, reason, err))
				mu.Unlock()
			}
			return &agent.Result{ClaudeID: "session-" + req.Name, ResultText: "Resolved", SessionDir: req.SessionDir, NumTurns: 2}, nil
		}
	}
	f.engine.mu.Unlock()

	f.start(t)
	var masons RoleCapacity
	for deadline := time.Now().Add(demoTimeout); ; time.Sleep(10 * time.Millisecond) {
		capacity, diagnostic := f.s.capacityStatus(nil)
		if diagnostic != nil {
			t.Fatal(diagnostic.Message)
		}
		masons = capacity.Roles[slices.IndexFunc(capacity.Roles, func(r RoleCapacity) bool { return r.Role == masonRole })]
		if masons.Used == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the resolve turns were never in flight together: %+v", masons)
		}
	}
	if masons.Limit != 2 || len(masons.Waiting) != 0 {
		t.Fatalf("mason capacity with both resolve turns in flight %+v", masons)
	}
	for _, unit := range []string{"resume", "audit"} {
		th, err := f.repository().Thread(stream, masonAgent(unit))
		must(t, err)
		if last := th.Turns[len(th.Turns)-1]; last.Request.TurnID != resolveTurnID(unit, 1) || !last.CompletedAt.IsZero() {
			t.Fatalf("unit %s's mason's last turn is %s, completed at %s", unit, last.Request.TurnID, last.CompletedAt)
		}
	}
	close(release)
	f.awaitUnit(t, stream, "resume", UnitReviewing)
	f.awaitUnit(t, stream, "audit", UnitReviewing)
	f.stop(t)
	mu.Lock()
	defer mu.Unlock()
	if len(problems) != 0 {
		t.Fatal(strings.Join(problems, "\n"))
	}
}
