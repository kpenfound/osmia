package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/kb"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/scheduler"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/trace"
)

const verdictTool = "verdict"
const verdictOutcome = "verdict"
const reviewerRole = "reviewer"

var reviewerActor = trace.Actor{Kind: "service", ID: reviewerRole}

type ReviewEvidence struct {
	Criterion string `json:"criterion"`
	Evidence  string `json:"evidence"`
}

type ReviewFinding struct {
	Criterion string `json:"criterion"`
	Severity  string `json:"severity"`
	Evidence  string `json:"evidence"`
	Action    string `json:"action"`
}

type UnitVerdict struct {
	Decision   string            `json:"decision"`
	Evidence   []ReviewEvidence  `json:"evidence"`
	Findings   []ReviewFinding   `json:"findings"`
	ExtraPaths []PathExplanation `json:"extra_paths,omitempty"`
}

type PathExplanation struct {
	Path        string `json:"path"`
	Explanation string `json:"explanation"`
}

type UnitReviewResult struct {
	Identity UnitReviewIdentity `json:"identity"`
	Turn     string             `json:"turn"`
	Verdict  UnitVerdict        `json:"verdict"`
	Bounces  int                `json:"bounces"`
}

type reviewerReports struct {
	mu       sync.Mutex
	accepted map[string]UnitVerdict
}

