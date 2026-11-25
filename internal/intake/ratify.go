package intake

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

// PresentPacket records the workstream's ratification packet as the next
// revision of shed/round-<n>/packet.json whenever it differs from the packet
// already recorded. A workstream whose debate has neither concluded nor been
// skipped is at no decision point and gets none; every owner action that
// changes what the packet says gives it a new revision, so what the API serves
// is what the trace holds.
func (d *Debate) PresentPacket(ctx context.Context, stream config.WorkstreamID, skipped bool) error {
	packet, err := d.Packet(stream, skipped)
	if err != nil || packet == nil {
		return err
	}
	content, err := shed.EncodePacket(*packet)
	if err != nil {
		return err
	}
	documents, err := trace.Read[trace.Document](d.Repository, stream)
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
	if recorded.Content == string(content) {
		return nil
	}
	doc := trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: shed.PacketDocumentID(packet.Round), Revision: recorded.Revision + 1,
		Project: d.Repository.Project(), Workstream: stream, At: d.S.Now(), Actor: ShedActor, Cause: "ratification-packet"}, Path: path, Content: string(content)}
	return d.Repository.RecordDocuments(ctx, []trace.Document{doc})
}

// Packet builds the workstream's current ratification packet, or nothing when
// its debate has neither concluded nor been skipped. The conclusion it carries
// is the recorded reason debate ended, the revisions are the latest recorded
// ones and the dissent record is the dissent that stands now.
func (d *Debate) Packet(stream config.WorkstreamID, skipped bool) (*shed.Packet, error) {
	state, err := d.Repository.Workflow(stream, ShedSubject)
	if err != nil {
		return nil, err
	}
	kind, round, ok := ShedState(state.Value)
	switch {
	case skipped:
		if !ok {
			round = 1
		}
	case !ok || kind != "concluded":
		return nil, nil
	}
	id := fmt.Sprintf("shed-concluded-%d", round)
	if skipped {
		id = SkipTransition
	}
	transitions, err := trace.Read[trace.Transition](d.Repository, stream)
	if err != nil {
		return nil, err
	}
	i := slices.IndexFunc(transitions, func(t trace.Transition) bool { return t.ID == id })
	if i < 0 {
		return nil, fmt.Errorf("workstream %s reached %q and transition %s is missing", stream, state.Value, id)
	}
	pin, err := LatestPin(d.Repository, stream)
	if err != nil {
		return nil, err
	}
	entries, err := Dissent(d.Repository, stream)
	if err != nil {
		return nil, err
	}
	packet := shed.Present(round, pin, skipped, transitions[i].Reason, entries)
	return &packet, nil
}

