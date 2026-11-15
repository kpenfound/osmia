package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/followup"
	"github.com/kpenfound/osmia/internal/hearsay"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/trace"
)

const (
	// decisionLimit bounds each prose field of a decision record.
	decisionLimit = 16 << 10
	// rejectedLimit bounds one rejected option, and maxRejected the count
	// Hearsay accepts.
	rejectedLimit = 1 << 10
	maxRejected   = 32
)

// The deciders of Hearsay's decision record.
const (
	deciderOwner = "owner"
	deciderChief = "chief_of_staff"
)

// decider is who made a decision Hearsay is told about.
type decider int

const (
	// byChief is the zero value: the chief of staff's own decision.
	byChief decider = iota
	byOwner
	byJev
)

// publishedDecision is one Hearsay decision before it is routed to a source. An owner
// decision goes to the owner source with the owner as author. The chief of
// staff's and Jev's go to the work source and name no author, and Jev's names
// Jev and its judgment in its text. workstream is empty for a decision about
// the project as a whole.
type publishedDecision struct {
	id         string
	by         decider
	judgment   string
	workstream config.WorkstreamID

	context, outcome, rationale string
	rejected                    []string
	// supersedes is the id of the earlier decision this one revisits.
	supersedes                          string
	specDocument, pullRequest, question string
	created, updated                    time.Time
}

// decider is the value of the record's decider field. Hearsay accepts only
// owner, chief_of_staff and committee and takes authority from the source, so a
// decision Jev made is posted as the chief of staff's and names Jev in its text.
func (d publishedDecision) decider() string {
	if d.by == byOwner {
		return deciderOwner
	}
	return deciderChief
}

// source is the Hearsay source that holds the decision: the owner source for
// the owner's decisions and the work source for every other.
func (d publishedDecision) source(in *publishInputs) string {
	if d.by == byOwner {
		return in.ownerSource
	}
	return in.workSource
}

// jevDecider returns who decided a record that names its decider and the Jev
// judgment it comes from. A record that names Jev without a judgment is not
// one Jev made.
func jevDecider(by, judgment string) bool {
	return by == trace.DecidedByJev && trace.ValidDecider(by, judgment)
}

// jevNote opens a rationale with the statement that Jev decided and the
// judgment it came from, so the decision is never read as the owner's.
func jevNote(judgment, rest string) string {
	note := fmt.Sprintf("Decided by Jev, the decision maker for the owner, with judgment %s; the owner did not decide it.", judgment)
	if rest = strings.TrimSpace(rest); rest != "" {
		note += " " + rest
	}
	return note
}

// jevID is the id of a decision Jev made: the workstream, "jev", the kind and
// the subject, so no owner or chief decision about the same subject can hold
// it on another source.
func jevID(workstream config.WorkstreamID, kind string, parts ...string) string {
	return decisionID(append([]string{string(workstream), "jev", kind}, parts...)...)
}

// decisionID derives a decision's id from the Osmia ids it is about. The ids
// are joined when they fit Hearsay's id grammar, and otherwise hashed, so the
// same inputs always give the same id.
func decisionID(parts ...string) string {
	id := strings.Join(parts, "-")
	if hearsay.ValidID(id) {
		return id
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return "d-" + hex.EncodeToString(sum[:16])
}

func shortHash(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:8])
}

// decisionRecords posts the decisions made about the project: those of each
// eligible workstream, the charter proposals the owner decided, and the
// workgroup's amendments to its project spec. Owner decisions go to the owner
// source; the chief of staff's go to the work source and stay inferred. A
// decision that revisits an earlier one names it in supersedes, and is posted
// after it.
func decisionRecords(in *publishInputs) []hearsay.Record {
	var all []publishedDecision
	for _, st := range in.streams {
		if !st.decisionsRead {
			continue
		}
		all = append(all, st.decisions(in)...)
	}
	all = append(all, in.workgroupDecisions()...)

	byID := map[string]publishedDecision{}
	var ordered []publishedDecision
	for _, d := range all {
		if d.id == "" || !hearsay.ValidID(d.id) || strings.TrimSpace(d.outcome) == "" {
			continue
		}
		if _, dup := byID[d.id]; dup {
			continue
		}
		byID[d.id] = d
		ordered = append(ordered, d)
	}
	var out []hearsay.Record
	for _, d := range ordered {
		out = append(out, in.decisionRecord(d, byID))
	}
	return out
}

