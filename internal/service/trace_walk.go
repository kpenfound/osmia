package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/followup"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
)

// errTraceNotFound reports a criterion, unit or commit the workstream's trace
// does not know.
var errTraceNotFound = errors.New("not in the workstream trace")

// The states of a link a trace walk could not follow. LinkUnavailable is a
// link the trace should hold by now, or one a record names, that it does not
// hold or cannot read. LinkUnfinished is a link the work under way will
// produce. LinkNotCreated is a link nothing has started to produce, or one a
// terminal workstream never produced.
const (
	LinkUnavailable = "unavailable"
	LinkUnfinished  = "unfinished"
	LinkNotCreated  = "not-created"
)

// TraceGap is one link a walk could not follow: what the link is, the unit
// it belongs to, if any, its state and why.
type TraceGap struct {
	Link   string `json:"link"`
	Unit   string `json:"unit,omitempty"`
	State  string `json:"state"`
	Reason string `json:"reason"`
}

// TraceRef identifies one revision of one trace record: its kind, the
// record ID and revision, the document path of a document, and the
// record's provenance.
type TraceRef struct {
	Kind     string      `json:"kind"`
	ID       string      `json:"id"`
	Revision int         `json:"revision"`
	Path     string      `json:"path,omitempty"`
	At       time.Time   `json:"at"`
	Actor    trace.Actor `json:"actor"`
	Cause    string      `json:"cause"`
}

// TraceSeal is the seal.json revision a walk follows.
type TraceSeal struct {
	Ref    TraceRef  `json:"ref"`
	Seal   int       `json:"seal"`
	Round  int       `json:"round"`
	Base   seal.Base `json:"base"`
	Branch string    `json:"branch"`
}

// TraceCriterion is a criterion with its text in one spec revision.
type TraceCriterion struct {
	Criterion string `json:"criterion"`
	Text      string `json:"text"`
}

// TraceAddress is a criterion a unit addresses and its planned proof.
type TraceAddress struct {
	Criterion string     `json:"criterion"`
	Text      string     `json:"text"`
	Proof     plan.Proof `json:"proof"`
}

// TraceReport is one revision of a unit's report.json.
type TraceReport struct {
	Ref       TraceRef          `json:"ref"`
	Turn      string            `json:"turn"`
	Seal      int               `json:"seal"`
	Outcome   string            `json:"outcome"`
	Branch    string            `json:"branch"`
	Base      string            `json:"base"`
	Candidate string            `json:"candidate"`
	Criteria  []CriterionReport `json:"criteria"`
}

// TraceReview is one revision of a unit's review.json that records a
// verdict, with the candidate identity and report revision it reviewed.
type TraceReview struct {
	Ref        TraceRef              `json:"ref"`
	Turn       string                `json:"turn"`
	Decision   string                `json:"decision"`
	Report     string                `json:"report"`
	Seal       int                   `json:"seal"`
	Candidate  coreadapter.Candidate `json:"candidate"`
	DiffSHA256 string                `json:"diff_sha256"`
	Bounces    int                   `json:"bounces"`
	Evidence   []ReviewEvidence      `json:"evidence"`
	Findings   []ReviewFinding       `json:"findings"`
}

// TraceRuling is a decision that shaped the work: the owner's ratification
// of the sealed revisions, a ruling on a contested unit, or the answer to a
// question, with the question revision it answered.
type TraceRuling struct {
	Kind      string    `json:"kind"`
	Ref       TraceRef  `json:"ref"`
	Unit      string    `json:"unit,omitempty"`
	Question  *TraceRef `json:"question,omitempty"`
	Decision  string    `json:"decision"`
	Owner     string    `json:"owner,omitempty"`
	Answer    string    `json:"answer,omitempty"`
	Citations []string  `json:"citations,omitempty"`
}

// The kinds of a TraceRuling.
const (
	rulingRatification = "ratification"
	rulingContested    = "contested"
	rulingQuestion     = "question"
)

// TraceLanding is one revision of a unit's landing.json.
type TraceLanding struct {
	Ref TraceRef `json:"ref"`
	UnitLanding
}

// TraceRebase is one revision of a unit's rebase.json.
type TraceRebase struct {
	Ref TraceRef `json:"ref"`
	UnitRebase
}

// TraceTurn is one agent turn: its request, its response once recorded, and
// the cost of its attempts. Status is running until a response is recorded.
type TraceTurn struct {
	Agent        string    `json:"agent"`
	Role         string    `json:"role"`
	Turn         string    `json:"turn"`
	Status       string    `json:"status"`
	Request      TraceRef  `json:"request"`
	Response     *TraceRef `json:"response,omitempty"`
	Failure      string    `json:"failure,omitempty"`
	CostUSD      float64   `json:"cost_usd"`
	UnknownCosts int       `json:"unknown_costs"`
}

// TraceEvent is one record in a unit's history.
type TraceEvent struct {
	At      time.Time `json:"at"`
	Ref     TraceRef  `json:"ref"`
	Summary string    `json:"summary"`
}

// UnitTrace is the walk of one unit: where it is defined and what it
// addresses, every revision of its reports, verdicts, rebases and landings,
// the rulings on it, its turns and their cost, and its history in order.
type UnitTrace struct {
	Project      config.ProjectID    `json:"project"`
	Workstream   config.WorkstreamID `json:"workstream"`
	Feature      string              `json:"feature"`
	Unit         string              `json:"unit"`
	Title        string              `json:"title,omitempty"`
	State        string              `json:"state"`
	Source       string              `json:"source"`
	Definition   *TraceRef           `json:"definition,omitempty"`
	Addresses    []TraceAddress      `json:"addresses"`
	DependsOn    []string            `json:"depends_on"`
	Reports      []TraceReport       `json:"reports"`
	Reviews      []TraceReview       `json:"reviews"`
	Rulings      []TraceRuling       `json:"rulings"`
	Rebases      []TraceRebase       `json:"rebases"`
	Landings     []TraceLanding      `json:"landings"`
	Turns        []TraceTurn         `json:"turns"`
	CostUSD      float64             `json:"cost_usd"`
	UnknownCosts int                 `json:"unknown_costs"`
	History      []TraceEvent        `json:"history"`
	Gaps         []TraceGap          `json:"gaps"`
	Complete     bool                `json:"complete"`
}

// The sources of a unit: the sealed plan or a final-review follow-up.
const (
	unitFromPlan     = "plan"
	unitFromFollowup = "followup"
)

