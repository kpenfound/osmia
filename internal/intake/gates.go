package intake

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/jev"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
)

// GateHold bounds how long a gate's card stays out of the owner's inbox while
// Jev's judgment of it is pending or its confident pick is not yet applied. A
// gate whose judgment cannot finish within it goes to the owner as it is.
const GateHold = 10 * time.Minute

// Bounds on what a gate's state shows Jev, so a request stays within its
// context.
const (
	maxGateSpec    = 24 << 10
	maxGatePlan    = 16 << 10
	maxGateCharter = 8 << 10
	maxGateText    = 2 << 10
)

// escalateOption is the outcome of a chief-of-staff decision that puts the
// subject to the owner. A binding pick of any other outcome that still ended
// at the owner's gate was one the service could not carry out.
const escalateOption = "escalate"

// GateDecision is what a gate's records show of its ordinary decision.
type GateDecision struct {
	// Recorded says the gate's ordinary decision is recorded, by the owner or
	// by Jev.
	Recorded bool
	// ByJev says Jev made the recorded decision.
	ByJev bool
	// Incomplete says only part of the decision is recorded: some of the
	// questions of an escalation are ruled and others are not.
	Incomplete bool
}

// Gate is one owner gate Jev may decide, derived from the records that show the
// item reached it. It is judged once, when it is reached, and its confident
// pick is applied through the owner command's path.
type Gate struct {
	Kind    jev.DecisionKind
	Project config.ProjectID
	// Stream is the workstream whose trace holds the gate's judgment.
	Stream config.WorkstreamID
	// Key is the occurrence key of the gate's subject.
	Key string
	// Label names the subject in logs.
	Label string
	// Cause is the record that made the item reach the gate.
	Cause   string
	Reached time.Time
	// Options are the options Jev may pick, only ones that need no
	// owner-written text.
	Options []jev.DecisionOption
	// State builds what the card or packet shows, and the records it rests on.
	State func(ctx context.Context) (string, []jev.Source, error)
	// Apply carries the pick out through the owner command's path, as Jev. A
	// refusal is the service refusing the transition the pick calls for.
	Apply func(ctx context.Context, res jev.Result) *APIError
	// Decision reads the gate's ordinary decision from the records.
	Decision func() (GateDecision, error)
}

// ID identifies the gate within the service.
func (g Gate) ID() string {
	return fmt.Sprintf("%s/%s/%s/%s", g.Project, g.Stream, g.Kind, g.Key)
}

// GateResult returns what Jev's judgment of the gate recorded, without asking.
func (s *Controller) GateResult(repository *trace.Repository, g Gate) (jev.Result, error) {
	if s.Jev == nil {
		return jev.Result{Status: jev.None}, nil
	}
	return s.Jev.Lookup(repository, g.Stream, g.Kind, g.Key)
}

// optionLabel is how a pick is named to the owner: an option the chief wrote
// by its text, the others by their name.
func optionLabel(options []jev.DecisionOption, pick string) string {
	for _, o := range options {
		if o.Name == pick && strings.HasPrefix(o.Name, "option-") {
			return o.Description
		}
	}
	return pick
}

// holdsForJev reports whether the card of a gate is held back from the owner,
// and when it is not, the recommendation Jev's result gives it and Jev's result
// as the card shows it. A card is held while Jev's judgment is pending, which
// is while Jev is configured and nothing is recorded, or while its binding pick
// waits to be applied. It is read-only: it never asks Jev and records nothing.
func (s *Controller) holdsForJev(repository *trace.Repository, g Gate) (held bool, rec string, view *api.InboxJev) {
	if s.Jev == nil {
		return false, "", nil
	}
	res, err := s.GateResult(repository, g)
	if err != nil {
		return false, "", nil
	}
	within := s.Now().Sub(g.Reached) < GateHold
	switch res.Status {
	case jev.None:
		return within && s.Jev.Status().Mode != jev.ModeUnconfigured, "", nil
	case jev.Binding:
		if res.Refused == "" {
			return within, "", nil
		}
		view = &api.InboxJev{Pick: res.Pick, Confidence: res.Confidence, Threshold: res.Threshold, Binding: true, Refused: res.Refused, Judgment: res.Judgment}
		return false, fmt.Sprintf("Jev decided %s with confidence %.2f, but the service refused it: %s. The decision is yours.", optionLabel(g.Options, res.Pick), res.Confidence, res.Refused), view
	case jev.Nonbinding:
		view = &api.InboxJev{Pick: res.Pick, Confidence: res.Confidence, Threshold: res.Threshold, Deferred: res.Deferred, Judgment: res.Judgment}
		if res.Deferred {
			return false, "Jev deferred this decision to you.", view
		}
		return false, fmt.Sprintf("Jev recommends %s with confidence %.2f, below the %.2f it needs to decide.", optionLabel(g.Options, res.Pick), res.Confidence, res.Threshold), view
	}
	return false, "", nil
}

