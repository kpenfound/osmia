package intake

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

// Amendment subject states after a recorded decision.
const (
	AmendmentPresented = "presented"
	AmendmentApproved  = "approved"
	AmendmentRejected  = "rejected"
	AmendmentResealed  = "resealed"
	AmendmentUnapplied = "unapplied"
	AmendmentRuled     = "ruled"
)

var AmendmentActor = trace.Actor{Kind: "service", ID: "amendments"}

func AmendmentDecisionPath(id string) string { return "amendments/" + id + "/decision.json" }

func AmendmentDecisionID(id string) string { return "amendment_" + id + "_decision" }

// amendmentDecided is the ID of the transition that records the k-th
// decision on an amendment.
func AmendmentDecided(id string, k int) string { return fmt.Sprintf("amendment-%s-decided-%d", id, k) }

// amendmentRulingTurn is the turn that delivers the recorded ruling on an
// amendment to its requester's thread.
func AmendmentRulingTurn(id string) string { return "amendment_" + id + "_ruling" }

// amendmentSealMatches compares the sealed documents in force at a recorded
// seal revision with the current seal. An upstream base move keeps that identity.
func AmendmentSealMatches(repository *trace.Repository, stream config.WorkstreamID, revision int, current seal.Seal) (bool, error) {
	docs, err := trace.Read[trace.Document](repository, stream)
	if err != nil {
		return false, err
	}
	for _, doc := range docs {
		if doc.ID != seal.DocumentID || doc.Revision != revision {
			continue
		}
		original, err := seal.Parse([]byte(doc.Content))
		if err != nil {
			return false, fmt.Errorf("seal.json revision %d: %w", revision, err)
		}
		return original.Seal == current.Seal && original.SpecHash == current.SpecHash && original.Revision == current.Revision, nil
	}
	return false, fmt.Errorf("seal.json revision %d is missing", revision)
}