// Packet serves the workstream's recorded ratification packet. ProjectSpec
// is set when the workstream belongs to a multi-member workgroup whose
// project spec is already drafted, so the owner sees it alongside this
// workstream's own packet before ratifying either.
func (s *Controller) Packet(raw string) (PacketResponse, *APIError) {
	project, stream, repository, api := s.DesignMemberTrace(raw)
	if api != nil {
		return PacketResponse{}, api
	}
	packet, doc, found, err := shed.LatestPacket(repository, stream)
	if err != nil {
		return PacketResponse{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot read the ratification packet of workstream %s; check the trace repository", stream)}
	}
	if !found {
		return PacketResponse{}, &APIError{Code: NotFound, Message: fmt.Sprintf("workstream %s has no ratification packet; the chief of staff presents one once debate concludes or the owner skips it", stream)}
	}
	out := PacketResponse{Project: project, Workstream: stream, Packet: packet, Revision: doc.Revision, At: doc.At}
	if spec, _, found, err := repository.ProjectSpecOf(stream); err == nil && found {
		out.ProjectSpec = &spec
		id, err := repository.CrossRepoWorkgroupOf(stream)
		if err != nil {
			return PacketResponse{}, &APIError{Code: Internal, Message: err.Error()}
		}
		if id != "" {
			wt, err := trace.OpenWorkgroupAuthority(s.Current().Root, id)
			if wt != nil {
				defer wt.Close()
			}
			if err != nil {
				return PacketResponse{}, &APIError{Code: Internal, Message: err.Error()}
			}
			spec, revision, found, err := trace.ProjectSpecWithRevisionFor(wt, id)
			if err != nil {
				return PacketResponse{}, &APIError{Code: Internal, Message: err.Error()}
			}
			if found {
				out.ProjectSpec, out.ProjectSpecRevision = &spec, revision
				g, _, err := repository.CrossRepoWorkgroup(id)
				if err != nil {
					return PacketResponse{}, &APIError{Code: Internal, Message: err.Error()}
				}
				approval, _, approved, err := trace.ReadGroupApproval(wt, g.Members[0])
				if err != nil {
					return PacketResponse{}, &APIError{Code: Internal, Message: err.Error()}
				}
				if approved {
					out.ProjectSpec, out.ProjectSpecRevision = &approval.ProjectSpec, approval.ProjectSpecRevision
					if out.ProjectSpecRevision == 0 {
						revision, err := trace.RecordedProjectSpecRevision(wt, id, approval.ProjectSpec)
						if err != nil {
							return PacketResponse{}, &APIError{Code: Internal, Message: err.Error()}
						}
						out.ProjectSpecRevision = revision
					}
				}
			}
		}
	}
	return out, nil
}

// Ratify records the owner's approval of the exact revisions of the spec and
// the plan they read in the packet. It changes no state of its own: the
// ratification is the record, the record asks for the sealing, and the
// sealing moves the workstream on. For a workstream whose workgroup spans
// more than one repo, this one call instead seals the workgroup's whole
// project spec: every real member is ratified at its own current packet
// pin, all at once, refusing before recording anything if the graph is
// invalid or a member has no slice. A one-workstream workgroup keeps
// exactly today's single-workstream behaviour.
func (s *Controller) Ratify(ctx context.Context, raw string, req RatifyRequest) (RatifyResponse, *APIError) {
	return s.RatifyBy(ctx, raw, req, Deciding{})
}

// RatifyBy is Ratify as the given decider: the owner, or Jev deciding the
// ratification gate of a workstream whose ratification is its own alone. A
// workstream that belongs to a cross-repo workgroup needs the owner's group
// approval, which Jev does not give.
func (s *Controller) RatifyBy(ctx context.Context, raw string, req RatifyRequest, d Deciding) (RatifyResponse, *APIError) {
	if req.Spec < 1 || req.Plan < 1 && !(req.Plan == 0 && s.planless(raw)) {
		return RatifyResponse{}, &APIError{Code: Validation, Message: "ratify the revisions of the spec and the plan the packet named"}
	}
	_, stream, repo, api := s.DesignMemberTrace(raw)
	if api != nil {
		return RatifyResponse{}, api
	}
	if kind, err := StreamKind(repo, stream); err == nil && !kind.Documents.Planned() && req.Plan != 0 {
		return RatifyResponse{}, &APIError{Code: Validation, Message: fmt.Sprintf("a %s workstream ratifies its climb spec revision alone and has no plan revision", kind.Name)}
	}
	id, err := repo.CrossRepoWorkgroupOf(stream)
	if err != nil {
		return RatifyResponse{}, &APIError{Code: Internal, Message: err.Error()}
	}
	if id != "" && d.Jev() {
		return RatifyResponse{}, &APIError{Code: Conflict, Message: fmt.Sprintf("workstream %s belongs to workgroup %s; the owner gives its group approval", stream, id)}
	}
	if id != "" {
		required, err := repo.GroupApprovalRequired(stream)
		if err != nil {
			return RatifyResponse{}, &APIError{Code: Internal, Message: err.Error()}
		}
		if required && req.ProjectSpecRevision < 1 {
			return RatifyResponse{}, &APIError{Code: Conflict, Message: ProjectSpecCatchingUp}
		}
		out, found, api := s.ResumeGroupApproval(ctx, repo, stream, id, shed.Pin{Spec: req.Spec, Plan: req.Plan}, req.ProjectSpecRevision)
		if found {
			return out, api
		}
	}
	o, api := s.ShedAction(string(stream), InShedState, SketchedState)
	if api != nil {
		return RatifyResponse{}, api
	}
	workgroup, err := o.Repository.CrossRepoWorkgroupOf(o.Stream)
	if err != nil {
		return RatifyResponse{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot read the cross-repo workgroup of workstream %s; check the trace repository", o.Stream)}
	}
	asked := shed.Pin{Spec: req.Spec, Plan: req.Plan}
	required, err := o.Repository.GroupApprovalRequired(o.Stream)
	if err != nil {
		return RatifyResponse{}, &APIError{Code: Internal, Message: err.Error()}
	}
	if !required {
		return s.ratifyMember(ctx, o, asked, d)
	}
	if d.Jev() {
		return RatifyResponse{}, &APIError{Code: Conflict, Message: fmt.Sprintf("workstream %s needs the owner's group approval", o.Stream)}
	}
	return s.RatifyWorkgroup(ctx, o, workgroup, asked, req.ProjectSpecRevision)
}

// RatifyMember ratifies one workstream at the given pin, so each member of a
// workgroup can be ratified at its own current pin.
func (s *Controller) RatifyMember(ctx context.Context, o *ShedOwner, asked shed.Pin) (RatifyResponse, *APIError) {
	return s.ratifyMember(ctx, o, asked, Deciding{})
}

func (s *Controller) ratifyMember(ctx context.Context, o *ShedOwner, asked shed.Pin, d Deciding) (RatifyResponse, *APIError) {
	latest, ratified, err := LatestRatification(o.Repository, o.Stream)
	if err != nil {
		return RatifyResponse{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot read the ratifications of workstream %s; check the trace repository", o.Stream)}
	}
	if ratified && latest.Revision == asked {
		return s.Sealing(ctx, o, latest, fmt.Sprintf("workstream %s is ratified at %s already", o.Stream, asked))
	}
	entries, err := Dissent(o.Repository, o.Stream)
	if err != nil {
		return RatifyResponse{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot read the dissent record of workstream %s; check the trace repository", o.Stream)}
	}
	refusals, err := s.Refusals(ctx, o, asked, entries)
	if err != nil {
		return RatifyResponse{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot read the spec and plan of workstream %s; check the trace repository", o.Stream)}
	}
	if len(refusals) > 0 {
		return RatifyResponse{}, &APIError{Code: Conflict, Message: fmt.Sprintf("workstream %s is not ratified:\n- %s", o.Stream, strings.Join(refusals, "\n- "))}
	}
	record := shed.Ratify(o.Round, asked, entries)
	record.DecidedBy, record.Judgment = d.By(), d.Judgment
	content, err := shed.EncodeRatification(record)
	if err != nil {
		return RatifyResponse{}, &APIError{Code: Validation, Message: "the ratification cannot be recorded: " + err.Error()}
	}
	o.Deciding = d
	reason := fmt.Sprintf("%s ratified %s after round %d", d.Name(), asked, o.Round)
	if len(record.Dispositions) > 0 {
		reason += fmt.Sprintf(", over %s the owner disposed of", shed.Objections(len(record.Dispositions)))
	}
	if dismissed := len(entries) - len(shed.ForOwner(entries)); dismissed > 0 {
		reason += fmt.Sprintf(", with %s the chief of staff dismissed", shed.Objections(dismissed))
	}
	if api := s.recordRatification(ctx, o, d, string(content), reason); api != nil {
		return RatifyResponse{}, api
	}
	return RatifyResponse{Project: o.Project, Workstream: o.Stream, Round: record.Round, Spec: asked.Spec, Plan: asked.Plan, Sealing: "requested", Detail: reason + "; the sealing is asked for"}, nil
}

func (s *Controller) recordRatification(ctx context.Context, o *ShedOwner, d Deciding, content, reason string) *APIError {
	at := o.Now()
	id := shed.RatificationDocumentID(o.Round)
	doc, err := ownerShedDocument(o.Repository, o.Project, o.Stream, id, shed.RatificationPath(o.Round), content, at, d.Actor())
	if err != nil {
		return &APIError{Code: Internal, Message: fmt.Sprintf("cannot record the owner's shed action on workstream %s; check the trace repository", o.Stream)}
	}
	transition := fmt.Sprintf("%s-%d", id, doc.Revision)
	value := fmt.Sprintf("ratified-%d", o.Round)
	res, err := s.route(ctx, PinnedMove{
		Repo: o.Repository, Stream: o.Stream, Subject: OwnerSubject, State: o.Owner,
		Trigger: TriggerRatified, Cause: fmt.Sprintf("%s@%d", transition, at.UnixNano()),
		Target: TargetRatified, Enters: value, Description: routeRatifiedDescription, Gate: true,
		Jev: d.Judgment, Ref: transition, ID: transition, HeaderCause: "owner-shed", Reason: reason, Actor: d.Actor(), At: at,
		Documents: []trace.Document{doc}, Events: []trace.Event{trace.Notice(transition, "owner", reason)},
	})
	return RouteError(res, err, fmt.Sprintf("the ratification of workstream %s", o.Stream))
}

const ProjectSpecCatchingUp = "the project spec is catching up with the latest member revisions; read the packet again"

// RatifyWorkgroup validates the entire draft before recording one durable
// owner decision. Member decisions and seals are preparations; only durable
// group completion grants effective ratification and production admission.
func (s *Controller) RatifyWorkgroup(ctx context.Context, o *ShedOwner, id config.WorkgroupID, asked shed.Pin, projectSpecRevision int) (RatifyResponse, *APIError) {
	lock := s.GroupApprovalLock(id)
	lock.Lock()
	defer lock.Unlock()
	failed := &APIError{Code: Internal, Message: fmt.Sprintf("cannot read the project spec of workgroup %s; check the trace repository", id)}
	wt, g, err := s.ApprovalTrace(ctx, o.Repository, id)
	if err != nil {
		return RatifyResponse{}, failed
	}
	defer wt.Close()
	spec, revision, found, err := trace.ProjectSpecWithRevisionFor(wt, id)
	if err != nil {
		return RatifyResponse{}, failed
	}
	if !found || len(g.Members) < 2 {
		return RatifyResponse{}, &APIError{Code: Conflict, Message: fmt.Sprintf("workgroup %s has no recorded project spec yet; the architect has not finished drafting it", id)}
	}
	if _, _, approved, err := trace.ReadGroupApproval(wt, g.Members[0]); err != nil {
		return RatifyResponse{}, failed
	} else if !approved {
		message, err := s.ContractGuardRefusal(wt, g, spec)
		if err != nil {
			return RatifyResponse{}, failed
		}
		if message != "" {
			return RatifyResponse{}, &APIError{Code: Conflict, Message: message}
		}
	}
	if projectSpecRevision < 1 || projectSpecRevision != revision {
		return RatifyResponse{}, &APIError{Code: Conflict, Message: ProjectSpecCatchingUp}
	}
	real := g.Members[1:]
	if err := trace.ValidateGraph(real, spec.Graph); err != nil {
		return RatifyResponse{}, &APIError{Code: Validation, Message: fmt.Sprintf("workgroup %s's project spec graph is invalid: %v", id, err)}
	}
	for _, m := range real {
		if _, ok := trace.SliceFor(spec, m); !ok {
			return RatifyResponse{}, &APIError{Code: Conflict, Message: fmt.Sprintf("workgroup %s's project spec does not cover every member: workstream %s has no slice", id, m)}
		}
	}
	owners := make(map[config.WorkstreamID]*ShedOwner, len(real))
	pins := make(map[config.WorkstreamID]shed.Pin, len(real))
	var refused []string
	for _, m := range real {
		mo, api := s.ShedAction(string(m), InShedState, SketchedState)
		if api != nil {
			refused = append(refused, fmt.Sprintf("workstream %s: %s", m, api.Message))
			continue
		}
		pin := mo.Pin
		if m == o.Stream {
			pin = asked
		}
		owners[m], pins[m] = mo, pin
		entries, err := Dissent(mo.Repository, mo.Stream)
		if err != nil {
			return RatifyResponse{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot read the dissent record of workstream %s; check the trace repository", mo.Stream)}
		}
		memberRefusals, err := s.Refusals(ctx, mo, pin, entries)
		if err != nil {
			return RatifyResponse{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot read the spec and plan of workstream %s; check the trace repository", mo.Stream)}
		}
		for _, r := range memberRefusals {
			refused = append(refused, fmt.Sprintf("workstream %s: %s", m, r))
		}
		if len(memberRefusals) > 0 {
			continue
		}
		matches, err := SliceMatchesPin(spec, mo.Repository, m, pin)
		if err != nil {
			return RatifyResponse{}, &APIError{Code: Internal, Message: err.Error()}
		}
		if !matches {
			refused = append(refused, fmt.Sprintf("workstream %s: %s", m, ProjectSpecCatchingUp))
		}
	}
	if len(refused) > 0 {
		return RatifyResponse{}, &APIError{Code: Conflict, Message: fmt.Sprintf("workgroup %s is not ratified:\n- %s", id, strings.Join(refused, "\n- "))}
	}
	if err := s.Step("group-ratify-validated"); err != nil {
		return RatifyResponse{}, &APIError{Code: Internal, Message: err.Error()}
	}
	decision, _, exists, err := trace.ReadGroupApproval(wt, g.Members[0])
	if err != nil {
		return RatifyResponse{}, &APIError{Code: Internal, Message: err.Error()}
	}
	if exists {
		matched := slices.ContainsFunc(decision.Members, func(m trace.GroupMemberApproval) bool {
			return m.Workstream == o.Stream && m.Spec == asked.Spec && m.Plan == asked.Plan
		})
		if !matched {
			return RatifyResponse{}, &APIError{Code: Conflict, Message: "the request differs from the immutable workgroup approval"}
		}
	}
	if !exists {
		decision = trace.GroupApproval{Workgroup: id, ProjectSpec: spec, ProjectSpecRevision: revision}
		for _, m := range real {
			mo, pin := owners[m], pins[m]
			entries, err := Dissent(mo.Repository, m)
			if err != nil {
				return RatifyResponse{}, &APIError{Code: Internal, Message: err.Error()}
			}
			record := shed.Ratify(mo.Round, pin, entries)
			content, err := shed.EncodeRatification(record)
			if err != nil {
				return RatifyResponse{}, &APIError{Code: Internal, Message: err.Error()}
			}
			decision.Members = append(decision.Members, trace.GroupMemberApproval{Project: mo.Project, Workstream: m, Spec: pin.Spec, Plan: pin.Plan, Round: mo.Round, Record: content})
		}
		if err := trace.RecordGroupApproval(ctx, wt, g.Members[0], decision, s.Now(), OwnerActor); err != nil {
			return RatifyResponse{}, &APIError{Code: Internal, Message: err.Error()}
		}
	}
	if err := s.Step("group-decision-recorded"); err != nil {
		return RatifyResponse{}, &APIError{Code: Internal, Message: err.Error()}
	}
	if err := s.PrepareGroupApproval(ctx, decision); err != nil {
		return RatifyResponse{}, &APIError{Code: Internal, Message: err.Error()}
	}
	return GroupApprovalResponse(o.Project, o.Stream, decision), nil
}

// Sealing reports the state of the sealing a ratification's record asked
// for: requested until the controller publishes it, then pending or running.
// One that failed is asked for again by recording the ratification again as
// its next revision, which the controller seals afresh.
func (s *Controller) Sealing(ctx context.Context, o *ShedOwner, r Ratification, detail string) (RatifyResponse, *APIError) {
	out := RatifyResponse{Project: o.Project, Workstream: o.Stream, Round: r.Round, Spec: r.Revision.Spec, Plan: r.Revision.Plan, Sealing: "requested", Detail: detail + "; the sealing is asked for"}
	z := &Sealer{S: s.SealerDependencies(), Repository: o.Repository}
	failed := &APIError{Code: Internal, Message: fmt.Sprintf("cannot read the sealings of workstream %s; check the trace repository", o.Stream)}
	op, requested, err := z.Sealing(o.Stream, r)
	if err != nil {
		return RatifyResponse{}, failed
	}
	if !requested {
		return out, nil
	}
	in, err := DecodeSeal(op.Operation)
	if err != nil {
		return RatifyResponse{}, failed
	}
	if op.Result == nil {
		state, reason, _ := Progress(op)
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
	o.Deciding = decidingOf(r.DecidedBy, r.Judgment)
	reason := fmt.Sprintf("%s ratified %s after round %d again; sealing %d failed and the sealing is asked for again", o.Deciding.Name(), r.Revision, r.Round, in.Seal)
	if api := o.Record(ctx, shed.RatificationDocumentID(r.Round), shed.RatificationPath(r.Round), string(content), fmt.Sprintf("ratified-%d", r.Round), reason); api != nil {
		return RatifyResponse{}, api
	}
	out.Detail = fmt.Sprintf("%s; sealing %d failed and the sealing is asked for again", detail, in.Seal)
	return out, nil
}

// Refusals lists every reason the workstream cannot be ratified at the given
// revisions: a blocking objection the owner has not disposed of, a plan that
// does not validate, revisions that are not the current ones, and a workstream
// that is not at its decision point. Every reason that applies is listed, so
// one call tells the owner everything they have to settle.
func (s *Controller) Refusals(ctx context.Context, o *ShedOwner, asked shed.Pin, entries []shed.Entry) ([]string, error) {
	var out []string
	if o.Feature == SketchedState && !o.Skipped {
		out = append(out, fmt.Sprintf("the workstream is %s and debate is not skipped: it is ratified in the shed", SketchedState))
	}
	if asked != o.Pin {
		out = append(out, fmt.Sprintf("%s are ratified, and the current revisions are %s: read the packet again", asked, o.Pin))
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
	problems, err := s.PlanProblems(ctx, o)
	if err != nil {
		return nil, err
	}
	noun := "the plan"
	if kind, err := StreamKind(o.Repository, o.Stream); err == nil && !kind.Documents.Planned() {
		noun = "the climb spec"
	}
	for _, p := range problems {
		out = append(out, noun+" is not valid: "+p)
	}
	return out, nil
}

// PlanProblems validates the workstream's latest recorded spec and plan, the
// revisions a ratification approves, exactly as the architect's draft is
// validated.
func (s *Controller) PlanProblems(ctx context.Context, o *ShedOwner) ([]string, error) {
	dr := &Drafter{S: s.DrafterDependencies(), Repository: o.Repository}
	latest, err := dr.Latest(o.Stream)
	if err != nil {
		return nil, err
	}
	kind, err := StreamKind(o.Repository, o.Stream)
	if err != nil {
		return nil, err
	}
	if !kind.Documents.Planned() {
		doc, drafted := latest[kind.Documents.Drafted[0]]
		if !drafted {
			return nil, errors.New("the workstream has no recorded climb spec")
		}
		return ClimbProblems(doc.Content, s.Current()), nil
	}
	spec, drafted := latest[plan.SpecDocument]
	graph, planned := latest[plan.PlanDocument]
	if !drafted || !planned {
		return nil, errors.New("the workstream has no recorded spec and plan")
	}
	s.ReseedEntities(ctx, o.Repository)
	return dr.Validate(spec, graph)
}

// planless reports whether the workstream's kind ratifies no plan.
func (s *Controller) planless(raw string) bool {
	_, stream, repo, api := s.DesignMemberTrace(raw)
	if api != nil {
		return false
	}
	kind, err := StreamKind(repo, stream)
	return err == nil && !kind.Documents.Planned()
}
