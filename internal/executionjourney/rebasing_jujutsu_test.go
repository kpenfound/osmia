package executionjourney

import (
	context "context"
	json "encoding/json"
	errors "errors"
	fmt "fmt"
	os "os"
	filepath "path/filepath"
	slices "slices"
	strings "strings"
	sync "sync"
	testing "testing"
	time "time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	agent "github.com/kpenfound/agentcore/agent"
	config "github.com/kpenfound/osmia/internal/config"
	coreadapter "github.com/kpenfound/osmia/internal/coreadapter"
	seal "github.com/kpenfound/osmia/internal/seal"
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	trace "github.com/kpenfound/osmia/internal/trace"
	workspace "github.com/kpenfound/osmia/internal/workspace"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

var (
	uploadReport = "send chunks"
	auditReport  = "record acknowledged chunks"
)

var jujutsuConflicts = map[string]map[string]string{
	"resume": {fixturekit.MasonWrote: "package trace\n\n// resume\n"},
	"audit":  {fixturekit.MasonWrote: "package trace\n\n// audit\n", intakefixture.TrackedFile: ""},
	"upload": {"internal/upload/upload.go": "package upload\n"},
}

var jujutsuConflicted = map[string][]string{"resume": {fixturekit.MasonWrote}, "audit": {fixturekit.MasonWrote, intakefixture.TrackedFile}, "upload": nil}

func newJujutsuLandedFixture(t *testing.T, key string) (*journey.ShedFixture, config.WorkstreamID, *trace.Repository, *journey.Foreman, string, map[string]coreadapter.Operation) {
	t.Helper()
	f, _ := journey.NewParallelMasonFixtureOn(t, config.WorkspacesJujutsu, 1, 3, intakefixture.DisjointPlan)
	base := strings.TrimSpace(fixturekit.DemoGit(t, f.Clone, "-C", f.Clone, "rev-parse", "HEAD"))
	stream, repository := journey.SeedBuild(t, f, key, intakefixture.DisjointPlan, config.WorkspacesJujutsu, base)
	journey.SeedReview(t, f, repository, stream, "resume", journey.ResumeReport)
	lands := &journey.Foreman{Masons: journey.NewMasonController(f.S, repository)}
	landed, ops := intakefixture.PrepareJujutsuLanding(t, repository, stream, f.UnitSeedDriver(repository), journey.NewUnitWorkspaces(f.S.Cfg, repository).Find, jujutsuConflicts, uploadReport, auditReport, func(files map[string]string) string { return journey.MoveFeature(t, f, stream, files) }, lands.Pass, func(unit string) []trace.OperationRecord {
		return journey.RebaseOperations(t, repository, stream, unit)
	})

	return f, stream, repository, lands, landed, ops
}

func newJujutsuRebaseFixture(t *testing.T, key string) (*journey.ShedFixture, config.WorkstreamID, *trace.Repository, *journey.Foreman, string) {
	t.Helper()
	f, stream, repository, lands, landed, ops := newJujutsuLandedFixture(t, key)
	for _, unit := range []string{"resume", "upload", "audit"} {
		if result := journey.SettleOperation(t, f.S, repository, stream, ops[unit], journey.Rebaser{Foreman: lands}); result.Outcome != "succeeded" {
			t.Fatalf("rebase of unit %s: %+v", unit, result)
		}
	}
	return f, stream, repository, lands, landed
}

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

func TestJujutsuRebaseStoresEachUnitsConflictAndRoutesItToItsMason(t *testing.T) {
	t.Parallel()
	f, stream, repository, lands, landed := newJujutsuRebaseFixture(t, "jj-conflicts")
	ctx := context.Background()
	m := lands.Masons
	units := journey.NewUnitWorkspaces(f.S.Cfg, repository)
	g := intakefixture.ProviderOf(t, units.StreamWorkspaces, stream)
	if _, ok := g.(*workspace.Jujutsu); !ok {
		t.Fatalf("the unit workspaces are %T", g)
	}

	for _, unit := range []string{"resume", "audit"} {
		rebases := journey.UnitRebases(t, repository, stream, unit)
		if len(rebases) != 1 || !slices.Equal(rebases[0].Conflicts, jujutsuConflicted[unit]) || rebases[0].Onto != landed {
			t.Fatalf("unit %s's rebases %+v", unit, rebases)
		}
		if stored, err := g.StoredConflicts(ctx, rebases[0].Commit); err != nil || !slices.Equal(stored, jujutsuConflicted[unit]) {
			t.Fatalf("unit %s's rebased commit %s stores %v: %v", unit, rebases[0].Commit, stored, err)
		}
		if c, err := g.Commit(ctx, rebases[0].Commit); err != nil || !slices.Equal(c.Parents, []string{landed}) {
			t.Fatalf("unit %s's rebased commit %+v: %v", unit, c, err)
		}
		w, _, _, err := units.Find(ctx, stream, unit)
		fixturekit.Must(t, err)
		data, err := os.ReadFile(filepath.Join(w.Path, fixturekit.MasonWrote))
		fixturekit.Must(t, err)
		onto, base, change := conflictSides(t, data)
		if !slices.Contains(onto, "// landed") || len(base) != 0 || !slices.Contains(change, "// "+unit) {
			t.Fatalf("unit %s's conflicted %s reads %q | %q | %q", unit, fixturekit.MasonWrote, onto, base, change)
		}
	}
	audit, _, _, err := units.Find(ctx, stream, "audit")
	fixturekit.Must(t, err)
	data, err := os.ReadFile(filepath.Join(audit.Path, intakefixture.TrackedFile))
	fixturekit.Must(t, err)
	if onto, base, change := conflictSides(t, data); !slices.Equal(onto, []string{"package trace", "", "// landed git"}) || !slices.Equal(base, []string{"package trace"}) || len(change) != 0 {
		t.Fatalf("audit's deleted %s reads %q | %q | %q", intakefixture.TrackedFile, onto, base, change)
	}
	rebases := journey.UnitRebases(t, repository, stream, "upload")
	if len(rebases) != 1 || len(rebases[0].Conflicts) != 0 || rebases[0].Onto != landed {
		t.Fatalf("upload's rebases %+v", rebases)
	}
	if stored, err := g.StoredConflicts(ctx, rebases[0].Commit); err != nil || len(stored) != 0 {
		t.Fatalf("upload's rebased commit stores %v: %v", stored, err)
	}
	w, _, _, err := units.Find(ctx, stream, "upload")
	fixturekit.Must(t, err)
	for name, want := range map[string]string{fixturekit.MasonWrote: "package trace\n\n// landed\n", intakefixture.TrackedFile: "package trace\n\n// landed git\n", "internal/upload/upload.go": "package upload\n"} {
		if data, err := os.ReadFile(filepath.Join(w.Path, name)); err != nil || string(data) != want {
			t.Fatalf("upload's %s after its rebase: %q %v", name, data, err)
		}
	}

	fixturekit.Must(t, lands.Pass(ctx))
	if ops := journey.LandOperations(t, repository, stream); len(ops) != 0 {
		t.Fatalf("a landing was asked for with conflicted units: %+v", ops)
	}
	for _, unit := range []string{"resume", "audit"} {
		if state, err := repository.Workflow(stream, trace.UnitSubject(unit)); err != nil || state.Value != intake.UnitImplementing {
			t.Fatalf("the conflicted unit %s is %+v: %v", unit, state, err)
		}
		th, err := repository.Thread(stream, intake.MasonAgent(unit))
		fixturekit.Must(t, err)
		resolve := th.Turns[len(th.Turns)-1]
		if resolve.Request.TurnID != execution.ResolveTurnID(unit, 1) || !resolve.CompletedAt.IsZero() {
			t.Fatalf("unit %s's mason's last turn is %s", unit, resolve.Request.TurnID)
		}
		for _, want := range []string{"- " + strings.Join(jujutsuConflicted[unit], "\n- ") + "\n", `the lines between "<<<<<<<" and "|||||||" are the feature branch's`, `those between "=======" and ">>>>>>>" are your unit's`, "A side that deleted the file holds no lines.", "## end of spec.md"} {
			if !strings.Contains(resolve.Request.Prompt, want) {
				t.Fatalf("unit %s's resolve turn lacks %q:\n%s", unit, want, resolve.Request.Prompt)
			}
		}
	}
	if th, err := repository.Thread(stream, intake.MasonAgent("upload")); err != nil || th.Turns[len(th.Turns)-1].Request.TurnID == execution.ResolveTurnID("upload", 1) {
		t.Fatalf("the cleanly rebased unit's mason got a resolve turn: %v", err)
	}

	resume, _, _, err := units.Find(ctx, stream, "resume")
	fixturekit.Must(t, err)
	fixturekit.Must(t, os.WriteFile(filepath.Join(resume.Path, fixturekit.MasonWrote), []byte("package trace\n\n// landed and resumed\n"), 0600))
	journey.CompleteMasonTurn(t, f, repository, stream, "resume", journey.ResumeReport)
	done := journey.CompleteMasonTurn(t, f, repository, stream, "audit", auditReport)
	fixturekit.Must(t, m.Pass(ctx))

	for unit, want := range map[string]string{"resume": "+// landed and resumed", "upload": "+package upload"} {
		if state, err := repository.Workflow(stream, trace.UnitSubject(unit)); err != nil || state.Value != execution.UnitChecking {
			t.Fatalf("unit %s is %+v: %v", unit, state, err)
		}
		_, report := latestReport(t, repository, stream, unit)
		if report.Base != landed {
			t.Fatalf("unit %s's report %+v, want a candidate from %s", unit, report, landed)
		}
		if stored, err := g.StoredConflicts(ctx, report.Candidate); err != nil || len(stored) != 0 {
			t.Fatalf("unit %s's candidate stores %v: %v", unit, stored, err)
		}
		req, _, err := m.CandidateEvidence(ctx, stream, unit)
		fixturekit.Must(t, err)
		if strings.Contains(req.Diff, "<<<<<<<") || strings.Contains(req.Diff, ">>>>>>>") || strings.Contains(req.Diff, "jjconflict") || !strings.Contains(req.Diff, want) {
			t.Fatalf("unit %s's reviewer diff:\n%s", unit, req.Diff)
		}
	}
	if state, err := repository.Workflow(stream, trace.UnitSubject("audit")); err != nil || state.Value != intake.UnitImplementing {
		t.Fatalf("a candidate holding its conflict left implementing: %+v %v", state, err)
	}
	if doc, _ := latestReport(t, repository, stream, "audit"); doc.Revision != 0 {
		t.Fatalf("a candidate holding its conflict was reported as revision %d", doc.Revision)
	}
	th, err := repository.Thread(stream, intake.MasonAgent("audit"))
	fixturekit.Must(t, err)
	remind := th.Turns[len(th.Turns)-1]
	if remind.Request.TurnID != fmt.Sprintf("%s-markers-%d", intake.MasonAgent("audit"), done.Sequence) || !strings.Contains(remind.Request.Prompt, "still carry conflict markers") || !strings.Contains(remind.Request.Prompt, fixturekit.MasonWrote+", "+intakefixture.TrackedFile) {
		t.Fatalf("the reminder %s:\n%s", remind.Request.TurnID, remind.Request.Prompt)
	}
}

func plantUnit(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, unit, to string) {
	t.Helper()
	subject := trace.UnitSubject(unit)
	state, err := repository.Workflow(stream, subject)
	fixturekit.Must(t, err)
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: subject + "-planted-" + to, Revision: 1, Project: repository.Project(), Workstream: stream, Unit: unit, At: time.Now().UTC(), Actor: execution.ForemanActor, Cause: "test"}
	_, err = repository.Transact(context.Background(), trace.Transaction{ExpectedVersion: state.Version,
		Transition: trace.Transition{Header: h, Subject: subject, From: state.Value, To: to, Reason: "the test moves it"}})
	fixturekit.Must(t, err)
}

