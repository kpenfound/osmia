package shed_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/kb"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
)

const (
	project config.ProjectID    = "p_00000000000000000000000000000001"
	stream  config.WorkstreamID = "w_00000000000000000000000000000001"
	alice                       = "agent_committee_1"
	bob                         = "agent_committee_2"

	specOne = "# Uploads\n\n## Acceptance criteria\n\n1. Uploads resume after a restart.\n2. Progress is reported.\n"
	// specTwo drops criterion 2 and planTwo renames the unit, so a citation
	// that resolves against one revision does not against the other.
	specTwo = "# Uploads\n\n## Acceptance criteria\n\n1. Uploads resume after a restart.\n"
	planOne = `{"version":1,"units":[{"id":"resume","addresses":[{"criterion":"spec#1","proof":{"kind":"new-test","name":"TestResume"}}],"depends_on":[],"footprint":["internal.store"]}]}`
	planTwo = `{"version":1,"units":[{"id":"restart","addresses":[{"criterion":"spec#1","proof":{"kind":"new-test","name":"TestResume"}}],"depends_on":[],"footprint":["internal.store"]}]}`
)

var (
	start = time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	owner = trace.Actor{Kind: "owner", ID: "local"}
	one   = shed.Pin{Spec: 1, Plan: 1}
	two   = shed.Pin{Spec: 2, Plan: 1}
)

func objection(round int, member string, k int) shed.Objection {
	return shed.Objection{ID: shed.ObjectionID(round, member, k), Kind: shed.Fit, Part: "spec#1", Argument: "It works against the store.", Citations: []string{"kb/store.md"}}
}

func record(round int, member string, pin shed.Pin, objections int, conceded ...string) shed.Record {
	r := shed.Record{Version: shed.Version, Round: round, Member: member, Revision: pin}
	for k := 1; k <= objections; k++ {
		r.Objections = append(r.Objections, objection(round, member, k))
	}
	for _, id := range conceded {
		r.Concessions = append(r.Concessions, shed.Concession{Objection: id, Reason: "The reply settles it."})
	}
	return r
}

func ids(dissent []shed.Dissent) []string {
	out := []string{}
	for _, d := range dissent {
		out = append(out, d.ID)
	}
	return out
}

func TestOpenDissent(t *testing.T) {
	a1, a2, b1 := shed.ObjectionID(1, alice, 1), shed.ObjectionID(1, alice, 2), shed.ObjectionID(1, bob, 1)
	first := []shed.Record{record(1, alice, one, 2), record(1, bob, one, 1)}
	failed := record(2, alice, two, 0)
	failed.Failure = "the agent exited"
	for _, tc := range []struct {
		name    string
		records []shed.Record
		want    []string
	}{
		{"nothing recorded", nil, []string{}},
		{"objections stand", first, []string{a1, a2, b1}},
		{"a concession withdraws exactly the objection it names", append(first[:2:2], record(2, alice, one, 0, a2)), []string{a1, b1}},
		{"a member concedes only its own objections", append(first[:2:2], record(2, bob, one, 0, a1)), []string{a1, a2, b1}},
		{"an objection conceded in the turn that made it", []shed.Record{record(1, alice, one, 2, a1)}, []string{a2}},
		{"a silent turn on the same revision is no new dissent and settles nothing", append(first[:2:2], record(2, alice, one, 0), record(2, bob, one, 0)), []string{a1, a2, b1}},
		{"a silent turn on a later revision accepts it and supersedes the member's own objections", append(first[:2:2], record(2, alice, two, 0)), []string{b1}},
		{"a later plan revision supersedes as a later spec revision does", append(first[:2:2], record(2, bob, shed.Pin{Spec: 1, Plan: 2}, 0)), []string{a1, a2}},
		{"a concession on a later revision accepts it", append(first[:2:2], record(2, alice, two, 0, a1)), []string{b1}},
		{"a new objection on the later revision accepts nothing", append(first[:2:2], record(2, alice, two, 1)), []string{a1, a2, b1, shed.ObjectionID(2, alice, 1)}},
		{"a failed turn on the later revision accepts nothing", append(first[:2:2], failed), []string{a1, a2, b1}},
		{"records are read in round order", []shed.Record{record(2, alice, two, 0), record(1, alice, one, 1)}, []string{}},
		{"an acceptance supersedes only what came before it", []shed.Record{record(1, alice, one, 1), record(2, alice, two, 0), record(3, alice, two, 1)}, []string{shed.ObjectionID(3, alice, 1)}},
	} {
		if got := ids(shed.OpenDissent(tc.records)); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: open dissent %v, want %v", tc.name, got, tc.want)
		}
	}
	got := shed.OpenDissent(first)[2]
	if got.Member != bob || got.Round != 1 || got.Revision != one || got.Kind != shed.Fit || got.Part != "spec#1" {
		t.Fatalf("dissent %+v", got)
	}
}