// amendmentDecisions returns the recorded decisions on an amendment, oldest
// first.
func AmendmentDecisions(repository *trace.Repository, stream config.WorkstreamID, id string) ([]AmendmentDecision, error) {
	docs, err := trace.Read[trace.Document](repository, stream)
	if err != nil {
		return nil, err
	}
	var out []AmendmentDecision
	for _, d := range docs {
		if d.Path != AmendmentDecisionPath(id) {
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
// one more for every additional round requested.
func CurrentAmendmentRound(repository *trace.Repository, stream config.WorkstreamID, id string) (int, error) {
	decisions, err := AmendmentDecisions(repository, stream, id)
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

// amendmentCase is what a decision on one amendment reads.
type AmendmentCase struct {
	Request   trace.Amendment
	State     trace.WorkflowState
	Round     int
	Packet    trace.Document
	Spec      trace.Document
	Plan      trace.Document
	Decisions []AmendmentDecision
}

// readAmendment reads one amendment of the workstream and reports whether the
// workstream filed it.
func ReadAmendment(repository *trace.Repository, stream config.WorkstreamID, id string) (AmendmentCase, bool, error) {
	requests, err := trace.Read[trace.Amendment](repository, stream)
	if err != nil {
		return AmendmentCase{}, false, err
	}
	i := slices.IndexFunc(requests, func(a trace.Amendment) bool { return a.ID == id })
	if i < 0 {
		return AmendmentCase{}, false, nil
	}
	c := AmendmentCase{Request: requests[i]}
	if c.State, err = repository.Workflow(stream, AmendmentSubject(id)); err != nil {
		return AmendmentCase{}, false, err
	}
	if c.Decisions, err = AmendmentDecisions(repository, stream, id); err != nil {
		return AmendmentCase{}, false, err
	}
	if c.Round, err = CurrentAmendmentRound(repository, stream, id); err != nil {
		return AmendmentCase{}, false, err
	}
	docs, err := trace.Read[trace.Document](repository, stream)
	if err != nil {
		return AmendmentCase{}, false, err
	}
	for _, d := range docs {
		switch {
		case d.Path == AmendmentPacketPath(id) && d.Revision > c.Packet.Revision:
			c.Packet = d
		case d.Path == "amendments/"+id+"/"+plan.SpecPath && d.Revision > c.Spec.Revision:
			c.Spec = d
		case d.Path == "amendments/"+id+"/"+plan.PlanPath && d.Revision > c.Plan.Revision:
			c.Plan = d
		}
	}
	return c, true, nil
}

func (c AmendmentCase) Response(project config.ProjectID, stream config.WorkstreamID, detail string) AmendmentResponse {
	out := AmendmentResponse{Project: project, Workstream: stream, Amendment: c.Request.ID, State: c.State.Value, Round: c.Round, Revision: c.Packet.Revision, Detail: detail}
	if c.Packet.Revision > 0 {
		out.Packet = json.RawMessage(c.Packet.Content)
	}
	if len(c.Decisions) > 0 {
		out.Decision = &c.Decisions[len(c.Decisions)-1]
	}
	return out
}

// amendmentView serves one amendment of a workstream.
func (s *Controller) AmendmentView(raw, id string) (AmendmentResponse, *APIError) {
	project, stream, repository, api := s.ConversationTrace(raw)
	if api != nil {
		return AmendmentResponse{}, api
	}
	c, found, err := ReadAmendment(repository, stream, id)
	if err != nil {
		return AmendmentResponse{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot read amendment %s of workstream %s; check the trace repository", id, stream)}
	}
	if !found {
		return AmendmentResponse{}, &APIError{Code: NotFound, Message: fmt.Sprintf("workstream %s has no amendment %s", stream, id)}
	}
	return c.Response(project, stream, ""), nil
}

// decideAmendment records the owner's decision on the named packet revision
// of a presented amendment.
func (s *Controller) DecideAmendment(ctx context.Context, raw, id string, req AmendmentDecisionRequest) (AmendmentResponse, *APIError) {
	project, stream, repository, api := s.ConversationTrace(raw)
	if api != nil {
		return AmendmentResponse{}, api
	}
	return s.RecordAmendmentDecision(ctx, project, stream, repository, id, req, OwnerActor, "owner-amendment")
}

// recordAmendmentDecision records one decision on an amendment, as the
// actor, and moves the amendment to what the decision asks for: approved,
// rejected, or proposed again for another debate round. A decision on a
// packet revision already decided returns that decision when it
// is the same and is refused when it differs, so a retry never decides
// twice. Approval is refused while an objection blocks it, and approval or
// overrule once the sealed spec or plan moved since the request was filed.
func (s *Controller) RecordAmendmentDecision(ctx context.Context, project config.ProjectID, stream config.WorkstreamID, repository *trace.Repository, id string, req AmendmentDecisionRequest, actor trace.Actor, cause string) (AmendmentResponse, *APIError) {
	failed := &APIError{Code: Internal, Message: fmt.Sprintf("cannot record the decision on amendment %s of workstream %s; check the trace repository", id, stream)}
	decision := strings.TrimSpace(req.Decision)
	if !slices.Contains([]string{AmendmentApprove, AmendmentReject, AmendmentRound, AmendmentOverrule}, decision) {
		return AmendmentResponse{}, &APIError{Code: Validation, Message: "an amendment decision is approve, reject, round or overrule"}
	}
	if req.Packet < 1 {
		return AmendmentResponse{}, &APIError{Code: Validation, Message: "name the revision of the amendment's packet the decision is on; read it with osmia amendment"}
	}
	feature, err := repository.Workflow(stream, trace.FeatureSubject)
	if err != nil {
		return AmendmentResponse{}, failed
	}
	if feature.Value != BuildingState && feature.Value != AssembledState {
		return AmendmentResponse{}, &APIError{Code: Conflict, Message: fmt.Sprintf("workstream %s is %s; amendments are decided while it is building or assembled", stream, FeatureState(feature.Value))}
	}
	c, found, err := ReadAmendment(repository, stream, id)
	if err != nil {
		return AmendmentResponse{}, failed
	}
	if !found {
		return AmendmentResponse{}, &APIError{Code: NotFound, Message: fmt.Sprintf("workstream %s has no amendment %s", stream, id)}
	}
	for _, d := range c.Decisions {
		if d.Packet != req.Packet {
			continue
		}
		if d.Decision == decision {
			return c.Response(project, stream, fmt.Sprintf("the recorded decision to %s amendment %s on packet revision %d is already recorded", decision, id, req.Packet)), nil
		}
		return AmendmentResponse{}, &APIError{Code: Conflict, Message: fmt.Sprintf("packet revision %d of amendment %s is already decided: %s", req.Packet, id, d.Decision)}
	}
	if c.State.Value != AmendmentPresented || c.Packet.Revision == 0 {
		return AmendmentResponse{}, &APIError{Code: Conflict, Message: fmt.Sprintf("amendment %s of workstream %s is %s; it can be decided once its packet is presented", id, stream, FeatureState(c.State.Value))}
	}
	if req.Packet != c.Packet.Revision {
		return AmendmentResponse{}, &APIError{Code: Conflict, Message: fmt.Sprintf("packet revision %d of amendment %s is not the latest; revision %d is presented, read it with osmia amendment", req.Packet, id, c.Packet.Revision)}
	}
	current, sealDoc, sealed, err := seal.Latest(repository, stream)
	if err != nil || !sealed {
		return AmendmentResponse{}, failed
	}
	records, err := AmendmentRecords(repository, stream, id)
	if err != nil {
		return AmendmentResponse{}, failed
	}
	entries := AmendmentDissent(records)
	record := AmendmentDecision{Actor: actor.ID, Decision: decision, Note: strings.TrimSpace(req.Note), Round: c.Round, Packet: c.Packet.Revision, Spec: c.Spec.Revision, Plan: c.Plan.Revision, SealRevision: sealDoc.Revision}
	if actor.Kind != "owner" {
		if api := s.EngineeringRevision(repository, stream, c, current, entries, req.Note); api != nil {
			return AmendmentResponse{}, api
		}
	}
	var to, reason string
	switch decision {
	case AmendmentApprove, AmendmentOverrule:
		matching, err := AmendmentSealMatches(repository, stream, c.Request.SealRevision, current)
		if err != nil {
			return AmendmentResponse{}, failed
		}
		if !matching {
			return AmendmentResponse{}, &APIError{Code: Conflict, Message: fmt.Sprintf("the sealed spec or plan changed since amendment %s was filed against seal.json revision %d; revision %d is in force, so reject it and file a new request", id, c.Request.SealRevision, sealDoc.Revision)}
		}
		if decision == AmendmentApprove {
			if blocked := shed.Blocked(entries); len(blocked) > 0 {
				return AmendmentResponse{}, &APIError{Code: Conflict, Message: fmt.Sprintf("objections %s block amendment %s; overrule them or reject the amendment", EntryIDs(blocked), id)}
			}
			reason = fmt.Sprintf("the owner approved amendment %s on packet revision %d after round %d", id, c.Packet.Revision, c.Round)
		} else {
			if len(entries) == 0 {
				return AmendmentResponse{}, &APIError{Code: Conflict, Message: fmt.Sprintf("no objection stands against amendment %s; approve it instead", id)}
			}
			for _, e := range entries {
				record.Overruled = append(record.Overruled, e.ID)
			}
			reason = fmt.Sprintf("the owner overruled objections %s and approved amendment %s on packet revision %d after round %d", strings.Join(record.Overruled, ", "), id, c.Packet.Revision, c.Round)
		}
		to = AmendmentApproved
		reason += "; the spec and plan are versioned and resealed next"
	case AmendmentReject:
		to = AmendmentRejected
		reason = fmt.Sprintf("the owner rejected amendment %s on packet revision %d; the sealed spec and plan stay in force", id, c.Packet.Revision)
	case AmendmentRound:
		if limit := s.Current().Shed.MaxRounds; c.Round >= limit {
			return AmendmentResponse{}, &APIError{Code: Conflict, Message: fmt.Sprintf("amendment %s has had %d of shed.max_rounds (%d) debate rounds; approve, overrule or reject it", id, c.Round, limit)}
		}
		to = "proposed"
		reason = fmt.Sprintf("the owner asked for debate round %d on amendment %s after reading packet revision %d", c.Round+1, id, c.Packet.Revision)
	}
	if actor.Kind != "owner" {
		reason = strings.ReplaceAll(reason, "the owner", "the chief of staff")
	}
	if record.Note != "" {
		reason += ": " + record.Note
	}
	content, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return AmendmentResponse{}, failed
	}
	k := len(c.Decisions) + 1
	at := s.Now()
	doc := trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: AmendmentDecisionID(id), Revision: k, Project: project, Workstream: stream, At: at, Actor: actor, Cause: cause}, Path: AmendmentDecisionPath(id), Content: string(content) + "\n"}
	transition := AmendmentDecided(id, k)
	tx := trace.Transaction{ExpectedVersion: c.State.Version,
		Transition: trace.Transition{Header: trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: transition, Revision: 1, Project: project, Workstream: stream, Unit: c.Request.Unit, At: at, Actor: actor, Cause: cause}, Subject: AmendmentSubject(id), From: AmendmentPresented, To: to, Reason: reason},
		Events:     []trace.Event{trace.Notice(transition, "amendment", reason)}}
	if _, err := repository.RecordDocumentsWith(ctx, []trace.Document{doc}, tx); err != nil {
		if errors.Is(err, trace.ErrConflict) {
			return AmendmentResponse{}, &APIError{Code: Conflict, Message: fmt.Sprintf("amendment %s changed while the decision was recorded; read it again with osmia amendment and retry", id)}
		}
		return AmendmentResponse{}, failed
	}
	c.State = trace.WorkflowState{Value: to, Version: c.State.Version + 1}
	c.Decisions = append(c.Decisions, record)
	if decision == AmendmentRound {
		c.Round++
	}
	return c.Response(project, stream, reason), nil
}

// decideAmendmentTool records a decision on an amendment that the
// owner gave the chief of staff in a message.
const DecideAmendmentTool = "decide_amendment"

// amendmentGuidance tells the chief of staff when and how to use
// decide_amendment. It belongs in the system prompt of every owner message
// turn.
const AmendmentGuidance = "When the owner's message decides an amendment you presented, call decide_amendment with the amendment's number, the packet revision you presented, the owner's decision and the owner's own words as the note. " +
	"The decisions are approve, reject, round for one more bounded debate round, and overrule to approve over the objections that still stand. For a plan-only revision preserving approved intent, call decide_amendment yourself with owner_decided false and explain why the revised assignments still fulfill the spec. Investigate engineering disagreements and discovered work internally. Spec changes and charter exceptions require an owner decision; use owner_decided true only to relay the decision in this owner message."

// decideAmendment returns the decide_amendment tool of one claimed
// chief-of-staff turn. It records the same decision as POST
// /amendment/<workstream>/<n> on the turn's workstream. Delegated engineering
// decisions identify the chief; relayed decisions identify the owner. A refusal is an
// ordinary result, {"recorded":false,"reason":...}, and records nothing. A
// shared chief of staff names the member whose amendment it decides.
func (c *Controls) DecideAmendment(repository *trace.Repository, scope coreadapter.Scope) coreadapter.Tool {
	tool := coreadapter.Tool{Name: DecideAmendmentTool, Effect: coreadapter.ToolMemory,
		Description: "Decide a plan-only engineering revision that preserves approved intent, with a reason. Set owner_decided true only to relay an explicit owner decision on this turn; spec changes require it. amendment: its number; packet: the presented packet revision; decision: approve, reject, round (one more bounded debate round) or overrule (approve over the objections that stand); note: engineering reasoning, or the owner's words when relaying an owner decision. " +
			"Approval versions the spec and plan and reseals; rejection keeps the sealed documents; either way the requester receives the ruling and its unit resumes. workstream: a member workstream ID of your workgroup to act on; omit for this conversation's own workstream.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"amendment":{"type":"string"},"packet":{"type":"integer","minimum":1},"decision":{"type":"string","enum":["approve","reject","round","overrule"]},"note":{"type":"string"},"owner_decided":{"type":"boolean"},"workstream":{"type":"string"}},"required":["amendment","packet","decision"],"additionalProperties":false}`)}
	tool.Handle = func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
		var input struct {
			Amendment    string              `json:"amendment"`
			Packet       int                 `json:"packet"`
			Decision     string              `json:"decision"`
			Note         string              `json:"note"`
			OwnerDecided bool                `json:"owner_decided"`
			Workstream   config.WorkstreamID `json:"workstream"`
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
			return nil, errors.New("amendment decisions are unavailable")
		}
		request, owner, err := repository.OwnerTurn(trace.ChiefOfStaff, scope)
		if err != nil {
			return nil, err
		}
		target, stream, refusal, err := repository.ChiefMember(config.WorkstreamID(scope.Workstream), input.Workstream)
		if err != nil {
			return nil, err
		}
		if refusal != "" {
			return PriorityRefusal(refusal)
		}
		if input.OwnerDecided && !owner {
			return PriorityRefusal("this turn does not answer an owner message")
		}
		actor := trace.Actor{Kind: "agent", ID: trace.ChiefOfStaff}
		if input.OwnerDecided {
			actor = request.Actor
		}
		out, api := s.RecordAmendmentDecision(ctx, target.Project(), stream, target, input.Amendment,
			AmendmentDecisionRequest{Decision: input.Decision, Note: input.Note, Packet: input.Packet}, actor, request.ID)
		if api != nil {
			if api.Code == Internal {
				return nil, errors.New(api.Message)
			}
			return PriorityRefusal(api.Message)
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

func EntryIDs(entries []shed.Entry) string {
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
// finds either all of them or none. An approval that can no longer apply
// moves to unapplied with the reason, and the sealed documents stay in force.
func (a AmendmentDebate) Reseal(ctx context.Context, stream config.WorkstreamID, req trace.Amendment, state trace.WorkflowState) error {
	decisions, err := AmendmentDecisions(a.Repository, stream, req.ID)
	if err != nil {
		return err
	}
	if len(decisions) == 0 {
		return fmt.Errorf("approved amendment %s has no decision", req.ID)
	}
	d := decisions[len(decisions)-1]
	cause := AmendmentDecided(req.ID, len(decisions))
	unapplied := func(reason string) error {
		id := "amendment-" + req.ID + "-" + AmendmentUnapplied
		reason = fmt.Sprintf("approved amendment %s cannot be applied and the sealed spec and plan stay in force: %s", req.ID, reason)
		tx := trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: a.AmendmentHeader(id, stream, req.Unit, cause), Subject: AmendmentSubject(req.ID), From: AmendmentApproved, To: AmendmentUnapplied, Reason: reason},
			Events: []trace.Event{trace.Notice(id, "amendment", reason)}}
		if _, err := a.Repository.Transact(ctx, tx); err != nil && !errors.Is(err, trace.ErrConflict) {
			return err
		}
		return nil
	}
	current, sealDoc, found, err := seal.Latest(a.Repository, stream)
	if err != nil {
		return err
	}
	if !found {
		return unapplied("the workstream has no seal")
	}
	matching, err := AmendmentSealMatches(a.Repository, stream, d.SealRevision, current)
	if err != nil {
		return err
	}
	if !matching {
		return unapplied(fmt.Sprintf("the sealed spec or plan changed since the decision on seal.json revision %d; revision %d is in force", d.SealRevision, sealDoc.Revision))
	}
	docs, err := trace.Read[trace.Document](a.Repository, stream)
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
	var affected AmendmentAffected
	if err := json.Unmarshal([]byte(affectedDoc.Content), &affected); err != nil {
		return err
	}
	idle, err := RevisionWorkersIdle(a.Repository, stream, affected.Units)
	if err != nil {
		return err
	}
	if !idle {
		return nil
	}
	at := a.S.Now()
	document := func(id, path string, revision int, content string) trace.Document {
		return trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: id, Revision: revision, Project: a.Repository.Project(), Workstream: stream, At: at, Actor: AmendmentActor, Cause: cause}, Path: path, Content: content}
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
		entities, err := kb.Load(a.Repository)
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
	application, err := ClassifyAmendment(req, d, affectedDoc, sealedPlan, draftPlan,
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
	feature, err := a.Repository.Workflow(stream, trace.FeatureSubject)
	if err != nil {
		return err
	}
	resealed := fmt.Sprintf("seal %d is recorded", next.Seal)
	if next.Seal == current.Seal {
		resealed = fmt.Sprintf("seal %d keeps its spec hash", next.Seal)
	}
	id := "amendment-" + req.ID + "-" + AmendmentResealed
	reason := fmt.Sprintf("approved amendment %s versions %s; %s on the same base %s; the workstream stays %s and no code is edited", req.ID, strings.Join(changed, " and "), resealed, current.Base.Commit, feature.Value)
	tx := trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: a.AmendmentHeader(id, stream, req.Unit, cause), Subject: AmendmentSubject(req.ID), From: AmendmentApproved, To: AmendmentResealed, Reason: reason},
		Events: []trace.Event{trace.Notice(id, "amendment", reason)}}
	if _, err := a.Repository.RecordDocumentsWith(ctx, recorded, tx); err != nil && !errors.Is(err, trace.ErrConflict) {
		return err
	}
	return nil
}

func (a AmendmentDebate) AmendmentHeader(id string, stream config.WorkstreamID, unit, cause string) trace.Header {
	return trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: a.Repository.Project(), Workstream: stream, Unit: unit, At: a.S.Now(), Actor: AmendmentActor, Cause: cause}
}

