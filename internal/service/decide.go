package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/jev"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
)

const decideTool = "decide"

// Bounds on what a decision's state shows Jev from the records.
const (
	decideSpec     = 24 << 10
	decidePlan     = 16 << 10
	decideCharter  = 8 << 10
	decideEvidence = 8 << 10
	decideRulings  = 8
)

// decisionSubject is a subject the chief of staff may decide today, prepared
// for Jev: the occurrence, the options Jev may pick and the state built from
// the subject's records.
type decisionSubject struct {
	repository *trace.Repository
	stream     config.WorkstreamID
	unit       string
	kind       jev.DecisionKind
	key        string
	options    []jev.DecisionOption
	// state builds the state with the chief of staff's evidence summary.
	state func(evidence string) (string, []jev.Source, error)
}

func decisionOptions(pairs ...string) []jev.DecisionOption {
	out := make([]jev.DecisionOption, 0, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, jev.DecisionOption{Name: pairs[i], Description: pairs[i+1]})
	}
	return out
}

// decide returns the decide tool of one claimed chief-of-staff turn. The chief
// of staff names a subject it may decide and gives its evidence; the service
// checks from the subject's records that the chief may decide it today, builds
// the rest of the state and asks Jev. A subject that is the owner's is refused
// as an ordinary result, with no request and no judgment. The chief's outcome
// tool for the subject records its decision only after this result exists.
func (c *runtimeControls) decide(repository *trace.Repository, scope coreadapter.Scope) coreadapter.Tool {
	tool := coreadapter.Tool{Name: decideTool, Effect: coreadapter.ToolMemory,
		Description: "Ask Jev to decide a subject you may decide, after you investigated it. kind: question-routing (subject: a question's number; Jev picks answer, route_amendment or escalate), contested-ruling (subject: a contested unit's ID; Jev picks review, revise or escalate) or objection (subject: a committee objection's ID; Jev picks dismiss or escalate). " +
			"evidence: a short summary of what you found and why, which Jev reads with the subject's records; Jev never reads code. options: only for a kind whose options you write; the kinds above have fixed options. " +
			"workstream: a member workstream ID of your workgroup to act on; omit for this conversation's own workstream. The result says Jev's pick and confidence when it is binding: the outcome tool of the subject then records only that outcome, with your own text. " +
			"Otherwise the decision is yours, as it was before. Calling decide again for the same subject returns the same result. A subject that is the owner's, such as a conflict between member charters or the owner's own objection, is refused. " +
			"The outcome tools (answer, escalate, route_amendment, resolve_contested, resolve_objection) refuse a subject you have not called decide for.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"kind":{"type":"string","enum":["question-routing","contested-ruling","objection"]},"subject":{"type":"string"},"evidence":{"type":"string"},"options":{"type":"array","items":{"type":"object","properties":{"name":{"type":"string"},"description":{"type":"string"}},"required":["name","description"],"additionalProperties":false}},"workstream":{"type":"string"}},"required":["kind","subject","evidence"],"additionalProperties":false}`)}
	tool.Handle = func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
		var input struct {
			Kind     jev.DecisionKind `json:"kind"`
			Subject  string           `json:"subject"`
			Evidence string           `json:"evidence"`
			Options  []struct {
				Name        string `json:"name"`
				Description string `json:"description"`
			} `json:"options"`
			Workstream config.WorkstreamID `json:"workstream"`
		}
		d := json.NewDecoder(bytes.NewReader(raw))
		d.DisallowUnknownFields()
		if err := d.Decode(&input); err != nil {
			return nil, fmt.Errorf("tool input: %w", err)
		}
		if err := d.Decode(new(any)); err != io.EOF {
			return nil, errors.New("tool input must be one object")
		}
		s := c.service.Load()
		if s == nil {
			return nil, errors.New("decisions are unavailable")
		}
		if err := repository.ActiveTurn(scope); err != nil {
			return nil, err
		}
		refuse := func(reason string) (json.RawMessage, error) { return priorityRefusal(reason) }
		evidence := strings.TrimSpace(input.Evidence)
		if evidence == "" {
			return refuse("evidence is required: a short summary of what you found")
		}
		if len(input.Options) > 0 {
			return refuse(fmt.Sprintf("the options of %s are fixed; omit options", input.Kind))
		}
		subject := strings.TrimSpace(input.Subject)
		if subject == "" {
			return refuse("a subject is required")
		}
		target, stream, refusal, err := repository.ChiefMember(config.WorkstreamID(scope.Workstream), input.Workstream)
		if err != nil {
			return nil, err
		}
		if refusal != "" {
			return refuse(refusal)
		}
		var sub decisionSubject
		var why string
		switch input.Kind {
		case jev.KindQuestionRouting:
			sub, why, err = s.questionSubject(ctx, repository, config.WorkstreamID(scope.Workstream), subject)
		case jev.KindContestedRuling:
			sub, why, err = s.contestSubject(ctx, target, stream, subject)
		case jev.KindObjection:
			sub, why, err = s.objectionSubject(ctx, target, stream, subject)
		default:
			return refuse(fmt.Sprintf("decide covers question-routing, contested-ruling and objection; %q is not one", input.Kind))
		}
		var refused *trace.QuestionRefused
		if errors.As(err, &refused) {
			return refuse(refused.Reason)
		}
		if err != nil {
			return nil, err
		}
		if why != "" {
			return refuse(why)
		}
		text, sources, err := sub.state(limitEvidence(evidence))
		if err != nil {
			return nil, err
		}
		res := s.jev.Decide(ctx, sub.repository, jev.DecisionRequest{
			Scope: coreadapter.Scope{Project: string(sub.repository.Project()), Workstream: string(sub.stream), Unit: sub.unit, Thread: scope.Thread, Turn: scope.Turn, Role: scope.Role},
			Kind:  sub.kind, Occurrence: sub.key, State: text, Options: sub.options, Sources: sources})
		return decisionResult(sub, res)
	}
	return tool
}

