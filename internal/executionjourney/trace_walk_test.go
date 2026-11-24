package executionjourney

import (
	context "context"
	json "encoding/json"
	errors "errors"
	fmt "fmt"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	os "os"
	exec "os/exec"
	filepath "path/filepath"
	reflect "reflect"
	slices "slices"
	strings "strings"
	testing "testing"
	time "time"

	config "github.com/kpenfound/osmia/internal/config"
	coreadapter "github.com/kpenfound/osmia/internal/coreadapter"
	plan "github.com/kpenfound/osmia/internal/plan"
	seal "github.com/kpenfound/osmia/internal/seal"
	shed "github.com/kpenfound/osmia/internal/shed"
	trace "github.com/kpenfound/osmia/internal/trace"
)

const walkSpec = "# Feature\n\n## Acceptance criteria\n\n1. Specs parse.\n2. Plans validate.\n3. Plans encode.\n"

type walkFixture struct {
	t    *testing.T
	repo *trace.Repository
	dir  string
	root config.Root
	at   time.Time
}

func sha(c byte) string { return strings.Repeat(string(c), 40) }

func newWalkFixture(t *testing.T) *walkFixture {
	t.Helper()
	base := t.TempDir()
	root, err := config.ResolveRoot(filepath.Join(base, "osmia"), "")
	fixturekit.Must(t, err)
	clone := filepath.Join(base, "target")
	cmd := exec.Command("git", "init", "--quiet", clone)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TEMPLATE_DIR="}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	f := &walkFixture{t: t, root: root, at: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)}
	f.repo, err = trace.Create(context.Background(), root, config.Project{ID: fixturekit.Project, Clone: clone}, f.now(), intake.OwnerActor)
	fixturekit.Must(t, err)
	t.Cleanup(func() { f.repo.Close() })
	f.dir, err = root.ProjectTrace(fixturekit.Project)
	fixturekit.Must(t, err)
	fixturekit.Must(t, f.repo.CreateWorkstream(context.Background(), fixturekit.Stream, f.now(), intake.OwnerActor))
	return f
}

func (f *walkFixture) now() time.Time {
	f.at = f.at.Add(time.Second)
	return f.at
}

func (f *walkFixture) header(schema, id, unit string, revision int, actor trace.Actor) trace.Header {
	return trace.Header{Schema: "osmia.trace." + schema, Version: trace.Version, ID: id, Revision: revision, Project: fixturekit.Project, Workstream: fixturekit.Stream, Unit: unit, At: f.now(), Actor: actor, Cause: "walk-test"}
}

func (f *walkFixture) doc(id, path, unit string, content any) trace.Document {
	f.t.Helper()
	text, ok := content.(string)
	if !ok {
		data, err := json.MarshalIndent(content, "", "  ")
		fixturekit.Must(f.t, err)
		text = string(data) + "\n"
	}
	docs, err := trace.Read[trace.Document](f.repo, fixturekit.Stream)
	fixturekit.Must(f.t, err)
	revision := 1
	for _, d := range docs {
		if d.ID == id {
			revision = d.Revision + 1
		}
	}
	d := trace.Document{Header: f.header("document", id, unit, revision, execution.ReviewerActor), Path: path, Content: text}
	fixturekit.Must(f.t, f.repo.RecordDocuments(context.Background(), []trace.Document{d}))
	return d
}

func (f *walkFixture) move(subject, unit, to string) {
	f.t.Helper()
	state, err := f.repo.Workflow(fixturekit.Stream, subject)
	fixturekit.Must(f.t, err)
	id := fmt.Sprintf("%s-%s-%d", subject, to, state.Version)
	_, err = f.repo.Transact(context.Background(), trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: f.header("transition", id, unit, 1, execution.ForemanActor), Subject: subject, From: state.Value, To: to, Reason: "walk test"}})
	fixturekit.Must(f.t, err)
}

func (f *walkFixture) unit(unit string, states ...string) {
	for _, s := range states {
		f.move(trace.UnitSubject(unit), unit, s)
	}
}

func (f *walkFixture) append(r trace.Record) {
	f.t.Helper()
	fixturekit.Must(f.t, f.repo.Append(context.Background(), r))
}