// applyJevCard sets the recommendation and Jev's result of a card the owner
// sees after Jev's judgment. Jev's pick replaces the chief of staff's
// recommendation when it gave one; a deferral keeps the chief's, behind Jev's
// word.
func applyJevCard(e *InboxEntry, rec string, view *api.InboxJev) {
	if view == nil {
		return
	}
	e.Jev = view
	switch {
	case view.Deferred && strings.TrimSpace(e.Recommendation) != "":
		e.Recommendation = rec + " " + e.Recommendation
	default:
		e.Recommendation = rec
	}
	e.QuickReply = ""
}

// guardedCard applies the gate to a card: it reports whether the card shows,
// and decorates it with Jev's result when it does.
func (s *Controller) guardedCard(repository *trace.Repository, e *InboxEntry, g *Gate) bool {
	if g == nil {
		return true
	}
	held, rec, view := s.holdsForJev(repository, *g)
	if held {
		return false
	}
	applyJevCard(e, rec, view)
	return true
}

// CarriedJudgment returns the binding Jev judgment recorded for the subject by
// a decision of the chief of staff that was not carried out, when there is one:
// a binding pick of an outcome other than putting the subject to the owner,
// whose subject nevertheless reached the owner's gate. Such an item goes to the
// owner without a gate judgment, with Jev's judgment shown.
func (s *Controller) CarriedJudgment(repository *trace.Repository, stream config.WorkstreamID, kind jev.DecisionKind, key string) (jev.Result, bool) {
	if s.Jev == nil {
		return jev.Result{}, false
	}
	res, err := s.Jev.Lookup(repository, stream, kind, key)
	if err != nil || res.Status != jev.Binding || res.Pick == escalateOption {
		return jev.Result{}, false
	}
	return res, true
}

// CarriedCard decorates a card with the binding judgment its subject carries.
func CarriedCard(e *InboxEntry, res jev.Result, label string) {
	e.Jev = &api.InboxJev{Pick: res.Pick, Confidence: res.Confidence, Threshold: res.Threshold, Binding: true, Refused: "the service could not carry the decision out", Judgment: res.Judgment}
	e.Recommendation = fmt.Sprintf("Jev decided %s for %s with confidence %.2f, but the service could not carry it out. The decision is yours.", res.Pick, label, res.Confidence)
	e.QuickReply = ""
}

// GateSection is one labelled part of a gate's state.
func GateSection(b *strings.Builder, name, text string, limit int) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	if limit > 0 && len(text) > limit {
		cut := limit
		for cut > 0 && text[cut]&0xC0 == 0x80 {
			cut--
		}
		text = text[:cut] + "\n[truncated]"
	}
	fmt.Fprintf(b, "## %s\n%s\n\n", name, text)
}

// documentAt returns the content of the workstream's record at a revision.
func documentAt(repository *trace.Repository, stream config.WorkstreamID, id string, revision int) (string, bool) {
	docs, err := trace.Read[trace.Document](repository, stream)
	if err != nil {
		return "", false
	}
	for _, d := range docs {
		if d.ID == id && d.Revision == revision {
			return d.Content, true
		}
	}
	return "", false
}

// dissentText states a dissent record the way the packet shows it.
func dissentText(entries []shed.Entry) string {
	var b strings.Builder
	for _, e := range entries {
		weight := "advisory"
		if e.Blocking {
			weight = "blocking"
		}
		about := ""
		if e.Part != "" {
			about = " on " + e.Part
		}
		fmt.Fprintf(&b, "- %s (%s, %s, by %s in round %d%s, against %s)", e.ID, e.Kind, weight, e.Member, e.Round, about, e.Revision)
		switch {
		case e.Disposition != "":
			who := "the owner"
			if e.By != "" {
				who = "the chief of staff"
			}
			fmt.Fprintf(&b, " %s by %s", e.Disposition, who)
		case e.Escalated:
			b.WriteString(" escalated to the owner")
		}
		fmt.Fprintf(&b, ": %s", limitText(e.Argument, maxGateText))
		if e.Note != "" {
			fmt.Fprintf(&b, " Note: %s", limitText(e.Note, maxGateText))
		}
		if e.Escalation != "" {
			fmt.Fprintf(&b, " Chief of staff's escalation: %s", limitText(e.Escalation, maxGateText))
		}
		b.WriteString("\n")
	}
	return b.String()
}

func limitText(text string, limit int) string {
	text = strings.TrimSpace(text)
	if len(text) <= limit {
		return text
	}
	cut := limit
	for cut > 0 && text[cut]&0xC0 == 0x80 {
		cut--
	}
	return text[:cut] + " [truncated]"
}

