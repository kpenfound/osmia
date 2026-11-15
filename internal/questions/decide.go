package questions

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/jev"
	"github.com/kpenfound/osmia/internal/trace"
)

// The outcomes of routing a question, which are the options of Jev's routing
// decision.
const (
	OutcomeAnswer         = "answer"
	OutcomeRouteAmendment = "route_amendment"
	OutcomeEscalate       = "escalate"
)

// RoutingRoles are the roles whose questions Jev takes part in routing: the
// workers that build a feature. A question of any other role, such as the
// architect's judgment that a project needs no change, is the chief of
// staff's and the owner's as it always was.
var RoutingRoles = []string{"mason", "reviewer"}

// RoutingSubject is an open question the chief of staff may route with Jev.
type RoutingSubject struct {
	// Repository holds the trace of Stream, the workstream the question
	// belongs to.
	Repository *trace.Repository
	Stream     config.WorkstreamID
	Question   trace.QuestionState
	// Role is the role of the asker.
	Role string
}

// Key is the question's occurrence key.
func (s RoutingSubject) Key() string { return jev.QuestionKey(s.Question.Asked.ID) }

// Label names the question for the chief of staff.
func (s RoutingSubject) Label() string { return fmt.Sprintf("question %s of workstream %s", s.Question.Asked.ID, s.Stream) }

// RoutingSubjectOf resolves the question id the way the chief of staff's
// question tools name it (a number of the turn's own workstream, or
// "<workstream>:<number>" of a workgroup member). The returned reason is
// non-empty when the chief may not route the question with Jev today: it is not
// open, it reports a conflict between member charters, or its asker is not a
// role Jev takes part in routing for. Such a question keeps the route it has.
func RoutingSubjectOf(repository *trace.Repository, host config.WorkstreamID, id string) (RoutingSubject, string, error) {
	member, local, err := repository.ChiefQuestionTarget(host, strings.TrimSpace(id))
	if err != nil {
		return RoutingSubject{}, "", err
	}
	target, err := repository.ProjectFor(member)
	if err != nil {
		return RoutingSubject{}, "", err
	}
	all, err := target.Questions(member)
	if err != nil {
		return RoutingSubject{}, "", err
	}
	i := slices.IndexFunc(all, func(q trace.QuestionState) bool { return q.Asked.ID == local })
	if i < 0 {
		return RoutingSubject{}, fmt.Sprintf("there is no question %s", id), nil
	}
	q := all[i]
	s := RoutingSubject{Repository: target, Stream: member, Question: q}
	switch {
	case q.State != trace.QuestionOpen:
		return s, fmt.Sprintf("question %s is %s, not open; only an open question is routed", id, q.State), nil
	case len(q.Asked.CharterConflict) > 0:
		return s, fmt.Sprintf("question %s reports a conflict between member charters; it goes straight to the owner and is not decided with Jev", id), nil
	}
	thread, err := target.Thread(member, q.Asked.AskedBy.ID)
	if err != nil {
		return s, fmt.Sprintf("the asker of question %s is unknown; it is not decided with Jev", id), nil
	}
	s.Role = thread.Identity.Role
	if !slices.Contains(RoutingRoles, s.Role) {
		return s, fmt.Sprintf("question %s was asked by the %s, whose questions the chief of staff and the owner decide; it is not decided with Jev", id, s.Role), nil
	}
	return s, "", nil
}

// requireRouting applies the shared precondition to the chief of staff's
// outcome for question id: when the question is one routed with Jev, a decide
// result must exist for it, and a binding pick allows only itself. A question
// that is not routed with Jev, or no longer open, passes: the tool's own checks
// decide it as they always did.
func requireRouting(repository *trace.Repository, host config.WorkstreamID, id, outcome string) (*jev.Result, string, error) {
	s, reason, err := RoutingSubjectOf(repository, host, id)
	var refused *trace.QuestionRefused
	if errors.As(err, &refused) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	if reason != "" {
		return nil, "", nil
	}
	return jev.Require(s.Repository, s.Stream, jev.KindQuestionRouting, s.Key(), outcome, s.Label())
}

// handToOwner puts a question to the owner's inbox when the service refused the
// transition Jev's binding pick calls for. The escalation carries no gate
// judgment: the question already carries Jev's binding judgment, which the
// owner's card shows, and Jev is not asked again about it.
func handToOwner(ctx context.Context, repository *trace.Repository, agent string, scope coreadapter.Scope, id string, bound *jev.Result, why string, at time.Time) (string, error) {
	s, _, err := RoutingSubjectOf(repository, config.WorkstreamID(scope.Workstream), id)
	if err != nil {
		return "", err
	}
	text := strings.TrimSpace(s.Question.Asked.Question)
	req := trace.EscalationRequest{Questions: []string{id},
		Rephrasing:     fmt.Sprintf("Question %s from %s: %s", s.Question.Asked.ID, s.Question.Asked.AskedBy.ID, text),
		Blocked:        "The asker waits for the answer.",
		Recommendation: fmt.Sprintf("Jev decided %s with confidence %.2f (judgment %s), but the service refused it: %s. The decision is yours.", bound.Pick, bound.Confidence, bound.Judgment, why)}
	return repository.EscalateQuestions(ctx, agent, scope, req, at)
}