func TestPinAfter(t *testing.T) {
	for _, tc := range []struct {
		p, q shed.Pin
		want bool
	}{{one, one, false}, {two, one, true}, {one, two, false}, {shed.Pin{Spec: 1, Plan: 2}, one, true}, {shed.Pin{Spec: 2, Plan: 1}, shed.Pin{Spec: 1, Plan: 2}, false}} {
		if got := tc.p.After(tc.q); got != tc.want {
			t.Errorf("%+v after %+v: %v", tc.p, tc.q, got)
		}
	}
}

func TestRecordRoundTripsAndRefusesInvalidContent(t *testing.T) {
	r := record(1, alice, one, 1, shed.ObjectionID(1, alice, 1))
	r.Turn, r.Objections[0].Citations = "shed-1-agent_committee_1-1", []string{"charter#<1>"}
	data, err := shed.Encode(r)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"revision": {`) || !strings.Contains(string(data), `"charter#<1>"`) || !strings.HasSuffix(string(data), "}\n") {
		t.Fatalf("encoded:\n%s", data)
	}
	if got, err := shed.Parse(data); err != nil || !reflect.DeepEqual(got, r) {
		t.Fatalf("parsed %+v %v", got, err)
	}
	silent, err := shed.Encode(record(2, bob, two, 0))
	if err != nil || !strings.Contains(string(silent), `"objections": []`) || !strings.Contains(string(silent), `"concessions": []`) {
		t.Fatalf("silent record:\n%s %v", silent, err)
	}
	if parsed, err := shed.Parse(silent); err != nil || !parsed.Silent() || record(1, alice, one, 1).Silent() {
		t.Fatalf("silent: %+v %v", parsed, err)
	}
	failed := record(1, alice, one, 0)
	failed.Failure = "x"
	if failed.Silent() || record(1, alice, one, 0, "x").Silent() {
		t.Fatal("a failed or conceding turn is not silent")
	}
	for name, mutate := range map[string]func(*shed.Record){
		"version":           func(r *shed.Record) { r.Version = 2 },
		"round":             func(r *shed.Record) { r.Round = 0 },
		"member":            func(r *shed.Record) { r.Member = "../escape" },
		"spec revision":     func(r *shed.Record) { r.Revision.Spec = 0 },
		"plan revision":     func(r *shed.Record) { r.Revision.Plan = 0 },
		"objection id":      func(r *shed.Record) { r.Objections[0].ID = "other" },
		"objection kind":    func(r *shed.Record) { r.Objections[0].Kind = "taste" },
		"objection part":    func(r *shed.Record) { r.Objections[0].Part = " " },
		"argument":          func(r *shed.Record) { r.Objections[0].Argument = "" },
		"citations":         func(r *shed.Record) { r.Objections[0].Citations = nil },
		"conceded":          func(r *shed.Record) { r.Concessions[0].Objection = "" },
		"concession reason": func(r *shed.Record) { r.Concessions[0].Reason = "" },
	} {
		bad := record(1, alice, one, 1, shed.ObjectionID(1, alice, 1))
		mutate(&bad)
		if _, err := shed.Encode(bad); err == nil {
			t.Errorf("%s: encoded", name)
		}
		raw, err := json.Marshal(bad)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := shed.Parse(raw); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
	for _, raw := range []string{``, `[]`, string(data) + `{}`, strings.Replace(string(data), `"round"`, `"extra": 1, "round"`, 1)} {
		if _, err := shed.Parse([]byte(raw)); err == nil {
			t.Errorf("parsed %q", raw)
		}
	}
}

type fixture struct {
	repo *trace.Repository
	dir  string
}

// setup creates a trace with a ruled charter, one knowledge-base file, an
// entity map and two revisions of the workstream's spec and plan.
func setup(t *testing.T) fixture {
	t.Helper()
	ctx := context.Background()
	base := t.TempDir()
	root, err := config.ResolveRoot(filepath.Join(base, "osmia"), "")
	if err != nil {
		t.Fatal(err)
	}
	p := config.Project{ID: project, Clone: filepath.Join(base, "target")}
	if err := os.Mkdir(p.Clone, 0700); err != nil {
		t.Fatal(err)
	}
	repo, err := trace.Create(ctx, root, p, start, owner)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { repo.Close() })
	dir, err := root.ProjectTrace(project)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.CreateWorkstream(ctx, stream, start, owner); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "charter.md"), []byte("# Charter\n\n1. Every change has a test.\n2. Keep state in files.\n3. First of two.\n3. Second of two.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	f := fixture{repo: repo, dir: dir}
	f.documents(t, "", document("", "subsystem-store", "kb/store.md", "# Store\nState lives in files under the root.\n", 1))
	if err := kb.Store(ctx, repo, kb.Map{Version: 1, Entities: []kb.Entity{{ID: "internal.store", Name: "Store", Aliases: []string{"the store"}, Paths: []string{"internal/store/**"}}}}, start, owner, "seed"); err != nil {
		t.Fatal(err)
	}
	f.documents(t, stream, document(stream, plan.SpecDocument, plan.SpecPath, specOne, 1), document(stream, plan.PlanDocument, plan.PlanPath, planOne, 1))
	f.documents(t, stream, document(stream, plan.SpecDocument, plan.SpecPath, specTwo, 2), document(stream, plan.PlanDocument, plan.PlanPath, planTwo, 2))
	return f
}

func document(scope config.WorkstreamID, id, path, content string, revision int) trace.Document {
	return trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: id, Revision: revision, Project: project, Workstream: scope, At: start, Actor: owner, Cause: "test"}, Path: path, Content: content}
}

func (f fixture) documents(t *testing.T, _ config.WorkstreamID, docs ...trace.Document) {
	t.Helper()
	if err := f.repo.RecordDocuments(context.Background(), docs); err != nil {
		t.Fatal(err)
	}
}

// turn returns a member's turn pinned to revision 1 of both documents, with
// what Save was last given in saved.
func (f fixture) turn(t *testing.T, round int, member string, earlier []shed.Record, saved *shed.Record) shed.Turn {
	t.Helper()
	graph, err := plan.Parse([]byte(planOne))
	if err != nil {
		t.Fatal(err)
	}
	return shed.Turn{Repository: f.repo, Stream: stream, Spec: plan.ParseSpec(specOne), Plan: graph, Earlier: earlier, Now: func() time.Time { return start },
		Record: shed.Record{Round: round, Member: member, Revision: one, Turn: "turn-1"},
		Save:   func(r shed.Record) error { *saved = r; return nil }}
}

func tools(t *testing.T, turn shed.Turn) map[string]coreadapter.Tool {
	t.Helper()
	list, err := shed.Tools(turn)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]coreadapter.Tool{}
	for _, tool := range list {
		if tool.Effect != coreadapter.ToolMemory || tool.Handle == nil || !json.Valid(tool.InputSchema) {
			t.Fatalf("tool %s: %+v", tool.Name, tool)
		}
		out[tool.Name] = tool
	}
	if len(out) != 2 || out[shed.ObjectTool].Name == "" || out[shed.ConcedeTool].Name == "" {
		t.Fatalf("tools %v", out)
	}
	return out
}

func call(t *testing.T, tool coreadapter.Tool, input any) (recorded bool, text string) {
	t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	out, err := tool.Handle(context.Background(), raw)
	if err != nil {
		t.Fatalf("%s %s: %v", tool.Name, raw, err)
	}
	var result struct {
		Recorded  bool   `json:"recorded"`
		Reason    string `json:"reason"`
		Objection string `json:"objection"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatal(err)
	}
	if result.Recorded {
		return true, result.Objection
	}
	return false, result.Reason
}

