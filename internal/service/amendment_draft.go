package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/trace"
)

const AmendmentDraftAction = "architect-amendment-draft"

// amendmentWaiting is the state of an amendment whose draft is parked on the
// architect's question until the answer is queued on the architect's thread.
const amendmentWaiting = "waiting"

var errAmendmentText = errors.New("not UTF-8 text")

type amendmentDrafter struct{ *drafter }

// amendmentDraftInput names the amendment an operation drafts. Resume numbers
// the operation that drafts it again after it parked on the architect's
// question, from 1; the amendment's own draft operation has none.
type amendmentDraftInput struct {
	ID     string `json:"id"`
	Resume int    `json:"resume,omitempty"`
}

// runIDs returns the transition and event of the operation that drafts the
// amendment: its own, or the resumption the input numbers.
func (in amendmentDraftInput) runIDs() (transition, event string) {
	transition = "amendment-" + in.ID + "-draft"
	if in.Resume > 0 {
		transition = fmt.Sprintf("%s-resume-%d", transition, in.Resume)
	}
	return transition, trace.EventID(transition, "run")
}

// parkedID is the transition that parks the draft the operation runs: the
// k-th park of the amendment follows its k-1-th resumption.
func (in amendmentDraftInput) parkedID() string {
	return fmt.Sprintf("amendment-%s-waiting-%d", in.ID, in.Resume+1)
}

func amendmentTurnID(turn string) (string, bool) {
	var id, attempt int
	if _, err := fmt.Sscanf(turn, "amend-%d-%d", &id, &attempt); err != nil || id < 1 || attempt < 1 || turn != fmt.Sprintf("amend-%d-%d", id, attempt) {
		return "", false
	}
	return fmt.Sprint(id), true
}

func amendmentSubject(id string) string { return "amendment_" + id }

// amendmentChain maps the architect's turns that draft amendment id to the
// attempt each continues: the attempts, the turns that deliver the answers to
// their questions and the turns that continue them after a hard pause. The
// architect's other turns on the amendment, such as its replies in the
// amendment's debate, are not part of the draft.
func amendmentChain(t trace.Thread, id string, asked []trace.QuestionState) map[string]string {
	origins := askChain(t, "amend-"+id+"-", asked)
	maps.DeleteFunc(origins, func(_, origin string) bool {
		got, ok := amendmentTurnID(origin)
		return !ok || got != id
	})
	return origins
}

// Pass requests a draft of every filed amendment of a building or assembled
// workstream, and drafts a parked amendment again once the answer to the
// architect's question is queued on its thread.
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
			switch state.Value {
			case "filed":
				if err := a.ensureThread(ctx, stream); err != nil {
					return err
				}
				err = a.request(ctx, stream, state, amendmentDraftInput{ID: request.ID}, "amendment_"+request.ID+"_filed", "the architect drafts a revision against the sealed documents")
			case amendmentWaiting:
				err = a.resume(ctx, stream, state, request.ID)
			}
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// request publishes the draft the input names as a durable operation and
// moves the amendment to drafting.
func (a *amendmentDrafter) request(ctx context.Context, stream config.WorkstreamID, state trace.WorkflowState, in amendmentDraftInput, cause, reason string) error {
	input, err := json.Marshal(in)
	if err != nil {
		return err
	}
	transition, event := in.runIDs()
	body := "Draft amendment " + in.ID
	if in.Resume > 0 {
		body += ", resumed"
	}
	op := coreadapter.Operation{ID: trace.OperationID(a.repository.Project(), stream, event), Boundary: coreadapter.RunnerBoundary, Action: AmendmentDraftAction, Input: input}
	tx := trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: a.header(transition, stream, cause, a.s.now()), Subject: amendmentSubject(in.ID), From: state.Value, To: "drafting", Reason: reason}, Events: []trace.Event{{ID: event, Kind: "draft", Body: body, Operation: &op}}}
	_, err = a.repository.Transact(ctx, tx)
	return err
}

