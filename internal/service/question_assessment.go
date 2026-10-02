package service

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"slices"
	"strings"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/events"
	"github.com/kpenfound/osmia/internal/followup"
	"github.com/kpenfound/osmia/internal/jev"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/systemone"
	"github.com/kpenfound/osmia/internal/trace"
)

// questionAssessmentTask is the Jev judgment that assesses an open question
// for the chief of staff. Bump questionAssessmentVersion whenever its
// questions, their interpretation or the thresholds below change.
const (
	questionAssessmentTask    = "question-assessment"
	questionAssessmentVersion = 1
)

// The probabilities at which an answer becomes a signal. Each signal argues
// for the more careful outcome, so a false one costs the chief an
// explanation, and a missed one is the error the signals exist to catch.
const (
	amendmentThreshold = 0.6
	conflictThreshold  = 0.6
	projectThreshold   = 0.7
)

// maxAssessedQuestions bounds the questions one turn assesses, and so how
// long its session waits to start.
const maxAssessedQuestions = 8

// maxAssessedRulings bounds the rulings a question is compared against; the
// newest are kept. maxRulingRunes bounds each text of a ruling.
const (
	maxAssessedRulings = 32
	maxRulingRunes     = 1000
)

// maxQuestionRunes bounds the question as the judgment reads it.
const maxQuestionRunes = 8000

// maxAssessmentState bounds the judgment's state and its longest question in
// bytes, keeping them within Jev's context. The sealed spec and the plan's
// units are included only when they fit.
const maxAssessmentState = 72 * 1024

// The question IDs of an assessment.
const (
	amendmentQuestion = "amendment"
	conflictQuestion  = "conflict"
	scopeQuestion     = "scope"
)

// noRuling is the conflict option that the question contradicts no ruling.
const noRuling = "none"

// questionAdvice is the preamble of the advice a chief-of-staff turn's prompt
// ends with.
const questionAdvice = "Jev signals on the open questions above. Jev, a small judgment model, read each question against the sealed spec and plan and the owner's rulings; " +
	"a signal is advice, not a decision, and never requires an outcome. Check each one against the record before you choose, and choose only what the record supports. " +
	"When you choose against a signal, say why in your final response."

// questionSignals returns the advice of a chief-of-staff turn: Jev signals on
// the open questions the turn delivers, or "" when the boost is off, no
// question is open or no signal reaches its threshold.
func (s *Service) questionSignals(repository *trace.Repository) func(context.Context, trace.TurnRequest, coreadapter.Scope) string {
	return func(ctx context.Context, req trace.TurnRequest, scope coreadapter.Scope) string {
		if scope.Role != trace.ChiefOfStaff || s.jev.Status().Mode == jev.ModeDisabled {
			return ""
		}
		a := assessor{judge: s.jev, repository: repository, stream: config.WorkstreamID(scope.Workstream), scope: scope}
		advice, err := a.advise(ctx, req)
		if err != nil {
			log.Printf("osmia: workstream %s turn %s has no Jev signals: %v", scope.Workstream, scope.Turn, err)
			return ""
		}
		return advice
	}
}

type assessor struct {
	judge      *jev.Judge
	repository *trace.Repository
	stream     config.WorkstreamID
	scope      coreadapter.Scope
}

func (a assessor) advise(ctx context.Context, req trace.TurnRequest) (string, error) {
	delivered, err := a.delivered(req)
	if err != nil || len(delivered) == 0 {
		return "", err
	}
	sealed, err := a.sealed()
	if err != nil {
		return "", err
	}
	rulings, err := a.rulings()
	if err != nil {
		return "", err
	}
	var lines []string
	for _, q := range delivered {
		role := ""
		if t, err := a.repository.Thread(a.stream, q.Asked.AskedBy.ID); err == nil {
			role = t.Identity.Role
		}
		judgment, ok, err := a.judgment(q, role, sealed, rulings)
		if err != nil {
			return "", err
		}
		if !ok {
			continue
		}
		decision := a.judge.Evaluate(ctx, a.repository, judgment.Judgment)
		if decision.Outcome != jev.Accepted {
			continue
		}
		for _, signal := range judgment.signals(*decision.Response) {
			lines = append(lines, fmt.Sprintf("- Question %s (judgment %s): %s", q.Asked.ID, decision.ID, signal))
		}
	}
	if len(lines) == 0 {
		return "", nil
	}
	return questionAdvice + "\n" + strings.Join(lines, "\n"), nil
}

