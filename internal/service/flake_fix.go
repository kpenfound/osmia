package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/flakes"
	"github.com/kpenfound/osmia/internal/ghlog"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/trace"
)

const maxFlakeNotes = 8 << 10

type flakeFixResult struct {
	Action     string              `json:"action"`
	Occurrence string              `json:"occurrence"`
	Group      string              `json:"group"`
	Decision   string              `json:"decision"`
	Workstream config.WorkstreamID `json:"workstream,omitempty"`
	Reason     string              `json:"reason,omitempty"`
	Next       string              `json:"next"`
}

func (c *runtimeControls) flakeFix(repository *trace.Repository, scope coreadapter.Scope) coreadapter.Tool {
	tool := coreadapter.Tool{Name: flakeFixToolName, Effect: coreadapter.ToolMemory,
		Description: "Act on a flake occurrence a flake notice listed: open a flake-fix workstream that finds and fixes the cause of the flakiness, or decline it. " +
			"action: open or decline. occurrence: the occurrence ID, exactly as the notice gives it. " +
			"notes (open only): your own notes for the workstream's hand-in, such as where you suspect the cause; the service writes the rest of the hand-in from the recorded evidence, and you cannot add other content. " +
			"reason (decline only, required): why the flake needs no workstream, for example the flaky test was added by the unit itself; it is recorded and shown in the originating workstream. " +
			"workstream: the originating workstream when it is a member of your workgroup other than this conversation's own. " +
			"An occurrence is decided once: calling the tool again for it returns its decision and does nothing else, even after its workstream was delivered or abandoned. " +
			"When the occurrence's group already has an open flake-fix workstream, open adds the occurrence to it and tells that workstream's chief of staff instead of opening another. " +
			"The tool refuses when the project has flake-fix workstreams off, for an unknown occurrence, and for a run that belongs to a flake-fix workstream.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"action":{"type":"string","enum":["open","decline"]},"occurrence":{"type":"string"},"notes":{"type":"string"},"reason":{"type":"string"},"workstream":{"type":"string"}},"required":["action","occurrence"],"additionalProperties":false}`)}
	tool.Handle = func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
		var input struct {
			Action     string              `json:"action"`
			Occurrence string              `json:"occurrence"`
			Notes      string              `json:"notes"`
			Reason     string              `json:"reason"`
			Workstream config.WorkstreamID `json:"workstream"`
		}
		refuse := func(reason string) (json.RawMessage, error) { return priorityRefusal(reason) }
		d := json.NewDecoder(bytes.NewReader(raw))
		d.DisallowUnknownFields()
		if err := d.Decode(&input); err != nil {
			if strings.Contains(err.Error(), "unknown field") {
				return refuse(fmt.Sprintf("%s takes only action, occurrence, notes, reason and workstream; it cannot hand in other content (%v)", flakeFixToolName, err))
			}
			return nil, fmt.Errorf("tool input: %w", err)
		}
		if err := d.Decode(new(any)); err != io.EOF {
			return nil, errors.New("tool input must be one object")
		}
		s := c.service.Load()
		if s == nil {
			return nil, errors.New("flake-fix workstreams are unavailable")
		}
		if err := repository.ActiveTurn(scope); err != nil {
			return nil, err
		}
		target, stream, refusal, err := repository.ManagerMember(config.WorkstreamID(scope.Workstream), input.Workstream)
		if err != nil {
			return nil, err
		}
		if refusal != "" {
			return refuse(refusal)
		}
		occurrence, notes, reason := strings.TrimSpace(input.Occurrence), strings.TrimSpace(input.Notes), strings.TrimSpace(input.Reason)
		switch {
		case input.Action != "open" && input.Action != "decline":
			return refuse(fmt.Sprintf("action must be open or decline, not %q", input.Action))
		case occurrence == "":
			return refuse("an occurrence is required: the ID the flake notice gives")
		case input.Action == "decline" && reason == "":
			return refuse("declining needs a reason, which is recorded in the originating workstream")
		case input.Action == "decline" && notes != "":
			return refuse("notes belong to open; decline takes a reason")
		case input.Action == "open" && reason != "":
			return refuse("a reason belongs to decline; open takes notes")
		case len(notes) > maxFlakeNotes:
			return refuse(fmt.Sprintf("notes are longer than %d bytes", maxFlakeNotes))
		}
		res, why, err := s.actOnFlake(ctx, target, stream, input.Action, occurrence, notes, reason)
		if err != nil {
			return nil, err
		}
		if why != "" {
			return refuse(why)
		}
		return json.Marshal(res)
	}
	return tool
}

