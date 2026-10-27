package intake

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kpenfound/osmia/internal/bundle"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/isolation"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
)

// RoundAction is the runner-boundary operation action that runs the
// committee's turns of one shed round.
const RoundAction = "shed-round"

const (
	CommitteeRole = "committee"
	// InShedState is the feature state of a workstream whose spec and plan the
	// committee debates.
	InShedState = "in-shed"
	// shedSubject is the workflow subject that tracks a workstream's debate:
	// round-<n> while the committee runs, waiting-<n> while round n waits
	// for the answer to a member's question, heard-<n> once its contributions
	// are recorded, reply-<n> while the architect answers, replied-<n> once
	// its reply is recorded, redraft-<n> while the architect writes the
	// redraft the owner asked for after round n and redrafted-<n> once it is
	// recorded, asked-<n> while the architect's reply or redraft after round
	// n waits for the answer to its question, concluded-<n> once debate ended after round n, and failed-<n>
	// when round n ended without a record or a reply.
	ShedSubject = "shed"
	// ownerSubject is the workflow subject that tracks the owner's own part
	// in the shed, one value per action: objected-<n>, ruled-<n>,
	// overruled-<n>, more-<n>, redraft-<n>, ratified-<n> and skipped. The
	// debate controller only reads it, so an owner action never races a round
	// for the shed subject.
	OwnerSubject = "shed-owner"
	// skippedValue is the owner-subject value of the action that skips
	// debate, and skipTransition the transition that records it. A later
	// action moves the subject on; the skip itself is the transition, and it
	// stands as long as the workstream is in the shed.
	SkippedValue   = "skipped"
	SkipTransition = "shed-owner-skip"
	// maxRoundAttempts bounds the turns one member may start in one round
	// after service stops interrupt earlier ones: the librarian's bound.
	MaxRoundAttempts = MaxExtractionAttempts
)

var ShedActor = trace.Actor{Kind: "service", ID: "shed"}

// Committee supplies the execution boundary of the committee's shed turns
// and final reviews: the core execution engine and the role-scoped MCP host. The service owns
// each member's view, tools, prompt and record.
type Committee struct {
	Engine coreadapter.Engine
	Hosts  coreadapter.MCPHosts
}

// roundInput pins a round to the revisions of spec.md and plan.json every
// member reads. Resume numbers the operation that runs the round again after
// it parked on a member's question, from 1; the round's own operation has
// none. Redraft distinguishes the architect's redraft at the owner's request
// from its reply to the round: it comes from the operation's action, not from
// its recorded input.
type RoundInput struct {
	Round     int    `json:"round"`
	Amendment string `json:"amendment,omitempty"`
	Spec      int    `json:"spec"`
	Plan      int    `json:"plan"`
	Resume    int    `json:"resume,omitempty"`
	Redraft   bool   `json:"-"`
}

func (in RoundInput) Pin() shed.Pin { return shed.Pin{Spec: in.Spec, Plan: in.Plan} }

// runIDs returns the transition and event of the operation that runs the
// round: the round's own, or the resumption the input numbers.
func (in RoundInput) RunIDs() (transition, event string) {
	if in.Resume == 0 {
		return RoundIDs(in.Round)
	}
	transition = fmt.Sprintf("shed-round-%d-resume-%d", in.Round, in.Resume)
	return transition, trace.EventID(transition, "run")
}

// parkedID is the transition that parks the round the operation runs: the
// k-th park of round n follows its k-1-th resumption.
func (in RoundInput) ParkedID() string {
	return fmt.Sprintf("shed-round-%d-waiting-%d", in.Round, in.Resume+1)
}

const CommitteeAgentPrefix = "agent_committee_"

func CommitteeAgent(i int) string { return CommitteeAgentPrefix + strconv.Itoa(i) }

func CommitteeThread(i int) string { return "thread_committee_" + strconv.Itoa(i) }

func RoundIDs(n int) (transition, event string) {
	transition = fmt.Sprintf("shed-round-%d", n)
	return transition, trace.EventID(transition, "run")
}

func RoundTurnPrefix(n int, agent string) string { return fmt.Sprintf("shed-%d-%s-", n, agent) }

func RoundTurnID(n int, agent string, attempt int) string {
	return RoundTurnPrefix(n, agent) + strconv.Itoa(attempt)
}

func (in RoundInput) MemberPrefix(agent string) string {
	if in.Amendment != "" {
		return fmt.Sprintf("amend-%s-round-%d-%s-", in.Amendment, in.Round, agent)
	}
	return RoundTurnPrefix(in.Round, agent)
}

func (in RoundInput) MemberChain(t trace.Thread, asked []trace.QuestionState) map[string]string {
	return AskChain(t, in.MemberPrefix(t.Identity.ID), asked)
}

func (in RoundInput) MemberTurns(t trace.Thread, asked []trace.QuestionState) []trace.QueuedTurn {
	return ChainTurns(t, in.MemberChain(t, asked))
}

// shedStates are the kinds of shed-subject value, each followed by the round
// it is about.
var ShedStates = []string{"round", "waiting", "heard", "reply", "asked", "replied", "redraft", "redrafted", "concluded", "failed"}

// shedState splits a shed-subject value into its kind and round number.
func ShedState(value string) (kind string, n int, ok bool) {
	kind, number, found := strings.Cut(value, "-")
	n, err := strconv.Atoi(number)
	return kind, n, found && err == nil && n > 0 && slices.Contains(ShedStates, kind)
}

// debate is the shed controller. Its pass moves every sketched workstream
// into the shed with its committee, or without one when the owner skipped
// debate at hand-in, and derives the debate's next step from the trace: a
// round, the architect's reply to it, or the conclusion. Its
// reconciler runs each round operation: one turn per member, in parallel
// against the pinned revision, and the record of what each contributed.
type Debate struct {
	S          *DebateDependencies
	Repository *trace.Repository
}

var _ coreadapter.Reconciler = (*Debate)(nil)