// rule delivers the recorded ruling on a decided amendment, rejected,
// unapplied or applied, to its requester's thread as the requester's next
// turn, resumes the unit the request parked in the stage it preserved, and
// moves the amendment to ruled. Each step finds
// what an earlier pass did, so a restart between them completes the rest
// once.
func (a AmendmentDebate) Rule(ctx context.Context, stream config.WorkstreamID, req trace.Amendment, state trace.WorkflowState) error {
	decisions, err := AmendmentDecisions(a.Repository, stream, req.ID)
	if err != nil {
		return err
	}
	if len(decisions) == 0 {
		return fmt.Errorf("decided amendment %s has no decision", req.ID)
	}
	d := decisions[len(decisions)-1]
	cause := AmendmentDecided(req.ID, len(decisions))
	transitions, err := trace.Read[trace.Transition](a.Repository, stream)
	if err != nil {
		return err
	}
	var outcomes []string
	for _, t := range transitions {
		if t.Subject == AmendmentSubject(req.ID) && (t.To == state.Value || state.Value == AmendmentApplied && t.To == AmendmentResealed) {
			outcomes = append(outcomes, t.Reason)
		}
	}
	if err := a.DeliverRuling(ctx, stream, req, d, cause, strings.Join(outcomes, "; ")); err != nil {
		return err
	}
	verb := map[string]string{AmendmentApplied: "approved", AmendmentRejected: "rejected", AmendmentUnapplied: "approved but could not apply"}[state.Value]
	if err := a.ResumeUnit(ctx, stream, req, cause, verb, d.Actor, transitions); err != nil {
		return err
	}
	id := "amendment-" + req.ID + "-" + AmendmentRuled
	reason := fmt.Sprintf("the recorded ruling on amendment %s is delivered to %s", req.ID, req.Requester.ID)
	tx := trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: a.AmendmentHeader(id, stream, req.Unit, cause), Subject: AmendmentSubject(req.ID), From: state.Value, To: AmendmentRuled, Reason: reason}}
	if _, err := a.Repository.Transact(ctx, tx); err != nil && !errors.Is(err, trace.ErrConflict) {
		return err
	}
	return nil
}

