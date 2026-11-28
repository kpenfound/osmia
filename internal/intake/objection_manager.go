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
	"github.com/kpenfound/osmia/internal/jev"
	"github.com/kpenfound/osmia/internal/router"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
)

const ResolveObjectionTool = "resolve_objection"

// ObjectionGuidance tells the manager how to handle the committee's
// objections at the ratification gate. It belongs in the system prompt of
// every manager turn.
const ObjectionGuidance = "## Committee objections\n" +
	"When debate concludes or the owner skips it, every committee objection that still stands comes to you before the owner sees the ratification packet, so the owner reads only what needs the owner. " +
	"Read each one in shed/round-<n>/ with the architect's reply, the spec, the plan, the charter and the entity map. Then call decide with kind objection, the objection's ID as subject and a short evidence summary: Jev decides whether it is dismissed or escalated. " +
	"When Jev's decision is binding, resolve_objection records only the outcome it picked, and you still write the note. When decide says the decision is yours, decide as follows. resolve_objection refuses an objection decide has not been called for.\n\n" +
	"- Call resolve_objection with dismiss and a note saying what answers it when the record answers the objection: the architect's reply or the drafted documents already address it, the charter rule it cites does not apply, or it is engineering advice the plan can carry.\n" +
	"- Call resolve_objection with escalate and a short note for the owner only when the objection needs the owner: it would change approved intent, needs an owner-reserved tradeoff, or you are not sure the record answers it. The note says what the objection is about, the finding that matters and what you recommend.\n\n" +
	"The owner's inbox lists the ratification once you have decided every committee objection, and shows the owner only the objections you escalated and the owner's own."

// ObjectionDecisionRequest is the manager's decision on one committee
// objection.
type ObjectionDecisionRequest struct {
	Objection string
	Decision  shed.Decide
	Note      string
	Turn      string
	// Judgment is the Jev judgment record whose binding pick the decision
	// carries out, or empty.
	Judgment string
}

// ObjectionSubject returns the committee objection the manager may
// decide today, and the round of the shed it stands in. It is refused before
// debate concluded or was skipped, for a conflict between member charters, for
// the owner's own objections, for an objection the owner already ruled on and
// for one the manager already decided. The same check runs before Jev is
// asked about the objection.
func (s *Controller) ObjectionSubject(repository *trace.Repository, stream config.WorkstreamID, objection string) (shed.Entry, int, *APIError) {
	failed := &APIError{Code: Internal, Message: fmt.Sprintf("cannot read the shed of workstream %s; check the trace repository", stream)}
	feature, err := repository.Workflow(stream, trace.FeatureSubject)
	if err != nil {
		return shed.Entry{}, 0, failed
	}
	if feature.Value != InShedState {
		return shed.Entry{}, 0, &APIError{Code: Conflict, Message: fmt.Sprintf("workstream %s is %s; objections are decided in the shed", stream, FeatureState(feature.Value))}
	}
	state, err := repository.Workflow(stream, ShedSubject)
	if err != nil {
		return shed.Entry{}, 0, failed
	}
	skipped, err := SkippedDebate(repository, stream)
	if err != nil {
		return shed.Entry{}, 0, failed
	}
	kind, round, ok := ShedState(state.Value)
	if !ok {
		round = 1
	}
	if !skipped && kind != "concluded" {
		return shed.Entry{}, 0, &APIError{Code: Conflict, Message: fmt.Sprintf("debate on workstream %s has not concluded; decide its objections once the ratification packet is presented", stream)}
	}
	entries, err := Dissent(repository, stream)
	if err != nil {
		return shed.Entry{}, 0, failed
	}
	i := slices.IndexFunc(entries, func(e shed.Entry) bool { return e.ID == objection })
	switch {
	case i < 0:
		return shed.Entry{}, 0, &APIError{Code: NotFound, Message: fmt.Sprintf("no objection %s stands in workstream %s", objection, stream)}
	case entries[i].Kind == shed.CharterConflict:
		return shed.Entry{}, 0, &APIError{Code: Conflict, Message: "a conflict between member charters is escalated to the owner; the chief cannot resolve it"}
	case entries[i].Member == shed.OwnerMember:
		return shed.Entry{}, 0, &APIError{Code: Conflict, Message: fmt.Sprintf("objection %s is the owner's own; the owner decides it", objection)}
	case entries[i].By == trace.Manager || entries[i].Escalated:
		return shed.Entry{}, 0, &APIError{Code: Conflict, Message: fmt.Sprintf("you already decided objection %s; the owner rules on it now", objection)}
	case entries[i].Disposition != "":
		return shed.Entry{}, 0, &APIError{Code: Conflict, Message: fmt.Sprintf("the owner already ruled %s on objection %s", entries[i].Disposition, objection)}
	}
	return entries[i], round, nil
}