// charterText returns the project's charter for a gate's state.
func (s *Controller) charterText(ctx context.Context, project config.ProjectID) string {
	if s.LoadCharter == nil {
		return ""
	}
	doc, _, err := s.LoadCharter(ctx, project)
	if err != nil {
		return ""
	}
	return doc.Content
}

// ratificationGate is the ratification gate of a workstream whose open card is
// e, or nil when Jev may not decide it: the card offers ratify only while no
// objection blocks, and a workstream of a cross-repo workgroup is ratified by
// the owner's group approval.
func (s *Controller) ratificationGate(repository *trace.Repository, stream config.WorkstreamID, e InboxEntry) (*Gate, error) {
	if !slices.Equal(e.Options, []string{"ratify"}) {
		return nil, nil
	}
	if id, err := repository.CrossRepoWorkgroupOf(stream); err != nil || id != "" {
		return nil, err
	}
	if required, err := repository.GroupApprovalRequired(stream); err != nil || required {
		return nil, err
	}
	packet, doc, found, err := shed.LatestPacket(repository, stream)
	if err != nil || !found {
		return nil, err
	}
	project := repository.Project()
	g := &Gate{Kind: jev.KindRatification, Project: project, Stream: stream, Key: jev.RatificationKey(packet.Round, packet.Revision.Spec, packet.Revision.Plan),
		Label: fmt.Sprintf("the ratification of %s of workstream %s", packet.Revision, stream), Cause: shed.PacketDocumentID(packet.Round), Reached: doc.At,
		Options: []jev.DecisionOption{{Name: "ratify", Description: "Ratify the spec and plan as drafted: the workstream is sealed and built."}}}
	g.State = func(ctx context.Context) (string, []jev.Source, error) {
		var b strings.Builder
		GateSection(&b, "Decision", fmt.Sprintf("The ratification gate of workstream %s. The only decision offered is to ratify %s, which seals the spec and the plan and builds the workstream. Anything else about the draft is the owner's.", stream, packet.Revision), 0)
		GateSection(&b, "Card", e.Question+"\nBlocked: "+e.Blocked, 0)
		GateSection(&b, "Chief of staff's recommendation", packet.Recommendation, 0)
		entries, err := Dissent(repository, stream)
		if err != nil {
			return "", nil, err
		}
		if len(entries) == 0 {
			GateSection(&b, "Dissent", "No objection stands.", 0)
		} else {
			GateSection(&b, "Dissent that stands", dissentText(entries), 0)
		}
		if spec, ok := documentAt(repository, stream, plan.SpecDocument, packet.Revision.Spec); ok {
			GateSection(&b, fmt.Sprintf("Spec (revision %d)", packet.Revision.Spec), spec, maxGateSpec)
		}
		if graph, ok := documentAt(repository, stream, plan.PlanDocument, packet.Revision.Plan); ok {
			GateSection(&b, fmt.Sprintf("Plan (revision %d)", packet.Revision.Plan), graph, maxGatePlan)
		}
		GateSection(&b, "Charter", s.charterText(ctx, project), maxGateCharter)
		return b.String(), []jev.Source{{Kind: "packet", ID: shed.PacketDocumentID(packet.Round), Revision: doc.Revision},
			{Kind: "spec", ID: plan.SpecDocument, Revision: packet.Revision.Spec}, {Kind: "plan", ID: plan.PlanDocument, Revision: packet.Revision.Plan}}, nil
	}
	g.Apply = func(ctx context.Context, res jev.Result) *APIError {
		if res.Pick != "ratify" {
			return &APIError{Code: Conflict, Message: fmt.Sprintf("%q is not a ratification decision", res.Pick)}
		}
		_, api := s.RatifyBy(ctx, string(stream), RatifyRequest{Spec: packet.Revision.Spec, Plan: packet.Revision.Plan}, Deciding{Judgment: res.Judgment})
		return api
	}
	g.Decision = func() (GateDecision, error) {
		ratified, recorded, err := LatestRatification(repository, stream)
		if err != nil || !recorded || ratified.Round != packet.Round || ratified.Revision != packet.Revision {
			return GateDecision{}, err
		}
		return GateDecision{Recorded: true, ByJev: ratified.DecidedBy == trace.DecidedByJev}, nil
	}
	return g, nil
}

// objectionIsJevs reports whether Jev may decide the objection at the
// ratification gate: a committee objection the chief of staff escalated that
// blocks ratification and that nobody disposed of. A conflict between member
// charters and the owner's own objection are the owner's.
func objectionIsJevs(e shed.Entry) bool {
	return e.Escalated && e.Blocking && e.Disposition == "" && e.Kind != shed.CharterConflict && e.Member != shed.OwnerMember
}