func (f *walkFixture) sealed() {
	f.doc(plan.SpecDocument, plan.SpecPath, "", walkSpec)
	graph := plan.Plan{Version: plan.Version, Units: []plan.Unit{
		{ID: "a", Title: "Parse specs", Task: "Parse the spec's criteria.", Acceptance: []string{"TestParse covers numbered criteria"}, Criteria: []string{"spec#1"}, DependsOn: []string{}, Footprint: []string{"spec"}},
		{ID: "b", Title: "Validate plans", Task: "Validate and encode plans.", Acceptance: []string{"TestValidate passes", "TestEncode covers the canonical form"}, Criteria: []string{"spec#2", "spec#3"}, DependsOn: []string{"a"}, Footprint: []string{"plan"}},
	}}
	encoded, err := plan.Encode(graph)
	fixturekit.Must(f.t, err)
	f.doc(plan.PlanDocument, plan.PlanPath, "", string(encoded))
	ratification, err := shed.EncodeRatification(shed.Ratify(1, shed.Pin{Spec: 1, Plan: 1}, nil))
	fixturekit.Must(f.t, err)
	f.doc(shed.RatificationDocumentID(1), shed.RatificationPath(1), "", string(ratification))
	s, err := seal.Encode(seal.Seal{Version: seal.Version, Seal: 1, Round: 1, Revision: shed.Pin{Spec: 1, Plan: 1}, SpecHash: seal.SpecHash(walkSpec), Base: seal.Base{Remote: "upstream", Branch: "main", Commit: sha('0')}, Branch: "osmia/feature", Workspace: "/branches/feature"})
	fixturekit.Must(f.t, err)
	f.doc(seal.DocumentID, seal.Path, "", string(s))
	f.move(trace.FeatureSubject, "", intake.RatifiedState)
	f.move(trace.FeatureSubject, "", intake.BuildingState)
	f.unit("a", intake.UnitPlanned, intake.UnitReady)
	f.unit("b", intake.UnitPlanned)
}

func (f *walkFixture) report(unit, candidate, base string) {
	r := execution.UnitReport{Unit: unit, Turn: "mason-" + unit + "-1", Seal: 1, Outcome: "done", Branch: "osmia/unit-" + unit, Base: base, Candidate: candidate}
	f.doc(execution.ReportDocument(unit), fmt.Sprintf(execution.ReportPath, unit), unit, r)
}

func (f *walkFixture) review(unit string, report int, candidate, base, decision string, bounces int) {
	identity := service.UnitReviewIdentity{Subject: string(fixturekit.Stream) + "/" + unit, Candidate: coreadapter.Candidate{Revision: candidate, BaseRevision: base, SpecRevision: "1", PlanRevision: "1"}, DiffSHA256: "digest-" + candidate[:4], Report: fmt.Sprintf("units/%s/report.json revision %d", unit, report), Seal: 1}
	path := fmt.Sprintf("units/%s/review.json", unit)
	f.doc(execution.ReviewDocument(unit), path, unit, identity)
	verdict := service.UnitVerdict{Decision: decision, Summary: "reviewed " + candidate, Findings: []api.ReviewFinding{}}
	if decision == "material_findings" {
		verdict.Findings = append(verdict.Findings, api.ReviewFinding{Severity: "major", Evidence: "missing case", Action: "add it"})
	}
	f.doc(execution.ReviewDocument(unit), path, unit, service.UnitReviewResult{Identity: identity, Turn: fmt.Sprintf("review-%s-%d", unit, report), Verdict: verdict, Bounces: bounces})
}

func (f *walkFixture) land(unit string, review int, candidate, base, commit string, criteria ...string) {
	f.doc(execution.LandingDocument(unit), fmt.Sprintf("units/%s/landing.json", unit), unit, api.UnitLanding{Unit: unit, Operation: "op-land-" + unit, Approval: fmt.Sprintf("units/%s/review.json revision %d", unit, review), Turn: "review", Candidate: candidate, Base: base,
		Spec: "1", Plan: "1", Seal: 1, Criteria: criteria, Branch: "osmia/feature", Commit: commit, Message: "Land " + unit + "\n\nOsmia-Operation: op-land-" + unit + "\n"})
}