// The machine and trigger of a committee objection's exit, and the states its
// subject enters: dismissed on the owner's behalf, or escalated to the owner.
const (
	ObjectionMachine         = "objection"
	TriggerObjectionDecision = "objection.chief_decision"
	ObjectionDismissed       = "dismissed"
	ObjectionEscalated       = "escalated"
)

func objectionSubject(objection string) string { return "objection-" + objection }

func objectionTarget(d shed.Decide) string {
	if d == shed.Dismiss {
		return ObjectionDismissed
	}
	return ObjectionEscalated
}

func objectionDecision(target string) shed.Decide {
	if target == ObjectionDismissed {
		return shed.Dismiss
	}
	return shed.Escalate
}

func (s *Controller) objectionDocument(repository *trace.Repository, stream config.WorkstreamID, objection string, decision shed.Decide, note, turn, judgment string) (trace.Document, *APIError) {
	failed := &APIError{Code: Internal, Message: fmt.Sprintf("cannot read the shed of workstream %s; check the trace repository", stream)}
	_, round, api := s.ObjectionSubject(repository, stream, objection)
	if api != nil {
		return trace.Document{}, api
	}
	pin, err := LatestPin(repository, stream)
	if err != nil {
		return trace.Document{}, &APIError{Code: Conflict, Message: fmt.Sprintf("workstream %s has no recorded spec and plan yet", stream)}
	}
	rounds, err := shed.AllManagerDecisions(repository, stream)
	if err != nil {
		return trace.Document{}, failed
	}
	decisions := shed.ManagerDecisions{Version: shed.Version, Round: round, Revision: pin}
	for _, r := range rounds {
		if r.Round == round {
			decisions = r
		}
	}
	chosen := shed.ManagerDecision{Objection: objection, Decision: decision, Note: note, Turn: turn}
	if judgment != "" {
		chosen.DecidedBy, chosen.Judgment = trace.DecidedByJev, judgment
	}
	decisions.Decisions = append(slices.Clone(decisions.Decisions), chosen)
	content, err := shed.EncodeManagerDecisions(decisions)
	if err != nil {
		return trace.Document{}, &APIError{Code: Validation, Message: "the decision cannot be recorded: " + err.Error()}
	}
	docs, err := trace.Read[trace.Document](repository, stream)
	if err != nil {
		return trace.Document{}, failed
	}
	revision := 1
	for _, d := range docs {
		if d.Path == shed.ManagerPath(round) {
			revision = d.Revision + 1
		}
	}
	h := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: shed.ManagerDocumentID(round), Revision: revision, Project: repository.Project(), Workstream: stream, At: s.Now(), Actor: ManagerActor, Cause: objection}
	return trace.Document{Header: h, Path: shed.ManagerPath(round), Content: string(content)}, nil
}

// RegisterObjectionEffects defines, in the router's registry, the effect of
// entering each state a committee objection's decision leads to: the record of
// the manager's decision. A decision Jev made, whether it departed from
// the manager's or a binding pick the manager carries out, names Jev and its
// judgment. load returns the intake controller.
func RegisterObjectionEffects(reg *router.Effects, load func() *Controller) {
	for _, target := range []string{ObjectionDismissed, ObjectionEscalated} {
		reg.Register(ObjectionMachine, target, func(_ context.Context, p router.Plan) (router.Effect, error) {
			s := load()
			if s == nil {
				return router.Effect{}, errors.New("objection decisions are unavailable")
			}
			x := p.Exit
			note, judgment := x.Note, ""
			switch p.Routing.DecidedBy {
			case trace.DecidedByJev:
				judgment = p.Routing.Judgment
				note = fmt.Sprintf("Jev decided %s instead of the chief of staff's %s (judgment %s). The chief of staff's note: %s", objectionDecision(p.Target.Name), x.Default, judgment, x.Note)
			case trace.DecidedByJevGate:
				judgment = p.Routing.Judgment
			}
			doc, api := s.objectionDocument(x.Repo, x.Workstream, x.HeaderCause, objectionDecision(p.Target.Name), note, x.Turn, judgment)
			switch {
			case api == nil:
				return router.Effect{Documents: []trace.Document{doc}}, nil
			case api.Code == Internal:
				return router.Effect{}, errors.New(api.Message)
			}
			return router.Effect{}, router.Refusal(api.Message)
		})
	}
}