// objectionGates returns the gates of the workstream's escalated objections:
// the committee objections the chief of staff put to the owner that block
// ratification, while the ratification packet is open. A conflict between
// member charters and the owner's own objections are the owner's, and Jev may
// only dismiss or overrule, which need no owner-written text.
func (s *Controller) objectionGates(repository *trace.Repository, stream config.WorkstreamID, e InboxEntry) ([]Gate, error) {
	entries, err := Dissent(repository, stream)
	if err != nil {
		return nil, err
	}
	round := 0
	if state, err := repository.Workflow(stream, ShedSubject); err == nil {
		if _, n, ok := ShedState(state.Value); ok {
			round = n
		}
	}
	if round == 0 {
		round = 1
	}
	project := repository.Project()
	var out []Gate
	for _, entry := range entries {
		if !objectionIsJevs(entry) {
			continue
		}
		entry := entry
		reached := e.OpenedAt
		if docs, err := trace.Read[trace.Document](repository, stream); err == nil {
			for _, d := range docs {
				if d.Path == shed.ChiefPath(round) {
					reached = d.At
				}
			}
		}
		g := Gate{Kind: jev.KindEscalatedObjection, Project: project, Stream: stream, Key: jev.ObjectionKey(entry.ID),
			Label: fmt.Sprintf("objection %s of workstream %s", entry.ID, stream), Cause: shed.ChiefDocumentID(round), Reached: reached,
			Options: []jev.DecisionOption{
				{Name: string(shed.Dismissed), Description: "Dismiss the objection: it is settled, blocks no longer and is kept as the recorded disposition."},
				{Name: string(shed.Overruled), Description: "Overrule the objection: proceed in spite of it; it blocks no longer and is kept as the recorded disposition."}}}
		g.State = func(ctx context.Context) (string, []jev.Source, error) {
			var b strings.Builder
			GateSection(&b, "Decision", fmt.Sprintf("Objection %s of workstream %s blocks ratification and the chief of staff escalated it to the owner. Choose how the owner disposes of it: dismiss it, or overrule it and proceed. Sustaining it needs the owner's own words and is not offered here.", entry.ID, stream), 0)
			GateSection(&b, "The objection", dissentText([]shed.Entry{entry}), 0)
			GateSection(&b, "Dissent that stands", dissentText(entries), 0)
			if packet, doc, found, err := shed.LatestPacket(repository, stream); err == nil && found {
				GateSection(&b, "Ratification packet", fmt.Sprintf("Round %d, %s. %s\nChief of staff's recommendation: %s", packet.Round, packet.Revision, packet.Conclusion, packet.Recommendation), 0)
				if spec, ok := documentAt(repository, stream, plan.SpecDocument, packet.Revision.Spec); ok {
					GateSection(&b, fmt.Sprintf("Spec (revision %d)", packet.Revision.Spec), spec, maxGateSpec)
				}
				GateSection(&b, "Charter", s.charterText(ctx, project), maxGateCharter)
				return b.String(), []jev.Source{{Kind: "packet", ID: shed.PacketDocumentID(packet.Round), Revision: doc.Revision}, {Kind: "chief-decisions", ID: shed.ChiefDocumentID(round)}}, nil
			}
			return b.String(), []jev.Source{{Kind: "chief-decisions", ID: shed.ChiefDocumentID(round)}}, nil
		}
		g.Apply = func(ctx context.Context, res jev.Result) *APIError {
			disposition := shed.Disposition(res.Pick)
			if disposition != shed.Dismissed && disposition != shed.Overruled {
				return &APIError{Code: Conflict, Message: fmt.Sprintf("%q is not a decision on an objection", res.Pick)}
			}
			o, api := s.ShedAction(string(stream), InShedState)
			if api != nil {
				return api
			}
			note := fmt.Sprintf("Decided by Jev with confidence %.2f (judgment %s).", res.Confidence, res.Judgment)
			action := "ruled"
			if disposition == shed.Overruled {
				action = "overruled"
			}
			_, api = s.DisposeBy(ctx, o, entry.ID, disposition, note, action, Deciding{Judgment: res.Judgment})
			return api
		}
		g.Decision = func() (GateDecision, error) {
			now, err := Dissent(repository, stream)
			if err != nil {
				return GateDecision{}, err
			}
			i := slices.IndexFunc(now, func(x shed.Entry) bool { return x.ID == entry.ID })
			if i < 0 {
				// An objection the member conceded no longer stands.
				return GateDecision{Recorded: true}, nil
			}
			if now[i].Disposition == "" || now[i].By != "" {
				return GateDecision{}, nil
			}
			rounds, err := shed.AllRulings(repository, stream)
			if err != nil {
				return GateDecision{}, err
			}
			byJev := false
			for _, r := range rounds {
				for _, one := range r.Rulings {
					if one.Objection == entry.ID {
						byJev = one.DecidedBy == trace.DecidedByJev
					}
				}
			}
			return GateDecision{Recorded: true, ByJev: byJev}, nil
		}
		out = append(out, g)
	}
	return out, nil
}