type flakeFixLookup struct {
	group      flakes.Group
	occurrence flakes.Occurrence
}

func findFlakeOccurrence(store *flakes.Store, id string) (flakeFixLookup, bool, error) {
	groups, err := store.Groups()
	if err != nil {
		return flakeFixLookup{}, false, err
	}
	for _, g := range groups {
		for _, o := range g.Occurrences {
			if o.ID == id {
				return flakeFixLookup{group: g, occurrence: o}, true, nil
			}
		}
	}
	return flakeFixLookup{}, false, nil
}

func (s *Service) flakeStore(repository *trace.Repository) *flakes.Store {
	return &flakes.Store{Repository: repository, Actor: managerActor, Now: s.now}
}

func (s *Service) actOnFlake(ctx context.Context, repository *trace.Repository, stream config.WorkstreamID, action, id, notes, reason string) (flakeFixResult, string, error) {
	cfg := s.about(repository)
	if !cfg.Project.FlakeFixes {
		return flakeFixResult{}, fmt.Sprintf("flake-fix workstreams are off for project %s; the owner turns them on in the project's settings", repository.Project()), nil
	}
	s.flakeFixMu.Lock()
	defer s.flakeFixMu.Unlock()
	store := s.flakeStore(repository)
	found, ok, err := findFlakeOccurrence(store, id)
	if err != nil {
		return flakeFixResult{}, "", err
	}
	switch {
	case !ok:
		return flakeFixResult{}, fmt.Sprintf("there is no recorded flake occurrence %s in project %s", id, repository.Project()), nil
	case found.occurrence.Source.Workstream != stream:
		return flakeFixResult{}, fmt.Sprintf("flake occurrence %s belongs to workstream %s; name it as the workstream input", id, found.occurrence.Source.Workstream), nil
	case found.occurrence.FromFlakeFix:
		return flakeFixResult{}, fmt.Sprintf("flake occurrence %s came from a flake-fix workstream's own run; it opens nothing", id), nil
	}
	group, run := found.group.Group, found.occurrence.Run
	decision := found.occurrence.Decision
	if decision.State == flakes.Pending {
		if action == "decline" {
			decision, err = store.Decline(ctx, group, run, reason)
		} else {
			decision, err = store.ReserveWithNotes(ctx, group, run, notes)
		}
		if err != nil {
			return flakeFixResult{}, "", err
		}
	}
	if decision.State == flakes.Opening {
		if _, why, err := s.completeFlakeOpening(ctx, repository, store, group, run); err != nil || why != "" {
			return flakeFixResult{}, why, err
		}
	}
	if err := s.announceFlakeDecision(ctx, repository, store, group, run); err != nil {
		ghlog.Logger(ctx).Warn("the flake decision will be announced by the next flake pass", "occurrence", id, "error", err.Error())
	}
	found, ok, err = findFlakeOccurrence(store, id)
	if err != nil || !ok {
		return flakeFixResult{}, "", errors.Join(err, fmt.Errorf("flake occurrence %s is no longer recorded", id))
	}
	d := found.occurrence.Decision
	res := flakeFixResult{Action: action, Occurrence: id, Group: group, Decision: d.State, Workstream: d.Workstream, Reason: d.Reason}
	switch d.State {
	case flakes.Opened:
		res.Next = fmt.Sprintf("Flake-fix workstream %s was handed in for group %s. It goes through the architect, the committee and the owner's ratification like any hand-in; the originating workstream carries on regardless.", d.Workstream, group)
	case flakes.Joined:
		res.Next = fmt.Sprintf("Group %s already has flake-fix workstream %s open. The occurrence joined it and its chief of staff has the evidence; nothing was opened.", group, d.Workstream)
	case flakes.Declined:
		res.Next = "The occurrence is declined and its reason is recorded in the originating workstream. A later flake in the group notifies you again."
	default:
		res.Next = "The occurrence is not decided yet; call the tool again."
	}
	if (action == "open" && d.State == flakes.Declined) || (action == "decline" && d.State != flakes.Declined) {
		res.Next = fmt.Sprintf("The occurrence was already decided (%s), so %s did nothing. %s", d.State, action, res.Next)
	}
	return res, "", nil
}