func (s *Controller) objectionExit(repository *trace.Repository, stream config.WorkstreamID, req ObjectionDecisionRequest) (router.Exit, error) {
	state, err := repository.Workflow(stream, objectionSubject(req.Objection))
	if err != nil {
		return router.Exit{}, err
	}
	def := objectionTarget(req.Decision)
	x := router.Exit{
		Repo: repository, Project: repository.Project(), Workstream: stream, Machine: ObjectionMachine,
		Subject: objectionSubject(req.Objection), From: state.Value, Version: state.Version, Trigger: TriggerObjectionDecision,
		Cause: req.Objection + "-" + string(req.Decision), HeaderCause: req.Objection, Actor: ManagerActor, Note: req.Note, Turn: req.Turn,
		At:      s.Now(),
		Default: def,
		Targets: []router.Target{
			{Name: ObjectionDismissed, Bar: router.BarStrict, Description: "Dismiss the objection on the owner's behalf: the record answers it, so it blocks no longer and is not put to the owner.",
				Corroborate: []router.Condition{{Question: "record_answers", AtLeast: router.LevelStrict}}},
			{Name: ObjectionEscalated, Bar: router.BarModest, Description: "Escalate the objection: the owner rules on it at the ratification gate."},
		},
		Questions: []router.Question{
			{Name: "record_answers", Instructions: "The architect's reply, the spec, the plan or the charter answers the objection, so it needs no change."},
			{Name: "charter_veto", Instructions: "The objection cites a charter rule that the plan or its units break."},
		},
		TransitionID: func(t router.Target) string { return objectionSubject(req.Objection) + "-" + t.Name },
		Evidence: router.Evidence{
			Intent:  "The committee's objection stands at the ratification gate. The chief of staff dismisses it only when the record answers it, and escalates it when it needs the owner.",
			Records: []router.Entry{{Kind: "chief-note", Text: req.Note}},
		},
		Prereq: router.Prerequisites{Other: func(_ context.Context, _ router.Target) (string, error) {
			if _, _, api := s.ObjectionSubject(repository, stream, req.Objection); api != nil {
				if api.Code == Internal {
					return "", errors.New(api.Message)
				}
				return api.Message, nil
			}
			return "", nil
		}},
	}
	if entry, _, api := s.ObjectionSubject(repository, stream, req.Objection); api == nil {
		x.Evidence.Records = append([]router.Entry{{Kind: "objection", ID: entry.ID, Text: fmt.Sprintf("%s by %s against %s: %s", entry.Kind, entry.Member, entry.Part, entry.Argument)}}, x.Evidence.Records...)
	}
	if req.Judgment != "" {
		x.Authority = &router.Authority{Kind: router.AuthorityJevGate, Target: def, Judgment: req.Judgment}
	} else {
		x.Authority = &router.Authority{Kind: router.AuthorityManager, Target: def}
	}
	return x, nil
}