// Pass reconciles every workstream of the trace except the librarian's.
func (d *Debate) Pass(ctx context.Context) error {
	streams, err := d.Repository.Workstreams()
	if err != nil {
		return err
	}
	librarian := LibrarianWorkstream(d.Repository.Project())
	for _, stream := range streams {
		if stream == librarian {
			continue
		}
		if session, err := IsSession(d.Repository, stream); err != nil {
			return fmt.Errorf("workstream %s shed: %w", stream, err)
		} else if session {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := d.Reconcile(ctx, stream); err != nil {
			return fmt.Errorf("workstream %s shed: %w", stream, err)
		}
	}
	return nil
}

// reconcile gives a sketched workstream its committee and moves it to
// in-shed, then takes the next step of a workstream in the shed. Every other
// feature state needs nothing. A service without a committee runner leaves a
// sketched workstream where it is, waiting for a service that has one, except
// one whose debate the owner skipped at hand-in: it needs no committee and
// enters the shed without one.
func (d *Debate) Reconcile(ctx context.Context, stream config.WorkstreamID) error {
	feature, err := d.Repository.Workflow(stream, trace.FeatureSubject)
	if err != nil {
		return err
	}
	if feature.Value != SketchedState && feature.Value != InShedState {
		return nil
	}
	if _, decided, err := d.Repository.GroupApprovalOf(stream); err != nil || decided {
		return err
	}
	if required, err := d.Repository.GroupApprovalRequired(stream); err != nil {
		return err
	} else if required {
		group, err := d.Repository.CrossRepoWorkgroupOf(stream)
		if err != nil {
			return err
		}
		if _, _, drafted, err := d.Repository.CrossRepoProjectSpec(group); err != nil || !drafted {
			return err
		}
	}
	skipped, err := SkippedDebate(d.Repository, stream)
	if err != nil {
		return err
	}
	pin, err := d.DraftPin(stream)
	if err != nil {
		return err
	}
	if err := d.S.Step("shed-draft-read"); err != nil {
		return err
	}
	// A skipped debate runs no committee turn, so the workstream needs
	// neither a committee nor a round; it waits in the shed for the owner to
	// ratify both documents, with the packet that asks them to.
	if skipped {
		if feature.Value == SketchedState {
			if err := d.EnterSkipped(ctx, stream, feature, pin); err != nil {
				return err
			}
		}
		return d.PresentPacket(ctx, stream, true)
	}
	if feature.Value == SketchedState {
		if d.S.Options.Committee == nil {
			return nil
		}
		if err := d.EnsureCommittee(ctx, stream, d.S.Current().Capacity.Committee); err != nil {
			return err
		}
		// The committee is the threads that exist, which is what every round
		// runs.
		members, err := d.Committee(stream)
		if err != nil {
			return err
		}
		reason := fmt.Sprintf("%s enter the shed with a committee of %d", pin, len(members))
		_, err = d.Repository.MoveFeatureState(ctx, d.Header(InShedState, stream, SketchedState, d.S.Now()), SketchedState, InShedState, reason)
		if errors.Is(err, trace.ErrConflict) {
			return nil
		}
		if err != nil {
			return err
		}
	}
	if err := d.Step(ctx, stream); err != nil {
		return err
	}
	// The packet is presented once the step of this pass concluded the
	// debate, and follows every owner action that changes what it says.
	return d.PresentPacket(ctx, stream, false)
}

// skippedAtHandIn reports whether t is the skip of debate a hand-in recorded.
func SkippedAtHandIn(t trace.Transition) bool {
	return t.ID == SkipTransition && t.Cause == HandInTransition
}

// enterSkipped moves a sketched workstream whose debate the owner skipped at
// hand-in into the shed without a committee, and asks the chief of staff to
// present the packet: a skip recorded before the workstream was sketched told
// the chief of staff nothing about the drafted documents. A skip through the
// shed API moves the workstream itself. A workstream that moved meanwhile is
// left to the next pass.
func (d *Debate) EnterSkipped(ctx context.Context, stream config.WorkstreamID, feature trace.WorkflowState, pin shed.Pin) error {
	transitions, err := trace.Read[trace.Transition](d.Repository, stream)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(transitions, SkippedAtHandIn) {
		return nil
	}
	entries, err := Dissent(d.Repository, stream)
	if err != nil {
		return err
	}
	h := d.Header(InShedState, stream, SkipTransition, d.S.Now())
	reason := fmt.Sprintf("%s enter the shed without a committee: the owner skipped debate", pin)
	body := fmt.Sprintf("Workstream state changed from %s to %s: %s.\n%s", SketchedState, InShedState, reason, Presentation(shed.Recommend(entries))+DecideFirst(entries))
	_, err = d.Repository.Transact(ctx, trace.Transaction{ExpectedVersion: feature.Version,
		Transition: trace.Transition{Header: h, Subject: trace.FeatureSubject, From: SketchedState, To: InShedState, Reason: reason},
		Events:     []trace.Event{trace.Notice(h.ID, "state", body)}})
	if errors.Is(err, trace.ErrConflict) {
		return nil
	}
	return err
}

// step derives what the debate needs next from the shed state and the
// recorded rounds. No round yet: round 1. A round parked on a member's
// question resumes once every member that asked has its answer queued, and a
// reply or redraft parked on the architect's question once its answer is. A
// heard round with no open dissent concludes the debate by consensus; one
// with open dissent gets the architect's reply. After the reply, the next
// round runs against the latest revision unless shed.max_rounds rounds have
// run, which concludes the debate with its dissent open. A conclusion the
// owner followed with a request for a redraft gets that redraft, and the round
// after it debates what the architect wrote. A round, a reply or a redraft in
// progress, a parked round or answer whose answers are not all queued, a concluded
// debate the owner has not answered and a failed round need nothing. A round
// waits for a service that can run the committee and a reply or redraft for
// one that can run the architect; concluding runs no turn and waits for
// neither.
func (d *Debate) Step(ctx context.Context, stream config.WorkstreamID) error {
	state, err := d.Repository.Workflow(stream, ShedSubject)
	if err != nil {
		return err
	}
	// A reply commits its redraft before moving the shed to replied. Read
	// the revision after the state so a completed reply's redraft is visible
	// when deciding whether another round is needed. A later shed transition
	// conflicts with this state's version instead of using a stale decision.
	latest, err := d.DraftPin(stream)
	if err != nil {
		return err
	}
	if state.Value == "" {
		if d.S.Options.Committee == nil {
			return nil
		}
		return d.Request(ctx, stream, state, RoundInput{Round: 1, Spec: latest.Spec, Plan: latest.Plan}, InShedState)
	}
	kind, n, ok := ShedState(state.Value)
	if !ok || kind != "waiting" && kind != "asked" && kind != "heard" && kind != "replied" && kind != "concluded" && kind != "redrafted" {
		return nil
	}
	if kind == "waiting" {
		if d.S.Options.Committee == nil {
			return nil
		}
		return d.Resume(ctx, stream, state, n)
	}
	if kind == "asked" {
		if d.S.Options.Architect == nil {
			return nil
		}
		return d.ResumeAnswer(ctx, stream, state, n)
	}
	requests, err := shed.Requests(d.Repository, stream)
	if err != nil {
		return err
	}
	redrafts, err := shed.Redrafts(d.Repository, stream)
	if err != nil {
		return err
	}
	configured := d.S.Current().Shed.MaxRounds
	limit := shed.Limit(configured, requests, redrafts)
	if kind == "concluded" {
		// A redraft the owner asked for after this conclusion comes before
		// the round that debates it: recording it moves the shed to
		// redrafted-<n>, where that round is requested.
		if slices.ContainsFunc(redrafts, func(r shed.Redraft) bool { return r.Round == n }) {
			if d.S.Options.Architect == nil {
				return nil
			}
			asked, err := d.Asked(stream, n)
			if err != nil {
				return err
			}
			return d.RequestRedraft(ctx, stream, state, RoundInput{Round: n, Spec: latest.Spec, Plan: latest.Plan, Redraft: true}, asked.Note)
		}
		// Debate otherwise resumes only where the owner asked for further
		// rounds after this conclusion; the cap bounds how many of them run.
		if n >= limit || !slices.ContainsFunc(requests, func(m shed.More) bool { return m.Round == n }) {
			return nil
		}
		if d.S.Options.Committee == nil {
			return nil
		}
		return d.Request(ctx, stream, state, RoundInput{Round: n + 1, Spec: latest.Spec, Plan: latest.Plan}, shed.MoreDocumentID(n))
	}
	if kind == "redrafted" {
		// The owner asked for the redraft to be debated: the round runs
		// whether the redraft changed anything or not.
		if d.S.Options.Committee == nil {
			return nil
		}
		id, _ := RoundInput{Round: n, Redraft: true}.Ids()
		return d.Request(ctx, stream, state, RoundInput{Round: n + 1, Spec: latest.Spec, Plan: latest.Plan}, id+"-redrafted")
	}
	records, err := shed.Records(d.Repository, stream)
	if err != nil {
		return err
	}
	open, err := DissentRecord(d.Repository, stream, records)
	if err != nil {
		return err
	}
	redrafted := slices.ContainsFunc(records, func(r shed.Record) bool { return r.Round == n && r.Revision != latest })
	round, _ := RoundIDs(n)
	switch {
	case len(shed.Blocked(open)) == 0 && kind != "heard" && !redrafted:
		return d.Conclude(ctx, stream, state, n, round+"-heard", Unopposed(records, open, n), open)
	case kind == "heard" && len(shed.Standing(open)) == 0:
		return d.Conclude(ctx, stream, state, n, round+"-heard", Unopposed(records, open, n), open)
	case kind == "heard":
		// The reply waits for a service that can run the architect.
		if d.S.Options.Architect == nil {
			return nil
		}
		// Every record of a round carries the revision the round was pinned to.
		i := slices.IndexFunc(records, func(r shed.Record) bool { return r.Round == n })
		if i < 0 {
			return fmt.Errorf("round %d was heard and has no record", n)
		}
		return d.RequestReply(ctx, stream, state, RoundInput{Round: n, Spec: records[i].Revision.Spec, Plan: records[i].Revision.Plan}, len(open))
	case n >= limit:
		reply, _ := ReplyIDs(n)
		return d.Conclude(ctx, stream, state, n, reply+"-replied", fmt.Sprintf("debate stopped after round %d, %s, with %s; the cap approves nothing", n, Bound(configured, requests, redrafts, limit), Standing(open)), open)
	}
	if d.S.Options.Committee == nil {
		return nil
	}
	reply, _ := ReplyIDs(n)
	return d.Request(ctx, stream, state, RoundInput{Round: n + 1, Spec: latest.Spec, Plan: latest.Plan}, reply+"-replied")
}

// bound names what stopped the debate at its last round: the configured cap,
// or the rounds the owner asked for once they have, which replace it. A
// redraft asks for the one round that debates it.
func Bound(configured int, requests []shed.More, redrafts []shed.Redraft, limit int) string {
	switch {
	case len(requests) == 0 && len(redrafts) == 0:
		return fmt.Sprintf("at the shed.max_rounds cap of %d", configured)
	case slices.ContainsFunc(requests, func(m shed.More) bool { return m.Round+m.Rounds == limit }):
		return fmt.Sprintf("at round %d, the last of the further rounds the owner asked for", limit)
	}
	return fmt.Sprintf("at round %d, the round that debated the redraft the owner asked for", limit)
}

// presentation asks the chief of staff to put the decision the packet holds to
// the owner. It is the attention item of a workstream waiting at the gate.
func Presentation(recommendation string) string {
	return "The ratification packet is recorded for the owner: the spec and plan revisions, the dissent record and the recommendation to " + recommendation +
		".\nPresent it, and make the attention of your status the decision the owner has to take now: ratify the revisions, overrule or sustain what blocks, ask for a redraft or for further rounds, or abandon the workstream."
}

// decideFirst asks the chief of staff to decide the committee objections that
// stand before it presents the packet, or is empty when none waits for it.
func DecideFirst(entries []shed.Entry) string {
	undecided := shed.Undecided(entries)
	if len(undecided) == 0 {
		return ""
	}
	return fmt.Sprintf("\nBefore you present it, decide %s with resolve_objection: %s. The owner's inbox lists the ratification once each one is decided, with only the objections you escalate.",
		shed.Objections(len(undecided)), EntryIDs(undecided))
}

// unopposed is why a debate with nothing standing concludes after round n. It
// is consensus only among the members whose turns ended normally: a round in
// which every turn failed reviewed nothing, and the reason says so. Dissent
// the owner disposed of is not agreement either.
func Unopposed(records []shed.Record, open []shed.Entry, n int) string {
	if len(open) > 0 && len(shed.Standing(open)) == 0 {
		if len(shed.ForOwner(open)) < len(open) {
			return fmt.Sprintf("debate concluded after round %d: every objection that stood was disposed of, by the owner or by the chief of staff on the owner's behalf", n)
		}
		return fmt.Sprintf("debate concluded after round %d: the owner disposed of every objection that stood", n)
	}
	if len(open) > 0 {
		return fmt.Sprintf("debate concluded after round %d: no charter or owner objection blocks the proposal; engineering advice remains in the record", n)
	}
	members, failed := 0, 0
	for _, r := range records {
		if r.Round == n && !r.Owned() {
			members++
			if r.Failure != "" {
				failed++
			}
		}
	}
	switch {
	case failed == members:
		return fmt.Sprintf("debate concluded after round %d without a review: the turns of all %d members failed, so no objection stands and nobody agreed", n, members)
	case failed > 0:
		return fmt.Sprintf("debate concluded by consensus after round %d: no objection stands; the turns of %d of %d members failed", n, failed, members)
	}
	return fmt.Sprintf("debate concluded by consensus after round %d: no objection stands", n)
}

// Dissent returns the workstream's dissent record, computed from the recorded
// rounds, the owner's rulings and the chief of staff's decisions: every
// objection that stands, with its kind, its member, the part it names, how it
// was disposed of or escalated and whether it blocks.
func Dissent(repository *trace.Repository, stream config.WorkstreamID) ([]shed.Entry, error) {
	records, err := shed.Records(repository, stream)
	if err != nil {
		return nil, err
	}
	return DissentRecord(repository, stream, records)
}

// dissentRecord is the dissent record of the given records under the owner's
// rulings and the chief of staff's decisions on the workstream.
func DissentRecord(repository *trace.Repository, stream config.WorkstreamID, records []shed.Record) ([]shed.Entry, error) {
	rulings, err := shed.AllRulings(repository, stream)
	if err != nil {
		return nil, err
	}
	chief, err := shed.AllChiefDecisions(repository, stream)
	if err != nil {
		return nil, err
	}
	return shed.DissentRecord(records, rulings, chief), nil
}

// standing counts the open dissent and how much of it blocks.
func Standing(open []shed.Entry) string {
	blocking := 0
	for _, e := range open {
		if e.Blocking {
			blocking++
		}
	}
	return fmt.Sprintf("%s standing, %d of them blocking", shed.Objections(len(open)), blocking)
}

// conclude ends the debate after round n. The workstream stays in the shed:
// the conclusion is a shed transition, and its notice gives the chief of
// staff the reason, the dissent that stands and the packet to present to the
// owner.
func (d *Debate) Conclude(ctx context.Context, stream config.WorkstreamID, state trace.WorkflowState, n int, cause, reason string, open []shed.Entry) error {
	id := fmt.Sprintf("shed-concluded-%d", n)
	var body strings.Builder
	fmt.Fprintf(&body, "Debate concluded: %s. The workstream stays %s until the owner decides.", reason, InShedState)
	if len(open) > 0 {
		body.WriteString("\nOpen dissent:")
	}
	for _, e := range open {
		weight := "advisory"
		if e.Blocking {
			weight = "blocking"
		}
		if e.Disposition != "" {
			weight = string(e.Disposition) + ", " + weight
		}
		// The owner's own objection names no part.
		about := " on " + e.Part
		if e.Part == "" {
			about = ""
		}
		fmt.Fprintf(&body, "\n- %s (%s, %s, by %s in round %d%s, against %s): %s", e.ID, e.Kind, weight, e.Member, e.Round, about, e.Revision, e.Argument)
	}
	body.WriteString("\n" + Presentation(shed.Recommend(open)) + DecideFirst(open))
	tx := trace.Transaction{ExpectedVersion: state.Version,
		Transition: trace.Transition{Header: d.Header(id, stream, cause, d.S.Now()), Subject: ShedSubject, From: state.Value, To: fmt.Sprintf("concluded-%d", n), Reason: reason},
		Events:     []trace.Event{trace.Notice(id, "concluded", body.String())}}
	_, err := d.Repository.Transact(ctx, tx)
	return err
}

func (d *Debate) Header(id string, stream config.WorkstreamID, cause string, at time.Time) trace.Header {
	return trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: d.Repository.Project(), Workstream: stream, At: at, Actor: ShedActor, Cause: cause}
}

