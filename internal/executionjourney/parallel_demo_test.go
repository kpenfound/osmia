package executionjourney

import (
	context "context"
	fmt "fmt"
	os "os"
	filepath "path/filepath"
	reflect "reflect"
	slices "slices"
	sync "sync"
	testing "testing"
	time "time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	agent "github.com/kpenfound/agentcore/agent"
	config "github.com/kpenfound/osmia/internal/config"
	kb "github.com/kpenfound/osmia/internal/kb"
	runtime "github.com/kpenfound/osmia/internal/runtime"
	trace "github.com/kpenfound/osmia/internal/trace"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

const demoPlan = `{"version": 2, "units": [
  {"id": "resume", "title": "Resume from the last chunk", "task": "Resume from the last chunk", "acceptance": ["spec#1 holds, shown by TestResume (new-test)"], "criteria": ["spec#1"], "depends_on": [], "footprint": ["internal.trace"]},
  {"id": "dedupe", "title": "Skip acknowledged chunks", "task": "Skip acknowledged chunks", "acceptance": ["spec#2 holds, shown by no chunk is sent twice (reviewer-judgement)"], "criteria": ["spec#2"], "depends_on": [], "footprint": ["internal.trace"]},
  {"id": "upload", "title": "Send chunks", "task": "Send chunks", "acceptance": ["spec#1 holds, shown by TestUpload (new-test)"], "criteria": ["spec#1"], "depends_on": [], "footprint": ["internal.upload"]},
  {"id": "audit", "title": "Record acknowledged chunks", "task": "Record acknowledged chunks", "acceptance": ["spec#2 holds, shown by acknowledgements are recorded (reviewer-judgement)"], "criteria": ["spec#2"], "depends_on": [], "footprint": ["internal.audit"]}
]}
`

func TestParallelUnitsDemonstration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	f, masons, passed := journey.NewParallelMasonFixtureCounting(t, 2, 4, demoPlan)
	defer func() { f.Stop(t) }()
	recovered := intake.MasonAgent("dedupe") + "-recover-1"
	masons.Play[execution.MasonTurnID("resume")] = intakefixture.ReportDone("Uploads resume")
	masons.Play[recovered] = func(ctx context.Context, _ agent.Request, tools *mcp.ClientSession) error {
		if recorded, reason, err := intakefixture.Done(ctx, tools, map[string]any{"outcome": "Acknowledged chunks are skipped"}); err != nil || !recorded {
			return fmt.Errorf("done refused: %q %v", reason, err)
		}
		return nil
	}
	f.Engine.Mu.Lock()
	f.Engine.Turns[recovered] = masons.Turn

	for _, unit := range []string{"upload", "audit"} {
		for i := 1; i <= 3; i++ {
			name := fmt.Sprintf("%s-clarify-%d", intake.MasonAgent(unit), i)
			masons.Play[name] = func(context.Context, agent.Request, *mcp.ClientSession) error { return intakefixture.ErrFailTurn }
			f.Engine.Turns[name] = masons.Turn
		}
	}
	f.Engine.Mu.Unlock()

	factory := runtime.Target{Scope: "factory"}
	built := builtPaused(f, t, factory, "uploads", "audits")
	a, b := built[0], built[1]

	lo, hi := intakefixture.LowHigh(a, b)
	units := []string{"resume", "dedupe", "upload", "audit"}
	for _, stream := range []config.WorkstreamID{lo, hi} {
		awaitDispatches(f, t, stream, "audit", 1)
		var want []api.UnitStatus
		for _, unit := range units {
			want = append(want, deferred(f, t, stream, unit, factoryPaused))
		}
		f.CheckUnits(t, stream, want)
		if st, err := f.C.Status(ctx, stream); err != nil || len(st.Advisories) != 0 {
			t.Fatalf("advisories of %s while paused: %+v %v", stream, st.Advisories, err)
		}
	}

	var mu sync.Mutex
	dedupeTurns := 0
	entered := make(chan struct{})
	f.Engine.Mu.Lock()
	f.Engine.Turns[execution.MasonTurnID("dedupe")] = func(ctx context.Context, req agent.Request, turn *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		if intakefixture.SessionStream(req) != string(hi) {
			return masons.Turn(ctx, req, turn, tools)
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
	f.Engine.Mu.Unlock()

	intakefixture.Mutation(t, f.C, "PUT", "priority", api.PriorityRequest{Project: f.Project, Workstreams: []config.WorkstreamID{hi, lo}})
	intakefixture.Mutation(t, f.C, "DELETE", "pause", factory)
	select {
	case <-entered:
	case <-time.After(fixturekit.DemoTimeout):
		t.Fatal("the mason of dedupe never started")
	}
	f.AwaitMasonRan(t, hi, "upload")
	if got, want := journey.Starts(t, f, hi), []string{trace.UnitSubject("resume"), trace.UnitSubject("upload"), trace.UnitSubject("dedupe")}; !slices.Equal(got, want) {
		t.Fatalf("%s started %v, want %v", hi, got, want)
	}
	if got := journey.Starts(t, f, lo); len(got) != 0 {
		t.Fatalf("%s started %v behind a higher-priority workstream", lo, got)
	}
	resumeDone := movedAt(t, f, hi, "resume", execution.UnitChecking)
	if upload := movedAt(t, f, hi, "upload", intake.UnitImplementing); !upload.Before(resumeDone) {
		t.Fatalf("upload started at %s, after resume left implementing at %s", upload, resumeDone)
	}
	if dedupe := movedAt(t, f, hi, "dedupe", intake.UnitImplementing); !dedupe.After(resumeDone) {
		t.Fatalf("dedupe started at %s, before resume left implementing at %s", dedupe, resumeDone)
	}
	f.AwaitUnit(t, hi, "resume", intake.UnitReviewing)
	behind := api.UnitDispatch{Reason: execution.DeferPriority, Limit: 2, Workstreams: []config.WorkstreamID{hi}, Message: "Waits for a mason slot: all 2 are in use, and higher-priority workstreams start first: " + string(hi) + "."}
	f.CheckUnits(t, hi, []api.UnitStatus{{Unit: "resume", State: intake.UnitReviewing, Card: &intakefixture.ExampleCard}, {Unit: "dedupe", State: intake.UnitImplementing}, {Unit: "upload", State: intake.UnitImplementing}, deferred(f, t, hi, "audit", slotless(2))})
	var want []api.UnitStatus
	for _, unit := range units {
		want = append(want, deferred(f, t, lo, unit, behind))
	}
	f.CheckUnits(t, lo, want)
	if got, want := dispatches(f, t, hi, "dedupe"), []string{"deferred paused", "deferred entangled", "started"}; !slices.Equal(got, want) {
		t.Fatalf("decisions on dedupe of %s: %q, want %q", hi, got, want)
	}

	intakefixture.Mutation(t, f.C, "DELETE", "priority", api.ClearPriorityRequest{Project: f.Project})
	f.Stop(t)
	f.Start(t)

	f.AwaitMasonRan(t, lo, "resume")
	f.AwaitMasonRan(t, hi, "audit")
	intakefixture.AwaitPasses(t, passed, 3)
	intakefixture.Settled(t, func() int { mu.Lock(); defer mu.Unlock(); return dedupeTurns })
	masons.Check(t)
	mu.Lock()
	if dedupeTurns != 1 {
		t.Fatalf("the first turn of dedupe ran %d times", dedupeTurns)
	}
	mu.Unlock()
	th := f.Thread(t, hi, intake.MasonAgent("dedupe"))
	if len(th.Turns) != 2 || th.Turns[0].Request.TurnID != execution.MasonTurnID("dedupe") || th.Turns[0].Status() != "interrupted" || th.Turns[1].Request.TurnID != recovered || th.Turns[1].Status() != "idle" {
		t.Fatalf("dedupe's mason thread %+v", th.Turns)
	}
	workspace := filepath.Join(f.Opts.Config.Root, intake.UnitsDirectory, string(f.Project), string(hi), "dedupe")
	if data, err := os.ReadFile(filepath.Join(workspace, "internal", "trace", "dedupe.go")); err != nil || string(data) != "package trace\n" {
		t.Fatalf("dedupe's workspace lost the interrupted turn's file: %q %v", data, err)
	}
	if got, want := journey.Starts(t, f, hi), []string{trace.UnitSubject("resume"), trace.UnitSubject("upload"), trace.UnitSubject("dedupe"), trace.UnitSubject("audit")}; !slices.Equal(got, want) {
		t.Fatalf("%s started %v, want %v", hi, got, want)
	}
	if got, want := journey.Starts(t, f, lo), []string{trace.UnitSubject("resume")}; !slices.Equal(got, want) {
		t.Fatalf("%s started %v, want %v", lo, got, want)
	}
	order := []time.Time{movedAt(t, f, hi, "dedupe", execution.UnitChecking), movedAt(t, f, lo, "resume", intake.UnitImplementing), movedAt(t, f, lo, "resume", execution.UnitChecking), movedAt(t, f, hi, "audit", intake.UnitImplementing)}
	if !slices.IsSortedFunc(order, time.Time.Compare) {
		t.Fatalf("dedupe of %s finished, resume of %s started and finished, audit of %s started at %v", hi, lo, hi, order)
	}
	for stream, ran := range map[config.WorkstreamID][]string{
		hi: {execution.MasonTurnID("audit"), recovered, execution.MasonTurnID("resume"), execution.MasonTurnID("upload")},
		lo: {execution.MasonTurnID("resume")},
	} {
		var got []string
		for _, req := range masons.Requests(stream) {
			if req.Name != intake.MasonAgent("upload")+"-clarify-1" && req.Name != intake.MasonAgent("audit")+"-clarify-1" {
				got = append(got, req.Name)
			}
		}
		if slices.Sort(got); !slices.Equal(got, ran) {
			t.Fatalf("mason turns of %s ran %v, want %v", stream, got, ran)
		}
	}
	f.AwaitUnit(t, hi, "dedupe", intake.UnitReviewing)
	f.AwaitUnit(t, lo, "resume", intake.UnitReviewing)
	f.CheckUnits(t, hi, []api.UnitStatus{{Unit: "resume", State: intake.UnitReviewing, Card: &intakefixture.ExampleCard}, {Unit: "dedupe", State: intake.UnitReviewing, Card: &intakefixture.ExampleCard}, {Unit: "upload", State: intake.UnitImplementing}, {Unit: "audit", State: intake.UnitImplementing}})
	f.CheckUnits(t, lo, []api.UnitStatus{{Unit: "resume", State: intake.UnitReviewing, Card: &intakefixture.ExampleCard}, deferred(f, t, lo, "dedupe", slotless(2)), deferred(f, t, lo, "upload", slotless(2)), deferred(f, t, lo, "audit", slotless(2))})
	if got, want := dispatches(f, t, lo, "dedupe"), []string{"deferred paused", "deferred priority", "deferred capacity", "deferred entangled", "deferred capacity"}; !slices.Equal(got, want) {
		t.Fatalf("decisions on dedupe of %s: %q, want %q", lo, got, want)
	}

	mapping, err := kb.Load(f.Repository())
	fixturekit.Must(t, err)
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
		message := fmt.Sprintf("Workstream %s of this project builds in subsystem internal: both sealed footprints cover internal.audit, internal.trace, internal.upload (seal 1 of this workstream, seal 1 of that one). Its changes may conflict with this workstream's before both pull requests are open. This is an advisory and blocks neither workstream; the owner can pause or reprioritise either.", other)
		f.AwaitEventTurns(t, mine, message)
		st, err := f.C.Status(ctx, mine)
		fixturekit.Must(t, err)
		want := []api.OverlapAdvisory{{Workstream: other, Seal: 1, OtherSeal: 1, Subsystems: []string{"internal"}, Entities: []string{"internal.audit", "internal.trace", "internal.upload"}, Paths: paths, Message: message}}
		if st.State == nil || *st.State != intake.BuildingState || !reflect.DeepEqual(st.Advisories, want) {
			t.Fatalf("status of %s: state %v, advisories %+v, want %+v", mine, st.State, st.Advisories, want)
		}
	}
}

func movedAt(t *testing.T, f *journey.ShedFixture, stream config.WorkstreamID, unit, to string) time.Time {
	t.Helper()
	for _, tr := range intakefixture.AllTransitions(t, f.S.Cfg.Root, f.Project, stream) {
		if tr.Actor == intake.MasonActor && tr.Subject == trace.UnitSubject(unit) && tr.To == to {
			return tr.At
		}
	}
	t.Fatalf("unit %s of %s never moved to %s", unit, stream, to)
	return time.Time{}
}
