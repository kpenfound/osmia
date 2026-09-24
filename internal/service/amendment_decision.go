package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/kpenfound/osmia/internal/amendment"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/envelope"
	"github.com/kpenfound/osmia/internal/kb"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
)

// The owner's decisions on a presented amendment.
const (
	AmendmentApprove  = "approve"
	AmendmentReject   = "reject"
	AmendmentRound    = "round"
	AmendmentOverrule = "overrule"
)

// Amendment subject states after the owner's decision.
const (
	amendmentPresented = "presented"
	amendmentApproved  = "approved"
	amendmentRejected  = "rejected"
	amendmentResealed  = "resealed"
	amendmentUnapplied = "unapplied"
	amendmentRuled     = "ruled"
)

var amendmentActor = trace.Actor{Kind: "service", ID: "amendments"}

func amendmentDecisionPath(id string) string { return "amendments/" + id + "/decision.json" }
func amendmentDecisionID(id string) string   { return "amendment_" + id + "_decision" }

// amendmentDecided is the ID of the transition that records the owner's k-th
// decision on an amendment.
func amendmentDecided(id string, k int) string { return fmt.Sprintf("amendment-%s-decided-%d", id, k) }

// amendmentRulingTurn is the turn that delivers the owner's ruling on an
// amendment to its requester's thread.
func amendmentRulingTurn(id string) string { return "amendment_" + id + "_ruling" }

// AmendmentDecision is one owner decision on a presented amendment, recorded
// as one revision of amendments/<n>/decision.json. Round is the debate round
// the decided packet followed, Packet the revision of packet.json the owner
// read, Spec and Plan the revisions of the proposed amendments/<n>/spec.md
// and plan.json, and SealRevision the revision of seal.json in force.
// Overruled names the objections an overrule set aside.
type AmendmentDecision struct {
	Decision     string   `json:"decision"`
	Note         string   `json:"note,omitempty"`
	Round        int      `json:"round"`
	Packet       int      `json:"packet"`
	Spec         int      `json:"spec"`
	Plan         int      `json:"plan"`
	SealRevision int      `json:"seal_revision"`
	Overruled    []string `json:"overruled,omitempty"`
}

// AmendmentDecisionRequest is the owner's decision on the named revision of
// an amendment's packet.
type AmendmentDecisionRequest struct {
	Decision string `json:"decision"`
	Note     string `json:"note,omitempty"`
	Packet   int    `json:"packet"`
}

// AmendmentResponse describes one amendment: its state, the debate round it
// reached, its latest packet and revision, and the owner's latest decision.
type AmendmentResponse struct {
	Project    config.ProjectID    `json:"project"`
	Workstream config.WorkstreamID `json:"workstream"`
	Amendment  string              `json:"amendment"`
	State      string              `json:"state"`
	Round      int                 `json:"round"`
	Packet     json.RawMessage     `json:"packet,omitempty"`
	Revision   int                 `json:"revision,omitempty"`
	Decision   *AmendmentDecision  `json:"decision,omitempty"`
	Detail     string              `json:"detail,omitempty"`
}

// amendmentDecisions returns the owner's decisions on an amendment, oldest
// first.
func amendmentDecisions(repository *trace.Repository, stream config.WorkstreamID, id string) ([]AmendmentDecision, error) {
	docs, err := trace.Read[trace.Document](repository, stream)
	if err != nil {
		return nil, err
	}
	var out []AmendmentDecision
	for _, d := range docs {
		if d.Path != amendmentDecisionPath(id) {
			continue
		}
		var decision AmendmentDecision
		if err := json.Unmarshal([]byte(d.Content), &decision); err != nil {
			return nil, fmt.Errorf("%s revision %d: %w", d.Path, d.Revision, err)
		}
		out = append(out, decision)
	}
	return out, nil
}

// amendmentRound returns the debate round an amendment is in: the first, and
// one more for every round the owner asked for.
func amendmentRound(repository *trace.Repository, stream config.WorkstreamID, id string) (int, error) {
	decisions, err := amendmentDecisions(repository, stream, id)
	if err != nil {
		return 0, err
	}
	round := 1
	for _, d := range decisions {
		if d.Decision == AmendmentRound {
			round++
		}
	}
	return round, nil
}