// latestPin returns the latest recorded revisions of the spec and the plan.
func LatestPin(repository *trace.Repository, stream config.WorkstreamID) (shed.Pin, error) {
	docs, err := trace.Read[trace.Document](repository, stream)
	if err != nil {
		return shed.Pin{}, err
	}
	var pin shed.Pin
	for _, doc := range docs {
		switch doc.ID {
		case plan.SpecDocument:
			pin.Spec = doc.Revision
		case plan.PlanDocument:
			pin.Plan = doc.Revision
		}
	}
	if pin.Spec == 0 || pin.Plan == 0 {
		return pin, fmt.Errorf("workstream %s has no recorded spec and plan", stream)
	}
	return pin, nil
}

// ensureCommittee creates the committee threads the workstream is missing, up
// to the given number of members.
func (d *Debate) EnsureCommittee(ctx context.Context, stream config.WorkstreamID, members int) error {
	for i := 1; i <= members; i++ {
		_, err := d.Repository.Thread(stream, CommitteeAgent(i))
		if err == nil {
			continue
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		identity := trace.Agent{Header: trace.Header{Schema: "osmia.trace.agent", Version: trace.Version, ID: CommitteeAgent(i), Revision: 1, Project: d.Repository.Project(), Workstream: stream, At: d.S.Now(), Actor: ShedActor, Cause: "committee-thread"}, Role: CommitteeRole, ThreadID: CommitteeThread(i)}
		if err := d.Repository.CreateThread(ctx, identity); err != nil {
			return err
		}
	}
	return nil
}

// committee returns the workstream's committee members, the agents of its
// committee threads, in order.
func (d *Debate) Committee(stream config.WorkstreamID) ([]string, error) {
	threads, err := d.Repository.Threads(stream)
	if err != nil {
		return nil, err
	}
	var members []string
	for _, t := range threads {
		if t.Identity.Role == CommitteeRole {
			members = append(members, t.Identity.ID)
		}
	}
	slices.SortFunc(members, func(a, b string) int {
		if len(a) != len(b) {
			return len(a) - len(b)
		}
		return strings.Compare(a, b)
	})
	if len(members) == 0 {
		return nil, fmt.Errorf("workstream %s has no committee", stream)
	}
	return members, nil
}

// request publishes a round, or the resumption of a parked one, as a durable
// operation.
func (d *Debate) Request(ctx context.Context, stream config.WorkstreamID, state trace.WorkflowState, in RoundInput, cause string) error {
	// A paused workstream is asked for nothing until the pause is lifted.
	if _, paused := d.S.Pausing(d.Repository.Project(), stream); paused {
		return nil
	}
	transition, event := in.RunIDs()
	input, err := EncodeRound(in)
	if err != nil {
		return err
	}
	op := coreadapter.Operation{ID: trace.OperationID(d.Repository.Project(), stream, event), Boundary: coreadapter.RunnerBoundary, Action: RoundAction, Input: input}
	reason := fmt.Sprintf("the committee is asked for round %d against %s", in.Round, in.Pin())
	body := fmt.Sprintf("Committee round %d against %s", in.Round, in.Pin())
	if in.Resume > 0 {
		reason = fmt.Sprintf("round %d against %s resumes: every member that asked has its answer", in.Round, in.Pin())
		body = fmt.Sprintf("Committee round %d against %s, resumed", in.Round, in.Pin())
	}
	tx := trace.Transaction{ExpectedVersion: state.Version,
		Transition: trace.Transition{Header: d.Header(transition, stream, cause, d.S.Now()), Subject: ShedSubject, From: state.Value, To: fmt.Sprintf("round-%d", in.Round), Reason: reason},
		Events:     []trace.Event{{ID: event, Kind: "shed-round", Body: body, Operation: &op}}}
	_, err = d.Repository.Transact(ctx, tx)
	return err
}

// resume requests round n again once every member whose last turn of the
// round asked a question has its answer queued on its thread. The resumption
// is pinned to the round's own revision and caused by the transition that
// parked it. A member still waiting parks the round as it is.
func (d *Debate) Resume(ctx context.Context, stream config.WorkstreamID, state trace.WorkflowState, n int) error {
	members, err := d.Committee(stream)
	if err != nil {
		return err
	}
	asked, err := d.Repository.Questions(stream)
	if err != nil {
		return err
	}
	for _, member := range members {
		t, err := d.Repository.Thread(stream, member)
		if err != nil {
			return err
		}
		if turns := RoundTurns(t, n, asked); len(turns) > 0 && AskedBy(asked, t.Identity.ThreadID, turns[len(turns)-1].Request.TurnID) != "" {
			return nil
		}
	}
	in, err := d.Requested(stream, n)
	if err != nil {
		return err
	}
	parks, err := d.Parks(stream, n)
	if err != nil {
		return err
	}
	if len(parks) == 0 {
		return fmt.Errorf("round %d is parked without a parking transition", n)
	}
	in.Resume = len(parks)
	return d.Request(ctx, stream, state, in, parks[len(parks)-1].ID)
}

// requested returns the input round n was requested with: its pinned revision.
func (d *Debate) Requested(stream config.WorkstreamID, n int) (RoundInput, error) {
	ops, err := d.Repository.Operations(stream)
	if err != nil {
		return RoundInput{}, err
	}
	_, event := RoundIDs(n)
	id := trace.OperationID(d.Repository.Project(), stream, event)
	for _, o := range ops {
		if o.Operation.ID == id {
			return DecodeRound(o.Operation)
		}
	}
	return RoundInput{}, fmt.Errorf("round %d has no recorded operation", n)
}

// parks returns the transitions that parked round n, in order.
func (d *Debate) Parks(stream config.WorkstreamID, n int) ([]trace.Transition, error) {
	transitions, err := trace.Read[trace.Transition](d.Repository, stream)
	if err != nil {
		return nil, err
	}
	round, _ := RoundIDs(n)
	var out []trace.Transition
	for _, t := range transitions {
		if t.Subject == ShedSubject && strings.HasPrefix(t.ID, round+"-waiting-") {
			out = append(out, t)
		}
	}
	return out, nil
}

func EncodeRound(in RoundInput) (json.RawMessage, error) { return json.Marshal(in) }

func DecodeRound(op coreadapter.Operation) (RoundInput, error) { return DecodeShed(op, RoundAction) }

// decodeShed reads the input of a shed operation of the given action: a round
// and the revision it is pinned to.
func DecodeShed(op coreadapter.Operation, action string) (RoundInput, error) {
	var in RoundInput
	if op.Boundary != coreadapter.RunnerBoundary || op.Action != action {
		return in, fmt.Errorf("unsupported runner operation %q", op.Action)
	}
	dec := json.NewDecoder(bytes.NewReader(op.Input))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return in, fmt.Errorf("invalid %s operation input: %w", action, err)
	}
	if in.Round < 1 || in.Spec < 1 || in.Plan < 1 {
		return in, fmt.Errorf("%s operation requires a positive round and pinned revisions", action)
	}
	if in.Resume < 0 {
		return in, fmt.Errorf("%s operation input carries an invalid resumption number", action)
	}
	return in, nil
}

