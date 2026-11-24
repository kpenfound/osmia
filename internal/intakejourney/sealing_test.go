package intakejourney

import (
	context "context"
	errors "errors"
	fmt "fmt"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/journey"
	os "os"
	filepath "path/filepath"
	slices "slices"
	strings "strings"
	"sync/atomic"
	testing "testing"
	time "time"

	config "github.com/kpenfound/osmia/internal/config"
	intake "github.com/kpenfound/osmia/internal/intake"
	kb "github.com/kpenfound/osmia/internal/kb"
	plan "github.com/kpenfound/osmia/internal/plan"
	router "github.com/kpenfound/osmia/internal/router"
	seal "github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	shed "github.com/kpenfound/osmia/internal/shed"
	trace "github.com/kpenfound/osmia/internal/trace"
)

func TestSealingRecordsTheSealAndCreatesTheFeatureBranch(t *testing.T) {
	t.Parallel()
	f := journey.NewDebateFixture(t, 1, 1)
	defer f.Stop(t)
	ctx := context.Background()
	commit := f.Upstream(t)
	home := filepath.Dir(f.Clone)
	stream, out := f.Ratified(t)
	if out.Sealing != "requested" || !strings.HasSuffix(out.Detail, "; the sealing is asked for") {
		t.Fatalf("ratification %+v", out)
	}
	f.AwaitFeature(t, stream, intake.BuildingState)

	ops := fixturekit.AwaitAcknowledged(t, func(t *testing.T) []trace.OperationRecord { return f.SealOperations(t, stream) })
	if len(ops) != 1 || ops[0].Result == nil || ops[0].Result.Outcome != "succeeded" {
		t.Fatalf("seal operations %+v", ops)
	}
	operation := ops[0].Operation.ID
	if in, err := intake.DecodeSeal(ops[0].Operation); err != nil || in != (intake.SealInput{Seal: 1, Round: 1, Spec: 1, Plan: 1, Ratification: 1}) {
		t.Fatalf("seal operation %+v: %v", ops[0].Operation, err)
	}
	if moves := f.SealMoves(t, stream); !slices.Equal(moves, []string{"sealing-1"}) {
		t.Fatalf("seal subject went %v", moves)
	}
	if request := f.Transition(t, stream, "seal-1"); request.Cause != shed.RatificationDocumentID(1)+"-1" || request.Reason != "the owner ratified spec.md revision 1 and plan.json revision 1 in round 1; sealing 1 fetches upstream, records the seal and the footprints and creates the feature branch" {
		t.Fatalf("the request %+v", request)
	}

	docs := f.Documents(t, stream, seal.DocumentID)
	if len(docs) != 1 || docs[0].Path != seal.Path || docs[0].Revision != 1 || docs[0].Actor != intake.SealingActor || docs[0].Cause != operation {
		t.Fatalf("seal documents %+v", docs)
	}
	record, err := seal.Parse([]byte(docs[0].Content))
	fixturekit.Must(t, err)
	branch := "osmia/" + string(stream)
	workspace := filepath.Join(f.Opts.Config.Root, "branches", string(f.Project), string(stream))
	spec := f.Documents(t, stream, plan.SpecDocument)
	entities, err := kb.Load(f.Repository())
	fixturekit.Must(t, err)
	footprint := entities.ResolveEntities([]string{"internal.trace"})
	want := seal.Seal{Version: 1, Seal: 1, Round: 1, Revision: shed.Pin{Spec: 1, Plan: 1}, SpecHash: seal.SpecHash(spec[0].Content),
		Base: seal.Base{Remote: "upstream", Branch: "main", Commit: commit}, Branch: branch, Workspace: workspace,
		Footprints: []seal.Footprint{{Unit: "resume", Entities: footprint.Entities, Paths: footprint.Paths}, {Unit: "dedupe", Entities: footprint.Entities, Paths: footprint.Paths}}}
	if wantContent, err := seal.Encode(want); err != nil || docs[0].Content != string(wantContent) {
		t.Fatalf("seal.json:\n%s\nwant:\n%s\n%v", docs[0].Content, wantContent, err)
	}
	if len(footprint.Entities) == 0 || len(footprint.Paths) == 0 || len(footprint.Unresolved) != 0 || len(record.SpecHash) != 71 {
		t.Fatalf("the footprint %+v of seal %+v", footprint, record)
	}
	if data, err := fixturekit.WorkstreamDocument(t, f.S.Current().Root, f.Project, stream, seal.Path); err != nil || string(data) != docs[0].Content {
		t.Fatalf("seal.json on disk: %q %v", data, err)
	}

	if tip := strings.TrimSpace(fixturekit.DemoGit(t, home, "-C", f.Clone, "rev-parse", "refs/heads/"+branch)); tip != commit {
		t.Fatalf("the feature branch is at %s, want %s", tip, commit)
	}
	if on := strings.TrimSpace(fixturekit.DemoGit(t, home, "-C", workspace, "rev-parse", "--abbrev-ref", "HEAD")); on != branch {
		t.Fatalf("the workspace is on %s", on)
	}
	if _, err := os.Stat(filepath.Join(workspace, "internal", "trace", "git.go")); err != nil {
		t.Fatalf("the workspace's files: %v", err)
	}
	if refs := fixturekit.DemoGit(t, home, "-C", filepath.Join(home, "remotes", "dagger", "dagger.git"), "for-each-ref", "--format=%(refname)"); strings.TrimSpace(refs) != "refs/heads/main" {
		t.Fatalf("upstream's refs after the seal:\n%s", refs)
	}

	moved := f.Transition(t, stream, intake.RatifiedState)
	reason := fmt.Sprintf("sealed spec.md revision 1 and plan.json revision 1 at %s of upstream/main (%s); feature branch %s is checked out in %s; the footprints of 2 units are recorded", commit, record.SpecHash, branch, workspace)
	if moved.From != intake.InShedState || moved.To != intake.RatifiedState || moved.Cause != operation || moved.Actor != intake.SealingActor || moved.Reason != reason {
		t.Fatalf("the move to ratified %+v", moved)
	}
	if body := f.Notice(t, stream, intake.RatifiedState); body != "Workstream state changed from in-shed to ratified: "+reason {
		t.Fatalf("the notice %q", body)
	}
	if ops[0].Result.Evidence != reason {
		t.Fatalf("the result's evidence %q", ops[0].Result.Evidence)
	}
	status, err := f.C.Status(ctx, stream)
	fixturekit.Must(t, err)
	if status.State == nil || *status.State != intake.BuildingState {
		t.Fatalf("status %+v", status)
	}

	if _, err := f.C.Ratify(ctx, stream, 1, 1); !intakefixture.Failed(err, api.Conflict) || !strings.Contains(err.Error(), "is building; the owner takes part in the shed while it is in-shed or sketched") {
		t.Fatalf("ratifying a sealed workstream: %v", err)
	}
	if docs := f.Documents(t, stream, seal.DocumentID); len(docs) != 1 {
		t.Fatalf("seal documents after: %+v", docs)
	}
	if out := fixturekit.DemoGit(t, home, "-C", f.Clone, "worktree", "list", "--porcelain"); strings.Count(out, "worktree ") != 2 {
		t.Fatalf("worktrees:\n%s", out)
	}
}