func plantDocument(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, unit, id, path string, value any) trace.Document {
	t.Helper()
	docs, err := trace.Read[trace.Document](repository, stream)
	fixturekit.Must(t, err)
	revision := 1
	for _, d := range docs {
		if d.ID == id {
			revision = d.Revision + 1
		}
	}
	content, err := json.MarshalIndent(value, "", "  ")
	fixturekit.Must(t, err)
	doc := trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: id, Revision: revision, Project: repository.Project(), Workstream: stream, Unit: unit, At: time.Now().UTC(), Actor: execution.ForemanActor, Cause: "test"},
		Path: path, Content: string(content) + "\n"}
	fixturekit.Must(t, repository.RecordDocuments(context.Background(), []trace.Document{doc}))
	return doc
}

func TestJujutsuCandidateHoldingAConflictIsNeitherReviewedNorLanded(t *testing.T) {
	t.Parallel()
	f, stream, repository, lands, landed := newJujutsuRebaseFixture(t, "jj-gates")
	ctx := context.Background()
	rebases := journey.UnitRebases(t, repository, stream, "audit")
	if len(rebases) != 1 || len(rebases[0].Conflicts) == 0 {
		t.Fatalf("audit's rebases %+v", rebases)
	}
	candidate := rebases[0].Commit
	if tip, _, err := intakefixture.ProviderOf(t, journey.NewUnitWorkspaces(f.S.Cfg, repository).StreamWorkspaces, stream).Branch(ctx, execution.UnitBranch(stream, "audit")); err != nil || tip != candidate {
		t.Fatalf("audit's branch is at %s, %v; want %s", tip, err, candidate)
	}
	latest, _, _, err := seal.Latest(repository, stream)
	fixturekit.Must(t, err)
	report := plantDocument(t, repository, stream, "audit", execution.ReportDocument("audit"), fmt.Sprintf(execution.ReportPath, "audit"),
		execution.UnitReport{Unit: "audit", Turn: "test", Seal: latest.Seal, Outcome: "Built", Branch: execution.UnitBranch(stream, "audit"), Base: landed, Candidate: candidate})
	plantUnit(t, repository, stream, "audit", intake.UnitReviewing)
	refused := fmt.Sprintf("candidate %s holds unresolved conflicts in %s, %s", candidate, fixturekit.MasonWrote, intakefixture.TrackedFile)

	r := &journey.Reviewers{Masons: lands.Masons}
	state, err := repository.Workflow(stream, trace.UnitSubject("audit"))
	fixturekit.Must(t, err)
	fixturekit.Must(t, r.One(ctx, stream, "audit", state, false))
	if th, err := repository.Thread(stream, intake.ReviewerAgent("audit")); err == nil && len(th.Turns) != 0 || err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the candidate holding a conflict was offered for review: %+v %v", th.Turns, err)
	}
	blocked := slices.IndexFunc(intakefixture.AllTransitions(t, f.S.Cfg.Root, f.Project, stream), func(tr trace.Transition) bool {
		return tr.Subject == execution.ReviewBlockedSubject("audit") && tr.Reason == "unit audit stays reviewing: its reviewer bundle cannot be assembled: "+refused
	})
	if blocked < 0 {
		t.Fatal("the refused review is not recorded")
	}

	review := plantDocument(t, repository, stream, "audit", execution.ReviewDocument("audit"), "units/audit/review.json", service.UnitReviewResult{
		Identity: service.UnitReviewIdentity{Subject: string(stream) + "/audit", Candidate: coreadapter.Candidate{Revision: candidate, BaseRevision: landed, SpecRevision: fmt.Sprint(latest.Revision.Spec), PlanRevision: fmt.Sprint(latest.Revision.Plan)}, Report: fmt.Sprintf("%s revision %d", report.Path, report.Revision), Seal: latest.Seal},
		Turn:     "test", Verdict: service.UnitVerdict{Decision: "satisfactory", Summary: fixturekit.ReviewSummary}})
	plantUnit(t, repository, stream, "audit", intake.UnitApproved)
	_, result, satisfactory, err := lands.Approval(stream, "audit", review.Revision)
	if err != nil || !satisfactory {
		t.Fatalf("the planted approval does not hold: %v", err)
	}
	fixturekit.Must(t, lands.Driver().Request(ctx, stream, "audit", review, result))
	ops := journey.LandOperations(t, repository, stream)
	if len(ops) != 1 {
		t.Fatalf("landings %+v", ops)
	}
	landing, err := lands.Apply(ctx, ops[0].Operation)
	if err != nil || landing.Outcome != "failed" || landing.Evidence != "unit audit did not land: "+refused {
		t.Fatalf("the landing of a candidate holding a conflict: %+v %v", landing, err)
	}
	if tip := journey.FeatureTip(t, f, stream); tip != landed {
		t.Fatalf("the feature branch moved to %s", tip)
	}
	if state, err := repository.Workflow(stream, trace.UnitSubject("audit")); err != nil || state.Value != intake.UnitApproved {
		t.Fatalf("the refused unit is %+v: %v", state, err)
	}
}