// deliverRuling queues the ruling as the next turn of the requester's thread
// unless the thread holds it already. The turn carries the system prompt of
// the turn that asked for the amendment, or asked the question the chief of
// staff routed as one, and the profile the requester's role is bound to now.
// A requester the trace holds no thread for is sent nothing, and neither is
// a drift mason: its thread takes turns for a drift rebase's conflicts
// alone.
func (a AmendmentDebate) DeliverRuling(ctx context.Context, stream config.WorkstreamID, req trace.Amendment, d AmendmentDecision, cause, outcome string) error {
	if req.Requester.ID == DriftMasonAgent {
		return nil
	}
	th, err := a.Repository.Thread(stream, req.Requester.ID)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if th.Identity.Role == ReviewerRole {
		app, doc, err := a.Application(stream, req.ID)
		if err != nil {
			return err
		}
		// A changed assignment needs a fresh candidate before the reviewer resumes.
		if doc.Revision > 0 && slices.Contains(app.Rework, req.Unit) {
			return nil
		}
	}
	turn := AmendmentRulingTurn(req.ID)
	asking := req.Turn
	if req.QuestionID != "" {
		asked, err := a.Repository.Questions(stream)
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
	profile, _, err := a.S.RoleExecution(a.S.About(a.Repository), th.Identity.Role)
	if err != nil {
		return err
	}
	_, err = a.Repository.EnqueueTurn(ctx, trace.TurnRequest{
		Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_" + turn, Revision: 1, Project: a.Repository.Project(), Workstream: stream, Unit: req.Unit,
			At: a.S.Now(), Actor: AmendmentActor, Cause: cause, Depth: 1},
		AgentID: th.Identity.ID, ThreadID: th.Identity.ThreadID, TurnID: turn, Profile: profile, SystemPrompt: system,
		Prompt: AmendmentRulingPrompt(req, d, th.Identity.Role, outcome)})
	return err
}

