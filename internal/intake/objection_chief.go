package intake

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
)

const ResolveObjectionTool = "resolve_objection"

// objectionGuidance tells the chief of staff how to handle the committee's
// objections at the ratification gate. It belongs in the system prompt of
// every chief-of-staff turn.
const ObjectionGuidance = "When debate concludes or the owner skips it, every committee objection that still stands comes to you before the owner sees the ratification packet. " +
	"Read each one in shed/round-<n>/ with the architect's reply, the spec, the plan, the charter and the knowledge base. When the record answers it, because the architect's reply or the drafted documents already address it, the charter rule it cites does not apply, or it is engineering advice the plan can carry, call resolve_objection with dismiss and a note saying what answers it. " +
	"Call resolve_objection with escalate and a short note for the owner, saying what the objection is about, the finding that matters and what you recommend, only when it needs the owner: it would change approved intent, needs an owner-reserved tradeoff, or you are not sure the record answers it. " +
	"The owner's inbox lists the ratification once you have decided every committee objection, and shows the owner only the objections you escalated and the owner's own."

// objectionDecisionRequest is the chief of staff's decision on one committee
// objection.
type ObjectionDecisionRequest struct {
	Objection string
	Decision  shed.Decide
	Note      string
	Turn      string
}

// decideObjection records the chief of staff's decision on one committee
// objection that stands at the workstream's ratification gate: dismissed on
// the owner's behalf, or escalated to the owner. It is refused before debate
// concluded or was skipped, for the owner's own objections, for an objection
// the owner already ruled on and for one the chief of staff already decided.
func (s *Controller) DecideObjection(ctx context.Context, repository *trace.Repository, stream config.WorkstreamID, req ObjectionDecisionRequest) (shed.Entry, *APIError) {
	failed := &APIError{Code: Internal, Message: fmt.Sprintf("cannot read the shed of workstream %s; check the trace repository", stream)}
	feature, err := repository.Workflow(stream, trace.FeatureSubject)
	if err != nil {
		return shed.Entry{}, failed
	}
	if feature.Value != InShedState {
		return shed.Entry{}, &APIError{Code: Conflict, Message: fmt.Sprintf("workstream %s is %s; objections are decided in the shed", stream, FeatureState(feature.Value))}
	}
	state, err := repository.Workflow(stream, ShedSubject)
	if err != nil {
		return shed.Entry{}, failed
	}
	skipped, err := SkippedDebate(repository, stream)
	if err != nil {
		return shed.Entry{}, failed
	}
	kind, round, ok := ShedState(state.Value)
	if !ok {
		round = 1
	}
	if !skipped && kind != "concluded" {
		return shed.Entry{}, &APIError{Code: Conflict, Message: fmt.Sprintf("debate on workstream %s has not concluded; decide its objections once the ratification packet is presented", stream)}
	}
	entries, err := Dissent(repository, stream)
	if err != nil {
		return shed.Entry{}, failed
	}
	i := slices.IndexFunc(entries, func(e shed.Entry) bool { return e.ID == req.Objection })
	switch {
	case i < 0:
		return shed.Entry{}, &APIError{Code: NotFound, Message: fmt.Sprintf("no objection %s stands in workstream %s", req.Objection, stream)}
	case entries[i].Kind == shed.CharterConflict:
		return shed.Entry{}, &APIError{Code: Conflict, Message: "a conflict between member charters is escalated to the owner; the chief cannot resolve it"}
	case entries[i].Member == shed.OwnerMember:
		return shed.Entry{}, &APIError{Code: Conflict, Message: fmt.Sprintf("objection %s is the owner's own; the owner decides it", req.Objection)}
	case entries[i].By == shed.ChiefOfStaff || entries[i].Escalated:
		return shed.Entry{}, &APIError{Code: Conflict, Message: fmt.Sprintf("you already decided objection %s; the owner rules on it now", req.Objection)}
	case entries[i].Disposition != "":
		return shed.Entry{}, &APIError{Code: Conflict, Message: fmt.Sprintf("the owner already ruled %s on objection %s", entries[i].Disposition, req.Objection)}
	}
	pin, err := LatestPin(repository, stream)
	if err != nil {
		return shed.Entry{}, &APIError{Code: Conflict, Message: fmt.Sprintf("workstream %s has no recorded spec and plan yet", stream)}
	}
	rounds, err := shed.AllChiefDecisions(repository, stream)
	if err != nil {
		return shed.Entry{}, failed
	}
	decisions := shed.ChiefDecisions{Version: shed.Version, Round: round, Revision: pin}
	for _, r := range rounds {
		if r.Round == round {
			decisions = r
		}
	}
	decisions.Decisions = append(slices.Clone(decisions.Decisions), shed.ChiefDecision{Objection: req.Objection, Decision: req.Decision, Note: req.Note, Turn: req.Turn})
	content, err := shed.EncodeChiefDecisions(decisions)
	if err != nil {
		return shed.Entry{}, &APIError{Code: Validation, Message: "the decision cannot be recorded: " + err.Error()}
	}
	docs, err := trace.Read[trace.Document](repository, stream)
	if err != nil {
		return shed.Entry{}, failed
	}
	revision := 1
	for _, d := range docs {
		if d.Path == shed.ChiefPath(round) {
			revision = d.Revision + 1
		}
	}
	h := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: shed.ChiefDocumentID(round), Revision: revision, Project: repository.Project(), Workstream: stream, At: s.Now(), Actor: ChiefActor, Cause: req.Objection}
	if err := repository.RecordDocuments(ctx, []trace.Document{{Header: h, Path: shed.ChiefPath(round), Content: string(content)}}); err != nil {
		return shed.Entry{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot record the decision on objection %s of workstream %s; check the trace repository", req.Objection, stream)}
	}
	entries, err = Dissent(repository, stream)
	if err != nil {
		return shed.Entry{}, failed
	}
	i = slices.IndexFunc(entries, func(e shed.Entry) bool { return e.ID == req.Objection })
	if i < 0 {
		return shed.Entry{}, failed
	}
	return entries[i], nil
}