// TraceDelivery is the delivery chain of a workstream: the final report an
// approval names, the owner's approval a publication names, and the
// publication.
type TraceDelivery struct {
	Report      *TraceRef  `json:"report,omitempty"`
	Outcome     string     `json:"outcome,omitempty"`
	Reviewed    string     `json:"reviewed,omitempty"`
	Upstream    *seal.Base `json:"upstream,omitempty"`
	Approval    *TraceRef  `json:"approval,omitempty"`
	Approved    string     `json:"approved,omitempty"`
	Publication *TraceRef  `json:"publication,omitempty"`
	Status      string     `json:"status,omitempty"`
	Published   string     `json:"published,omitempty"`
	PullRequest int        `json:"pull_request,omitempty"`
	URL         string     `json:"url,omitempty"`
}

// CriterionTrace is the walk of one sealed criterion: the seal and the spec
// and plan revisions it pins, the rulings that govern the criterion, every
// unit that addresses it with its evidence filtered to the criterion, the
// final report's account of it and the delivery.
type CriterionTrace struct {
	Project    config.ProjectID    `json:"project"`
	Workstream config.WorkstreamID `json:"workstream"`
	Feature    string              `json:"feature"`
	Criterion  string              `json:"criterion"`
	Text       string              `json:"text,omitempty"`
	Seal       *TraceSeal          `json:"seal,omitempty"`
	Spec       *TraceRef           `json:"spec,omitempty"`
	Plan       *TraceRef           `json:"plan,omitempty"`
	Rulings    []TraceRuling       `json:"rulings"`
	Units      []UnitTrace         `json:"units"`
	Final      *FinalCriterion     `json:"final,omitempty"`
	Delivery   *TraceDelivery      `json:"delivery,omitempty"`
	Gaps       []TraceGap          `json:"gaps"`
	Complete   bool                `json:"complete"`
}

// CommitRecord is one record that names a commit, with the role it gives
// the commit.
type CommitRecord struct {
	Role string   `json:"role"`
	Unit string   `json:"unit,omitempty"`
	Ref  TraceRef `json:"ref"`
}

// The roles a record gives a commit.
const (
	commitLanding      = "landing"
	commitCandidate    = "candidate"
	commitBase         = "base"
	commitRebase       = "unit-rebase"
	commitSealBase     = "seal-base"
	commitFinalRebase  = "final-rebase"
	commitFinalReview  = "final-review"
	commitApproved     = "delivery-approval"
	commitPublished    = "publication"
	commitPublishedRev = "publication-reviewed"
)

// CommitLanding is a unit landing a commit leads to, followed back to the
// review revision that approved it, the report that review read, the seal,
// spec and plan revisions the landing records, the criteria in that spec
// revision, and the rulings on the unit.
type CommitLanding struct {
	Unit     string           `json:"unit"`
	Landing  TraceLanding     `json:"landing"`
	Review   *TraceReview     `json:"review,omitempty"`
	Report   *TraceReport     `json:"report,omitempty"`
	Seal     *TraceSeal       `json:"seal,omitempty"`
	Spec     *TraceRef        `json:"spec,omitempty"`
	Plan     *TraceRef        `json:"plan,omitempty"`
	Criteria []TraceCriterion `json:"criteria"`
	Rulings  []TraceRuling    `json:"rulings"`
}

// CommitTrace is the walk of one commit: every record that names it, the
// landings it is or delivers, and the delivery chain when it is the reviewed
// or published delivery. Operation is the Osmia-Operation trailer of the
// commit's message, which names the landing or publication a rebased or
// squashed commit came from.
type CommitTrace struct {
	Project    config.ProjectID    `json:"project"`
	Workstream config.WorkstreamID `json:"workstream"`
	Feature    string              `json:"feature"`
	Commit     string              `json:"commit"`
	Operation  string              `json:"operation,omitempty"`
	Records    []CommitRecord      `json:"records"`
	Landings   []CommitLanding     `json:"landings"`
	Delivery   *TraceDelivery      `json:"delivery,omitempty"`
	Gaps       []TraceGap          `json:"gaps"`
	Complete   bool                `json:"complete"`
}

// traceWalk is one read of a workstream's trace. Every walk reads the
// records once, so a walk sees one consistent revision of the trace, and
// records nothing.
type traceWalk struct {
	project config.ProjectID
	stream  config.WorkstreamID
	records []trace.Record
	docs    []trace.Document
	// states holds the state of every workflow subject, and entered the
	// transition that last moved it.
	states  map[string]string
	entered map[string]trace.Transition
	feature string
	roles   map[string]string
	// seal is the latest seal, with the spec and plan revisions it pins.
	seal    *TraceSeal
	sealed  seal.Seal
	spec    plan.Spec
	specDoc *trace.Document
	planDoc *trace.Document
	units   []walkUnit
	gaps    []TraceGap
}

// walkUnit is a unit of the sealed plan or a follow-up, with the document
// revision that defines it.
type walkUnit struct {
	unit   plan.Unit
	source string
	def    trace.Document
}

func loadTraceWalk(repository *trace.Repository, stream config.WorkstreamID) (*traceWalk, error) {
	records, err := trace.Read[trace.Record](repository, stream)
	if err != nil {
		return nil, err
	}
	w := &traceWalk{project: repository.Project(), stream: stream, records: records, states: map[string]string{}, entered: map[string]trace.Transition{}, roles: map[string]string{}}
	for _, r := range records {
		switch v := r.(type) {
		case trace.Document:
			w.docs = append(w.docs, v)
		case trace.Transition:
			w.states[v.Subject] = v.To
			w.entered[v.Subject] = v
		case trace.Agent:
			w.roles[v.ID] = v.Role
		}
	}
	w.feature = w.states[trace.FeatureSubject]
	w.loadSeal()
	return w, nil
}

// terminal reports whether the workstream is delivered or abandoned, so
// nothing more is produced for it.
func (w *traceWalk) terminal() bool {
	return w.feature == AbandonedState || w.feature == DeliveredState
}

// state returns the state of a link that is not there, from how far the
// work that produces it has come: progress has reached done when the link
// should exist, or producing when the work under way produces it.
func (w *traceWalk) state(progress, producing, done int) string {
	switch {
	case progress >= done:
		return LinkUnavailable
	case progress == producing && !w.terminal():
		return LinkUnfinished
	}
	return LinkNotCreated
}

