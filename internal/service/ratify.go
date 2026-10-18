package service

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
)

// presentPacket records the workstream's ratification packet as the next
// revision of shed/round-<n>/packet.json whenever it differs from the packet
// already recorded. A workstream whose debate has neither concluded nor been
// skipped is at no decision point and gets none; every owner action that
// changes what the packet says gives it a new revision, so what the API serves
// is what the trace holds.
func (d *debate) presentPacket(ctx context.Context, stream config.WorkstreamID, skipped bool) error {
	packet, err := d.packet(stream, skipped)
	if err != nil || packet == nil {
		return err
	}
	content, err := shed.EncodePacket(*packet)
	if err != nil {
		return err
	}
	documents, err := trace.Read[trace.Document](d.repository, stream)
	if err != nil {
		return err
	}
	path := shed.PacketPath(packet.Round)
	var recorded trace.Document
	for _, doc := range documents {
		if doc.Path == path {
			recorded = doc
		}
	}
	// The comparison is against the latest revision alone, which is the one
	// the API serves: a packet that says again what an earlier revision said
	// is recorded, or the owner would read a stale decision.
	if recorded.Content == string(content) {
		return nil
	}
	doc := trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: shed.PacketDocumentID(packet.Round), Revision: recorded.Revision + 1,
		Project: d.repository.Project(), Workstream: stream, At: d.s.now(), Actor: shedActor, Cause: "ratification-packet"}, Path: path, Content: string(content)}
	return d.repository.RecordDocuments(ctx, []trace.Document{doc})
}

// packet builds the workstream's current ratification packet, or nothing when
// its debate has neither concluded nor been skipped. The conclusion it carries
// is the recorded reason debate ended, the revisions are the latest recorded
// ones and the dissent record is the dissent that stands now.
func (d *debate) packet(stream config.WorkstreamID, skipped bool) (*shed.Packet, error) {
	state, err := d.repository.Workflow(stream, shedSubject)
	if err != nil {
		return nil, err
	}
	kind, round, ok := shedState(state.Value)
	switch {
	case skipped:
		// A skipped debate is the decision point from the moment it is
		// recorded, whether a round ran before it or not.
		if !ok {
			round = 1
		}
	case !ok || kind != "concluded":
		return nil, nil
	}
	id := fmt.Sprintf("shed-concluded-%d", round)
	if skipped {
		id = skipTransition
	}
	transitions, err := trace.Read[trace.Transition](d.repository, stream)
	if err != nil {
		return nil, err
	}
	i := slices.IndexFunc(transitions, func(t trace.Transition) bool { return t.ID == id })
	if i < 0 {
		return nil, fmt.Errorf("workstream %s reached %q and transition %s is missing", stream, state.Value, id)
	}
	pin, err := latestPin(d.repository, stream)
	if err != nil {
		return nil, err
	}
	entries, err := Dissent(d.repository, stream)
	if err != nil {
		return nil, err
	}
	packet := shed.Present(round, pin, skipped, transitions[i].Reason, entries)
	return &packet, nil
}