func TestJujutsuDriftConflictIsResolvedInItsWorkspaceBeforeReview(t *testing.T) {
	t.Parallel()
	f := journey.NewDebateFixtureWith(t, 1, 1, "", func(opts *journey.Options) { intakefixture.OnWorkspaces(t, opts.Config.Root, config.WorkspacesJujutsu) })
	base := f.Upstream(t)
	stream, repository := journey.SeedBuild(t, f, "jj-drift", fixturekit.ValidPlan, config.WorkspacesJujutsu, base)
	d := journey.Drifter{Foreman: &journey.Foreman{Masons: journey.NewMasonController(f.S, repository)}}
	ctx := context.Background()
	before, upstream, op := journey.ConflictedDrift(t, f, d, stream)

	journey.AwaitResolution(t, f.S, d, stream, op, "its mason's resolution of CODEOWNERS")
	w := journey.ResolutionWorkspace(t, f, stream)
	g := intakefixture.ProviderOf(t, lifecycle.DriftWorkspaces(f.S.Cfg, repository), stream)
	if _, ok := g.(*workspace.Jujutsu); !ok {
		t.Fatalf("the drift workspaces are %T", g)
	}
	if stop, conflicts, replaying, err := g.Replaying(ctx, w); err != nil || !replaying || stop != before || !slices.Equal(conflicts, []string{"CODEOWNERS"}) {
		t.Fatalf("the replay stopped at %s with %v (%t): %v", stop, conflicts, replaying, err)
	}
	data, err := os.ReadFile(filepath.Join(w.Path, "CODEOWNERS"))
	fixturekit.Must(t, err)
	for _, want := range []string{"<<<<<<< ", "@upstream", "||||||| ", "=======", "@feature", ">>>>>>> "} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("the resolution workspace's CODEOWNERS lacks %q:\n%s", want, data)
		}
	}
	th, err := repository.Thread(stream, intake.DriftMasonAgent)
	fixturekit.Must(t, err)
	for _, want := range []string{"- CODEOWNERS\n", `the lines between "<<<<<<<" and "|||||||" are the branch as rebased onto upstream so far`, `those between "=======" and ">>>>>>>" are the commit being replayed`} {
		if !strings.Contains(th.Turns[0].Request.Prompt, want) {
			t.Fatalf("the resolve turn lacks %q:\n%s", want, th.Turns[0].Request.Prompt)
		}
	}

	left := journey.CompleteDriftTurn(t, f, repository, stream, intake.DriftMasonAgent, journey.ResolvedDone("Left it"))
	journey.AwaitResolution(t, f.S, d, stream, op, "its mason's resolution of CODEOWNERS")
	if ids := intakefixture.TurnIDs(t, repository, stream, intake.DriftMasonAgent); !slices.Equal(ids, []string{lifecycle.DriftResolveTurnID(1, 1), fmt.Sprintf("%s-markers-%d", intake.DriftMasonAgent, left.Sequence)}) {
		t.Fatalf("drift mason turns %v", ids)
	}
	if _, err := repository.Thread(stream, intake.DriftReviewerAgent); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a resolution holding its conflict went to review: %v", err)
	}

	fixturekit.Must(t, os.WriteFile(filepath.Join(w.Path, "CODEOWNERS"), []byte("/internal/ @upstream @feature\n"), 0600))
	journey.CompleteDriftTurn(t, f, repository, stream, intake.DriftMasonAgent, journey.ResolvedDone("Kept both owners"))
	journey.AwaitResolution(t, f.S, d, stream, op, "review 1 of candidate")
	records := journey.DriftRecords(t, repository, stream)
	candidate := records[len(records)-1].Candidate
	if stored, err := g.StoredConflicts(ctx, candidate); err != nil || len(stored) != 0 || journey.ParentOf(t, f, candidate) != upstream {
		t.Fatalf("the resolved candidate %s stores %v: %v", candidate, stored, err)
	}
	review, err := repository.Thread(stream, intake.DriftReviewerAgent)
	fixturekit.Must(t, err)
	prompt := review.Turns[0].Request.Prompt + journey.DriftResolutionDiff(t, f, repository, stream)
	if strings.Contains(prompt, "<<<<<<<") || strings.Contains(prompt, ">>>>>>>") || strings.Contains(prompt, "jjconflict") || !strings.Contains(prompt, "+/internal/ @upstream @feature") {
		t.Fatalf("the drift reviewer reads:\n%s", prompt)
	}
	if tip := journey.FeatureTip(t, f, stream); tip != before {
		t.Fatalf("the feature branch moved to %s before review", tip)
	}

	journey.CompleteDriftTurn(t, f, repository, stream, intake.DriftReviewerAgent, journey.DriftVerdict(t, journey.ApprovedResolution))
	if result, err := journey.AttemptOperation(t, f.S, repository, stream, op, d); err != nil || result.Outcome != "succeeded" {
		t.Fatalf("the approved drift rebase %+v %v", result, err)
	}
	if tip := journey.FeatureTip(t, f, stream); tip != candidate || journey.FileAt(t, f, tip, "CODEOWNERS") != "/internal/ @upstream @feature\n" {
		t.Fatalf("the feature branch is at %s, not the approved candidate %s", tip, candidate)
	}
}