func (f *walkFixture) buildA() {
	mason := trace.Actor{Kind: "agent", ID: "mason-a"}
	f.append(trace.Agent{Header: f.header("agent", "mason-a", "a", 1, mason), Role: "mason", ThreadID: "mason-a"})
	request := trace.TurnRequest{Header: f.header("turn-request", "request_mason-a-1", "a", 1, mason), AgentID: "mason-a", ThreadID: "mason-a", TurnID: "mason-a-1", Profile: coreadapter.Profile{Name: "default", Backend: "fake", Model: "fake"}, Prompt: "build a"}
	f.append(request)
	f.unit("a", intake.UnitImplementing)
	f.append(trace.Question{Header: f.header("question", "q1", "a", 1, mason), AskedBy: mason, Thread: "mason-a", Turn: "mason-a-1", Question: "Which parser?"})
	f.append(trace.Ruling{Header: f.header("ruling", "q1-ruling", "a", 1, intake.OwnerActor), QuestionID: "q1", QuestionRevision: 1, Decision: trace.DecisionRuling, OwnerResponse: "The strict one.", Citations: []string{"spec#1"}})
	usage := coreadapter.Usage{CostUSD: 0.25, CostKnown: true, Turns: 1}
	f.append(trace.TurnResponse{Header: f.header("turn-response", "response_mason-a-1", "a", 1, mason), AgentID: "mason-a", ThreadID: "mason-a", TurnID: "mason-a-1", RequestID: request.ID, RequestRevision: 1, Result: coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "fake", ID: "s1"}, StartedAt: f.at, Usage: usage}})
	f.append(trace.Cost{Header: f.header("cost", "cost-mason-a-1", "a", 1, intake.OwnerActor), Entry: coreadapter.LedgerEntry{Scope: coreadapter.Scope{Project: string(fixturekit.Project), Workstream: string(fixturekit.Stream), Unit: "a", Thread: "mason-a", Turn: "mason-a-1", Role: "mason"}, AttemptID: "attempt-1", At: f.at, Usage: usage}})
	f.report("a", sha('1'), sha('0'))
	f.unit("a", intake.UnitReviewing)
	f.review("a", 1, sha('1'), sha('0'), "material_findings", 1)
	f.unit("a", intake.UnitImplementing)
	f.report("a", sha('2'), sha('0'))
	f.unit("a", intake.UnitReviewing)
	f.review("a", 2, sha('2'), sha('0'), "satisfactory", 1)
	f.unit("a", intake.UnitApproved)
	f.land("a", 4, sha('2'), sha('0'), sha('a'), "spec#1")
	f.unit("a", intake.UnitMerged)
	f.unit("b", intake.UnitReady)
}

func (f *walkFixture) buildB() {
	f.unit("b", intake.UnitImplementing)
	f.report("b", sha('3'), sha('a'))
	f.unit("b", intake.UnitReviewing)
	f.review("b", 1, sha('3'), sha('a'), "satisfactory", 0)
	f.unit("b", intake.UnitApproved)
	f.land("b", 2, sha('3'), sha('a'), sha('b'), "spec#2", "spec#3")
	f.unit("b", intake.UnitMerged)
}

func (f *walkFixture) deliver() {
	f.move(trace.FeatureSubject, "", intake.AssembledState)
	upstream := seal.Base{Remote: "upstream", Branch: "main", Commit: sha('9')}
	f.doc(delivery.FinalReportDocument, delivery.FinalReportPath, "", api.FinalReport{Review: 1, Operation: "op-final", Outcome: delivery.FinalReviewed, Branch: "osmia/feature", Before: sha('b'), Commit: sha('f'), Upstream: &upstream, Seal: 1, SpecHash: seal.SpecHash(walkSpec), Spec: 1, Plan: 1, Charter: 1, Turn: "final-1-member-1",
		Criteria: []api.FinalCriterion{{Criterion: "spec#1", Text: "Specs parse.", Evidence: "TestParse"}, {Criterion: "spec#2", Text: "Plans validate.", Evidence: "TestValidate"}, {Criterion: "spec#3", Text: "Plans encode.", Evidence: "TestEncode"}}})
	approval := f.doc(delivery.DeliveryDocument, delivery.DeliveryPath, "", api.DeliveryApproval{Review: 1, ReviewRevision: 1, Commit: sha('f'), Seal: 1, Spec: 1, Plan: 1, Charter: 1, Description: "Deliver", DescriptionHash: delivery.DescriptionHash("Deliver"), At: f.at})
	f.doc(delivery.PublicationDocument, delivery.PublicationPath, "", api.DeliveryPublication{Approval: approval.Revision, Operation: "op-publish", Status: delivery.PublicationOpened, Style: delivery.SquashStyle, Fork: "owner/project", Remote: "origin", Branch: "osmia/feature", Reviewed: sha('f'), Commit: sha('d'), Upstream: "upstream/project", Base: "main", PullRequest: 7, URL: "https://example.test/pull/7", Title: "Deliver", Description: "Deliver", DescriptionHash: delivery.DescriptionHash("Deliver")})
	f.move(trace.FeatureSubject, "", intake.DeliveredState)
}