// stream returns the workstream a round operation belongs to: the one whose
// run event derives the operation ID.
func (d *Debate) Stream(op coreadapter.Operation, in RoundInput) (config.WorkstreamID, error) {
	_, event := in.RunIDs()
	return d.Owner(op, event)
}

// owner returns the workstream whose event derives the operation ID.
func (d *Debate) Owner(op coreadapter.Operation, event string) (config.WorkstreamID, error) {
	streams, err := d.Repository.Workstreams()
	if err != nil {
		return "", err
	}
	for _, stream := range streams {
		if trace.OperationID(d.Repository.Project(), stream, event) == op.ID {
			return stream, nil
		}
	}
	return "", fmt.Errorf("%s operation %s belongs to no workstream", op.Action, op.ID)
}

// outcome returns the recorded terminal result of the operation that runs
// round n: succeeded once the committee was heard, failed when the round's
// failed transition is recorded, waiting when the operation parked the round
// on a member's question, nil before any of them.
func (d *Debate) Outcome(stream config.WorkstreamID, n int, operation string) (*coreadapter.OperationResult, error) {
	transitions, err := trace.Read[trace.Transition](d.Repository, stream)
	if err != nil {
		return nil, err
	}
	round, _ := RoundIDs(n)
	for _, t := range transitions {
		switch {
		case t.Subject != ShedSubject:
		case t.ID == round+"-heard":
			return &coreadapter.OperationResult{Outcome: "succeeded", Evidence: t.Reason}, nil
		case t.ID == round+"-failed":
			return &coreadapter.OperationResult{Outcome: "failed", Evidence: t.Reason}, nil
		case strings.HasPrefix(t.ID, round+"-waiting-") && t.Cause == operation:
			return &coreadapter.OperationResult{Outcome: questions.Waiting, Evidence: t.Reason}, nil
		}
	}
	return nil, nil
}