// amendmentRulingPrompt delivers an attributed amendment ruling to its
// requester. The request and the decision note
// are quoted in the shared envelope.
func AmendmentRulingPrompt(req trace.Amendment, d AmendmentDecision, role, outcome string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "A decision was recorded on amendment %s, which you asked for. Decision: %s. Actor: %s.\nOutcome: %s\n", req.ID, d.Decision, d.Actor, outcome)
	switch role {
	case MasonRole:
		b.WriteString("Continue the unit in your existing workspace against the sealed spec and plan, and call done when its acceptance holds.\n")
	case ReviewerRole:
		b.WriteString("Your review of the same candidate resumes against the sealed spec and plan.\n")
	}
	b.WriteString("\n")
	request := fmt.Sprintf("Citations: %s\nChange: %s\nReason: %s", strings.Join(req.Citations, ", "), req.Change, req.Reason)
	sections := []envelope.Section{{Name: "question", Text: request}}
	if d.Note != "" {
		section := "owner_response"
		if d.Actor == trace.ChiefOfStaff {
			section = "answer"
		}
		sections = append(sections, envelope.Section{Name: section, Text: d.Note})
	}
	part, _ := envelope.Render(sections...)
	b.WriteString(part)
	return b.String()
}

// resumeUnit moves the unit the amendment parked from waiting back to the
// stage its waiting transition preserved: implementing for a mason's request,
// reviewing for a reviewer's. A unit the request did not park, or one that
// left that wait, is not moved.
func (a AmendmentDebate) ResumeUnit(ctx context.Context, stream config.WorkstreamID, req trace.Amendment, cause, verb, actor string, transitions []trace.Transition) error {
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
	stage := map[string]string{MasonRole: UnitImplementing, ReviewerRole: UnitReviewing}[wait.Actor.ID]
	if wait.ID == "" || latest.ID != parked || stage == "" {
		return nil
	}
	state, err := a.Repository.Workflow(stream, subject)
	if err != nil {
		return err
	}
	if state.Value != UnitWaiting {
		return nil
	}
	reason := fmt.Sprintf("unit %s resumes %s: the owner %s amendment %s, and the ruling is the %s's next turn", req.Unit, stage, verb, req.ID, wait.Actor.ID)
	if actor == trace.ChiefOfStaff {
		reason = strings.ReplaceAll(reason, "the owner", "the chief of staff")
	}
	h := a.AmendmentHeader(resumed, stream, req.Unit, cause)
	tx := trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: h, Subject: subject, From: UnitWaiting, To: stage, Reason: reason}, Events: []trace.Event{trace.Notice(resumed, "unit", reason)}}
	if _, err := a.Repository.Transact(ctx, tx); err != nil && !errors.Is(err, trace.ErrConflict) {
		return err
	}
	return nil
}