// decisionRecord is the record of one decision. byID resolves the decision it
// supersedes to the source that holds it.
func (in *publishInputs) decisionRecord(d publishedDecision, byID map[string]publishedDecision) hearsay.Record {
	source := d.source(in)
	parent := in.projectRef()
	if d.workstream != "" {
		parent = in.workstreamRef(d.workstream)
	}
	after := []string{parent}
	w := hearsay.Wire{Kind: hearsay.KindDecision, Project: string(in.project), Workstream: string(d.workstream), Decision: d.id,
		Context: boundedText(d.context, decisionLimit), Outcome: boundedText(d.outcome, decisionLimit), Rationale: boundedText(d.rationale, decisionLimit),
		Decider: d.decider(), CreatedAt: hearsay.Time(d.created), UpdatedAt: hearsay.Time(d.updated)}
	if d.by == byOwner {
		w.Author = in.owner
	}
	for _, option := range d.rejected {
		if option = strings.TrimSpace(option); option != "" && len(w.RejectedOptions) < maxRejected {
			w.RejectedOptions = append(w.RejectedOptions, boundedText(option, rejectedLimit))
		}
	}
	if earlier, ok := byID[d.supersedes]; ok && d.supersedes != d.id {
		w.Supersedes = earlier.id
		after = append(after, hearsay.Ref(earlier.source(in), "decision/"+earlier.id))
	}
	if hearsay.ValidID(d.specDocument) {
		w.SpecDocument = d.specDocument
	}
	if isWebURL(d.pullRequest) {
		w.PullRequest = d.pullRequest
	}
	if d.question != "" && len(d.question) <= 256 && !strings.ContainsAny(d.question, " \t\r\n") {
		w.Question = d.question
	}
	return hearsay.Record{Source: source, Key: "decision/" + d.id, Tier: hearsay.TierRecord, After: after, Versions: []hearsay.Wire{w}}
}

// readDecisions reads the trace records a workstream's decisions are built
// from. A workstream whose records cannot be read this pass posts no decision
// in it, and its decisions are left for a later one.
func (st *publishStream) readDecisions(repo *trace.Repository) error {
	id := st.id()
	var err error
	if st.questions, err = repo.Questions(id); err != nil {
		return err
	}
	if st.rulings, err = trace.Read[trace.Ruling](repo, id); err != nil {
		return err
	}
	if st.amendments, err = trace.Read[trace.Amendment](repo, id); err != nil {
		return err
	}
	if st.assumptions, err = trace.Assumptions(repo, id); err != nil {
		return err
	}
	if st.followups, err = followup.Read(repo, id); err != nil {
		return err
	}
	st.decisionsRead = true
	return nil
}

// links are the document and pull request every decision of the workstream
// links to, where they exist.
func (st *publishStream) links(in *publishInputs) (spec, pullRequest string) {
	if documents := st.sealedSpecs(in); len(documents) > 0 {
		spec = documents[len(documents)-1].document
	}
	if st.evidence.Published && isWebURL(st.evidence.Publication.URL) {
		pullRequest = st.evidence.Publication.URL
	}
	return spec, pullRequest
}

// decisions are the decisions made in one workstream, a chain of revisited
// decisions in the order they were made.
func (st *publishStream) decisions(in *publishInputs) []publishedDecision {
	spec, pr := st.links(in)
	var out []publishedDecision
	out = append(out, st.questionDecisions()...)
	out = append(out, st.assumptionDecisions()...)
	out = append(out, st.amendmentDecisions()...)
	out = append(out, st.discoveryDecisions()...)
	out = append(out, st.acceptedFindings()...)
	out = append(out, st.followupDecisions()...)
	out = append(out, st.charterDecisions()...)
	for i := range out {
		if out[i].workstream != "" {
			out[i].specDocument, out[i].pullRequest = spec, pr
		}
	}
	return out
}

// questionRef names a question across workstreams.
func (st *publishStream) questionRef(id string) string { return string(st.id()) + ":" + id }