// ratificationEntry is the workstream's ratification card as the owner sees it:
// RatificationEntry, held back while Jev's judgment of the ratification or of an
// escalated objection is pending or its pick is not applied, and with Jev's
// result as its recommendation when the gate came to the owner after it.
func (s *Controller) ratificationEntry(repository *trace.Repository, stream config.WorkstreamID) (InboxEntry, bool, error) {
	e, open, err := RatificationEntry(repository, stream)
	if err != nil || !open || s.Jev == nil {
		return e, open, err
	}
	objections, err := s.objectionGates(repository, stream, e)
	if err != nil {
		return InboxEntry{}, false, err
	}
	var recs []string
	var first *api.InboxJev
	for _, g := range objections {
		held, rec, view := s.holdsForJev(repository, g)
		if held {
			return InboxEntry{}, false, nil
		}
		if view != nil {
			recs = append(recs, fmt.Sprintf("Objection %s: %s", strings.TrimPrefix(g.Key, "objection:"), rec))
			if first == nil {
				first = view
			}
		}
	}
	if len(recs) > 0 {
		e.Jev = first
		e.Recommendation = strings.Join(recs, " ") + " " + e.Recommendation
	}
	gate, err := s.ratificationGate(repository, stream, e)
	if err != nil {
		return InboxEntry{}, false, err
	}
	if !s.guardedCard(repository, &e, gate) {
		return InboxEntry{}, false, nil
	}
	return e, true, nil
}

// amendmentGate is the gate of a presented amendment whose card is e, or nil
// when Jev may not decide it: only an amendment that changes the sealed spec,
// the approved intent, with no objection that blocks it, is Jev's to approve or
// decline. A plan-only amendment is the chief of staff's, and an objection that
// blocks needs the owner.
func (s *Controller) amendmentGate(repository *trace.Repository, stream config.WorkstreamID, c AmendmentCase, current seal.Seal, entries []shed.Entry, e InboxEntry) (*Gate, error) {
	if len(shed.Blocked(entries)) > 0 {
		return nil, nil
	}
	sealed, _ := documentAt(repository, stream, plan.SpecDocument, current.Revision.Spec)
	if sealed == "" || c.Spec.Content == sealed {
		return nil, nil
	}
	project := repository.Project()
	id := c.Request.ID
	g := &Gate{Kind: jev.KindIntentAmendment, Project: project, Stream: stream, Key: jev.AmendmentKey(id),
		Label: fmt.Sprintf("amendment %s of workstream %s", id, stream), Cause: c.Packet.ID, Reached: c.Packet.At,
		Options: []jev.DecisionOption{
			{Name: AmendmentApprove, Description: "Approve the amendment: the spec and plan are versioned and resealed, and the unit that asked resumes."},
			{Name: AmendmentReject, Description: "Decline the amendment: the sealed spec and plan stay in force and the requester is told."}}}
	g.State = func(ctx context.Context) (string, []jev.Source, error) {
		var b strings.Builder
		GateSection(&b, "Decision", fmt.Sprintf("Amendment %s of workstream %s changes the approved intent, so it is the owner's to approve or decline. Rounds of further debate and overruling objections are the owner's, not offered here.", id, stream), 0)
		GateSection(&b, "Card", e.Question+"\nBlocked: "+e.Blocked, 0)
		GateSection(&b, "Request", fmt.Sprintf("Requested by %s (%s). Citations: %s\nChange: %s\nReason: %s", c.Request.Requester.ID, c.Request.Role, strings.Join(c.Request.Citations, ", "), c.Request.Change, c.Request.Reason), 0)
		GateSection(&b, "Chief of staff's recommendation", e.Recommendation, 0)
		if len(entries) == 0 {
			GateSection(&b, "Dissent", "No objection stands.", 0)
		} else {
			GateSection(&b, "Dissent that stands", dissentText(entries), 0)
		}
		GateSection(&b, "Sealed spec", sealed, maxGateSpec)
		GateSection(&b, "Proposed spec", c.Spec.Content, maxGateSpec)
		GateSection(&b, "Proposed plan", c.Plan.Content, maxGatePlan)
		GateSection(&b, "Charter", s.charterText(ctx, project), maxGateCharter)
		return b.String(), []jev.Source{{Kind: "amendment-packet", ID: c.Packet.ID, Revision: c.Packet.Revision}, {Kind: "amendment", ID: c.Request.ID, Revision: c.Request.Revision}}, nil
	}
	g.Apply = func(ctx context.Context, res jev.Result) *APIError {
		if res.Pick != AmendmentApprove && res.Pick != AmendmentReject {
			return &APIError{Code: Conflict, Message: fmt.Sprintf("%q is not an amendment decision", res.Pick)}
		}
		note := fmt.Sprintf("Decided by Jev with confidence %.2f (judgment %s).", res.Confidence, res.Judgment)
		_, api := s.RecordAmendmentDecisionBy(ctx, project, stream, repository, id, AmendmentDecisionRequest{Decision: res.Pick, Note: note, Packet: c.Packet.Revision}, trace.JevActor, c.Packet.ID, res.Judgment)
		return api
	}
	g.Decision = func() (GateDecision, error) {
		now, found, err := ReadAmendment(repository, stream, id)
		if err != nil || !found {
			return GateDecision{}, err
		}
		for _, d := range now.Decisions {
			if d.Packet == c.Packet.Revision {
				return GateDecision{Recorded: true, ByJev: d.Actor == trace.DecidedByJev}, nil
			}
		}
		return GateDecision{}, nil
	}
	return g, nil
}

