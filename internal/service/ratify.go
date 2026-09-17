package service

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/kb"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
)

// Sealing is what a recorded ratification triggers: the seal of the ratified
// spec and plan, and everything the workstream needs to start building. The
// gate records the owner's approval and calls it; it owns no state of its own.
type Sealing interface {
	Seal(ctx context.Context, project config.ProjectID, stream config.WorkstreamID, revision shed.Pin) error
}

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
	revision := 0
	for _, doc := range documents {
		if doc.Path == path {
			if doc.Content == string(content) {
				return nil
			}
			revision = doc.Revision
		}
	}
	doc := trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: shed.PacketDocumentID(packet.Round), Revision: revision + 1,
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

// packet serves the workstream's recorded ratification packet.
func (s *Service) packet(raw string) (PacketResponse, *APIError) {
	project, stream, repository, api := s.conversationTrace(raw)
	if api != nil {
		return PacketResponse{}, api
	}
	documents, err := trace.Read[trace.Document](repository, stream)
	if err != nil {
		return PacketResponse{}, &APIError{Internal, fmt.Sprintf("cannot read the ratification packet of workstream %s; check the trace repository", stream)}
	}
	var latest trace.Document
	round := 0
	for _, doc := range documents {
		if at, ok := shed.PacketRound(doc.Path); ok && at >= round {
			latest, round = doc, at
		}
	}
	if latest.Path == "" {
		return PacketResponse{}, &APIError{NotFound, fmt.Sprintf("workstream %s has no ratification packet; the chief of staff presents one once debate concludes or the owner skips it", stream)}
	}
	packet, err := shed.ParsePacket([]byte(latest.Content))
	if err != nil {
		return PacketResponse{}, &APIError{Internal, fmt.Sprintf("the ratification packet of workstream %s is not readable; check the trace repository", stream)}
	}
	return PacketResponse{Project: project, Workstream: stream, Packet: packet, Revision: latest.Revision, At: latest.At}, nil
}

// ratify records the owner's approval of the exact revisions of the spec and
// the plan they read in the packet, and triggers the sealing operation. It
// changes no state of its own: the ratification is the record, and sealing
// moves the workstream on.
func (s *Service) ratify(ctx context.Context, raw string, req RatifyRequest) (RatifyResponse, *APIError) {
	if req.Spec < 1 || req.Plan < 1 {
		return RatifyResponse{}, &APIError{Validation, "ratify the revisions of the spec and the plan the packet named"}
	}
	o, api := s.shedAction(raw, InShedState, SketchedState)
	if api != nil {
		return RatifyResponse{}, api
	}
	ratified, err := shed.Ratifications(o.repository, o.stream)
	if err != nil {
		return RatifyResponse{}, &APIError{Internal, fmt.Sprintf("cannot read the ratifications of workstream %s; check the trace repository", o.stream)}
	}
	asked := shed.Pin{Spec: req.Spec, Plan: req.Plan}
	if slices.ContainsFunc(ratified, func(r shed.Ratification) bool { return r.Revision == asked }) {
		return RatifyResponse{}, &APIError{Conflict, fmt.Sprintf("workstream %s is already ratified at %s", o.stream, asked)}
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
	if api := o.record(ctx, shed.RatificationDocumentID(o.round), shed.RatificationPath(o.round), string(content), fmt.Sprintf("ratified-%d", o.round), reason); api != nil {
		return RatifyResponse{}, api
	}
	out := RatifyResponse{Project: o.project, Workstream: o.stream, Round: o.round, Spec: asked.Spec, Plan: asked.Plan, Detail: reason}
	if s.options.Sealing == nil {
		return out, nil
	}
	if err := s.options.Sealing.Seal(ctx, o.project, o.stream, asked); err != nil {
		return out, &APIError{Internal, fmt.Sprintf("%s is ratified for workstream %s and the sealing did not start; check osmia status", asked, o.stream)}
	}
	out.Sealed = true
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
// revisions a ratification approves.
func (s *Service) planProblems(o *shedOwner) ([]string, error) {
	documents, err := trace.Read[trace.Document](o.repository, o.stream)
	if err != nil {
		return nil, err
	}
	latest := map[string]trace.Document{}
	for _, doc := range documents {
		latest[doc.ID] = doc
	}
	spec, ok := latest[plan.SpecDocument]
	graph, drafted := latest[plan.PlanDocument]
	if !ok || !drafted {
		return nil, errors.New("the workstream has no recorded spec and plan")
	}
	entities, err := kb.Load(o.repository)
	if err != nil {
		return nil, err
	}
	return validateDraft(spec.Content, graph.Content, entities), nil
}