func (s *Service) completeFlakeOpening(ctx context.Context, repository *trace.Repository, store *flakes.Store, group, run string) (flakes.Decision, string, error) {
	g, found, err := store.Group(group)
	if err != nil || !found {
		return flakes.Decision{}, "", errors.Join(err, fmt.Errorf("flake group %q is not recorded", group))
	}
	o, ok := g.Find(run)
	if !ok {
		return flakes.Decision{}, "", flakes.ErrUnknownOccurrence
	}
	d := o.Decision
	if d.State != flakes.Opening {
		return d, "", nil
	}
	if d.Key == "" || d.Workstream == "" {
		return flakes.Decision{}, "", fmt.Errorf("the reserved opening of flake occurrence %s has no hand-in key", o.ID)
	}
	if _, archived := s.archivedWorkstream(string(d.Workstream)); !archived {
		existing, text, err := flakeHandIn(repository, d.Workstream)
		if err != nil {
			return flakes.Decision{}, "", err
		}
		if !existing.handed {
			if text == "" {
				if text, err = flakes.RenderHandIn(g, run, d.Notes); err != nil {
					return flakes.Decision{}, "", err
				}
			}
			origin := &intake.HandInOrigin{Actor: managerActor, Reason: flakeHandInReason(o, group)}
			out, apiErr := s.intake().HandInAs(ctx, api.HandInRequest{Project: repository.Project(), Key: d.Key, Stdin: &text}, origin)
			if apiErr != nil {
				switch apiErr.Code {
				case intake.Internal, intake.Conflict:
					return flakes.Decision{}, "", fmt.Errorf("handing in flake-fix workstream %s: %s", d.Workstream, apiErr.Message)
				}
				return flakes.Decision{}, fmt.Sprintf("the hand-in of the flake-fix workstream could not be made yet: %s. The occurrence stays reserved; call the tool again", apiErr.Message), nil
			}
			if out.Workstream != d.Workstream {
				return flakes.Decision{}, "", fmt.Errorf("the hand-in made workstream %s, not the reserved %s", out.Workstream, d.Workstream)
			}
		}
	}
	if err := s.step("flake-fix-handed"); err != nil {
		return flakes.Decision{}, "", err
	}
	decided, err := store.Opened(ctx, group, run, d.Workstream)
	return decided, "", err
}

type flakeHandInState struct {
	handed bool
}

func flakeHandIn(repository *trace.Repository, workstream config.WorkstreamID) (flakeHandInState, string, error) {
	streams, err := repository.Workstreams()
	if err != nil {
		return flakeHandInState{}, "", err
	}
	if !slices.Contains(streams, workstream) {
		return flakeHandInState{}, "", nil
	}
	transitions, err := trace.Read[trace.Transition](repository, workstream)
	if err != nil {
		return flakeHandInState{}, "", err
	}
	state := flakeHandInState{handed: slices.ContainsFunc(transitions, func(t trace.Transition) bool { return t.ID == intake.HandInTransition })}
	docs, err := trace.Read[trace.Document](repository, workstream)
	if err != nil {
		return flakeHandInState{}, "", err
	}
	for _, d := range docs {
		if d.ID == intake.HandedDocument {
			return state, d.Content, nil
		}
	}
	return state, "", nil
}

func flakeHandInReason(o flakes.Occurrence, group string) string {
	origin := flakeRun{stream: o.Source.Workstream, source: o.Source}
	return fmt.Sprintf("the chief of staff opened this flake-fix workstream from flake occurrence %s of group %s, recorded by %s in %s", o.ID, group, origin.describe(), o.RunPath())
}

func flakeDecisionSubject(occurrence string) string {
	return "flake-decision-" + flakeDigest("decision", occurrence)
}