// DecideObjection records the manager's decision on one committee
// objection that stands at the workstream's ratification gate: dismissed on
// the owner's behalf, or escalated to the owner. It is refused as
// ObjectionSubject describes. A decision is a pinned move: Jev's binding pick
// and the manager's own decision are each entered as they were made. A non-empty
// Judgment is the Jev judgment record whose binding pick the decision carries
// out; the decision then names Jev as its decider and the judgment as its
// source. A decision the router holds records nothing.
func (s *Controller) DecideObjection(ctx context.Context, repository *trace.Repository, stream config.WorkstreamID, req ObjectionDecisionRequest) (shed.Entry, *APIError) {
	failed := &APIError{Code: Internal, Message: fmt.Sprintf("cannot read the shed of workstream %s; check the trace repository", stream)}
	if _, _, api := s.ObjectionSubject(repository, stream, req.Objection); api != nil {
		return shed.Entry{}, api
	}
	if s.Router == nil {
		return shed.Entry{}, &APIError{Code: Internal, Message: "routing objection decisions is unavailable"}
	}
	x, err := s.objectionExit(repository, stream, req)
	if err != nil {
		return shed.Entry{}, failed
	}
	res, err := s.Router.Route(ctx, x)
	switch {
	case err != nil && errors.Is(err, trace.ErrConflict):
		return shed.Entry{}, &APIError{Code: Conflict, Message: fmt.Sprintf("objection %s was decided already", req.Objection)}
	case err != nil:
		return shed.Entry{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot record the decision on objection %s of workstream %s; check the trace repository", req.Objection, stream)}
	case !res.Committed:
		return shed.Entry{}, &APIError{Code: Conflict, Message: fmt.Sprintf("the decision on objection %s is not recorded: the router %s it (%s); the chief of staff's notice has what Jev saw", req.Objection, routeWord(res), res.Hold)}
	}
	entries, err := Dissent(repository, stream)
	if err != nil {
		return shed.Entry{}, failed
	}
	i := slices.IndexFunc(entries, func(e shed.Entry) bool { return e.ID == req.Objection })
	if i < 0 {
		return shed.Entry{}, failed
	}
	return entries[i], nil
}

func routeWord(res router.Result) string {
	switch res.Journal {
	case router.StateOwner:
		return "sent to the owner"
	case router.StateRefused:
		return "refused"
	case router.StateStale:
		return "found stale"
	}
	return "held"
}

// ResolveObjection returns the resolve_objection tool of one claimed
// manager turn. The manager dismisses a committee objection at
// the ratification gate on the owner's behalf, or escalates it to the owner.
// A decision the service refuses is an ordinary result,
// {"recorded":false,"reason":...}, and records nothing. A shared manager of
// staff names the member whose debate the objection belongs to.
func (c *Controls) ResolveObjection(repository *trace.Repository, scope coreadapter.Scope) coreadapter.Tool {
	tool := coreadapter.Tool{Name: ResolveObjectionTool, Effect: coreadapter.ToolMemory,
		Description: "Decide a committee objection that stands once this workstream's debate concluded, before the owner sees the ratification packet. objection: its ID; decision: dismiss (the record answers it: it no longer blocks and is not put to the owner) or escalate (the owner rules on it); " +
			"note: for dismiss, what answers the objection, citing the architect's reply, the spec, the plan, the charter or the entity map; for escalate, in a few short sentences for the owner, what the objection is about, the finding that matters and what you recommend. Dismiss only when you are confident the record answers it; otherwise escalate. The owner's own objections are the owner's. workstream: a member workstream ID of your workgroup to act on; omit for this conversation's own workstream.",
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
		target, stream, refusal, err := repository.ManagerMember(config.WorkstreamID(scope.Workstream), input.Workstream)
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
		objection := strings.TrimSpace(input.Objection)
		if _, _, api := s.ObjectionSubject(target, stream, objection); api != nil {
			if api.Code == Internal {
				return nil, errors.New(api.Message)
			}
			return PriorityRefusal(api.Message)
		}
		bound, why, err := jev.Require(target, stream, jev.KindObjection, jev.ObjectionKey(objection), string(decision), "objection "+objection)
		if err != nil {
			return nil, err
		}
		if why != "" {
			return PriorityRefusal(why)
		}
		request := ObjectionDecisionRequest{Objection: objection, Decision: decision, Note: note, Turn: scope.Turn}
		if bound != nil {
			request.Judgment = bound.Judgment
		}
		entry, api := s.DecideObjection(ctx, target, stream, request)
		if api != nil && bound != nil && api.Code != Internal && decision != shed.Escalate {
			request.Decision = shed.Escalate
			request.Note = fmt.Sprintf("Jev decided %s with confidence %.2f (judgment %s), but the service refused it: %s. The decision is yours. The chief of staff's note: %s", bound.Pick, bound.Confidence, bound.Judgment, api.Message, note)
			if entry, handed := s.DecideObjection(ctx, target, stream, request); handed == nil {
				return json.Marshal(struct {
					Recorded  bool   `json:"recorded"`
					Objection string `json:"objection"`
					Blocking  bool   `json:"blocking"`
					Detail    string `json:"detail"`
				}{true, entry.ID, entry.Blocking, "the service could not carry out Jev's decision (" + api.Message + "); the objection is in the owner's ratification packet with Jev's judgment"})
			}
		}
		if api != nil {
			if api.Code == Internal {
				return nil, errors.New(api.Message)
			}
			return PriorityRefusal(api.Message)
		}
		detail := "the objection is dismissed on the owner's behalf and is not put to the owner"
		if entry.Escalated {
			detail = "the objection is in the owner's ratification packet with your note"
		}
		if entry.Escalated != (decision == shed.Escalate) {
			detail += "; Jev's answers departed from your decision, so it is recorded as Jev's"
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