func (f *walkFixture) snapshot() map[string]int {
	f.t.Helper()
	streamDir, err := f.root.WorkstreamTrace(fixturekit.Project, fixturekit.Stream)
	fixturekit.Must(f.t, err)
	return map[string]int{f.dir: fixturekit.TraceCommits(f.t, f.dir), streamDir: fixturekit.TraceCommits(f.t, streamDir)}
}

func gapStates(gaps []api.TraceGap) map[string]string {
	out := map[string]string{}
	for _, g := range gaps {
		out[g.Link] = g.State
	}
	return out
}

func TestTraceWalkCompleteChain(t *testing.T) {
	t.Parallel()
	f := newWalkFixture(t)
	f.sealed()
	f.buildA()
	f.buildB()
	f.deliver()
	before := f.snapshot()

	c, err := service.WalkCriterion(f.repo, fixturekit.Stream, "spec#1")
	fixturekit.Must(t, err)
	if !c.Complete || len(c.Gaps) != 0 {
		t.Fatalf("criterion gaps %+v", c.Gaps)
	}
	if c.Text != "Specs parse." || c.Spec.Revision != 1 || c.Plan.Revision != 1 || c.Seal.Seal != 1 || c.Feature != intake.DeliveredState {
		t.Fatalf("criterion %+v", c)
	}

	if len(c.Rulings) != 1 || c.Rulings[0].Kind != service.RulingRatification || c.Rulings[0].Ref.Path != shed.RatificationPath(1) {
		t.Fatalf("criterion rulings %+v", c.Rulings)
	}
	if len(c.Units) != 1 || c.Units[0].Unit != "a" {
		t.Fatalf("criterion units %+v", c.Units)
	}
	a := c.Units[0]
	if len(a.Criteria) != 1 || a.Criteria[0].Text != "Specs parse." || a.Task != "Parse the spec's criteria." || len(a.Acceptance) != 1 || a.Acceptance[0] != "TestParse covers numbered criteria" {
		t.Fatalf("unit a task %q acceptance %v criteria %+v", a.Task, a.Acceptance, a.Criteria)
	}
	if len(a.Reports) != 2 || a.Reports[0].Candidate != sha('1') || a.Reports[1].Candidate != sha('2') {
		t.Fatalf("unit a reports %+v", a.Reports)
	}
	if len(a.Reviews) != 2 || a.Reviews[0].Decision != "material_findings" || a.Reviews[0].Ref.Revision != 2 || a.Reviews[1].Decision != "satisfactory" || a.Reviews[1].Ref.Revision != 4 || a.Reviews[1].Report != "units/a/report.json revision 2" {
		t.Fatalf("unit a reviews %+v", a.Reviews)
	}
	if len(a.Landings) != 1 || a.Landings[0].Commit != sha('a') {
		t.Fatalf("unit a landings %+v", a.Landings)
	}
	if len(a.Rulings) != 1 || a.Rulings[0].Owner != "The strict one." || a.Rulings[0].Question == nil || a.Rulings[0].Question.ID != "q1" {
		t.Fatalf("unit a rulings %+v", a.Rulings)
	}
	if len(a.Turns) != 1 || a.Turns[0].Status != "idle" || a.Turns[0].Role != "mason" || a.Turns[0].CostUSD != 0.25 || a.CostUSD != 0.25 {
		t.Fatalf("unit a turns %+v cost %v", a.Turns, a.CostUSD)
	}

	two, err := service.WalkCriterion(f.repo, fixturekit.Stream, "spec#2")
	fixturekit.Must(t, err)
	b := two.Units[0]
	if len(b.Criteria) != 1 || b.Criteria[0].Criterion != "spec#2" || len(b.Acceptance) != 2 || len(b.Reports) != 1 || len(b.Reviews) != 1 || b.Reviews[0].Summary != "reviewed "+sha('3') {
		t.Fatalf("spec#2 walk of unit b %+v", b)
	}
	if c.Final == nil || c.Final.Evidence != "TestParse" {
		t.Fatalf("final account %+v", c.Final)
	}
	d := c.Delivery
	if d == nil || d.Report.Revision != 1 || d.Approval.Revision != 1 || d.Reviewed != sha('f') || d.Published != sha('d') || d.PullRequest != 7 {
		t.Fatalf("delivery %+v", d)
	}

	u, err := service.TraceUnit(f.repo, fixturekit.Stream, "a")
	fixturekit.Must(t, err)
	if !u.Complete || u.State != intake.UnitMerged || u.Source != service.UnitFromPlan || u.Definition.ID != plan.PlanDocument {
		t.Fatalf("unit %+v", u)
	}
	if !slices.IsSortedFunc(u.History, func(x, y api.TraceEvent) int { return x.At.Compare(y.At) }) {
		t.Fatal("unit history is not in time order")
	}
	var reviews []int
	var states []string
	for _, e := range u.History {
		if e.Ref.Kind == "document" && e.Ref.ID == execution.ReviewDocument("a") {
			reviews = append(reviews, e.Ref.Revision)
		}
		if _, moved, ok := strings.Cut(e.Summary, trace.UnitSubject("a")+": "); ok && e.Ref.Kind == "transition" {
			_, to, _ := strings.Cut(moved, " -> ")
			to, _, _ = strings.Cut(to, ":")
			states = append(states, to)
		}
	}
	if !slices.Equal(reviews, []int{1, 2, 3, 4}) {
		t.Fatalf("review revisions in history %v", reviews)
	}
	if !slices.Equal(states, []string{intake.UnitPlanned, intake.UnitReady, intake.UnitImplementing, intake.UnitReviewing, intake.UnitImplementing, intake.UnitReviewing, intake.UnitApproved, intake.UnitMerged}) {
		t.Fatalf("unit states in history %v", states)
	}

	landing, err := service.TraceCommit(f.repo, fixturekit.Stream, sha('a'), "", "")
	fixturekit.Must(t, err)
	if !landing.Complete || len(landing.Landings) != 1 {
		t.Fatalf("landing commit %+v", landing)
	}
	l := landing.Landings[0]
	if l.Unit != "a" || l.Review.Ref.Revision != 4 || l.Report.Ref.Revision != 2 || l.Report.Candidate != sha('2') || l.Spec.Revision != 1 || l.Plan.Revision != 1 || l.Seal.Seal != 1 || !reflect.DeepEqual(l.Criteria, []api.TraceCriterion{{Criterion: "spec#1", Text: "Specs parse."}}) || len(l.Rulings) != 1 {
		t.Fatalf("landing %+v", l)
	}

	published, err := service.TraceCommit(f.repo, fixturekit.Stream, sha('d'), "", "")
	fixturekit.Must(t, err)
	var units []string
	for _, l := range published.Landings {
		units = append(units, l.Unit)
	}
	if !published.Complete || published.Delivery == nil || published.Delivery.Publication == nil || !slices.Equal(units, []string{"a", "b"}) {
		t.Fatalf("published commit %+v", published)
	}

	rebased, err := service.TraceCommit(f.repo, fixturekit.Stream, sha('e'), "Land a\n\nOsmia-Operation: op-land-a\n", "")
	fixturekit.Must(t, err)
	if rebased.Operation != "op-land-a" || len(rebased.Landings) != 1 || rebased.Landings[0].Unit != "a" {
		t.Fatalf("rebased commit %+v", rebased)
	}

	approved, err := service.TraceCommit(f.repo, fixturekit.Stream, sha('2'), "", "")
	fixturekit.Must(t, err)
	if !approved.Complete || len(approved.Landings) != 1 || approved.Landings[0].Landing.Commit != sha('a') {
		t.Fatalf("approved candidate %+v", approved)
	}

	sentBack, err := service.TraceCommit(f.repo, fixturekit.Stream, sha('1'), "", "")
	fixturekit.Must(t, err)
	if !sentBack.Complete || len(sentBack.Landings) != 0 || len(sentBack.Records) != 2 || sentBack.Records[0].Role != service.CommitCandidate || sentBack.Records[0].Unit != "a" {
		t.Fatalf("sent-back candidate %+v", sentBack)
	}

	again, err := service.WalkCriterion(f.repo, fixturekit.Stream, "spec#1")
	fixturekit.Must(t, err)
	first, _ := json.Marshal(c)
	second, _ := json.Marshal(again)
	if string(first) != string(second) {
		t.Fatalf("walks differ:\n%s\n%s", first, second)
	}
	if !reflect.DeepEqual(before, f.snapshot()) {
		t.Fatal("a trace walk changed the trace")
	}

	f.doc(delivery.DeliveryDocument, delivery.DeliveryPath, "", api.DeliveryApproval{Review: 1, ReviewRevision: 1, Commit: sha('f'), Seal: 1, Spec: 1, Plan: 1, Charter: 1, Description: "Edited", DescriptionHash: delivery.DescriptionHash("Edited"), At: f.at})
	c, err = service.WalkCriterion(f.repo, fixturekit.Stream, "spec#1")
	fixturekit.Must(t, err)
	if c.Delivery.Approval.Revision != 1 {
		t.Fatalf("delivery follows approval revision %d", c.Delivery.Approval.Revision)
	}
}