func (s *Service) announceFlakeDecision(ctx context.Context, repository *trace.Repository, store *flakes.Store, group, run string) error {
	g, found, err := store.Group(group)
	if err != nil || !found {
		return errors.Join(err, fmt.Errorf("flake group %q is not recorded", group))
	}
	o, ok := g.Find(run)
	if !ok {
		return flakes.ErrUnknownOccurrence
	}
	if !o.Decision.Final() {
		return nil
	}
	return errors.Join(s.announceToOrigin(ctx, repository, store, group, o), s.announceToTarget(ctx, repository, store, group, o))
}

func flakeDecisionText(o flakes.Occurrence, group string) (to, reason string) {
	switch o.Decision.State {
	case flakes.Opened:
		return "opened", fmt.Sprintf("the chief of staff opened flake-fix workstream %s for flake occurrence %s of group %s (%s)", o.Decision.Workstream, o.ID, group, codeList(o.Links))
	case flakes.Joined:
		return "joined", fmt.Sprintf("flake occurrence %s of group %s joined the open flake-fix workstream %s (%s)", o.ID, group, o.Decision.Workstream, codeList(o.Links))
	}
	return "declined", fmt.Sprintf("the chief of staff declined flake occurrence %s of group %s (%s): %s", o.ID, group, codeList(o.Links), o.Decision.Reason)
}

func (s *Service) closeFlakeDestination(ctx context.Context, store *flakes.Store, group string, o flakes.Occurrence, destination string, workstream config.WorkstreamID, archive runtime.Archive) error {
	by := archivedBy(archive)
	who := "the owner"
	if by == api.ArchivedByManager {
		who = "the manager, through archive-on-merge"
	}
	note := fmt.Sprintf("closed as moot: workstream %s was archived by %s, which deletes the trace and feed this announcement of the %s decision would be written to", workstream, who, o.Decision.State)
	return store.MarkMoot(ctx, group, o.Run, flakes.Moot{Destination: destination, Workstream: workstream, ArchivedBy: trace.StoredRole(by), Note: note})
}

func (s *Service) announceToOrigin(ctx context.Context, repository *trace.Repository, store *flakes.Store, group string, o flakes.Occurrence) error {
	if o.IsAnnounced(flakes.DestinationOrigin) || o.IsMoot(flakes.DestinationOrigin) {
		return nil
	}
	origin := o.Source.Workstream
	if archive, archived := s.archivedWorkstream(string(origin)); archived {
		return s.closeFlakeDestination(ctx, store, group, o, flakes.DestinationOrigin, origin, archive)
	}
	if err := s.step("flake-announce-origin " + o.ID); err != nil {
		return fmt.Errorf("announcing flake occurrence %s in workstream %s: %w", o.ID, origin, err)
	}
	subject := flakeDecisionSubject(o.ID)
	streams, err := repository.Workstreams()
	if err != nil {
		return err
	}
	if !slices.Contains(streams, origin) {
		return fmt.Errorf("announcing flake occurrence %s: workstream %s has no trace", o.ID, origin)
	}
	state, err := repository.Workflow(origin, subject)
	if err != nil {
		return err
	}
	if state.Value == "" {
		to, reason := flakeDecisionText(o, group)
		h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: subject, Revision: 1, Project: repository.Project(), Workstream: origin, At: s.now(), Actor: managerActor, Cause: flakeFixToolName}
		tx := trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: h, Subject: subject, From: "", To: to, Reason: reason}}
		if _, err := repository.Transact(ctx, tx); err != nil {
			if !errors.Is(err, trace.ErrConflict) {
				return fmt.Errorf("announcing flake occurrence %s in workstream %s: %w", o.ID, origin, err)
			}
			if state, err = repository.Workflow(origin, subject); err != nil || state.Value == "" {
				return errors.Join(err, fmt.Errorf("announcing flake occurrence %s in workstream %s: the trace changed concurrently", o.ID, origin))
			}
		}
	}
	return store.MarkAnnounced(ctx, group, o.Run, flakes.DestinationOrigin)
}