// questionDecisions are the rulings on escalated questions and the chief of
// staff's answers about intent or scope. The owner's ruling is the first
// revision of the question's ruling, and a ruling Jev made is the first revision
// too, written by Jev with its judgment. The chief's relay of either, a later
// revision, is never posted as a ruling. An answer that cites a code inspection
// is engineering and is not posted; one Jev bound is posted as Jev's.
func (st *publishStream) questionDecisions() []publishedDecision {
	var out []publishedDecision
	for _, q := range st.questions {
		var first, answer *trace.Ruling
		for i, r := range st.rulings {
			if r.QuestionID != q.Asked.ID {
				continue
			}
			switch {
			case r.Revision == 1 && r.Decision == trace.DecisionRuling && (r.Actor.Kind == "owner" || jevRuling(r)) && strings.TrimSpace(r.OwnerResponse) != "":
				first = &st.rulings[i]
			case r.Decision == trace.DecisionAnswer && strings.TrimSpace(r.ReturnedAnswer) != "":
				answer = &st.rulings[i]
			}
		}
		asked := strings.TrimSpace(q.Asked.Question)
		switch {
		case first != nil:
			out = append(out, st.ownerRuling(q, *first))
		case answer != nil && !citesCode(*answer):
			d := publishedDecision{
				id: decisionID(string(st.id()), "answer", q.Asked.ID), workstream: st.id(),
				context:   "A question to the chief of staff about intent or scope: " + asked,
				outcome:   strings.TrimSpace(answer.ReturnedAnswer),
				rationale: "Answered by the chief of staff from the record" + citedFrom(answer.Citations) + ".",
				question:  st.questionRef(q.Asked.ID), created: answer.At, updated: answer.At}
			if jevRuling(*answer) {
				d.id, d.by, d.judgment = jevID(st.id(), "answer", q.Asked.ID), byJev, answer.Judgment
				d.rationale = jevNote(answer.Judgment, "The answer's text is the chief of staff's, from the record"+citedFrom(answer.Citations)+".")
			}
			out = append(out, d)
		}
	}
	return out
}

// jevRuling reports whether Jev made the decision a ruling records. The trace
// guarantees that Jev's ruling is written by the Jev actor and that the Jev
// actor writes no other ruling, apart from the chief of staff's relay of it.
func jevRuling(r trace.Ruling) bool {
	return jevDecider(r.DecidedBy, r.Judgment) && r.Actor == trace.JevActor
}

// citesCode reports whether an answer rests on a recorded code inspection.
func citesCode(r trace.Ruling) bool {
	return slices.ContainsFunc(r.Citations, func(c string) bool { return strings.HasPrefix(c, "inspection#") })
}

func citedFrom(citations []string) string {
	if len(citations) == 0 {
		return ""
	}
	return ", citing " + strings.Join(citations, ", ")
}

