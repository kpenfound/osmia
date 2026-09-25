package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/trace"
)

const AmendmentDraftAction = "architect-amendment-draft"

var errAmendmentText = errors.New("not UTF-8 text")

type amendmentDrafter struct{ *drafter }

type amendmentDraftInput struct {
	ID string `json:"id"`
}

func amendmentTurnID(turn string) (string, bool) {
	var id, attempt int
	if _, err := fmt.Sscanf(turn, "amend-%d-%d", &id, &attempt); err != nil || id < 1 || attempt < 1 || turn != fmt.Sprintf("amend-%d-%d", id, attempt) {
		return "", false
	}
	return fmt.Sprint(id), true
}

func amendmentSubject(id string) string { return "amendment_" + id }

func (a *amendmentDrafter) Pass(ctx context.Context) error {
	if a.s.options.Architect == nil {
		return nil
	}
	streams, err := a.repository.Workstreams()
	if err != nil {
		return err
	}
	for _, stream := range streams {
		feature, err := a.repository.Workflow(stream, trace.FeatureSubject)
		if err != nil {
			return err
		}
		if feature.Value != "building" && feature.Value != "assembled" {
			continue
		}
		// A paused workstream is asked for nothing until the pause is lifted.
		if _, paused := a.s.pausing(a.repository.Project(), stream); paused {
			continue
		}
		requests, err := trace.Read[trace.Amendment](a.repository, stream)
		if err != nil {
			return err
		}
		for _, request := range requests {
			state, err := a.repository.Workflow(stream, amendmentSubject(request.ID))
			if err != nil {
				return err
			}
			if state.Value != "filed" {
				continue
			}
			if err := a.ensureThread(ctx, stream); err != nil {
				return err
			}
			input, err := json.Marshal(amendmentDraftInput{request.ID})
			if err != nil {
				return err
			}
			id := "amendment-" + request.ID + "-draft"
			event := trace.EventID(id, "run")
			op := coreadapter.Operation{ID: trace.OperationID(a.repository.Project(), stream, event), Boundary: coreadapter.RunnerBoundary, Action: AmendmentDraftAction, Input: input}
			tx := trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: a.header(id, stream, "amendment_"+request.ID+"_filed", a.s.now()), Subject: amendmentSubject(request.ID), From: "filed", To: "drafting", Reason: "the architect drafts a revision against the sealed documents"}, Events: []trace.Event{{ID: event, Kind: "draft", Body: "Draft amendment " + request.ID, Operation: &op}}}
			if _, err := a.repository.Transact(ctx, tx); err != nil {
				return err
			}
		}
	}
	return nil
}

func (a *amendmentDrafter) locate(op coreadapter.Operation) (config.WorkstreamID, trace.Amendment, error) {
	var in amendmentDraftInput
	if op.Action != AmendmentDraftAction || op.Boundary != coreadapter.RunnerBoundary || json.Unmarshal(op.Input, &in) != nil || in.ID == "" {
		return "", trace.Amendment{}, errors.New("invalid amendment draft operation")
	}
	streams, err := a.repository.Workstreams()
	if err != nil {
		return "", trace.Amendment{}, err
	}
	for _, stream := range streams {
		if trace.OperationID(a.repository.Project(), stream, trace.EventID("amendment-"+in.ID+"-draft", "run")) != op.ID {
			continue
		}
		requests, err := trace.Read[trace.Amendment](a.repository, stream)
		if err != nil {
			return "", trace.Amendment{}, err
		}
		for _, request := range requests {
			if request.ID == in.ID {
				return stream, request, nil
			}
		}
	}
	return "", trace.Amendment{}, fmt.Errorf("amendment draft operation %s has no request", op.ID)
}

func (a *amendmentDrafter) outcome(stream config.WorkstreamID, id, operation string) (*coreadapter.OperationResult, error) {
	transitions, err := trace.Read[trace.Transition](a.repository, stream)
	if err != nil {
		return nil, err
	}
	for _, t := range transitions {
		if t.Subject == amendmentSubject(id) && t.Cause == operation && (t.To == "proposed" || t.To == "invalid" || t.To == "declined") {
			result := coreadapter.OperationResult{Outcome: "succeeded", Evidence: t.Reason}
			if t.To == "invalid" {
				result.Outcome = "failed"
			}
			return &result, nil
		}
	}
	return nil, nil
}