// amendmentCase is what the owner's decision on one amendment reads.
type amendmentCase struct {
	request   trace.Amendment
	state     trace.WorkflowState
	round     int
	packet    trace.Document
	spec      trace.Document
	plan      trace.Document
	decisions []AmendmentDecision
}

// readAmendment reads one amendment of the workstream and reports whether the
// workstream filed it.
func readAmendment(repository *trace.Repository, stream config.WorkstreamID, id string) (amendmentCase, bool, error) {
	requests, err := trace.Read[trace.Amendment](repository, stream)
	if err != nil {
		return amendmentCase{}, false, err
	}
	i := slices.IndexFunc(requests, func(a trace.Amendment) bool { return a.ID == id })
	if i < 0 {
		return amendmentCase{}, false, nil
	}
	c := amendmentCase{request: requests[i]}
	if c.state, err = repository.Workflow(stream, amendmentSubject(id)); err != nil {
		return amendmentCase{}, false, err
	}
	if c.decisions, err = amendmentDecisions(repository, stream, id); err != nil {
		return amendmentCase{}, false, err
	}
	if c.round, err = amendmentRound(repository, stream, id); err != nil {
		return amendmentCase{}, false, err
	}
	docs, err := trace.Read[trace.Document](repository, stream)
	if err != nil {
		return amendmentCase{}, false, err
	}
	for _, d := range docs {
		switch {
		case d.Path == amendmentPacketPath(id) && d.Revision > c.packet.Revision:
			c.packet = d
		case d.Path == "amendments/"+id+"/"+plan.SpecPath && d.Revision > c.spec.Revision:
			c.spec = d
		case d.Path == "amendments/"+id+"/"+plan.PlanPath && d.Revision > c.plan.Revision:
			c.plan = d
		}
	}
	return c, true, nil
}

func (c amendmentCase) response(project config.ProjectID, stream config.WorkstreamID, detail string) AmendmentResponse {
	out := AmendmentResponse{Project: project, Workstream: stream, Amendment: c.request.ID, State: c.state.Value, Round: c.round, Revision: c.packet.Revision, Detail: detail}
	if c.packet.Revision > 0 {
		out.Packet = json.RawMessage(c.packet.Content)
	}
	if len(c.decisions) > 0 {
		out.Decision = &c.decisions[len(c.decisions)-1]
	}
	return out
}

// amendmentView serves one amendment of a workstream.
func (s *Service) amendmentView(raw, id string) (AmendmentResponse, *APIError) {
	project, stream, repository, api := s.conversationTrace(raw)
	if api != nil {
		return AmendmentResponse{}, api
	}
	c, found, err := readAmendment(repository, stream, id)
	if err != nil {
		return AmendmentResponse{}, &APIError{Internal, fmt.Sprintf("cannot read amendment %s of workstream %s; check the trace repository", id, stream)}
	}
	if !found {
		return AmendmentResponse{}, &APIError{NotFound, fmt.Sprintf("workstream %s has no amendment %s", stream, id)}
	}
	return c.response(project, stream, ""), nil
}

// decideAmendment records the owner's decision on the named packet revision
// of a presented amendment.
func (s *Service) decideAmendment(ctx context.Context, raw, id string, req AmendmentDecisionRequest) (AmendmentResponse, *APIError) {
	project, stream, repository, api := s.conversationTrace(raw)
	if api != nil {
		return AmendmentResponse{}, api
	}
	return s.recordAmendmentDecision(ctx, project, stream, repository, id, req, ownerActor, "owner-amendment")
}