func (r *reviewerReports) tool(scope coreadapter.Scope) coreadapter.Tool {
	tool := coreadapter.Tool{Name: verdictTool, Effect: coreadapter.ToolMemory,
		Description: "Record a verdict for the exact candidate in this turn. Cite each unit criterion with evidence. Material findings need an action the mason can take. End your turn after acceptance.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"decision":{"type":"string"},"evidence":{"type":"array","items":{"type":"object","properties":{"criterion":{"type":"string"},"evidence":{"type":"string"}},"required":["criterion","evidence"],"additionalProperties":false}},"findings":{"type":"array","items":{"type":"object","properties":{"criterion":{"type":"string"},"severity":{"type":"string"},"evidence":{"type":"string"},"action":{"type":"string"}},"required":["criterion","severity","evidence","action"],"additionalProperties":false}},"extra_paths":{"type":"array","items":{"type":"object","properties":{"path":{"type":"string"},"explanation":{"type":"string"}},"required":["path","explanation"],"additionalProperties":false}}},"required":["decision","evidence","findings"],"additionalProperties":false}`)}
	tool.Handle = func(_ context.Context, raw json.RawMessage) (json.RawMessage, error) {
		var verdict UnitVerdict
		if err := json.Unmarshal(raw, &verdict); err != nil {
			return nil, err
		}
		if verdict.Decision != "satisfactory" && verdict.Decision != "material_findings" {
			return refuseReport("decision must be satisfactory or material_findings")
		}
		if len(verdict.Evidence) == 0 {
			return refuseReport("criterion-linked evidence is required")
		}
		for _, e := range verdict.Evidence {
			if strings.TrimSpace(e.Criterion) == "" || strings.TrimSpace(e.Evidence) == "" {
				return refuseReport("each criterion needs evidence")
			}
		}
		if verdict.Decision == "material_findings" && len(verdict.Findings) == 0 {
			return refuseReport("material findings are required")
		}
		if verdict.Decision == "satisfactory" && len(verdict.Findings) != 0 {
			return refuseReport("satisfactory verdict cannot carry material findings")
		}
		for _, f := range verdict.Findings {
			if strings.TrimSpace(f.Criterion) == "" || strings.TrimSpace(f.Severity) == "" || strings.TrimSpace(f.Evidence) == "" || strings.TrimSpace(f.Action) == "" {
				return refuseReport("each finding needs criterion, severity, evidence and action")
			}
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.accepted == nil {
			r.accepted = map[string]UnitVerdict{}
		}
		key := turnKey(scope)
		if _, exists := r.accepted[key]; exists {
			return refuseReport("this turn already recorded a verdict; end the turn")
		}
		r.accepted[key] = verdict
		return json.Marshal(map[string]any{"recorded": true, "next": "End your turn now."})
	}
	return tool
}

type verdictTurns struct {
	coreadapter.Turns
	reports *reviewerReports
}

func (t *verdictTurns) Run(ctx context.Context, prepared coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
	result, err := t.Turns.Run(ctx, prepared)
	t.reports.mu.Lock()
	verdict, ok := t.reports.accepted[turnKey(prepared.Scope)]
	delete(t.reports.accepted, turnKey(prepared.Scope))
	t.reports.mu.Unlock()
	if ok {
		data, encodeErr := json.Marshal(verdict)
		if encodeErr != nil {
			return result, errors.Join(err, encodeErr)
		}
		result.Outcome = &coreadapter.Outcome{Status: verdictOutcome, Report: string(data)}
	}
	return result, err
}

func (t *verdictTurns) CheckResume(ctx context.Context, previous, next coreadapter.Profile, session coreadapter.BackendSession) error {
	if checker, ok := t.Turns.(coreadapter.ResumeChecker); ok {
		return checker.CheckResume(ctx, previous, next, session)
	}
	return coreadapter.ErrResumeUnavailable
}

type reviewers struct{ *masons }

func reviewerAgent(unit string) string {
	return reviewerRole + strings.TrimPrefix(trace.UnitSubject(unit), "unit")
}
func reviewTurnID(unit string, version uint64) string {
	return fmt.Sprintf("%s-review-%d", reviewerAgent(unit), version)
}

func answerQuestionID(turn string) string {
	id := strings.TrimPrefix(turn, "answer_")
	id, _, _ = strings.Cut(id, "-recover-")
	return id
}

func (r *reviewers) Pass(ctx context.Context) error {
	streams, err := r.repository.Workstreams()
	if err != nil {
		return err
	}
	state, _ := r.s.store.Effective()
	for _, stream := range streams {
		if stream == librarianWorkstream(r.repository.Project()) {
			continue
		}
		b, found, err := r.read(stream)
		if err != nil {
			return err
		}
		if !found {
			continue
		}
		paused := scheduler.Paused(state.Pauses, r.cfg.Project.ID, stream)
		for _, u := range b.plan.Units {
			unit := u.ID
			st := b.states[trace.UnitSubject(unit)]
			if st.Value == UnitContested {
				if err := r.resumeContested(ctx, stream, unit, st); err != nil {
					return err
				}
				continue
			}
			if st.Value == UnitImplementing {
				if result, ok, err := r.storedResult(stream, unit, st); err != nil {
					return err
				} else if ok && result.Verdict.Decision == "material_findings" {
					if err := r.enqueueFindings(ctx, stream, unit, result); err != nil {
						return err
					}
				}
				continue
			}
			if st.Value != UnitReviewing && st.Value != UnitWaiting {
				continue
			}
			if st.Value == UnitWaiting {
				transitions, err := trace.Read[trace.Transition](r.repository, stream)
				if err != nil {
					return err
				}
				reviewerWaiting := false
				for i := len(transitions) - 1; i >= 0; i-- {
					if transitions[i].Subject == trace.UnitSubject(unit) && transitions[i].To == UnitWaiting {
						reviewerWaiting = transitions[i].Actor == reviewerActor
						break
					}
				}
				if !reviewerWaiting {
					continue
				}
				value, err := r.followQuestion(ctx, stream, unit, st)
				if err != nil {
					return err
				}
				if value != UnitReviewing {
					continue
				}
				st, err = r.repository.Workflow(stream, trace.UnitSubject(unit))
				if err != nil {
					return err
				}
			} else if value, err := r.followQuestion(ctx, stream, unit, st); err != nil {
				return err
			} else if value != UnitReviewing {
				continue
			}
			if err := r.one(ctx, stream, unit, st, paused); err != nil {
				return fmt.Errorf("workstream %s unit %s review: %w", stream, unit, err)
			}
		}
	}
	return nil
}

func (r *reviewers) followQuestion(ctx context.Context, stream config.WorkstreamID, unit string, state trace.WorkflowState) (string, error) {
	th, err := r.repository.Thread(stream, reviewerAgent(unit))
	if errors.Is(err, os.ErrNotExist) || err == nil && len(th.Turns) == 0 {
		return state.Value, nil
	}
	if err != nil {
		return "", err
	}
	asked, err := r.repository.Questions(stream)
	if err != nil {
		return "", err
	}
	var q string
	var to, cause, reason string
	if state.Value == UnitReviewing {
		transitions, err := trace.Read[trace.Transition](r.repository, stream)
		if err != nil {
			return "", err
		}
		for _, turn := range th.Turns {
			candidate := askedBy(asked, th.Identity.ThreadID, turn.Request.TurnID)
			id := fmt.Sprintf("%s-reviewer-%s-%s", trace.UnitSubject(unit), UnitWaiting, candidate)
			if candidate != "" && !slices.ContainsFunc(transitions, func(t trace.Transition) bool { return t.ID == id }) {
				q = candidate
				break
			}
		}
		if q == "" {
			return state.Value, nil
		}
		to, cause = UnitWaiting, trace.QuestionSubject(q)+"_"+trace.QuestionOpen
		reason = fmt.Sprintf("unit %s is waiting: its reviewer asked question %s; the exact candidate is kept until the answer arrives", unit, q)
	} else if state.Value == UnitWaiting {
		last := th.Turns[len(th.Turns)-1]
		i := slices.IndexFunc(asked, func(q trace.QuestionState) bool {
			return q.Asked.Thread == th.Identity.ThreadID && strings.HasPrefix(last.Request.TurnID, "answer_") && q.Asked.ID == answerQuestionID(last.Request.TurnID)
		})
		if i < 0 {
			return state.Value, nil
		}
		q = asked[i].Asked.ID
		to, cause = UnitReviewing, trace.QuestionSubject(q)+"_"+trace.QuestionAnswered
		reason = fmt.Sprintf("unit %s resumes reviewing: the answer to question %s is the reviewer's next turn over the same candidate", unit, q)
	} else {
		return state.Value, nil
	}
	id := fmt.Sprintf("%s-reviewer-%s-%s", trace.UnitSubject(unit), to, q)
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: r.repository.Project(), Workstream: stream, Unit: unit, At: r.s.now(), Actor: reviewerActor, Cause: cause}
	_, err = r.repository.Transact(ctx, trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: h, Subject: trace.UnitSubject(unit), From: state.Value, To: to, Reason: reason}})
	if errors.Is(err, trace.ErrConflict) {
		return state.Value, nil
	}
	return to, err
}