// featureProgress orders the feature states up to delivery.
func featureProgress(state string) int {
	switch state {
	case RatifiedState:
		return 1
	case BuildingState:
		return 2
	case AssembledState:
		return 3
	case DeliveredState:
		return 4
	}
	return 0
}

// loadSeal reads the latest seal, the spec and plan revisions it pins and
// the follow-ups, and records a gap for each it cannot read.
func (w *traceWalk) loadSeal() {
	latest, ok := w.latest(seal.DocumentID)
	if !ok {
		w.gaps = append(w.gaps, TraceGap{Link: seal.Path, State: w.state(featureProgress(w.feature), 1, 2), Reason: fmt.Sprintf("the workstream is %s and has no seal", orNone(w.feature))})
		return
	}
	s, err := seal.Parse([]byte(latest.Content))
	if err != nil {
		w.gaps = append(w.gaps, unreadable(latest, "", err))
		return
	}
	w.sealed = s
	w.seal = &TraceSeal{Ref: refOf(latest), Seal: s.Seal, Round: s.Round, Base: s.Base, Branch: s.Branch}
	if d, ok := w.document(plan.SpecDocument, s.Revision.Spec); ok {
		w.specDoc = &d
		w.spec = plan.ParseSpec(d.Content)
	} else {
		w.gaps = append(w.gaps, missingRevision(plan.SpecPath, s.Revision.Spec, fmt.Sprintf("seal %d", s.Seal)))
	}
	d, ok := w.document(plan.PlanDocument, s.Revision.Plan)
	if !ok {
		w.gaps = append(w.gaps, missingRevision(plan.PlanPath, s.Revision.Plan, fmt.Sprintf("seal %d", s.Seal)))
	} else {
		w.planDoc = &d
		p, err := plan.Parse([]byte(d.Content))
		if err != nil {
			w.gaps = append(w.gaps, unreadable(d, "", err))
		}
		for _, u := range p.Units {
			w.units = append(w.units, walkUnit{unit: u, source: unitFromPlan, def: d})
		}
	}
	for _, d := range w.docs {
		if d.ID != followup.DocumentID {
			continue
		}
		var batch []followup.Unit
		if err := json.Unmarshal([]byte(d.Content), &batch); err != nil {
			w.gaps = append(w.gaps, unreadable(d, "", err))
		}
		for _, u := range batch {
			w.units = append(w.units, walkUnit{unit: u.Unit, source: unitFromFollowup, def: d})
		}
	}
}

