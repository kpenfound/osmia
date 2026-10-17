package trace

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
)

// AmendmentRequest contains the checked citation and seal identity supplied
// by the tool. The trace checks lifecycle and turn identity under its lock.
// Upstream names the drift rebase whose upstream change the requesting turn
// read, when it read one.
type AmendmentRequest struct {
	QuestionID   string
	Citations    []string
	Change       string
	Reason       string
	Seal         int
	SealRevision int
	SpecHash     string
	Upstream     *UpstreamMove
}

// FileBudgetAmendment records one service budget request and its filed state
// together. It leaves every unit state untouched.
func (r *Repository) FileBudgetAmendment(ctx context.Context, stream config.WorkstreamID, req AmendmentRequest, at time.Time) (Amendment, error) {
	r, err := r.scoped(stream)
	if err != nil {
		return Amendment{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	log, v, err := r.loadWorkflow(stream)
	if err != nil {
		return Amendment{}, err
	}
	records, _, err := r.scan()
	if err != nil {
		return Amendment{}, err
	}
	n := 1
	for _, record := range records {
		a, ok := record.(Amendment)
		if !ok || a.Workstream != stream {
			continue
		}
		if a.Role == "service" && a.Cause == "budget-per-unit" {
			return a, nil
		}
		if i, err := strconv.Atoi(a.ID); err == nil && i >= n {
			n = i + 1
		}
	}
	if state := v.states[FeatureSubject].Value; state != "building" && state != "assembled" {
		return Amendment{}, refused("budget amendments require a building or assembled workstream")
	}
	if len(req.Citations) == 0 || !present(req.Change) || !present(req.Reason) || req.Seal < 1 || req.SealRevision < 1 || !present(req.SpecHash) {
		return Amendment{}, refused("citations, proposed change, reason and sealed revision are required")
	}
	var latestSeal Document
	for _, record := range records {
		if d, ok := record.(Document); ok && d.Workstream == stream && d.Path == "seal.json" && d.Revision >= latestSeal.Revision {
			latestSeal = d
		}
	}
	if latestSeal.ID == "" || latestSeal.Revision != req.SealRevision {
		return Amendment{}, fmt.Errorf("%w: the sealed revision changed", ErrConflict)
	}
	id := strconv.Itoa(n)
	actor := Actor{Kind: "service", ID: "budget"}
	h := Header{Schema: "osmia.trace.amendment", Version: Version, ID: id, Revision: 1, Project: r.project, Workstream: stream, At: at, Actor: actor, Cause: "budget-per-unit", Depth: 1}
	a := Amendment{Header: h, Requester: actor, Role: "service", Thread: "budget", Turn: "budget", Citations: req.Citations, Change: req.Change, Reason: req.Reason, Seal: req.Seal, SealRevision: req.SealRevision, SpecHash: req.SpecHash}
	if err := validate(a); err != nil {
		return Amendment{}, err
	}
	transitionID := "amendment_" + id + "_filed"
	tx := Transaction{ExpectedVersion: 0, Transition: Transition{Header: Header{Schema: "osmia.trace.transition", Version: Version, ID: transitionID, Revision: 1, Project: r.project, Workstream: stream, At: at, Actor: actor, Cause: h.Cause, Depth: 2}, Subject: "amendment_" + id, To: "filed", Reason: "per-unit budget exceeded"}, Events: []Event{Notice(transitionID, "amendment", fmt.Sprintf("Budget amendment %s filed for workstream %s: %s", id, stream, req.Reason))}}
	files, _, err := r.stage(ctx, stream, log, v, []Record{a}, tx)
	if err != nil {
		return Amendment{}, err
	}
	if err := r.publish(ctx, files); err != nil {
		return Amendment{}, err
	}
	_ = r.wake.Notify(context.Background())
	return a, nil
}

// AmendmentOutcome is what filing an amendment produced: ordinarily a
// per-workstream Amendment, or, when the request would change a contract
// section read-only to the requesting workstream, a ProjectAmendment
// recorded at the project level instead, debated as a delta against the
// ratified project spec and never applied inside the workstream (spec#10).
// ProjectLevel reports which; exactly one of Amendment.ID and
// ProjectAmendment.ID is set.
type AmendmentOutcome struct {
	Amendment        Amendment
	ProjectAmendment ProjectAmendment
	ProjectLevel     bool
}

// contractTouch returns stream's own workgroup's recorded project spec, its
// workgroup ID, and which of citations names one of stream's own
// contract-criteria numbers in it: read-only inside stream. project must be
// the project's own repository.
func contractTouch(project *Repository, stream config.WorkstreamID, citations []string) (ProjectSpec, config.WorkgroupID, []string, error) {
	spec, workgroup, found, err := project.ProjectSpecOf(stream)
	if err != nil || !found {
		return ProjectSpec{}, workgroup, nil, err
	}
	return touchedCitations(spec, workgroup, stream, citations)
}

// contractTouchLocked is contractTouch's variant for a caller that already
// holds member's own scoped repository locked: see projectSpecOfLocked.
func contractTouchLocked(project *Repository, v *workflowView, member config.WorkstreamID, citations []string, records []Record) (ProjectSpec, config.WorkgroupID, []string, error) {
	spec, workgroup, found, err := projectSpecOfLocked(project, v, member, records)
	if err != nil || !found {
		return ProjectSpec{}, workgroup, nil, err
	}
	return touchedCitations(spec, workgroup, member, citations)
}

func touchedCitations(spec ProjectSpec, workgroup config.WorkgroupID, stream config.WorkstreamID, citations []string) (ProjectSpec, config.WorkgroupID, []string, error) {
	var touched []string
	for _, c := range citations {
		if ContractCitation(spec, stream, c) {
			touched = append(touched, c)
		}
	}
	return spec, workgroup, touched, nil
}

// FileAmendment atomically records a request, its notice and the requester's
// waiting transition. Repeated calls from the same turn return the same ID.
// A request that cites a drift rebase also raises an upstream moved event in
// the same commit; another request of the same agent and thread citing the
// same drift rebase returns the one already filed, so a turn that recovers
// an interrupted one files nothing twice. A mason turn with no unit, which
// resolves the feature branch's conflicts with upstream, parks nothing.
//
// A chief-of-staff tool naming a question (route_amendment) resolves it
// through the same member-workstream routing every other chief-of-staff
// question tool uses: the amendment is recorded against the asking
// member's own workstream, never against the chief's host workstream by
// default. When the request would change a contract section read-only to
// that member's own slice, the amendment is never recorded against the
// workstream at all: it becomes a ProjectAmendment instead, and the
// question is routed to it the same way. A repeated chief call naming the
// same already-routed question returns that same ProjectAmendment, rather
// than the refusal a second routing of an ordinary question would get.
func (r *Repository) FileAmendment(ctx context.Context, agent string, scope coreadapter.Scope, req AmendmentRequest, at time.Time) (AmendmentOutcome, error) {
	project := r
	if scope.Role != "mason" && scope.Role != "reviewer" && scope.Role != ChiefOfStaff {
		return AmendmentOutcome{}, fmt.Errorf("only masons, reviewers and the chief of staff may file amendments")
	}
	host := config.WorkstreamID(scope.Workstream)
	if scope.Role != ChiefOfStaff {
		if req.QuestionID != "" {
			return AmendmentOutcome{}, refused("only the chief of staff may route a question")
		}
		_, _, touched, err := contractTouch(project, host, req.Citations)
		if err != nil {
			return AmendmentOutcome{}, err
		}
		if len(touched) > 0 {
			return AmendmentOutcome{}, refused("%s names a contract section, read-only inside a workstream; ask the chief of staff to raise it as a project-level amendment", strings.Join(touched, ", "))
		}
	}

	var (
		target  *Repository
		member  config.WorkstreamID
		turn    QueuedTurn
		log     workflowLog
		v       *workflowView
		records []Record
		err     error
	)
	if scope.Role == ChiefOfStaff && req.QuestionID != "" {
		var localID string
		target, localID, turn, err = r.chiefQuestionScope(ctx, agent, scope, req.QuestionID, at)
		if err != nil {
			return AmendmentOutcome{}, err
		}
		req.QuestionID = localID
		member = target.stream
		log, v, err = target.loadWorkflow(member)
		if err != nil {
			target.mu.Unlock()
			return AmendmentOutcome{}, err
		}
		records, _, err = target.scan()
		if err != nil {
			target.mu.Unlock()
			return AmendmentOutcome{}, err
		}
	} else {
		member = host
		target, err = r.scoped(member)
		if err != nil {
			return AmendmentOutcome{}, err
		}
		target.mu.Lock()
		turn, log, v, records, err = target.questionTurn(ctx, agent, scope, at)
		if err != nil {
			target.mu.Unlock()
			return AmendmentOutcome{}, err
		}
	}
	defer target.mu.Unlock()

	for _, record := range records {
		if a, ok := record.(Amendment); ok && a.Workstream == member && a.Actor.ID == agent && a.Thread == scope.Thread && a.Turn == scope.Turn {
			return AmendmentOutcome{Amendment: a}, nil
		}
	}
	// A question already routed to a ProjectAmendment leaves no Amendment
	// record for the loop above to match, since it is never recorded
	// against the workstream at all: a repeated chief call for the same
	// question is still idempotent, checked here before openQuestion below
	// would otherwise refuse it as already routed.
	if scope.Role == ChiefOfStaff && req.QuestionID != "" {
		pas, _, err := ProjectAmendments(project)
		if err != nil {
			return AmendmentOutcome{}, err
		}
		for _, pa := range pas {
			if pa.Origin == member && pa.QuestionID == req.QuestionID {
				return AmendmentOutcome{ProjectAmendment: pa, ProjectLevel: true}, nil
			}
		}
	}
	if req.Upstream != nil {
		if scope.Role == ChiefOfStaff || req.Upstream.Drift < 1 || !present(req.Upstream.From) || !present(req.Upstream.To) {
			return AmendmentOutcome{}, fmt.Errorf("an amendment cites a drift rebase by its number and upstream commits, from a mason or reviewer turn")
		}
		for _, record := range records {
			if a, ok := record.(Amendment); ok && a.Workstream == member && a.Actor.ID == agent && a.Thread == scope.Thread && a.Upstream != nil && a.Upstream.Drift == req.Upstream.Drift {
				return AmendmentOutcome{Amendment: a}, nil
			}
		}
	}
	if scope.Role != ChiefOfStaff {
		for _, q := range questions(records, v, member) {
			if q.Asked.AskedBy.ID == agent && q.Asked.Thread == scope.Thread && q.Asked.Turn == scope.Turn {
				return AmendmentOutcome{}, refused("this turn already asked question %s; end the turn", q.Asked.ID)
			}
		}
	}
	if state := v.states[FeatureSubject].Value; state != "building" && state != "assembled" {
		return AmendmentOutcome{}, refused("amendments require a building or assembled workstream")
	}
	if len(req.Citations) == 0 || !present(req.Change) || !present(req.Reason) || req.Seal < 1 || req.SealRevision < 1 || !present(req.SpecHash) {
		return AmendmentOutcome{}, refused("citations, proposed change, reason and sealed revision are required")
	}
	var latestSeal Document
	for _, record := range records {
		if d, ok := record.(Document); ok && d.Workstream == member && d.Path == "seal.json" && d.Revision >= latestSeal.Revision {
			latestSeal = d
		}
	}
	if latestSeal.ID == "" || latestSeal.Revision != req.SealRevision {
		return AmendmentOutcome{}, refused("the sealed revision changed; retry against the current spec and plan")
	}
	requester := Actor{Kind: "agent", ID: agent}
	unit := scope.Unit
	parkRole := scope.Role
	if scope.Role == ChiefOfStaff && req.QuestionID != "" {
		q, err := openQuestion(questions(records, v, member), req.QuestionID)
		if err != nil {
			return AmendmentOutcome{}, err
		}
		requester, unit = q.Asked.AskedBy, q.Asked.Unit
		for _, record := range records {
			if identity, ok := record.(Agent); ok && identity.Workstream == member && identity.ID == requester.ID {
				parkRole = identity.Role
			}
		}
	}
	var from string
	if (parkRole == "mason" || parkRole == "reviewer") && !(scope.Role != ChiefOfStaff && unit == "" && req.Upstream != nil) {
		stage := map[string]string{"mason": "implementing", "reviewer": "reviewing"}[parkRole]
		from = v.states[UnitSubject(unit)].Value
		if unit == "" || from != stage && !(scope.Role == ChiefOfStaff && from == "waiting") {
			return AmendmentOutcome{}, refused("the requesting unit must be %s or waiting on its question", stage)
		}
	}

	// target.mu is already held (deferred above): v is already loaded for
	// member under that lock, so this must use the locked variant, never
	// contractTouch itself, which would re-lock the same instance through
	// ProjectSpecOf -> CrossRepoWorkgroupOf -> Workflow -> scoped(member).
	_, workgroup, touched, err := contractTouchLocked(project, v, member, req.Citations, records)
	if err != nil {
		return AmendmentOutcome{}, err
	}
	if len(touched) > 0 {
		pa := ProjectAmendment{Workgroup: workgroup, Origin: member, Role: scope.Role, Requester: requester, QuestionID: req.QuestionID, Citations: req.Citations, Change: req.Change, Reason: req.Reason}
		filed, err := FileProjectAmendment(ctx, project, pa, at)
		if err != nil {
			return AmendmentOutcome{}, err
		}
		var txs []Transaction
		if req.QuestionID != "" {
			// These two transitions land on member's own trace, through
			// target.stage below, so they are stamped with target's own
			// project identity. For a workgroup confined to one repo that
			// is the same value as project.Project(); for a cross-repo
			// workgroup it is member's own real project, never the
			// workgroup trace's own synthetic one.
			h := Header{Version: Version, Project: target.project, Workstream: member, At: at, Actor: Actor{Kind: "agent", ID: agent}, Cause: turn.Request.ID, Depth: turn.Request.Depth + 1}
			txs = append(txs, questionTransition(v, h, req.QuestionID, QuestionOpen, QuestionRouted, fmt.Sprintf("The chief of staff routed question %s to project-level amendment %s of workgroup %s", req.QuestionID, filed.ID, workgroup)))
		}
		if from != "" {
			subject := UnitSubject(unit)
			stage := map[string]string{"mason": "implementing", "reviewer": "reviewing"}[parkRole]
			reason := fmt.Sprintf("unit %s waits for project-level amendment %s of workgroup %s, citing %s, read-only inside the workstream; its %s stage and candidate are preserved", unit, filed.ID, workgroup, strings.Join(touched, ", "), stage)
			wait := Transition{Header: Header{Schema: "osmia.trace.transition", Version: Version, ID: subject + "_waiting_project_amendment_" + string(workgroup) + "_" + filed.ID, Revision: 1, Project: target.project, Workstream: member, Unit: unit, At: at, Actor: Actor{Kind: "service", ID: parkRole}, Cause: "project-amendment-" + filed.ID, Depth: 2}, Subject: subject, From: from, To: "waiting", Reason: reason}
			txs = append(txs, Transaction{ExpectedVersion: v.states[subject].Version, Transition: wait})
		}
		if len(txs) > 0 {
			files, _, err := target.stage(ctx, member, log, v, nil, txs...)
			if err != nil {
				return AmendmentOutcome{}, err
			}
			if err := target.publish(ctx, files); err != nil {
				return AmendmentOutcome{}, err
			}
			_ = target.wake.Notify(context.Background())
		}
		return AmendmentOutcome{ProjectAmendment: filed, ProjectLevel: true}, nil
	}

	n := 1
	for _, record := range records {
		if a, ok := record.(Amendment); ok && a.Workstream == member {
			if i, err := strconv.Atoi(a.ID); err == nil && i >= n {
				n = i + 1
			}
		}
	}
	id := strconv.Itoa(n)
	h := Header{Schema: "osmia.trace.amendment", Version: Version, ID: id, Revision: 1, Project: target.project, Workstream: member, Unit: unit, At: at, Actor: Actor{Kind: "agent", ID: agent}, Cause: turn.Request.ID, Depth: turn.Request.Depth + 1}
	a := Amendment{Header: h, Requester: requester, Role: scope.Role, Thread: scope.Thread, Turn: scope.Turn, QuestionID: req.QuestionID, Citations: req.Citations, Change: req.Change, Reason: req.Reason, Seal: req.Seal, SealRevision: req.SealRevision, SpecHash: req.SpecHash, Upstream: req.Upstream}
	if err := validate(a); err != nil {
		return AmendmentOutcome{}, err
	}
	var txs []Transaction
	if req.QuestionID != "" {
		q := questionTransition(v, h, req.QuestionID, QuestionOpen, QuestionRouted, fmt.Sprintf("The chief of staff routed question %s to amendment %s", req.QuestionID, id))
		txs = append(txs, q)
	}
	transitionID := "amendment_" + id + "_filed"
	transition := Transition{Header: Header{Schema: "osmia.trace.transition", Version: Version, ID: transitionID, Revision: 1, Project: target.project, Workstream: member, Unit: unit, At: at, Actor: h.Actor, Cause: h.Cause, Depth: h.Depth}, Subject: "amendment_" + id, From: "", To: "filed", Reason: fmt.Sprintf("%s filed amendment %s", scope.Role, id)}
	cited := strings.Join(req.Citations, ", ")
	events := []Event{Notice(transitionID, "amendment", fmt.Sprintf("Amendment %s was filed by the %s for %s: %s", id, scope.Role, cited, strings.TrimSpace(req.Reason)))}
	if m := req.Upstream; m != nil {
		events[0].Body = fmt.Sprintf("Amendment %s was filed by the %s for %s, citing upstream commit %s: %s", id, scope.Role, cited, m.To, strings.TrimSpace(req.Reason))
		events = append(events, UpstreamMoved(transitionID, member, *m, fmt.Sprintf("amendment %s was filed by the %s: upstream commit %s changes what %s means", id, scope.Role, m.To, cited)))
	}
	txs = append(txs, Transaction{ExpectedVersion: 0, Transition: transition, Events: events})
	if from != "" {
		subject := UnitSubject(unit)
		stage := map[string]string{"mason": "implementing", "reviewer": "reviewing"}[parkRole]
		wait := Transition{Header: Header{Schema: "osmia.trace.transition", Version: Version, ID: subject + "_waiting_amendment_" + id, Revision: 1, Project: target.project, Workstream: member, Unit: unit, At: at, Actor: Actor{Kind: "service", ID: parkRole}, Cause: transitionID, Depth: h.Depth + 1}, Subject: subject, From: from, To: "waiting", Reason: fmt.Sprintf("unit %s waits for amendment %s; its %s stage and candidate are preserved", unit, id, stage)}
		txs = append(txs, Transaction{ExpectedVersion: v.states[subject].Version, Transition: wait})
	}
	files, _, err := target.stage(ctx, member, log, v, []Record{a}, txs...)
	if err != nil {
		return AmendmentOutcome{}, err
	}
	if err := target.publish(ctx, files); err != nil {
		return AmendmentOutcome{}, err
	}
	_ = target.wake.Notify(context.Background())
	return AmendmentOutcome{Amendment: a}, nil
}

// OwnerTurn returns the request of the scope's active, uncaptured
// chief-of-staff turn and reports whether that turn answers a message from
// the owner.
func (r *Repository) OwnerTurn(agent string, scope coreadapter.Scope) (TurnRequest, bool, error) {
	r, err := r.scoped(config.WorkstreamID(scope.Workstream))
	if err != nil {
		return TurnRequest{}, false, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if scope.Role != ChiefOfStaff {
		return TurnRequest{}, false, fmt.Errorf("only a chief-of-staff turn answers the owner")
	}
	_, q, err := r.turnScope(agent, scope, true)
	if err != nil {
		return TurnRequest{}, false, err
	}
	return q.Request, q.Request.Actor.Kind == "owner", nil
}