func (r *reviewers) one(ctx context.Context, stream config.WorkstreamID, unit string, state trace.WorkflowState, paused bool) error {
	agent := reviewerAgent(unit)
	if result, ok, err := r.storedResult(stream, unit, state); err != nil {
		return err
	} else if ok {
		return r.applyReview(ctx, stream, unit, state, result)
	}
	th, err := r.repository.Thread(stream, agent)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	turnID := reviewTurnID(unit, state.Version)
	if err == nil && len(th.Turns) != 0 {
		last := th.Turns[len(th.Turns)-1]
		if strings.HasPrefix(last.Request.TurnID, "answer_") {
			if th.Status == "interrupted" && th.Active == last.Request.TurnID && last.Response == nil {
				if err := r.repository.AbandonTurn(ctx, stream, agent, last.Request.TurnID, r.s.now()); err != nil {
					return err
				}
				th, err = r.repository.Thread(stream, agent)
				if err != nil {
					return err
				}
				last = th.Turns[len(th.Turns)-1]
			}
			if last.Status() == "interrupted" && !paused {
				return r.recover(ctx, last)
			}
			if last.CompletedAt.IsZero() {
				return nil
			}
			transitions, err := trace.Read[trace.Transition](r.repository, stream)
			if err != nil {
				return err
			}
			latest := trace.Transition{}
			for _, t := range transitions {
				if t.Subject == trace.UnitSubject(unit) {
					latest = t
				}
			}
			currentAnswer := latest.From == UnitWaiting && latest.To == UnitReviewing && latest.Cause == trace.QuestionSubject(answerQuestionID(last.Request.TurnID))+"_"+trace.QuestionAnswered
			if currentAnswer && last.Status() == "idle" && last.Response.Result.Outcome != nil && last.Response.Result.Outcome.Status == verdictOutcome {
				return r.finishReview(ctx, stream, unit, state, last)
			}
			// An answer supplies context but never grants approval on its own.
			// The next review turn still requires a verdict for this candidate.
		}
		if last.Request.TurnID == turnID || strings.HasPrefix(last.Request.TurnID, turnID+"-recover-") {
			if th.Status == "interrupted" && th.Active == last.Request.TurnID && last.Response == nil {
				if err := r.repository.AbandonTurn(ctx, stream, agent, last.Request.TurnID, r.s.now()); err != nil {
					return err
				}
				th, err = r.repository.Thread(stream, agent)
				if err != nil {
					return err
				}
				last = th.Turns[len(th.Turns)-1]
			}
			if last.Status() == "interrupted" && !paused {
				return r.recover(ctx, last)
			}
			if last.CompletedAt.IsZero() {
				return nil
			}
			if last.Status() != "idle" || last.Response.Result.Outcome == nil || last.Response.Result.Outcome.Status != verdictOutcome {
				return nil
			}
			return r.finishReview(ctx, stream, unit, state, last)
		}
	}
	if paused {
		return nil
	}
	req, identity, err := r.prepareUnitReview(ctx, stream, unit)
	if err != nil {
		return nil
	} // preparation records the block for the chief
	if err := r.ensureThread(ctx, stream, unit); err != nil {
		return err
	}
	profile, _, err := r.s.roleExecution(r.cfg, reviewerRole)
	if err != nil {
		return err
	}
	content, _ := json.MarshalIndent(identity, "", "  ")
	prompt := fmt.Sprintf("Review this exact candidate against the sealed spec, plan and mason report. Record criterion-linked evidence with verdict. Explain each changed path outside the sealed footprint in extra_paths. Unresolved or ambiguous path mappings require a plan amendment before approval. Material findings must say what the mason should change. The candidate identity is:\n%s\n\nExact diff:\n%s", content, req.Diff)
	guidance, err := r.reviewGuidance(stream, unit, identity.Candidate.Revision)
	if err != nil {
		return err
	}
	prompt += guidance
	for _, item := range req.Context {
		prompt += "\n\n" + item.Source + ":\n" + item.Content
	}
	request := trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_" + turnID, Revision: 1, Project: r.repository.Project(), Workstream: stream, Unit: unit, At: r.s.now(), Actor: reviewerActor, Cause: reviewDocument(unit), Depth: 1}, AgentID: agent, ThreadID: agent, TurnID: turnID, Profile: profile, SystemPrompt: "You are the unit reviewer. Read only the supplied candidate evidence. Call ask if a decision is needed and end the turn; review resumes when the answer arrives. Call amend with sealed spec or plan citations, proposed change and reason if those documents need to change, then end the turn. Otherwise call verdict with your decision. You cannot edit the candidate.", Prompt: prompt}
	_, err = r.repository.EnqueueTurn(ctx, request)
	return err
}