// recordAmendmentDecision records one decision on an amendment, as the
// actor, and moves the amendment to what the decision asks for: approved,
// rejected, or proposed again for another debate round. A decision on a
// packet revision the owner already decided returns that decision when it
// is the same and is refused when it differs, so a retry never decides
// twice. Approval is refused while an objection blocks it, and approval or
// overrule once the sealed documents moved since the request was filed.
func (s *Service) recordAmendmentDecision(ctx context.Context, project config.ProjectID, stream config.WorkstreamID, repository *trace.Repository, id string, req AmendmentDecisionRequest, actor trace.Actor, cause string) (AmendmentResponse, *APIError) {
	failed := &APIError{Internal, fmt.Sprintf("cannot record the decision on amendment %s of workstream %s; check the trace repository", id, stream)}
	decision := strings.TrimSpace(req.Decision)
	if !slices.Contains([]string{AmendmentApprove, AmendmentReject, AmendmentRound, AmendmentOverrule}, decision) {
		return AmendmentResponse{}, &APIError{Validation, "an amendment decision is approve, reject, round or overrule"}
	}
	if req.Packet < 1 {
		return AmendmentResponse{}, &APIError{Validation, "name the revision of the amendment's packet the decision is on; read it with osmia amendment"}
	}
	feature, err := repository.Workflow(stream, trace.FeatureSubject)
	if err != nil {
		return AmendmentResponse{}, failed
	}
	if feature.Value != BuildingState && feature.Value != AssembledState {
		return AmendmentResponse{}, &APIError{Conflict, fmt.Sprintf("workstream %s is %s; amendments are decided while it is building or assembled", stream, featureState(feature.Value))}
	}
	c, found, err := readAmendment(repository, stream, id)
	if err != nil {
		return AmendmentResponse{}, failed
	}
	if !found {
		return AmendmentResponse{}, &APIError{NotFound, fmt.Sprintf("workstream %s has no amendment %s", stream, id)}
	}
	for _, d := range c.decisions {
		if d.Packet != req.Packet {
			continue
		}
		if d.Decision == decision {
			return c.response(project, stream, fmt.Sprintf("the owner's decision to %s amendment %s on packet revision %d is already recorded", decision, id, req.Packet)), nil
		}
		return AmendmentResponse{}, &APIError{Conflict, fmt.Sprintf("packet revision %d of amendment %s is already decided: %s", req.Packet, id, d.Decision)}
	}
	if c.state.Value != amendmentPresented || c.packet.Revision == 0 {
		return AmendmentResponse{}, &APIError{Conflict, fmt.Sprintf("amendment %s of workstream %s is %s; the owner decides it once the chief of staff presents it", id, stream, featureState(c.state.Value))}
	}
	if req.Packet != c.packet.Revision {
		return AmendmentResponse{}, &APIError{Conflict, fmt.Sprintf("packet revision %d of amendment %s is not the latest; revision %d is presented, read it with osmia amendment", req.Packet, id, c.packet.Revision)}
	}
	_, sealDoc, sealed, err := seal.Latest(repository, stream)
	if err != nil || !sealed {
		return AmendmentResponse{}, failed
	}
	records, err := amendmentRecords(repository, stream, id)
	if err != nil {
		return AmendmentResponse{}, failed
	}
	entries := amendmentDissent(records)
	record := AmendmentDecision{Decision: decision, Note: strings.TrimSpace(req.Note), Round: c.round, Packet: c.packet.Revision, Spec: c.spec.Revision, Plan: c.plan.Revision, SealRevision: sealDoc.Revision}
	var to, reason string
	switch decision {
	case AmendmentApprove, AmendmentOverrule:
		if sealDoc.Revision != c.request.SealRevision {
			return AmendmentResponse{}, &APIError{Conflict, fmt.Sprintf("the sealed documents changed since amendment %s was filed against seal.json revision %d; revision %d is in force, so reject it and file a new request", id, c.request.SealRevision, sealDoc.Revision)}
		}
		if decision == AmendmentApprove {
			if blocked := shed.Blocked(entries); len(blocked) > 0 {
				return AmendmentResponse{}, &APIError{Conflict, fmt.Sprintf("objections %s block amendment %s; overrule them or reject the amendment", entryIDs(blocked), id)}
			}
			reason = fmt.Sprintf("the owner approved amendment %s on packet revision %d after round %d", id, c.packet.Revision, c.round)
		} else {
			if len(entries) == 0 {
				return AmendmentResponse{}, &APIError{Conflict, fmt.Sprintf("no objection stands against amendment %s; approve it instead", id)}
			}
			for _, e := range entries {
				record.Overruled = append(record.Overruled, e.ID)
			}
			reason = fmt.Sprintf("the owner overruled objections %s and approved amendment %s on packet revision %d after round %d", strings.Join(record.Overruled, ", "), id, c.packet.Revision, c.round)
		}
		to = amendmentApproved
		reason += "; the spec and plan are versioned and resealed next"
	case AmendmentReject:
		to = amendmentRejected
		reason = fmt.Sprintf("the owner rejected amendment %s on packet revision %d; the sealed spec and plan stay in force", id, c.packet.Revision)
	case AmendmentRound:
		if limit := s.current().Shed.MaxRounds; c.round >= limit {
			return AmendmentResponse{}, &APIError{Conflict, fmt.Sprintf("amendment %s has had %d of shed.max_rounds (%d) debate rounds; approve, overrule or reject it", id, c.round, limit)}
		}
		to = "proposed"
		reason = fmt.Sprintf("the owner asked for debate round %d on amendment %s after reading packet revision %d", c.round+1, id, c.packet.Revision)
	}
	if record.Note != "" {
		reason += ": " + record.Note
	}
	content, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return AmendmentResponse{}, failed
	}
	k := len(c.decisions) + 1
	at := s.now()
	doc := trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: amendmentDecisionID(id), Revision: k, Project: project, Workstream: stream, At: at, Actor: actor, Cause: cause}, Path: amendmentDecisionPath(id), Content: string(content) + "\n"}
	transition := amendmentDecided(id, k)
	tx := trace.Transaction{ExpectedVersion: c.state.Version,
		Transition: trace.Transition{Header: trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: transition, Revision: 1, Project: project, Workstream: stream, Unit: c.request.Unit, At: at, Actor: actor, Cause: cause}, Subject: amendmentSubject(id), From: amendmentPresented, To: to, Reason: reason},
		Events:     []trace.Event{trace.Notice(transition, "amendment", reason)}}
	if _, err := repository.RecordDocumentsWith(ctx, []trace.Document{doc}, tx); err != nil {
		if errors.Is(err, trace.ErrConflict) {
			return AmendmentResponse{}, &APIError{Conflict, fmt.Sprintf("amendment %s changed while the decision was recorded; read it again with osmia amendment and retry", id)}
		}
		return AmendmentResponse{}, failed
	}
	c.state = trace.WorkflowState{Value: to, Version: c.state.Version + 1}
	c.decisions = append(c.decisions, record)
	if decision == AmendmentRound {
		c.round++
	}
	return c.response(project, stream, reason), nil
}