func TestSealingRetriesAFailedFetch(t *testing.T) {
	t.Parallel()
	f := journey.NewDebateFixture(t, 1, 1)
	defer f.Stop(t)
	ctx := context.Background()
	stream, _ := f.Ratified(t)
	reason := f.AwaitRetry(t, stream, 1, "the clone has no remote whose URL names dagger/dagger")
	f.StillInShed(t, stream)
	again, err := f.C.Ratify(ctx, stream, 1, 1)
	fixturekit.Must(t, err)
	if again.Sealing != "pending" && again.Sealing != "running" {
		t.Fatalf("ratifying again %+v", again)
	}

	pending := fmt.Sprintf("workstream %s is ratified at spec.md revision 1 and plan.json revision 1 already; sealing 1 is %s", stream, again.Sealing)
	if again.Detail != pending && again.Detail != pending+" after a failed attempt: "+reason {
		t.Fatalf("ratifying again %+v", again)
	}
	if ops := f.SealOperations(t, stream); len(ops) != 1 {
		t.Fatalf("ratifying again asked for %+v", ops)
	}
	if docs := f.Documents(t, stream, shed.RatificationDocumentID(1)); len(docs) != 1 {
		t.Fatalf("ratifying again recorded %+v", docs)
	}

	fixturekit.Must(t, (&intake.Sealer{S: f.S.Intake().SealerDependencies(), Repository: f.Repository()}).Pass(ctx))
	if ops := f.SealOperations(t, stream); len(ops) != 1 {
		t.Fatalf("the pass asked for %+v", ops)
	}
	if moves := f.SealMoves(t, stream); !slices.Equal(moves, []string{"sealing-1"}) {
		t.Fatalf("seal subject went %v", moves)
	}

	commit := f.Upstream(t)
	f.AwaitFeature(t, stream, intake.BuildingState)
	ops := fixturekit.AwaitAcknowledged(t, func(t *testing.T) []trace.OperationRecord { return f.SealOperations(t, stream) })
	if len(ops) != 1 || ops[0].Result == nil || ops[0].Result.Outcome != "succeeded" {
		t.Fatalf("seal operations %+v", ops)
	}
	record, _, found, err := seal.Latest(f.Repository(), stream)
	fixturekit.Must(t, err)
	if !found || record.Seal != 1 || record.Base.Commit != commit {
		t.Fatalf("the seal %+v", record)
	}
}

