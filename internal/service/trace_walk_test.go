package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
)

const walkSpec = "# Feature\n\n## Acceptance criteria\n\n1. Specs parse.\n2. Plans validate.\n"

// walkFixture records a workstream's trace directly in a temporary trace
// repository, one record at a time on a clock that advances per record.
type walkFixture struct {
	t    *testing.T
	repo *trace.Repository
	dir  string
	at   time.Time
}

func sha(c byte) string { return strings.Repeat(string(c), 40) }

func newWalkFixture(t *testing.T) *walkFixture {
	t.Helper()
	base := t.TempDir()
	root, err := config.ResolveRoot(filepath.Join(base, "osmia"), "")
	must(t, err)
	clone := filepath.Join(base, "target")
	cmd := exec.Command("git", "init", "--quiet", clone)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TEMPLATE_DIR="}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	f := &walkFixture{t: t, at: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)}
	f.repo, err = trace.Create(context.Background(), root, config.Project{ID: project, Clone: clone}, f.now(), ownerActor)
	must(t, err)
	t.Cleanup(func() { f.repo.Close() })
	f.dir, err = root.ProjectTrace(project)
	must(t, err)
	must(t, f.repo.CreateWorkstream(context.Background(), stream, f.now(), ownerActor))
	return f
}

func (f *walkFixture) now() time.Time {
	f.at = f.at.Add(time.Second)
	return f.at
}

func (f *walkFixture) header(schema, id, unit string, revision int, actor trace.Actor) trace.Header {
	return trace.Header{Schema: "osmia.trace." + schema, Version: trace.Version, ID: id, Revision: revision, Project: project, Workstream: stream, Unit: unit, At: f.now(), Actor: actor, Cause: "walk-test"}
}

// doc records the next revision of a document; content that is not a string
// is recorded as indented JSON.
func (f *walkFixture) doc(id, path, unit string, content any) trace.Document {
	f.t.Helper()
	text, ok := content.(string)
	if !ok {
		data, err := json.MarshalIndent(content, "", "  ")
		must(f.t, err)
		text = string(data) + "\n"
	}
	docs, err := trace.Read[trace.Document](f.repo, stream)
	must(f.t, err)
	revision := 1
	for _, d := range docs {
		if d.ID == id {
			revision = d.Revision + 1
		}
	}
	d := trace.Document{Header: f.header("document", id, unit, revision, reviewerActor), Path: path, Content: text}
	must(f.t, f.repo.RecordDocuments(context.Background(), []trace.Document{d}))
	return d
}

// move transitions a workflow subject to a state.
func (f *walkFixture) move(subject, unit, to string) {
	f.t.Helper()
	state, err := f.repo.Workflow(stream, subject)
	must(f.t, err)
	id := fmt.Sprintf("%s-%s-%d", subject, to, state.Version)
	_, err = f.repo.Transact(context.Background(), trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: f.header("transition", id, unit, 1, foremanActor), Subject: subject, From: state.Value, To: to, Reason: "walk test"}})
	must(f.t, err)
}

func (f *walkFixture) unit(unit string, states ...string) {
	for _, s := range states {
		f.move(trace.UnitSubject(unit), unit, s)
	}
}

func (f *walkFixture) append(r trace.Record) {
	f.t.Helper()
	must(f.t, f.repo.Append(context.Background(), r))
}