// resume requests the draft of amendment id again once the answer to the
// architect's question is queued on its thread, caused by the transition
// that parked the draft.
func (a *amendmentDrafter) resume(ctx context.Context, stream config.WorkstreamID, state trace.WorkflowState, id string) error {
	thread, err := a.repository.Thread(stream, architectAgent)
	if err != nil {
		return err
	}
	asked, err := a.repository.Questions(stream)
	if err != nil {
		return err
	}
	if chain := chainTurns(thread, amendmentChain(thread, id, asked)); len(chain) > 0 && askedBy(asked, architectThread, chain[len(chain)-1].Request.TurnID) != "" {
		return nil
	}
	transitions, err := trace.Read[trace.Transition](a.repository, stream)
	if err != nil {
		return err
	}
	var parks []trace.Transition
	for _, t := range transitions {
		if t.Subject == amendmentSubject(id) && strings.HasPrefix(t.ID, "amendment-"+id+"-waiting-") {
			parks = append(parks, t)
		}
	}
	if len(parks) == 0 {
		return fmt.Errorf("amendment %s is %s without a parking transition", id, state.Value)
	}
	return a.request(ctx, stream, state, amendmentDraftInput{ID: id, Resume: len(parks)}, parks[len(parks)-1].ID, fmt.Sprintf("amendment %s resumes: the architect's question is answered", id))
}

// locate returns the workstream, the request and the input of an amendment
// draft operation.
func (a *amendmentDrafter) locate(op coreadapter.Operation) (config.WorkstreamID, trace.Amendment, amendmentDraftInput, error) {
	var in amendmentDraftInput
	if op.Action != AmendmentDraftAction || op.Boundary != coreadapter.RunnerBoundary || json.Unmarshal(op.Input, &in) != nil || in.ID == "" || in.Resume < 0 {
		return "", trace.Amendment{}, in, errors.New("invalid amendment draft operation")
	}
	streams, err := a.repository.Workstreams()
	if err != nil {
		return "", trace.Amendment{}, in, err
	}
	_, event := in.runIDs()
	for _, stream := range streams {
		if trace.OperationID(a.repository.Project(), stream, event) != op.ID {
			continue
		}
		requests, err := trace.Read[trace.Amendment](a.repository, stream)
		if err != nil {
			return "", trace.Amendment{}, in, err
		}
		for _, request := range requests {
			if request.ID == in.ID {
				return stream, request, in, nil
			}
		}
	}
	return "", trace.Amendment{}, in, fmt.Errorf("amendment draft operation %s has no request", op.ID)
}

// outcome returns the recorded result of the operation that drafts amendment
// id: succeeded once it proposed or declined the amendment, failed once it
// recorded the draft invalid, waiting once it parked the draft on the
// architect's question, nil before any of them.
func (a *amendmentDrafter) outcome(stream config.WorkstreamID, id, operation string) (*coreadapter.OperationResult, error) {
	transitions, err := trace.Read[trace.Transition](a.repository, stream)
	if err != nil {
		return nil, err
	}
	for _, t := range transitions {
		if t.Subject != amendmentSubject(id) || t.Cause != operation {
			continue
		}
		switch t.To {
		case "proposed", "declined":
			return &coreadapter.OperationResult{Outcome: "succeeded", Evidence: t.Reason}, nil
		case "invalid":
			return &coreadapter.OperationResult{Outcome: "failed", Evidence: t.Reason}, nil
		case amendmentWaiting:
			return &coreadapter.OperationResult{Outcome: questions.Waiting, Evidence: t.Reason}, nil
		}
	}
	return nil, nil
}