func crashedRebase(t *testing.T, f *journey.ShedFixture, repository *trace.Repository, r journey.Rebaser, stream config.WorkstreamID, unit string, op coreadapter.Operation, onto string, conflicts []string) string {
	t.Helper()
	ctx := context.Background()
	crashed := false
	f.S.Boundary = func(name string) error {
		if name == "rebase-moved" && !crashed {
			crashed = true
			return errors.New("crash")
		}
		return nil
	}
	defer func() { f.S.Boundary = nil }()
	if result, err := r.Apply(ctx, op); err == nil || !strings.Contains(err.Error(), "crash") {
		t.Fatalf("unit %s's rebase never moved the workspace: %+v %v", unit, result, err)
	}
	g := intakefixture.ProviderOf(t, journey.NewUnitWorkspaces(f.S.Cfg, repository).StreamWorkspaces, stream)
	moved, _, err := g.Branch(ctx, execution.UnitBranch(stream, unit))
	fixturekit.Must(t, err)
	if observed, err := r.Inspect(ctx, op); err != nil || observed.State != coreadapter.EffectAbsent || !strings.Contains(observed.Evidence, "this operation's rebase of snapshot") {
		t.Fatalf("inspection of unit %s's interrupted rebase %+v %v", unit, observed, err)
	}
	if result, err := r.Apply(ctx, op); err != nil || result.Outcome != "succeeded" {
		t.Fatalf("unit %s's retried rebase %+v %v", unit, result, err)
	}
	if tip, _, err := g.Branch(ctx, execution.UnitBranch(stream, unit)); err != nil || tip != moved {
		t.Fatalf("the retry moved unit %s's branch from %s to %s: %v", unit, moved, tip, err)
	}
	if c, err := g.Commit(ctx, moved); err != nil || !slices.Equal(c.Parents, []string{onto}) {
		t.Fatalf("unit %s's rebased commit %+v: %v", unit, c, err)
	}
	if stored, err := g.StoredConflicts(ctx, moved); err != nil || !slices.Equal(stored, conflicts) {
		t.Fatalf("unit %s's rebased commit stores %v: %v", unit, stored, err)
	}
	change, err := execution.ReadUnitChange(repository, stream, unit)
	fixturekit.Must(t, err)
	if got, err := g.ChangeOf(ctx, moved); err != nil || change == "" || got != change {
		t.Fatalf("unit %s's rebased commit is change %q, %v; want %q", unit, got, err, change)
	}
	rebases := journey.UnitRebases(t, repository, stream, unit)
	last := rebases[len(rebases)-1]
	if last.Commit != moved || last.Onto != onto || !slices.Equal(last.Conflicts, conflicts) {
		t.Fatalf("unit %s's rebase records %+v", unit, rebases)
	}
	transition, _ := execution.RebaseIDs(unit, last.Rebase)
	var outcomes []string
	for _, tr := range intakefixture.AllTransitions(t, f.S.Cfg.Root, f.Project, stream) {
		if tr.Subject == execution.RebaseSubject(unit) && strings.HasPrefix(tr.ID, transition+"-") {
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
	journey.SettleOperation(t, f.S, repository, stream, op, r)
	return moved
}

func rebaseNumber(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, unit string, k int) coreadapter.Operation {
	t.Helper()
	for _, o := range journey.RebaseOperations(t, repository, stream, unit) {
		if in, err := execution.DecodeRebase(o.Operation); err == nil && in.Rebase == k {
			return o.Operation
		}
	}
	t.Fatalf("unit %s has no rebase %d", unit, k)
	return coreadapter.Operation{}
}

func TestInterruptedJujutsuConflictedRebaseCompletesOnRetry(t *testing.T) {
	t.Parallel()
	f, stream, repository, lands, landed, ops := newJujutsuLandedFixture(t, "jj-interrupted")
	ctx := context.Background()
	r := journey.Rebaser{Foreman: lands}
	if result := journey.SettleOperation(t, f.S, repository, stream, ops["resume"], r); result.Outcome != "succeeded" {
		t.Fatalf("rebase of unit resume: %+v", result)
	}
	crashedRebase(t, f, repository, r, stream, "upload", ops["upload"], landed, nil)
	commits := []string{crashedRebase(t, f, repository, r, stream, "audit", ops["audit"], landed, jujutsuConflicted["audit"])}
	for k, landing := range []struct {
		files     map[string]string
		conflicts []string
	}{
		{map[string]string{"LANDED.md": "landed again\n"}, jujutsuConflicted["audit"]},
		{map[string]string{fixturekit.MasonWrote: jujutsuConflicts["audit"][fixturekit.MasonWrote], intakefixture.TrackedFile: ""}, nil},
	} {
		onto := journey.MoveFeature(t, f, stream, landing.files)
		fixturekit.Must(t, lands.Pass(ctx))
		for _, unit := range []string{"resume", "upload"} {
			if result := journey.SettleOperation(t, f.S, repository, stream, rebaseNumber(t, repository, stream, unit, k+2), r); result.Outcome != "succeeded" {
				t.Fatalf("rebase %d of unit %s: %+v", k+2, unit, result)
			}
		}
		commits = append(commits, crashedRebase(t, f, repository, r, stream, "audit", rebaseNumber(t, repository, stream, "audit", k+2), onto, landing.conflicts))
	}
	rebases := journey.UnitRebases(t, repository, stream, "audit")
	if len(rebases) != 3 || rebases[1].Snapshot != commits[0] || rebases[2].Snapshot != commits[1] {
		t.Fatalf("audit's rebase records %+v, rebased commits %v", rebases, commits)
	}
	w, _, _, err := journey.NewUnitWorkspaces(f.S.Cfg, repository).Find(ctx, stream, "audit")
	fixturekit.Must(t, err)
	if data, err := os.ReadFile(filepath.Join(w.Path, fixturekit.MasonWrote)); err != nil || string(data) != jujutsuConflicts["audit"][fixturekit.MasonWrote] {
		t.Fatalf("audit's resolved %s holds %q: %v", fixturekit.MasonWrote, data, err)
	}
	if _, err := os.Stat(filepath.Join(w.Path, intakefixture.TrackedFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("audit's %s after both sides deleted it: %v", intakefixture.TrackedFile, err)
	}
}

func TestJujutsuConflictedUnitsAreResolvedInParallel(t *testing.T) {
	t.Parallel()
	f, stream, repository, lands, _ := newJujutsuRebaseFixture(t, "jj-parallel")
	fixturekit.Must(t, lands.Pass(context.Background()))
	for _, unit := range []string{"resume", "audit"} {
		th, err := repository.Thread(stream, intake.MasonAgent(unit))
		fixturekit.Must(t, err)
		if last := th.Turns[len(th.Turns)-1]; last.Request.TurnID != execution.ResolveTurnID(unit, 1) {
			t.Fatalf("unit %s's mason's last turn is %s", unit, last.Request.TurnID)
		}
	}
	fixturekit.Must(t, repository.Close())
	path := filepath.Join(f.Opts.Config.Root, "config.toml")
	data, err := os.ReadFile(path)
	fixturekit.Must(t, err)

	raised := strings.Replace(string(data), "\nmasons = 1\nper_workstream = 3\n", "\nmasons = 2\nper_workstream = 4\n", 1)
	if raised == string(data) {
		t.Fatalf("%s sets no capacity of one mason", path)
	}
	fixturekit.Must(t, os.WriteFile(path, []byte(raised), 0600))

	resolutions := map[string]map[string]string{
		"resume": {fixturekit.MasonWrote: "package trace\n\n// landed and resumed\n"},
		"audit":  {fixturekit.MasonWrote: "package trace\n\n// landed and audited\n", intakefixture.TrackedFile: "package trace\n\n// landed git\n"},
	}
	var mu sync.Mutex
	var problems []string
	release := make(chan struct{})
	f.Engine.Mu.Lock()
	for unit, files := range resolutions {
		f.Engine.Turns[execution.ResolveTurnID(unit, 1)] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
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
			if recorded, reason, err := intakefixture.Done(ctx, tools, map[string]any{"outcome": "Resolved"}); err != nil || !recorded {
				mu.Lock()
				problems = append(problems, fmt.Sprintf("unit %s's done refused: %q %v", unit, reason, err))
				mu.Unlock()
			}
			return &agent.Result{ClaudeID: "session-" + req.Name, ResultText: "Resolved", SessionDir: req.SessionDir, NumTurns: 2}, nil
		}
	}
	f.Engine.Mu.Unlock()

	f.Start(t)
	var masons api.RoleCapacity
	intakefixture.Soon(t, "both resolve turns to be in flight together", func() bool {
		capacity := f.S.CapacityStatus(nil)
		masons = capacity.Roles[slices.IndexFunc(capacity.Roles, func(r api.RoleCapacity) bool { return r.Role == intake.MasonRole })]
		return masons.Used == 2
	})
	if fixturekit.LimitOf(t, masons) != 2 || len(masons.Waiting) != 0 {
		t.Fatalf("mason capacity with both resolve turns in flight %+v", masons)
	}
	for _, unit := range []string{"resume", "audit"} {
		th, err := f.Repository().Thread(stream, intake.MasonAgent(unit))
		fixturekit.Must(t, err)
		if last := th.Turns[len(th.Turns)-1]; last.Request.TurnID != execution.ResolveTurnID(unit, 1) || !last.CompletedAt.IsZero() {
			t.Fatalf("unit %s's mason's last turn is %s, completed at %s", unit, last.Request.TurnID, last.CompletedAt)
		}
	}
	close(release)
	f.AwaitUnit(t, stream, "resume", intake.UnitReviewing)
	f.AwaitUnit(t, stream, "audit", intake.UnitReviewing)
	f.Stop(t)
	mu.Lock()
	defer mu.Unlock()
	if len(problems) != 0 {
		t.Fatal(strings.Join(problems, "\n"))
	}
}

func TestJujutsuRebasedUnitKeepsItsChangeAndReturnsToReview(t *testing.T) {
	t.Parallel()
	f, stream, repository := journey.NewApprovedFixtureOn(t, "jj-change", config.WorkspacesJujutsu)
	ctx := context.Background()
	if backend, err := repository.Workspaces(stream); err != nil || backend != config.WorkspacesJujutsu {
		t.Fatalf("workstream %s is on %q: %v", stream, backend, err)
	}
	change, err := execution.ReadUnitChange(repository, stream, "dedupe")
	fixturekit.Must(t, err)
	other, err := execution.ReadUnitChange(repository, stream, "resume")
	fixturekit.Must(t, err)
	if change == "" || other == "" || other == change {
		t.Fatalf("the units' recorded changes are %q and %q", change, other)
	}
	docs, err := trace.Read[trace.Document](repository, stream)
	fixturekit.Must(t, err)
	i := slices.IndexFunc(docs, func(d trace.Document) bool { return d.ID == execution.ChangeDocument("dedupe") })
	if i < 0 || docs[i].Path != "units/dedupe/change.json" || docs[i].Revision != 1 || docs[i].Actor != intake.MasonActor || docs[i].Cause != trace.UnitSubject("dedupe")+"-"+intake.UnitReady {
		t.Fatalf("dedupe's change record %+v", docs)
	}
	var recorded execution.UnitChange
	fixturekit.Must(t, json.Unmarshal([]byte(docs[i].Content), &recorded))
	if recorded != (execution.UnitChange{Unit: "dedupe", Branch: execution.UnitBranch(stream, "dedupe"), Change: change}) {
		t.Fatalf("dedupe's change record %+v", recorded)
	}
	g := intakefixture.ProviderOf(t, journey.NewUnitWorkspaces(f.S.Cfg, repository).StreamWorkspaces, stream)
	_, approved := journey.ApprovedReview(t, repository, stream, "dedupe")
	candidate := approved.Identity.Candidate.Revision
	if got, err := g.ChangeOf(ctx, candidate); err != nil || got != change {
		t.Fatalf("dedupe's approved candidate %s is change %q, %v; want %s", candidate, got, err, change)
	}
	fixturekit.Must(t, repository.Close())
	f.Start(t)
	defer f.Stop(t)

	f.AwaitMerged(t, stream, "resume")
	f.AwaitUnit(t, stream, "dedupe", intake.UnitReviewing)
	var rebases []trace.OperationRecord
	intakefixture.Soon(t, "dedupe's rebase to finish", func() bool {
		rebases = journey.RebaseOperations(t, f.Repository(), stream, "dedupe")
		return len(rebases) == 1 && rebases[0].Result != nil
	})
	_, landing := journey.ApprovedReview(t, f.Repository(), stream, "resume")
	landed := landedCommits(f, t, stream, landing.Identity.Candidate.BaseRevision)
	if len(landed) != 1 {
		t.Fatalf("the feature branch holds %v", landed)
	}
	recordedRebases := journey.UnitRebases(t, f.Repository(), stream, "dedupe")
	if len(recordedRebases) != 1 {
		t.Fatalf("rebase records %+v", recordedRebases)
	}
	rebase := recordedRebases[0]
	if rebase.State != intake.UnitApproved || rebase.Snapshot != candidate || rebase.Onto != landed[0] || len(rebase.Conflicts) != 0 || rebase.Commit == candidate {
		t.Fatalf("rebase.json %+v", rebase)
	}
	if got, err := g.ChangeOf(ctx, rebase.Commit); err != nil || got != change {
		t.Fatalf("dedupe's rebased commit %s is change %q, %v; want %s", rebase.Commit, got, err, change)
	}
	if tip, _, err := g.Branch(ctx, execution.UnitBranch(stream, "dedupe")); err != nil || tip != rebase.Commit {
		t.Fatalf("dedupe's branch is at %s, %v; want %s", tip, err, rebase.Commit)
	}
	c, err := g.Commit(ctx, rebase.Commit)
	fixturekit.Must(t, err)
	if service.OperationTrailer(c.Message) != rebases[0].Operation.ID || !slices.Equal(c.Parents, landed) {
		t.Fatalf("dedupe's rebased commit %+v", c)
	}
	_, report := latestReport(t, f.Repository(), stream, "dedupe")
	if report.Candidate != rebase.Commit || report.Base != landed[0] {
		t.Fatalf("dedupe's report after its rebase %+v", report)
	}
	back := slices.IndexFunc(intakefixture.AllTransitions(t, f.S.Cfg.Root, f.Project, stream), func(tr trace.Transition) bool {
		return tr.ID == trace.UnitSubject("dedupe")+"-checking-rebase-1" && tr.From == intake.UnitApproved && tr.To == execution.UnitChecking && strings.HasPrefix(tr.Reason, "the approval no longer holds: ")
	})
	if back < 0 {
		t.Fatal("dedupe's return to review is not recorded")
	}
	if kept, err := execution.ReadUnitChange(f.Repository(), stream, "dedupe"); err != nil || kept != change {
		t.Fatalf("dedupe's recorded change is %q after its rebase: %v", kept, err)
	}
	if ops := journey.LandOperations(t, f.Repository(), stream); len(ops) != 1 {
		t.Fatalf("the rebased approval was asked to land: %+v", ops)
	}

	for _, commit := range []string{candidate, rebase.Commit} {
		result, apiErr := journey.InspectForTest(f.S).TraceView(ctx, string(stream), "commit", commit)
		if apiErr != nil {
			t.Fatalf("the walk of %s: %+v", commit, apiErr)
		}
		walk := result.(api.CommitTrace)
		if !slices.ContainsFunc(walk.Records, func(r api.CommitRecord) bool {
			return r.Role == service.CommitChange && r.Unit == "dedupe" && r.Ref.Path == "units/dedupe/change.json"
		}) || slices.ContainsFunc(walk.Records, func(r api.CommitRecord) bool { return r.Role == service.CommitChange && r.Unit != "dedupe" }) {
			t.Fatalf("the walk of %s records %+v", commit, walk.Records)
		}
		data, err := json.Marshal(walk)
		fixturekit.Must(t, err)
		if strings.Contains(string(data), change) {
			t.Fatalf("the walk of %s shows the change ID: %s", commit, data)
		}
	}
}