// sealed records the spec, the plan with units a (spec#1) and b (spec#2,
// after a), the owner's ratification and seal 1 on the upstream commit 0...0
// and moves the workstream to building.
func (f *walkFixture) sealed() {
	f.doc(plan.SpecDocument, plan.SpecPath, "", walkSpec)
	graph := plan.Plan{Version: plan.Version, Units: []plan.Unit{
		{ID: "a", Title: "Parse specs", Addresses: []plan.Address{{Criterion: "spec#1", Proof: plan.Proof{Kind: plan.NewTest, Name: "TestParse"}}}, DependsOn: []string{}, Footprint: []string{"spec"}},
		{ID: "b", Title: "Validate plans", Addresses: []plan.Address{{Criterion: "spec#2", Proof: plan.Proof{Kind: plan.ExistingTest, Name: "TestValidate"}}}, DependsOn: []string{"a"}, Footprint: []string{"plan"}},
	}}
	encoded, err := plan.Encode(graph)
	must(f.t, err)
	f.doc(plan.PlanDocument, plan.PlanPath, "", string(encoded))
	ratification, err := shed.EncodeRatification(shed.Ratify(1, shed.Pin{Spec: 1, Plan: 1}, nil))
	must(f.t, err)
	f.doc(shed.RatificationDocumentID(1), shed.RatificationPath(1), "", string(ratification))
	s, err := seal.Encode(seal.Seal{Version: seal.Version, Seal: 1, Round: 1, Revision: shed.Pin{Spec: 1, Plan: 1}, SpecHash: seal.SpecHash(walkSpec), Base: seal.Base{Remote: "upstream", Branch: "main", Commit: sha('0')}, Branch: "osmia/feature", Workspace: "/branches/feature"})
	must(f.t, err)
	f.doc(seal.DocumentID, seal.Path, "", string(s))
	f.move(trace.FeatureSubject, "", RatifiedState)
	f.move(trace.FeatureSubject, "", BuildingState)
	f.unit("a", UnitPlanned, UnitReady)
	f.unit("b", UnitPlanned)
}

func (f *walkFixture) report(unit, candidate, base, criterion string) {
	f.doc(reportDocument(unit), fmt.Sprintf(reportPath, unit), unit, UnitReport{Unit: unit, Turn: "mason-" + unit + "-1", Seal: 1, Outcome: "done", Branch: "osmia/unit-" + unit, Base: base, Candidate: candidate,
		Criteria: []CriterionReport{{Criterion: criterion, Done: "implemented", Evidence: "tests pass at " + candidate, Proof: "TestParse"}}})
}

// review records the identity a review is asked for, then its verdict on
// the given report revision.
func (f *walkFixture) review(unit string, report int, candidate, base, criterion, decision string, bounces int) {
	identity := UnitReviewIdentity{Subject: string(stream) + "/" + unit, Candidate: coreadapter.Candidate{Revision: candidate, BaseRevision: base, SpecRevision: "1", PlanRevision: "1"}, DiffSHA256: "digest-" + candidate[:4], Report: fmt.Sprintf("units/%s/report.json revision %d", unit, report), Seal: 1}
	path := fmt.Sprintf("units/%s/review.json", unit)
	f.doc(reviewDocument(unit), path, unit, identity)
	verdict := UnitVerdict{Decision: decision, Evidence: []ReviewEvidence{{Criterion: criterion, Evidence: "reviewed " + candidate}}, Findings: []ReviewFinding{}}
	if decision == "material_findings" {
		verdict.Findings = []ReviewFinding{{Criterion: criterion, Severity: "major", Evidence: "missing case", Action: "add it"}}
	}
	f.doc(reviewDocument(unit), path, unit, UnitReviewResult{Identity: identity, Turn: fmt.Sprintf("review-%s-%d", unit, report), Verdict: verdict, Bounces: bounces})
}

func (f *walkFixture) land(unit string, review int, candidate, base, commit, criterion string) {
	f.doc(landingDocument(unit), fmt.Sprintf("units/%s/landing.json", unit), unit, UnitLanding{Unit: unit, Operation: "op-land-" + unit, Approval: fmt.Sprintf("units/%s/review.json revision %d", unit, review), Turn: "review", Candidate: candidate, Base: base,
		Spec: "1", Plan: "1", Seal: 1, Criteria: []string{criterion}, Branch: "osmia/feature", Commit: commit, Message: "Land " + unit + "\n\nOsmia-Operation: op-land-" + unit + "\n"})
}