func TestSealingResumesFromTheBranchTheCloneHolds(t *testing.T) {
	t.Parallel()
	f := journey.NewDebateFixture(t, 1, 1)
	defer f.Stop(t)
	ctx := context.Background()
	commit := f.Upstream(t)
	home := filepath.Dir(f.Clone)
	p := &intakefixture.Faults{}
	f.Member(1, 1, 1, intakefixture.Concedes(p))
	stream := f.HandIn(t, "design", fixturekit.HandedDesign)
	f.AwaitShed(t, stream, "concluded-1")
	p.Check(t)
	f.AwaitPacket(t, stream, "ratify: no objection stands")
	branch := "osmia/" + string(stream)

	fixturekit.Must(t, os.WriteFile(filepath.Join(f.Clone, "local.txt"), []byte("local\n"), 0600))
	fixturekit.DemoGit(t, home, "-C", f.Clone, "add", "local.txt")
	fixturekit.DemoGit(t, home, "-C", f.Clone, "-c", "user.name=Owner", "-c", "user.email=owner@example.invalid", "commit", "-qm", "local")
	local := strings.TrimSpace(fixturekit.DemoGit(t, home, "-C", f.Clone, "rev-parse", "HEAD"))
	fixturekit.DemoGit(t, home, "-C", f.Clone, "branch", branch, local)

	_, err := f.C.Ratify(ctx, stream, 1, 1)
	fixturekit.Must(t, err)
	f.AwaitSealMove(t, stream, "failed-1")
	reason := fmt.Sprintf("sealing 1 of spec.md revision 1 and plan.json revision 1 failed: the clone has a branch %s at %s that is not on upstream/main; it is not the service's feature branch", branch, local)
	if got := f.Transition(t, stream, "seal-1-failed"); got.Reason != reason || got.From != "sealing-1" || got.Cause != "seal-1" {
		t.Fatalf("the failure %+v", got)
	} else if got.To != "failed-1" || got.DefaultTo != "failed-1" || got.DecidedBy != trace.DecidedByCode || got.Departed() {
		t.Fatalf("the failure does not record the default it took: %+v", got)
	}
	if rec, ok := f.RouteOf(t, stream, intake.TriggerSealFailed); !ok || rec.Outcome != router.OutcomeFollowed || rec.Default != intake.TargetFailed || rec.State != router.StateCommitted {
		t.Fatalf("route %+v (found %v)", rec, ok)
	}
	if body := f.Notice(t, stream, "seal-1-failed"); body != fmt.Sprintf("The sealing of spec.md revision 1 and plan.json revision 1 failed and the workstream is not ratified: the clone has a branch %s at %s that is not on upstream/main; it is not the service's feature branch. Once that is put right, ratifying the same revisions again asks for the sealing again.", branch, local) {
		t.Fatalf("the notice %q", body)
	}
	ops := fixturekit.AwaitAcknowledged(t, func(t *testing.T) []trace.OperationRecord { return f.SealOperations(t, stream) })
	if len(ops) != 1 || ops[0].Result == nil || ops[0].Result.Outcome != "failed" || ops[0].Result.Evidence != reason {
		t.Fatalf("seal operations %+v", ops)
	}
	f.StillInShed(t, stream)
	if _, err := os.Stat(filepath.Join(f.Opts.Config.Root, "branches", string(f.Project), string(stream))); !os.IsNotExist(err) {
		t.Fatalf("a workspace was made on the stray branch: %v", err)
	}

	fixturekit.Must(t, (&intake.Sealer{S: f.S.Intake().SealerDependencies(), Repository: f.Repository()}).Pass(ctx))
	if ops := f.SealOperations(t, stream); len(ops) != 1 {
		t.Fatalf("the pass asked for %+v", ops)
	}
	fixturekit.DemoGit(t, home, "-C", f.Clone, "branch", "-f", branch, commit)
	again, err := f.C.Ratify(ctx, stream, 1, 1)
	fixturekit.Must(t, err)
	if again.Sealing != "requested" || again.Detail != fmt.Sprintf("workstream %s is ratified at spec.md revision 1 and plan.json revision 1 already; sealing 1 failed and the sealing is asked for again", stream) {
		t.Fatalf("ratifying again %+v", again)
	}

	docs := f.Documents(t, stream, shed.RatificationDocumentID(1))
	if len(docs) != 2 || docs[1].Revision != 2 || docs[1].Content != docs[0].Content {
		t.Fatalf("ratification documents %+v", docs)
	}
	if got := f.Transition(t, stream, shed.RatificationDocumentID(1)+"-2"); got.To != "ratified-1" || got.Reason != "the owner ratified spec.md revision 1 and plan.json revision 1 after round 1 again; sealing 1 failed and the sealing is asked for again" {
		t.Fatalf("the second ratification %+v", got)
	}
	f.AwaitFeature(t, stream, intake.BuildingState)
	ops = fixturekit.AwaitAcknowledged(t, func(t *testing.T) []trace.OperationRecord { return f.SealOperations(t, stream) })
	if len(ops) != 2 || ops[1].Result == nil || ops[1].Result.Outcome != "succeeded" {
		t.Fatalf("seal operations %+v", ops)
	}

	if i := slices.IndexFunc(ops[1].History, func(a trace.OperationAction) bool { return a.Kind == "observe" }); i < 0 || ops[1].History[i].Observation.Evidence != fmt.Sprintf("the clone has feature branch %s; the sealing resumes from it", branch) {
		t.Fatalf("the observation of sealing 2: %+v", ops[1].History)
	}
	record, doc, found, err := seal.Latest(f.Repository(), stream)
	fixturekit.Must(t, err)
	if !found || doc.Revision != 1 || record.Seal != 2 || record.Base.Commit != commit {
		t.Fatalf("the seal %+v (revision %d)", record, doc.Revision)
	}
	if moves := f.SealMoves(t, stream); !slices.Equal(moves, []string{"sealing-1", "failed-1", "sealing-2"}) {
		t.Fatalf("seal subject went %v", moves)
	}
	if tip := strings.TrimSpace(fixturekit.DemoGit(t, home, "-C", f.Clone, "rev-parse", "refs/heads/"+branch)); tip != commit {
		t.Fatalf("the feature branch is at %s, want %s", tip, commit)
	}
	if out := fixturekit.DemoGit(t, home, "-C", f.Clone, "worktree", "list", "--porcelain"); strings.Count(out, "worktree ") != 2 {
		t.Fatalf("worktrees:\n%s", out)
	}
}

