package api

import (
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/trace"
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

// TraceReport is one revision of a unit's report.json.
type TraceReport struct {
	Ref       TraceRef `json:"ref"`
	Turn      string   `json:"turn"`
	Seal      int      `json:"seal"`
	Outcome   string   `json:"outcome"`
	Branch    string   `json:"branch"`
	Base      string   `json:"base"`
	Candidate string   `json:"candidate"`
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
	Summary    string                `json:"summary"`
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
// Stop says why the service stopped an interrupted turn on purpose.
type TraceTurn struct {
	Agent        string          `json:"agent"`
	Role         string          `json:"role"`
	Turn         string          `json:"turn"`
	Status       string          `json:"status"`
	Request      TraceRef        `json:"request"`
	Response     *TraceRef       `json:"response,omitempty"`
	Failure      string          `json:"failure,omitempty"`
	Stop         *trace.TurnStop `json:"stop,omitempty"`
	CostUSD      float64         `json:"cost_usd"`
	UnknownCosts int             `json:"unknown_costs"`
}

// TraceEvent is one record in a unit's history.
type TraceEvent struct {
	At      time.Time `json:"at"`
	Ref     TraceRef  `json:"ref"`
	Summary string    `json:"summary"`
}

// UnitTrace is the walk of one unit: where it is defined, its task and
// acceptance and the criteria it serves, every revision of its reports, verdicts, rebases and landings,
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
	Task         string              `json:"task"`
	Acceptance   []string            `json:"acceptance"`
	Criteria     []TraceCriterion    `json:"criteria"`
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

// TraceDelivery is the delivery chain of a workstream: the final report an
// approval names, the owner's approval a publication names, and the
// publication, or the owner's own merge of the branch into upstream that
// delivered it without one, with the upstream commit found holding it.
type TraceDelivery struct {
	Merge       *TraceRef  `json:"merge,omitempty"`
	Merged      string     `json:"merged,omitempty"`
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
// unit that serves it with its reports and reviews, the
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