type input struct {
	Kind      string   `json:"kind"`
	Part      string   `json:"part"`
	Argument  string   `json:"argument"`
	Citations []string `json:"citations"`
}

func TestObjectValidatesEveryCitationAndKind(t *testing.T) {
	f := setup(t)
	var saved shed.Record
	object := tools(t, f.turn(t, 1, alice, nil, &saved))[shed.ObjectTool]
	refused := []struct {
		name   string
		in     input
		reason string
	}{
		{"unknown kind", input{"taste", "spec#1", "x", []string{"charter#1"}}, `kind "taste" is not one of charter, fit, size or proof`},
		{"empty argument", input{"fit", "spec#1", " ", []string{"charter#1"}}, "an objection requires an argument"},
		{"no citation", input{"fit", "spec#1", "x", []string{}}, "an objection requires at least one citation"},
		{"charter rule that does not exist", input{"charter", "spec#1", "x", []string{"charter#9"}}, "the charter has no rule numbered 9 exactly once"},
		{"charter rule numbered twice", input{"charter", "spec#1", "x", []string{"charter#3"}}, "the charter has no rule numbered 3 exactly once"},
		{"charter citation that is no number", input{"charter", "spec#1", "x", []string{"charter#one"}}, `citation "charter#one" is not one of charter#<n>`},
		{"criterion that does not exist", input{"fit", "spec#1", "x", []string{"spec#3"}}, "spec.md revision 1 has no acceptance criterion numbered 3 exactly once"},
		{"criterion citation that is no number", input{"fit", "spec#1", "x", []string{"spec#0"}}, `citation "spec#0" is not one of`},
		{"unit that does not exist", input{"fit", "spec#1", "x", []string{"plan#upload"}}, "plan.json revision 1 has no unit upload exactly once"},
		{"unit of a later revision only", input{"fit", "spec#1", "x", []string{"plan#restart"}}, "plan.json revision 1 has no unit restart exactly once"},
		{"empty unit", input{"fit", "spec#1", "x", []string{"plan#"}}, "plan.json revision 1 has no unit  exactly once"},
		{"knowledge-base file that does not exist", input{"fit", "spec#1", "x", []string{"kb/queue.md"}}, "the knowledge base has no file kb/queue.md"},
		{"knowledge-base path that escapes", input{"fit", "spec#1", "x", []string{"kb/../charter.md"}}, `citation "kb/../charter.md" is not one of`},
		{"entity that does not exist", input{"fit", "spec#1", "x", []string{"kb/entities.json#internal.queue"}}, `the entity map has no entity "internal.queue"`},
		{"empty entity", input{"fit", "spec#1", "x", []string{"kb/entities.json#"}}, `the entity map has no entity ""`},
		{"entity map without an entity", input{"fit", "spec#1", "x", []string{"kb/entities.json"}}, `citation "kb/entities.json" is not one of`},
		{"ruling", input{"fit", "spec#1", "x", []string{"ruling#ruling_1"}}, `citation "ruling#ruling_1" is not one of`},
		{"free text", input{"fit", "spec#1", "x", []string{"the README"}}, `citation "the README" is not one of`},
		{"one invalid citation among valid ones", input{"fit", "spec#1", "x", []string{"charter#1", "spec#9"}}, "has no acceptance criterion numbered 9"},
		{"charter objection without a charter citation", input{"charter", "spec#1", "x", []string{"spec#1", "kb/store.md"}}, "a charter objection must cite the charter rule the part violates"},
		{"part that is free text", input{"fit", "the introduction", "x", []string{"charter#1"}}, `part "the introduction" is not one of spec#<n>`},
		{"part naming a missing criterion", input{"fit", "spec#7", "x", []string{"charter#1"}}, `part "spec#7" names nothing in the pinned revision`},
		{"part naming a missing unit", input{"size", "plan#upload", "x", []string{"charter#1"}}, `part "plan#upload" names nothing in the pinned revision`},
		{"size objection on a criterion", input{"size", "spec#1", "x", []string{"charter#1"}}, "a size objection concerns a unit"},
		{"size objection on the whole plan", input{"size", "plan", "x", []string{"charter#1"}}, "a size objection concerns a unit"},
		{"proof objection on a unit", input{"proof", "plan#resume", "x", []string{"charter#1"}}, "a proof objection concerns a criterion"},
	}
	for _, tc := range refused {
		recorded, reason := call(t, object, tc.in)
		if recorded || !strings.Contains(reason, tc.reason) {
			t.Errorf("%s: recorded %v, reason %q, want %q", tc.name, recorded, reason, tc.reason)
		}
	}
	if !reflect.DeepEqual(saved, shed.Record{}) {
		t.Fatalf("a refused objection was kept: %+v", saved)
	}
	for _, raw := range []string{`{"kind":"fit"`, `{"kind":"fit","part":"spec#1","argument":"x","citations":["charter#1"],"extra":1}`, `{"kind":"fit","part":"spec#1","argument":"x","citations":["charter#1"]} {}`} {
		if _, err := object.Handle(context.Background(), json.RawMessage(raw)); err == nil {
			t.Errorf("input %s accepted", raw)
		}
	}
	accepted := []input{
		{"charter", "spec#2", "Progress has no test.", []string{"charter#1", "spec#2"}},
		{"fit", "plan", "The plan ignores the store.", []string{"kb/store.md", "kb/entities.json#internal.store", "kb/entities.json#The Store"}},
		{"size", "plan#resume", "It addresses everything.", []string{"plan#resume"}},
		{"proof", "spec#2", "No unit shows it.", []string{"spec#2"}},
		{"fit", "spec", "It misses the design.", []string{"charter#2"}},
	}
	for i, in := range accepted {
		recorded, id := call(t, object, in)
		if want := shed.ObjectionID(1, alice, i+1); !recorded || id != want {
			t.Fatalf("objection %d: recorded %v %q, want %q", i+1, recorded, id, want)
		}
		if len(saved.Objections) != i+1 || !reflect.DeepEqual(saved.Objections[i], shed.Objection{ID: id, Kind: shed.Kind(in.Kind), Part: in.Part, Argument: in.Argument, Citations: in.Citations}) {
			t.Fatalf("saved %+v", saved)
		}
	}
	if saved.Version != shed.Version || saved.Round != 1 || saved.Member != alice || saved.Revision != one || saved.Turn != "turn-1" || len(saved.Concessions) != 0 {
		t.Fatalf("saved %+v", saved)
	}
	if _, err := shed.Encode(saved); err != nil {
		t.Fatalf("what the tools keep is not a valid record: %v", err)
	}
}

