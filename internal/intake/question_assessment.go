package intake

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/kpenfound/agentcore/jev/systemone"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/events"
	"github.com/kpenfound/osmia/internal/followup"
	"github.com/kpenfound/osmia/internal/ghlog"
	"github.com/kpenfound/osmia/internal/jev"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/trace"
)

// questionAssessmentTask is the Jev judgment that assesses an open question
// for the chief of staff. Bump questionAssessmentVersion whenever its
// questions, their interpretation or the thresholds below change.
const (
	QuestionAssessmentTask    = "question-assessment"
	QuestionAssessmentVersion = 1
)

// The probabilities at which an answer becomes a signal. Each signal argues
// for the more careful outcome, so a false one costs the chief an
// explanation, and a missed one is the error the signals exist to catch.
const (
	AmendmentThreshold = 0.6
	ConflictThreshold  = 0.6
	ProjectThreshold   = 0.7
)

// maxAssessedQuestions bounds the questions one turn assesses, and so how
// long its session waits to start.
const MaxAssessedQuestions = 8

// maxAssessedRulings bounds the rulings a question is compared against; the
// newest are kept. maxRulingRunes bounds each text of a ruling.
const (
	MaxAssessedRulings = 32
	MaxRulingRunes     = 1000
)

// maxQuestionRunes bounds the question as the judgment reads it.
const MaxQuestionRunes = 8000

// maxAssessmentState bounds the judgment's state and its longest question in
// bytes, keeping them within Jev's context. The sealed spec and the plan's
// units are included only when they fit.
const MaxAssessmentState = 72 * 1024

// The question IDs of an assessment.
const (
	AmendmentQuestion = "amendment"
	ConflictQuestion  = "conflict"
	ScopeQuestion     = "scope"
)

// noRuling is the conflict option that the question contradicts no ruling.
const NoRuling = "none"

// questionAdvice is the preamble of the advice a chief-of-staff turn's prompt
// ends with.
const QuestionAdvice = "Jev signals on the open questions above. Jev, a small judgment model, read each question against the sealed spec and plan and the owner's rulings; " +
	"a signal is advice, not a decision, and never requires an outcome. Check each one against the record before you choose, and choose only what the record supports. " +
	"When you choose against a signal, say why in your final response."

// questionSignals returns the advice of a chief-of-staff turn: Jev signals on
// the open questions the turn delivers, or "" when the boost is off, no
// question is open or no signal reaches its threshold.
func (s *Controller) QuestionSignals(repository *trace.Repository) func(context.Context, trace.TurnRequest, coreadapter.Scope) string {
	return func(ctx context.Context, req trace.TurnRequest, scope coreadapter.Scope) string {
		if scope.Role != trace.ChiefOfStaff || s.Jev.Status().Mode == jev.ModeDisabled {
			return ""
		}
		a := Assessor{Judge: s.Jev, Repository: repository, Stream: config.WorkstreamID(scope.Workstream), Scope: scope}
		advice, err := a.Advise(ctx, req)
		if err != nil {
			ghlog.Logger(ctx).Warn("no Jev signals for turn", "workstream", scope.Workstream, "turn", scope.Turn, "error", err.Error())
			return ""
		}
		return advice
	}
}

type Assessor struct {
	Judge      *jev.Judge
	Repository *trace.Repository
	Stream     config.WorkstreamID
	Scope      coreadapter.Scope
}

func (a Assessor) Advise(ctx context.Context, req trace.TurnRequest) (string, error) {
	delivered, err := a.Delivered(req)
	if err != nil || len(delivered) == 0 {
		return "", err
	}
	sealed, err := a.Sealed()
	if err != nil {
		return "", err
	}
	rulings, err := a.Rulings()
	if err != nil {
		return "", err
	}
	var lines []string
	for _, q := range delivered {
		role := ""
		if t, err := a.Repository.Thread(a.Stream, q.Asked.AskedBy.ID); err == nil {
			role = t.Identity.Role
		}
		judgment, ok, err := a.Judgment(q, role, sealed, rulings)
		if err != nil {
			return "", err
		}
		if !ok {
			continue
		}
		decision := a.Judge.Evaluate(ctx, a.Repository, judgment.Judgment)
		if decision.Outcome != jev.Accepted {
			continue
		}
		for _, signal := range judgment.Signals(*decision.Response) {
			lines = append(lines, fmt.Sprintf("- Question %s (judgment %s): %s", q.Asked.ID, decision.ID, signal))
		}
	}
	if len(lines) == 0 {
		return "", nil
	}
	return QuestionAdvice + "\n" + strings.Join(lines, "\n"), nil
}

