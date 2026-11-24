package jev

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/kpenfound/agentcore/jev/systemone"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/trace"
)

// DecisionKind names a kind of decision Jev makes. Each kind has its own
// judgment task name and version.
type DecisionKind string

// The decision kinds: the owner gates Jev decides, the owner's own decisions
// Jev decides at the owner threshold, and the chief of staff's decisions.
const (
	KindRatification       DecisionKind = "ratification"
	KindIntentAmendment    DecisionKind = "intent-amendment"
	KindEscalatedQuestion  DecisionKind = "escalated-question"
	KindEscalatedContest   DecisionKind = "escalated-contest"
	KindEscalatedObjection DecisionKind = "escalated-objection"
	KindQuestionRouting    DecisionKind = "question-routing"
	KindContestedRuling    DecisionKind = "contested-ruling"
	KindObjection          DecisionKind = "objection"
	KindPlanAmendment      DecisionKind = "plan-amendment"
	KindDiscovery          DecisionKind = "discovery-disposition"
	KindTriage             DecisionKind = "final-review-triage"
	KindDeliveryRoute      DecisionKind = "delivery-route"

	KindDeliveryApproval    DecisionKind = "delivery-approval"
	KindCharterProposal     DecisionKind = "charter-proposal"
	KindCharterConflict     DecisionKind = "charter-conflict"
	KindArchitectEscalation DecisionKind = "architect-escalation"
	KindOwnerObjection      DecisionKind = "owner-objection"
	KindCancellation        DecisionKind = "cancellation"
)

type kindSpec struct {
	version      int
	instructions string
	owner        bool
}

var kinds = map[DecisionKind]kindSpec{
	KindRatification: {1, "The state is a ratification packet of a feature the factory drafted: the spec and plan revisions, what the committee objected to and how those objections stand. " +
		"Should the owner's ratification be given, so the feature is built as drafted? Choose ratify only when nothing in the packet needs the owner's attention.", false},
	KindIntentAmendment:    {1, "The state is an amendment request that changes the approved intent of a feature, with the debate about it. Choose whether to approve or decline it.", false},
	KindEscalatedQuestion:  {1, "The state is a question the chief of staff escalated to the owner, with the options the chief wrote and its evidence. Choose the option the owner would choose.", false},
	KindEscalatedContest:   {1, "The state is a unit of work contested after repeated failed attempts, escalated to the owner with the options its card offers. Choose the option the owner would choose.", false},
	KindEscalatedObjection: {1, "The state is a committee objection escalated to the owner at the ratification gate, with the options its card offers. Choose the option the owner would choose.", false},
	KindQuestionRouting:    {1, "The state is a question a worker asked while building a feature, with the chief of staff's evidence. Choose how it is routed: answered within the approved intent, routed as an amendment, or escalated to the owner.", false},
	KindContestedRuling:    {1, "The state is a contested unit of work, with the chief of staff's evidence. Choose how the chief of staff rules: review again, revise the unit, or escalate to the owner.", false},
	KindObjection:          {1, "The state is a committee objection to the spec or plan, with the chief of staff's evidence. Choose whether the chief dismisses it or escalates it to the owner.", false},
	KindPlanAmendment:      {1, "The state is an amendment request that leaves the approved intent unchanged, with the debate about it. Choose whether to approve or decline it.", false},
	KindDiscovery:          {1, "The state is a piece of work a worker discovered during a feature. Choose its disposition: assign it to a unit, decline it, or retain it as a suggestion.", false},
	KindTriage:             {1, "The state is one blocking finding of a final review, with the chief of staff's evidence. Choose whether it needs code in the revision or is left to the owner.", false},
	KindDeliveryRoute: {1, "The state is a finished workstream of a project that may land directly: its spec summary, the final review report, the check results, and the size of the change and the areas it touches. No code is shown. " +
		"Choose how the workstream is delivered: land it, so the base branch is fast-forwarded to the checked commit, or deliver it as the draft pull request that already exists, marked ready for review. " +
		"Choose land only when the review found nothing that needs a person's eyes and the change is small and contained enough to go straight to the base branch. Choose pull-request when a person should see the change first. Defer when neither is clearly right.", false},

	KindDeliveryApproval: {1, "The state is the owner's delivery card for an assembled feature: the final review's findings, the check result, the chief of staff's delivery brief and the pull request text the service will publish. " +
		"Should the owner mark the pull request ready for review with exactly that text? Choose approve only when nothing on the card needs the owner's attention.", true},
	KindCharterProposal:     {1, "The state is a rule the chief of staff proposed for the project's charter from an owner ruling, with the charter as it stands. Choose whether the owner ratifies the rule as written or declines it.", true},
	KindCharterConflict:     {1, "The state is a conflict between the charters of the projects a feature touches, put to the owner with the options the card offers. Choose the option the owner would choose.", true},
	KindArchitectEscalation: {1, "The state is a question the architect raised and the chief of staff escalated to the owner, such as the judgment that a selected project needs no change, with the options the chief wrote. Choose the option the owner would choose.", true},
	KindOwnerObjection:      {1, "The state is an objection the owner raised to the spec or plan, standing at the ratification gate, with the debate about it. Choose how the owner disposes of it now: dismiss it, or overrule it and proceed.", true},
	KindCancellation:        {1, "The state is the impact summary of cancelling a workgroup or one of its member workstreams: what happens to each workstream and to its open pull requests. Choose the option the owner would choose.", true},
}