// packet serves the workstream's recorded ratification packet. ProjectSpec
// is set when the workstream belongs to a multi-member workgroup whose
// project spec is already drafted, so the owner sees it alongside this
// workstream's own packet before ratifying either.
func (s *Service) packet(raw string) (PacketResponse, *APIError) {
	project, stream, repository, api := s.designMemberTrace(raw)
	if api != nil {
		return PacketResponse{}, api
	}
	packet, doc, found, err := shed.LatestPacket(repository, stream)
	if err != nil {
		return PacketResponse{}, &APIError{Internal, fmt.Sprintf("cannot read the ratification packet of workstream %s; check the trace repository", stream)}
	}
	if !found {
		return PacketResponse{}, &APIError{NotFound, fmt.Sprintf("workstream %s has no ratification packet; the chief of staff presents one once debate concludes or the owner skips it", stream)}
	}
	out := PacketResponse{Project: project, Workstream: stream, Packet: packet, Revision: doc.Revision, At: doc.At}
	if spec, _, found, err := repository.ProjectSpecOf(stream); err == nil && found {
		out.ProjectSpec = &spec
		id, err := repository.CrossRepoWorkgroupOf(stream)
		if err != nil {
			return PacketResponse{}, &APIError{Internal, err.Error()}
		}
		if id != "" {
			wt, err := trace.OpenWorkgroupAuthority(s.current().Root, id)
			if wt != nil {
				defer wt.Close()
			}
			if err != nil {
				return PacketResponse{}, &APIError{Internal, err.Error()}
			}
			spec, revision, found, err := trace.ProjectSpecWithRevisionFor(wt, id)
			if err != nil {
				return PacketResponse{}, &APIError{Internal, err.Error()}
			}
			if found {
				out.ProjectSpec, out.ProjectSpecRevision = &spec, revision
				_, g, _, err := repository.CrossRepoProjectSpec(id)
				if err != nil {
					return PacketResponse{}, &APIError{Internal, err.Error()}
				}
				approval, _, approved, err := trace.ReadGroupApproval(wt, g.Members[0])
				if err != nil {
					return PacketResponse{}, &APIError{Internal, err.Error()}
				}
				if approved {
					out.ProjectSpec, out.ProjectSpecRevision = &approval.ProjectSpec, approval.ProjectSpecRevision
					if out.ProjectSpecRevision == 0 {
						revision, err := trace.RecordedProjectSpecRevision(wt, id, approval.ProjectSpec)
						if err != nil {
							return PacketResponse{}, &APIError{Internal, err.Error()}
						}
						out.ProjectSpecRevision = revision
					}
				}
			}
		}
	}
	return out, nil
}

// ratify records the owner's approval of the exact revisions of the spec and
// the plan they read in the packet. It changes no state of its own: the
// ratification is the record, the record asks for the sealing, and the
// sealing moves the workstream on. For a workstream whose workgroup spans
// more than one repo, this one call instead seals the workgroup's whole
// project spec: every real member is ratified at its own current packet
// pin, all at once, refusing before recording anything if the graph is
// invalid or a member has no slice. A one-workstream workgroup keeps
// exactly today's single-workstream behaviour.
func (s *Service) ratify(ctx context.Context, raw string, req RatifyRequest) (RatifyResponse, *APIError) {
	if req.Spec < 1 || req.Plan < 1 {
		return RatifyResponse{}, &APIError{Validation, "ratify the revisions of the spec and the plan the packet named"}
	}
	_, stream, repo, api := s.designMemberTrace(raw)
	if api != nil {
		return RatifyResponse{}, api
	}
	id, err := repo.CrossRepoWorkgroupOf(stream)
	if err != nil {
		return RatifyResponse{}, &APIError{Internal, err.Error()}
	}
	if id != "" {
		required, err := repo.GroupApprovalRequired(stream)
		if err != nil {
			return RatifyResponse{}, &APIError{Internal, err.Error()}
		}
		if required && req.ProjectSpecRevision < 1 {
			return RatifyResponse{}, &APIError{Conflict, projectSpecCatchingUp}
		}
		out, found, api := s.resumeGroupApproval(ctx, repo, stream, id, shed.Pin{Spec: req.Spec, Plan: req.Plan}, req.ProjectSpecRevision)
		if found {
			return out, api
		}
	}
	o, api := s.shedAction(string(stream), InShedState, SketchedState)
	if api != nil {
		return RatifyResponse{}, api
	}
	workgroup, err := o.repository.CrossRepoWorkgroupOf(o.stream)
	if err != nil {
		return RatifyResponse{}, &APIError{Internal, fmt.Sprintf("cannot read the cross-repo workgroup of workstream %s; check the trace repository", o.stream)}
	}
	asked := shed.Pin{Spec: req.Spec, Plan: req.Plan}
	required, err := o.repository.GroupApprovalRequired(o.stream)
	if err != nil {
		return RatifyResponse{}, &APIError{Internal, err.Error()}
	}
	if !required {
		return s.ratifyMember(ctx, o, asked)
	}
	return s.ratifyWorkgroup(ctx, o, workgroup, asked, req.ProjectSpecRevision)
}