// roundChain maps every turn of the member's round n to the attempt it
// continues.
func RoundChain(t trace.Thread, n int, asked []trace.QuestionState) map[string]string {
	return AskChain(t, RoundTurnPrefix(n, t.Identity.ID), asked)
}

// roundTurns returns the member's turns of round n in thread order: its
// attempts and the turns that answered their questions.
func RoundTurns(t trace.Thread, n int, asked []trace.QuestionState) []trace.QueuedTurn {
	return ChainTurns(t, RoundChain(t, n, asked))
}

// Inspect reads the recorded transitions and the committee threads. A
// recorded outcome completes the operation; a member turn this session is
// running is unknown; everything else is absent, and Apply decides between
// running, retrying and recording.
func (d *Debate) Inspect(_ context.Context, op coreadapter.Operation) (coreadapter.Observation, error) {
	in, err := DecodeRound(op)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	stream, err := d.Stream(op, in)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	result, err := d.Outcome(stream, in.Round, op.ID)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	if result != nil {
		return coreadapter.Observation{State: coreadapter.EffectCompleted, Evidence: "round " + result.Outcome, Result: result}, nil
	}
	members, err := d.Committee(stream)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	asked, err := d.Repository.Questions(stream)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	for _, member := range members {
		t, err := d.Repository.Thread(stream, member)
		if err != nil {
			return coreadapter.Observation{}, err
		}
		turns := RoundTurns(t, in.Round, asked)
		if len(turns) == 0 {
			continue
		}
		if last := turns[len(turns)-1]; last.Claim != nil && last.Response == nil && t.Status != "interrupted" {
			return coreadapter.Observation{State: coreadapter.EffectUnknown, Evidence: "committee turn " + last.Request.TurnID + " is running"}, nil
		}
	}
	return coreadapter.Observation{State: coreadapter.EffectAbsent, Evidence: fmt.Sprintf("round %d is not recorded", in.Round)}, nil
}

// errNoCommittee leaves a round pending in a service that cannot run the
// committee's turns, so no member's attempts are spent on the missing runner.
var ErrNoCommittee = errors.New("this service has no agent runner for the committee")

// Apply drives the round to a terminal result. Every member's turn runs at
// the same time against the pinned revision, without the trace's operation
// lock when the reconciler runs the round beside its other operations, so
// rounds of different workstreams overlap; once all have ended, the lock is
// taken back and each
// member's contributions are recorded as one file of the round, in one
// commit, and the shed moves to heard-<n>. A member whose turn failed is
// recorded with the failure and what it contributed before it. A member
// whose turn asked a question parks the round instead: the shed moves to
// waiting-<n>, the operation ends waiting, and the controller requests the
// round again once the answers are queued. Abandoning the workstream cancels
// the running turns and fails a round that has no record; a round whose files
// are committed is heard all the same. Storage errors and a missing committee
// runner leave the operation pending for another attempt, which finds the
// turns that already ended in the threads, as does a pause covering the
// workstream: a member's turn a hard pause stopped is continued within its
// attempt once the pause is lifted.
func (d *Debate) Apply(ctx context.Context, op coreadapter.Operation) (coreadapter.OperationResult, error) {
	in, err := DecodeRound(op)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	stream, err := d.Stream(op, in)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	if result, err := d.Outcome(stream, in.Round, op.ID); err != nil || result != nil {
		if err != nil {
			return coreadapter.OperationResult{}, err
		}
		return *result, nil
	}
	cfg := d.S.About(d.Repository)
	if !cfg.HasProject() || cfg.Project.ID != d.Repository.Project() {
		return coreadapter.OperationResult{}, errors.New("the project is not active")
	}
	members, err := d.Committee(stream)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	// Abandoning the workstream cancels the running turns, not the recording
	// of what they left.
	running, cancel := context.WithCancel(ctx)
	defer cancel()
	defer d.S.TrackTurn(stream, cancel)()
	records := make([]shed.Record, len(members))
	waiting := make([]string, len(members))
	failures := make([]error, len(members))
	var wg sync.WaitGroup
	for i, member := range members {
		wg.Add(1)
		go func() {
			defer wg.Done()
			records[i], waiting[i], failures[i] = d.Member(trace.Beside(ctx), trace.Beside(running), cfg, stream, op.ID, in, member)
		}()
	}
	wg.Wait()
	if err := errors.Join(failures...); err != nil {
		return coreadapter.OperationResult{}, err
	}
	var result coreadapter.OperationResult
	err = trace.Relock(ctx, func() error {
		result, err = d.Hear(ctx, op, stream, in, members, records, waiting)
		return err
	})
	return result, err
}

// hear ends the round once every member's turn has ended: it fails the round
// of an abandoned workstream, parks it on the members' questions or records
// what the members contributed.
func (d *Debate) Hear(ctx context.Context, op coreadapter.Operation, stream config.WorkstreamID, in RoundInput, members []string, records []shed.Record, waiting []string) (coreadapter.OperationResult, error) {
	recorded, err := shed.Records(d.Repository, stream)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	// A round whose files are committed was heard, whatever happened to the
	// workstream since: only a round without a record fails on abandonment.
	heard := slices.ContainsFunc(recorded, func(r shed.Record) bool { return r.Round == in.Round && !r.Owned() })
	if !heard {
		gone, err := Abandoned(d.Repository, stream)
		if err != nil {
			return coreadapter.OperationResult{}, err
		}
		if gone {
			return d.Terminal(ctx, op.ID, stream, in, "failed", fmt.Sprintf("round %d failed: the workstream was abandoned, so the committee is not heard", in.Round))
		}
		var parked []string
		for i, member := range members {
			if waiting[i] != "" {
				parked = append(parked, fmt.Sprintf("%s waits for the answer to question %s", member, waiting[i]))
			}
		}
		if len(parked) > 0 {
			return d.Terminal(ctx, op.ID, stream, in, questions.Waiting, fmt.Sprintf("round %d against %s is parked: %s", in.Round, in.Pin(), strings.Join(parked, "; ")))
		}
	}
	return d.Record(ctx, op.ID, stream, in, records, recorded)
}