func (r *reviewers) reviewGuidance(stream config.WorkstreamID, unit, candidate string) (string, error) {
	asked, err := r.repository.Questions(stream)
	if err != nil {
		return "", err
	}
	var guidance string
	for _, q := range asked {
		if q.Asked.Thread == reviewerAgent(unit) && q.State == trace.QuestionAnswered && q.Ruling != nil {
			guidance += "\n\n" + questions.Prompt(q.Asked, *q.Ruling)
		}
	}
	docs, err := trace.Read[trace.Document](r.repository, stream)
	if err != nil {
		return "", err
	}
	var latest ContestedRuling
	for _, d := range docs {
		if strings.HasPrefix(d.Path, "units/"+trace.UnitSubject(unit)+"/ruling-") {
			var ruling ContestedRuling
			if json.Unmarshal([]byte(d.Content), &ruling) == nil && ruling.Bounces > latest.Bounces {
				latest = ruling
			}
		}
	}
	if latest.Decision == "review" && latest.Candidate == candidate {
		guidance += fmt.Sprintf("\n\nThe owner ruled on contested candidate %s: review it again. Owner note: %s", latest.Candidate, latest.Note)
	}
	return guidance, nil
}

func (r *reviewers) ensureThread(ctx context.Context, stream config.WorkstreamID, unit string) error {
	agent := reviewerAgent(unit)
	if _, err := r.repository.Thread(stream, agent); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return r.repository.CreateThread(ctx, trace.Agent{Header: trace.Header{Schema: "osmia.trace.agent", Version: trace.Version, ID: agent, Revision: 1, Project: r.repository.Project(), Workstream: stream, Unit: unit, At: r.s.now(), Actor: reviewerActor, Cause: reviewDocument(unit)}, Role: reviewerRole, ThreadID: agent})
}

func (r *reviewers) recover(ctx context.Context, last trace.QueuedTurn) error {
	req := last.Request
	req.ID = "request_" + req.TurnID + "-recover-" + fmt.Sprint(last.Sequence)
	req.TurnID += "-recover-" + fmt.Sprint(last.Sequence)
	req.Cause = last.Response.ID
	req.At = r.s.now()
	req.Prompt += "\n\nThe previous turn was interrupted. Review the same exact candidate and record a verdict."
	_, err := r.repository.EnqueueTurn(ctx, req)
	return err
}