func TestTraceWalkIncompleteChain(t *testing.T) {
	t.Parallel()
	f := newWalkFixture(t)
	f.sealed()
	f.buildA()
	f.unit("b", intake.UnitImplementing)

	c, err := service.WalkCriterion(f.repo, fixturekit.Stream, "spec#2")
	fixturekit.Must(t, err)
	if c.Complete || c.Delivery != nil || c.Final != nil {
		t.Fatalf("criterion %+v", c)
	}
	if got := gapStates(c.Gaps); !reflect.DeepEqual(got, map[string]string{delivery.FinalReportPath: service.LinkNotCreated, delivery.DeliveryPath: service.LinkNotCreated, delivery.PublicationPath: service.LinkNotCreated}) {
		t.Fatalf("criterion gaps %v", got)
	}
	b := c.Units[0]
	if b.Complete || !reflect.DeepEqual(gapStates(b.Gaps), map[string]string{"units/b/report.json": service.LinkUnfinished}) {
		t.Fatalf("unit b gaps %+v", b.Gaps)
	}

	f.unit("b", intake.UnitWaiting)
	u, err := service.TraceUnit(f.repo, fixturekit.Stream, "b")
	fixturekit.Must(t, err)
	if !reflect.DeepEqual(gapStates(u.Gaps), map[string]string{"units/b/report.json": service.LinkUnfinished}) {
		t.Fatalf("waiting unit gaps %+v", u.Gaps)
	}
	f.unit("b", intake.UnitImplementing)

	f.report("b", sha('3'), sha('a'))
	f.unit("b", intake.UnitReviewing)
	u, err = service.TraceUnit(f.repo, fixturekit.Stream, "b")
	fixturekit.Must(t, err)
	if !reflect.DeepEqual(gapStates(u.Gaps), map[string]string{"verdict on units/b/report.json revision 1": service.LinkUnfinished}) {
		t.Fatalf("reviewing gaps %+v", u.Gaps)
	}
	f.review("b", 1, sha('3'), sha('a'), "satisfactory", 0)
	f.unit("b", intake.UnitApproved)
	u, err = service.TraceUnit(f.repo, fixturekit.Stream, "b")
	fixturekit.Must(t, err)
	if !reflect.DeepEqual(gapStates(u.Gaps), map[string]string{"landing of units/b/review.json revision 2": service.LinkUnfinished}) {
		t.Fatalf("approved gaps %+v", u.Gaps)
	}

	candidate, err := service.TraceCommit(f.repo, fixturekit.Stream, sha('3'), "", "")
	fixturekit.Must(t, err)
	if candidate.Complete || len(candidate.Landings) != 0 || gapStates(candidate.Gaps)["landing of candidate "+sha('3')] != service.LinkUnfinished {
		t.Fatalf("candidate %+v", candidate)
	}

	f.land("b", 9, sha('3'), sha('a'), sha('b'), "spec#2", "spec#3")
	f.unit("b", intake.UnitMerged)
	u, err = service.TraceUnit(f.repo, fixturekit.Stream, "b")
	fixturekit.Must(t, err)
	if got := gapStates(u.Gaps); got["units/b/review.json revision 9"] != service.LinkUnavailable || got["landing of units/b/review.json revision 2"] != service.LinkUnavailable {
		t.Fatalf("merged gaps %v", got)
	}
	landed, err := service.TraceCommit(f.repo, fixturekit.Stream, sha('b'), "", "")
	fixturekit.Must(t, err)
	if landed.Complete || landed.Landings[0].Review != nil || gapStates(landed.Gaps)["units/b/review.json revision 9"] != service.LinkUnavailable {
		t.Fatalf("landed commit %+v", landed)
	}

	for _, err := range []error{
		func() error {
			_, err := service.TraceCommit(f.repo, fixturekit.Stream, sha('7'), "", "")
			return err
		}(),
		func() error { _, err := service.WalkCriterion(f.repo, fixturekit.Stream, "spec#4"); return err }(),
		func() error { _, err := service.TraceUnit(f.repo, fixturekit.Stream, "z"); return err }(),
	} {
		if !errors.Is(err, service.ErrTraceNotFound) {
			t.Fatalf("unknown walk: %v", err)
		}
	}
}