func (a *amendmentDrafter) Inspect(_ context.Context, op coreadapter.Operation) (coreadapter.Observation, error) {
	stream, request, err := a.locate(op)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	if result, err := a.outcome(stream, request.ID, op.ID); err != nil || result != nil {
		return coreadapter.Observation{State: coreadapter.EffectCompleted, Evidence: resultEvidence(result), Result: result}, err
	}
	thread, err := a.repository.Thread(stream, architectAgent)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	for _, turn := range thread.Turns {
		if id, ok := amendmentTurnID(continuedTurn(turn.Request.TurnID)); ok && id == request.ID && turn.Claim != nil && turn.Response == nil && thread.Status != "interrupted" {
			return coreadapter.Observation{State: coreadapter.EffectUnknown, Evidence: "architect turn is running"}, nil
		}
	}
	return coreadapter.Observation{State: coreadapter.EffectAbsent, Evidence: "no amendment outcome or running architect turn"}, nil
}

func resultEvidence(result *coreadapter.OperationResult) string {
	if result == nil {
		return "amendment outcome lookup found no result"
	}
	return "amendment outcome recorded: " + result.Evidence
}

func (a *amendmentDrafter) Apply(ctx context.Context, op coreadapter.Operation) (coreadapter.OperationResult, error) {
	stream, request, err := a.locate(op)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	if result, err := a.outcome(stream, request.ID, op.ID); err != nil || result != nil {
		if err != nil {
			return coreadapter.OperationResult{}, err
		}
		return *result, nil
	}
	if a.s.options.Architect == nil {
		return coreadapter.OperationResult{}, errNoArchitect
	}
	cfg := a.s.current()
	for {
		if err := ctx.Err(); err != nil {
			return coreadapter.OperationResult{}, err
		}
		thread, err := a.repository.Thread(stream, architectAgent)
		if err != nil {
			return coreadapter.OperationResult{}, err
		}
		// A turn a hard pause stopped is continued within its attempt, so
		// only the attempts' own turns count.
		var turns []trace.QueuedTurn
		tries := 0
		for _, turn := range thread.Turns {
			if id, ok := amendmentTurnID(continuedTurn(turn.Request.TurnID)); ok && id == request.ID {
				turns = append(turns, turn)
				if !isContinuation(turn.Request.TurnID) {
					tries++
				}
			}
		}
		if len(turns) > 0 && stoppedTurn(&turns[len(turns)-1]) {
			if err := a.s.held(a.repository, stream); err != nil {
				return coreadapter.OperationResult{}, err
			}
			if _, err := a.s.continueStopped(ctx, a.repository, cfg, architectRole, turns[len(turns)-1]); err != nil {
				return coreadapter.OperationResult{}, err
			}
			continue
		}
		if len(turns) == 0 || turns[len(turns)-1].Status() == "interrupted" {
			if tries >= maxDraftAttempts {
				return a.finish(ctx, op.ID, stream, request.ID, "invalid", "the architect turn was interrupted too many times", nil)
			}
			if err := a.s.held(a.repository, stream); err != nil {
				return coreadapter.OperationResult{}, err
			}
			profile, _, err := a.s.roleExecution(cfg, architectRole)
			if err != nil {
				return coreadapter.OperationResult{}, err
			}
			turn := fmt.Sprintf("amend-%s-%d", request.ID, tries+1)
			prompt := fmt.Sprintf("Draft amendment %s. Read request.json, draft/spec.md, draft/plan.json, charter.md and context.md. Deliver only the changed spec.md and/or plan.json with %s; unchanged documents will be carried forward. To decline, deliver decline.txt with your reason. Keep merged units identical. Do not edit code. Every dependency must name an existing unit, the graph must be acyclic, and every footprint must resolve against the entity map.", request.ID, DraftTool)
			system := fmt.Sprintf("You are the architect of the %s project (%s). Draft a revision to the sealed spec and plan for the amendment request. Read only your scoped files, deliver through %s, and do not edit implementation code or use version control.", cfg.Project.Name, cfg.Project.Upstream, DraftTool)
			req := trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_" + turn, Revision: 1, Project: a.repository.Project(), Workstream: stream, At: a.s.now(), Actor: draftingActor, Cause: op.ID, Depth: 1}, AgentID: architectAgent, ThreadID: architectThread, TurnID: turn, Profile: profile, SystemPrompt: system, Prompt: prompt}
			if _, err := a.repository.EnqueueTurn(ctx, req); err != nil {
				return coreadapter.OperationResult{}, err
			}
			continue
		}
		last := turns[len(turns)-1]
		if last.Claim != nil && last.Response == nil && thread.Status == "interrupted" {
			if err := a.repository.AbandonTurn(ctx, stream, architectAgent, last.Request.TurnID, a.s.now()); err != nil {
				return coreadapter.OperationResult{}, err
			}
			continue
		}
		if last.CompletedAt.IsZero() {
			if last.Response == nil {
				if err := a.s.held(a.repository, stream); err != nil {
					return coreadapter.OperationResult{}, err
				}
			}
			if _, err := a.dispatch(ctx, stream, last.Request.TurnID); err != nil {
				return coreadapter.OperationResult{}, err
			}
			if err := os.RemoveAll(filepath.Join(a.turnDirectory(stream, last.Request.TurnID), "workspace")); err != nil {
				return coreadapter.OperationResult{}, err
			}
			continue
		}
		if last.Status() != "idle" {
			return a.finish(ctx, op.ID, stream, request.ID, "invalid", "architect turn failed: "+last.Response.Failure, nil)
		}
		return a.record(ctx, op.ID, stream, request, continuedTurn(last.Request.TurnID))
	}
}