var alwaysDefers = map[DecisionKind]bool{KindDeliveryRoute: true}

// Kinds returns the decision kinds in a fixed order.
func Kinds() []DecisionKind {
	out := make([]DecisionKind, 0, len(kinds))
	for k := range kinds {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// Valid reports whether k is a decision kind.
func (k DecisionKind) Valid() bool { _, ok := kinds[k]; return ok }

// Task is the judgment task name of k.
func (k DecisionKind) Task() string { return "decide-" + string(k) }

// Version is the judgment version of k's task.
func (k DecisionKind) Version() int { return kinds[k].version }

// Owner reports whether k is one of the owner's own decisions, binding at the
// owner threshold.
func (k DecisionKind) Owner() bool { return kinds[k].owner }

// Occurrence identifies the one subject occurrence a decision judgment
// belongs to, within its workstream: a decision kind and the subject's key.
// Ask numbers the requests sent for it after fallbacks, from 0, and
// Threshold is the confidence its pick needed to be binding.
type Occurrence struct {
	Kind      string  `json:"kind"`
	Key       string  `json:"key"`
	Ask       int     `json:"ask,omitempty"`
	Threshold float64 `json:"threshold,omitempty"`
}

// RatificationKey is the occurrence key of a ratification round of one spec and
// plan revision pair.
func RatificationKey(round, spec, plan int) string {
	return fmt.Sprintf("ratification:round-%d:spec-%d:plan-%d", round, spec, plan)
}

// AmendmentKey is the occurrence key of an amendment.
func AmendmentKey(id string) string { return "amendment:" + id }

// QuestionKey is the occurrence key of a question.
func QuestionKey(id string) string { return "question:" + id }

// ContestKey is the occurrence key of a contest, keyed by the contest's own ID
// because a unit can be contested again.
func ContestKey(id string) string { return "contest:" + id }

// ObjectionKey is the occurrence key of an objection.
func ObjectionKey(id string) string { return "objection:" + id }
func DiscoveryKey(id string) string { return "discovery:" + id }
func FindingKey(review int, finding string) string {
	return fmt.Sprintf("finding:review-%d:%s", review, finding)
}
func CharterKey(question string) string { return "charter:" + question }

// DeliveryKey is the occurrence of one delivery card: the final review, its
// revision, the reviewed commit and the draft's hash it pins.
func DeliveryKey(review, revision int, commit, draft string) string {
	return fmt.Sprintf("delivery:review-%d:revision-%d:%s:%s", review, revision, commit, draft)
}

// CancellationKey is the occurrence of one cancellation's impact summary at the
// revision it was presented.
func CancellationKey(change string, revision int) string {
	return fmt.Sprintf("cancellation:%s:revision-%d", change, revision)
}

// DeliveryRouteKey is the occurrence key of a workstream's delivery: the final
// review round and the pushed commit the delivery is of.
func DeliveryRouteKey(stream config.WorkstreamID, review int, commit string) string {
	return fmt.Sprintf("delivery:%s:review-%d:%s", stream, review, commit)
}

const maxOccurrenceKey = 256

// MaxDecisionBytes bounds a decision request's body; a larger one falls back
// without being sent.
const MaxDecisionBytes = 72 << 10

// Defer is the option every decision offers besides the caller's own when
// they are fewer than two: Jev's way to say the decision is not its to make.
// A pick of it is never binding.
const Defer = "defer"

const questionID = "decision"

// DecisionOption is one option Jev may pick. Description tells Jev what the
// option does.
type DecisionOption struct {
	Name        string
	Description string
}

// DecisionRequest asks Jev for one decision.
type DecisionRequest struct {
	// Scope names where the decision is made. A missing thread, turn or role
	// takes a stable value derived from the occurrence, so a decision made by
	// service code outside a turn is recorded and costed under the scope's
	// workstream.
	Scope coreadapter.Scope
	// Cause is the trace record that asked, such as the gate's record or the
	// chief's turn request.
	Cause string
	Depth int
	Kind  DecisionKind
	// Occurrence is the subject occurrence's key, built with the key
	// functions of this package.
	Occurrence string
	// State is what the card, packet or chief summary shows.
	State   string
	Options []DecisionOption
	Sources []Source
}

// ResultStatus says what is recorded for an occurrence.
type ResultStatus string

const (
	// None: nothing is recorded; Jev has not answered or fallen back.
	None ResultStatus = "none"
	// Binding: Jev's pick reached the threshold and is the occurrence's
	// decision.
	Binding ResultStatus = "binding"
	// Nonbinding: Jev answered below the threshold, or deferred. The answer
	// is the occurrence's result but is never enforced.
	Nonbinding ResultStatus = "nonbinding"
	// FallbackOnly: only fallbacks are recorded, so the occurrence is still
	// pending and asking again sends a new request.
	FallbackOnly ResultStatus = "fallback"
)

// Result is what is recorded for an occurrence, or what asking produced. A
// fallback is never a pick: Pick and Confidence are set only for Binding and
// Nonbinding results.
type Result struct {
	Status ResultStatus
	// Pick is the option Jev chose and Confidence its confidence; Deferred
	// says Jev picked Defer, and Pick is then empty.
	Pick       string
	Confidence float64
	Deferred   bool
	// Threshold is the threshold the result was judged against.
	Threshold float64
	// Judgment is the judgment record the result is read from, or for a
	// fallback the latest fallback's.
	Judgment string
	// Reason and Detail say why a fallback fell back.
	Reason Reason
	Detail string
	// Recorded says the result was read from the trace, not asked now.
	Recorded bool
	// Refused is why the service refused the transition a binding pick called
	// for, when it did. The pick stays binding for the occurrence, but is not
	// applied again, and its decision is the owner's.
	Refused string
}

// ValidOccurrence reports why a decision kind and occurrence key are not
// usable, or "".
func ValidOccurrence(kind DecisionKind, key string) string {
	switch {
	case !kind.Valid():
		return fmt.Sprintf("unknown decision kind %q", kind)
	case strings.TrimSpace(key) == "" || len(key) > maxOccurrenceKey:
		return "an occurrence key of 1 to 256 characters is required"
	case strings.ContainsFunc(key, func(r rune) bool { return r < ' ' || r == 0x7f }):
		return "an occurrence key holds no control characters"
	}
	return ""
}

func (j *Judge) threshold() float64 {
	if t := j.Config().DecisionThreshold; t > 0 && t <= 1 {
		return t
	}
	return config.DefaultJevDecisionThreshold
}

// DecisionThreshold is the configured [jev] decision_threshold, or its default
// while the setting is out of range: the probability a decision needs to be
// binding.
func (j *Judge) DecisionThreshold() float64 { return j.threshold() }

// OwnerThreshold is the configured [jev] owner_threshold, or its default while
// the setting is out of range: the probability a pick on one of the owner's
// own decisions needs to be binding.
func (j *Judge) OwnerThreshold() float64 {
	if t := j.Config().OwnerThreshold; t > 0 && t <= 1 {
		return t
	}
	return config.DefaultJevOwnerThreshold
}

// Threshold is the probability a pick of kind needs to be binding: the owner
// threshold for the owner's own decisions, the decision threshold otherwise.
func (j *Judge) Threshold(kind DecisionKind) float64 {
	if kind.Owner() {
		return j.OwnerThreshold()
	}
	return j.threshold()
}

func acceptance(kind DecisionKind, threshold float64) string {
	name := "decision_threshold="
	if kind.Owner() {
		name = "owner_threshold="
	}
	return name + strconv.FormatFloat(threshold, 'g', -1, 64)
}

type recorded struct {
	id       string
	revision int
	record   Record
}

func occurrenceRecords(repo *trace.Repository, stream config.WorkstreamID, kind DecisionKind, key string) ([]recorded, error) {
	docs, err := trace.Read[trace.Document](repo, stream)
	if err != nil {
		return nil, err
	}
	latest := map[string]trace.Document{}
	var order []string
	for _, d := range docs {
		if !strings.HasPrefix(d.Path, "judgments/") {
			continue
		}
		if prior, ok := latest[d.ID]; !ok {
			order = append(order, d.ID)
			latest[d.ID] = d
		} else if d.Revision > prior.Revision {
			latest[d.ID] = d
		}
	}
	var out []recorded
	for _, id := range order {
		d := latest[id]
		var r Record
		if json.Unmarshal([]byte(d.Content), &r) != nil {
			continue
		}
		if o := r.Occurrence; o == nil || o.Kind != string(kind) || o.Key != key {
			continue
		}
		out = append(out, recorded{id: id, revision: d.Revision, record: r})
	}
	return out, nil
}

func (r Record) pick() (string, float64, bool) {
	a, ok := r.Answers[questionID]
	if !ok || a.Choice == "" {
		return "", 0, false
	}
	return a.Choice, a.Confidence, true
}

func resultOf(records []recorded) Result {
	var fallback *recorded
	for i := range records {
		rec := records[i]
		switch rec.record.State {
		case StateAccepted:
			choice, confidence, ok := rec.record.pick()
			if !ok {
				continue
			}
			threshold := 0.0
			if o := rec.record.Occurrence; o != nil {
				threshold = o.Threshold
			}
			res := Result{Status: Nonbinding, Confidence: confidence, Threshold: threshold, Judgment: rec.id, Recorded: true}
			switch {
			case choice == Defer:
				res.Deferred = true
			default:
				res.Pick = choice
				if confidence >= threshold && threshold > 0 {
					res.Status = Binding
					res.Refused = rec.record.Refused
				}
			}
			return res
		case StateFallback:
			fallback = &records[i]
		}
	}
	if fallback != nil {
		r := fallback.record
		return Result{Status: FallbackOnly, Judgment: fallback.id, Reason: r.Reason, Detail: r.Detail, Recorded: true}
	}
	return Result{Status: None}
}

// Lookup returns what is recorded for the occurrence in the workstream's
// trace, without asking Jev: a binding pick, a nonbinding answer, only
// fallbacks, or none. A pending attempt that never finished counts as none.
func (j *Judge) Lookup(repo *trace.Repository, stream config.WorkstreamID, kind DecisionKind, key string) (Result, error) {
	return Recorded(repo, stream, kind, key)
}

// Recorded is Lookup for a caller that holds no Judge: what the workstream's
// trace records for the occurrence, without asking Jev.
func Recorded(repo *trace.Repository, stream config.WorkstreamID, kind DecisionKind, key string) (Result, error) {
	if reason := ValidOccurrence(kind, key); reason != "" {
		return Result{}, errors.New(reason)
	}
	records, err := occurrenceRecords(repo, stream, kind, key)
	if err != nil {
		return Result{}, err
	}
	return resultOf(records), nil
}

// Refuse records, as the next revision of the judgment, that the service
// refused the transition the occurrence's binding pick calls for, so the pick
// is never applied again and the occurrence is never asked again. It is a
// no-op for a judgment already refused. The judgment must be an accepted one
// of the workstream.
func (j *Judge) Refuse(ctx context.Context, repo *trace.Repository, stream config.WorkstreamID, judgment, reason string) error {
	latest, revision, err := load(repo, stream, judgment)
	switch {
	case err != nil:
		return err
	case revision == 0 || latest.State != StateAccepted:
		return fmt.Errorf("judgment %s is not an accepted judgment", judgment)
	case latest.Refused != "":
		return nil
	}
	latest.Refused = reason
	data, err := json.Marshal(latest)
	if err != nil {
		return err
	}
	s := latest.Scope
	h := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: judgment, Revision: revision + 1, Project: config.ProjectID(s.Project), Workstream: stream, Unit: s.Unit, At: j.now(), Actor: trace.Actor{Kind: "service", ID: Role}, Cause: "refused-" + judgment}
	return repo.RecordDocuments(context.WithoutCancel(ctx), []trace.Document{{Header: h, Path: Path(judgment), Content: string(data)}})
}

var occurrenceLocks sync.Map

func lockOccurrence(repo *trace.Repository, stream config.WorkstreamID, kind DecisionKind, key string) func() {
	id := fmt.Sprintf("%p/%s/%s/%s", repo, stream, kind, key)
	m, _ := occurrenceLocks.LoadOrStore(id, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// Decide returns the decision for the occurrence. It looks up the recorded
// judgments of the occurrence before it builds any request: a recorded
// binding pick or nonbinding answer is returned as recorded, whatever the
// request, model or threshold are now, and after a restart. Only an occurrence
// with no result sends a request, and the pick is binding when its confidence
// reaches the threshold of its kind. Every failure is a recorded
// fallback, never a pick, and a later ask of the same occurrence asks Jev
// again. It must not run on the scheduling path.
func (j *Judge) Decide(ctx context.Context, repo *trace.Repository, req DecisionRequest) Result {
	fallback := func(reason Reason, detail string) Result {
		return Result{Status: FallbackOnly, Reason: reason, Detail: detail}
	}
	if reason := ValidOccurrence(req.Kind, req.Occurrence); reason != "" {
		return fallback(ReasonRejected, reason)
	}
	if req.Scope.Workstream == "" {
		return fallback(ReasonRejected, "decision scope needs a workstream")
	}
	stream := config.WorkstreamID(req.Scope.Workstream)
	unlock := lockOccurrence(repo, stream, req.Kind, req.Occurrence)
	defer unlock()
	records, err := occurrenceRecords(repo, stream, req.Kind, req.Occurrence)
	if err != nil {
		return fallback(ReasonUnrecorded, "judgment records cannot be read")
	}
	if res := resultOf(records); res.Status == Binding || res.Status == Nonbinding {
		return res
	}
	asks := 0
	for _, r := range records {
		if r.record.State == StateFallback {
			asks++
		}
	}
	threshold := j.Threshold(req.Kind)
	options, err := decisionOptions(req.Options, alwaysDefers[req.Kind])
	if err != nil {
		return fallback(ReasonRejected, err.Error())
	}
	names := make([]string, 0, len(options))
	for _, o := range options {
		names = append(names, o.Name)
	}
	spec := kinds[req.Kind]
	scope := req.Scope
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(string(req.Kind)+"\x00"+req.Occurrence)))[:16]
	if scope.Thread == "" {
		scope.Thread = "decisions"
	}
	if scope.Turn == "" {
		scope.Turn = "decision-" + digest
	}
	if scope.Role == "" {
		scope.Role = "service"
	}
	cause := req.Cause
	if cause == "" {
		cause = "decision-" + digest
	}
	question := systemone.Choice(spec.instructions, options...)
	judgment := Judgment{
		Scope:      scope,
		Cause:      cause,
		Depth:      req.Depth,
		Task:       req.Kind.Task(),
		Version:    spec.version,
		Acceptance: acceptance(req.Kind, threshold),
		Sources:    req.Sources,
		Occurrence: &Occurrence{Kind: string(req.Kind), Key: req.Occurrence, Ask: asks, Threshold: threshold},
		MaxBytes:   MaxDecisionBytes,
		Request:    systemone.Request{State: req.State, Questions: map[string]systemone.Question{questionID: question}},
		Accept: func(r systemone.Response) string {
			a, ok := r.Answers[questionID]
			if !ok || !slices.Contains(names, a.Choice) {
				return "the answer picks none of the options"
			}
			return ""
		},
	}
	d := j.Evaluate(ctx, repo, judgment)
	if d.Outcome != Accepted {
		res := fallback(d.Reason, d.Detail)
		res.Judgment = d.ID
		return res
	}
	records, err = occurrenceRecords(repo, stream, req.Kind, req.Occurrence)
	if err != nil {
		return fallback(ReasonUnrecorded, "judgment records cannot be read")
	}
	res := resultOf(records)
	res.Recorded = d.Recovered
	return res
}

func decisionOptions(in []DecisionOption, withDefer bool) ([]systemone.Option, error) {
	if len(in) == 0 {
		return nil, errors.New("a decision needs an option")
	}
	seen := map[string]bool{}
	out := make([]systemone.Option, 0, len(in)+1)
	for _, o := range in {
		if strings.TrimSpace(o.Name) == "" || o.Name == Defer || seen[o.Name] {
			return nil, fmt.Errorf("decision option %q is empty, repeated or reserved", o.Name)
		}
		seen[o.Name] = true
		out = append(out, systemone.Option{Name: o.Name, Description: o.Description})
	}
	if len(out) < 2 || withDefer {
		out = append(out, systemone.Option{Name: Defer, Description: "Do not decide: the owner should look at this first."})
	}
	return out, nil
}