// amendmentEntry is a presented amendment's card as the owner sees it, held
// back while Jev's judgment of it is pending or its pick is not applied.
func (s *Controller) amendmentEntry(repository *trace.Repository, stream config.WorkstreamID, id string) (InboxEntry, bool, error) {
	e, open, err := s.AmendmentEntry(repository, stream, id)
	if err != nil || !open || s.Jev == nil {
		return e, open, err
	}
	g, err := s.AmendmentGateOf(repository, stream, id, e)
	if err != nil {
		return InboxEntry{}, false, err
	}
	if !s.guardedCard(repository, &e, g) {
		return InboxEntry{}, false, nil
	}
	return e, true, nil
}

// AmendmentGateOf returns the gate of the presented amendment id whose card is
// e, or nil.
func (s *Controller) AmendmentGateOf(repository *trace.Repository, stream config.WorkstreamID, id string, e InboxEntry) (*Gate, error) {
	c, found, err := ReadAmendment(repository, stream, id)
	if err != nil || !found {
		return nil, err
	}
	records, err := AmendmentRecords(repository, stream, id)
	if err != nil {
		return nil, err
	}
	current, _, sealed, err := seal.Latest(repository, stream)
	if err != nil || !sealed {
		return nil, err
	}
	return s.amendmentGate(repository, stream, c, current, AmendmentDissent(records), e)
}

// escalationRoles are the roles whose escalated questions Jev may decide: the
// workers that build a feature. An architect's question, such as the judgment
// that a project needs no change, is the owner's.
var escalationRoles = []string{MasonRole, ReviewerRole}