func TestTraceWalkKeepsRevisions(t *testing.T) {
	t.Parallel()
	f := newWalkFixture(t)
	f.sealed()
	f.buildA()
	revised := strings.Replace(walkSpec, "Specs parse.", "Specs parse strictly.", 1)
	f.doc(plan.SpecDocument, plan.SpecPath, "", revised)

	c, err := service.WalkCriterion(f.repo, fixturekit.Stream, "spec#1")
	fixturekit.Must(t, err)
	if c.Text != "Specs parse." || c.Spec.Revision != 1 || c.Units[0].Criteria[0].Text != "Specs parse." {
		t.Fatalf("criterion after an unsealed revision %+v", c)
	}

	ratification, err := shed.EncodeRatification(shed.Ratify(2, shed.Pin{Spec: 2, Plan: 1}, nil))
	fixturekit.Must(t, err)
	f.doc(shed.RatificationDocumentID(2), shed.RatificationPath(2), "", string(ratification))
	s, err := seal.Encode(seal.Seal{Version: seal.Version, Seal: 2, Round: 2, Revision: shed.Pin{Spec: 2, Plan: 1}, SpecHash: seal.SpecHash(revised), Base: seal.Base{Remote: "upstream", Branch: "main", Commit: sha('0')}, Branch: "osmia/feature", Workspace: "/branches/feature"})
	fixturekit.Must(t, err)
	f.doc(seal.DocumentID, seal.Path, "", string(s))

	c, err = service.WalkCriterion(f.repo, fixturekit.Stream, "spec#1")
	fixturekit.Must(t, err)
	if c.Text != "Specs parse strictly." || c.Spec.Revision != 2 || c.Seal.Seal != 2 || c.Rulings[0].Ref.Path != shed.RatificationPath(2) {
		t.Fatalf("criterion after resealing %+v", c)
	}
	a := c.Units[0]
	if a.Reports[1].Seal != 1 || a.Reviews[1].Candidate.SpecRevision != "1" || a.Landings[0].Spec != "1" {
		t.Fatalf("unit evidence lost its recorded revisions %+v", a)
	}
	landing, err := service.TraceCommit(f.repo, fixturekit.Stream, sha('a'), "", "")
	fixturekit.Must(t, err)
	l := landing.Landings[0]
	if l.Spec.Revision != 1 || l.Seal.Seal != 1 || l.Criteria[0].Text != "Specs parse." {
		t.Fatalf("landing resolved a newer revision %+v", l)
	}
}