func TestConcedeSettlesOnlyTheMembersStandingObjections(t *testing.T) {
	f := setup(t)
	a1, a2, b1 := shed.ObjectionID(1, alice, 1), shed.ObjectionID(1, alice, 2), shed.ObjectionID(1, bob, 1)
	earlier := []shed.Record{record(1, alice, one, 2), record(1, bob, one, 1), record(2, alice, one, 0, a2)}
	var saved shed.Record
	both := tools(t, f.turn(t, 3, alice, earlier, &saved))
	concede, object := both[shed.ConcedeTool], both[shed.ObjectTool]
	type concession struct {
		Objection string `json:"objection"`
		Reason    string `json:"reason"`
	}
	for name, tc := range map[string]struct {
		in     concession
		reason string
	}{
		"unknown objection":         {concession{"agent_committee_1-r9-1", "x"}, "no objection agent_committee_1-r9-1 is recorded"},
		"another member's":          {concession{b1, "x"}, "objection " + b1 + " is agent_committee_2's; you may concede only your own"},
		"already conceded":          {concession{a2, "x"}, "objection " + a2 + " no longer stands"},
		"without a reason":          {concession{a1, " "}, "a concession requires a reason"},
		"an ID that is not a key":   {concession{"../escape", "x"}, "no objection ../escape is recorded"},
		"an objection never stated": {concession{"", "x"}, "no objection  is recorded"},
	} {
		if recorded, reason := call(t, concede, tc.in); recorded || reason != tc.reason {
			t.Errorf("%s: recorded %v, reason %q", name, recorded, reason)
		}
	}
	if !reflect.DeepEqual(saved, shed.Record{}) {
		t.Fatalf("a refused concession was kept: %+v", saved)
	}
	if recorded, id := call(t, concede, concession{a1, "The reply settles it."}); !recorded || id != a1 {
		t.Fatalf("concede: %v %q", recorded, id)
	}
	if recorded, reason := call(t, concede, concession{a1, "again"}); recorded || reason != "objection "+a1+" no longer stands" {
		t.Fatalf("conceded twice: %v %q", recorded, reason)
	}
	// An objection made in this turn can be withdrawn in it, once.
	recorded, made := call(t, object, input{"fit", "spec#1", "x", []string{"charter#1"}})
	if !recorded || made != shed.ObjectionID(3, alice, 1) {
		t.Fatalf("object: %v %q", recorded, made)
	}
	if recorded, _ := call(t, concede, concession{made, "I misread it."}); !recorded {
		t.Fatal("own objection of this turn not conceded")
	}
	if recorded, _ := call(t, concede, concession{made, "again"}); recorded {
		t.Fatal("conceded twice")
	}
	want := []shed.Concession{{Objection: a1, Reason: "The reply settles it."}, {Objection: made, Reason: "I misread it."}}
	if !reflect.DeepEqual(saved.Concessions, want) || len(saved.Objections) != 1 {
		t.Fatalf("saved %+v", saved)
	}
	if got := ids(shed.OpenDissent(append(earlier, saved))); !reflect.DeepEqual(got, []string{b1}) {
		t.Fatalf("open dissent %v", got)
	}
}