func (a *amendmentDrafter) Inspect(_ context.Context, op coreadapter.Operation) (coreadapter.Observation, error) {
	stream, request, _, err := a.locate(op)
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
	asked, err := a.repository.Questions(stream)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	for _, turn := range chainTurns(thread, amendmentChain(thread, request.ID, asked)) {
		if turn.Claim != nil && turn.Response == nil && thread.Status != "interrupted" {
			return coreadapter.Observation{State: coreadapter.EffectUnknown, Evidence: "architect turn " + turn.Request.TurnID + " is running"}, nil
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

// Apply drives the amendment's draft to a result: it abandons a turn a
// previous service stop interrupted, runs the pending turn, an attempt or the
// answer to the architect's question, starts a new attempt while attempts
// remain, and records a completed attempt's revision. A turn that asked a
// question parks the draft instead: the amendment moves to waiting, the
// operation ends waiting, and Pass drafts it again once the answer is queued.
// The turn that delivers the answer continues the attempt that asked, so the
// wait spends no attempt and the answer adds to what the attempt delivered.
func (a *amendmentDrafter) Apply(ctx context.Context, op coreadapter.Operation) (coreadapter.OperationResult, error) {
	stream, request, in, err := a.locate(op)
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
	cfg := a.s.about(a.repository)
	for {
		if err := ctx.Err(); err != nil {
			return coreadapter.OperationResult{}, err
		}
		thread, err := a.repository.Thread(stream, architectAgent)
		if err != nil {
			return coreadapter.OperationResult{}, err
		}
		asked, err := a.repository.Questions(stream)
		if err != nil {
			return coreadapter.OperationResult{}, err
		}
		origins := amendmentChain(thread, request.ID, asked)
		chain := chainTurns(thread, origins)
		tries := attempts(chain, origins)
		// Turns run in sequence, so the oldest unfinished turn of the chain is
		// the one to drive: the answer to a question may be queued behind the
		// turn that asked it while a service stop left that turn unfinished.
		var last, pending *trace.QueuedTurn
		if len(chain) > 0 {
			last = &chain[len(chain)-1]
		}
		if i := slices.IndexFunc(chain, func(q trace.QueuedTurn) bool { return q.CompletedAt.IsZero() }); i >= 0 {
			pending = &chain[i]
		}
		switch {
		case pending != nil && pending.Claim != nil && pending.Response == nil:
			if thread.Status != "interrupted" {
				return coreadapter.OperationResult{}, errors.New("architect turn " + pending.Request.TurnID + " is still running")
			}
			if err := a.repository.AbandonTurn(ctx, stream, architectAgent, pending.Request.TurnID, a.s.now()); err != nil {
				return coreadapter.OperationResult{}, err
			}
		case pending != nil:
			if pending.Response == nil {
				if err := a.s.held(a.repository, stream); err != nil {
					return coreadapter.OperationResult{}, err
				}
			}
			if _, err := a.dispatch(ctx, stream, pending.Request.TurnID); err != nil {
				return coreadapter.OperationResult{}, err
			}
			if err := os.RemoveAll(filepath.Join(a.turnDirectory(stream, pending.Request.TurnID), "workspace")); err != nil {
				return coreadapter.OperationResult{}, err
			}
		case last != nil && askedBy(asked, architectThread, last.Request.TurnID) != "":
			id := askedBy(asked, architectThread, last.Request.TurnID)
			return a.end(ctx, op.ID, stream, request.ID, in.parkedID(), amendmentWaiting, coreadapter.OperationResult{Outcome: questions.Waiting, Evidence: fmt.Sprintf("amendment %s is parked: the architect waits for the answer to question %s", request.ID, id)}, nil)
		case last == nil || last.Status() == "interrupted":
			// A turn a hard pause stopped is continued within its attempt.
			if !stoppedTurn(last) && len(tries) >= maxDraftAttempts {
				return a.finish(ctx, op.ID, stream, request.ID, "invalid", "the architect turn was interrupted too many times", nil)
			}
			if err := a.s.held(a.repository, stream); err != nil {
				return coreadapter.OperationResult{}, err
			}
			if stoppedTurn(last) {
				if _, err := a.s.continueStopped(ctx, a.repository, cfg, architectRole, *last); err != nil {
					return coreadapter.OperationResult{}, err
				}
				continue
			}
			if err := a.enqueue(ctx, cfg, stream, request.ID, len(tries)+1, op.ID, received(thread, origins, asked)); err != nil {
				return coreadapter.OperationResult{}, err
			}
		case last.Status() == "idle":
			return a.record(ctx, op.ID, stream, request, origins[last.Request.TurnID])
		default:
			return a.finish(ctx, op.ID, stream, request.ID, "invalid", "architect turn failed: "+last.Response.Failure, nil)
		}
	}
}

// enqueue accepts the architect turn of one attempt to draft amendment id. An
// attempt after one a service stop interrupted repeats the answers the
// architect received in the earlier attempts.
func (a *amendmentDrafter) enqueue(ctx context.Context, cfg *config.Config, stream config.WorkstreamID, id string, attempt int, operation string, answers []string) error {
	profile, _, err := a.s.roleExecution(cfg, architectRole)
	if err != nil {
		return err
	}
	turn := fmt.Sprintf("amend-%s-%d", id, attempt)
	prompt := fmt.Sprintf("Draft amendment %s. Read request.json, draft/spec.md, draft/plan.json, charter.md and context.md. Deliver only the changed spec.md and/or plan.json with %s; unchanged documents will be carried forward. To decline, deliver decline.txt with your reason. For engineering revisions preserve spec.md byte-for-byte: update only the execution plan within approved intent. Keep assignments outcome-focused with explicit neighboring ownership. Constraints hold only the owner's hard limits; put file-level pointers in guidance. Footprints schedule work and need no revision for a mason to change code outside them. Incorporate necessary discoveries by assigning them to an existing unit or adding a coherent unit; preserve started identities and keep merged units identical. Do not edit code. Every dependency must name an existing unit, the graph must be acyclic, and every footprint must resolve against the entity map.", id, DraftTool) + answered("amendment", answers)
	system := fmt.Sprintf("You are the architect of the %s project (%s). Draft a revision to the sealed spec and plan for the amendment request. Read only your scoped files, deliver through %s, and do not edit implementation code or use version control.", cfg.Project.Name, cfg.Project.Upstream, DraftTool)
	req := trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_" + turn, Revision: 1, Project: a.repository.Project(), Workstream: stream, At: a.s.now(), Actor: draftingActor, Cause: operation, Depth: 1}, AgentID: architectAgent, ThreadID: architectThread, TurnID: turn, Profile: profile, SystemPrompt: system, Prompt: prompt}
	_, err = a.repository.EnqueueTurn(ctx, req)
	return err
}

type amendmentAffected struct {
	Criteria []string `json:"criteria"`
	Units    []string `json:"units"`
}

func affectedRevision(oldSpec, newSpec string, oldPlan, newPlan plan.Plan) amendmentAffected {
	a := amendmentAffected{Criteria: []string{}, Units: []string{}}
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
	for id := range unitIDs {
		old, was := oldUnits[id]
		now, is := newUnits[id]
		touched := !was || !is || !reflect.DeepEqual(old, now)
		for _, u := range []plan.Unit{old, now} {
			for _, c := range u.Criteria {
				if changed[c] {
					touched = true
				}
			}
		}
		if touched {
			a.Units = append(a.Units, id)
		}
	}
	slices.Sort(a.Criteria)
	slices.Sort(a.Units)
	return a
}

// amendmentRevision is what an amendment attempt delivered: a decline with
// its reason, or the revised spec and plan, the delivered documents in place
// of the sealed ones, with every problem that makes the revision invalid.
type amendmentRevision struct {
	declined      bool
	reason        string
	oldSpec, spec string
	graph         string
	before, after plan.Plan
	problems      []string
}

// revision reads and validates what the amendment attempt turn delivered.
func (a *amendmentDrafter) revision(stream config.WorkstreamID, turn string) (amendmentRevision, error) {
	latest, err := a.latest(stream)
	if err != nil {
		return amendmentRevision{}, err
	}
	oldSpec, oldPlan := latest[plan.SpecDocument].Content, latest[plan.PlanDocument].Content
	if oldSpec == "" || oldPlan == "" {
		return amendmentRevision{}, errors.New("sealed spec or plan is missing")
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
	readFailure := func(err error) (amendmentRevision, error) {
		if errors.Is(err, errAmendmentText) {
			return amendmentRevision{problems: []string{err.Error()}}, nil
		}
		return amendmentRevision{}, err
	}
	if reason, ok, err := read("decline.txt"); err != nil {
		return readFailure(err)
	} else if ok {
		if strings.TrimSpace(reason) == "" {
			reason = "the architect gave no reason"
		}
		return amendmentRevision{declined: true, reason: reason}, nil
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
		return amendmentRevision{problems: []string{"no revised document was delivered"}}, nil
	}
	problems, err := a.validate(trace.Document{Content: spec}, trace.Document{Content: graph})
	if err != nil {
		return amendmentRevision{}, err
	}
	before, oldErr := plan.Parse([]byte(oldPlan))
	after, newErr := plan.Parse([]byte(graph))
	if oldErr != nil {
		return amendmentRevision{}, oldErr
	}
	if newErr == nil {
		for _, old := range before.Units {
			state, err := a.repository.Workflow(stream, trace.UnitSubject(old.ID))
			if err != nil {
				return amendmentRevision{}, err
			}
			if state.Value == "merged" {
				now, ok := after.Unit(old.ID)
				if !ok || !reflect.DeepEqual(old, now) {
					problems = append(problems, fmt.Sprintf("merged unit %s was rewritten", old.ID))
				}
			}
		}
	}
	if len(problems) == 0 && spec == oldSpec && graph == oldPlan {
		problems = []string{"revision makes no change"}
	}
	return amendmentRevision{oldSpec: oldSpec, spec: spec, graph: graph, before: before, after: after, problems: problems}, nil
}

// judge is the draftJudge of an amendment draft.
func (a *amendmentDrafter) judge(stream config.WorkstreamID, attempt string) (string, []string, error) {
	r, err := a.revision(stream, attempt)
	switch {
	case err != nil:
		return "", nil, err
	case r.declined:
		return draftDeclined, nil, nil
	case len(r.problems) > 0:
		return draftInvalid, r.problems, nil
	}
	return draftValid, nil, nil
}

func (a *amendmentDrafter) record(ctx context.Context, operation string, stream config.WorkstreamID, request trace.Amendment, turn string) (coreadapter.OperationResult, error) {
	r, err := a.revision(stream, turn)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	if r.declined {
		return a.finish(ctx, operation, stream, request.ID, "declined", r.reason, nil)
	}
	if len(r.problems) > 0 {
		return a.finish(ctx, operation, stream, request.ID, "invalid", strings.Join(r.problems, "; "), nil)
	}
	spec, graph := r.spec, r.graph
	affected := affectedRevision(r.oldSpec, spec, r.before, r.after)
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

// finish records the draft's terminal outcome, proposed, declined or invalid,
// with the candidate documents of a proposal.
func (a *amendmentDrafter) finish(ctx context.Context, operation string, stream config.WorkstreamID, id, outcome, reason string, docs []trace.Document) (coreadapter.OperationResult, error) {
	result := coreadapter.OperationResult{Outcome: "succeeded", Evidence: reason}
	if outcome == "invalid" {
		result.Outcome = "failed"
	}
	return a.end(ctx, operation, stream, id, "amendment-"+id+"-"+outcome, outcome, result, docs)
}

// end moves amendment id from drafting to the given state with the given
// transition, and the documents when there are any, and returns result. An
// amendment no longer drafting returns what the operation recorded.
func (a *amendmentDrafter) end(ctx context.Context, operation string, stream config.WorkstreamID, id, transition, to string, result coreadapter.OperationResult, docs []trace.Document) (coreadapter.OperationResult, error) {
	state, err := a.repository.Workflow(stream, amendmentSubject(id))
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	if state.Value != "drafting" {
		recorded, err := a.outcome(stream, id, operation)
		if err != nil {
			return coreadapter.OperationResult{}, err
		}
		if recorded != nil {
			return *recorded, nil
		}
		return coreadapter.OperationResult{}, trace.ErrConflict
	}
	tx := trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: a.header(transition, stream, operation, a.s.now()), Subject: amendmentSubject(id), From: "drafting", To: to, Reason: result.Evidence}}
	if len(docs) > 0 {
		_, err = a.repository.RecordDocumentsWith(ctx, docs, tx)
	} else {
		_, err = a.repository.Transact(ctx, tx)
	}
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	return result, nil
}