func (r *reviewers) storedResult(stream config.WorkstreamID, unit string, state trace.WorkflowState) (UnitReviewResult, bool, error) {
	docs, err := trace.Read[trace.Document](r.repository, stream)
	if err != nil {
		return UnitReviewResult{}, false, err
	}
	var latest trace.Document
	for _, d := range docs {
		if d.ID == reviewDocument(unit) {
			latest = d
		}
	}
	var result UnitReviewResult
	if latest.Revision == 0 || json.Unmarshal([]byte(latest.Content), &result) != nil || result.Turn == "" {
		return UnitReviewResult{}, false, nil
	}
	if state.Value == UnitReviewing && result.Turn != reviewTurnID(unit, state.Version) && !strings.HasPrefix(result.Turn, reviewTurnID(unit, state.Version)+"-recover-") {
		if !strings.HasPrefix(result.Turn, "answer_") {
			return UnitReviewResult{}, false, nil
		}
		transitions, err := trace.Read[trace.Transition](r.repository, stream)
		if err != nil {
			return UnitReviewResult{}, false, err
		}
		latest := trace.Transition{}
		for _, t := range transitions {
			if t.Subject == trace.UnitSubject(unit) {
				latest = t
			}
		}
		if latest.From != UnitWaiting || latest.To != UnitReviewing || latest.Cause != trace.QuestionSubject(answerQuestionID(result.Turn))+"_"+trace.QuestionAnswered {
			return UnitReviewResult{}, false, nil
		}
	}
	return result, true, nil
}

// turnIdentity follows answer turns back to the exact evidence that prompted
// the question. Current workspace contents cannot replace that identity.
func (r *reviewers) turnIdentity(stream config.WorkstreamID, unit string, turn trace.QueuedTurn) (UnitReviewIdentity, error) {
	if !strings.HasPrefix(turn.Request.TurnID, "answer_") {
		return reviewIdentityInPrompt(turn.Request.Prompt)
	}
	thread, err := r.repository.Thread(stream, reviewerAgent(unit))
	if err != nil {
		return UnitReviewIdentity{}, err
	}
	asked, err := r.repository.Questions(stream)
	if err != nil {
		return UnitReviewIdentity{}, err
	}
	for range thread.Turns {
		question := answerQuestionID(turn.Request.TurnID)
		i := slices.IndexFunc(asked, func(q trace.QuestionState) bool {
			return q.Asked.ID == question && q.Asked.Thread == thread.Identity.ThreadID
		})
		if i < 0 {
			break
		}
		j := slices.IndexFunc(thread.Turns, func(t trace.QueuedTurn) bool { return t.Request.TurnID == asked[i].Asked.Turn })
		if j < 0 {
			break
		}
		turn = thread.Turns[j]
		if !strings.HasPrefix(turn.Request.TurnID, "answer_") {
			return reviewIdentityInPrompt(turn.Request.Prompt)
		}
	}
	return UnitReviewIdentity{}, errors.New("review answer has no original candidate identity")
}

func (r *reviewers) finishReview(ctx context.Context, stream config.WorkstreamID, unit string, state trace.WorkflowState, turn trace.QueuedTurn) error {
	var verdict UnitVerdict
	if err := json.Unmarshal([]byte(turn.Response.Result.Outcome.Report), &verdict); err != nil {
		return err
	}
	identity, err := r.turnIdentity(stream, unit, turn)
	if err != nil {
		return err
	}
	_, current, evidenceErr := r.unitReviewEvidence(ctx, stream, unit)
	reviewed := identity
	if evidenceErr == nil {
		if reviewed, err = r.carried(stream, unit, identity, current); err != nil {
			return err
		}
	}
	if evidenceErr == nil && staleReview(reviewed, current) == "" {
		planned, err := sealedUnit(r.repository, coreadapter.Scope{Workstream: string(stream), Unit: unit})
		if err != nil {
			return err
		}
		if reason := validateVerdict(planned, verdict); reason != "" {
			return r.recordReviewPreparationError(ctx, stream, unit, turn.Response.ID, "unit "+unit+" stays reviewing: "+reason)
		}
	}
	result := UnitReviewResult{Identity: identity, Turn: turn.Request.TurnID, Verdict: verdict}
	docs, err := trace.Read[trace.Document](r.repository, stream)
	if err != nil {
		return err
	}
	var latest trace.Document
	for _, d := range docs {
		if d.ID == reviewDocument(unit) {
			latest = d
			var prior UnitReviewResult
			if json.Unmarshal([]byte(d.Content), &prior) == nil && prior.Verdict.Decision == "material_findings" && prior.Bounces > result.Bounces {
				result.Bounces = prior.Bounces
			}
		}
	}
	if verdict.Decision == "material_findings" {
		result.Bounces++
	}
	content, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	h := latest.Header
	h.Revision++
	h.At = r.s.now()
	h.Actor = reviewerActor
	h.Cause = turn.Response.ID
	if err := r.repository.RecordDocuments(ctx, []trace.Document{{Header: h, Path: latest.Path, Content: string(content) + "\n"}}); err != nil {
		return err
	}
	return r.applyReview(ctx, stream, unit, state, result)
}