// decideAmendmentTool records the owner's decision on an amendment that the
// owner gave the chief of staff in a message.
const decideAmendmentTool = "decide_amendment"

// amendmentGuidance tells the chief of staff when and how to use
// decide_amendment. It belongs in the system prompt of every owner message
// turn.
const amendmentGuidance = "When the owner's message decides an amendment you presented, call decide_amendment with the amendment's number, the packet revision you presented, the owner's decision and the owner's own words as the note. " +
	"The decisions are approve, reject, round for one more bounded debate round, and overrule to approve over the objections that still stand. Never decide an amendment the owner has not decided in the message; the owner can also decide it with osmia amendment."

// decideAmendment returns the decide_amendment tool of one claimed
// chief-of-staff turn. It records the same decision as POST
// /amendment/<workstream>/<n> on the turn's workstream, with the owner whose
// message the turn answers as its actor. A decision the service refuses is an
// ordinary result, {"recorded":false,"reason":...}, and records nothing.
func (c *runtimeControls) decideAmendment(repository *trace.Repository, scope coreadapter.Scope) coreadapter.Tool {
	tool := coreadapter.Tool{Name: decideAmendmentTool, Effect: coreadapter.ToolMemory,
		Description: "Record the owner's decision on an amendment you presented, only when the owner decides it in a message. amendment: its number; packet: the revision of its packet the owner read; decision: approve, reject, round (one more bounded debate round) or overrule (approve over the objections that stand); note: the owner's words. " +
			"Approval versions the spec and plan and reseals; rejection keeps the sealed documents; either way the requester receives the ruling and its unit resumes.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"amendment":{"type":"string"},"packet":{"type":"integer","minimum":1},"decision":{"type":"string","enum":["approve","reject","round","overrule"]},"note":{"type":"string"}},"required":["amendment","packet","decision"],"additionalProperties":false}`)}
	tool.Handle = func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
		var input struct {
			Amendment string `json:"amendment"`
			Packet    int    `json:"packet"`
			Decision  string `json:"decision"`
			Note      string `json:"note"`
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
			return nil, errors.New("amendment decisions are unavailable")
		}
		request, owner, err := repository.OwnerTurn(trace.ChiefOfStaff, scope)
		if err != nil {
			return nil, err
		}
		if !owner {
			return priorityRefusal("only the owner decides an amendment; this turn does not answer a message from the owner")
		}
		out, api := s.recordAmendmentDecision(ctx, repository.Project(), config.WorkstreamID(scope.Workstream), repository, input.Amendment,
			AmendmentDecisionRequest{Decision: input.Decision, Note: input.Note, Packet: input.Packet}, request.Actor, request.ID)
		if api != nil {
			if api.Code == Internal {
				return nil, errors.New(api.Message)
			}
			return priorityRefusal(api.Message)
		}
		return json.Marshal(struct {
			Recorded  bool   `json:"recorded"`
			Amendment string `json:"amendment"`
			State     string `json:"state"`
			Detail    string `json:"detail"`
		}{true, out.Amendment, out.State, out.Detail})
	}
	return tool
}