// delivered returns the open questions whose question events the turn
// delivers, oldest first, at most maxAssessedQuestions of them. A recovery
// turn delivers the events of the turn it continues.
func (a Assessor) Delivered(req trace.TurnRequest) ([]trace.QuestionState, error) {
	t, err := a.Repository.Thread(a.Stream, req.AgentID)
	if err != nil {
		return nil, err
	}
	turn := DeliveryTurn(t, req)
	outbox, err := a.Repository.Outbox(a.Stream)
	if err != nil {
		return nil, err
	}
	// An event is named by the transition that raised it; a question's
	// notice by the question's move to open.
	opened := map[string]bool{}
	for _, e := range outbox {
		for _, h := range e.History {
			if h.Kind == "claim" && events.TurnID(h.Token) == turn {
				opened[e.TransitionID] = true
			}
		}
	}
	if len(opened) == 0 {
		return nil, nil
	}
	all, err := a.Repository.Questions(a.Stream)
	if err != nil {
		return nil, err
	}
	var out []trace.QuestionState
	for _, q := range all {
		if q.State == trace.QuestionOpen && opened[trace.QuestionSubject(q.Asked.ID)+"_"+trace.QuestionOpen] && len(out) < MaxAssessedQuestions {
			out = append(out, q)
		}
	}
	return out, nil
}

// deliveryTurn returns the turn whose events req delivers: req's own, or for
// a recovery continuation, which names the response of the turn it
// continues, that turn's.
func DeliveryTurn(t trace.Thread, req trace.TurnRequest) string {
	for range t.Turns {
		if req.Actor != RecoveryActor {
			break
		}
		i := slices.IndexFunc(t.Turns, func(q trace.QueuedTurn) bool { return q.Response != nil && q.Response.ID == req.Cause })
		if i < 0 {
			break
		}
		req = t.Turns[i].Request
	}
	return req.TurnID
}

// sealedDocuments are the documents a sealed workstream builds against.
type SealedDocuments struct {
	Seal     trace.Document
	Spec     trace.Document
	Plan     trace.Document
	Criteria []plan.Criterion
	Units    plan.Plan
}

// sealed returns the latest seal's spec and plan, or nil before ratification.
func (a Assessor) Sealed() (*SealedDocuments, error) {
	s, doc, ok, err := seal.Latest(a.Repository, a.Stream)
	if err != nil || !ok {
		return nil, err
	}
	documents, err := trace.Read[trace.Document](a.Repository, a.Stream)
	if err != nil {
		return nil, err
	}
	out := &SealedDocuments{Seal: doc}
	for _, d := range documents {
		switch {
		case d.ID == plan.SpecDocument && d.Revision == s.Revision.Spec:
			out.Spec = d
		case d.ID == plan.PlanDocument && d.Revision == s.Revision.Plan:
			out.Plan = d
		}
	}
	if out.Spec.Revision == 0 || out.Plan.Revision == 0 {
		return nil, fmt.Errorf("seal %d names revisions the trace does not hold", s.Seal)
	}
	out.Criteria = plan.ParseSpec(out.Spec.Content).Criteria
	if out.Units, err = plan.Parse([]byte(out.Plan.Content)); err != nil {
		return nil, fmt.Errorf("%s revision %d: %w", plan.PlanPath, out.Plan.Revision, err)
	}
	return out, nil
}

// assessedRuling is an owner ruling a question is compared against: one of
// this workstream's, or a notice from any workstream of the project.
type AssessedRuling struct {
	Ruling     trace.Ruling
	Workstream config.WorkstreamID
	Question   string
}

// describe names the ruling in a signal.
func (r AssessedRuling) Describe(stream config.WorkstreamID) string {
	where := ""
	if r.Workstream != stream {
		where = " in workstream " + string(r.Workstream)
	}
	return fmt.Sprintf("the owner's ruling on question %s%s, %q", r.Ruling.QuestionID, where, TruncateRunes(r.Ruling.OwnerResponse, 200))
}