func validateVerdict(unit plan.Unit, v UnitVerdict) string {
	if v.Decision != "satisfactory" && v.Decision != "material_findings" {
		return "invalid review decision"
	}
	want := map[string]bool{}
	for _, a := range unit.Addresses {
		want[a.Criterion] = true
	}
	seen := map[string]bool{}
	for _, e := range v.Evidence {
		if !want[e.Criterion] || seen[e.Criterion] || strings.TrimSpace(e.Evidence) == "" {
			return "review evidence must cite each unit criterion once"
		}
		seen[e.Criterion] = true
	}
	if len(seen) != len(want) {
		return "review evidence must cite each unit criterion once"
	}
	if v.Decision == "satisfactory" && len(v.Findings) > 0 || v.Decision == "material_findings" && len(v.Findings) == 0 {
		return "review findings do not match the decision"
	}
	for _, f := range v.Findings {
		if !want[f.Criterion] || strings.TrimSpace(f.Severity) == "" || strings.TrimSpace(f.Evidence) == "" || strings.TrimSpace(f.Action) == "" {
			return "review finding needs an addressed criterion, severity, evidence and action"
		}
	}
	return ""
}

func (r *reviewers) applyReview(ctx context.Context, stream config.WorkstreamID, unit string, state trace.WorkflowState, result UnitReviewResult) error {
	// The durable result retains every governing revision and the diff digest.
	if result.Identity.Subject != string(stream)+"/"+unit || result.Identity.Candidate.Revision == "" || result.Identity.Candidate.BaseRevision == "" || result.Identity.Candidate.SpecRevision == "" || result.Identity.Candidate.PlanRevision == "" || result.Identity.DiffSHA256 == "" {
		return errors.New("review result has incomplete candidate identity")
	}
	if reason, err := r.staleInputs(ctx, stream, unit, result.Identity); err != nil {
		return err
	} else if reason != "" {
		return r.refreshReview(ctx, stream, unit, state, reason)
	}
	planned, err := sealedUnit(r.repository, coreadapter.Scope{Workstream: string(stream), Unit: unit})
	if err != nil {
		return err
	}
	if reason := validateVerdict(planned, result.Verdict); reason != "" {
		return errors.New(reason)
	}
	if result.Verdict.Decision == "satisfactory" {
		if reason, err := r.checkReviewFootprint(ctx, stream, unit, result); err != nil {
			return err
		} else if reason != "" {
			return r.refreshReview(ctx, stream, unit, state, reason)
		}
	}
	to := UnitApproved
	if result.Verdict.Decision == "material_findings" {
		to = UnitImplementing
		if result.Bounces >= r.cfg.Shed.MaxBounces {
			to = UnitContested
		}
	}
	id := fmt.Sprintf("%s-%s-%d", trace.UnitSubject(unit), to, state.Version)
	reason := fmt.Sprintf("reviewer verdict %s on %s: candidate %s from %s, spec %s, plan %s; review %s", result.Verdict.Decision, result.Turn, result.Identity.Candidate.Revision, result.Identity.Candidate.BaseRevision, result.Identity.Candidate.SpecRevision, result.Identity.Candidate.PlanRevision, reviewDocument(unit))
	if to == UnitContested {
		reason += fmt.Sprintf("; %d material send-backs reached shed.max_bounces; the owner must rule review or revise before the unit moves", result.Bounces)
	}
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: r.repository.Project(), Workstream: stream, Unit: unit, At: r.s.now(), Actor: reviewerActor, Cause: reviewDocument(unit)}
	_, err = r.repository.Transact(ctx, trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: h, Subject: trace.UnitSubject(unit), From: UnitReviewing, To: to, Reason: reason}, Events: []trace.Event{trace.Notice(id, "unit", fmt.Sprintf("Unit %s review %s: %s.", unit, result.Verdict.Decision, reason))}})
	if errors.Is(err, trace.ErrConflict) {
		return nil
	}
	if err != nil {
		return err
	}
	if to == UnitImplementing {
		return r.enqueueFindings(ctx, stream, unit, result)
	}
	return nil
}