func (s *Service) announceToTarget(ctx context.Context, repository *trace.Repository, store *flakes.Store, group string, o flakes.Occurrence) error {
	target := o.Decision.Workstream
	if o.Decision.State != flakes.Joined || target == "" || o.FromFlakeFix || o.IsMoot(flakes.DestinationTarget) ||
		slices.ContainsFunc(o.Surfaced, func(sf flakes.Surfacing) bool { return sf.Workstream == target }) {
		return nil
	}
	if archive, archived := s.archivedWorkstream(string(target)); archived {
		return s.closeFlakeDestination(ctx, store, group, o, flakes.DestinationTarget, target, archive)
	}
	if err := s.step("flake-announce-target " + o.ID); err != nil {
		return fmt.Errorf("surfacing flake occurrence %s to workstream %s: %w", o.ID, target, err)
	}
	recorder := &flakeRecorder{s: s, repository: repository}
	run := flakeRun{stream: o.Source.Workstream, path: o.RunPath(), source: o.Source}
	return recorder.surface(ctx, store, run, located{group: group, Occurrence: o})
}

func (f *flakeRecorder) announce(ctx context.Context) error {
	store := f.s.flakeStore(f.repository)
	groups, err := store.Groups()
	if err != nil {
		return err
	}
	var failures []error
	for _, g := range groups {
		for _, o := range g.Occurrences {
			if !o.Decision.Final() || f.announced(o) {
				continue
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			f.s.flakeFixMu.Lock()
			err := f.s.announceFlakeDecision(ctx, f.repository, store, g.Group, o.Run)
			f.s.flakeFixMu.Unlock()
			if err != nil {
				failures = append(failures, err)
			}
		}
	}
	return errors.Join(failures...)
}

func (f *flakeRecorder) announced(o flakes.Occurrence) bool {
	if !o.IsAnnounced(flakes.DestinationOrigin) && !o.IsMoot(flakes.DestinationOrigin) {
		return false
	}
	target := o.Decision.Workstream
	if o.Decision.State != flakes.Joined || target == "" || o.FromFlakeFix || o.IsMoot(flakes.DestinationTarget) {
		return true
	}
	return slices.ContainsFunc(o.Surfaced, func(sf flakes.Surfacing) bool { return sf.Workstream == target })
}

func (f *flakeRecorder) settle(ctx context.Context) error {
	store := f.s.flakeStore(f.repository)
	groups, err := store.Groups()
	if err != nil {
		return err
	}
	var failures []error
	resume := func(group string, o flakes.Occurrence) error {
		f.s.flakeFixMu.Lock()
		defer f.s.flakeFixMu.Unlock()
		if _, why, err := f.s.completeFlakeOpening(ctx, f.repository, store, group, o.Run); err != nil {
			return err
		} else if why != "" {
			return errors.New(why)
		}
		return nil
	}
	for _, g := range groups {
		for _, o := range g.Occurrences {
			if o.Decision.State != flakes.Opening {
				continue
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := resume(g.Group, o); err != nil {
				failures = append(failures, err)
			}
		}
	}
	if err := f.announce(ctx); err != nil {
		failures = append(failures, err)
	}
	if groups, err = store.Groups(); err != nil {
		return errors.Join(append(failures, err)...)
	}
	for _, g := range groups {
		for _, workstream := range g.Associated() {
			finished, err := f.s.flakeFixFinished(f.repository, workstream)
			if err != nil {
				failures = append(failures, err)
				continue
			}
			if finished {
				if _, err := store.CloseWorkstream(ctx, workstream); err != nil {
					failures = append(failures, err)
				}
			}
		}
	}
	return errors.Join(failures...)
}

func (s *Service) flakeFixFinished(repository *trace.Repository, workstream config.WorkstreamID) (bool, error) {
	if _, archived := s.archivedWorkstream(string(workstream)); archived {
		return true, nil
	}
	streams, err := repository.Workstreams()
	if err != nil || !slices.Contains(streams, workstream) {
		return false, err
	}
	state, err := repository.Workflow(workstream, trace.FeatureSubject)
	if err != nil {
		return false, err
	}
	return intake.Finished(state.Value), nil
}

func (s *Service) closeFlakeFix(ctx context.Context, repository *trace.Repository, workstream config.WorkstreamID) {
	if _, err := s.flakeStore(repository).CloseWorkstream(context.WithoutCancel(ctx), workstream); err != nil {
		ghlog.Logger(ctx).Warn("flake groups will be closed by the next flake pass", "workstream", string(workstream), "error", err.Error())
	}
}