// escalationGate returns the gate of an escalation whose card is view, or, when
// Jev may not decide it, nil and the binding Jev judgment it already carries.
// Jev decides an escalation only from the options the chief of staff wrote, and
// never one that reports a conflict between member charters.
func (s *Controller) escalationGate(repository *trace.Repository, e trace.InboxEntry, view InboxEntry) (*Gate, *jev.Result, error) {
	if e.Hold != "" || len(e.Questions) == 0 || e.Questions[0].Asked.ID == "" {
		return nil, nil, nil
	}
	for _, q := range e.Questions {
		if res, ok := s.CarriedJudgment(repository, q.Asked.Workstream, jev.KindQuestionRouting, jev.QuestionKey(q.Asked.ID)); ok {
			return nil, &res, nil
		}
	}
	for _, q := range e.Questions {
		if len(q.Asked.CharterConflict) > 0 {
			return nil, nil, nil
		}
		thread, err := repository.Thread(q.Asked.Workstream, q.Asked.AskedBy.ID)
		if err != nil || !slices.Contains(escalationRoles, thread.Identity.Role) {
			return nil, nil, nil
		}
	}
	var options []jev.DecisionOption
	seen := map[string]bool{}
	for i, text := range e.Options {
		text = strings.TrimSpace(text)
		if text == "" || seen[text] {
			continue
		}
		seen[text] = true
		options = append(options, jev.DecisionOption{Name: fmt.Sprintf("option-%d", i+1), Description: text})
	}
	if len(options) == 0 {
		return nil, nil, nil
	}
	first := e.Questions[0].Asked
	project := repository.Project()
	number := e.Number
	g := &Gate{Kind: jev.KindEscalatedQuestion, Project: project, Stream: first.Workstream, Key: jev.QuestionKey(first.ID),
		Label: fmt.Sprintf("inbox entry %d of workstream %s", number, first.Workstream), Cause: fmt.Sprintf("escalation-%d", number), Reached: e.EscalatedAt, Options: options}
	g.State = func(ctx context.Context) (string, []jev.Source, error) {
		var b strings.Builder
		GateSection(&b, "Decision", fmt.Sprintf("The chief of staff escalated inbox entry %d to the owner. Choose the option the owner would choose; the option is the owner's ruling.", number), 0)
		GateSection(&b, "Question", view.Question, 0)
		GateSection(&b, "What waits", view.Blocked, 0)
		GateSection(&b, "Section in tension", e.Section, 0)
		GateSection(&b, "The tension", e.Conflict, 0)
		var asked strings.Builder
		var sources []jev.Source
		for _, q := range e.Questions {
			fmt.Fprintf(&asked, "- Question %s, asked by %s", q.Asked.ID, q.Asked.AskedBy.ID)
			if q.Asked.Unit != "" {
				fmt.Fprintf(&asked, " for unit %s", q.Asked.Unit)
			}
			fmt.Fprintf(&asked, ": %s\n", limitText(q.Asked.Question, maxGateText))
			sources = append(sources, jev.Source{Kind: "question", ID: q.Asked.ID, Revision: q.Latest.Revision})
		}
		GateSection(&b, "Questions as the askers put them", asked.String(), 0)
		GateSection(&b, "Chief of staff's recommendation", e.Recommendation, 0)
		GateSection(&b, "Charter", s.charterText(ctx, project), maxGateCharter)
		return b.String(), sources, nil
	}
	g.Apply = func(ctx context.Context, res jev.Result) *APIError {
		text := ""
		for _, o := range options {
			if o.Name == res.Pick {
				text = o.Description
			}
		}
		if text == "" {
			return &APIError{Code: Conflict, Message: fmt.Sprintf("%q is not one of the options the chief of staff wrote", res.Pick)}
		}
		_, api := s.RuleInboxBy(ctx, repository, number, text, nil, Deciding{Judgment: res.Judgment})
		return api
	}
	g.Decision = func() (GateDecision, error) {
		entries, err := repository.Inbox()
		if err != nil {
			return GateDecision{}, err
		}
		i := slices.IndexFunc(entries, func(x trace.InboxEntry) bool { return x.Number == number })
		if i < 0 {
			return GateDecision{}, nil
		}
		ruled, escalated, byJev := 0, 0, false
		for _, q := range entries[i].Questions {
			switch {
			case q.State == trace.QuestionEscalated:
				escalated++
			case q.Ruling != nil:
				ruled++
				byJev = q.Ruling.DecidedBy == trace.DecidedByJev
			}
		}
		return GateDecision{Recorded: ruled > 0, ByJev: byJev, Incomplete: ruled > 0 && escalated > 0}, nil
	}
	return g, nil, nil
}

// EscalationCard is an escalation's card as the owner sees it, or nothing while
// Jev's judgment of it is pending or its pick is not applied. A card whose
// subject carries a binding judgment Jev made as the chief of staff's pick,
// which the service could not carry out, shows it.
func (s *Controller) EscalationCard(repository *trace.Repository, e trace.InboxEntry, view InboxEntry) (InboxEntry, bool, error) {
	if s.Jev == nil {
		return view, true, nil
	}
	g, carried, err := s.escalationGate(repository, e, view)
	if err != nil {
		return InboxEntry{}, false, err
	}
	if carried != nil {
		CarriedCard(&view, *carried, "the chief of staff's decision on this question")
		return view, true, nil
	}
	if !s.guardedCard(repository, &view, g) {
		return InboxEntry{}, false, nil
	}
	return view, true, nil
}

// RuleInboxBy records the ruling on the escalation as the given decider. It is
// RuleInbox, which is the owner's, for a decision that may be Jev's.
func (s *Controller) RuleInboxBy(ctx context.Context, repository *trace.Repository, number int, text string, unknown *APIError, d Deciding) (AnswerResponse, *APIError) {
	if unknown == nil {
		unknown = &APIError{Code: Validation, Message: fmt.Sprintf("there is no inbox entry %d; list the entries with osmia inbox", number)}
	}
	return s.ruleInbox(ctx, repository, number, text, unknown, d)
}