// buildA takes unit a through a send-back to its merge: candidate 1...1 is
// sent back, candidate 2...2 is approved and lands as a...a. A mason turn,
// its cost and an owner ruling on the mason's question are recorded.
func (f *walkFixture) buildA() {
	mason := trace.Actor{Kind: "agent", ID: "mason-a"}
	f.append(trace.Agent{Header: f.header("agent", "mason-a", "a", 1, mason), Role: "mason", ThreadID: "mason-a"})
	request := trace.TurnRequest{Header: f.header("turn-request", "request_mason-a-1", "a", 1, mason), AgentID: "mason-a", ThreadID: "mason-a", TurnID: "mason-a-1", Profile: coreadapter.Profile{Name: "default", Backend: "fake", Model: "fake"}, Prompt: "build a"}
	f.append(request)
	f.unit("a", UnitImplementing)
	f.append(trace.Question{Header: f.header("question", "q1", "a", 1, mason), AskedBy: mason, Thread: "mason-a", Turn: "mason-a-1", Question: "Which parser?"})
	f.append(trace.Ruling{Header: f.header("ruling", "q1-ruling", "a", 1, ownerActor), QuestionID: "q1", QuestionRevision: 1, Decision: trace.DecisionRuling, OwnerResponse: "The strict one.", Citations: []string{"spec#1"}})
	usage := coreadapter.Usage{CostUSD: 0.25, CostKnown: true, Turns: 1}
	f.append(trace.TurnResponse{Header: f.header("turn-response", "response_mason-a-1", "a", 1, mason), AgentID: "mason-a", ThreadID: "mason-a", TurnID: "mason-a-1", RequestID: request.ID, RequestRevision: 1, Result: coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "fake", ID: "s1"}, StartedAt: f.at, Usage: usage}})
	f.append(trace.Cost{Header: f.header("cost", "cost-mason-a-1", "a", 1, ownerActor), Entry: coreadapter.LedgerEntry{Scope: coreadapter.Scope{Project: string(project), Workstream: string(stream), Unit: "a", Thread: "mason-a", Turn: "mason-a-1", Role: "mason"}, AttemptID: "attempt-1", At: f.at, Usage: usage}})
	f.report("a", sha('1'), sha('0'), "spec#1")
	f.unit("a", UnitReviewing)
	f.review("a", 1, sha('1'), sha('0'), "spec#1", "material_findings", 1)
	f.unit("a", UnitImplementing)
	f.report("a", sha('2'), sha('0'), "spec#1")
	f.unit("a", UnitReviewing)
	f.review("a", 2, sha('2'), sha('0'), "spec#1", "satisfactory", 1)
	f.unit("a", UnitApproved)
	f.land("a", 4, sha('2'), sha('0'), sha('a'), "spec#1")
	f.unit("a", UnitMerged)
	f.unit("b", UnitReady)
}

// buildB lands unit b as b...b on top of a...a.
func (f *walkFixture) buildB() {
	f.unit("b", UnitImplementing)
	f.report("b", sha('3'), sha('a'), "spec#2")
	f.unit("b", UnitReviewing)
	f.review("b", 1, sha('3'), sha('a'), "spec#2", "satisfactory", 0)
	f.unit("b", UnitApproved)
	f.land("b", 2, sha('3'), sha('a'), sha('b'), "spec#2")
	f.unit("b", UnitMerged)
}

// deliver records the final review of the rebased branch f...f, the
// owner's approval and its publication as one squashed commit d...d.
func (f *walkFixture) deliver() {
	f.move(trace.FeatureSubject, "", AssembledState)
	upstream := seal.Base{Remote: "upstream", Branch: "main", Commit: sha('9')}
	f.doc(finalReportDocument, finalReportPath, "", FinalReport{Review: 1, Operation: "op-final", Outcome: finalReviewed, Branch: "osmia/feature", Before: sha('b'), Commit: sha('f'), Upstream: &upstream, Seal: 1, SpecHash: seal.SpecHash(walkSpec), Spec: 1, Plan: 1, Charter: 1, Turn: "final-1-member-1",
		Criteria: []FinalCriterion{{Criterion: "spec#1", Text: "Specs parse.", Evidence: "TestParse"}, {Criterion: "spec#2", Text: "Plans validate.", Evidence: "TestValidate"}}})
	approval := f.doc(deliveryDocument, deliveryPath, "", DeliveryApproval{Review: 1, ReviewRevision: 1, Commit: sha('f'), Seal: 1, Spec: 1, Plan: 1, Charter: 1, Description: "Deliver", DescriptionHash: descriptionHash("Deliver"), At: f.at})
	f.doc(publicationDocument, publicationPath, "", DeliveryPublication{Approval: approval.Revision, Operation: "op-publish", Status: publicationOpened, Style: squashStyle, Fork: "owner/project", Remote: "origin", Branch: "osmia/feature", Reviewed: sha('f'), Commit: sha('d'), Upstream: "upstream/project", Base: "main", PullRequest: 7, URL: "https://example.test/pull/7", Title: "Deliver", Description: "Deliver", DescriptionHash: descriptionHash("Deliver")})
	f.move(trace.FeatureSubject, "", DeliveredState)
}