// delivered returns the open questions whose question events the turn
// delivers, oldest first, at most maxAssessedQuestions of them. A recovery
// turn delivers the events of the turn it continues.
func (a assessor) delivered(req trace.TurnRequest) ([]trace.QuestionState, error) {
	t, err := a.repository.Thread(a.stream, req.AgentID)
	if err != nil {
		return nil, err
	}
	turn := deliveryTurn(t, req)
	outbox, err := a.repository.Outbox(a.stream)
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
	all, err := a.repository.Questions(a.stream)
	if err != nil {
		return nil, err
	}
	var out []trace.QuestionState
	for _, q := range all {
		if q.State == trace.QuestionOpen && opened[trace.QuestionSubject(q.Asked.ID)+"_"+trace.QuestionOpen] && len(out) < maxAssessedQuestions {
			out = append(out, q)
		}
	}
	return out, nil
}

// deliveryTurn returns the turn whose events req delivers: req's own, or for
// a recovery continuation, which names the response of the turn it
// continues, that turn's.
func deliveryTurn(t trace.Thread, req trace.TurnRequest) string {
	for range t.Turns {
		if req.Actor != recoveryActor {
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
type sealedDocuments struct {
	seal     trace.Document
	spec     trace.Document
	plan     trace.Document
	criteria []plan.Criterion
	units    plan.Plan
}

// sealed returns the latest seal's spec and plan, or nil before ratification.
func (a assessor) sealed() (*sealedDocuments, error) {
	s, doc, ok, err := seal.Latest(a.repository, a.stream)
	if err != nil || !ok {
		return nil, err
	}
	documents, err := trace.Read[trace.Document](a.repository, a.stream)
	if err != nil {
		return nil, err
	}
	out := &sealedDocuments{seal: doc}
	for _, d := range documents {
		switch {
		case d.ID == plan.SpecDocument && d.Revision == s.Revision.Spec:
			out.spec = d
		case d.ID == plan.PlanDocument && d.Revision == s.Revision.Plan:
			out.plan = d
		}
	}
	if out.spec.Revision == 0 || out.plan.Revision == 0 {
		return nil, fmt.Errorf("seal %d names revisions the trace does not hold", s.Seal)
	}
	out.criteria = plan.ParseSpec(out.spec.Content).Criteria
	if out.units, err = plan.Parse([]byte(out.plan.Content)); err != nil {
		return nil, fmt.Errorf("%s revision %d: %w", plan.PlanPath, out.plan.Revision, err)
	}
	return out, nil
}

// assessedRuling is an owner ruling a question is compared against: one of
// this workstream's, or a notice from any workstream of the project.
type assessedRuling struct {
	ruling     trace.Ruling
	workstream config.WorkstreamID
	question   string
}

// describe names the ruling in a signal.
func (r assessedRuling) describe(stream config.WorkstreamID) string {
	where := ""
	if r.workstream != stream {
		where = " in workstream " + string(r.workstream)
	}
	return fmt.Sprintf("the owner's ruling on question %s%s, %q", r.ruling.QuestionID, where, truncateRunes(r.ruling.OwnerResponse, 200))
}

// rulings returns the owner's rulings on this workstream's questions and the
// project's notices, oldest first, at most maxAssessedRulings of the newest.
// A relay gives every ruling of its batch the same text, so a batch is one
// ruling, the first of them.
func (a assessor) rulings() ([]assessedRuling, error) {
	streams, err := a.repository.Workstreams()
	if err != nil {
		return nil, err
	}
	var out []assessedRuling
	for _, ws := range streams {
		records, err := trace.Read[trace.Ruling](a.repository, ws)
		if err != nil {
			return nil, err
		}
		latest := map[string]trace.Ruling{}
		for _, r := range records {
			latest[r.ID] = r
		}
		asked, err := a.repository.Questions(ws)
		if err != nil {
			return nil, err
		}
		for _, q := range asked {
			r, ok := latest[q.Asked.ID]
			if !ok || r.Decision != trace.DecisionRuling || !present(r.OwnerResponse) || ws != a.stream && r.Scope != trace.ScopeNotify {
				continue
			}
			same := func(o assessedRuling) bool {
				return o.workstream == ws && o.ruling.At.Equal(r.At) && o.ruling.OwnerResponse == r.OwnerResponse && o.ruling.ReturnedAnswer == r.ReturnedAnswer
			}
			if slices.ContainsFunc(out, same) {
				continue
			}
			// The owner ruled on the question as it was sent to them.
			text := q.Asked.Question
			if present(q.Latest.SentToOwner) {
				text = q.Latest.SentToOwner
			}
			out = append(out, assessedRuling{ruling: r, workstream: ws, question: text})
		}
	}
	slices.SortStableFunc(out, func(x, y assessedRuling) int { return x.ruling.At.Compare(y.ruling.At) })
	if len(out) > maxAssessedRulings {
		out = out[len(out)-maxAssessedRulings:]
	}
	return out, nil
}

func present(s string) bool { return strings.TrimSpace(s) != "" }

// assessment is one question's judgment and the rulings its conflict
// question offers, in option order.
type assessment struct {
	jev.Judgment
	stream  config.WorkstreamID
	rulings []assessedRuling
}

// judgment builds the assessment of question q, asked by an agent of role.
// It reports false when the question does not fit a judgment.
func (a assessor) judgment(q trace.QuestionState, role string, sealed *sealedDocuments, rulings []assessedRuling) (assessment, bool, error) {
	asked := map[string]any{"text": truncateRunes(q.Asked.Question, maxQuestionRunes)}
	if role != "" {
		asked["asked_by"] = role
	}
	state := map[string]any{"question": asked}
	sources := []jev.Source{{Kind: "question", ID: q.Latest.ID, Revision: q.Latest.Revision}}
	questions := map[string]systemone.Question{scopeQuestion: scopeNoul()}
	if sealed != nil {
		sources = append(sources, jev.Source{Kind: "seal", ID: sealed.seal.ID, Revision: sealed.seal.Revision}, jev.Source{Kind: "spec", ID: sealed.spec.ID, Revision: sealed.spec.Revision}, jev.Source{Kind: "plan", ID: sealed.plan.ID, Revision: sealed.plan.Revision})
		criteria := make([]map[string]string, 0, len(sealed.criteria))
		for _, c := range sealed.criteria {
			criteria = append(criteria, map[string]string{"citation": plan.Cite(c.Number), "text": c.Text})
		}
		state["criteria"] = criteria
		if q.Asked.Unit != "" {
			u, ok := sealed.units.Unit(q.Asked.Unit)
			if !ok {
				item, found, err := followup.Find(a.repository, a.stream, q.Asked.Unit)
				if err != nil {
					return assessment{}, false, err
				}
				u, ok = item.Unit, found
			}
			if ok {
				asked["unit"] = u.ID
				state["unit"] = map[string]any{"id": u.ID, "title": u.Title, "task": u.Task, "acceptance": u.Acceptance, "criteria": u.Criteria}
			}
		}
		questions[amendmentQuestion] = amendmentNoul()
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
	if size() > maxAssessmentState {
		return assessment{}, false, nil
	}
	// The newest rulings that fit are offered.
	for n := len(rulings); n > 0; n-- {
		questions[conflictQuestion] = conflictChoice(rulings[len(rulings)-n:])
		if size() <= maxAssessmentState {
			rulings = rulings[len(rulings)-n:]
			break
		}
		delete(questions, conflictQuestion)
	}
	if _, ok := questions[conflictQuestion]; !ok {
		rulings = nil
	}
	for _, r := range rulings {
		sources = append(sources, jev.Source{Kind: "ruling", ID: trace.RecordPath(r.ruling), Revision: r.ruling.Revision})
	}
	if sealed != nil {
		state["spec"] = sealed.spec.Content
		if size() > maxAssessmentState {
			delete(state, "spec")
		}
		units := make([]map[string]any, 0, len(sealed.units.Units))
		for _, u := range sealed.units.Units {
			units = append(units, map[string]any{"id": u.ID, "title": u.Title, "task": u.Task})
		}
		state["plan"] = units
		if size() > maxAssessmentState {
			delete(state, "plan")
		}
	}
	out := assessment{stream: a.stream, rulings: rulings}
	out.Judgment = jev.Judgment{
		Scope:   a.scope,
		Cause:   q.Asked.ID,
		Depth:   q.Asked.Depth,
		Task:    questionAssessmentTask,
		Version: questionAssessmentVersion,
		Sources: sources,
		Request: systemone.Request{State: state, Questions: questions},
		Accept: func(r systemone.Response) string {
			if len(out.signals(r)) == 0 {
				return "no signal reached its threshold"
			}
			return ""
		},
	}
	return out, true, nil
}

func amendmentNoul() systemone.Question {
	q := systemone.Noul("The state holds a question an agent building a feature asked, the acceptance criteria of the feature's sealed spec and, when the agent works on a unit of the sealed plan, " +
		"that unit's task and acceptance. Would the honest answer to the question require changing the sealed spec or plan, such as a criterion, a unit's task or acceptance, or how the work is cut into units, " +
		"rather than doing the work as they are written?")
	q.Yes = "The spec or plan as sealed is wrong, contradictory, incomplete or impossible on this point, so following it as written leaves the feature wrong."
	q.No = "The sealed spec and plan already settle the question, or it asks how to do the work they describe."
	return q
}

func scopeNoul() systemone.Question {
	q := systemone.Noul("The state holds a question an agent building a feature asked. Does the question ask for a standing rule for work on the whole project, " +
		"such as a convention, policy or constraint that would bind other features too, rather than a decision about this feature alone?")
	q.Yes = "The answer would be a rule every feature on the project should follow."
	q.No = "The answer matters to this feature only, or the question asks for no rule."
	return q
}

// conflictChoice asks which ruling answering the question as it proposes
// would contradict; each option holds its ruling.
func conflictChoice(rulings []assessedRuling) systemone.Question {
	options := make([]systemone.Option, 0, len(rulings)+1)
	for i, r := range rulings {
		ruling := map[string]string{"question": truncateRunes(r.question, maxRulingRunes), "ruling": truncateRunes(r.ruling.OwnerResponse, maxRulingRunes)}
		if present(r.ruling.ReturnedAnswer) {
			ruling["relayed"] = truncateRunes(r.ruling.ReturnedAnswer, maxRulingRunes)
		}
		options = append(options, systemone.Option{Name: rulingOption(i), Description: ruling})
	}
	options = append(options, systemone.Option{Name: noRuling, Description: "Answering it as it proposes or assumes contradicts none of these rulings."})
	return systemone.Choice("The options are rulings the owner already made, each with the question it answered. Would answering the question in the state the way it proposes or assumes contradict one of them? "+
		"Choose the ruling it would contradict, or none.", options...)
}

func rulingOption(i int) string { return fmt.Sprintf("ruling-%d", i+1) }

// signals returns, for each answer of r that reaches its threshold, the
// signal it gives the chief of staff.
func (a assessment) signals(r systemone.Response) []string {
	var out []string
	if ans, ok := r.Answers[amendmentQuestion]; ok && ans.Kind == systemone.KindNoul && ans.Noul >= amendmentThreshold {
		out = append(out, fmt.Sprintf("the honest answer may change the sealed spec or plan (probability %.2f); route_amendment fits when it does", ans.Noul))
	}
	if ans, ok := r.Answers[conflictQuestion]; ok && ans.Kind == systemone.KindChoice {
		for i, ruling := range a.rulings {
			if p := ans.Probabilities[ans.Choice]; ans.Choice == rulingOption(i) && p >= conflictThreshold {
				out = append(out, fmt.Sprintf("answering as the question proposes may contradict %s (probability %.2f); escalate fits when it does", ruling.describe(a.stream), p))
			}
		}
	}
	if ans, ok := r.Answers[scopeQuestion]; ok && ans.Kind == systemone.KindNoul && ans.Noul >= projectThreshold {
		out = append(out, fmt.Sprintf("the question may ask for a standing rule for the whole project rather than this feature (probability %.2f); "+
			"a notice, or a charter proposal once the owner rules, fits when it does", ans.Noul))
	}
	return out
}

// truncateRunes returns s cut to at most n runes.
func truncateRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}