func TestSealingSealsTheLatestRatificationOnly(t *testing.T) {
	t.Parallel()
	f := journey.NewDebateFixture(t, 1, 1)
	defer f.Stop(t)
	ctx := context.Background()
	stream, _ := f.Ratified(t)
	f.AwaitRetry(t, stream, 1, "no remote whose URL names")

	intakefixture.RecordSpec(t, f.Repository(), stream, strings.Replace(fixturekit.ValidSpec, "never sent again", "not sent twice", 1))
	f.AwaitDocument(t, stream, plan.SpecDocument, 2)
	f.AwaitPacket(t, stream, "ratify: no objection stands")
	second, err := f.C.Ratify(ctx, stream, 2, 1)
	fixturekit.Must(t, err)
	if second.Sealing != "requested" || !strings.HasSuffix(second.Detail, "; the sealing is asked for") {
		t.Fatalf("the second ratification %+v", second)
	}

	if got := f.AwaitSealFailure(t, stream, 1); got.Reason != "sealing 1 of spec.md revision 1 and plan.json revision 1 failed: the owner's latest ratification is of spec.md revision 2 and plan.json revision 1 in round 1, not of spec.md revision 1 and plan.json revision 1 in round 1" {
		t.Fatalf("the failure %+v", got)
	}
	commit := f.Upstream(t)
	f.AwaitFeature(t, stream, intake.BuildingState)
	if moves := f.SealMoves(t, stream); !slices.Equal(moves, []string{"sealing-1", "failed-1", "sealing-2"}) && !slices.Equal(moves, []string{"sealing-1", "sealing-2", "sealing-2"}) {
		t.Fatalf("seal subject went %v", moves)
	}
	record, _, found, err := seal.Latest(f.Repository(), stream)
	fixturekit.Must(t, err)
	spec := f.Documents(t, stream, plan.SpecDocument)
	if !found || record.Seal != 2 || record.Revision != (shed.Pin{Spec: 2, Plan: 1}) || record.SpecHash != seal.SpecHash(spec[1].Content) || record.Base.Commit != commit {
		t.Fatalf("the seal %+v", record)
	}
	if ops := fixturekit.AwaitAcknowledged(t, func(t *testing.T) []trace.OperationRecord { return f.SealOperations(t, stream) }); len(ops) != 2 || ops[0].Result.Outcome != "failed" || ops[1].Result.Outcome != "succeeded" {
		t.Fatalf("seal operations %+v", ops)
	}

}