func limitEvidence(s string) string {
	if len(s) <= decideEvidence {
		return s
	}
	cut := decideEvidence
	for cut > 0 && s[cut]&0xC0 == 0x80 {
		cut--
	}
	return s[:cut] + " [truncated]"
}

// decisionResult tells the chief of staff what Jev's decision is: a binding
// pick with its confidence, or that the decision is the chief's.
func decisionResult(sub decisionSubject, res jev.Result) (json.RawMessage, error) {
	type result struct {
		Decision   string  `json:"decision"`
		Kind       string  `json:"kind"`
		Pick       string  `json:"pick,omitempty"`
		Confidence float64 `json:"confidence,omitempty"`
		Next       string  `json:"next"`
	}
	switch {
	case res.Status == jev.Binding && res.Refused == "":
		return json.Marshal(result{"binding", string(sub.kind), res.Pick, res.Confidence,
			fmt.Sprintf("Jev decided %s with confidence %.2f. The outcome tool now records only %s, and you write the text it needs.", res.Pick, res.Confidence, res.Pick)})
	case res.Status == jev.Binding:
		return json.Marshal(result{Decision: "chief", Kind: string(sub.kind), Next: "Jev's decision could not be carried out and the subject is with the owner."})
	case res.Status == jev.None || res.Status == jev.FallbackOnly && res.Judgment == "":
		return json.Marshal(result{Decision: "unavailable", Kind: string(sub.kind), Next: "Jev's decision could not be recorded now. Call decide again; the outcome tool refuses this subject until a result exists."})
	}
	return json.Marshal(result{Decision: "chief", Kind: string(sub.kind), Next: "Jev's decision is not binding here. The decision is yours: record any outcome the record supports."})
}

// stateSections builds the state a decision's request shows.
type stateSections struct {
	strings.Builder
}

func (b *stateSections) add(name, text string, limit int) {
	intake.GateSection(&b.Builder, name, text, limit)
}

// sealedText returns the sealed spec and plan documents of the workstream, when
// it is sealed.
func sealedText(repository *trace.Repository, stream config.WorkstreamID) (specText, planText string, sources []jev.Source) {
	current, _, found, err := seal.Latest(repository, stream)
	if err != nil || !found {
		return "", "", nil
	}
	docs, err := trace.Read[trace.Document](repository, stream)
	if err != nil {
		return "", "", nil
	}
	for _, d := range docs {
		switch {
		case d.ID == plan.SpecDocument && d.Revision == current.Revision.Spec:
			specText = d.Content
			sources = append(sources, jev.Source{Kind: "spec", ID: d.ID, Revision: d.Revision})
		case d.ID == plan.PlanDocument && d.Revision == current.Revision.Plan:
			planText = d.Content
			sources = append(sources, jev.Source{Kind: "plan", ID: d.ID, Revision: d.Revision})
		}
	}
	return specText, planText, sources
}

func (s *Service) charterSection(ctx context.Context, repository *trace.Repository) string {
	doc, _, err := s.loadCharter(ctx, repository.Project())
	if err != nil {
		return ""
	}
	return doc.Content
}