func reviewIdentityInPrompt(prompt string) (UnitReviewIdentity, error) {
	const start, end = "The candidate identity is:\n", "\n\nExact diff:\n"
	_, body, ok := strings.Cut(prompt, start)
	if !ok {
		return UnitReviewIdentity{}, errors.New("review turn lacks candidate identity")
	}
	body, _, ok = strings.Cut(body, end)
	if !ok {
		return UnitReviewIdentity{}, errors.New("review turn lacks exact diff")
	}
	var identity UnitReviewIdentity
	err := json.Unmarshal([]byte(body), &identity)
	return identity, err
}

func staleReview(reviewed, current UnitReviewIdentity) string {
	for _, field := range []struct{ name, old, now string }{
		{"candidate", reviewed.Candidate.Revision, current.Candidate.Revision},
		{"base", reviewed.Candidate.BaseRevision, current.Candidate.BaseRevision},
		{"spec", reviewed.Candidate.SpecRevision, current.Candidate.SpecRevision},
		{"plan", reviewed.Candidate.PlanRevision, current.Candidate.PlanRevision},
		{"diff", reviewed.DiffSHA256, current.DiffSHA256},
		{"mason report", reviewed.Report, current.Report},
	} {
		if field.old != field.now {
			return "stale " + field.name + " revision; review the current candidate again"
		}
	}
	if reviewed.Seal != current.Seal {
		return "stale seal; review the current candidate again"
	}
	return ""
}

// staleInputs returns why a reviewed identity no longer names the unit's
// current report, seal, candidate, base, spec and plan: the recorded report
// and seal, the tips of the unit branch and the feature branch, and the latest
// spec and plan revisions. A seal, spec and plan that approved amendments
// replaced without reworking, notifying or removing the unit are current. It
// returns "" when every input is current.
func (m *masons) staleInputs(ctx context.Context, stream config.WorkstreamID, unit string, reviewed UnitReviewIdentity) (string, error) {
	_, current, err := m.candidateEvidence(ctx, stream, unit)
	if err != nil {
		return "stale review inputs: " + err.Error(), nil
	}
	if reviewed, err = m.carried(stream, unit, reviewed, current); err != nil {
		return "", err
	}
	if reason := staleReview(reviewed, current); reason != "" {
		return reason, nil
	}
	git := newUnitWorkspaces(m.cfg).git
	candidate, exists, err := git.Branch(ctx, unitBranch(stream, unit))
	if err != nil {
		return "", err
	}
	if !exists || candidate != reviewed.Candidate.Revision {
		return "stale candidate revision; review the current candidate again", nil
	}
	base, exists, err := git.Branch(ctx, featureBranch(stream))
	if err != nil {
		return "", err
	}
	if !exists || base != reviewed.Candidate.BaseRevision {
		return "stale base revision; review the current candidate again", nil
	}
	docs, err := trace.Read[trace.Document](m.repository, stream)
	if err != nil {
		return "", err
	}
	latest := map[string]int{}
	for _, d := range docs {
		if d.Revision > latest[d.ID] {
			latest[d.ID] = d.Revision
		}
	}
	if fmt.Sprint(latest[plan.SpecDocument]) != reviewed.Candidate.SpecRevision {
		return "stale spec revision; review the current spec again", nil
	}
	if fmt.Sprint(latest[plan.PlanDocument]) != reviewed.Candidate.PlanRevision {
		return "stale plan revision; review the current plan again", nil
	}
	return "", nil
}

func (r *reviewers) refreshReview(ctx context.Context, stream config.WorkstreamID, unit string, state trace.WorkflowState, reason string) error {
	id := fmt.Sprintf("%s-review-refresh-%d", trace.UnitSubject(unit), state.Version)
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: r.repository.Project(), Workstream: stream, Unit: unit, At: r.s.now(), Actor: reviewerActor, Cause: reviewDocument(unit)}
	_, err := r.repository.Transact(ctx, trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: h, Subject: trace.UnitSubject(unit), From: UnitReviewing, To: UnitReviewing, Reason: reason}, Events: []trace.Event{trace.Notice(id, "chief", "Unit "+unit+" stays reviewing: "+reason)}})
	if errors.Is(err, trace.ErrConflict) {
		return nil
	}
	return err
}