func TestSealingChecksAbandonmentAndResumesAfterTheBranchIsCreated(t *testing.T) {
	t.Parallel()
	var boundary atomic.Pointer[func(string) error]
	setBoundary := func(fn func(string) error) { boundary.Store(&fn) }
	f := journey.NewDebateFixtureWith(t, 1, 1, "", func(opts *journey.Options) {
		opts.Boundary = func(_ context.Context, name string) error {
			if fn := boundary.Load(); fn != nil {
				return (*fn)(name)
			}
			return nil
		}
	})
	ctx := context.Background()
	f.Stop(t)
	f.Opts.Committee = nil
	f.Start(t)
	defer f.Stop(t)
	commit := f.Upstream(t)
	home := filepath.Dir(f.Clone)
	skipped := func(key string) config.WorkstreamID {
		stream := f.HandIn(t, key, fixturekit.HandedDesign)
		f.Await(t, stream, intakefixture.Sketched)
		if _, err := f.C.ShedSkip(ctx, stream); err != nil {
			t.Fatal(err)
		}
		f.AwaitPacket(t, stream, "ratify: no objection stands")
		return stream
	}
	gone := skipped("first")
	abandoned := make(chan error, 1)
	setBoundary(func(name string) error {
		if name == "seal-branch-created" && len(abandoned) == 0 {
			_, err := f.C.Abandon(ctx, gone, "Not needed after all.")
			abandoned <- err
		}
		return nil
	})
	if _, err := f.C.Ratify(ctx, gone, 1, 1); err != nil {
		t.Fatal(err)
	}
	f.AwaitSealMove(t, gone, "failed-1")
	fixturekit.Must(t, <-abandoned)
	branch := "osmia/" + string(gone)
	if got := f.Transition(t, gone, "seal-1-failed"); got.Reason != fmt.Sprintf("sealing 1 of spec.md revision 1 and plan.json revision 1 failed: the owner abandoned the workstream; its feature branch %s stays in the clone", branch) {
		t.Fatalf("the failure %+v", got)
	}
	if tip := strings.TrimSpace(fixturekit.DemoGit(t, home, "-C", f.Clone, "rev-parse", "refs/heads/"+branch)); tip != commit {
		t.Fatalf("the abandoned workstream's branch is at %q, want %s", tip, commit)
	}
	if _, _, found, err := seal.Latest(f.Repository(), gone); err != nil || found {
		t.Fatalf("seal of the abandoned workstream: %v %v", found, err)
	}
	if state, err := f.Repository().Workflow(gone, trace.FeatureSubject); err != nil || state.Value != intake.AbandonedState {
		t.Fatalf("feature %+v %v", state, err)
	}

	stream := skipped("second")
	crashed := make(chan struct{}, 1)
	setBoundary(func(name string) error {
		if name == "seal-branch-created" && len(crashed) == 0 {
			crashed <- struct{}{}
			return errors.New("crash")
		}
		return nil
	})
	if _, err := f.C.Ratify(ctx, stream, 1, 1); err != nil {
		t.Fatal(err)
	}
	f.AwaitFeature(t, stream, intake.BuildingState)
	fixturekit.AwaitAcknowledged(t, func(t *testing.T) []trace.OperationRecord { return f.BuildOperations(t, stream) })
	boundary.Store(nil)
	ops := fixturekit.AwaitAcknowledged(t, func(t *testing.T) []trace.OperationRecord { return f.SealOperations(t, stream) })
	if len(ops) != 1 || ops[0].Result == nil || ops[0].Result.Outcome != "succeeded" {
		t.Fatalf("seal operations %+v", ops)
	}
	var observed []string
	retries := 0
	for _, a := range ops[0].History {
		switch a.Kind {
		case "observe":
			observed = append(observed, a.Observation.Evidence)
		case "retry":
			retries++
			if !strings.Contains(a.Failure, "crash") {
				t.Fatalf("retry %+v", a)
			}
		}
	}
	branch = "osmia/" + string(stream)
	if retries != 1 || !slices.Equal(observed, []string{"the clone has no feature branch " + branch, fmt.Sprintf("the clone has feature branch %s; the sealing resumes from it", branch)}) {
		t.Fatalf("%d retries, observed %q", retries, observed)
	}
	record, _, found, err := seal.Latest(f.Repository(), stream)
	fixturekit.Must(t, err)
	if !found || record.Base.Commit != commit || record.Seal != 1 {
		t.Fatalf("the seal %+v", record)
	}
	if out := fixturekit.DemoGit(t, home, "-C", f.Clone, "worktree", "list", "--porcelain"); strings.Count(out, "branch refs/heads/"+branch+"\n") != 1 {
		t.Fatalf("worktrees:\n%s", out)
	}

	third := skipped("third")
	recorded := make(chan struct{}, 1)
	setBoundary(func(name string) error {
		if name == "seal-recorded" && len(recorded) == 0 {
			recorded <- struct{}{}
			return errors.New("crash")
		}
		return nil
	})
	if _, err := f.C.Ratify(ctx, third, 1, 1); err != nil {
		t.Fatal(err)
	}
	f.AwaitFeature(t, third, intake.BuildingState)
	fixturekit.AwaitAcknowledged(t, func(t *testing.T) []trace.OperationRecord { return f.BuildOperations(t, third) })
	boundary.Store(nil)
	if docs := f.Documents(t, third, seal.DocumentID); len(docs) != 1 || docs[0].Revision != 1 {
		t.Fatalf("seal documents after the crash %+v", docs)
	}
	ops = fixturekit.AwaitAcknowledged(t, func(t *testing.T) []trace.OperationRecord { return f.SealOperations(t, third) })
	if len(ops) != 1 || ops[0].Result == nil || ops[0].Result.Outcome != "succeeded" || slices.IndexFunc(ops[0].History, func(a trace.OperationAction) bool { return a.Kind == "retry" }) < 0 {
		t.Fatalf("seal operations %+v", ops)
	}
}