// unitSection states a sealed unit's assignment.
func unitSection(repository *trace.Repository, stream config.WorkstreamID, unit string) string {
	if unit == "" {
		return ""
	}
	u, err := sealedUnit(repository, coreadapter.Scope{Workstream: string(stream), Unit: unit})
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%s: %s\nAcceptance:\n- %s", u.ID, u.Task, strings.Join(u.Acceptance, "\n- "))
}

// questionSubject prepares the open question id for routing, or says why the
// chief of staff may not route it with Jev.
func (s *Service) questionSubject(ctx context.Context, repository *trace.Repository, host config.WorkstreamID, id string) (decisionSubject, string, error) {
	q, why, err := questions.RoutingSubjectOf(repository, host, id)
	if err != nil || why != "" {
		return decisionSubject{}, why, err
	}
	asked := q.Question.Asked
	return decisionSubject{repository: q.Repository, stream: q.Stream, unit: asked.Unit, kind: jev.KindQuestionRouting, key: q.Key(),
		options: decisionOptions(
			questions.OutcomeAnswer, "Answer the question within the approved intent: it is an engineering decision the spec, the plan and the owner's rulings leave to the chief of staff.",
			questions.OutcomeRouteAmendment, "Route the question as an amendment: it needs revised assignments, dependencies, newly discovered work, or a change to the plan, which the amendment flow decides.",
			questions.OutcomeEscalate, "Escalate the question to the owner: answering it would contradict or extend the agreed design, relax a constraint, contradict an owner ruling, or needs an owner-reserved tradeoff."),
		state: func(evidence string) (string, []jev.Source, error) {
			var b stateSections
			b.add("Decision", fmt.Sprintf("The chief of staff routes an open question that a %s asked while building workstream %s. Choose how it is routed.", q.Role, q.Stream), 0)
			b.add("Question", asked.Question, 0)
			b.add("The asker's unit", unitSection(q.Repository, q.Stream, asked.Unit), 0)
			b.add("Chief of staff's evidence", evidence, 0)
			if rulings := ownerRulings(q.Repository, q.Stream); rulings != "" {
				b.add("The owner's rulings on this workstream", rulings, 0)
			}
			specText, planText, sources := sealedText(q.Repository, q.Stream)
			b.add("Sealed spec", specText, decideSpec)
			b.add("Sealed plan", planText, decidePlan)
			b.add("Charter", s.charterSection(ctx, q.Repository), decideCharter)
			return b.String(), append([]jev.Source{{Kind: "question", ID: asked.ID, Revision: q.Question.Latest.Revision}}, sources...), nil
		}}, "", nil
}

// ownerRulings lists the newest owner rulings on the workstream's questions.
func ownerRulings(repository *trace.Repository, stream config.WorkstreamID) string {
	all, err := repository.Questions(stream)
	if err != nil {
		return ""
	}
	var lines []string
	for _, q := range all {
		if r := q.Ruling; r != nil && r.Decision == trace.DecisionRuling && strings.TrimSpace(r.OwnerResponse) != "" && r.DecidedBy != trace.DecidedByJev {
			lines = append(lines, fmt.Sprintf("- Question %s: %s -> %s", q.Asked.ID, strings.TrimSpace(q.Asked.Question), strings.TrimSpace(r.OwnerResponse)))
		}
	}
	if len(lines) > decideRulings {
		lines = lines[len(lines)-decideRulings:]
	}
	return strings.Join(lines, "\n")
}