// WorkstreamGates returns the gates the workstream has reached whose decision
// Jev may make: its ratification and escalated objections, its presented
// amendments that change intent, and its contested units the chief of staff
// escalated. Whatever the owner already decided is included, for the gate's own
// decision to say so.
func (s *Controller) WorkstreamGates(ctx context.Context, repository *trace.Repository, w trace.WorkstreamStatus) ([]Gate, error) {
	var out []Gate
	if w.State == InShedState || w.State == SketchedState {
		e, open, err := RatificationEntry(repository, w.Workstream)
		if err != nil {
			return nil, err
		}
		if open {
			objections, err := s.objectionGates(repository, w.Workstream, e)
			if err != nil {
				return nil, err
			}
			out = append(out, objections...)
			g, err := s.ratificationGate(repository, w.Workstream, e)
			if err != nil {
				return nil, err
			}
			if g != nil {
				out = append(out, *g)
			}
		}
	}
	if s.ContestedGate != nil {
		for _, gate := range w.Gates {
			if gate.Kind != UnitContested {
				continue
			}
			g, _, err := s.ContestedGate(repository, w.Workstream, gate.Reference)
			if err != nil {
				return nil, err
			}
			if g != nil {
				out = append(out, *g)
			}
		}
	}
	if w.State == BuildingState || w.State == AssembledState {
		for subject, state := range w.Subjects {
			id, ok := strings.CutPrefix(subject, AmendmentSubject(""))
			if !ok || state.Value != AmendmentPresented {
				continue
			}
			e, open, err := s.AmendmentEntry(repository, w.Workstream, id)
			if err != nil {
				return nil, err
			}
			if !open {
				continue
			}
			g, err := s.AmendmentGateOf(repository, w.Workstream, id, e)
			if err != nil {
				return nil, err
			}
			if g != nil {
				out = append(out, *g)
			}
		}
	}
	return out, nil
}

// ReachedGates returns the gates of the project's workstreams that Jev may
// decide and that are still open: reached, and neither carrying a decision the
// owner is to make nor already decided. It reads and writes nothing else.
func (s *Controller) ReachedGates(ctx context.Context, p *Project) ([]Gate, error) {
	repository := p.Repository
	statuses, err := repository.Statuses()
	if err != nil {
		return nil, err
	}
	gone := map[config.WorkstreamID]bool{}
	var out []Gate
	for _, w := range statuses {
		gone[w.Workstream] = w.State == AbandonedState
	}
	for _, w := range statuses {
		if gone[w.Workstream] || w.Workstream == LibrarianWorkstream(p.ID) {
			continue
		}
		gates, err := s.WorkstreamGates(ctx, repository, w)
		if err != nil {
			return nil, err
		}
		out = append(out, gates...)
	}
	escalations, err := repository.Inbox()
	if err != nil {
		return nil, err
	}
	for _, e := range escalations {
		if e.State != trace.QuestionEscalated || gone[e.Workstream] {
			continue
		}
		g, _, err := s.escalationGate(repository, e, InboxView(e))
		if err != nil {
			return nil, err
		}
		if g != nil {
			out = append(out, *g)
		}
	}
	return out, nil
}

// ContestedCard is a contested unit's card as the owner sees it: the service's
// ContestedEntry, held back while Jev's judgment of it is pending or its pick
// is not applied.
func (s *Controller) ContestedCard(repository *trace.Repository, stream config.WorkstreamID, unit string) (InboxEntry, bool, error) {
	e, open, err := s.ContestedEntry(repository, stream, unit)
	if err != nil || !open || s.Jev == nil || s.ContestedGate == nil {
		return e, open, err
	}
	g, carried, err := s.ContestedGate(repository, stream, unit)
	if err != nil {
		return InboxEntry{}, false, err
	}
	if carried != nil {
		CarriedCard(&e, *carried, "the chief of staff's ruling on this unit")
		return e, true, nil
	}
	if !s.guardedCard(repository, &e, g) {
		return InboxEntry{}, false, nil
	}
	return e, true, nil
}

// HeldGates returns the gates among the workstream's open owner gates that are
// held back from the owner while Jev's judgment of them is pending or its
// confident pick is not applied. They are in the records as open, but nothing
// waits on the owner for them yet, so status leaves them out as the inbox does.
// It only reads.
func (s *Controller) HeldGates(repository *trace.Repository, w trace.WorkstreamStatus) []trace.OwnerGate {
	if s.Jev == nil {
		return nil
	}
	var held []trace.OwnerGate
	var inbox []trace.InboxEntry
	for _, gate := range w.Gates {
		switch gate.Kind {
		case "ratification":
			_, open, err := RatificationEntry(repository, w.Workstream)
			if err != nil || !open {
				continue
			}
			if _, shown, err := s.ratificationEntry(repository, w.Workstream); err == nil && !shown {
				held = append(held, gate)
			}
		case "escalation":
			if inbox == nil {
				var err error
				if inbox, err = repository.Inbox(); err != nil {
					continue
				}
			}
			i := slices.IndexFunc(inbox, func(e trace.InboxEntry) bool { return fmt.Sprint(e.Number) == gate.Reference })
			if i < 0 {
				continue
			}
			if _, shown, err := s.EscalationCard(repository, inbox[i], InboxView(inbox[i])); err == nil && !shown {
				held = append(held, gate)
			}
		case "contested":
			_, open, err := s.ContestedEntry(repository, w.Workstream, gate.Reference)
			if err != nil || !open {
				continue
			}
			if _, shown, err := s.ContestedCard(repository, w.Workstream, gate.Reference); err == nil && !shown {
				held = append(held, gate)
			}
		}
	}
	return held
}
