package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/busybees/core/agent"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/kb"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/trace"
)

// demoPlan has resume and dedupe in internal.trace, ahead of upload and audit
// in internal.upload and internal.audit. No unit depends on another.
const demoPlan = `{"version": 1, "units": [
  {"id": "resume", "title": "Resume from the last chunk", "addresses": [{"criterion": "spec#1", "proof": {"kind": "new-test", "name": "TestResume"}}], "depends_on": [], "footprint": ["internal.trace"]},
  {"id": "dedupe", "title": "Skip acknowledged chunks", "addresses": [{"criterion": "spec#2", "proof": {"kind": "reviewer-judgement", "name": "no chunk is sent twice"}}], "depends_on": [], "footprint": ["internal.trace"]},
  {"id": "upload", "title": "Send chunks", "addresses": [{"criterion": "spec#1", "proof": {"kind": "new-test", "name": "TestUpload"}}], "depends_on": [], "footprint": ["internal.upload"]},
  {"id": "audit", "title": "Record acknowledged chunks", "addresses": [{"criterion": "spec#2", "proof": {"kind": "reviewer-judgement", "name": "acknowledgements are recorded"}}], "depends_on": [], "footprint": ["internal.audit"]}
]}
`

// TestM4ParallelUnitsDemonstration builds two workstreams of one project on
// two shared mason slots through the local API, with fake masons and chief of
// staff. See docs/m4-parallel-units.md.
func TestM4ParallelUnitsDemonstration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, masons := newParallelMasonFixture(t, 2, 3, demoPlan)
	defer func() { f.stop(t) }()
	dedupeReport := CriterionReport{Criterion: "spec#2", Done: "skip acknowledged chunks", Evidence: "no acknowledged chunk is sent again", Proof: "internal/trace/dedupe.go"}
	recovered := masonAgent("dedupe") + "-recover-1"
	masons.play[masonTurnID("resume")] = reportDone("Uploads resume")
	masons.play[recovered] = func(ctx context.Context, _ agent.Request, tools *mcp.ClientSession) error {
		if recorded, reason, err := done(ctx, tools, map[string]any{"outcome": "Acknowledged chunks are skipped", "criteria": []any{criterionArgs(dedupeReport)}}); err != nil || !recorded {
			return fmt.Errorf("done refused: %q %v", reason, err)
		}
		return nil
	}
	f.engine.mu.Lock()
	f.engine.turns[recovered] = masons.turn
	// A clean turn without an outcome gets a follow-up; failing it keeps the
	// unit implementing, holding its slot.
	for _, unit := range []string{"upload", "audit"} {
		for i := 1; i <= 3; i++ {
			name := fmt.Sprintf("%s-clarify-%d", masonAgent(unit), i)
			masons.play[name] = func(context.Context, agent.Request, *mcp.ClientSession) error { return errFailTurn }
			f.engine.turns[name] = masons.turn
		}
	}
	f.engine.mu.Unlock()

	// Both workstreams are built on the same plan while the factory is
	// paused: nothing starts, every ready unit says why, and paused
	// workstreams are not compared for overlap.
	factory := runtime.Target{Scope: "factory"}
	built := f.builtPaused(t, factory, "uploads", "audits")
	a, b := built[0], built[1]
	// The owner's priority order goes against the workstream ID order, which
	// would otherwise break the tie.
	lo, hi := lowHigh(a, b)
	units := []string{"resume", "dedupe", "upload", "audit"}
	for _, stream := range []config.WorkstreamID{lo, hi} {
		f.awaitDispatches(t, stream, "audit", 1)
		var want []UnitStatus
		for _, unit := range units {
			want = append(want, f.deferred(t, stream, unit, factoryPaused))
		}
		f.checkUnits(t, stream, want)
		if st, err := f.c.Status(ctx, stream); err != nil || len(st.Advisories) != 0 {
			t.Fatalf("advisories of %s while paused: %+v %v", stream, st.Advisories, err)
		}
	}

	// hi's dedupe mason saves a file and is still working when the service
	// stops.
	var mu sync.Mutex
	dedupeTurns := 0
	entered := make(chan struct{})
	f.engine.mu.Lock()
	f.engine.turns[masonTurnID("dedupe")] = func(ctx context.Context, req agent.Request, turn *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		if sessionStream(req) != string(hi) {
			return masons.turn(ctx, req, turn, tools)
		}
		mu.Lock()
		dedupeTurns++
		first := dedupeTurns == 1
		mu.Unlock()
		if err := os.WriteFile(filepath.Join(req.Workspace.Directory(), "internal", "trace", "dedupe.go"), []byte("package trace\n"), 0644); err != nil {
			return nil, err
		}
		if first {
			close(entered)
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	f.engine.mu.Unlock()

	// Priority: the owner puts hi first and lifts the pause. hi takes both
	// slots with its disjoint resume and upload; dedupe, ahead of upload in
	// the plan, shares resume's footprint and waits. resume's mason reports done, and its freed slot
	// goes to hi again, to dedupe, though lo never started a unit.
	mutation(t, f.c, "PUT", "priority", PriorityRequest{Project: f.project, Workstreams: []config.WorkstreamID{hi, lo}})
	mutation(t, f.c, "DELETE", "pause", factory)
	select {
	case <-entered:
	case <-time.After(demoTimeout):
		t.Fatal("the mason of dedupe never started")
	}
	f.awaitMasonRan(t, hi, "upload")
	if got, want := starts(t, f, hi), []string{trace.UnitSubject("resume"), trace.UnitSubject("upload"), trace.UnitSubject("dedupe")}; !slices.Equal(got, want) {
		t.Fatalf("%s started %v, want %v", hi, got, want)
	}
	if got := starts(t, f, lo); len(got) != 0 {
		t.Fatalf("%s started %v behind a higher-priority workstream", lo, got)
	}
	resumeDone := movedAt(t, f, hi, "resume", UnitReviewing)
	if upload := movedAt(t, f, hi, "upload", UnitImplementing); !upload.Before(resumeDone) {
		t.Fatalf("upload started at %s, after resume left implementing at %s", upload, resumeDone)
	}
	if dedupe := movedAt(t, f, hi, "dedupe", UnitImplementing); !dedupe.After(resumeDone) {
		t.Fatalf("dedupe started at %s, before resume left implementing at %s", dedupe, resumeDone)
	}
	behind := UnitDispatch{Reason: DeferPriority, Limit: 2, Workstreams: []config.WorkstreamID{hi}, Message: "Waits for a mason slot: all 2 are in use, and higher-priority workstreams start first: " + string(hi) + "."}
	f.checkUnits(t, hi, []UnitStatus{{Unit: "resume", State: UnitReviewing, Card: &exampleCard}, {Unit: "dedupe", State: UnitImplementing}, {Unit: "upload", State: UnitImplementing}, f.deferred(t, hi, "audit", slotless(2))})
	var want []UnitStatus
	for _, unit := range units {
		want = append(want, f.deferred(t, lo, unit, behind))
	}
	f.checkUnits(t, lo, want)
	if got, want := f.dispatches(t, hi, "dedupe"), []string{"deferred paused", "deferred entangled", "started"}; !slices.Equal(got, want) {
		t.Fatalf("decisions on dedupe of %s: %q, want %q", hi, got, want)
	}

	// Restart: the owner clears the priority while dedupe's mason is
	// working, and the service stops. On restart dedupe keeps its slot and
	// its workspace, with the interrupted turn's file, and gets one
	// continuation; nothing starts until it reports done.
	mutation(t, f.c, "DELETE", "priority", ClearPriorityRequest{Project: f.project})
	f.stop(t)
	f.start(t)

	// Rotation: at equal priority dedupe's freed slot goes to lo, which never
	// started a unit, and the slot lo's resume frees goes back to hi, which
	// started a unit longer ago.
	f.awaitMasonRan(t, lo, "resume")
	f.awaitMasonRan(t, hi, "audit")
	settle()
	masons.check(t)
	mu.Lock()
	if dedupeTurns != 1 {
		t.Fatalf("the first turn of dedupe ran %d times", dedupeTurns)
	}
	mu.Unlock()
	th := f.thread(t, hi, masonAgent("dedupe"))
	if len(th.Turns) != 2 || th.Turns[0].Request.TurnID != masonTurnID("dedupe") || th.Turns[0].Status() != "interrupted" || th.Turns[1].Request.TurnID != recovered || th.Turns[1].Status() != "idle" {
		t.Fatalf("dedupe's mason thread %+v", th.Turns)
	}
	workspace := filepath.Join(f.opts.Config.Root, unitsDirectory, string(f.project), string(hi), "dedupe")
	if data, err := os.ReadFile(filepath.Join(workspace, "internal", "trace", "dedupe.go")); err != nil || string(data) != "package trace\n" {
		t.Fatalf("dedupe's workspace lost the interrupted turn's file: %q %v", data, err)
	}
	if got, want := starts(t, f, hi), []string{trace.UnitSubject("resume"), trace.UnitSubject("upload"), trace.UnitSubject("dedupe"), trace.UnitSubject("audit")}; !slices.Equal(got, want) {
		t.Fatalf("%s started %v, want %v", hi, got, want)
	}
	if got, want := starts(t, f, lo), []string{trace.UnitSubject("resume")}; !slices.Equal(got, want) {
		t.Fatalf("%s started %v, want %v", lo, got, want)
	}
	order := []time.Time{movedAt(t, f, hi, "dedupe", UnitReviewing), movedAt(t, f, lo, "resume", UnitImplementing), movedAt(t, f, lo, "resume", UnitReviewing), movedAt(t, f, hi, "audit", UnitImplementing)}
	if !slices.IsSortedFunc(order, time.Time.Compare) {
		t.Fatalf("dedupe of %s finished, resume of %s started and finished, audit of %s started at %v", hi, lo, hi, order)
	}
	for stream, ran := range map[config.WorkstreamID][]string{
		hi: {masonTurnID("audit"), recovered, masonTurnID("resume"), masonTurnID("upload")},
		lo: {masonTurnID("resume")},
	} {
		var got []string
		for _, req := range masons.requests(stream) {
			if req.Name != masonAgent("upload")+"-clarify-1" && req.Name != masonAgent("audit")+"-clarify-1" {
				got = append(got, req.Name)
			}
		}
		if slices.Sort(got); !slices.Equal(got, ran) {
			t.Fatalf("mason turns of %s ran %v, want %v", stream, got, ran)
		}
	}
	f.checkUnits(t, hi, []UnitStatus{{Unit: "resume", State: UnitReviewing, Card: &exampleCard}, {Unit: "dedupe", State: UnitReviewing, Card: &exampleCard}, {Unit: "upload", State: UnitImplementing}, {Unit: "audit", State: UnitImplementing}})
	f.checkUnits(t, lo, []UnitStatus{{Unit: "resume", State: UnitReviewing, Card: &exampleCard}, f.deferred(t, lo, "dedupe", slotless(2)), f.deferred(t, lo, "upload", slotless(2)), f.deferred(t, lo, "audit", slotless(2))})
	if got, want := f.dispatches(t, lo, "dedupe"), []string{"deferred paused", "deferred priority", "deferred capacity", "deferred entangled", "deferred capacity"}; !slices.Equal(got, want) {
		t.Fatalf("decisions on dedupe of %s: %q, want %q", lo, got, want)
	}

	// Overlap: both workstreams build in the same code, so each chief of
	// staff is warned about the other while both are still building and
	// neither pull request is open. Neither is held back.
	mapping, err := kb.Load(f.repository())
	must(t, err)
	var paths []string
	for _, id := range []string{"internal.audit", "internal.trace", "internal.upload"} {
		e, ok := mapping.Lookup(id)
		if !ok {
			t.Fatalf("entity %s is missing", id)
		}
		paths = append(paths, e.Paths...)
	}
	slices.Sort(paths)
	for mine, other := range map[config.WorkstreamID]config.WorkstreamID{lo: hi, hi: lo} {
		message := fmt.Sprintf("Workstream %s of this project builds in subsystem internal, internal.audit, internal.upload: both sealed footprints cover internal.audit, internal.trace, internal.upload (seal 1 of this workstream, seal 1 of that one). Its changes may conflict with this workstream's before both pull requests are open. This is an advisory and blocks neither workstream; the owner can pause or reprioritise either.", other)
		f.awaitEventTurns(t, mine, message)
		st, err := f.c.Status(ctx, mine)
		must(t, err)
		want := []OverlapAdvisory{{Workstream: other, Seal: 1, OtherSeal: 1, Subsystems: []string{"internal", "internal.audit", "internal.upload"}, Entities: []string{"internal.audit", "internal.trace", "internal.upload"}, Paths: paths, Message: message}}
		if st.State == nil || *st.State != BuildingState || !reflect.DeepEqual(st.Advisories, want) {
			t.Fatalf("status of %s: state %v, advisories %+v, want %+v", mine, st.State, st.Advisories, want)
		}
	}
}

// movedAt returns when the mason controller moved the unit of the workstream
// to the given state for the first time.
func movedAt(t *testing.T, f *shedFixture, stream config.WorkstreamID, unit, to string) time.Time {
	t.Helper()
	for _, tr := range allTransitions(t, f.trace, stream) {
		if tr.Actor == masonActor && tr.Subject == trace.UnitSubject(unit) && tr.To == to {
			return tr.At
		}
	}
	t.Fatalf("unit %s of %s never moved to %s", unit, stream, to)
	return time.Time{}
}