// ratifyMember is the single-workstream ratification: unchanged from what
// ratify always did, parameterized by the pin to ratify at instead of
// reading the request directly, so a multi-member workgroup's other
// members can be ratified at their own current pin by the same logic.
func (s *Service) ratifyMember(ctx context.Context, o *shedOwner, asked shed.Pin) (RatifyResponse, *APIError) {
	latest, ratified, err := latestRatification(o.repository, o.stream)
	if err != nil {
		return RatifyResponse{}, &APIError{Internal, fmt.Sprintf("cannot read the ratifications of workstream %s; check the trace repository", o.stream)}
	}
	if ratified && latest.Revision == asked {
		// The revisions are approved already: the gate reports the sealing
		// their record asked for, and records the ratification again only
		// when that sealing failed.
		return s.sealing(ctx, o, latest, fmt.Sprintf("workstream %s is ratified at %s already", o.stream, asked))
	}
	entries, err := Dissent(o.repository, o.stream)
	if err != nil {
		return RatifyResponse{}, &APIError{Internal, fmt.Sprintf("cannot read the dissent record of workstream %s; check the trace repository", o.stream)}
	}
	refusals, err := s.refusals(o, asked, entries)
	if err != nil {
		return RatifyResponse{}, &APIError{Internal, fmt.Sprintf("cannot read the spec and plan of workstream %s; check the trace repository", o.stream)}
	}
	if len(refusals) > 0 {
		return RatifyResponse{}, &APIError{Conflict, fmt.Sprintf("workstream %s is not ratified:\n- %s", o.stream, strings.Join(refusals, "\n- "))}
	}
	record := shed.Ratify(o.round, asked, entries)
	content, err := shed.EncodeRatification(record)
	if err != nil {
		return RatifyResponse{}, &APIError{Validation, "the ratification cannot be recorded: " + err.Error()}
	}
	reason := fmt.Sprintf("the owner ratified %s after round %d", asked, o.round)
	if len(record.Dispositions) > 0 {
		reason += fmt.Sprintf(", over %s the owner disposed of", shed.Objections(len(record.Dispositions)))
	}
	if dismissed := len(entries) - len(shed.ForOwner(entries)); dismissed > 0 {
		reason += fmt.Sprintf(", with %s the chief of staff dismissed", shed.Objections(dismissed))
	}
	if api := o.record(ctx, shed.RatificationDocumentID(o.round), shed.RatificationPath(o.round), string(content), fmt.Sprintf("ratified-%d", o.round), reason); api != nil {
		return RatifyResponse{}, api
	}
	return RatifyResponse{Project: o.project, Workstream: o.stream, Round: record.Round, Spec: asked.Spec, Plan: asked.Plan, Sealing: "requested", Detail: reason + "; the sealing is asked for"}, nil
}

const projectSpecCatchingUp = "the project spec is catching up with the latest member revisions; read the packet again"