func missingRevision(path string, revision int, by string) TraceGap {
	return TraceGap{Link: fmt.Sprintf("%s revision %d", path, revision), State: LinkUnavailable, Reason: fmt.Sprintf("%s names it and the trace does not hold it", by)}
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// latest returns the latest revision of the document with the given ID.
func (w *traceWalk) latest(id string) (trace.Document, bool) {
	var found trace.Document
	for _, d := range w.docs {
		if d.ID == id {
			found = d
		}
	}
	return found, found.Revision > 0
}

// document returns the given revision of the document with the given ID.
func (w *traceWalk) document(id string, revision int) (trace.Document, bool) {
	i := slices.IndexFunc(w.docs, func(d trace.Document) bool { return d.ID == id && d.Revision == revision })
	if i < 0 {
		return trace.Document{}, false
	}
	return w.docs[i], true
}

// revisions returns every revision of the document with the given ID in
// recorded order.
func (w *traceWalk) revisions(id string) []trace.Document {
	var out []trace.Document
	for _, d := range w.docs {
		if d.ID == id {
			out = append(out, d)
		}
	}
	return out
}

// revisionOf reads a "<path> revision <n>" reference as reviews and
// landings record it.
func revisionOf(s string) (string, int, bool) {
	path, n, ok := strings.Cut(s, " revision ")
	revision, err := strconv.Atoi(n)
	return path, revision, ok && err == nil && revision > 0
}

func refOf(r trace.Record) TraceRef {
	var ref TraceRef
	var h trace.Header
	switch v := r.(type) {
	case trace.Document:
		ref.Kind, ref.Path, h = "document", v.Path, v.Header
	case trace.Transition:
		ref.Kind, h = "transition", v.Header
	case trace.Question:
		ref.Kind, h = "question", v.Header
	case trace.Ruling:
		ref.Kind, h = "ruling", v.Header
	case trace.Agent:
		ref.Kind, h = "agent", v.Header
	case trace.TurnRequest:
		ref.Kind, h = "turn-request", v.Header
	case trace.TurnResponse:
		ref.Kind, h = "turn-response", v.Header
	case trace.Cost:
		ref.Kind, h = "cost", v.Header
	case trace.Status:
		ref.Kind, h = "status", v.Header
	}
	ref.ID, ref.Revision, ref.At, ref.Actor, ref.Cause = h.ID, h.Revision, h.At, h.Actor, h.Cause
	return ref
}

func recordHeader(r trace.Record) trace.Header {
	switch v := r.(type) {
	case trace.Document:
		return v.Header
	case trace.Transition:
		return v.Header
	case trace.Question:
		return v.Header
	case trace.Ruling:
		return v.Header
	case trace.Agent:
		return v.Header
	case trace.TurnRequest:
		return v.Header
	case trace.TurnResponse:
		return v.Header
	case trace.Cost:
		return v.Header
	case trace.Status:
		return v.Header
	}
	return trace.Header{}
}

// findUnit returns the unit of the sealed plan or the follow-ups.
func (w *traceWalk) findUnit(id string) (walkUnit, bool) {
	i := slices.IndexFunc(w.units, func(u walkUnit) bool { return u.unit.ID == id })
	if i < 0 {
		return walkUnit{}, false
	}
	return w.units[i], true
}

// criterionText returns the criterion's text in the given spec.
func criterionText(spec plan.Spec, citation string) (string, bool) {
	n, ok := plan.ParseCitation(citation)
	if !ok {
		return "", false
	}
	c, ok := spec.Criterion(n)
	return c.Text, ok
}

// unitProgress orders a unit's states by how far the unit has come. A
// waiting unit is as far as the state it waits in.
func (w *traceWalk) unitProgress(unit string) int {
	subject := trace.UnitSubject(unit)
	state := w.states[subject]
	if state == UnitWaiting {
		state = w.entered[subject].From
	}
	switch state {
	case UnitImplementing:
		return 1
	case UnitReviewing:
		return 2
	case UnitContested:
		return 3
	case UnitApproved:
		return 4
	case UnitMerged:
		return 5
	}
	return 0
}

// traceUnit walks one unit of the workstream.
func traceUnit(repository *trace.Repository, stream config.WorkstreamID, unit string) (UnitTrace, error) {
	w, err := loadTraceWalk(repository, stream)
	if err != nil {
		return UnitTrace{}, err
	}
	u, ok := w.unit(unit, "")
	if !ok {
		return UnitTrace{}, fmt.Errorf("unit %s: %w", unit, errTraceNotFound)
	}
	// The seal and the revisions it pins define the unit.
	u.Gaps = append(slices.Clone(w.gaps), u.Gaps...)
	u.Complete = len(u.Gaps) == 0
	return u, nil
}

// unit builds the unit's walk, its evidence restricted to one criterion
// when criterion is not empty. It reports false for a unit that neither the
// sealed plan nor a follow-up defines and no record names.
func (w *traceWalk) unit(id, criterion string) (UnitTrace, bool) {
	t := UnitTrace{Project: w.project, Workstream: w.stream, Feature: w.feature, Unit: id, State: w.states[trace.UnitSubject(id)], Addresses: []TraceAddress{}, DependsOn: []string{}, Reports: []TraceReport{}, Reviews: []TraceReview{}, Rulings: []TraceRuling{}, Rebases: []TraceRebase{}, Landings: []TraceLanding{}, Turns: []TraceTurn{}, History: []TraceEvent{}, Gaps: []TraceGap{}}
	defined, ok := w.findUnit(id)
	if ok {
		ref := refOf(defined.def)
		t.Title, t.Source, t.Definition = defined.unit.Title, defined.source, &ref
		t.DependsOn = append(t.DependsOn, defined.unit.DependsOn...)
		for _, a := range defined.unit.Addresses {
			if criterion != "" && a.Criterion != criterion {
				continue
			}
			text, _ := criterionText(w.spec, a.Criterion)
			t.Addresses = append(t.Addresses, TraceAddress{Criterion: a.Criterion, Text: text, Proof: a.Proof})
		}
	} else if w.seal != nil {
		t.Gaps = append(t.Gaps, TraceGap{Link: "unit definition", Unit: id, State: LinkUnavailable, Reason: fmt.Sprintf("neither plan.json revision %d nor a follow-up defines the unit", w.sealed.Revision.Plan)})
	}
	named := false
	for _, r := range w.records {
		if recordHeader(r).Unit == id {
			named = true
			break
		}
	}
	if !ok && !named {
		return UnitTrace{}, false
	}
	w.unitDocuments(&t, criterion)
	t.Rulings = w.unitRulings(id)
	w.unitTurns(&t)
	w.unitHistory(&t)
	t.Gaps = append(t.Gaps, w.unitGaps(t)...)
	t.Complete = len(t.Gaps) == 0
	return t, true
}

func (w *traceWalk) unitDocuments(t *UnitTrace, criterion string) {
	for _, d := range w.revisions(reportDocument(t.Unit)) {
		var r UnitReport
		if err := json.Unmarshal([]byte(d.Content), &r); err != nil {
			t.Gaps = append(t.Gaps, unreadable(d, t.Unit, err))
			continue
		}
		report := TraceReport{Ref: refOf(d), Turn: r.Turn, Seal: r.Seal, Outcome: r.Outcome, Branch: r.Branch, Base: r.Base, Candidate: r.Candidate, Criteria: []CriterionReport{}}
		for _, c := range r.Criteria {
			if criterion == "" || c.Criterion == criterion {
				report.Criteria = append(report.Criteria, c)
			}
		}
		t.Reports = append(t.Reports, report)
	}
	for _, d := range w.revisions(reviewDocument(t.Unit)) {
		review, ok, err := reviewOf(d, criterion)
		if err != nil {
			t.Gaps = append(t.Gaps, unreadable(d, t.Unit, err))
		}
		if ok {
			t.Reviews = append(t.Reviews, review)
		}
	}
	for _, d := range w.revisions(rebaseDocument(t.Unit)) {
		var r UnitRebase
		if err := json.Unmarshal([]byte(d.Content), &r); err != nil {
			t.Gaps = append(t.Gaps, unreadable(d, t.Unit, err))
			continue
		}
		t.Rebases = append(t.Rebases, TraceRebase{Ref: refOf(d), UnitRebase: r})
	}
	for _, d := range w.revisions(landingDocument(t.Unit)) {
		l, err := landingOf(d)
		if err != nil {
			t.Gaps = append(t.Gaps, unreadable(d, t.Unit, err))
			continue
		}
		t.Landings = append(t.Landings, l)
	}
}

func unreadable(d trace.Document, unit string, err error) TraceGap {
	return TraceGap{Link: fmt.Sprintf("%s revision %d", d.Path, d.Revision), Unit: unit, State: LinkUnavailable, Reason: fmt.Sprintf("the recorded revision cannot be read: %v", err)}
}

// reviewOf reads a review.json revision. It reports false for a revision
// that records only the candidate identity a review was asked for.
func reviewOf(d trace.Document, criterion string) (TraceReview, bool, error) {
	var r UnitReviewResult
	if err := json.Unmarshal([]byte(d.Content), &r); err != nil {
		return TraceReview{}, false, err
	}
	if r.Turn == "" {
		return TraceReview{}, false, nil
	}
	review := TraceReview{Ref: refOf(d), Turn: r.Turn, Decision: r.Verdict.Decision, Report: r.Identity.Report, Seal: r.Identity.Seal, Candidate: r.Identity.Candidate, DiffSHA256: r.Identity.DiffSHA256, Bounces: r.Bounces, Evidence: []ReviewEvidence{}, Findings: []ReviewFinding{}}
	for _, e := range r.Verdict.Evidence {
		if criterion == "" || e.Criterion == criterion {
			review.Evidence = append(review.Evidence, e)
		}
	}
	for _, f := range r.Verdict.Findings {
		if criterion == "" || f.Criterion == criterion {
			review.Findings = append(review.Findings, f)
		}
	}
	return review, true, nil
}

func landingOf(d trace.Document) (TraceLanding, error) {
	var l UnitLanding
	if err := json.Unmarshal([]byte(d.Content), &l); err != nil {
		return TraceLanding{}, err
	}
	return TraceLanding{Ref: refOf(d), UnitLanding: l}, nil
}

// unitRulings returns the owner's rulings on the unit's contested
// candidates and the answers to the questions asked about the unit, in
// recorded order.
func (w *traceWalk) unitRulings(unit string) []TraceRuling {
	out := []TraceRuling{}
	prefix := trace.UnitSubject(unit) + "-ruling-"
	for _, r := range w.records {
		switch v := r.(type) {
		case trace.Document:
			if v.Unit != unit || !strings.HasPrefix(v.ID, prefix) {
				continue
			}
			var c ContestedRuling
			ruling := TraceRuling{Kind: rulingContested, Ref: refOf(v), Unit: unit}
			if err := json.Unmarshal([]byte(v.Content), &c); err == nil {
				ruling.Decision, ruling.Owner = c.Decision, c.Note
			}
			out = append(out, ruling)
		case trace.Ruling:
			if v.Unit == unit {
				out = append(out, w.questionRuling(v))
			}
		}
	}
	return out
}

func (w *traceWalk) questionRuling(v trace.Ruling) TraceRuling {
	ruling := TraceRuling{Kind: rulingQuestion, Ref: refOf(v), Unit: v.Unit, Decision: v.Decision, Owner: v.OwnerResponse, Answer: v.ReturnedAnswer, Citations: v.Citations}
	for _, r := range w.records {
		if q, ok := r.(trace.Question); ok && q.ID == v.QuestionID && q.Revision == v.QuestionRevision {
			ref := refOf(q)
			ruling.Question = &ref
		}
	}
	return ruling
}

// unitTurns collects the turns requested for the unit, their responses
// and the cost recorded for them.
func (w *traceWalk) unitTurns(t *UnitTrace) {
	index := map[string]int{}
	for _, r := range w.records {
		switch v := r.(type) {
		case trace.TurnRequest:
			if v.Unit != t.Unit {
				continue
			}
			index[v.AgentID+"/"+v.TurnID] = len(t.Turns)
			t.Turns = append(t.Turns, TraceTurn{Agent: v.AgentID, Role: w.roles[v.AgentID], Turn: v.TurnID, Status: "running", Request: refOf(v)})
		case trace.TurnResponse:
			i, ok := index[v.AgentID+"/"+v.TurnID]
			if !ok {
				continue
			}
			ref := refOf(v)
			t.Turns[i].Response, t.Turns[i].Failure, t.Turns[i].Status = &ref, v.Failure, trace.QueuedTurn{Response: &v, CompletedAt: v.At}.Status()
		case trace.Cost:
			i, ok := index[v.Entry.Scope.Thread+"/"+v.Entry.Scope.Turn]
			if !ok {
				continue
			}
			t.Turns[i].CostUSD += v.Entry.Usage.CostUSD
			t.CostUSD += v.Entry.Usage.CostUSD
			if !v.Entry.Usage.CostKnown {
				t.Turns[i].UnknownCosts++
				t.UnknownCosts++
			}
		}
	}
}

// unitHistory lists every record of the unit in time order; records at the
// same time keep their recorded order.
func (w *traceWalk) unitHistory(t *UnitTrace) {
	for _, r := range w.records {
		h := recordHeader(r)
		if h.Unit != t.Unit {
			continue
		}
		var summary string
		switch v := r.(type) {
		case trace.Transition:
			summary = fmt.Sprintf("%s: %s -> %s: %s", v.Subject, orNone(v.From), v.To, v.Reason)
		case trace.Document:
			summary = fmt.Sprintf("%s revision %d", v.Path, v.Revision)
		case trace.Question:
			summary = "question: " + v.Question
		case trace.Ruling:
			summary = fmt.Sprintf("ruling %s on question %s revision %d", v.Decision, v.QuestionID, v.QuestionRevision)
		case trace.Agent:
			summary = fmt.Sprintf("agent %s (%s)", v.ID, v.Role)
		case trace.TurnRequest:
			summary = fmt.Sprintf("turn %s requested of %s", v.TurnID, v.AgentID)
		case trace.TurnResponse:
			summary = fmt.Sprintf("turn %s of %s ended", v.TurnID, v.AgentID)
		case trace.Cost:
			summary = fmt.Sprintf("cost of turn %s attempt %s: $%.4f (known=%t)", v.Entry.Scope.Turn, v.Entry.AttemptID, v.Entry.Usage.CostUSD, v.Entry.Usage.CostKnown)
		default:
			continue
		}
		t.History = append(t.History, TraceEvent{At: h.At, Ref: refOf(r), Summary: summary})
	}
	sort.SliceStable(t.History, func(i, j int) bool { return t.History[i].At.Before(t.History[j].At) })
}

// unitGaps returns the links of the unit's chain that are missing: a
// report, a verdict on the latest report, a landing of the latest approval,
// and every revision a review or landing names that the trace does not
// hold.
func (w *traceWalk) unitGaps(t UnitTrace) []TraceGap {
	var gaps []TraceGap
	progress := w.unitProgress(t.Unit)
	state := orNone(t.State)
	if len(t.Reports) == 0 {
		gaps = append(gaps, TraceGap{Link: fmt.Sprintf(reportPath, t.Unit), Unit: t.Unit, State: w.state(progress, 1, 2), Reason: fmt.Sprintf("unit %s is %s and has no mason report", t.Unit, state)})
	} else {
		latest := t.Reports[len(t.Reports)-1].Ref
		named := fmt.Sprintf("%s revision %d", latest.Path, latest.Revision)
		if !slices.ContainsFunc(t.Reviews, func(r TraceReview) bool { return r.Report == named }) {
			s := LinkUnfinished
			if progress >= 3 {
				s = LinkUnavailable
			} else if w.terminal() {
				s = LinkNotCreated
			}
			gaps = append(gaps, TraceGap{Link: "verdict on " + named, Unit: t.Unit, State: s, Reason: fmt.Sprintf("unit %s is %s and no verdict reviews its latest report", t.Unit, state)})
		}
	}
	for _, r := range t.Reviews {
		path, revision, ok := revisionOf(r.Report)
		if _, found := w.document(reportDocument(t.Unit), revision); !ok || !found || path != fmt.Sprintf(reportPath, t.Unit) {
			gaps = append(gaps, TraceGap{Link: r.Report, Unit: t.Unit, State: LinkUnavailable, Reason: fmt.Sprintf("review.json revision %d names it and the trace does not hold it", r.Ref.Revision)})
		}
	}
	for _, l := range t.Landings {
		if _, ok := w.approval(t.Unit, l.Approval); !ok {
			gaps = append(gaps, TraceGap{Link: l.Approval, Unit: t.Unit, State: LinkUnavailable, Reason: fmt.Sprintf("landing.json revision %d names it as its approval and the trace does not hold a satisfactory verdict there", l.Ref.Revision)})
		}
	}
	if approved := w.latestApproval(t.Reviews); approved != nil && !slices.ContainsFunc(t.Landings, func(l TraceLanding) bool { return l.Approval == approvalName(*approved) }) {
		gaps = append(gaps, TraceGap{Link: "landing of " + approvalName(*approved), Unit: t.Unit, State: w.state(progress, 4, 5), Reason: fmt.Sprintf("unit %s is %s and its approval has not landed", t.Unit, state)})
	} else if approved == nil && len(t.Landings) == 0 && progress >= 4 {
		gaps = append(gaps, TraceGap{Link: fmt.Sprintf("units/%s/review.json", t.Unit), Unit: t.Unit, State: LinkUnavailable, Reason: fmt.Sprintf("unit %s is %s and the trace holds no satisfactory verdict", t.Unit, state)})
	}
	return gaps
}

func approvalName(r TraceReview) string {
	return fmt.Sprintf("%s revision %d", r.Ref.Path, r.Ref.Revision)
}

// latestApproval returns the latest satisfactory verdict when it is the
// unit's latest verdict.
func (w *traceWalk) latestApproval(reviews []TraceReview) *TraceReview {
	if len(reviews) == 0 || reviews[len(reviews)-1].Decision != "satisfactory" {
		return nil
	}
	return &reviews[len(reviews)-1]
}

// approval returns the satisfactory verdict a landing names.
func (w *traceWalk) approval(unit, name string) (TraceReview, bool) {
	path, revision, ok := revisionOf(name)
	d, found := w.document(reviewDocument(unit), revision)
	if !ok || !found || path != d.Path {
		return TraceReview{}, false
	}
	review, ok, err := reviewOf(d, "")
	return review, ok && err == nil && review.Decision == "satisfactory"
}

// traceCriterion walks one criterion of the workstream's sealed spec, cited
// as spec#<n>.
func traceCriterion(repository *trace.Repository, stream config.WorkstreamID, criterion string) (CriterionTrace, error) {
	if _, ok := plan.ParseCitation(criterion); !ok {
		return CriterionTrace{}, fmt.Errorf("criterion %q is not cited as spec#<n>", criterion)
	}
	w, err := loadTraceWalk(repository, stream)
	if err != nil {
		return CriterionTrace{}, err
	}
	t := CriterionTrace{Project: w.project, Workstream: stream, Feature: w.feature, Criterion: criterion, Seal: w.seal, Rulings: []TraceRuling{}, Units: []UnitTrace{}, Gaps: append([]TraceGap{}, w.gaps...)}
	if w.seal == nil {
		t.Complete = false
		return t, nil
	}
	if w.specDoc != nil {
		text, ok := criterionText(w.spec, criterion)
		if !ok {
			return CriterionTrace{}, fmt.Errorf("criterion %s of spec.md revision %d: %w", criterion, w.specDoc.Revision, errTraceNotFound)
		}
		ref := refOf(*w.specDoc)
		t.Text, t.Spec = text, &ref
	}
	if w.planDoc != nil {
		ref := refOf(*w.planDoc)
		t.Plan = &ref
	}
	if ruling, gap := w.ratification(); gap != nil {
		t.Gaps = append(t.Gaps, *gap)
	} else {
		t.Rulings = append(t.Rulings, ruling)
	}
	for _, r := range w.records {
		if v, ok := r.(trace.Ruling); ok && v.Unit == "" && slices.Contains(v.Citations, criterion) {
			t.Rulings = append(t.Rulings, w.questionRuling(v))
		}
	}
	for _, u := range w.units {
		if !slices.ContainsFunc(u.unit.Addresses, func(a plan.Address) bool { return a.Criterion == criterion }) {
			continue
		}
		unit, _ := w.unit(u.unit.ID, criterion)
		t.Units = append(t.Units, unit)
	}
	if len(t.Units) == 0 && w.planDoc != nil {
		t.Gaps = append(t.Gaps, TraceGap{Link: "units addressing " + criterion, State: LinkUnavailable, Reason: fmt.Sprintf("no unit of plan.json revision %d or a follow-up addresses the criterion", w.planDoc.Revision)})
	}
	delivery, report, gaps := w.delivery()
	t.Delivery, t.Gaps = delivery, append(t.Gaps, gaps...)
	if report != nil {
		i := slices.IndexFunc(report.Criteria, func(c FinalCriterion) bool { return c.Criterion == criterion })
		if i >= 0 {
			t.Final = &report.Criteria[i]
		} else if report.Outcome == finalReviewed {
			t.Gaps = append(t.Gaps, TraceGap{Link: "final account of " + criterion, State: LinkUnavailable, Reason: fmt.Sprintf("final/report.json revision %d gives no account of the criterion", delivery.Report.Revision)})
		}
	}
	complete := len(t.Gaps) == 0
	for _, u := range t.Units {
		complete = complete && u.Complete
	}
	t.Complete = complete
	return t, nil
}

// ratification returns the owner's ratification of the revisions the seal
// pins.
func (w *traceWalk) ratification() (TraceRuling, *TraceGap) {
	path := shed.RatificationPath(w.sealed.Round)
	for _, d := range slices.Backward(w.docs) {
		if d.Path != path {
			continue
		}
		r, err := shed.ParseRatification([]byte(d.Content))
		if err != nil || r.Revision != w.sealed.Revision {
			break
		}
		return TraceRuling{Kind: rulingRatification, Ref: refOf(d), Decision: "ratified", Owner: fmt.Sprintf("spec.md revision %d and plan.json revision %d", r.Revision.Spec, r.Revision.Plan)}, nil
	}
	return TraceRuling{}, &TraceGap{Link: path, State: LinkUnavailable, Reason: fmt.Sprintf("seal %d names round %d and the trace holds no ratification of spec.md revision %d and plan.json revision %d there", w.sealed.Seal, w.sealed.Round, w.sealed.Revision.Spec, w.sealed.Revision.Plan)}
}

// delivery follows the delivery chain back from the latest publication:
// the approval revision it publishes and the final report revision that
// approval names. Without a publication it starts from the latest approval,
// and without one from the latest final report. It returns the final report
// the chain reaches.
func (w *traceWalk) delivery() (*TraceDelivery, *FinalReport, []TraceGap) {
	var gaps []TraceGap
	pending := func(link, reason string) {
		gaps = append(gaps, TraceGap{Link: link, State: w.state(featureProgress(w.feature), 3, 4), Reason: fmt.Sprintf("the workstream is %s and %s", orNone(w.feature), reason)})
	}
	// follow returns the document the chain names, or the latest revision
	// when the chain names none. It reports false when the chain names a
	// revision the trace does not hold, or holds unreadable content.
	follow := func(id, path string, revision int, by string, v any) (trace.Document, bool, bool) {
		var d trace.Document
		var found bool
		if revision > 0 {
			if d, found = w.document(id, revision); !found {
				gaps = append(gaps, missingRevision(path, revision, by))
				return d, false, false
			}
		} else {
			d, found = w.latest(id)
		}
		if found {
			if err := json.Unmarshal([]byte(d.Content), v); err != nil {
				gaps = append(gaps, unreadable(d, "", err))
				return d, false, false
			}
		}
		return d, found, true
	}
	t := &TraceDelivery{}
	var p DeliveryPublication
	d, published, ok := follow(publicationDocument, publicationPath, 0, "", &p)
	if !ok {
		return nil, nil, gaps
	}
	approvalRevision := 0
	if published {
		ref := refOf(d)
		t.Publication, t.Status, t.Published, t.PullRequest, t.URL = &ref, p.Status, p.Commit, p.PullRequest, p.URL
		approvalRevision = p.Approval
		if p.Status != publicationOpened {
			pending("pull request", "the publication is "+p.Status)
		}
	} else {
		pending(publicationPath, "it is not published")
	}
	var a DeliveryApproval
	d, approved, ok := follow(deliveryDocument, deliveryPath, approvalRevision, fmt.Sprintf("%s revision %d", publicationPath, d.Revision), &a)
	if !ok {
		return t, nil, gaps
	}
	reportRevision := 0
	if approved {
		ref := refOf(d)
		t.Approval, t.Approved, reportRevision = &ref, a.Commit, a.ReviewRevision
	} else {
		pending(deliveryPath, "the owner has not approved its delivery")
	}
	var r FinalReport
	d, reviewed, ok := follow(finalReportDocument, finalReportPath, reportRevision, fmt.Sprintf("%s revision %d", deliveryPath, d.Revision), &r)
	if !ok {
		return t, nil, gaps
	}
	if !reviewed {
		pending(finalReportPath, "it has no final review")
		if !published && !approved {
			return nil, nil, gaps
		}
		return t, nil, gaps
	}
	ref := refOf(d)
	t.Report, t.Outcome, t.Reviewed, t.Upstream = &ref, r.Outcome, r.Commit, r.Upstream
	return t, &r, gaps
}

var commitPattern = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

// traceCommit walks one commit back to the records that name it. message
// is the commit's message, or empty when it is not known: its
// Osmia-Operation trailer finds the landing or publication of a commit that
// was rebased or squashed after it was recorded.
func traceCommit(repository *trace.Repository, stream config.WorkstreamID, commit, message string) (CommitTrace, error) {
	if !commitPattern.MatchString(commit) {
		return CommitTrace{}, fmt.Errorf("commit %q is not a full commit ID", commit)
	}
	w, err := loadTraceWalk(repository, stream)
	if err != nil {
		return CommitTrace{}, err
	}
	t := CommitTrace{Project: w.project, Workstream: stream, Feature: w.feature, Commit: commit, Operation: operationTrailer(message), Records: []CommitRecord{}, Landings: []CommitLanding{}, Gaps: []TraceGap{}}
	record := func(role, unit string, d trace.Document) {
		t.Records = append(t.Records, CommitRecord{Role: role, Unit: unit, Ref: refOf(d)})
	}
	landed := map[string]bool{}
	delivered := false
	for _, d := range w.docs {
		switch d.ID {
		case seal.DocumentID:
			if s, err := seal.Parse([]byte(d.Content)); err == nil && s.Base.Commit == commit {
				record(commitSealBase, "", d)
			}
		case finalRebaseDocument:
			var r FinalRebase
			if json.Unmarshal([]byte(d.Content), &r) == nil && r.Commit == commit {
				record(commitFinalRebase, "", d)
				delivered = true
			}
		case finalReportDocument:
			var r FinalReport
			if json.Unmarshal([]byte(d.Content), &r) == nil && r.Commit == commit {
				record(commitFinalReview, "", d)
				delivered = true
			}
		case deliveryDocument:
			var a DeliveryApproval
			if json.Unmarshal([]byte(d.Content), &a) == nil && a.Commit == commit {
				record(commitApproved, "", d)
				delivered = true
			}
		case publicationDocument:
			var p DeliveryPublication
			if json.Unmarshal([]byte(d.Content), &p) == nil {
				if p.Commit == commit || t.Operation != "" && p.Operation == t.Operation {
					record(commitPublished, "", d)
					delivered = true
				} else if p.Reviewed == commit {
					record(commitPublishedRev, "", d)
					delivered = true
				}
			}
		}
		if d.Unit == "" {
			continue
		}
		switch d.ID {
		case reportDocument(d.Unit):
			var r UnitReport
			if json.Unmarshal([]byte(d.Content), &r) == nil {
				if r.Candidate == commit {
					record(commitCandidate, d.Unit, d)
				} else if r.Base == commit {
					record(commitBase, d.Unit, d)
				}
			}
		case reviewDocument(d.Unit):
			if r, ok, _ := reviewOf(d, ""); ok {
				if r.Candidate.Revision == commit {
					record(commitCandidate, d.Unit, d)
				} else if r.Candidate.BaseRevision == commit {
					record(commitBase, d.Unit, d)
				}
			}
		case rebaseDocument(d.Unit):
			var r UnitRebase
			if json.Unmarshal([]byte(d.Content), &r) == nil && (r.Commit == commit || t.Operation != "" && r.Operation == t.Operation) {
				record(commitRebase, d.Unit, d)
			}
		case landingDocument(d.Unit):
			l, err := landingOf(d)
			if err != nil {
				continue
			}
			switch {
			case l.Commit == commit || t.Operation != "" && l.Operation == t.Operation:
				record(commitLanding, d.Unit, d)
				t.Landings = append(t.Landings, w.commitLanding(l, &t.Gaps))
				landed[d.Unit+"/"+strconv.Itoa(d.Revision)] = true
			case l.Candidate == commit:
				record(commitCandidate, d.Unit, d)
			case l.Base == commit:
				record(commitBase, d.Unit, d)
			}
		}
	}
	if len(t.Records) == 0 {
		return CommitTrace{}, fmt.Errorf("commit %s: %w", commit, errTraceNotFound)
	}
	// A candidate leads to the landing of that candidate, if it landed.
	followed := map[string]bool{}
	for _, l := range t.Landings {
		followed[l.Unit] = true
	}
	for _, r := range t.Records {
		if r.Role != commitCandidate || followed[r.Unit] {
			continue
		}
		followed[r.Unit] = true
		var found *TraceLanding
		for _, d := range w.revisions(landingDocument(r.Unit)) {
			if l, err := landingOf(d); err == nil && l.Candidate == commit {
				found = &l
			}
		}
		if found != nil {
			t.Landings = append(t.Landings, w.commitLanding(*found, &t.Gaps))
			landed[r.Unit+"/"+strconv.Itoa(found.Ref.Revision)] = true
		} else if w.unitProgress(r.Unit) < 5 {
			t.Gaps = append(t.Gaps, TraceGap{Link: "landing of candidate " + commit, Unit: r.Unit, State: w.state(w.unitProgress(r.Unit), 4, 5), Reason: fmt.Sprintf("unit %s is %s and the candidate has not landed", r.Unit, orNone(w.states[trace.UnitSubject(r.Unit)]))})
		}
	}
	// A delivery commit leads to the latest landing of every unit.
	if delivered {
		delivery, _, gaps := w.delivery()
		t.Delivery = delivery
		for _, g := range gaps {
			if g.State == LinkUnavailable {
				t.Gaps = append(t.Gaps, g)
			}
		}
		for _, u := range w.units {
			latest, ok := w.latest(landingDocument(u.unit.ID))
			if !ok {
				t.Gaps = append(t.Gaps, TraceGap{Link: fmt.Sprintf("units/%s/landing.json", u.unit.ID), Unit: u.unit.ID, State: w.state(w.unitProgress(u.unit.ID), 4, 5), Reason: fmt.Sprintf("unit %s is %s and has not landed", u.unit.ID, orNone(w.states[trace.UnitSubject(u.unit.ID)]))})
				continue
			}
			if landed[u.unit.ID+"/"+strconv.Itoa(latest.Revision)] {
				continue
			}
			l, err := landingOf(latest)
			if err != nil {
				t.Gaps = append(t.Gaps, unreadable(latest, u.unit.ID, err))
				continue
			}
			t.Landings = append(t.Landings, w.commitLanding(l, &t.Gaps))
		}
	}
	t.Complete = len(t.Gaps) == 0
	return t, nil
}

// operationTrailer returns the Osmia-Operation trailer of a commit message.
func operationTrailer(message string) string {
	for line := range strings.SplitSeq(message, "\n") {
		if value, ok := strings.CutPrefix(line, landingTrailer+": "); ok {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// commitLanding follows a landing back to its approval, the report the
// approval reviewed and the revisions the landing records. It never
// resolves a revision the landing does not name.
func (w *traceWalk) commitLanding(l TraceLanding, gaps *[]TraceGap) CommitLanding {
	c := CommitLanding{Unit: l.Unit, Landing: l, Criteria: []TraceCriterion{}, Rulings: w.unitRulings(l.Unit)}
	gap := func(link, reason string) {
		*gaps = append(*gaps, TraceGap{Link: link, Unit: l.Unit, State: LinkUnavailable, Reason: reason})
	}
	if review, ok := w.approval(l.Unit, l.Approval); ok {
		c.Review = &review
		_, revision, _ := revisionOf(review.Report)
		if d, ok := w.document(reportDocument(l.Unit), revision); ok {
			var r UnitReport
			if json.Unmarshal([]byte(d.Content), &r) == nil {
				c.Report = &TraceReport{Ref: refOf(d), Turn: r.Turn, Seal: r.Seal, Outcome: r.Outcome, Branch: r.Branch, Base: r.Base, Candidate: r.Candidate, Criteria: r.Criteria}
			}
		}
		if c.Report == nil {
			gap(review.Report, fmt.Sprintf("the approving review.json revision %d names it and the trace does not hold it", review.Ref.Revision))
		}
	} else {
		gap(l.Approval, fmt.Sprintf("landing.json revision %d names it as its approval and the trace does not hold a satisfactory verdict there", l.Ref.Revision))
	}
	for _, d := range w.revisions(seal.DocumentID) {
		if s, err := seal.Parse([]byte(d.Content)); err == nil && s.Seal == l.Seal {
			c.Seal = &TraceSeal{Ref: refOf(d), Seal: s.Seal, Round: s.Round, Base: s.Base, Branch: s.Branch}
		}
	}
	if c.Seal == nil {
		gap(fmt.Sprintf("seal %d", l.Seal), fmt.Sprintf("landing.json revision %d names it and the trace does not hold it", l.Ref.Revision))
	}
	var spec plan.Spec
	for _, doc := range []struct {
		id, path, revision string
		ref                **TraceRef
	}{{plan.SpecDocument, plan.SpecPath, l.Spec, &c.Spec}, {plan.PlanDocument, plan.PlanPath, l.Plan, &c.Plan}} {
		n, err := strconv.Atoi(doc.revision)
		d, ok := w.document(doc.id, n)
		if err != nil || !ok {
			gap(fmt.Sprintf("%s revision %s", doc.path, doc.revision), fmt.Sprintf("landing.json revision %d names it and the trace does not hold it", l.Ref.Revision))
			continue
		}
		ref := refOf(d)
		*doc.ref = &ref
		if doc.id == plan.SpecDocument {
			spec = plan.ParseSpec(d.Content)
		}
	}
	for _, criterion := range l.Criteria {
		text, ok := criterionText(spec, criterion)
		if c.Spec != nil && !ok {
			gap(criterion, fmt.Sprintf("landing.json revision %d names it and spec.md revision %d does not hold it", l.Ref.Revision, c.Spec.Revision))
		}
		c.Criteria = append(c.Criteria, TraceCriterion{Criterion: criterion, Text: text})
	}
	return c
}