// member drives one member's turn of the round to its end and returns what
// the member contributed, with the ID of the question the member waits on
// when its last turn asked one, whatever that turn ended with: a question is
// answered on the thread it was asked on, so a turn that asked and then
// failed or was interrupted waits for its answer like one that ended waiting.
// It abandons a turn a previous service stop interrupted, starts a new turn
// while attempts remain and runs the pending turn, the next attempt or the
// answer to the member's question, through the dispatcher.
func (d *Debate) Member(ctx, running context.Context, cfg *config.Config, stream config.WorkstreamID, operation string, in RoundInput, member string) (shed.Record, string, error) {
	empty := shed.Record{Version: shed.Version, Round: in.Round, Member: member, Revision: in.Pin()}
	for {
		if err := ctx.Err(); err != nil {
			return empty, "", err
		}
		t, err := d.Repository.Thread(stream, member)
		if err != nil {
			return empty, "", err
		}
		asked, err := d.Repository.Questions(stream)
		if err != nil {
			return empty, "", err
		}
		turns := in.MemberTurns(t, asked)
		tries := len(Attempts(turns, in.MemberChain(t, asked)))
		// Turns run in sequence, so the oldest unfinished turn of the chain is
		// the one to drive: the answer to a question may be queued behind the
		// turn that asked it while a service stop left that turn unfinished.
		var last, pending *trace.QueuedTurn
		if len(turns) > 0 {
			last = &turns[len(turns)-1]
		}
		if i := slices.IndexFunc(turns, func(q trace.QueuedTurn) bool { return q.CompletedAt.IsZero() }); i >= 0 {
			pending = &turns[i]
		}
		switch {
		case pending != nil && pending.Claim != nil && pending.Response == nil:
			if t.Status != "interrupted" {
				return empty, "", errors.New("committee turn " + pending.Request.TurnID + " is still running")
			}
			if err := d.Repository.AbandonTurn(ctx, stream, member, pending.Request.TurnID, d.S.Now()); err != nil {
				return empty, "", err
			}
		case pending != nil:
			gone, err := Abandoned(d.Repository, stream)
			if err != nil {
				return empty, "", err
			}
			if gone && pending.Response == nil {
				if _, err := d.Repository.CancelTurns(ctx, stream, d.S.Now(), AbandonActor, CancelReason); err != nil {
					return empty, "", err
				}
				continue
			}
			// Completing a captured turn runs no member.
			turnCtx := running
			if pending.Response != nil {
				turnCtx = ctx
			} else if d.S.Options.Committee == nil {
				return empty, "", ErrNoCommittee
			} else if err := d.S.Held(d.Repository, stream); err != nil {
				return empty, "", err
			}
			if _, err := d.Dispatch(turnCtx, stream, in, member, pending.Request.TurnID); err != nil {
				return empty, "", err
			}
			if err := os.RemoveAll(filepath.Join(d.TurnDirectory(stream, pending.Request.TurnID), "workspace")); err != nil {
				return empty, "", err
			}
		case last == nil || last.Status() == "interrupted":
			if last != nil {
				if id := AskedBy(asked, t.Identity.ThreadID, last.Request.TurnID); id != "" {
					record, err := d.Contributed(stream, in.MemberChain(t, asked)[last.Request.TurnID], empty)
					return record, id, err
				}
			}
			if gone, err := Abandoned(d.Repository, stream); err != nil || gone {
				empty.Failure = "the workstream was abandoned, so the member ran no turn"
				return empty, "", err
			}
			// A turn a hard pause stopped is continued within its attempt.
			if !StoppedTurn(last) && tries >= MaxRoundAttempts {
				empty.Failure = fmt.Sprintf("the member's turn was interrupted %d times by service stops", tries)
				return empty, "", nil
			}
			if d.S.Options.Committee == nil {
				return empty, "", ErrNoCommittee
			}
			if err := d.S.Held(d.Repository, stream); err != nil {
				return empty, "", err
			}
			if StoppedTurn(last) {
				if _, err := d.S.ContinueStopped(ctx, d.Repository, cfg, CommitteeRole, *last); err != nil {
					return empty, "", err
				}
				continue
			}
			if err := d.Enqueue(ctx, cfg, stream, in, member, tries+1, operation, asked); err != nil {
				return empty, "", err
			}
		default:
			record, err := d.Contributed(stream, in.MemberChain(t, asked)[last.Request.TurnID], empty)
			if err != nil {
				return empty, "", err
			}
			if id := AskedBy(asked, t.Identity.ThreadID, last.Request.TurnID); id != "" {
				return record, id, nil
			}
			if last.Status() != "idle" {
				record.Failure = "the turn ended with status " + last.Status()
				if last.Response != nil && last.Response.Failure != "" {
					record.Failure = last.Response.Failure
				}
			}
			return record, "", nil
		}
	}
}

// terminal ends the operation's run of the round with a shed transition:
// heard-<n> once the committee is recorded, failed-<n> with why it was not,
// or waiting-<n> with the questions the round waits on; and returns the
// matching result. The transition already recorded is returned as it is.
func (d *Debate) Terminal(ctx context.Context, operation string, stream config.WorkstreamID, in RoundInput, kind, reason string) (coreadapter.OperationResult, error) {
	outcome := coreadapter.OperationResult{Outcome: "failed", Evidence: reason}
	round, _ := RoundIDs(in.Round)
	id := round + "-" + kind
	switch kind {
	case "heard":
		outcome.Outcome = "succeeded"
	case questions.Waiting:
		outcome.Outcome, id = questions.Waiting, in.ParkedID()
	}
	state, err := d.Repository.Workflow(stream, ShedSubject)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	if state.Value != fmt.Sprintf("round-%d", in.Round) {
		result, err := d.Outcome(stream, in.Round, operation)
		if err != nil || result == nil {
			return coreadapter.OperationResult{}, errors.Join(err, fmt.Errorf("round %d is %q, not in progress", in.Round, state.Value))
		}
		return *result, nil
	}
	tx := trace.Transaction{ExpectedVersion: state.Version,
		Transition: trace.Transition{Header: d.Header(id, stream, operation, d.S.Now()), Subject: ShedSubject, From: state.Value, To: fmt.Sprintf("%s-%d", kind, in.Round), Reason: reason}}
	if _, err := d.Repository.Transact(ctx, tx); err != nil {
		return coreadapter.OperationResult{}, err
	}
	return outcome, nil
}

// enqueue accepts one member's turn of one round attempt, fixing its profile
// and prompts. The prompt of a later attempt repeats the answers the member
// received in the round, whose turns a service stop interrupted.
func (d *Debate) Enqueue(ctx context.Context, cfg *config.Config, stream config.WorkstreamID, in RoundInput, member string, attempt int, operation string, asked []trace.QuestionState) error {
	profile, err := d.S.AgentProfile(cfg, CommitteeRole, member)
	if err != nil {
		return err
	}
	t, err := d.Repository.Thread(stream, member)
	if err != nil {
		return err
	}
	earlier, err := d.EarlierRound(stream, in)
	if err != nil {
		return err
	}
	var standing []shed.Dissent
	for _, dissent := range shed.OpenDissent(earlier) {
		if dissent.Member == member {
			standing = append(standing, dissent)
		}
	}
	answers := Received(t, in.MemberChain(t, asked), asked)
	turn := in.MemberPrefix(member) + strconv.Itoa(attempt)
	req := trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_" + turn, Revision: 1, Project: d.Repository.Project(), Workstream: stream, At: d.S.Now(), Actor: ShedActor, Cause: operation, Depth: 1},
		AgentID: member, ThreadID: t.Identity.ThreadID, TurnID: turn, Profile: profile, SystemPrompt: AmendmentCommitteeSystemPrompt(cfg.Project, in) + "\n\n" + MemberPerspective(cfg, member), Prompt: AmendmentCommitteePrompt(in, standing, answers)}
	_, err = d.Repository.EnqueueTurn(ctx, req)
	return err
}

// earlier returns the workstream's records of the rounds before round n.
func (d *Debate) Earlier(stream config.WorkstreamID, n int) ([]shed.Record, error) {
	records, err := shed.Records(d.Repository, stream)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(records, func(r shed.Record) bool { return r.Round >= n }), nil
}

func (d *Debate) EarlierRound(stream config.WorkstreamID, in RoundInput) ([]shed.Record, error) {
	if in.Amendment != "" {
		records, err := AmendmentRecords(d.Repository, stream, in.Amendment)
		return slices.DeleteFunc(records, func(r shed.Record) bool { return r.Round >= in.Round }), err
	}
	return d.Earlier(stream, in.Round)
}

// turnDirectory is the service-owned directory of one committee turn: its
// staged view, its backend session and what the member contributed.
func (d *Debate) TurnDirectory(stream config.WorkstreamID, turn string) string {
	return filepath.Join(d.S.Current().Root.String(), "shed", string(d.Repository.Project()), string(stream), turn)
}

func (d *Debate) Contributions(stream config.WorkstreamID, turn string) string {
	return filepath.Join(d.TurnDirectory(stream, turn), "output", "contributions.json")
}