func TestTraceWalkAbandoned(t *testing.T) {
	t.Parallel()
	f := newWalkFixture(t)
	f.sealed()
	f.buildA()
	f.unit("b", intake.UnitImplementing)
	f.move(trace.FeatureSubject, "", intake.AbandonedState)

	c, err := service.WalkCriterion(f.repo, fixturekit.Stream, "spec#2")
	fixturekit.Must(t, err)
	if c.Feature != intake.AbandonedState || c.Complete {
		t.Fatalf("criterion %+v", c)
	}
	if got := gapStates(c.Units[0].Gaps); !reflect.DeepEqual(got, map[string]string{"units/b/report.json": service.LinkNotCreated}) {
		t.Fatalf("abandoned unit gaps %v", got)
	}
	for link, state := range gapStates(c.Gaps) {
		if state != service.LinkNotCreated {
			t.Fatalf("abandoned gap %s is %s", link, state)
		}
	}
	a, err := service.TraceUnit(f.repo, fixturekit.Stream, "a")
	fixturekit.Must(t, err)
	if !a.Complete || a.State != intake.UnitMerged {
		t.Fatalf("merged unit of an abandoned workstream %+v", a)
	}
	if _, err := service.TraceCommit(f.repo, fixturekit.Stream, sha('a'), "", ""); err != nil {
		t.Fatal(err)
	}
}