func entryIDs(entries []shed.Entry) string {
	ids := make([]string, len(entries))
	for i, e := range entries {
		ids[i] = e.ID
	}
	return strings.Join(ids, ", ")
}

// reseal applies an approved amendment: it records the proposed spec and plan
// as the next revisions of spec.md and plan.json when they differ from the
// sealed ones, and the next revision of seal.json naming them. A changed spec
// takes the next seal number and its hash; a plan-only change keeps the seal
// number and hash and records the plan's footprints. The base commit, the
// feature branch and the feature state stay as they are. The first revision
// of amendments/<n>/application.json classifies the affected units, so from
// the resealing on no approval the amendment affects can land. The
// documents, the seal and the move to resealed are one commit, so a restart
// finds either all of them or none. An approval that can no longer apply moves to
// unapplied with the reason, and the sealed documents stay in force.
func (a amendmentDebate) reseal(ctx context.Context, stream config.WorkstreamID, req trace.Amendment, state trace.WorkflowState) error {
	decisions, err := amendmentDecisions(a.repository, stream, req.ID)
	if err != nil {
		return err
	}
	if len(decisions) == 0 {
		return fmt.Errorf("approved amendment %s has no decision", req.ID)
	}
	d := decisions[len(decisions)-1]
	cause := amendmentDecided(req.ID, len(decisions))
	unapplied := func(reason string) error {
		id := "amendment-" + req.ID + "-" + amendmentUnapplied
		reason = fmt.Sprintf("approved amendment %s cannot be applied and the sealed spec and plan stay in force: %s", req.ID, reason)
		tx := trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: a.amendmentHeader(id, stream, req.Unit, cause), Subject: amendmentSubject(req.ID), From: amendmentApproved, To: amendmentUnapplied, Reason: reason},
			Events: []trace.Event{trace.Notice(id, "amendment", reason)}}
		if _, err := a.repository.Transact(ctx, tx); err != nil && !errors.Is(err, trace.ErrConflict) {
			return err
		}
		return nil
	}
	current, sealDoc, found, err := seal.Latest(a.repository, stream)
	if err != nil {
		return err
	}
	if !found {
		return unapplied("the workstream has no seal")
	}
	if sealDoc.Revision != d.SealRevision {
		return unapplied(fmt.Sprintf("seal.json revision %d is in force, not revision %d the decision was on", sealDoc.Revision, d.SealRevision))
	}
	docs, err := trace.Read[trace.Document](a.repository, stream)
	if err != nil {
		return err
	}
	var sealedSpec, sealedPlan, draftSpec, draftPlan, affectedDoc trace.Document
	latestSpec, latestPlan := 0, 0
	for _, doc := range docs {
		switch {
		case doc.ID == plan.SpecDocument:
			latestSpec = max(latestSpec, doc.Revision)
			if doc.Revision == current.Revision.Spec {
				sealedSpec = doc
			}
		case doc.ID == plan.PlanDocument:
			latestPlan = max(latestPlan, doc.Revision)
			if doc.Revision == current.Revision.Plan {
				sealedPlan = doc
			}
		case doc.Path == "amendments/"+req.ID+"/"+plan.SpecPath && doc.Revision == d.Spec:
			draftSpec = doc
		case doc.Path == "amendments/"+req.ID+"/"+plan.PlanPath && doc.Revision == d.Plan:
			draftPlan = doc
		case doc.Path == "amendments/"+req.ID+"/affected.json" && doc.Revision >= affectedDoc.Revision:
			affectedDoc = doc
		}
	}
	if sealedSpec.ID == "" || sealedPlan.ID == "" || draftSpec.ID == "" || draftPlan.ID == "" || affectedDoc.ID == "" {
		return fmt.Errorf("amendment %s: the sealed or proposed spec and plan revisions or the affected set are missing", req.ID)
	}
	at := a.s.now()
	document := func(id, path string, revision int, content string) trace.Document {
		return trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: id, Revision: revision, Project: a.repository.Project(), Workstream: stream, At: at, Actor: amendmentActor, Cause: cause}, Path: path, Content: content}
	}
	next := current
	var changed []string
	var recorded []trace.Document
	if draftSpec.Content != sealedSpec.Content {
		next.Revision.Spec, next.SpecHash, next.Seal = latestSpec+1, seal.SpecHash(draftSpec.Content), current.Seal+1
		recorded = append(recorded, document(plan.SpecDocument, plan.SpecPath, next.Revision.Spec, draftSpec.Content))
		changed = append(changed, fmt.Sprintf("%s revision %d", plan.SpecPath, next.Revision.Spec))
	}
	if draftPlan.Content != sealedPlan.Content {
		p, err := plan.Parse([]byte(draftPlan.Content))
		if err != nil {
			return unapplied("the approved plan does not parse: " + err.Error())
		}
		entities, err := kb.Load(a.repository)
		if err != nil {
			return err
		}
		footprints, unresolved := seal.Take(p, entities)
		if len(unresolved) > 0 {
			return unapplied("the approved plan's footprints name what the entity map does not resolve: " + strings.Join(unresolved, ", "))
		}
		next.Revision.Plan, next.Footprints = latestPlan+1, footprints
		recorded = append(recorded, document(plan.PlanDocument, plan.PlanPath, next.Revision.Plan, draftPlan.Content))
		changed = append(changed, fmt.Sprintf("%s revision %d", plan.PlanPath, next.Revision.Plan))
	}
	if len(recorded) == 0 {
		return unapplied("the approved revision changes neither the sealed spec nor the sealed plan")
	}
	content, err := seal.Encode(next)
	if err != nil {
		return err
	}
	recorded = append(recorded, document(seal.DocumentID, seal.Path, sealDoc.Revision+1, string(content)))
	application, err := classifyAmendment(req, d, affectedDoc, sealedPlan, draftPlan,
		amendment.Pin{Seal: current.Seal, SealRevision: sealDoc.Revision, Spec: current.Revision.Spec, Plan: current.Revision.Plan},
		amendment.Pin{Seal: next.Seal, SealRevision: sealDoc.Revision + 1, Spec: next.Revision.Spec, Plan: next.Revision.Plan})
	if err != nil {
		return unapplied(err.Error())
	}
	content, err = json.MarshalIndent(application, "", "  ")
	if err != nil {
		return err
	}
	recorded = append(recorded, document(amendment.DocumentID(req.ID), amendment.Path(req.ID), 1, string(content)+"\n"))
	feature, err := a.repository.Workflow(stream, trace.FeatureSubject)
	if err != nil {
		return err
	}
	resealed := fmt.Sprintf("seal %d is recorded", next.Seal)
	if next.Seal == current.Seal {
		resealed = fmt.Sprintf("seal %d keeps its spec hash", next.Seal)
	}
	id := "amendment-" + req.ID + "-" + amendmentResealed
	reason := fmt.Sprintf("approved amendment %s versions %s; %s on the same base %s; the workstream stays %s and no code is edited", req.ID, strings.Join(changed, " and "), resealed, current.Base.Commit, feature.Value)
	tx := trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: a.amendmentHeader(id, stream, req.Unit, cause), Subject: amendmentSubject(req.ID), From: amendmentApproved, To: amendmentResealed, Reason: reason},
		Events: []trace.Event{trace.Notice(id, "amendment", reason)}}
	if _, err := a.repository.RecordDocumentsWith(ctx, recorded, tx); err != nil && !errors.Is(err, trace.ErrConflict) {
		return err
	}
	return nil
}