// contributed reads what the turn's tools kept. A turn that kept nothing was
// silent.
func (d *Debate) Contributed(stream config.WorkstreamID, turn string, empty shed.Record) (shed.Record, error) {
	empty.Turn = turn
	data, err := os.ReadFile(d.Contributions(stream, turn))
	if errors.Is(err, fs.ErrNotExist) {
		return empty, nil
	}
	if err != nil {
		return empty, err
	}
	record, err := shed.Parse(data)
	if err != nil {
		return empty, err
	}
	if record.Round != empty.Round || record.Member != empty.Member || record.Revision != empty.Revision || record.Turn != turn {
		return empty, fmt.Errorf("contributions of turn %s belong to another turn", turn)
	}
	return record, nil
}

// dispatch runs the turn through the thread dispatcher and runner, stopped by
// a hard pause covering the workstream.
func (d *Debate) Dispatch(ctx context.Context, stream config.WorkstreamID, in RoundInput, member, turn string) (coreadapter.OperationResult, error) {
	ctx, release := d.S.Stoppable(ctx, d.Repository.Project(), stream)
	defer release()
	dispatcher := thread.Dispatcher{Runner: d.S.ThreadRunner(d.S.About(d.Repository), d.Repository, &questions.Turns{Turns: d.Turns(stream, in), Repository: d.Repository}, d.S.Now), Prepare: func(_ context.Context, input thread.TurnInput) (coreadapter.PreparedTurn, error) {
		directory := filepath.Join(d.TurnDirectory(input.Workstream, input.Turn), "session")
		return coreadapter.PreparedTurn{SessionDirectory: directory}, os.MkdirAll(directory, 0700)
	}}
	op, err := thread.TurnOperation(d.Repository.Project(), "shed-turn", thread.TurnInput{Workstream: stream, Agent: member, Turn: turn})
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	return dispatcher.Apply(ctx, op)
}

// turns is a committee member's isolated turn path: a read-only private view
// of the owner's clone, the pinned spec and plan, the handed input, the
// charter, the context bundle and the earlier rounds, the file reading tool,
// the two contribution tools and the question tool, and no notes, write,
// execute, network or VCS capability. A turn that asks ends waiting, so its
// thread parks and its slot is free while the answer is found.
func (d *Debate) Turns(stream config.WorkstreamID, in RoundInput) *isolation.Turns {
	var engine coreadapter.Engine
	var hosts coreadapter.MCPHosts
	if c := d.S.Options.Committee; c != nil {
		engine, hosts = c.Engine, c.Hosts
	}
	turns := &isolation.Turns{
		Workspaces: StagedWorkspaces{},
		Views:      isolation.Views{Directory: filepath.Join(d.S.Current().Root.String(), "views")},
		Select: func(ctx context.Context, scope coreadapter.Scope) (isolation.Selection, error) {
			return d.SelectView(ctx, scope, in)
		},
		Grants: map[string]coreadapter.Capabilities{CommitteeRole: {Tools: []string{"file_read", shed.ObjectTool, shed.ConcedeTool, questions.AskTool}}},
		Scoped: func(_ context.Context, scope coreadapter.Scope) ([]coreadapter.Tool, error) {
			if scope.Workstream != string(stream) {
				return nil, errors.New("turn scope denied")
			}
			return d.Tools(scope, in)
		},
		Hosts:  hosts,
		Engine: engine,
	}
	return d.S.MemoryTurns(turns, d.S.About(d.Repository), d.Repository)
}

// tools binds the object, concede and ask tools to the claimed turn: the
// contributions validate against the pinned revision and are kept in the
// output directory of the attempt the turn belongs to, which the service
// records once the round ends. The turn that delivers the answer to a
// member's question continues the attempt that asked it: it holds what the
// member contributed before asking and numbers its objections after them.
func (d *Debate) Tools(scope coreadapter.Scope, in RoundInput) ([]coreadapter.Tool, error) {
	if scope.Role != CommitteeRole || scope.Project != string(d.Repository.Project()) {
		return nil, errors.New("turn scope denied")
	}
	stream := config.WorkstreamID(scope.Workstream)
	threads, err := d.Repository.Threads(stream)
	if err != nil {
		return nil, err
	}
	i := slices.IndexFunc(threads, func(t trace.Thread) bool {
		return t.Identity.Role == CommitteeRole && t.Identity.ThreadID == scope.Thread
	})
	if i < 0 {
		return nil, errors.New("turn scope denied")
	}
	asked, err := d.Repository.Questions(stream)
	if err != nil {
		return nil, err
	}
	origin, ok := in.MemberChain(threads[i], asked)[scope.Turn]
	if !ok {
		return nil, errors.New("turn scope denied")
	}
	member := threads[i].Identity.ID
	specDoc, planDoc, err := d.Pinned(stream, in)
	if err != nil {
		return nil, err
	}
	graph, err := plan.Parse([]byte(planDoc.Content))
	if err != nil {
		return nil, err
	}
	earlier, err := d.EarlierRound(stream, in)
	if err != nil {
		return nil, err
	}
	sofar, err := d.Contributed(stream, origin, shed.Record{Version: shed.Version, Round: in.Round, Member: member, Revision: in.Pin()})
	if err != nil {
		return nil, err
	}
	ask, err := questions.Tools(d.Repository, member, scope, d.S.Now)
	if err != nil {
		return nil, err
	}
	file := d.Contributions(stream, origin)
	contribute, err := shed.Tools(shed.Turn{Repository: d.Repository, Stream: stream, Spec: plan.ParseSpec(specDoc.Content), Plan: graph, Earlier: earlier, Now: d.S.Now,
		Record: sofar,
		Save: func(r shed.Record) error {
			data, err := shed.Encode(r)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
				return err
			}
			// A rename keeps the previous contributions whole if the write stops.
			if err := os.WriteFile(file+".tmp", data, 0600); err != nil {
				return err
			}
			return os.Rename(file+".tmp", file)
		}})
	if err != nil {
		return nil, err
	}
	return append(contribute, ask...), nil
}

// pinned returns the revisions of spec.md and plan.json the round is pinned to.
func (d *Debate) Pinned(stream config.WorkstreamID, in RoundInput) (spec, graph trace.Document, err error) {
	docs, err := trace.Read[trace.Document](d.Repository, stream)
	if err != nil {
		return spec, graph, err
	}
	for _, doc := range docs {
		if in.Amendment != "" {
			if doc.Path == "amendments/"+in.Amendment+"/spec.md" && doc.Revision == in.Spec {
				spec = doc
			}
			if doc.Path == "amendments/"+in.Amendment+"/plan.json" && doc.Revision == in.Plan {
				graph = doc
			}
			continue
		}
		switch {
		case doc.ID == plan.SpecDocument && doc.Revision == in.Spec:
			spec = doc
		case doc.ID == plan.PlanDocument && doc.Revision == in.Plan:
			graph = doc
		}
	}
	if spec.Revision == 0 || graph.Revision == 0 {
		return spec, graph, fmt.Errorf("workstream %s does not record %s", stream, in.Pin())
	}
	return spec, graph, nil
}

// selectView stages the member's view for the claimed turn and selects all of
// it, read-only.
func (d *Debate) SelectView(ctx context.Context, scope coreadapter.Scope, in RoundInput) (isolation.Selection, error) {
	cfg := d.S.About(d.Repository)
	if scope.Role != CommitteeRole || scope.Project != string(d.Repository.Project()) || !cfg.HasProject() || cfg.Project.ID != d.Repository.Project() {
		return isolation.Selection{}, errors.New("view selection denied")
	}
	stream, err := config.ParseWorkstreamID(scope.Workstream)
	if err != nil {
		return isolation.Selection{}, err
	}
	_, settings, err := d.S.RoleExecution(cfg, CommitteeRole)
	if err != nil {
		return isolation.Selection{}, err
	}
	if err := os.MkdirAll(filepath.Join(cfg.Root.String(), "views"), 0700); err != nil {
		return isolation.Selection{}, err
	}
	workspace := filepath.Join(d.TurnDirectory(stream, scope.Turn), "workspace")
	paths, err := d.Stage(ctx, cfg.Project.Clone, stream, in, workspace)
	if err != nil {
		return isolation.Selection{}, err
	}
	return isolation.Selection{Workspace: coreadapter.WorkspaceRequest{SourceDirectory: workspace, Directory: workspace}, Paths: paths, Execution: settings}, nil
}