// ratifyWorkgroup validates the entire draft before recording one durable
// owner decision. Member decisions and seals are preparations; only durable
// group completion grants effective ratification and production admission.
func (s *Service) ratifyWorkgroup(ctx context.Context, o *shedOwner, id config.WorkgroupID, asked shed.Pin, projectSpecRevision int) (RatifyResponse, *APIError) {
	lock := s.groupApprovalLock(id)
	lock.Lock()
	defer lock.Unlock()
	failed := &APIError{Internal, fmt.Sprintf("cannot read the project spec of workgroup %s; check the trace repository", id)}
	wt, g, err := s.approvalTrace(ctx, o.repository, id)
	if err != nil {
		return RatifyResponse{}, failed
	}
	defer wt.Close()
	spec, revision, found, err := trace.ProjectSpecWithRevisionFor(wt, id)
	if err != nil {
		return RatifyResponse{}, failed
	}
	if !found || len(g.Members) < 2 {
		return RatifyResponse{}, &APIError{Conflict, fmt.Sprintf("workgroup %s has no recorded project spec yet; the architect has not finished drafting it", id)}
	}
	if _, _, approved, err := trace.ReadGroupApproval(wt, g.Members[0]); err != nil {
		return RatifyResponse{}, failed
	} else if !approved {
		message, err := s.contractGuardRefusal(wt, g, spec)
		if err != nil {
			return RatifyResponse{}, failed
		}
		if message != "" {
			return RatifyResponse{}, &APIError{Conflict, message}
		}
	}
	if projectSpecRevision < 1 || projectSpecRevision != revision {
		return RatifyResponse{}, &APIError{Conflict, projectSpecCatchingUp}
	}
	real := g.Members[1:]
	if err := trace.ValidateGraph(real, spec.Graph); err != nil {
		return RatifyResponse{}, &APIError{Validation, fmt.Sprintf("workgroup %s's project spec graph is invalid: %v", id, err)}
	}
	for _, m := range real {
		if _, ok := trace.SliceFor(spec, m); !ok {
			return RatifyResponse{}, &APIError{Conflict, fmt.Sprintf("workgroup %s's project spec does not cover every member: workstream %s has no slice", id, m)}
		}
	}
	owners := make(map[config.WorkstreamID]*shedOwner, len(real))
	pins := make(map[config.WorkstreamID]shed.Pin, len(real))
	var refused []string
	for _, m := range real {
		mo, api := s.shedAction(string(m), InShedState, SketchedState)
		if api != nil {
			refused = append(refused, fmt.Sprintf("workstream %s: %s", m, api.Message))
			continue
		}
		pin := mo.pin
		if m == o.stream {
			pin = asked
		}
		owners[m], pins[m] = mo, pin
		entries, err := Dissent(mo.repository, mo.stream)
		if err != nil {
			return RatifyResponse{}, &APIError{Internal, fmt.Sprintf("cannot read the dissent record of workstream %s; check the trace repository", mo.stream)}
		}
		memberRefusals, err := s.refusals(mo, pin, entries)
		if err != nil {
			return RatifyResponse{}, &APIError{Internal, fmt.Sprintf("cannot read the spec and plan of workstream %s; check the trace repository", mo.stream)}
		}
		for _, r := range memberRefusals {
			refused = append(refused, fmt.Sprintf("workstream %s: %s", m, r))
		}
		if len(memberRefusals) > 0 {
			continue
		}
		matches, err := sliceMatchesPin(spec, mo.repository, m, pin)
		if err != nil {
			return RatifyResponse{}, &APIError{Internal, err.Error()}
		}
		if !matches {
			refused = append(refused, fmt.Sprintf("workstream %s: %s", m, projectSpecCatchingUp))
		}
	}
	if len(refused) > 0 {
		return RatifyResponse{}, &APIError{Conflict, fmt.Sprintf("workgroup %s is not ratified:\n- %s", id, strings.Join(refused, "\n- "))}
	}
	if err := s.step("group-ratify-validated"); err != nil {
		return RatifyResponse{}, &APIError{Internal, err.Error()}
	}
	decision, _, exists, err := trace.ReadGroupApproval(wt, g.Members[0])
	if err != nil {
		return RatifyResponse{}, &APIError{Internal, err.Error()}
	}
	if exists {
		matched := slices.ContainsFunc(decision.Members, func(m trace.GroupMemberApproval) bool {
			return m.Workstream == o.stream && m.Spec == asked.Spec && m.Plan == asked.Plan
		})
		if !matched {
			return RatifyResponse{}, &APIError{Conflict, "the request differs from the immutable workgroup approval"}
		}
	}
	if !exists {
		decision = trace.GroupApproval{Workgroup: id, ProjectSpec: spec, ProjectSpecRevision: revision}
		for _, m := range real {
			mo, pin := owners[m], pins[m]
			entries, err := Dissent(mo.repository, m)
			if err != nil {
				return RatifyResponse{}, &APIError{Internal, err.Error()}
			}
			record := shed.Ratify(mo.round, pin, entries)
			content, err := shed.EncodeRatification(record)
			if err != nil {
				return RatifyResponse{}, &APIError{Internal, err.Error()}
			}
			decision.Members = append(decision.Members, trace.GroupMemberApproval{Project: mo.project, Workstream: m, Spec: pin.Spec, Plan: pin.Plan, Round: mo.round, Record: content})
		}
		if err := trace.RecordGroupApproval(ctx, wt, g.Members[0], decision, s.now(), ownerActor); err != nil {
			return RatifyResponse{}, &APIError{Internal, err.Error()}
		}
	}
	if err := s.step("group-decision-recorded"); err != nil {
		return RatifyResponse{}, &APIError{Internal, err.Error()}
	}
	if err := s.prepareGroupApproval(ctx, decision); err != nil {
		return RatifyResponse{}, &APIError{Internal, err.Error()}
	}
	return groupApprovalResponse(o.project, o.stream, decision), nil
}