func (a amendmentDebate) amendmentHeader(id string, stream config.WorkstreamID, unit, cause string) trace.Header {
	return trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: a.repository.Project(), Workstream: stream, Unit: unit, At: a.s.now(), Actor: amendmentActor, Cause: cause}
}

// rule delivers the owner's ruling on a decided amendment, rejected,
// unapplied or applied, to its requester's thread as the requester's next
// turn, resumes the unit the request parked in the stage it preserved, and
// moves the amendment to ruled. Each step finds
// what an earlier pass did, so a restart between them completes the rest
// once.
func (a amendmentDebate) rule(ctx context.Context, stream config.WorkstreamID, req trace.Amendment, state trace.WorkflowState) error {
	decisions, err := amendmentDecisions(a.repository, stream, req.ID)
	if err != nil {
		return err
	}
	if len(decisions) == 0 {
		return fmt.Errorf("decided amendment %s has no decision", req.ID)
	}
	d := decisions[len(decisions)-1]
	cause := amendmentDecided(req.ID, len(decisions))
	transitions, err := trace.Read[trace.Transition](a.repository, stream)
	if err != nil {
		return err
	}
	var outcomes []string
	for _, t := range transitions {
		if t.Subject == amendmentSubject(req.ID) && (t.To == state.Value || state.Value == amendmentApplied && t.To == amendmentResealed) {
			outcomes = append(outcomes, t.Reason)
		}
	}
	if err := a.deliverRuling(ctx, stream, req, d, cause, strings.Join(outcomes, "; ")); err != nil {
		return err
	}
	verb := map[string]string{amendmentApplied: "approved", amendmentRejected: "rejected", amendmentUnapplied: "approved but could not apply"}[state.Value]
	if err := a.resumeUnit(ctx, stream, req, cause, verb, transitions); err != nil {
		return err
	}
	id := "amendment-" + req.ID + "-" + amendmentRuled
	reason := fmt.Sprintf("the owner's ruling on amendment %s is delivered to %s", req.ID, req.Requester.ID)
	tx := trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: a.amendmentHeader(id, stream, req.Unit, cause), Subject: amendmentSubject(req.ID), From: state.Value, To: amendmentRuled, Reason: reason}}
	if _, err := a.repository.Transact(ctx, tx); err != nil && !errors.Is(err, trace.ErrConflict) {
		return err
	}
	return nil
}