// rulings returns the owner's rulings on this workstream's questions and the
// project's notices, oldest first, at most maxAssessedRulings of the newest.
// A relay gives every ruling of its batch the same text, so a batch is one
// ruling, the first of them.
func (a Assessor) Rulings() ([]AssessedRuling, error) {
	streams, err := NonSessionWorkstreams(a.Repository)
	if err != nil {
		return nil, err
	}
	var out []AssessedRuling
	for _, ws := range streams {
		records, err := trace.Read[trace.Ruling](a.Repository, ws)
		if err != nil {
			return nil, err
		}
		latest := map[string]trace.Ruling{}
		for _, r := range records {
			latest[r.ID] = r
		}
		asked, err := a.Repository.Questions(ws)
		if err != nil {
			return nil, err
		}
		for _, q := range asked {
			r, ok := latest[q.Asked.ID]
			if !ok || r.Decision != trace.DecisionRuling || !Present(r.OwnerResponse) || ws != a.Stream && r.Scope != trace.ScopeNotify {
				continue
			}
			same := func(o AssessedRuling) bool {
				return o.Workstream == ws && o.Ruling.At.Equal(r.At) && o.Ruling.OwnerResponse == r.OwnerResponse && o.Ruling.ReturnedAnswer == r.ReturnedAnswer
			}
			if slices.ContainsFunc(out, same) {
				continue
			}
			// The owner ruled on the question as it was sent to them.
			text := q.Asked.Question
			if Present(q.Latest.SentToOwner) {
				text = q.Latest.SentToOwner
			}
			out = append(out, AssessedRuling{Ruling: r, Workstream: ws, Question: text})
		}
	}
	slices.SortStableFunc(out, func(x, y AssessedRuling) int { return x.Ruling.At.Compare(y.Ruling.At) })
	if len(out) > MaxAssessedRulings {
		out = out[len(out)-MaxAssessedRulings:]
	}
	return out, nil
}

func Present(s string) bool { return strings.TrimSpace(s) != "" }

// assessment is one question's judgment and the rulings its conflict
// question offers, in option order.
type Assessment struct {
	jev.Judgment
	Stream  config.WorkstreamID
	Rulings []AssessedRuling
}

// judgment builds the assessment of question q, asked by an agent of role.
// It reports false when the question does not fit a judgment.
func (a Assessor) Judgment(q trace.QuestionState, role string, sealed *SealedDocuments, rulings []AssessedRuling) (Assessment, bool, error) {
	asked := map[string]any{"text": TruncateRunes(q.Asked.Question, MaxQuestionRunes)}
	if role != "" {
		asked["asked_by"] = role
	}
	state := map[string]any{"question": asked}
	sources := []jev.Source{{Kind: "question", ID: q.Latest.ID, Revision: q.Latest.Revision}}
	questions := map[string]systemone.Question{ScopeQuestion: ScopeNoul()}
	if sealed != nil {
		sources = append(sources, jev.Source{Kind: "seal", ID: sealed.Seal.ID, Revision: sealed.Seal.Revision}, jev.Source{Kind: "spec", ID: sealed.Spec.ID, Revision: sealed.Spec.Revision}, jev.Source{Kind: "plan", ID: sealed.Plan.ID, Revision: sealed.Plan.Revision})
		criteria := make([]map[string]string, 0, len(sealed.Criteria))
		for _, c := range sealed.Criteria {
			criteria = append(criteria, map[string]string{"citation": plan.Cite(c.Number), "text": c.Text})
		}
		state["criteria"] = criteria
		if q.Asked.Unit != "" {
			u, ok := sealed.Units.Unit(q.Asked.Unit)
			if !ok {
				item, found, err := followup.Find(a.Repository, a.Stream, q.Asked.Unit)
				if err != nil {
					return Assessment{}, false, err
				}
				u, ok = item.Unit, found
			}
			if ok {
				asked["unit"] = u.ID
				state["unit"] = map[string]any{"id": u.ID, "title": u.Title, "task": u.Task, "acceptance": u.Acceptance, "criteria": u.Criteria}
			}
		}
		questions[AmendmentQuestion] = AmendmentNoul()
	}
	size := func() int {
		longest := 0
		for _, question := range questions {
			data, _ := json.Marshal(question)
			longest = max(longest, len(data))
		}
		data, _ := json.Marshal(state)
		return len(data) + longest
	}
	if size() > MaxAssessmentState {
		return Assessment{}, false, nil
	}
	// The newest rulings that fit are offered.
	for n := len(rulings); n > 0; n-- {
		questions[ConflictQuestion] = ConflictChoice(rulings[len(rulings)-n:])
		if size() <= MaxAssessmentState {
			rulings = rulings[len(rulings)-n:]
			break
		}
		delete(questions, ConflictQuestion)
	}
	if _, ok := questions[ConflictQuestion]; !ok {
		rulings = nil
	}
	for _, r := range rulings {
		sources = append(sources, jev.Source{Kind: "ruling", ID: trace.RecordPath(r.Ruling), Revision: r.Ruling.Revision})
	}
	if sealed != nil {
		state["spec"] = sealed.Spec.Content
		if size() > MaxAssessmentState {
			delete(state, "spec")
		}
		units := make([]map[string]any, 0, len(sealed.Units.Units))
		for _, u := range sealed.Units.Units {
			units = append(units, map[string]any{"id": u.ID, "title": u.Title, "task": u.Task})
		}
		state["plan"] = units
		if size() > MaxAssessmentState {
			delete(state, "plan")
		}
	}
	out := Assessment{Stream: a.Stream, Rulings: rulings}
	out.Judgment = jev.Judgment{
		Scope:   a.Scope,
		Cause:   q.Asked.ID,
		Depth:   q.Asked.Depth,
		Task:    QuestionAssessmentTask,
		Version: QuestionAssessmentVersion,
		Sources: sources,
		Request: systemone.Request{State: state, Questions: questions},
		Accept: func(r systemone.Response) string {
			if len(out.Signals(r)) == 0 {
				return "no signal reached its threshold"
			}
			return ""
		},
	}
	return out, true, nil
}