func TestTraceWalkBeforeSeal(t *testing.T) {
	t.Parallel()
	f := newWalkFixture(t)
	f.doc(plan.SpecDocument, plan.SpecPath, "", walkSpec)
	f.move(trace.FeatureSubject, "", intake.InShedState)
	c, err := service.WalkCriterion(f.repo, fixturekit.Stream, "spec#1")
	fixturekit.Must(t, err)
	if c.Complete || c.Text != "" || c.Spec != nil || !reflect.DeepEqual(gapStates(c.Gaps), map[string]string{seal.Path: service.LinkNotCreated}) {
		t.Fatalf("criterion before the seal %+v", c)
	}
	f.move(trace.FeatureSubject, "", intake.RatifiedState)
	c, err = service.WalkCriterion(f.repo, fixturekit.Stream, "spec#1")
	fixturekit.Must(t, err)
	if gapStates(c.Gaps)[seal.Path] != service.LinkUnfinished {
		t.Fatalf("criterion while sealing %+v", c.Gaps)
	}
	if _, err := service.WalkCriterion(f.repo, fixturekit.Stream, "1"); err == nil || errors.Is(err, service.ErrTraceNotFound) {
		t.Fatalf("uncited criterion: %v", err)
	}
}

func TestTraceWalkFindsAUnitsCommitByItsChange(t *testing.T) {
	t.Parallel()
	f := newWalkFixture(t)
	change := strings.Repeat("k", 32)
	f.doc(execution.ChangeDocument("a"), "units/a/change.json", "a", execution.UnitChange{Unit: "a", Branch: execution.UnitBranch(fixturekit.Stream, "a"), Change: change})
	f.doc(execution.ChangeDocument("b"), "units/b/change.json", "b", execution.UnitChange{Unit: "b", Branch: execution.UnitBranch(fixturekit.Stream, "b"), Change: strings.Repeat("z", 32)})
	walk, err := service.TraceCommit(f.repo, fixturekit.Stream, sha('9'), "", change)
	fixturekit.Must(t, err)
	if len(walk.Records) != 1 || walk.Records[0].Role != service.CommitChange || walk.Records[0].Unit != "a" || walk.Records[0].Ref.Path != "units/a/change.json" || walk.Records[0].Ref.Revision != 1 {
		t.Fatalf("the walk by change %+v", walk.Records)
	}
	for _, other := range []string{"", strings.Repeat("l", 32)} {
		if _, err := service.TraceCommit(f.repo, fixturekit.Stream, sha('9'), "", other); !errors.Is(err, service.ErrTraceNotFound) {
			t.Fatalf("a commit carrying change %q: %v", other, err)
		}
	}
}