type amendmentAffected struct {
	Criteria []string `json:"criteria"`
	Units    []string `json:"units"`
	Proofs   []string `json:"proofs"`
}

func affectedRevision(oldSpec, newSpec string, oldPlan, newPlan plan.Plan) amendmentAffected {
	a := amendmentAffected{Criteria: []string{}, Units: []string{}, Proofs: []string{}}
	oldCriteria := map[int]string{}
	for _, c := range plan.ParseSpec(oldSpec).Criteria {
		oldCriteria[c.Number] = c.Text
	}
	newCriteria := map[int]string{}
	for _, c := range plan.ParseSpec(newSpec).Criteria {
		newCriteria[c.Number] = c.Text
	}
	changed := map[string]bool{}
	for n, old := range oldCriteria {
		if newCriteria[n] != old {
			changed[plan.Cite(n)] = true
		}
	}
	for n, now := range newCriteria {
		if oldCriteria[n] != now {
			changed[plan.Cite(n)] = true
		}
	}
	for citation := range changed {
		a.Criteria = append(a.Criteria, citation)
	}
	oldUnits := map[string]plan.Unit{}
	newUnits := map[string]plan.Unit{}
	for _, u := range oldPlan.Units {
		oldUnits[u.ID] = u
	}
	for _, u := range newPlan.Units {
		newUnits[u.ID] = u
	}
	unitIDs := map[string]bool{}
	for id := range oldUnits {
		unitIDs[id] = true
	}
	for id := range newUnits {
		unitIDs[id] = true
	}
	proofs := map[string]bool{}
	for id := range unitIDs {
		old, was := oldUnits[id]
		now, is := newUnits[id]
		touched := !was || !is || !reflect.DeepEqual(old, now)
		for _, u := range []plan.Unit{old, now} {
			for _, address := range u.Addresses {
				if changed[address.Criterion] {
					touched = true
				}
			}
		}
		if touched {
			a.Units = append(a.Units, id)
		}
		oldProof := map[string]plan.Proof{}
		newProof := map[string]plan.Proof{}
		for _, address := range old.Addresses {
			oldProof[address.Criterion] = address.Proof
		}
		for _, address := range now.Addresses {
			newProof[address.Criterion] = address.Proof
		}
		for citation, proof := range oldProof {
			if touched || changed[citation] || !reflect.DeepEqual(proof, newProof[citation]) {
				proofs[id+":"+citation] = true
			}
		}
		for citation, proof := range newProof {
			if touched || changed[citation] || !reflect.DeepEqual(proof, oldProof[citation]) {
				proofs[id+":"+citation] = true
			}
		}
	}
	for proof := range proofs {
		a.Proofs = append(a.Proofs, proof)
	}
	slices.Sort(a.Criteria)
	slices.Sort(a.Units)
	slices.Sort(a.Proofs)
	return a
}