func AmendmentNoul() systemone.Question {
	q := systemone.Noul("The state holds a question an agent building a feature asked, the acceptance criteria of the feature's sealed spec and, when the agent works on a unit of the sealed plan, " +
		"that unit's task and acceptance. Would the honest answer to the question require changing the sealed spec or plan, such as a criterion, a unit's task or acceptance, or how the work is cut into units, " +
		"rather than doing the work as they are written?")
	q.Yes = "The spec or plan as sealed is wrong, contradictory, incomplete or impossible on this point, so following it as written leaves the feature wrong."
	q.No = "The sealed spec and plan already settle the question, or it asks how to do the work they describe."
	return q
}

func ScopeNoul() systemone.Question {
	q := systemone.Noul("The state holds a question an agent building a feature asked. Does the question ask for a standing rule for work on the whole project, " +
		"such as a convention, policy or constraint that would bind other features too, rather than a decision about this feature alone?")
	q.Yes = "The answer would be a rule every feature on the project should follow."
	q.No = "The answer matters to this feature only, or the question asks for no rule."
	return q
}

// conflictChoice asks which ruling answering the question as it proposes
// would contradict; each option holds its ruling.
func ConflictChoice(rulings []AssessedRuling) systemone.Question {
	options := make([]systemone.Option, 0, len(rulings)+1)
	for i, r := range rulings {
		ruling := map[string]string{"question": TruncateRunes(r.Question, MaxRulingRunes), "ruling": TruncateRunes(r.Ruling.OwnerResponse, MaxRulingRunes)}
		if Present(r.Ruling.ReturnedAnswer) {
			ruling["relayed"] = TruncateRunes(r.Ruling.ReturnedAnswer, MaxRulingRunes)
		}
		options = append(options, systemone.Option{Name: RulingOption(i), Description: ruling})
	}
	options = append(options, systemone.Option{Name: NoRuling, Description: "Answering it as it proposes or assumes contradicts none of these rulings."})
	return systemone.Choice("The options are rulings the owner already made, each with the question it answered. Would answering the question in the state the way it proposes or assumes contradict one of them? "+
		"Choose the ruling it would contradict, or none.", options...)
}

func RulingOption(i int) string { return fmt.Sprintf("ruling-%d", i+1) }

// signals returns, for each answer of r that reaches its threshold, the
// signal it gives the chief of staff.
func (a Assessment) Signals(r systemone.Response) []string {
	var out []string
	if ans, ok := r.Answers[AmendmentQuestion]; ok && ans.Kind == systemone.KindNoul && ans.Noul >= AmendmentThreshold {
		out = append(out, fmt.Sprintf("the honest answer may change the sealed spec or plan (probability %.2f); route_amendment fits when it does", ans.Noul))
	}
	if ans, ok := r.Answers[ConflictQuestion]; ok && ans.Kind == systemone.KindChoice {
		for i, ruling := range a.Rulings {
			if p := ans.Probabilities[ans.Choice]; ans.Choice == RulingOption(i) && p >= ConflictThreshold {
				out = append(out, fmt.Sprintf("answering as the question proposes may contradict %s (probability %.2f); escalate fits when it does", ruling.Describe(a.Stream), p))
			}
		}
	}
	if ans, ok := r.Answers[ScopeQuestion]; ok && ans.Kind == systemone.KindNoul && ans.Noul >= ProjectThreshold {
		out = append(out, fmt.Sprintf("the question may ask for a standing rule for the whole project rather than this feature (probability %.2f); "+
			"a notice, or a charter proposal once the owner rules, fits when it does", ans.Noul))
	}
	return out
}

// truncateRunes returns s cut to at most n runes.
func TruncateRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}