// sealing reports the state of the sealing a ratification's record asked
// for: requested until the controller publishes it, then pending or running.
// One that failed is asked for again by recording the ratification again as
// its next revision, which the controller seals afresh.
func (s *Service) sealing(ctx context.Context, o *shedOwner, r ratification, detail string) (RatifyResponse, *APIError) {
	out := RatifyResponse{Project: o.project, Workstream: o.stream, Round: r.Round, Spec: r.Revision.Spec, Plan: r.Revision.Plan, Sealing: "requested", Detail: detail + "; the sealing is asked for"}
	z := &sealer{s: s, repository: o.repository}
	failed := &APIError{Internal, fmt.Sprintf("cannot read the sealings of workstream %s; check the trace repository", o.stream)}
	op, requested, err := z.sealing(o.stream, r)
	if err != nil {
		return RatifyResponse{}, failed
	}
	if !requested {
		return out, nil
	}
	in, err := decodeSeal(op.Operation)
	if err != nil {
		return RatifyResponse{}, failed
	}
	if op.Result == nil {
		state, reason, _ := progress(op)
		out.Sealing, out.Detail = state, fmt.Sprintf("%s; sealing %d is %s", detail, in.Seal, state)
		if reason != "" {
			out.Detail += " after a failed attempt: " + reason
		}
		return out, nil
	}
	content, err := shed.EncodeRatification(r.Ratification)
	if err != nil {
		return RatifyResponse{}, failed
	}
	reason := fmt.Sprintf("the owner ratified %s after round %d again; sealing %d failed and the sealing is asked for again", r.Revision, r.Round, in.Seal)
	if api := o.record(ctx, shed.RatificationDocumentID(r.Round), shed.RatificationPath(r.Round), string(content), fmt.Sprintf("ratified-%d", r.Round), reason); api != nil {
		return RatifyResponse{}, api
	}
	out.Detail = fmt.Sprintf("%s; sealing %d failed and the sealing is asked for again", detail, in.Seal)
	return out, nil
}

// refusals lists every reason the workstream cannot be ratified at the given
// revisions: a blocking objection the owner has not disposed of, a plan that
// does not validate, revisions that are not the current ones, and a workstream
// that is not at its decision point. Every reason that applies is listed, so
// one call tells the owner everything they have to settle.
func (s *Service) refusals(o *shedOwner, asked shed.Pin, entries []shed.Entry) ([]string, error) {
	var out []string
	if o.feature == SketchedState && !o.skipped {
		out = append(out, fmt.Sprintf("the workstream is %s and debate is not skipped: it is ratified in the shed", SketchedState))
	}
	if asked != o.pin {
		out = append(out, fmt.Sprintf("%s are ratified, and the current revisions are %s: read the packet again", asked, o.pin))
	}
	for _, e := range shed.Blocked(entries) {
		about := " on " + e.Part
		if e.Part == "" {
			about = ""
		}
		reason := "has no disposition"
		if e.Disposition == shed.Sustained {
			reason = "is sustained and is not conceded"
		}
		out = append(out, fmt.Sprintf("objection %s (%s, by %s in round %d%s) blocks and %s", e.ID, e.Kind, e.Member, e.Round, about, reason))
	}
	problems, err := s.planProblems(o)
	if err != nil {
		return nil, err
	}
	for _, p := range problems {
		out = append(out, "the plan is not valid: "+p)
	}
	return out, nil
}

// planProblems validates the workstream's latest recorded spec and plan, the
// revisions a ratification approves, exactly as the architect's draft is
// validated.
func (s *Service) planProblems(o *shedOwner) ([]string, error) {
	dr := &drafter{s: s, repository: o.repository}
	latest, err := dr.latest(o.stream)
	if err != nil {
		return nil, err
	}
	spec, drafted := latest[plan.SpecDocument]
	graph, planned := latest[plan.PlanDocument]
	if !drafted || !planned {
		return nil, errors.New("the workstream has no recorded spec and plan")
	}
	return dr.validate(spec, graph)
}