// contestSubject prepares the contested unit for a ruling, or says why the
// chief of staff may not rule on it with Jev: the unit is not contested, it was
// moved there for the owner, or its contest was already decided.
func (s *Service) contestSubject(ctx context.Context, repository *trace.Repository, stream config.WorkstreamID, unit string) (decisionSubject, string, error) {
	contest, contested, err := unitContest(repository, stream, unit)
	if err != nil {
		return decisionSubject{}, "", err
	}
	if !contested {
		return decisionSubject{}, fmt.Sprintf("unit %s is not contested; move_unit moves a unit that is not", unit), nil
	}
	if moveContest(contest, unit) {
		return decisionSubject{}, fmt.Sprintf("unit %s was moved to contested for the owner; the owner rules on it", unit), nil
	}
	raised, _, err := contestRaised(repository, stream, unit, contest)
	if err != nil {
		return decisionSubject{}, "", err
	}
	if raised {
		return decisionSubject{}, fmt.Sprintf("you already decided contest %s of unit %s; the owner rules on it", contest.ID, unit), nil
	}
	return decisionSubject{repository: repository, stream: stream, unit: unit, kind: jev.KindContestedRuling, key: jev.ContestKey(contest.ID),
		options: decisionOptions(
			"review", "Have the unit's candidate reviewed again; the unit continues from that review.",
			"revise", "Send the unit back to its mason to revise it, with the chief of staff's note on what to change.",
			"escalate", "Put the contest to the owner: the fix changes approved intent, needs an owner-reserved tradeoff, or the contest cannot be resolved by engineering."),
		state: func(evidence string) (string, []jev.Source, error) {
			var b stateSections
			b.add("Decision", fmt.Sprintf("Unit %s of workstream %s is contested. The chief of staff rules on it first, on the owner's behalf. Choose how it is ruled.", unit, stream), 0)
			b.add("The contest", fmt.Sprintf("Raised at %s: %s", contest.At.UTC().Format("2006-01-02 15:04 MST"), contest.Reason), 0)
			b.add("The unit", unitSection(repository, stream, unit), 0)
			b.add("Chief of staff's evidence", evidence, 0)
			specText, planText, sources := sealedText(repository, stream)
			b.add("Sealed spec", specText, decideSpec)
			b.add("Sealed plan", planText, decidePlan)
			b.add("Charter", s.charterSection(ctx, repository), decideCharter)
			return b.String(), append([]jev.Source{{Kind: "contest", ID: contest.ID}}, sources...), nil
		}}, "", nil
}

// objectionSubject prepares the committee objection for a decision, or says why
// the chief of staff may not decide it with Jev: a conflict between member
// charters and the owner's own objection are the owner's, and an objection must
// stand at the ratification gate.
func (s *Service) objectionSubject(ctx context.Context, repository *trace.Repository, stream config.WorkstreamID, id string) (decisionSubject, string, error) {
	entry, round, api := s.intake().ObjectionSubject(repository, stream, id)
	if api != nil {
		if api.Code == intake.Internal {
			return decisionSubject{}, "", errors.New(api.Message)
		}
		return decisionSubject{}, api.Message, nil
	}
	return decisionSubject{repository: repository, stream: stream, kind: jev.KindObjection, key: jev.ObjectionKey(entry.ID),
		options: decisionOptions(
			string(shed.Dismiss), "Dismiss the objection on the owner's behalf: the record answers it, so it blocks no longer and is not put to the owner.",
			string(shed.Escalate), "Escalate the objection: the owner rules on it at the ratification gate."),
		state: func(evidence string) (string, []jev.Source, error) {
			var b stateSections
			b.add("Decision", fmt.Sprintf("Committee objection %s of workstream %s stands at the ratification gate. The chief of staff decides it before the owner sees the packet. Choose whether it is dismissed or escalated.", entry.ID, stream), 0)
			part := ""
			if entry.Part != "" {
				part = " on " + entry.Part
			}
			b.add("The objection", fmt.Sprintf("%s (%s, by %s in round %d%s, against %s): %s", entry.ID, entry.Kind, entry.Member, entry.Round, part, entry.Revision, entry.Argument), 0)
			b.add("Chief of staff's evidence", evidence, 0)
			var sources []jev.Source
			if packet, doc, found, err := shed.LatestPacket(repository, stream); err == nil && found {
				b.add("Ratification packet", fmt.Sprintf("Round %d, %s. %s", packet.Round, packet.Revision, packet.Conclusion), 0)
				docs, _ := trace.Read[trace.Document](repository, stream)
				for _, d := range docs {
					switch {
					case d.ID == plan.SpecDocument && d.Revision == packet.Revision.Spec:
						b.add(fmt.Sprintf("Spec (revision %d)", d.Revision), d.Content, decideSpec)
					case d.ID == plan.PlanDocument && d.Revision == packet.Revision.Plan:
						b.add(fmt.Sprintf("Plan (revision %d)", d.Revision), d.Content, decidePlan)
					}
				}
				sources = append(sources, jev.Source{Kind: "packet", ID: shed.PacketDocumentID(packet.Round), Revision: doc.Revision})
			}
			b.add("Charter", s.charterSection(ctx, repository), decideCharter)
			return b.String(), append([]jev.Source{{Kind: "objection", ID: entry.ID}, {Kind: "chief-decisions", ID: shed.ChiefDocumentID(round)}}, sources...), nil
		}}, "", nil
}