func TestSealingFailsOnAFootprintTheMapDoesNotResolve(t *testing.T) {
	t.Parallel()
	f := journey.NewDebateFixture(t, 1, 1)
	defer f.Stop(t)
	ctx := context.Background()
	stream, _ := f.Ratified(t)
	f.AwaitRetry(t, stream, 1, "no remote whose URL names")
	fixturekit.Must(t, kb.Store(ctx, f.Repository(), kb.Map{Version: kb.Version, Entities: []kb.Entity{{ID: "other", Name: "Other", Paths: []string{"other/**"}}}}, f.Clock.Now(), intake.OwnerActor, "test"))
	f.AwaitSealMove(t, stream, "failed-1")
	if got := f.Transition(t, stream, "seal-1-failed"); got.Reason != "sealing 1 of spec.md revision 1 and plan.json revision 1 failed: the plan's footprints name what the entity map does not resolve: resume: internal.trace, dedupe: internal.trace" {
		t.Fatalf("the failure %+v", got)
	}
	f.StillInShed(t, stream)
}

func newSealingRestartFixture(t *testing.T) *journey.ShedFixture {
	t.Helper()
	f := journey.NewDebateFixtureWith(t, 1, 1, "", func(opts *journey.Options) {
		opts.WriteTimeout = time.Minute
	})
	if f.C.DefaultTimeout < time.Minute || f.S.Options.WriteTimeout < time.Minute {
		f.Stop(t)
		t.Fatalf("sealing restart API budgets: client %s, server %s; want at least 1m each", f.C.DefaultTimeout, f.S.Options.WriteTimeout)
	}
	return f
}

