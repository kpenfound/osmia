package execution

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/bundle"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/followup"
	"github.com/kpenfound/osmia/internal/kb"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/router"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/scheduler"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/trace"
)

func MasonTurnID(unit string) string { return masonAgent(unit) + "-implement" }

func MasonTransitionID(unit string) string {
	return trace.UnitSubject(unit) + "-" + UnitImplementing
}

type masons struct {
	s          *Controller
	cfg        *config.Config
	repository *trace.Repository
}

type building struct {
	stream            config.WorkstreamID
	states            map[string]trace.WorkflowState
	reviewTransitions map[string]trace.Transition
	plan              plan.Plan
	started           time.Time
	inFlight          []string
	implementing      int
}

func (m *masons) Pass(ctx context.Context) error {
	streams, err := nonSessionWorkstreams(m.repository)
	if err != nil {
		return err
	}
	slots := map[config.WorkstreamID]int{}
	var read, candidates []building
	held := map[config.WorkstreamID]string{}
	for _, stream := range streams {
		if session, err := isSession(m.repository, stream); err != nil {
			return fmt.Errorf("workstream %s masons: %w", stream, err)
		} else if session {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if waiting, err := m.s.EdgeWaiting(m.repository, stream); err != nil {
			return err
		} else if waiting {
			continue
		}
		b, found, err := m.read(stream)
		if err != nil {
			return fmt.Errorf("workstream %s masons: %w", stream, err)
		}
		if !found {
			continue
		}
		blocked := ""
		for _, u := range b.plan.Units {
			state := b.states[trace.UnitSubject(u.ID)]
			if !unitDependenciesMerged(b, u) {
				continue
			}
			if state.Value != UnitImplementing && state.Value != UnitWaiting {
				continue
			}
			if state.Value == UnitWaiting {
				latest, err := LatestUnitTransition(m.repository, stream, u.ID)
				if err != nil {
					return err
				}
				amendmentWaiting := strings.HasPrefix(latest.Cause, "amendment_")
				masonWaiting, err := questionWaitOwner(m.repository, stream, u.ID, masonAgent(u.ID), masonActor)
				if err != nil {
					return err
				}
				if amendmentWaiting {
					b.inFlight = append(b.inFlight, u.ID)
					continue
				}
				if !masonWaiting {
					b.inFlight = append(b.inFlight, u.ID)
					continue
				}
			}
			if err := m.recoverInterrupted(ctx, stream, u.ID); err != nil {
				return fmt.Errorf("workstream %s unit %s: %w", stream, u.ID, err)
			}
			value, err := m.Follow(ctx, stream, u.ID, state)
			if err != nil {
				return fmt.Errorf("workstream %s unit %s: %w", stream, u.ID, err)
			}
			if value != UnitImplementing {
				b.inFlight = append(b.inFlight, u.ID)
				continue
			}
			state, err = m.repository.Workflow(stream, trace.UnitSubject(u.ID))
			if err != nil {
				return err
			}
			b.states[trace.UnitSubject(u.ID)] = state
			if queued, err := m.resumeMasonRuling(ctx, stream, u.ID); err != nil {
				return fmt.Errorf("workstream %s unit %s: %w", stream, u.ID, err)
			} else if queued {
				b.inFlight = append(b.inFlight, u.ID)
				b.implementing++
				slots[stream]++
				continue
			}
			if err := m.ResumeBounceRevision(ctx, stream, u.ID, b.reviewTransitions[trace.UnitSubject(u.ID)]); err != nil {
				return fmt.Errorf("workstream %s unit %s: %w", stream, u.ID, err)
			}
			moved, stuck, uncollected, err := m.finishReport(ctx, b, u.ID)
			if err != nil {
				return fmt.Errorf("workstream %s unit %s: %w", stream, u.ID, err)
			}
			if moved {
				if err := m.holdsItsSlot(&b, slots, u.ID); err != nil {
					return err
				}
				continue
			}
			classified, contested, err := m.Classify(ctx, stream, u.ID, state)
			if err != nil {
				return fmt.Errorf("workstream %s unit %s: %w", stream, u.ID, err)
			}
			if contested {
				if err := m.holdsItsSlot(&b, slots, u.ID); err != nil {
					return err
				}
				continue
			}
			if contested, err := m.contestFailure(ctx, stream, u.ID, state); err != nil {
				return fmt.Errorf("workstream %s unit %s: %w", stream, u.ID, err)
			} else if contested {
				if err := m.holdsItsSlot(&b, slots, u.ID); err != nil {
					return err
				}
				continue
			}
			continued, contested, err := m.continueEnding(ctx, stream, u.ID, state, uncollected)
			if err != nil {
				return fmt.Errorf("workstream %s unit %s: %w", stream, u.ID, err)
			}
			if contested {
				if err := m.holdsItsSlot(&b, slots, u.ID); err != nil {
					return err
				}
				continue
			}
			b.inFlight = append(b.inFlight, u.ID)
			b.implementing++
			queued := classified || continued
			if !stuck && !queued {
				if queued, err = m.recoverTurn(ctx, stream, u.ID); err != nil {
					return fmt.Errorf("workstream %s unit %s: %w", stream, u.ID, err)
				}
				if !queued {
					queued, err = m.Resume(ctx, stream, u.ID)
				}
				if err != nil {
					return fmt.Errorf("workstream %s unit %s: %w", stream, u.ID, err)
				}
			}
			if !queued {
				if blocked == "" {
					blocked = u.ID
				}
			} else {
				slots[stream]++
			}
		}
		if blocked != "" {
			held[stream] = blocked
		} else {
			candidates = append(candidates, b)
			continue
		}
		read = append(read, b)
	}
	if err := m.s.RefreshBudget(ctx, m.repository); err != nil {
		return err
	}
	state, _ := m.s.Effective()
	paused := func(stream config.WorkstreamID) bool { return scheduler.Paused(state.Pauses, m.cfg.Project.ID, stream) }
	implementing := 0
	for stream, count := range slots {
		if !paused(stream) {
			implementing += count
		}
	}
	candidates = slices.DeleteFunc(candidates, func(b building) bool {
		if paused(b.stream) {
			read = append(read, b)
			return true
		}
		return false
	})
	var entities kb.Map
	if len(candidates) > 0 {
		if entities, err = kb.Load(m.repository); err != nil {
			return err
		}
	}
	for implementing < m.cfg.Capacity.Masons {
		i, unit, err := m.nextStart(startOrder(candidates, state.Priorities, m.cfg.Project.ID), entities)
		if err != nil {
			return err
		}
		if i < 0 {
			break
		}
		b := candidates[i]
		started, blocked, err := m.start(ctx, b, unit)
		if err != nil {
			return fmt.Errorf("workstream %s unit %s: %w", b.stream, unit, err)
		}
		if !started {
			if blocked {
				held[b.stream] = unit
				read = append(read, b)
			}
			candidates = slices.Delete(candidates, i, i+1)
			continue
		}
		implementing++
		b.states[trace.UnitSubject(unit)] = trace.WorkflowState{Value: UnitImplementing}
		b.inFlight, b.implementing, b.started = append(b.inFlight, unit), b.implementing+1, m.s.Now()
		candidates[i] = b
	}
	decisions := dispatchPass{cfg: m.cfg, pauses: state.Pauses, rank: scheduler.Rank(state.Priorities, m.cfg.Project.ID), entities: entities, candidates: candidates, held: held}
	return m.recordDeferrals(ctx, decisions, append(read, candidates...))
}

func (m *masons) holdsItsSlot(b *building, slots map[config.WorkstreamID]int, unit string) error {
	state, err := m.repository.Workflow(b.stream, trace.UnitSubject(unit))
	if err != nil {
		return err
	}
	if state.Value == UnitImplementing {
		b.inFlight = append(b.inFlight, unit)
		b.implementing++
		slots[b.stream]++
	}
	return nil
}

func (m *masons) Classify(ctx context.Context, stream config.WorkstreamID, unit string, state trace.WorkflowState) (queued, contested bool, err error) {
	th, err := m.repository.Thread(stream, masonAgent(unit))
	if errors.Is(err, os.ErrNotExist) || err == nil && len(th.Turns) == 0 {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	last := th.Turns[len(th.Turns)-1]
	if last.CompletedAt.IsZero() || last.Response == nil || last.Response.Classification == nil {
		return false, false, nil
	}
	c := last.Response.Classification
	var reset uint64
	if ruling, found, err := LatestMasonRuling(m.repository, stream, unit); err != nil {
		return false, false, err
	} else if found {
		reset = ruling.ResetTurn
	}
	if last.Sequence <= reset {
		return false, false, nil
	}
	attempts := 0
	for _, turn := range th.Turns {
		if turn.Sequence > reset && countsAsAttempt(th, unit, turn) {
			attempts++
		}
	}
	id := trace.EventID(last.Response.ID, "classification")
	subject := id
	receipt, err := m.repository.Workflow(stream, subject)
	if err != nil {
		return false, false, err
	}
	if receipt.Value == "" {
		reason := fmt.Sprintf("mason turn %s classified %s: %s; tool counts %v", last.Request.TurnID, c.Class, c.Evidence, c.ToolCounts)
		if c.Judgment != "" {
			reason += "; Jev judgment " + c.Judgment
		}
		if c.Class == "gave_up" {
			reason += "; unit contested"
		} else if attempts >= m.cfg.Mason.MaxCleanTurns {
			reason += fmt.Sprintf("; clean-turn bound exhausted (%d/%d), unit contested", attempts, m.cfg.Mason.MaxCleanTurns)
		}
		h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: m.repository.Project(), Workstream: stream, Unit: unit, At: m.s.Now(), Actor: masonActor, Cause: last.Response.ID}
		_, err = m.repository.Transact(ctx, trace.Transaction{ExpectedVersion: 0, Transition: trace.Transition{Header: h, Subject: subject, From: "", To: "recorded", Reason: reason}, Events: []trace.Event{trace.Notice(id, "chief", reason)}})
		if err != nil && !errors.Is(err, trace.ErrConflict) {
			return false, false, err
		}
	}
	if c.Class == "gave_up" || attempts >= m.cfg.Mason.MaxCleanTurns {
		reason := "mason classified " + c.Class
		if c.Class != "gave_up" {
			reason = fmt.Sprintf("mason clean-turn bound exhausted (%d/%d) after %s", attempts, m.cfg.Mason.MaxCleanTurns, c.Class)
		}
		id := trace.EventID(last.Response.ID, "contested")
		err := m.contestEnding(ctx, stream, unit, state, last, id, reason, attempts, c.Evidence)
		return false, err == nil, err
	}
	profile, _, err := m.s.RoleExecution(m.cfg, masonRole)
	if err != nil {
		return false, false, err
	}
	turnID := masonAgent(unit) + "-clarify-" + fmt.Sprint(last.Sequence)
	prompt := "Your last turn ended without calling an outcome tool. Continue the unit from your workspace."
	switch c.Class {
	case "asked_in_prose":
		prompt += " You asked a question in prose. Call ask with the question if you need an answer."
	case "claims_done":
		prompt += " You claimed completion in prose. Call done with the required report if the unit's acceptance holds."
	default:
		prompt += " Call ask if you need a decision, or done with the required report when the unit's acceptance holds."
	}
	req := trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_" + turnID, Revision: 1, Project: m.repository.Project(), Workstream: stream, Unit: unit, At: m.s.Now(), Actor: masonActor, Cause: last.Response.ID, Depth: last.Request.Depth + 1}, AgentID: masonAgent(unit), ThreadID: masonAgent(unit), TurnID: turnID, Profile: profile, SystemPrompt: last.Request.SystemPrompt, Prompt: prompt}
	err = m.EnqueueTurn(ctx, req)
	return err == nil, false, err
}

func (m *masons) contestEnding(ctx context.Context, stream config.WorkstreamID, unit string, state trace.WorkflowState, last trace.QueuedTurn, id, reason string, attempts int, evidence string) error {
	_, err := m.s.raise(ctx, true, UnitExitInput{
		Repo: m.repository, Stream: stream, Unit: unit, State: state, Trigger: TriggerMasonEndedClean, Cause: last.Response.ID,
		Default: UnitContested, Targets: []string{UnitContested, UnitImplementing}, Actor: masonActor,
		ID: id, HeaderCause: last.Response.ID, Reason: reason, Events: []trace.Event{trace.Notice(id, "unit", fmt.Sprintf("Unit %s is contested: %s.", unit, reason))},
		Note: reason,
		Evidence: router.Evidence{Facts: map[string]any{"clean_turns": attempts, "max_clean_turns": m.cfg.Mason.MaxCleanTurns},
			Records: []router.Entry{{Kind: "mason turn", ID: last.Request.TurnID, Text: reason + "\n" + evidence}}},
	})
	return err
}

func (m *masons) resumeMasonRuling(ctx context.Context, stream config.WorkstreamID, unit string) (bool, error) {
	ruling, found, err := LatestMasonRuling(m.repository, stream, unit)
	if err != nil || !found {
		return false, err
	}
	th, err := m.repository.Thread(stream, masonAgent(unit))
	if err != nil {
		return false, err
	}
	if len(th.Turns) == 0 {
		return false, fmt.Errorf("ruled mason thread for unit %s is empty", unit)
	}
	last := th.Turns[len(th.Turns)-1]
	if last.Sequence > ruling.ResetTurn {
		return false, nil
	}
	profile, _, err := m.s.RoleExecution(m.cfg, masonRole)
	if err != nil {
		return false, err
	}
	turnID := fmt.Sprintf("%s-owner-revise-%d", masonAgent(unit), ruling.ResetTurn)
	prompt := "The " + ContestRuler(ruling.ContestedRuling) + " ruled that you should revise this unit in your existing workspace. Note: " + ruling.Note + "\nCheck the unit's acceptance, then call done or ask if you need a decision."
	if ruling.turn != "" {
		turnID, prompt = ruling.turn, ruling.prompt
	}
	if slices.ContainsFunc(th.Turns, func(q trace.QueuedTurn) bool { return q.Request.TurnID == turnID }) {
		return false, nil
	}
	req := trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_" + turnID, Revision: 1, Project: m.repository.Project(), Workstream: stream, Unit: unit, At: m.s.Now(), Actor: contestActor(ruling.ContestedRuling), Cause: ruling.Contest, Depth: last.Request.Depth + 1}, AgentID: masonAgent(unit), ThreadID: masonAgent(unit), TurnID: turnID, Profile: profile, SystemPrompt: last.Request.SystemPrompt, Prompt: prompt}
	err = m.EnqueueTurn(ctx, req)
	return err == nil, err
}

func (m *masons) nextStart(streams []building, entities kb.Map) (int, string, error) {
	for i, b := range streams {
		if b.implementing >= m.cfg.Project.Capacity.PerWorkstream {
			continue
		}
		unit, ok, err := nextReady(b, entities)
		if err != nil {
			return -1, "", fmt.Errorf("workstream %s: %w", b.stream, err)
		}
		if ok {
			return i, unit, nil
		}
	}
	return -1, "", nil
}

func (m *masons) recoverInterrupted(ctx context.Context, stream config.WorkstreamID, unit string) error {
	return m.RecoverView(ctx, stream, masonAgent(unit), func() (string, error) {
		w, _, found, err := NewUnitWorkspaces(m.cfg, m.repository).Find(ctx, stream, unit)
		if err != nil {
			return "", err
		}
		if !found {
			return "", fmt.Errorf("unit %s of workstream %s has no workspace", unit, stream)
		}
		return w.Path, nil
	})
}

func (m *masons) RecoverView(ctx context.Context, stream config.WorkstreamID, agent string, workspace func() (string, error)) error {
	th, err := m.repository.Thread(stream, agent)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || th.Status != "interrupted" || th.Active == "" {
		return err
	}
	var pending *trace.QueuedTurn
	for i := range th.Turns {
		if th.Turns[i].Request.TurnID == th.Active {
			pending = &th.Turns[i]
			break
		}
	}
	if pending == nil {
		return fmt.Errorf("interrupted mason thread has no active turn")
	}
	dir := TurnViews(m.cfg, m.repository.Project(), stream, agent, pending.Request.TurnID)
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	ready, err := os.ReadFile(filepath.Join(dir, "ready"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	views := slices.DeleteFunc(entries, func(entry os.DirEntry) bool {
		return entry.Name() == "ready" || entry.Name() == recordedName || entry.Name() == recordedName+".tmp"
	})
	if len(views) > 1 {
		return fmt.Errorf("mason turn %s has multiple surviving views", pending.Request.TurnID)
	}
	if len(views) == 1 {
		if !views[0].IsDir() || !strings.HasPrefix(views[0].Name(), "turn-") {
			return fmt.Errorf("mason turn %s has an unexpected view entry", pending.Request.TurnID)
		}
		view := filepath.Join(dir, views[0].Name())
		if string(ready) != views[0].Name() {
			if len(ready) != 0 {
				return fmt.Errorf("mason turn %s has a mismatched ready view", pending.Request.TurnID)
			}
			if err := os.RemoveAll(view); err != nil {
				return err
			}
			return m.repository.AbandonTurn(ctx, stream, agent, pending.Request.TurnID, m.s.Now())
		}
		path, err := workspace()
		if err != nil {
			return err
		}
		providers := NewUnitWorkspaces(m.cfg, m.repository).streamWorkspaces
		if agent == driftMasonAgent {
			providers = streamWorkspaces{Cfg: m.cfg, Repository: m.repository, Directory: driftsDirectory}
		}
		provider, err := providers.Of(stream)
		if err != nil {
			return err
		}
		if err := captureFiles(ctx, provider, view, path); err != nil {
			return err
		}
		if err := os.RemoveAll(view); err != nil {
			return err
		}
	}
	if err := os.Remove(filepath.Join(dir, "ready")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return m.repository.AbandonTurn(ctx, stream, agent, pending.Request.TurnID, m.s.Now())
}

func (m *masons) recoverTurn(ctx context.Context, stream config.WorkstreamID, unit string) (bool, error) {
	th, err := m.repository.Thread(stream, masonAgent(unit))
	if errors.Is(err, os.ErrNotExist) || err == nil && len(th.Turns) == 0 {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	last := th.Turns[len(th.Turns)-1]
	if last.Status() != "interrupted" {
		return false, nil
	}
	asked, err := m.repository.Questions(stream)
	if err != nil {
		return false, err
	}
	if askedBy(asked, th.Identity.ThreadID, last.Request.TurnID) != "" {
		return false, nil
	}
	req := last.Request
	profile, _, err := m.s.RoleExecution(m.cfg, masonRole)
	if err != nil {
		return false, err
	}
	req.Profile = profile
	req.ID = "request_" + masonAgent(unit) + "-recover-" + fmt.Sprint(last.Sequence)
	req.TurnID = masonAgent(unit) + "-recover-" + fmt.Sprint(last.Sequence)
	req.At = m.s.Now()
	req.Cause = last.Response.ID
	paths, recorded, err := NewUnitWorkspaces(m.cfg, m.repository).Changed(ctx, stream, unit, last.Request.TurnID)
	if err != nil {
		return false, err
	}
	req.Prompt = last.Request.Prompt + "\n\n" + Interruption(last) + " " + kept("that turn", paths, recorded) + " Continue from those files, check the unit's acceptance, and report done when it holds."
	err = m.EnqueueTurn(ctx, req)
	return err == nil, err
}

func (m *masons) Follow(ctx context.Context, stream config.WorkstreamID, unit string, state trace.WorkflowState) (string, error) {
	th, err := m.repository.Thread(stream, masonAgent(unit))
	if errors.Is(err, os.ErrNotExist) || err == nil && len(th.Turns) == 0 {
		return state.Value, nil
	}
	if err != nil {
		return "", err
	}
	asked, err := m.repository.Questions(stream)
	if err != nil {
		return "", err
	}
	last := th.Turns[len(th.Turns)-1]
	subject := trace.UnitSubject(unit)
	var to, id, cause, reason string
	switch state.Value {
	case UnitImplementing:
		transitions, err := trace.Read[trace.Transition](m.repository, stream)
		if err != nil {
			return "", err
		}
		q := askedBy(asked, th.Identity.ThreadID, last.Request.TurnID)
		id = fmt.Sprintf("%s-%s-%s", subject, UnitWaiting, q)
		if q == "" || slices.ContainsFunc(transitions, func(t trace.Transition) bool { return t.ID == id }) {
			return state.Value, nil
		}
		to, cause = UnitWaiting, trace.QuestionSubject(q)+"_"+trace.QuestionOpen
		reason = fmt.Sprintf("unit %s is waiting: its mason asked question %s; the unit's workspace is kept and it takes no mason slot until the answer arrives", unit, q)
	case UnitWaiting:
		waited, delivered, problem, err := waitedQuestion(m.repository, stream, unit, th, asked)
		if err != nil {
			return "", err
		}
		if problem != "" {
			return UnitContested, m.contestQuestionWait(ctx, stream, unit, state, masonActor, problem)
		}
		if !delivered {
			return state.Value, nil
		}
		q := waited.Asked.ID
		to, id, cause = UnitImplementing, fmt.Sprintf("%s-%s-%s", subject, UnitImplementing, q), trace.QuestionSubject(q)+"_"+trace.QuestionAnswered
		transitions, err := trace.Read[trace.Transition](m.repository, stream)
		if err != nil {
			return "", err
		}
		if slices.ContainsFunc(transitions, func(t trace.Transition) bool { return t.ID == id }) {
			id += fmt.Sprintf("-%d", state.Version)
		}
		reason = fmt.Sprintf("unit %s resumes implementing: the answer to question %s is its mason's next turn", unit, q)
	default:
		return state.Value, nil
	}
	res, err := m.s.raise(ctx, true, UnitExitInput{
		Repo: m.repository, Stream: stream, Unit: unit, State: state, Trigger: TriggerQuestionFollow, Cause: id,
		Default: to, Targets: []string{to}, Actor: masonActor, ID: id, HeaderCause: cause, Reason: reason,
	})
	switch {
	case err != nil:
		return "", err
	case res.Committed:
		return to, nil
	}
	return state.Value, nil
}

func (m *masons) read(stream config.WorkstreamID) (building, bool, error) {
	if held, err := m.s.ChangeHeld(m.repository, stream); err != nil || held {
		return building{}, false, err
	}
	return m.readActive(stream)
}

func (m *masons) readActive(stream config.WorkstreamID) (building, bool, error) {
	feature, err := m.repository.Workflow(stream, trace.FeatureSubject)
	if err != nil {
		return building{}, false, err
	}
	maintenance := false
	if feature.Value == DeliveredState {
		maintenance, err = m.s.WorkgroupEdgePending(m.repository, stream)
	}
	if err != nil || (feature.Value != BuildingState && feature.Value != AssembledState && !maintenance) {
		return building{}, false, err
	}
	return m.ReadSealed(stream)
}

func (m *masons) ReadSealed(stream config.WorkstreamID) (building, bool, error) {
	latest, _, found, err := seal.Latest(m.repository, stream)
	if err != nil || !found {
		return building{}, false, err
	}
	if planned, err := m.repository.Planned(stream); err != nil || !planned {
		return building{}, false, err
	}
	graph, err := sealedPlan(m.repository, stream, latest.Revision.Plan)
	if err != nil {
		return building{}, false, err
	}
	p, err := plan.Parse([]byte(graph.Content))
	if err != nil {
		return building{}, false, err
	}
	added, err := followup.Read(m.repository, stream)
	if err != nil {
		return building{}, false, err
	}
	for _, u := range added {
		p.Units = append(p.Units, u.Unit)
	}
	states, err := m.repository.WorkflowStates(stream)
	if err != nil {
		return building{}, false, err
	}
	transitions, err := trace.Read[trace.Transition](m.repository, stream)
	if err != nil {
		return building{}, false, err
	}
	b := building{stream: stream, states: states, plan: p}
	for _, t := range transitions {
		if BounceContest(t, t.Unit) || bounceRevisionTransition(t) || ordinarySendBack(t) {
			if b.reviewTransitions == nil {
				b.reviewTransitions = make(map[string]trace.Transition)
			}
			b.reviewTransitions[t.Subject] = t
		} else {
			delete(b.reviewTransitions, t.Subject)
		}
		if t.Actor == masonActor && t.From == UnitReady && t.To == UnitImplementing && t.At.After(b.started) {
			b.started = t.At
		}
	}
	return b, true, nil
}

func startOrder(streams []building, priorities []runtime.Priority, project config.ProjectID) []building {
	order := scheduler.Rank(priorities, project)
	slices.SortFunc(streams, func(a, b building) int {
		return cmp.Or(cmp.Compare(order(a.stream), order(b.stream)), a.started.Compare(b.started), strings.Compare(string(a.stream), string(b.stream)))
	})
	return streams
}

func nextReady(b building, entities kb.Map) (string, bool, error) {
	for _, u := range dependencyOrder(b.plan) {
		if b.states[trace.UnitSubject(u.ID)].Value != UnitReady || !unitDependenciesMerged(b, u) {
			continue
		}
		decision, err := plan.DecideStart(b.plan, entities, u.ID, b.inFlight)
		if err != nil {
			return "", false, err
		}
		if decision.CanStart {
			return u.ID, true, nil
		}
	}
	return "", false, nil
}

func dependencyOrder(p plan.Plan) []plan.Unit {
	placed := map[string]bool{}
	var out []plan.Unit
	for len(out) < len(p.Units) {
		i := slices.IndexFunc(p.Units, func(u plan.Unit) bool {
			return !placed[u.ID] && !slices.ContainsFunc(u.DependsOn, func(d string) bool { return !placed[d] })
		})
		if i < 0 {
			return out
		}
		placed[p.Units[i].ID] = true
		out = append(out, p.Units[i])
	}
	return out
}

func (m *masons) Bundle(ctx context.Context, stream config.WorkstreamID, unit string) (bundle.Mason, error) {
	files := bundle.Files{Provider: m.s.ContextFor(m.repository), Role: "mason", Repository: func(config.ProjectID) (*trace.Repository, error) { return m.repository, nil }}
	return files.Mason(ctx, m.repository.Project(), stream, unit)
}

func (m *masons) start(ctx context.Context, b building, unit string) (started, blocked bool, err error) {
	if held, err := m.s.ChangeHeld(m.repository, b.stream); err != nil || held {
		return false, false, err
	}
	subject := trace.UnitSubject(unit)
	stays := fmt.Sprintf("unit %s stays ready", unit)
	mason, err := m.Bundle(ctx, b.stream, unit)
	if errors.Is(err, bundle.ErrStaleSpec) {
		return false, true, m.block(ctx, b.stream, unit, subject+"-"+UnitReady, fmt.Sprintf("%s: its mason bundle cannot be assembled: %v", stays, err))
	}
	if err != nil {
		return false, false, err
	}
	w, base, err := NewUnitWorkspaces(m.cfg, m.repository).open(ctx, b.stream, unit)
	if err != nil && ctx.Err() == nil {
		return false, true, m.block(ctx, b.stream, unit, subject+"-"+UnitReady, fmt.Sprintf("%s: its workspace cannot be opened: %v", stays, err))
	}
	if err != nil {
		return false, false, errors.Join(ctx.Err(), err)
	}
	latest, err := latestDispatches(m.repository, b.stream)
	if err != nil {
		return false, false, err
	}
	decision, changed, err := m.dispatchRevision(b.stream, latest[unit], startedDispatch(unit, b.states[subject]))
	if err != nil {
		return false, false, err
	}
	var docs []trace.Document
	if changed {
		docs = append(docs, decision)
	}
	if change, found, err := m.changeRecord(ctx, b.stream, unit, w); err != nil {
		return false, false, err
	} else if found {
		docs = append(docs, change)
	}
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: MasonTransitionID(unit), Revision: 1, Project: m.repository.Project(), Workstream: b.stream, Unit: unit, At: m.s.Now(), Actor: masonActor, Cause: subject + "-" + UnitReady}
	tr := trace.Transition{Header: h, Subject: subject, From: UnitReady, To: UnitImplementing,
		Reason:    fmt.Sprintf("unit %s is the next ready unit of the plan of seal %d; its mason works in the unit's workspace on %s, created from %s at %s", unit, mason.Seal, w.Branch, featureBranch(b.stream), base),
		DecidedBy: trace.DecidedByCode, DefaultTo: UnitImplementing}
	tx := trace.Transaction{ExpectedVersion: b.states[subject].Version, Transition: tr,
		Events: []trace.Event{trace.Progress(MasonTransitionID(unit), "unit", fmt.Sprintf("Unit %s is implementing: its mason works on it in its unit workspace on %s.", unit, w.Branch))}}
	if len(docs) != 0 {
		_, err = m.repository.RecordDocumentsWith(ctx, docs, tx)
	} else {
		_, err = m.repository.Transact(ctx, tx)
	}
	if errors.Is(err, trace.ErrConflict) {
		return false, false, nil
	} else if err != nil {
		return false, false, err
	}
	if err := m.s.Step("mason-implementing:" + string(b.stream) + ":" + unit); err != nil {
		return false, false, err
	}
	return true, false, m.Enqueue(ctx, b.stream, unit, mason, tr)
}

func (m *masons) Resume(ctx context.Context, stream config.WorkstreamID, unit string) (bool, error) {
	queued, err := m.Queued(stream, unit)
	if err != nil || queued {
		return queued, err
	}
	moved := MasonTransitionID(unit)
	waits := fmt.Sprintf("unit %s is implementing and its mason's first turn is not queued", unit)
	if _, _, err := NewUnitWorkspaces(m.cfg, m.repository).open(ctx, stream, unit); err != nil {
		if ctx.Err() != nil {
			return false, errors.Join(ctx.Err(), err)
		}
		return false, m.block(ctx, stream, unit, moved, fmt.Sprintf("%s: its workspace cannot be opened: %v", waits, err))
	}
	transitions, err := trace.Read[trace.Transition](m.repository, stream)
	if err != nil {
		return false, err
	}
	i := slices.IndexFunc(transitions, func(t trace.Transition) bool { return t.ID == moved })
	if i < 0 {
		return false, fmt.Errorf("transition %s is missing", moved)
	}
	mason, err := m.Bundle(ctx, stream, unit)
	if errors.Is(err, bundle.ErrStaleSpec) {
		return false, m.block(ctx, stream, unit, moved, fmt.Sprintf("%s: its mason bundle cannot be assembled: %v", waits, err))
	}
	if err != nil {
		return false, err
	}
	return true, m.Enqueue(ctx, stream, unit, mason, transitions[i])
}

func BlockedSubject(unit string) string { return "blocked-" + masonAgent(unit) }

func (m *masons) block(ctx context.Context, stream config.WorkstreamID, unit, cause, reason string) error {
	subject := BlockedSubject(unit)
	state, err := m.repository.Workflow(stream, subject)
	if err != nil {
		return err
	}
	k := 1
	if state.Value != "" {
		if _, err := fmt.Sscanf(state.Value, "blocked-%d", &k); err != nil {
			return fmt.Errorf("subject %s is %q", subject, state.Value)
		}
		transitions, err := trace.Read[trace.Transition](m.repository, stream)
		if err != nil {
			return err
		}
		last := fmt.Sprintf("%s-%d", subject, k)
		if slices.ContainsFunc(transitions, func(t trace.Transition) bool { return t.ID == last && t.Reason == reason }) {
			return nil
		}
		k++
	}
	id := fmt.Sprintf("%s-%d", subject, k)
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: m.repository.Project(), Workstream: stream, Unit: unit, At: m.s.Now(), Actor: masonActor, Cause: cause}
	tx := trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: h, Subject: subject, From: state.Value, To: fmt.Sprintf("blocked-%d", k), Reason: reason},
		Events: []trace.Event{trace.Notice(id, "chief", "The mason controller is blocked: "+reason+". It tries again on every pass.")}}
	if _, err := m.repository.Transact(ctx, tx); err != nil && !errors.Is(err, trace.ErrConflict) {
		return err
	}
	return nil
}

func (m *masons) Queued(stream config.WorkstreamID, unit string) (bool, error) {
	t, err := m.repository.Thread(stream, masonAgent(unit))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(t.Turns, func(q trace.QueuedTurn) bool { return q.Request.TurnID == MasonTurnID(unit) }), nil
}

func (m *masons) Enqueue(ctx context.Context, stream config.WorkstreamID, unit string, mason bundle.Mason, moved trace.Transition) error {
	agent := masonAgent(unit)
	if _, err := m.repository.Thread(stream, agent); errors.Is(err, os.ErrNotExist) {
		identity := trace.Agent{Header: trace.Header{Schema: "osmia.trace.agent", Version: trace.Version, ID: agent, Revision: 1, Project: m.repository.Project(), Workstream: stream, Unit: unit, At: moved.At, Actor: masonActor, Cause: moved.ID}, Role: masonRole, ThreadID: agent}
		if err := m.repository.CreateThread(ctx, identity); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	profile, _, err := m.s.RoleExecution(m.cfg, masonRole)
	if err != nil {
		return err
	}
	turn := MasonTurnID(unit)
	req := trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_" + turn, Revision: 1, Project: m.repository.Project(), Workstream: stream, Unit: unit, At: moved.At, Actor: masonActor, Cause: moved.ID, Depth: 1},
		AgentID: agent, ThreadID: agent, TurnID: turn, Profile: profile, SystemPrompt: masonSystemPrompt(m.cfg.Project), Prompt: masonPrompt(mason)}
	err = m.EnqueueTurn(ctx, req)
	return err
}

func (m *masons) EnqueueTurn(ctx context.Context, req trace.TurnRequest) error {
	if _, err := m.repository.EnqueueTurn(ctx, req); err != nil {
		return err
	}
	if err := m.s.Step("mason-turn-queued:" + string(req.Workstream) + ":" + req.Unit); err != nil {
		return err
	}
	settle := context.WithoutCancel(ctx)
	gone, err := abandoned(m.repository, req.Workstream)
	if err != nil || !gone {
		return err
	}
	_, err = m.repository.CancelQueuedTurn(settle, req.Workstream, req.AgentID, req.TurnID, m.s.Now(), abandonActor, cancelReason)
	return err
}

func masonPrompt(m bundle.Mason) string {
	return fmt.Sprintf(`Build unit %s of this workstream.

Your view holds the project's files as the feature branch had them when the unit started, with the work done on the unit since. Do the unit's task below until every acceptance item holds, following the project's own conventions for tests. The unit's footprint schedules it; it does not limit what you change. The spec below is the one the owner ratified; build against it. When the task is done and its acceptance holds, call done with the outcome of your work: what it now does and how you checked the acceptance. Add a short headline, what happened in concrete terms, and needs_you only when the owner has a specific action. Then end your turn.

%s`, m.Unit, m.Render())
}

func Interruption(q trace.QueuedTurn) string {
	if q.Response != nil && q.Response.Stop != nil {
		return "A hard pause stopped your last turn."
	}
	if q.Response != nil && q.Response.Result.Client != "" && q.Response.Result.Client != coreadapter.LocalClient {
		return "The machine running your last turn was lost."
	}
	return "The service stopped during your last turn."
}

func unitDependenciesMerged(b building, u plan.Unit) bool {
	for _, id := range u.DependsOn {
		if b.states[trace.UnitSubject(id)].Value != UnitMerged {
			return false
		}
	}
	return true
}