// stage builds the view: the clone's tracked files under repo/, the pinned
// spec.md and plan.json, the handed input under handed/, charter.md, the
// rendered context bundle as context.md and the records of the earlier rounds
// under shed/. It returns the paths to select.
func (d *Debate) Stage(ctx context.Context, clone string, stream config.WorkstreamID, in RoundInput, workspace string) ([]string, error) {
	if err := os.RemoveAll(workspace); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(workspace, "repo"), 0700); err != nil {
		return nil, err
	}
	if err := CopyTracked(ctx, clone, filepath.Join(workspace, "repo")); err != nil {
		return nil, err
	}
	specDoc, planDoc, err := d.Pinned(stream, in)
	if err != nil {
		return nil, err
	}
	charter, err := d.Repository.Charter(ctx)
	if err != nil {
		return nil, err
	}
	b, err := d.S.ContextFor(d.Repository).Assemble(ctx, d.Repository.Project(), bundle.Scope{Role: "committee", Workstream: stream})
	if err != nil {
		return nil, err
	}
	files := map[string]string{plan.SpecPath: specDoc.Content, plan.PlanPath: planDoc.Content, "charter.md": charter.Content, "context.md": b.Brief()}
	paths := []string{"repo", plan.SpecPath, plan.PlanPath, "charter.md", "context.md"}
	docs, err := trace.Read[trace.Document](d.Repository, stream)
	if err != nil {
		return nil, err
	}
	for _, doc := range docs {
		if directory, _, _ := strings.Cut(doc.Path, "/"); directory == "handed" || directory == "shed" {
			files[doc.Path] = doc.Content
			if !slices.Contains(paths, directory) {
				paths = append(paths, directory)
			}
		}
	}
	if in.Amendment != "" {
		requests, err := trace.Read[trace.Amendment](d.Repository, stream)
		if err != nil {
			return nil, err
		}
		for _, request := range requests {
			if request.ID == in.Amendment {
				data, err := json.MarshalIndent(request, "", "  ")
				if err != nil {
					return nil, err
				}
				files["request.json"] = string(data)
				paths = append(paths, "request.json")
			}
		}
		for _, doc := range docs {
			if doc.Path == "amendments/"+in.Amendment+"/affected.json" {
				files["affected.json"] = doc.Content
				paths = append(paths, "affected.json")
			}
		}
	}
	for name, content := range files {
		path := filepath.Join(workspace, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			return nil, err
		}
	}
	return paths, nil
}

// record commits one file per member under shed/round-<n>/, each authored by
// its member and caused by the operation, then moves the shed to heard-<n>.
// recorded is what the workstream's shed already holds: a round with files in
// it only moves.
func (d *Debate) Record(ctx context.Context, operation string, stream config.WorkstreamID, in RoundInput, records, recorded []shed.Record) (coreadapter.OperationResult, error) {
	if !slices.ContainsFunc(recorded, func(r shed.Record) bool { return r.Round == in.Round && !r.Owned() }) {
		at := d.S.Now()
		var docs []trace.Document
		for _, r := range records {
			data, err := shed.Encode(r)
			if err != nil {
				return coreadapter.OperationResult{}, err
			}
			docs = append(docs, trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: shed.DocumentID(r.Round, r.Member), Revision: 1, Project: d.Repository.Project(), Workstream: stream, At: at, Actor: trace.Actor{Kind: "agent", ID: r.Member}, Cause: operation, Depth: 1},
				Path: shed.Path(r.Round, r.Member), Content: string(data)})
		}
		if err := d.Repository.RecordDocuments(ctx, docs); err != nil {
			return coreadapter.OperationResult{}, err
		}
		var err error
		if recorded, err = shed.Records(d.Repository, stream); err != nil {
			return coreadapter.OperationResult{}, err
		}
	}
	var objections, concessions, failed int
	for _, r := range recorded {
		if r.Round == in.Round && !r.Owned() {
			objections, concessions = objections+len(r.Objections), concessions+len(r.Concessions)
			if r.Failure != "" {
				failed++
			}
		}
	}
	reason := fmt.Sprintf("round %d against %s: %d members heard, %d objections, %d concessions, %d failed turns; %d objections stand", in.Round, in.Pin(), len(records), objections, concessions, failed, len(shed.OpenDissent(recorded)))
	return d.Terminal(ctx, operation, stream, in, "heard", reason)
}

func CommitteeSystemPrompt(p config.Project) string {
	return fmt.Sprintf("You are a member of the committee that debates one workstream's feature spec and plan for the %s project (%s) before anything is built. You test the draft against the project's charter, the owner's handed design and the decisions the knowledge base holds. You read; you hold no tool that writes, runs or fetches. You contribute only through %s and %s, and every objection cites what it rests on. When your view does not settle something you must know to judge the revision, call %s: the round waits for the answer, which arrives as your next turn, and your contributions so far are kept.", p.Name, p.Upstream, shed.ObjectTool, shed.ConcedeTool, questions.AskTool)
}

func CommitteePrompt(in RoundInput, standing []shed.Dissent, answers []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, `Round %d of the shed. Every member reads the same revision: %s.

Your view holds:
- spec.md and plan.json: the revision under debate. Cite a criterion as spec#<n> and a unit as plan#<unit>.
- handed/: what the owner handed in, unchanged. It says what to build.
- charter.md: the owner's rules for contributing to this project. Cite them as charter#<n>. Context labels all workgroup member charters by project ID. Each governs its member's slice, plan and units; check the workgroup spec and contracts against all of them. Cite another member as <project-id>/charter#<n>. Use kind charter_conflict and cite both rules when member charters conflict: only the owner resolves it.
- context.md: the project context bundle: the charter's rules, the knowledge-base prose per subsystem, the entity map and the recorded decisions. Cite a knowledge-base file as kb/<subsystem>.md and an entity as %s<entity>.
- repo/: the tracked files of the owner's clone, read-only.
- shed/round-<n>/: what every member contributed in earlier rounds, and the architect's reply to each as reply.json, when there are any.

Apply two tests and one judgement, and call %s once for each thing you find:
- charter: a part that violates a charter rule. This is a veto on that part; cite the rule.
- fit: the plan does not realise the handed design, or works against a decision the knowledge base holds. This is advice to the owner.
- size: a unit whose practical capacity or reviewability would improve enough from splitting to justify coordination; this is advice. Name the unit as the part.
- acceptance: unclear outcomes, missing ownership boundaries or insufficient verification; this is engineering advice. Name the unit as the part.

The plan is a revisable engineering approach. Challenge material risks and ownership gaps. Size and acceptance suggestions are advice, not owner gates. Prefer coherent outcomes and split only when coordination buys useful parallelism, reviewability or practical capacity; breadth alone is not a reason. Requirements belong in the spec; implementation guidance remains adaptable.

An objection that is refused comes back with the reason; correct it and call again. Ending your turn without objecting or conceding says you have no new dissent on this revision, and that you accept it in place of any earlier revision you objected to. Call %s when something you must know to judge the revision is not in your view: the round waits for the answer, which arrives as your next turn.
`, in.Round, in.Pin(), shed.EntityCitation, shed.ObjectTool, questions.AskTool)
	if len(standing) > 0 {
		fmt.Fprintf(&b, "\nYour objections that still stand. Call %s for each one that the revision or the debate has settled:\n", shed.ConcedeTool)
		for _, s := range standing {
			fmt.Fprintf(&b, "- %s (%s, %s, made against %s): %s\n", s.ID, s.Kind, s.Part, s.Revision, s.Argument)
		}
	}
	b.WriteString(Answered("round", answers))
	return b.String()
}