func (a *amendmentDrafter) record(ctx context.Context, operation string, stream config.WorkstreamID, request trace.Amendment, turn string) (coreadapter.OperationResult, error) {
	latest, err := a.latest(stream)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	oldSpec, oldPlan := latest[plan.SpecDocument].Content, latest[plan.PlanDocument].Content
	if oldSpec == "" || oldPlan == "" {
		return coreadapter.OperationResult{}, errors.New("sealed spec or plan is missing")
	}
	directory := filepath.Join(a.turnDirectory(stream, turn), "output")
	read := func(name string) (string, bool, error) {
		data, err := os.ReadFile(filepath.Join(directory, name))
		if os.IsNotExist(err) {
			return "", false, nil
		}
		if err == nil && !utf8.Valid(data) {
			return "", false, fmt.Errorf("%s: %w", name, errAmendmentText)
		}
		return string(data), err == nil, err
	}
	readFailure := func(err error) (coreadapter.OperationResult, error) {
		if errors.Is(err, errAmendmentText) {
			return a.finish(ctx, operation, stream, request.ID, "invalid", err.Error(), nil)
		}
		return coreadapter.OperationResult{}, err
	}
	if reason, ok, err := read("decline.txt"); err != nil {
		return readFailure(err)
	} else if ok {
		if strings.TrimSpace(reason) == "" {
			reason = "the architect gave no reason"
		}
		return a.finish(ctx, operation, stream, request.ID, "declined", reason, nil)
	}
	spec, hasSpec, err := read(plan.SpecPath)
	if err != nil {
		return readFailure(err)
	}
	graph, hasPlan, err := read(plan.PlanPath)
	if err != nil {
		return readFailure(err)
	}
	if !hasSpec {
		spec = oldSpec
	}
	if !hasPlan {
		graph = oldPlan
	}
	if !hasSpec && !hasPlan {
		return a.finish(ctx, operation, stream, request.ID, "invalid", "no revised document was delivered", nil)
	}
	problems, err := a.validate(trace.Document{Content: spec}, trace.Document{Content: graph})
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	before, oldErr := plan.Parse([]byte(oldPlan))
	after, newErr := plan.Parse([]byte(graph))
	if oldErr != nil {
		return coreadapter.OperationResult{}, oldErr
	}
	if newErr == nil {
		for _, old := range before.Units {
			state, err := a.repository.Workflow(stream, trace.UnitSubject(old.ID))
			if err != nil {
				return coreadapter.OperationResult{}, err
			}
			if state.Value == "merged" {
				now, ok := after.Unit(old.ID)
				if !ok || !reflect.DeepEqual(old, now) {
					problems = append(problems, fmt.Sprintf("merged unit %s was rewritten", old.ID))
				}
			}
		}
	}
	if len(problems) > 0 {
		return a.finish(ctx, operation, stream, request.ID, "invalid", strings.Join(problems, "; "), nil)
	}
	if spec == oldSpec && graph == oldPlan {
		return a.finish(ctx, operation, stream, request.ID, "invalid", "revision makes no change", nil)
	}
	affected := affectedRevision(oldSpec, spec, before, after)
	data, err := json.MarshalIndent(affected, "", "  ")
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	prefix := "amendments/" + request.ID + "/"
	at := a.s.now()
	var docs []trace.Document
	for _, item := range []struct{ id, path, content string }{{"amendment_" + request.ID + "_spec", prefix + plan.SpecPath, spec}, {"amendment_" + request.ID + "_plan", prefix + plan.PlanPath, graph}, {"amendment_" + request.ID + "_affected", prefix + "affected.json", string(data) + "\n"}} {
		docs = append(docs, trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: item.id, Revision: 1, Project: a.repository.Project(), Workstream: stream, At: at, Actor: architectActor, Cause: operation, Depth: 1}, Path: item.path, Content: item.content})
	}
	return a.finish(ctx, operation, stream, request.ID, "proposed", "the architect proposed a validated amendment revision", docs)
}

func (a *amendmentDrafter) finish(ctx context.Context, operation string, stream config.WorkstreamID, id, outcome, reason string, docs []trace.Document) (coreadapter.OperationResult, error) {
	state, err := a.repository.Workflow(stream, amendmentSubject(id))
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	if state.Value != "drafting" {
		result, err := a.outcome(stream, id, operation)
		if err != nil {
			return coreadapter.OperationResult{}, err
		}
		if result != nil {
			return *result, nil
		}
		return coreadapter.OperationResult{}, trace.ErrConflict
	}
	tx := trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: a.header("amendment-"+id+"-"+outcome, stream, operation, a.s.now()), Subject: amendmentSubject(id), From: "drafting", To: outcome, Reason: reason}}
	if len(docs) > 0 {
		_, err = a.repository.RecordDocumentsWith(ctx, docs, tx)
	} else {
		_, err = a.repository.Transact(ctx, tx)
	}
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	result := coreadapter.OperationResult{Outcome: "succeeded", Evidence: reason}
	if outcome == "invalid" {
		result.Outcome = "failed"
	}
	return result, nil
}