func TestSealingIsAskedForAfterARestartAndSealsFromSketched(t *testing.T) {
	t.Parallel()
	f := newSealingRestartFixture(t)
	ctx := context.Background()
	f.Stop(t)
	f.Opts.Committee = nil
	f.Start(t)
	commit := f.Upstream(t)
	stream := f.HandIn(t, "design", fixturekit.HandedDesign)
	f.Await(t, stream, intakefixture.Sketched)
	gone := f.HandIn(t, "second", fixturekit.HandedDesign)
	f.Await(t, gone, intakefixture.Sketched)
	f.Stop(t)

	repo, err := trace.Open(f.S.Current().Root, f.S.Current().Project)
	fixturekit.Must(t, err)

	ratify := func(stream config.WorkstreamID) {
		owner, err := repo.Workflow(stream, intake.OwnerSubject)
		fixturekit.Must(t, err)
		skip := trace.Transaction{ExpectedVersion: owner.Version, Transition: trace.Transition{Header: intake.OwnerHeader(intake.SkipTransition, f.Project, stream, "owner-shed", f.Clock.Now()), Subject: intake.OwnerSubject, From: "", To: intake.SkippedValue, Reason: "planted"}}
		_, err = repo.Transact(ctx, skip)
		fixturekit.Must(t, err)
		content, err := shed.EncodeRatification(shed.Ratify(1, shed.Pin{Spec: 1, Plan: 1}, nil))
		fixturekit.Must(t, err)
		fixturekit.Must(t, repo.RecordDocuments(ctx, []trace.Document{{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: shed.RatificationDocumentID(1), Revision: 1, Project: f.Project, Workstream: stream, At: f.Clock.Now(), Actor: intake.OwnerActor, Cause: "owner-shed"}, Path: shed.RatificationPath(1), Content: string(content)}}))
	}
	ratify(stream)

	ratify(gone)
	latest, found, err := intake.LatestRatification(repo, gone)
	fixturekit.Must(t, err)
	if !found {
		t.Fatal("no ratification planted")
	}
	if _, err := (&intake.Sealer{S: f.S.Intake().SealerDependencies(), Repository: repo}).Request(ctx, gone, latest); err != nil {
		t.Fatal(err)
	}
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: intake.AbandonTransition, Revision: 1, Project: f.Project, Workstream: gone, At: f.Clock.Now(), Actor: intake.OwnerActor, Cause: intake.AbandonTransition}
	_, err = repo.SetFeatureState(ctx, h, intake.AbandonedState, "Not needed.")
	fixturekit.Must(t, err)
	fixturekit.Must(t, repo.Close())

	f.Start(t)
	defer f.Stop(t)
	f.AwaitFeature(t, stream, intake.BuildingState)
	f.AwaitSealMove(t, gone, "failed-1")
	if got := f.Transition(t, gone, "seal-1-failed"); got.Reason != "sealing 1 of spec.md revision 1 and plan.json revision 1 failed: the workstream is abandoned, not in the shed" {
		t.Fatalf("the failure %+v", got)
	}
	if state, err := f.Repository().Workflow(gone, trace.FeatureSubject); err != nil || state.Value != intake.AbandonedState {
		t.Fatalf("feature %+v %v", state, err)
	}
	if _, found, err := intakefixture.ProviderOf(t, intake.FeatureWorkspaces(f.S.Current(), f.Repository()), gone).Branch(ctx, "osmia/"+string(gone)); err != nil || found {
		t.Fatalf("branch of the abandoned workstream: %v %v", found, err)
	}
	if moves := f.SealMoves(t, stream); !slices.Equal(moves, []string{"sealing-1"}) {
		t.Fatalf("seal subject went %v", moves)
	}
	moved := f.Transition(t, stream, intake.RatifiedState)
	if moved.From != intake.SketchedState || moved.To != intake.RatifiedState {
		t.Fatalf("the move to ratified %+v", moved)
	}
	record, _, found, err := seal.Latest(f.Repository(), stream)
	fixturekit.Must(t, err)
	if !found || record.Base.Commit != commit || record.Round != 1 {
		t.Fatalf("the seal %+v", record)
	}

	fixturekit.Must(t, (&intake.Sealer{S: f.S.Intake().SealerDependencies(), Repository: f.Repository()}).Pass(ctx))
	if ops := f.SealOperations(t, stream); len(ops) != 1 {
		t.Fatalf("seal operations %+v", ops)
	}
}