// deliverRuling queues the ruling as the next turn of the requester's thread
// unless the thread holds it already. The turn carries the system prompt of
// the turn that asked for the amendment, or asked the question the chief of
// staff routed as one, and the profile the requester's role is bound to now.
// A requester the trace holds no thread for is sent nothing.
func (a amendmentDebate) deliverRuling(ctx context.Context, stream config.WorkstreamID, req trace.Amendment, d AmendmentDecision, cause, outcome string) error {
	th, err := a.repository.Thread(stream, req.Requester.ID)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	turn := amendmentRulingTurn(req.ID)
	asking := req.Turn
	if req.QuestionID != "" {
		asked, err := a.repository.Questions(stream)
		if err != nil {
			return err
		}
		for _, q := range asked {
			if q.Asked.ID == req.QuestionID {
				asking = q.Asked.Turn
			}
		}
	}
	system := ""
	for i, t := range th.Turns {
		if t.Request.TurnID == turn {
			return nil
		}
		if t.Request.TurnID == asking || i == 0 {
			system = t.Request.SystemPrompt
		}
	}
	profile, _, err := a.s.roleExecution(a.s.current(), th.Identity.Role)
	if err != nil {
		return err
	}
	_, err = a.repository.EnqueueTurn(ctx, trace.TurnRequest{
		Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_" + turn, Revision: 1, Project: a.repository.Project(), Workstream: stream, Unit: req.Unit,
			At: a.s.now(), Actor: amendmentActor, Cause: cause, Depth: 1},
		AgentID: th.Identity.ID, ThreadID: th.Identity.ThreadID, TurnID: turn, Profile: profile, SystemPrompt: system,
		Prompt: amendmentRulingPrompt(req, d, th.Identity.Role, outcome)})
	return err
}