// snapshot returns the content of every file of the trace repository,
// Git metadata included.
func (f *walkFixture) snapshot() map[string]string {
	f.t.Helper()
	files := map[string]string{}
	must(f.t, filepath.WalkDir(f.dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		files[path] = string(data)
		return err
	}))
	return files
}

func gapStates(gaps []TraceGap) map[string]string {
	out := map[string]string{}
	for _, g := range gaps {
		out[g.Link] = g.State
	}
	return out
}

// TestTraceWalkCompleteChain walks a delivered workstream from a criterion
// down to its delivery and from its commits back up to the criteria,
// revisions and rulings, and checks that the walks record nothing and give
// the same answer twice.
func TestTraceWalkCompleteChain(t *testing.T) {
	t.Parallel()
	f := newWalkFixture(t)
	f.sealed()
	f.buildA()
	f.buildB()
	f.deliver()
	before := f.snapshot()

	c, err := traceCriterion(f.repo, stream, "spec#1")
	must(t, err)
	if !c.Complete || len(c.Gaps) != 0 {
		t.Fatalf("criterion gaps %+v", c.Gaps)
	}
	if c.Text != "Specs parse." || c.Spec.Revision != 1 || c.Plan.Revision != 1 || c.Seal.Seal != 1 || c.Feature != DeliveredState {
		t.Fatalf("criterion %+v", c)
	}
	// The mason's question is about unit a, so its ruling is the unit's.
	if len(c.Rulings) != 1 || c.Rulings[0].Kind != rulingRatification || c.Rulings[0].Ref.Path != shed.RatificationPath(1) {
		t.Fatalf("criterion rulings %+v", c.Rulings)
	}
	if len(c.Units) != 1 || c.Units[0].Unit != "a" {
		t.Fatalf("criterion units %+v", c.Units)
	}
	a := c.Units[0]
	if len(a.Addresses) != 1 || a.Addresses[0].Proof.Name != "TestParse" || a.Addresses[0].Text != "Specs parse." {
		t.Fatalf("unit a addresses %+v", a.Addresses)
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
	if c.Final == nil || c.Final.Evidence != "TestParse" {
		t.Fatalf("final account %+v", c.Final)
	}
	d := c.Delivery
	if d == nil || d.Report.Revision != 1 || d.Approval.Revision != 1 || d.Reviewed != sha('f') || d.Published != sha('d') || d.PullRequest != 7 {
		t.Fatalf("delivery %+v", d)
	}

	// The unit walk keeps every revision and orders its history in time.
	u, err := traceUnit(f.repo, stream, "a")
	must(t, err)
	if !u.Complete || u.State != UnitMerged || u.Source != unitFromPlan || u.Definition.ID != plan.PlanDocument {
		t.Fatalf("unit %+v", u)
	}
	if !slices.IsSortedFunc(u.History, func(x, y TraceEvent) int { return x.At.Compare(y.At) }) {
		t.Fatal("unit history is not in time order")
	}
	var reviews []int
	var states []string
	for _, e := range u.History {
		if e.Ref.Kind == "document" && e.Ref.ID == reviewDocument("a") {
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
	if !slices.Equal(states, []string{UnitPlanned, UnitReady, UnitImplementing, UnitReviewing, UnitImplementing, UnitReviewing, UnitApproved, UnitMerged}) {
		t.Fatalf("unit states in history %v", states)
	}

	// The landing commit leads back to its approval, the report that
	// approval read, and the criteria of the spec revision it landed on.
	landing, err := traceCommit(f.repo, stream, sha('a'), "")
	must(t, err)
	if !landing.Complete || len(landing.Landings) != 1 {
		t.Fatalf("landing commit %+v", landing)
	}
	l := landing.Landings[0]
	if l.Unit != "a" || l.Review.Ref.Revision != 4 || l.Report.Ref.Revision != 2 || l.Report.Candidate != sha('2') || l.Spec.Revision != 1 || l.Plan.Revision != 1 || l.Seal.Seal != 1 || !reflect.DeepEqual(l.Criteria, []TraceCriterion{{Criterion: "spec#1", Text: "Specs parse."}}) || len(l.Rulings) != 1 {
		t.Fatalf("landing %+v", l)
	}

	// The published commit leads to the delivery and every landing.
	published, err := traceCommit(f.repo, stream, sha('d'), "")
	must(t, err)
	var units []string
	for _, l := range published.Landings {
		units = append(units, l.Unit)
	}
	if !published.Complete || published.Delivery == nil || published.Delivery.Publication == nil || !slices.Equal(units, []string{"a", "b"}) {
		t.Fatalf("published commit %+v", published)
	}

	// A commit rebased after landing is found through its trailer.
	rebased, err := traceCommit(f.repo, stream, sha('e'), "Land a\n\nOsmia-Operation: op-land-a\n")
	must(t, err)
	if rebased.Operation != "op-land-a" || len(rebased.Landings) != 1 || rebased.Landings[0].Unit != "a" {
		t.Fatalf("rebased commit %+v", rebased)
	}

	// A candidate that was sent back names the unit and leads to no
	// landing; the unit landed another candidate, so nothing is missing.
	sentBack, err := traceCommit(f.repo, stream, sha('1'), "")
	must(t, err)
	if !sentBack.Complete || len(sentBack.Landings) != 0 || len(sentBack.Records) != 2 || sentBack.Records[0].Role != commitCandidate || sentBack.Records[0].Unit != "a" {
		t.Fatalf("sent-back candidate %+v", sentBack)
	}

	// A walk is deterministic and records nothing.
	again, err := traceCriterion(f.repo, stream, "spec#1")
	must(t, err)
	first, _ := json.Marshal(c)
	second, _ := json.Marshal(again)
	if string(first) != string(second) {
		t.Fatalf("walks differ:\n%s\n%s", first, second)
	}
	if !reflect.DeepEqual(before, f.snapshot()) {
		t.Fatal("a trace walk changed the trace repository")
	}

	// The publication names the approval it published; a newer approval
	// revision does not take its place.
	f.doc(deliveryDocument, deliveryPath, "", DeliveryApproval{Review: 1, ReviewRevision: 1, Commit: sha('f'), Seal: 1, Spec: 1, Plan: 1, Charter: 1, Description: "Edited", DescriptionHash: descriptionHash("Edited"), At: f.at})
	c, err = traceCriterion(f.repo, stream, "spec#1")
	must(t, err)
	if c.Delivery.Approval.Revision != 1 {
		t.Fatalf("delivery follows approval revision %d", c.Delivery.Approval.Revision)
	}
}

// TestTraceWalkIncompleteChain walks a workstream still building: every
// link not there yet is reported with its state, and nothing substitutes
// for it.
func TestTraceWalkIncompleteChain(t *testing.T) {
	t.Parallel()
	f := newWalkFixture(t)
	f.sealed()
	f.buildA()
	f.unit("b", UnitImplementing)

	c, err := traceCriterion(f.repo, stream, "spec#2")
	must(t, err)
	if c.Complete || c.Delivery != nil || c.Final != nil {
		t.Fatalf("criterion %+v", c)
	}
	if got := gapStates(c.Gaps); !reflect.DeepEqual(got, map[string]string{finalReportPath: LinkNotCreated, deliveryPath: LinkNotCreated, publicationPath: LinkNotCreated}) {
		t.Fatalf("criterion gaps %v", got)
	}
	b := c.Units[0]
	if b.Complete || !reflect.DeepEqual(gapStates(b.Gaps), map[string]string{"units/b/report.json": LinkUnfinished}) {
		t.Fatalf("unit b gaps %+v", b.Gaps)
	}

	// A reported unit waits for its verdict, then for its landing.
	f.report("b", sha('3'), sha('a'), "spec#2")
	f.unit("b", UnitReviewing)
	u, err := traceUnit(f.repo, stream, "b")
	must(t, err)
	if !reflect.DeepEqual(gapStates(u.Gaps), map[string]string{"verdict on units/b/report.json revision 1": LinkUnfinished}) {
		t.Fatalf("reviewing gaps %+v", u.Gaps)
	}
	f.review("b", 1, sha('3'), sha('a'), "spec#2", "satisfactory", 0)
	f.unit("b", UnitApproved)
	u, err = traceUnit(f.repo, stream, "b")
	must(t, err)
	if !reflect.DeepEqual(gapStates(u.Gaps), map[string]string{"landing of units/b/review.json revision 2": LinkUnfinished}) {
		t.Fatalf("approved gaps %+v", u.Gaps)
	}
	// The approved candidate has not landed yet.
	candidate, err := traceCommit(f.repo, stream, sha('3'), "")
	must(t, err)
	if candidate.Complete || len(candidate.Landings) != 0 || gapStates(candidate.Gaps)["landing of candidate "+sha('3')] != LinkUnfinished {
		t.Fatalf("candidate %+v", candidate)
	}

	// A landing that names an approval the trace does not hold is
	// unavailable, not resolved to the latest review.
	f.land("b", 9, sha('3'), sha('a'), sha('b'), "spec#2")
	f.unit("b", UnitMerged)
	u, err = traceUnit(f.repo, stream, "b")
	must(t, err)
	if got := gapStates(u.Gaps); got["units/b/review.json revision 9"] != LinkUnavailable || got["landing of units/b/review.json revision 2"] != LinkUnavailable {
		t.Fatalf("merged gaps %v", got)
	}
	landed, err := traceCommit(f.repo, stream, sha('b'), "")
	must(t, err)
	if landed.Complete || landed.Landings[0].Review != nil || gapStates(landed.Gaps)["units/b/review.json revision 9"] != LinkUnavailable {
		t.Fatalf("landed commit %+v", landed)
	}

	// Unknown commits, criteria and units are not in the trace.
	for _, err := range []error{
		func() error { _, err := traceCommit(f.repo, stream, sha('7'), ""); return err }(),
		func() error { _, err := traceCriterion(f.repo, stream, "spec#3"); return err }(),
		func() error { _, err := traceUnit(f.repo, stream, "z"); return err }(),
	} {
		if !errors.Is(err, errTraceNotFound) {
			t.Fatalf("unknown walk: %v", err)
		}
	}
}

// TestTraceWalkKeepsRevisions revises the spec after the seal: every walk
// keeps the sealed revision's text, and a later seal governs the criterion
// while the landing keeps the revisions it recorded.
func TestTraceWalkKeepsRevisions(t *testing.T) {
	t.Parallel()
	f := newWalkFixture(t)
	f.sealed()
	f.buildA()
	revised := strings.Replace(walkSpec, "Specs parse.", "Specs parse strictly.", 1)
	f.doc(plan.SpecDocument, plan.SpecPath, "", revised)

	c, err := traceCriterion(f.repo, stream, "spec#1")
	must(t, err)
	if c.Text != "Specs parse." || c.Spec.Revision != 1 || c.Units[0].Addresses[0].Text != "Specs parse." {
		t.Fatalf("criterion after an unsealed revision %+v", c)
	}

	ratification, err := shed.EncodeRatification(shed.Ratify(2, shed.Pin{Spec: 2, Plan: 1}, nil))
	must(t, err)
	f.doc(shed.RatificationDocumentID(2), shed.RatificationPath(2), "", string(ratification))
	s, err := seal.Encode(seal.Seal{Version: seal.Version, Seal: 2, Round: 2, Revision: shed.Pin{Spec: 2, Plan: 1}, SpecHash: seal.SpecHash(revised), Base: seal.Base{Remote: "upstream", Branch: "main", Commit: sha('0')}, Branch: "osmia/feature", Workspace: "/branches/feature"})
	must(t, err)
	f.doc(seal.DocumentID, seal.Path, "", string(s))

	c, err = traceCriterion(f.repo, stream, "spec#1")
	must(t, err)
	if c.Text != "Specs parse strictly." || c.Spec.Revision != 2 || c.Seal.Seal != 2 || c.Rulings[0].Ref.Path != shed.RatificationPath(2) {
		t.Fatalf("criterion after resealing %+v", c)
	}
	a := c.Units[0]
	if a.Reports[1].Seal != 1 || a.Reviews[1].Candidate.SpecRevision != "1" || a.Landings[0].Spec != "1" {
		t.Fatalf("unit evidence lost its recorded revisions %+v", a)
	}
	landing, err := traceCommit(f.repo, stream, sha('a'), "")
	must(t, err)
	l := landing.Landings[0]
	if l.Spec.Revision != 1 || l.Seal.Seal != 1 || l.Criteria[0].Text != "Specs parse." {
		t.Fatalf("landing resolved a newer revision %+v", l)
	}
}

// TestTraceWalkAbandoned walks a workstream abandoned while a unit was
// being built: the walks still read, and what was never produced is not
// reported as unfinished.
func TestTraceWalkAbandoned(t *testing.T) {
	t.Parallel()
	f := newWalkFixture(t)
	f.sealed()
	f.buildA()
	f.unit("b", UnitImplementing)
	f.move(trace.FeatureSubject, "", AbandonedState)

	c, err := traceCriterion(f.repo, stream, "spec#2")
	must(t, err)
	if c.Feature != AbandonedState || c.Complete {
		t.Fatalf("criterion %+v", c)
	}
	if got := gapStates(c.Units[0].Gaps); !reflect.DeepEqual(got, map[string]string{"units/b/report.json": LinkNotCreated}) {
		t.Fatalf("abandoned unit gaps %v", got)
	}
	for link, state := range gapStates(c.Gaps) {
		if state != LinkNotCreated {
			t.Fatalf("abandoned gap %s is %s", link, state)
		}
	}
	a, err := traceUnit(f.repo, stream, "a")
	must(t, err)
	if !a.Complete || a.State != UnitMerged {
		t.Fatalf("merged unit of an abandoned workstream %+v", a)
	}
	if _, err := traceCommit(f.repo, stream, sha('a'), ""); err != nil {
		t.Fatal(err)
	}
}

// TestTraceWalkBeforeSeal walks a workstream that has no seal yet.
func TestTraceWalkBeforeSeal(t *testing.T) {
	t.Parallel()
	f := newWalkFixture(t)
	f.doc(plan.SpecDocument, plan.SpecPath, "", walkSpec)
	f.move(trace.FeatureSubject, "", InShedState)
	c, err := traceCriterion(f.repo, stream, "spec#1")
	must(t, err)
	if c.Complete || c.Text != "" || c.Spec != nil || !reflect.DeepEqual(gapStates(c.Gaps), map[string]string{seal.Path: LinkNotCreated}) {
		t.Fatalf("criterion before the seal %+v", c)
	}
	f.move(trace.FeatureSubject, "", RatifiedState)
	c, err = traceCriterion(f.repo, stream, "spec#1")
	must(t, err)
	if gapStates(c.Gaps)[seal.Path] != LinkUnfinished {
		t.Fatalf("criterion while sealing %+v", c.Gaps)
	}
	if _, err := traceCriterion(f.repo, stream, "1"); err == nil || errors.Is(err, errTraceNotFound) {
		t.Fatalf("uncited criterion: %v", err)
	}
}