func TestStaleSealingFailureLeavesTheSubjectToTheLaterOne(t *testing.T) {
	t.Parallel()
	f := newSealingRestartFixture(t)
	ctx := context.Background()
	f.Stop(t)
	f.Opts.Committee = nil
	f.Start(t)
	stream := f.HandIn(t, "design", fixturekit.HandedDesign)
	f.Await(t, stream, intakefixture.Sketched)
	if _, err := f.C.ShedSkip(ctx, stream); err != nil {
		t.Fatal(err)
	}
	f.AwaitPacket(t, stream, "ratify: no objection stands")

	if _, err := f.C.Ratify(ctx, stream, 1, 1); err != nil {
		t.Fatal(err)
	}
	f.AwaitRetry(t, stream, 1, "no remote whose URL names")
	f.Stop(t)
	repo, err := trace.Open(f.S.Current().Root, f.S.Current().Project)
	fixturekit.Must(t, err)
	z := &intake.Sealer{S: f.S.Intake().SealerDependencies(), Repository: repo}
	fixturekit.Must(t, repo.RecordDocuments(ctx, []trace.Document{{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: plan.SpecDocument, Revision: 2, Project: f.Project, Workstream: stream, At: f.Clock.Now(), Actor: intake.OwnerActor, Cause: "owner-edit"}, Path: plan.SpecPath, Content: strings.Replace(fixturekit.ValidSpec, "never sent again", "not sent twice", 1)}}))
	ratify := func(revision int, pin shed.Pin) intake.Ratification {
		content, err := shed.EncodeRatification(shed.Ratify(1, pin, nil))
		fixturekit.Must(t, err)
		fixturekit.Must(t, repo.RecordDocuments(ctx, []trace.Document{{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: shed.RatificationDocumentID(1), Revision: revision, Project: f.Project, Workstream: stream, At: f.Clock.Now(), Actor: intake.OwnerActor, Cause: "owner-shed"}, Path: shed.RatificationPath(1), Content: string(content)}}))
		latest, found, err := intake.LatestRatification(repo, stream)
		fixturekit.Must(t, err)
		if !found || latest.Recorded != revision {
			t.Fatalf("latest ratification %+v %v", latest, found)
		}
		return latest
	}
	if k, err := z.Request(ctx, stream, ratify(2, shed.Pin{Spec: 2, Plan: 1})); err != nil || k != 2 {
		t.Fatalf("sealing %d asked for: %v", k, err)
	}

	operation := func(k int) trace.OperationRecord {
		ops, err := repo.Operations(stream)
		fixturekit.Must(t, err)
		for _, o := range ops {
			if in, err := intake.DecodeSeal(o.Operation); err == nil && in.Seal == k {
				return o
			}
		}
		t.Fatalf("no sealing %d", k)
		return trace.OperationRecord{}
	}
	entities, err := kb.Load(repo)
	fixturekit.Must(t, err)
	fixturekit.Must(t, kb.Store(ctx, repo, kb.Map{Version: kb.Version, Entities: []kb.Entity{{ID: "other", Name: "Other", Paths: []string{"other/**"}}}}, f.Clock.Now(), intake.OwnerActor, "test"))
	result, err := z.Apply(ctx, operation(2).Operation)
	fixturekit.Must(t, err)
	if result.Outcome != "failed" || !strings.Contains(result.Evidence, "the entity map does not resolve") {
		t.Fatalf("sealing 2: %+v", result)
	}
	result, err = z.Apply(ctx, operation(1).Operation)
	fixturekit.Must(t, err)
	if result.Outcome != "failed" || result.Evidence != "sealing 1 of spec.md revision 1 and plan.json revision 1 failed: the owner's latest ratification is of spec.md revision 2 and plan.json revision 1 in round 1, not of spec.md revision 1 and plan.json revision 1 in round 1" {
		t.Fatalf("sealing 1: %+v", result)
	}
	transitions, err := trace.Read[trace.Transition](repo, stream)
	fixturekit.Must(t, err)
	var moves []string
	for _, tr := range transitions {
		if tr.Subject == intake.SealSubject {
			moves = append(moves, tr.To)
		}
		if tr.ID == "seal-1-failed" && (tr.From != "failed-2" || tr.To != "failed-2") {
			t.Fatalf("the stale failure moved the subject: %+v", tr)
		}
	}
	if !slices.Equal(moves, []string{"sealing-1", "sealing-2", "failed-2", "failed-2"}) {
		t.Fatalf("seal subject went %v", moves)
	}

	fixturekit.Must(t, kb.Store(ctx, repo, entities, f.Clock.Now(), intake.OwnerActor, "test"))
	if k, err := z.Request(ctx, stream, ratify(3, shed.Pin{Spec: 2, Plan: 1})); err != nil || k != 3 {
		t.Fatalf("sealing %d asked for: %v", k, err)
	}
	fixturekit.Must(t, repo.Close())

	commit := f.Upstream(t)
	f.Start(t)
	defer f.Stop(t)
	f.AwaitFeature(t, stream, intake.BuildingState)
	ops := fixturekit.AwaitAcknowledged(t, func(t *testing.T) []trace.OperationRecord { return f.SealOperations(t, stream) })
	if len(ops) != 3 || ops[0].Result.Outcome != "failed" || ops[1].Result.Outcome != "failed" || ops[2].Result.Outcome != "succeeded" {
		t.Fatalf("seal operations %+v", ops)
	}
	record, _, found, err := seal.Latest(f.Repository(), stream)
	fixturekit.Must(t, err)
	if !found || record.Seal != 3 || record.Revision != (shed.Pin{Spec: 2, Plan: 1}) || record.Base.Commit != commit {
		t.Fatalf("the seal %+v", record)
	}
	if moves := f.SealMoves(t, stream); !slices.Equal(moves, []string{"sealing-1", "sealing-2", "failed-2", "failed-2", "sealing-3"}) {
		t.Fatalf("seal subject went %v", moves)
	}
}