// A turn on a later revision has accepted nothing until it ends, so the
// member can still concede what it raised against the earlier one.
func TestConcedeOnALaterRevisionBeforeTheTurnEnds(t *testing.T) {
	f := setup(t)
	var saved shed.Record
	turn := f.turn(t, 2, alice, []shed.Record{record(1, alice, one, 1)}, &saved)
	turn.Record.Revision = two
	concede := tools(t, turn)[shed.ConcedeTool]
	if recorded, reason := call(t, concede, map[string]string{"objection": shed.ObjectionID(1, alice, 1), "reason": "Revision 2 drops the criterion."}); !recorded {
		t.Fatalf("refused: %s", reason)
	}
}

func TestToolsKeepNothingWhenSavingFails(t *testing.T) {
	f := setup(t)
	var saved shed.Record
	turn := f.turn(t, 1, alice, nil, &saved)
	failing := errors.New("disk full")
	fail := true
	turn.Save = func(r shed.Record) error {
		if fail {
			return failing
		}
		saved = r
		return nil
	}
	object := tools(t, turn)[shed.ObjectTool]
	raw := json.RawMessage(`{"kind":"fit","part":"spec#1","argument":"x","citations":["charter#1"]}`)
	if _, err := object.Handle(context.Background(), raw); !errors.Is(err, failing) {
		t.Fatalf("error %v", err)
	}
	fail = false
	if recorded, id := call(t, object, input{"fit", "spec#1", "x", []string{"charter#1"}}); !recorded || id != shed.ObjectionID(1, alice, 1) || len(saved.Objections) != 1 {
		t.Fatalf("the objection that was not kept took an ID: %q %+v", id, saved)
	}
}