// amendmentRulingPrompt is the text of the turn that delivers the owner's
// ruling on an amendment to its requester. The request and the owner's note
// are quoted in the shared envelope.
func amendmentRulingPrompt(req trace.Amendment, d AmendmentDecision, role, outcome string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "The owner ruled on amendment %s, which you asked for. Decision: %s.\nOutcome: %s\n", req.ID, d.Decision, outcome)
	switch role {
	case masonRole:
		b.WriteString("Continue the unit in your existing workspace against the sealed spec and plan, and call done when its criteria and proofs hold.\n")
	case reviewerRole:
		b.WriteString("Your review of the same candidate resumes against the sealed spec and plan.\n")
	}
	b.WriteString("\n")
	request := fmt.Sprintf("Citations: %s\nChange: %s\nReason: %s", strings.Join(req.Citations, ", "), req.Change, req.Reason)
	sections := []envelope.Section{{Name: "question", Text: request}}
	if d.Note != "" {
		sections = append(sections, envelope.Section{Name: "owner_response", Text: d.Note})
	}
	part, _ := envelope.Render(sections...)
	b.WriteString(part)
	return b.String()
}

// resumeUnit moves the unit the amendment parked from waiting back to the
// stage its waiting transition preserved: implementing for a mason's request,
// reviewing for a reviewer's. A unit the request did not park, or one that
// left that wait, is not moved.
func (a amendmentDebate) resumeUnit(ctx context.Context, stream config.WorkstreamID, req trace.Amendment, cause, verb string, transitions []trace.Transition) error {
	if req.Unit == "" {
		return nil
	}
	subject := trace.UnitSubject(req.Unit)
	parked := subject + "_waiting_amendment_" + req.ID
	resumed := subject + "_resumed_amendment_" + req.ID
	var wait, latest trace.Transition
	for _, t := range transitions {
		if t.ID == resumed {
			return nil
		}
		if t.ID == parked {
			wait = t
		}
		if t.Subject == subject && t.To == UnitWaiting {
			latest = t
		}
	}
	stage := map[string]string{masonRole: UnitImplementing, reviewerRole: UnitReviewing}[wait.Actor.ID]
	if wait.ID == "" || latest.ID != parked || stage == "" {
		return nil
	}
	state, err := a.repository.Workflow(stream, subject)
	if err != nil {
		return err
	}
	if state.Value != UnitWaiting {
		return nil
	}
	reason := fmt.Sprintf("unit %s resumes %s: the owner %s amendment %s, and the ruling is the %s's next turn", req.Unit, stage, verb, req.ID, wait.Actor.ID)
	h := a.amendmentHeader(resumed, stream, req.Unit, cause)
	tx := trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: h, Subject: subject, From: UnitWaiting, To: stage, Reason: reason}, Events: []trace.Event{trace.Notice(resumed, "unit", reason)}}
	if _, err := a.repository.Transact(ctx, tx); err != nil && !errors.Is(err, trace.ErrConflict) {
		return err
	}
	return nil
}