func (r *reviewers) checkReviewFootprint(ctx context.Context, stream config.WorkstreamID, unit string, result UnitReviewResult) (string, error) {
	s, _, found, err := seal.Latest(r.repository, stream)
	if err != nil {
		return "", err
	}
	if !found {
		return "missing seal", nil
	}
	footprint, err := reviewFootprint(r.repository, stream, s, unit)
	if err != nil || len(footprint.Entities) == 0 {
		return "missing sealed footprint", nil
	}
	paths, err := newUnitWorkspaces(r.cfg).git.ChangedPaths(ctx, result.Identity.Candidate.BaseRevision, result.Identity.Candidate.Revision)
	if err != nil {
		return "", err
	}
	mapping, err := kb.Load(r.repository)
	if err != nil {
		return "", err
	}
	return footprintReason(mapping, footprint, paths, result.Verdict.ExtraPaths), nil
}

func footprintReason(mapping kb.Map, footprint seal.Footprint, paths []string, explanations []PathExplanation) string {
	resolved := mapping.ResolvePaths(paths)
	if len(resolved.Unresolved) != 0 {
		return "unresolved changed paths " + strings.Join(resolved.Unresolved, ", ") + "; request an owner approved plan amendment"
	}
	extras := map[string]bool{}
	for _, match := range resolved.Matches {
		if len(match.Entities) != 1 {
			return "ambiguous mapping for " + match.Path + "; request an owner approved plan amendment"
		}
		inSealedPath := slices.ContainsFunc(footprint.Paths, func(pattern string) bool { return kb.MatchPathPattern(pattern, match.Path) })
		if !slices.Contains(footprint.Entities, match.Entities[0]) || !inSealedPath {
			extras[match.Path] = true
		}
	}
	explained := map[string]bool{}
	for _, extra := range explanations {
		if !extras[extra.Path] || explained[extra.Path] || strings.TrimSpace(extra.Explanation) == "" {
			return "invalid explanation for changed path " + extra.Path
		}
		explained[extra.Path] = true
	}
	for _, path := range paths {
		if extras[path] && !explained[path] {
			return "unexplained changed path " + path + "; request an owner approved plan amendment when the footprint must change"
		}
	}
	return ""
}

func (r *reviewers) enqueueFindings(ctx context.Context, stream config.WorkstreamID, unit string, result UnitReviewResult) error {
	th, err := r.repository.Thread(stream, masonAgent(unit))
	if err != nil {
		return err
	}
	turn := fmt.Sprintf("%s-revise-%s", masonAgent(unit), strings.TrimPrefix(result.Turn, reviewerAgent(unit)+"-review-"))
	if slices.ContainsFunc(th.Turns, func(q trace.QueuedTurn) bool { return q.Request.TurnID == turn }) {
		return nil
	}
	profile, _, err := r.s.roleExecution(r.cfg, masonRole)
	if err != nil {
		return err
	}
	findings, _ := json.MarshalIndent(result.Verdict.Findings, "", "  ")
	request := trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_" + turn, Revision: 1, Project: r.repository.Project(), Workstream: stream, Unit: unit, At: r.s.now(), Actor: reviewerActor, Cause: result.Turn, Depth: 1}, AgentID: masonAgent(unit), ThreadID: masonAgent(unit), TurnID: turn, Profile: profile, SystemPrompt: masonSystemPrompt(r.cfg.Project), Prompt: fmt.Sprintf("The reviewer returned candidate %s for revision. Address these criterion-linked findings in your unit workspace, run the planned proofs, then call done with a new criterion report:\n%s", result.Identity.Candidate.Revision, findings)}
	ruling, found, err := latestContestedRuling(r.repository, stream, unit, result.Bounces)
	if err != nil {
		return err
	}
	if found && ruling.Decision == "revise" && ruling.Candidate == result.Identity.Candidate.Revision {
		request.Prompt += "\n\nThe owner ruled that this candidate needs revision: " + ruling.Note
	}
	_, err = r.repository.EnqueueTurn(ctx, request)
	return err
}