// ownerRuling is the decision the owner, or Jev for the owner, made on an
// escalated question, with the question as the owner read it and the
// escalation: the passage in tension, the conflict and the options.
func (st *publishStream) ownerRuling(q trace.QuestionState, ruling trace.Ruling) publishedDecision {
	jev := jevRuling(ruling)
	question := strings.TrimSpace(q.Latest.SentToOwner)
	if question == "" {
		question = strings.TrimSpace(q.Asked.Question)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Question: %s", question)
	ruledBy := "the owner"
	if jev {
		ruledBy = "Jev"
	}
	rationale := "Ruled by " + ruledBy + " on an escalated question."
	if e := q.Latest.Escalation; e != nil {
		fmt.Fprintf(&b, "\n\nPassage in tension: %s\n\nConflict: %s", strings.TrimSpace(e.Section), strings.TrimSpace(e.Conflict))
		if blocked := strings.TrimSpace(e.Blocked); blocked != "" {
			fmt.Fprintf(&b, "\n\nBlocked on the ruling: %s", blocked)
		}
		if len(e.Options) > 0 {
			b.WriteString("\n\nOptions put to the owner:")
			for _, o := range e.Options {
				fmt.Fprintf(&b, "\n- %s", strings.TrimSpace(o))
			}
		}
		if rec := strings.TrimSpace(e.Recommendation); rec != "" {
			rationale = fmt.Sprintf("Ruled by %s on escalated question, inbox entry %d. The chief of staff recommended: %s", ruledBy, e.Inbox, rec)
		}
	}
	d := publishedDecision{
		id: decisionID(string(st.id()), "ruling", q.Asked.ID), by: byOwner, workstream: st.id(),
		context: b.String(), outcome: strings.TrimSpace(ruling.OwnerResponse), rationale: rationale,
		question: st.questionRef(q.Asked.ID), created: ruling.At, updated: ruling.At}
	if jev {
		d.id, d.by, d.judgment = jevID(st.id(), "ruling", q.Asked.ID), byJev, ruling.Judgment
		d.rationale = jevNote(ruling.Judgment, rationale)
	}
	return d
}

// assumptionDecisions are the judgment calls the chief of staff logged
// instead of asking. One that affects several members of a workgroup is about
// the project as a whole, and each member project posts it once.
func (st *publishStream) assumptionDecisions() []publishedDecision {
	var out []publishedDecision
	for _, a := range st.assumptions {
		if strings.TrimSpace(a.Text) == "" {
			continue
		}
		d := publishedDecision{
			id:      decisionID("assumption", shortHash(a.Thread, a.Turn, a.ID, a.At.UTC().Format(time.RFC3339Nano), a.Text)),
			context: "A judgment call the chief of staff made within the stated intent instead of asking the owner.",
			outcome: strings.TrimSpace(a.Text), rationale: "It stays within the approved intent, so the chief of staff logged it and carried on.",
			created: a.At, updated: a.At}
		if len(a.Workstreams) <= 1 {
			d.workstream = st.id()
		}
		out = append(out, d)
	}
	return out
}

// subject identifies what an amendment is about: the criteria it cites.
func subject(citations []string) string {
	c := slices.Clone(citations)
	slices.Sort(c)
	return strings.Join(slices.Compact(c), "\x00")
}

// amendmentDecisions are the final decisions on the workstream's amendments:
// the owner's, Jev's, and the chief of staff's on plan-only revisions. A request
// that is not yet decided, a debate round and a service retirement are not
// decisions and are not posted. A later approved amendment about the same
// criteria revisits the earlier decision.
func (st *publishStream) amendmentDecisions() []publishedDecision {
	requests := map[string]trace.Amendment{}
	var order []string
	for _, a := range st.amendments {
		if _, ok := requests[a.ID]; !ok {
			order = append(order, a.ID)
		}
		requests[a.ID] = a
	}
	type decided struct {
		request  trace.Amendment
		document trace.Document
		verdict  intake.AmendmentDecision
	}
	var all []decided
	for _, id := range order {
		var final *decided
		for _, d := range st.documents {
			if d.Path != intake.AmendmentDecisionPath(id) {
				continue
			}
			var verdict intake.AmendmentDecision
			if json.Unmarshal([]byte(d.Content), &verdict) != nil {
				continue
			}
			if verdict.Decision == intake.AmendmentRound {
				continue
			}
			if final == nil || d.Revision > final.document.Revision {
				final = &decided{requests[id], d, verdict}
			}
		}
		if final == nil {
			continue
		}
		if _, ok := amendmentDecider(final.document, final.verdict); !ok {
			continue
		}
		all = append(all, *final)
	}
	slices.SortStableFunc(all, func(a, b decided) int { return a.document.At.Compare(b.document.At) })

	var out []publishedDecision
	lastBySubject := map[string]string{}
	for _, c := range all {
		req, verdict := c.request, c.verdict
		approved := verdict.Decision == intake.AmendmentApprove || verdict.Decision == intake.AmendmentOverrule
		change := strings.TrimSpace(req.Change)
		by, _ := amendmentDecider(c.document, verdict)
		d := publishedDecision{
			id: decisionID(string(st.id()), "amendment", req.ID), by: by, workstream: st.id(),
			context: fmt.Sprintf("Amendment %s to the sealed spec and plan, citing %s.\n\nChange: %s\n\nReason: %s", req.ID, strings.Join(req.Citations, ", "), change, strings.TrimSpace(req.Reason)),
			created: c.document.At, updated: c.document.At}
		switch {
		case approved:
			d.outcome = "Approved amendment " + req.ID + ": " + change
			d.rejected = []string{"Keep the sealed spec and plan unchanged"}
		default:
			d.outcome = "Rejected amendment " + req.ID + ": " + change
			d.rejected = []string{"Apply the amendment"}
		}
		d.rationale = strings.TrimSpace(verdict.Note)
		if d.rationale == "" {
			d.rationale = strings.TrimSpace(req.Reason)
		}
		if by == byJev {
			d.id, d.judgment = jevID(st.id(), "amendment", req.ID), verdict.Judgment
			d.rationale = jevNote(verdict.Judgment, d.rationale)
		}
		if req.QuestionID != "" {
			d.question = st.questionRef(req.QuestionID)
		}
		key := subject(req.Citations)
		if earlier, ok := lastBySubject[key]; ok && approved {
			d.supersedes = earlier
		}
		lastBySubject[key] = d.id
		out = append(out, d)
	}
	return out
}

// amendmentDecider is who made a final amendment decision, and whether it is
// one the publisher posts: the owner's, Jev's or the chief of staff's. A
// decision Jev made is written by the Jev actor and names Jev as its actor and
// its judgment; a record whose header and content disagree, or that names Jev
// without a judgment, is nobody's and is not posted.
func amendmentDecider(doc trace.Document, verdict intake.AmendmentDecision) (decider, bool) {
	if !trace.ValidDecisionActor(doc) {
		return 0, false
	}
	switch {
	case doc.Actor == trace.JevActor:
		return byJev, jevDecider(verdict.Actor, verdict.Judgment)
	case doc.Actor.Kind == "owner":
		return byOwner, true
	case doc.Actor.Kind == "agent" && doc.Actor.ID == trace.ChiefOfStaff:
		return byChief, true
	}
	return 0, false
}

// discovery is one revision of a discovered piece of work, as the chief of
// staff or an agent recorded it.
type discovery struct {
	doc  trace.Document
	work workDiscovery
}

// discoveryDecisions are the chief of staff's scope calls on discovered work:
// declined, or kept as an optional suggestion rather than required work. A
// discovery the chief of staff later assigns or reopens revisits that call.
func (st *publishStream) discoveryDecisions() []publishedDecision {
	byID := map[string][]discovery{}
	var order []string
	for _, d := range st.documents {
		if !strings.HasPrefix(d.Path, "discoveries/") {
			continue
		}
		var work workDiscovery
		if json.Unmarshal([]byte(d.Content), &work) != nil || work.ID == "" {
			continue
		}
		if _, ok := byID[work.ID]; !ok {
			order = append(order, work.ID)
		}
		byID[work.ID] = append(byID[work.ID], discovery{d, work})
	}
	var out []publishedDecision
	for _, id := range order {
		revisions := byID[id]
		slices.SortStableFunc(revisions, func(a, b discovery) int { return a.doc.Revision - b.doc.Revision })
		previous := ""
		for _, r := range revisions {
			w := r.work
			scopeCall := w.Disposition == "declined" || w.Disposition == "suggestion"
			if !scopeCall && previous == "" {
				continue
			}
			jev := jevDecider(w.DecidedBy, w.Judgment)
			d := publishedDecision{
				id:         decisionID(string(st.id()), "discovery", shortHash(id), fmt.Sprint(r.doc.Revision)),
				workstream: st.id(),
				context:    fmt.Sprintf("Work discovered against %s: %s", w.Criterion, strings.TrimSpace(w.Description)),
				created:    r.doc.At, updated: r.doc.At, supersedes: previous, rationale: strings.TrimSpace(w.Reason)}
			switch w.Disposition {
			case "declined":
				d.outcome = "Declined the discovered work: " + strings.TrimSpace(w.Description)
				d.rejected = []string{"Build it as necessary work"}
			case "suggestion":
				d.outcome = "Deferred the discovered work as an optional suggestion: " + strings.TrimSpace(w.Description)
				d.rejected = []string{"Build it now as necessary work"}
			case "assigned":
				d.outcome = fmt.Sprintf("Assigned the discovered work to unit %s: %s", w.Unit, strings.TrimSpace(w.Description))
			default:
				d.outcome = "Reopened the discovered work as necessary: " + strings.TrimSpace(w.Description)
			}
			if jev {
				d.id, d.by, d.judgment = jevID(st.id(), "discovery", shortHash(id), fmt.Sprint(r.doc.Revision)), byJev, w.Judgment
				d.rationale = jevNote(w.Judgment, d.rationale)
			}
			previous = d.id
			out = append(out, d)
		}
	}
	return out
}

// acceptedFindings are the final-review findings the owner accepted as they
// are by approving delivery, instead of having them fixed.
func (st *publishStream) acceptedFindings() []publishedDecision {
	type key struct {
		review  int
		finding string
	}
	first := map[key]trace.Document{}
	var order []key
	var approvals []trace.Document
	for _, d := range st.documents {
		if d.ID == delivery.DeliveryDocument {
			approvals = append(approvals, d)
		}
	}
	slices.SortStableFunc(approvals, func(a, b trace.Document) int { return a.Revision - b.Revision })
	for _, d := range approvals {
		var approval delivery.DeliveryApproval
		if json.Unmarshal([]byte(d.Content), &approval) != nil {
			continue
		}
		for _, finding := range approval.AcceptedBlocking {
			k := key{approval.Review, finding}
			if _, ok := first[k]; !ok {
				first[k] = d
				order = append(order, k)
			}
		}
	}
	var out []publishedDecision
	for _, k := range order {
		d := first[k]
		out = append(out, publishedDecision{
			id: decisionID(string(st.id()), "accepted", fmt.Sprint(k.review), shortHash(k.finding)), by: byOwner, workstream: st.id(),
			context:  fmt.Sprintf("Final review %d found this blocking: %s", k.review, strings.TrimSpace(k.finding)),
			outcome:  "Accepted at delivery as it is: " + strings.TrimSpace(k.finding),
			rejected: []string{"Repair it through a plan revision before delivery"}, rationale: "The owner approved delivery and accepted the finding instead of having it fixed.",
			created: d.At, updated: d.At})
	}
	return out
}

// followupDecisions are the chief of staff's scope calls on final-review
// findings: the gap is repaired by a follow-up unit after the work it
// concerns merged, rather than by reopening that work.
func (st *publishStream) followupDecisions() []publishedDecision {
	var out []publishedDecision
	for _, u := range st.followups {
		if u.Review < 1 || u.Amendment != "" {
			continue
		}
		at := st.followupAt(u)
		out = append(out, publishedDecision{
			id: decisionID(string(st.id()), "followup", u.Unit.ID), workstream: st.id(),
			context:   fmt.Sprintf("Final review %d found a gap against %s: %s", u.Review, u.Criterion, strings.TrimSpace(u.Gap)),
			outcome:   fmt.Sprintf("Repair the final-review gap against %s in follow-up unit %s, later than the merged work.", u.Criterion, u.Unit.ID),
			rationale: "The units serving the criterion had already merged and stay as they are, so a follow-up unit does the repair within the sealed slice.",
			rejected:  []string{"Reopen the merged units"}, created: at, updated: at})
	}
	return out
}

// followupAt is when the follow-up was recorded: the first revision of the
// workstream's follow-up record that holds it.
func (st *publishStream) followupAt(u followup.Unit) time.Time {
	var at time.Time
	for _, d := range st.documents {
		if d.ID != followup.DocumentID || (!at.IsZero() && !d.At.Before(at)) {
			continue
		}
		var batch []followup.Unit
		if json.Unmarshal([]byte(d.Content), &batch) != nil {
			continue
		}
		if slices.ContainsFunc(batch, func(b followup.Unit) bool { return b.Unit.ID == u.Unit.ID }) {
			at = d.At
		}
	}
	if at.IsZero() {
		return st.status.CreatedAt
	}
	return at
}

// charterDecisions are the charter proposals the owner decided: the rule the
// chief of staff drew from the owner's ruling, whether the owner ratified or
// declined it, and the owner's note. They are about the project's charter.
func (st *publishStream) charterDecisions() []publishedDecision {
	decided, err := trace.CharterDecisions(st.documents)
	if err != nil {
		return nil
	}
	var out []publishedDecision
	for _, c := range decided {
		p := c.Proposal
		question := p.Question
		if p.SourceQuestion != "" {
			question = p.SourceQuestion
		}
		origin := st.id()
		if p.SourceWorkstream != "" {
			origin = p.SourceWorkstream
		}
		d := publishedDecision{
			id: decisionID(string(st.id()), "charter", p.Question), by: byOwner,
			context:  fmt.Sprintf("The chief of staff drew a standing charter rule from the owner's ruling on question %s.\n\nOwner's ruling: %s\n\nProposed rule: %s", question, strings.TrimSpace(p.OwnerResponse), strings.TrimSpace(p.Rule)),
			question: string(origin) + ":" + question, created: c.Proposed.At, updated: c.Decided.At}
		note := strings.TrimSpace(p.Note)
		if p.Decision == trace.CharterRatify {
			d.outcome = "Ratified the charter rule: " + strings.TrimSpace(p.Rule)
			d.rejected = []string{"Leave the ruling as a one-off answer"}
			d.rationale = "The owner ratified the proposed rule."
		} else {
			d.outcome = "Declined the charter rule: " + strings.TrimSpace(p.Rule)
			d.rejected = []string{"Add the rule to the charter"}
			d.rationale = "The owner declined the proposed rule."
		}
		if note != "" {
			d.rationale += " Owner's note: " + note
		}
		out = append(out, d)
	}
	return out
}

// workgroupDecisions are the owner's rulings on amendments to a workgroup's
// project spec. Every member project posts them once on its own project, under
// the same ids.
func (in *publishInputs) workgroupDecisions() []publishedDecision {
	var out []publishedDecision
	seen := map[config.WorkgroupID]bool{}
	for _, st := range in.streams {
		g := st.group
		if g == nil || seen[g.ID] {
			continue
		}
		seen[g.ID] = true
		out = append(out, groupChangeDecisions(*g)...)
	}
	return out
}

func groupChangeDecisions(g trace.PublishedWorkgroup) []publishedDecision {
	type ruled struct {
		change lifecycle.WorkgroupChange
		at     time.Time
	}
	byID := map[string]*ruled{}
	var order []string
	docs := slices.Clone(g.Changes)
	slices.SortStableFunc(docs, func(a, b trace.Document) int { return a.Revision - b.Revision })
	for _, d := range docs {
		var c lifecycle.WorkgroupChange
		if json.Unmarshal([]byte(d.Content), &c) != nil || c.ID == "" {
			continue
		}
		if c.Request.Kind != "amend" || c.Decision == nil || (c.Decision.Decision != "approve" && c.Decision.Decision != "reject") {
			continue
		}
		if byID[c.ID] != nil {
			continue
		}
		byID[c.ID] = &ruled{c, d.At}
		order = append(order, c.ID)
	}
	all := make([]*ruled, 0, len(order))
	for _, id := range order {
		all = append(all, byID[id])
	}
	slices.SortStableFunc(all, func(a, b *ruled) int { return a.at.Compare(b.at) })

	var out []publishedDecision
	lastBySubject := map[string]string{}
	for _, r := range all {
		c := r.change
		approved := c.Decision.Decision == "approve"
		change := strings.TrimSpace(c.Request.Change)
		d := publishedDecision{
			id: decisionID(string(g.ID), "change", c.ID), by: byOwner, specDocument: workgroupSpecDocument(g.ID),
			created: r.at, updated: r.at}
		var b strings.Builder
		fmt.Fprintf(&b, "An amendment to the project spec of workgroup %s: %s", g.ID, change)
		if q := c.Requester; q != nil {
			fmt.Fprintf(&b, "\n\nRaised from %s, citing %s.\n\nReason: %s", c.Origin, strings.Join(q.Citations, ", "), strings.TrimSpace(q.Reason))
			if q.QuestionID != "" {
				d.question = string(q.Origin) + ":" + q.QuestionID
			}
		}
		d.context = b.String()
		if approved {
			d.outcome = "Approved the project spec amendment: " + change
			d.rejected = []string{"Keep the approved project spec unchanged"}
			d.rationale = "The owner approved the amendment after reading the impact summary."
		} else {
			d.outcome = "Rejected the project spec amendment: " + change
			d.rejected = []string{"Amend the project spec"}
			d.rationale = "The owner rejected the amendment after reading the impact summary."
		}
		if q := c.Requester; q != nil && len(q.Citations) > 0 {
			key := subject(q.Citations)
			if earlier, ok := lastBySubject[key]; ok && approved {
				d.supersedes = earlier
			}
			lastBySubject[key] = d.id
		}
		out = append(out, d)
	}
	return out
}