// resolveObjection returns the resolve_objection tool of one claimed
// chief-of-staff turn. The chief of staff dismisses a committee objection at
// the ratification gate on the owner's behalf, or escalates it to the owner.
// A decision the service refuses is an ordinary result,
// {"recorded":false,"reason":...}, and records nothing. A shared chief of
// staff names the member whose debate the objection belongs to.
func (c *Controls) ResolveObjection(repository *trace.Repository, scope coreadapter.Scope) coreadapter.Tool {
	tool := coreadapter.Tool{Name: ResolveObjectionTool, Effect: coreadapter.ToolMemory,
		Description: "Decide a committee objection that stands once this workstream's debate concluded, before the owner sees the ratification packet. objection: its ID; decision: dismiss (the record answers it: it no longer blocks and is not put to the owner) or escalate (the owner rules on it); " +
			"note: for dismiss, what answers the objection, citing the architect's reply, the spec, the plan, the charter or the knowledge base; for escalate, in a few short sentences for the owner, what the objection is about, the finding that matters and what you recommend. Dismiss only when you are confident the record answers it; otherwise escalate. The owner's own objections are the owner's. workstream: a member workstream ID of your workgroup to act on; omit for this conversation's own workstream.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"objection":{"type":"string"},"decision":{"type":"string","enum":["dismiss","escalate"]},"note":{"type":"string"},"workstream":{"type":"string"}},"required":["objection","decision","note"],"additionalProperties":false}`)}
	tool.Handle = func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
		var input struct {
			Objection  string              `json:"objection"`
			Decision   string              `json:"decision"`
			Note       string              `json:"note"`
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
		s := c.Load()
		if s == nil {
			return nil, errors.New("objection decisions are unavailable")
		}
		if err := repository.ActiveTurn(scope); err != nil {
			return nil, err
		}
		target, stream, refusal, err := repository.ChiefMember(config.WorkstreamID(scope.Workstream), input.Workstream)
		if err != nil {
			return nil, err
		}
		if refusal != "" {
			return PriorityRefusal(refusal)
		}
		decision := shed.Decide(input.Decision)
		if decision != shed.Dismiss && decision != shed.Escalate {
			return PriorityRefusal("a decision is dismiss or escalate")
		}
		note := strings.TrimSpace(input.Note)
		if note == "" {
			return PriorityRefusal("a note is required")
		}
		entry, api := s.DecideObjection(ctx, target, stream, ObjectionDecisionRequest{Objection: strings.TrimSpace(input.Objection), Decision: decision, Note: note, Turn: scope.Turn})
		if api != nil {
			if api.Code == Internal {
				return nil, errors.New(api.Message)
			}
			return PriorityRefusal(api.Message)
		}
		detail := "the objection is dismissed on the owner's behalf and is not put to the owner"
		if decision == shed.Escalate {
			detail = "the objection is in the owner's ratification packet with your note"
		}
		return json.Marshal(struct {
			Recorded  bool   `json:"recorded"`
			Objection string `json:"objection"`
			Blocking  bool   `json:"blocking"`
			Detail    string `json:"detail"`
		}{true, entry.ID, entry.Blocking, detail})
	}
	return tool
}