func TestToolsRequireTheirDependenciesAndAValidTurn(t *testing.T) {
	f := setup(t)
	var saved shed.Record
	for name, mutate := range map[string]func(*shed.Turn){
		"trace":    func(turn *shed.Turn) { turn.Repository = nil },
		"clock":    func(turn *shed.Turn) { turn.Now = nil },
		"save":     func(turn *shed.Turn) { turn.Save = nil },
		"round":    func(turn *shed.Turn) { turn.Record.Round = 0 },
		"member":   func(turn *shed.Turn) { turn.Record.Member = "" },
		"revision": func(turn *shed.Turn) { turn.Record.Revision = shed.Pin{} },
	} {
		turn := f.turn(t, 1, alice, nil, &saved)
		mutate(&turn)
		if _, err := shed.Tools(turn); err == nil {
			t.Errorf("%s: tools built", name)
		}
	}
}

func TestRecordsReadsTheLatestRevisionOfEveryShedFile(t *testing.T) {
	f := setup(t)
	if got, err := shed.Records(f.repo, stream); err != nil || len(got) != 0 {
		t.Fatalf("records %+v %v", got, err)
	}
	write := func(r shed.Record, revision int) {
		t.Helper()
		data, err := shed.Encode(r)
		if err != nil {
			t.Fatal(err)
		}
		f.documents(t, stream, document(stream, shed.DocumentID(r.Round, r.Member), shed.Path(r.Round, r.Member), string(data), revision))
	}
	second, first, other := record(2, alice, two, 0, shed.ObjectionID(1, alice, 1)), record(1, alice, one, 1), record(1, bob, one, 0)
	write(second, 1)
	write(other, 1)
	write(record(1, alice, one, 2), 1)
	write(first, 2)
	got, err := shed.Records(f.repo, stream)
	if err != nil || !reflect.DeepEqual(got, []shed.Record{first, other, second}) {
		t.Fatalf("records %+v %v", got, err)
	}
	if path := shed.Path(12, alice); path != "shed/round-12/agent_committee_1.json" || shed.DocumentID(12, alice) != "shed-round-12-agent_committee_1" {
		t.Fatalf("path %s", path)
	}
	// A file that records another round than its path names is refused.
	misplaced, err := shed.Encode(record(4, bob, one, 0))
	if err != nil {
		t.Fatal(err)
	}
	f.documents(t, stream, document(stream, shed.DocumentID(3, bob), shed.Path(3, bob), string(misplaced), 1))
	if _, err := shed.Records(f.repo, stream); err == nil || !strings.Contains(err.Error(), "shed/round-3/agent_committee_2.json") {
		t.Fatalf("misplaced record: %v", err)
	}
	f.documents(t, stream, document(stream, shed.DocumentID(3, bob), shed.Path(3, bob), "{}\n", 2))
	if _, err := shed.Records(f.repo, stream); err == nil {
		t.Fatal("invalid record read")
	}
}
