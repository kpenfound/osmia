package service

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
	"github.com/kpenfound/osmia/internal/followup"
	"github.com/kpenfound/osmia/internal/kb"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/scheduler"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/trace"
)

var masonActor = trace.Actor{Kind: "service", ID: "mason"}

// masonAgent returns the agent and thread ID of a unit's mason: mason-<unit>,
// or mason_<hash> for a unit whose subject is hashed.
func masonAgent(unit string) string {
	return "mason" + strings.TrimPrefix(trace.UnitSubject(unit), "unit")
}

// masonTurnID returns the ID of the first turn of a unit's mason, the one
// that starts the unit.
func masonTurnID(unit string) string { return masonAgent(unit) + "-implement" }

// masonTransitionID returns the ID of the transition that moves a unit from
// ready to implementing.
func masonTransitionID(unit string) string {
	return trace.UnitSubject(unit) + "-" + UnitImplementing
}

// masons is the mason controller. Its pass parks and resumes units on their
// masons' questions, and moves each implementing unit whose mason reported
// done to reviewing, with the unit's workspace snapshotted as its candidate.
// It then starts ready units of building and assembled workstreams: while
// fewer units than capacity.masons are implementing, it moves a ready unit
// entangled with none of its workstream's implementing or waiting units to
// implementing, and queues its mason's first turn with the unit's bundle in
// the unit's own workspace. The scheduler then runs that turn.
type masons struct {
	s          *Service
	cfg        *config.Config
	repository *trace.Repository
}

// building is one building workstream as a pass reads it.
type building struct {
	stream config.WorkstreamID
	states map[string]trace.WorkflowState
	plan   plan.Plan
	// started is when the workstream last started a unit, zero when never.
	started time.Time
	// inFlight names the workstream's implementing and waiting units.
	inFlight []string
	// implementing counts the workstream's implementing units, whether or not
	// a turn of theirs is queued or running.
	implementing int
}

// Pass follows the implementing and waiting units' questions and moves the
// units whose mason reported done to reviewing, which frees their mason
// slots, then starts the ready units capacity allows. Each start goes to the
// highest-priority workstream with a unit it can start and, among equals, the
// one that started a unit least recently; in that workstream, to the first
// ready unit in the plan's dependency order that is entangled with none of its
// implementing or waiting units. A workstream starts no unit while
// capacity.per_workstream of its units are implementing. A waiting unit takes
// no mason slot and does not count toward that cap. A paused workstream
// starts none, and its implementing units take no mason slot. A unit that
// cannot start, or an implementing unit whose first turn cannot be queued or
// whose candidate cannot be made, is blocked: the reason is recorded, the
// unit takes no mason slot and its workstream starts nothing else, and the
// other workstreams go on. Last, it records in units/<unit>/dispatch.json why
// each ready unit it did not start waits, when that differs from the unit's
// latest decision; a start records its own decision.
func (m *masons) Pass(ctx context.Context) error {
	streams, err := m.repository.Workstreams()
	if err != nil {
		return err
	}
	librarian := librarianWorkstream(m.repository.Project())
	state, _ := m.s.effective()
	paused := func(stream config.WorkstreamID) bool { return scheduler.Paused(state.Pauses, m.cfg.Project.ID, stream) }
	implementing := 0
	var read, candidates []building
	held := map[config.WorkstreamID]string{}
	for _, stream := range streams {
		if stream == librarian {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
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
			if state.Value != UnitImplementing && state.Value != UnitWaiting {
				continue
			}
			if state.Value == UnitWaiting {
				transitions, err := trace.Read[trace.Transition](m.repository, stream)
				if err != nil {
					return err
				}
				masonWaiting, amendmentWaiting := false, false
				for i := len(transitions) - 1; i >= 0; i-- {
					if transitions[i].Subject == trace.UnitSubject(u.ID) && transitions[i].To == UnitWaiting {
						amendmentWaiting = strings.HasPrefix(transitions[i].Cause, "amendment_")
						masonWaiting = transitions[i].Actor == masonActor
						break
					}
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
			value, err := m.follow(ctx, stream, u.ID, state)
			if err != nil {
				return fmt.Errorf("workstream %s unit %s: %w", stream, u.ID, err)
			}
			if value != UnitImplementing {
				b.inFlight = append(b.inFlight, u.ID)
				continue
			}
			if queued, err := m.resumeMasonRuling(ctx, stream, u.ID); err != nil {
				return fmt.Errorf("workstream %s unit %s: %w", stream, u.ID, err)
			} else if queued {
				b.inFlight = append(b.inFlight, u.ID)
				b.implementing++
				if !paused(stream) {
					implementing++
				}
				continue
			}
			moved, stuck, err := m.finish(ctx, b, u.ID)
			if err != nil {
				return fmt.Errorf("workstream %s unit %s: %w", stream, u.ID, err)
			}
			if moved {
				continue
			}
			classified, contested, err := m.classify(ctx, stream, u.ID, state)
			if err != nil {
				return fmt.Errorf("workstream %s unit %s: %w", stream, u.ID, err)
			}
			if contested {
				continue
			}
			if contested, err := m.contestFailure(ctx, stream, u.ID, state); err != nil {
				return fmt.Errorf("workstream %s unit %s: %w", stream, u.ID, err)
			} else if contested {
				continue
			}
			b.inFlight = append(b.inFlight, u.ID)
			b.implementing++
			queued := classified
			if !stuck && !queued {
				if queued, err = m.recoverTurn(ctx, stream, u.ID); err != nil {
					return fmt.Errorf("workstream %s unit %s: %w", stream, u.ID, err)
				}
				if !queued {
					queued, err = m.resume(ctx, stream, u.ID)
				}
				if err != nil {
					return fmt.Errorf("workstream %s unit %s: %w", stream, u.ID, err)
				}
			}
			if !queued {
				if blocked == "" {
					blocked = u.ID
				}
			} else if !paused(stream) {
				implementing++
			}
		}
		if blocked != "" {
			held[stream] = blocked
		} else if !paused(stream) {
			candidates = append(candidates, b)
			continue
		}
		read = append(read, b)
	}
	pending, err := refreshPending(m.repository)
	if err != nil {
		return err
	}
	if pending {
		return nil
	}
	var entities kb.Map
	if len(candidates) > 0 {
		if entities, err = kb.Load(m.repository); err != nil {
			return err
		}
	}
	// Each start re-sorts the workstreams, so equals take turns.
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
			// A unit whose state moved since it was read is left to the
			// next pass, with the rest of its workstream.
			if blocked {
				held[b.stream] = unit
				read = append(read, b)
			}
			candidates = slices.Delete(candidates, i, i+1)
			continue
		}
		implementing++
		b.states[trace.UnitSubject(unit)] = trace.WorkflowState{Value: UnitImplementing}
		b.inFlight, b.implementing, b.started = append(b.inFlight, unit), b.implementing+1, m.s.now()
		candidates[i] = b
	}
	decisions := dispatchPass{cfg: m.cfg, pauses: state.Pauses, rank: scheduler.Rank(state.Priorities, m.cfg.Project.ID), entities: entities, candidates: candidates, held: held}
	return m.recordDeferrals(ctx, decisions, append(read, candidates...))
}

// classify records one chief event per clean mason response, then either
// queues a bounded continuation or contests the unit. Stable IDs make a pass
// after a crash finish the same decision without duplicating its side effects.
func (m *masons) classify(ctx context.Context, stream config.WorkstreamID, unit string, state trace.WorkflowState) (queued, contested bool, err error) {
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
	if ruling, found, err := latestMasonRuling(m.repository, stream, unit); err != nil {
		return false, false, err
	} else if found {
		reset = ruling.ResetTurn
	}
	if last.Sequence <= reset {
		return false, false, nil
	}
	attempts := 0
	for _, turn := range th.Turns {
		if turn.Sequence > reset && !turn.CompletedAt.IsZero() && turn.Response != nil && turn.Response.Classification != nil {
			attempts++
		}
	}
	// The classification state is a receipt for the event. Its subject is
	// unique to this response and cannot be confused with a unit transition.
	id := trace.EventID(last.Response.ID, "classification")
	subject := id
	receipt, err := m.repository.Workflow(stream, subject)
	if err != nil {
		return false, false, err
	}
	if receipt.Value == "" {
		reason := fmt.Sprintf("mason turn %s classified %s: %s; tool counts %v", last.Request.TurnID, c.Class, c.Evidence, c.ToolCounts)
		if c.Class == "gave_up" {
			reason += "; unit contested"
		} else if attempts >= m.cfg.Mason.MaxCleanTurns {
			reason += fmt.Sprintf("; clean-turn bound exhausted (%d/%d), unit contested", attempts, m.cfg.Mason.MaxCleanTurns)
		}
		h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: m.repository.Project(), Workstream: stream, Unit: unit, At: m.s.now(), Actor: masonActor, Cause: last.Response.ID}
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
		h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: m.repository.Project(), Workstream: stream, Unit: unit, At: m.s.now(), Actor: masonActor, Cause: last.Response.ID}
		_, err := m.repository.Transact(ctx, trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: h, Subject: trace.UnitSubject(unit), From: UnitImplementing, To: UnitContested, Reason: reason}})
		if errors.Is(err, trace.ErrConflict) {
			return false, false, nil
		}
		return false, err == nil, err
	}
	profile, _, err := m.s.roleExecution(m.cfg, masonRole)
	if err != nil {
		return false, false, err
	}
	turnID := masonAgent(unit) + "-clarify-" + fmt.Sprint(last.Sequence)
	prompt := "Your last turn ended without calling an outcome tool. Continue the unit from your workspace."
	switch c.Class {
	case "asked_in_prose":
		prompt += " You asked a question in prose. Call ask with the question if you need an answer."
	case "claims_done":
		prompt += " You claimed completion in prose. Call done with the required report if the criteria and proofs hold."
	default:
		prompt += " Call ask if you need a decision, or done with the required report when the criteria and proofs hold."
	}
	req := trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_" + turnID, Revision: 1, Project: m.repository.Project(), Workstream: stream, Unit: unit, At: m.s.now(), Actor: masonActor, Cause: last.Response.ID, Depth: last.Request.Depth + 1}, AgentID: masonAgent(unit), ThreadID: masonAgent(unit), TurnID: turnID, Profile: profile, SystemPrompt: last.Request.SystemPrompt, Prompt: prompt}
	_, err = m.repository.EnqueueTurn(ctx, req)
	return err == nil, false, err
}

// resumeMasonRuling queues the owner's direction on the existing mason
// thread. The ruling's turn sequence gives a durable clean-turn reset.
func (m *masons) resumeMasonRuling(ctx context.Context, stream config.WorkstreamID, unit string) (bool, error) {
	ruling, found, err := latestMasonRuling(m.repository, stream, unit)
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
	profile, _, err := m.s.roleExecution(m.cfg, masonRole)
	if err != nil {
		return false, err
	}
	turnID := fmt.Sprintf("%s-owner-revise-%d", masonAgent(unit), ruling.ResetTurn)
	req := trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_" + turnID, Revision: 1, Project: m.repository.Project(), Workstream: stream, Unit: unit, At: m.s.now(), Actor: ownerActor, Cause: ruling.Contest, Depth: last.Request.Depth + 1}, AgentID: masonAgent(unit), ThreadID: masonAgent(unit), TurnID: turnID, Profile: profile, SystemPrompt: last.Request.SystemPrompt, Prompt: "The owner ruled that you should revise this unit in your existing workspace. Owner note: " + ruling.Note + "\nCheck the criteria and planned proofs, then call done with a criterion report or ask if you need a decision."}
	_, err = m.repository.EnqueueTurn(ctx, req)
	return err == nil, err
}

// nextStart returns the index of the first workstream, in the given order,
// with a unit it can start, and that unit, or -1 when none has one.
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

// recoverInterrupted copies a stopped turn's surviving view into its existing
// workspace before releasing the thread claim. Repeating the copy is safe if
// the service stops during recovery. The claim is released only after the
// copy, so another turn cannot overwrite work that has not been recovered.
func (m *masons) recoverInterrupted(ctx context.Context, stream config.WorkstreamID, unit string) error {
	return m.recoverView(ctx, stream, masonAgent(unit), func() (string, error) {
		w, _, found, err := newUnitWorkspaces(m.cfg, m.repository).find(ctx, stream, unit)
		if err != nil {
			return "", err
		}
		if !found {
			return "", fmt.Errorf("unit %s of workstream %s has no workspace", unit, stream)
		}
		return w.Path, nil
	})
}

// recoverView copies the surviving view of the agent's interrupted turn
// into the workspace directory returns, as recoverInterrupted does for a
// unit's mason, and releases the thread claim.
func (m *masons) recoverView(ctx context.Context, stream config.WorkstreamID, agent string, workspace func() (string, error)) error {
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
	dir := filepath.Join(m.cfg.Root.String(), "views", string(m.repository.Project()), string(stream), agent, pending.Request.TurnID)
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	ready, err := os.ReadFile(filepath.Join(dir, "ready"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	views := slices.DeleteFunc(entries, func(entry os.DirEntry) bool { return entry.Name() == "ready" })
	if len(views) > 1 {
		return fmt.Errorf("mason turn %s has multiple surviving views", pending.Request.TurnID)
	}
	if len(views) == 1 {
		if !views[0].IsDir() || !strings.HasPrefix(views[0].Name(), "turn-") {
			return fmt.Errorf("mason turn %s has an unexpected view entry", pending.Request.TurnID)
		}
		view := filepath.Join(dir, views[0].Name())
		if string(ready) != views[0].Name() {
			// The view was not fully copied before the service stopped; no
			// mason could have run against it yet.
			if len(ready) != 0 {
				return fmt.Errorf("mason turn %s has a mismatched ready view", pending.Request.TurnID)
			}
			if err := os.RemoveAll(view); err != nil {
				return err
			}
			return m.repository.AbandonTurn(ctx, stream, agent, pending.Request.TurnID, m.s.now())
		}
		path, err := workspace()
		if err != nil {
			return err
		}
		if err := mirror(view, path); err != nil {
			return err
		}
		if err := os.RemoveAll(view); err != nil {
			return err
		}
	}
	if err := os.Remove(filepath.Join(dir, "ready")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return m.repository.AbandonTurn(ctx, stream, agent, pending.Request.TurnID, m.s.now())
}

// recoverTurn queues one continuation for an interrupted mason turn unless
// that turn asked a question. The answer turn then supplies the continuation.
// A turn a hard pause stopped is interrupted too, and continues the same way.
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
	profile, _, err := m.s.roleExecution(m.cfg, masonRole)
	if err != nil {
		return false, err
	}
	req.Profile = profile
	req.ID = "request_" + masonAgent(unit) + "-recover-" + fmt.Sprint(last.Sequence)
	req.TurnID = masonAgent(unit) + "-recover-" + fmt.Sprint(last.Sequence)
	req.At = m.s.now()
	req.Cause = last.Response.ID
	req.Prompt = last.Request.Prompt + "\n\n" + interruption(last) + " Your workspace includes the files left by that turn. Continue from those files, check the unit's criteria and proofs, and report done when they hold."
	_, err = m.repository.EnqueueTurn(ctx, req)
	return err == nil, err
}

// follow moves a unit between implementing and waiting as its mason's thread
// says, and returns the unit's state. An implementing unit with a question
// whose waiting transition has not been recorded moves to waiting; a waiting
// unit whose mason's latest turn delivers the answer to one of its questions
// moves back to implementing. The workspace is left as it is either way. A
// unit whose state moved since it was read is left to the next pass.
func (m *masons) follow(ctx context.Context, stream config.WorkstreamID, unit string, state trace.WorkflowState) (string, error) {
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
		q := ""
		for _, turn := range th.Turns {
			candidate := askedBy(asked, th.Identity.ThreadID, turn.Request.TurnID)
			id := fmt.Sprintf("%s-%s-%s", subject, UnitWaiting, candidate)
			if candidate != "" && !slices.ContainsFunc(transitions, func(t trace.Transition) bool { return t.ID == id }) {
				q = candidate
				break
			}
		}
		if q == "" {
			return state.Value, nil
		}
		to, id, cause = UnitWaiting, fmt.Sprintf("%s-%s-%s", subject, UnitWaiting, q), trace.QuestionSubject(q)+"_"+trace.QuestionOpen
		reason = fmt.Sprintf("unit %s is waiting: its mason asked question %s; the unit's workspace is kept and it takes no mason slot until the answer arrives", unit, q)
	case UnitWaiting:
		i := slices.IndexFunc(asked, func(q trace.QuestionState) bool {
			return q.Asked.Thread == th.Identity.ThreadID && questions.TurnID(q.Asked.ID) == last.Request.TurnID
		})
		if i < 0 {
			return state.Value, nil
		}
		q := asked[i].Asked.ID
		to, id, cause = UnitImplementing, fmt.Sprintf("%s-%s-%s", subject, UnitImplementing, q), trace.QuestionSubject(q)+"_"+trace.QuestionAnswered
		reason = fmt.Sprintf("unit %s resumes implementing: the answer to question %s is its mason's next turn", unit, q)
	default:
		return state.Value, nil
	}
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: m.repository.Project(), Workstream: stream, Unit: unit, At: m.s.now(), Actor: masonActor, Cause: cause}
	tx := trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: h, Subject: subject, From: state.Value, To: to, Reason: reason}}
	if _, err := m.repository.Transact(ctx, tx); errors.Is(err, trace.ErrConflict) {
		return state.Value, nil
	} else if err != nil {
		return "", err
	}
	return to, nil
}

// read returns the workstream's unit states and effective plan while it is
// building or assembled, and when it last started a unit. The effective plan
// is the latest sealed plan followed by the recorded follow-up units.
func (m *masons) read(stream config.WorkstreamID) (building, bool, error) {
	feature, err := m.repository.Workflow(stream, trace.FeatureSubject)
	if err != nil || (feature.Value != BuildingState && feature.Value != AssembledState) {
		return building{}, false, err
	}
	latest, _, found, err := seal.Latest(m.repository, stream)
	if err != nil || !found {
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
		if t.Actor == masonActor && t.From == UnitReady && t.To == UnitImplementing && t.At.After(b.started) {
			b.started = t.At
		}
	}
	return b, true, nil
}

// startOrder sorts the workstreams by the project's priority order, those
// it does not name last, then by when each last started a unit, least
// recently first, then by ID.
func startOrder(streams []building, priorities []runtime.Priority, project config.ProjectID) []building {
	order := scheduler.Rank(priorities, project)
	slices.SortFunc(streams, func(a, b building) int {
		return cmp.Or(cmp.Compare(order(a.stream), order(b.stream)), a.started.Compare(b.started), strings.Compare(string(a.stream), string(b.stream)))
	})
	return streams
}

// nextReady returns the first ready unit of the workstream in its plan's
// dependency order that is entangled with none of the workstream's units in
// flight, its footprint resolved through entities.
func nextReady(b building, entities kb.Map) (string, bool, error) {
	for _, u := range dependencyOrder(b.plan) {
		if b.states[trace.UnitSubject(u.ID)].Value != UnitReady {
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

// dependencyOrder returns the plan's units so that every unit follows the
// units it depends on, each place going to the earliest unit in the plan
// whose dependencies are all placed.
func dependencyOrder(p plan.Plan) []plan.Unit {
	placed := map[string]bool{}
	var out []plan.Unit
	for len(out) < len(p.Units) {
		i := slices.IndexFunc(p.Units, func(u plan.Unit) bool {
			return !placed[u.ID] && !slices.ContainsFunc(u.DependsOn, func(d string) bool { return !placed[d] })
		})
		if i < 0 {
			// A sealed plan has no cycle.
			return out
		}
		placed[p.Units[i].ID] = true
		out = append(out, p.Units[i])
	}
	return out
}

// bundle assembles the mason bundle of one unit, from the service's files
// and this trace.
func (m *masons) bundle(ctx context.Context, stream config.WorkstreamID, unit string) (bundle.Mason, error) {
	files := bundle.Files{Repository: func(config.ProjectID) (*trace.Repository, error) { return m.repository, nil }, Now: m.s.now}
	return files.Mason(ctx, m.repository.Project(), stream, unit)
}

// start moves a ready unit to implementing and queues its mason's first
// turn, recording the decision to start it in units/<unit>/dispatch.json in
// the same commit as the move. The unit's bundle is assembled and its
// workspace opened first: a unit whose spec no longer matches its seal, or
// whose workspace cannot be opened, stays ready, and start records why it is
// blocked and reports it blocked. A unit whose state moved since it was read
// is left to the next pass.
func (m *masons) start(ctx context.Context, b building, unit string) (started, blocked bool, err error) {
	subject := trace.UnitSubject(unit)
	stays := fmt.Sprintf("unit %s stays ready", unit)
	mason, err := m.bundle(ctx, b.stream, unit)
	if errors.Is(err, bundle.ErrStaleSpec) {
		return false, true, m.block(ctx, b.stream, unit, subject+"-"+UnitReady, fmt.Sprintf("%s: its mason bundle cannot be assembled: %v", stays, err))
	}
	if err != nil {
		return false, false, err
	}
	w, base, err := newUnitWorkspaces(m.cfg, m.repository).open(ctx, b.stream, unit)
	if err != nil && ctx.Err() == nil {
		return false, true, m.block(ctx, b.stream, unit, subject+"-"+UnitReady, fmt.Sprintf("%s: its workspace cannot be opened: %v", stays, err))
	}
	if err != nil {
		return false, false, err
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
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: masonTransitionID(unit), Revision: 1, Project: m.repository.Project(), Workstream: b.stream, Unit: unit, At: m.s.now(), Actor: masonActor, Cause: subject + "-" + UnitReady}
	tr := trace.Transition{Header: h, Subject: subject, From: UnitReady, To: UnitImplementing,
		Reason: fmt.Sprintf("unit %s is the next ready unit of the plan of seal %d; its mason works in the unit's workspace on %s, created from %s at %s", unit, mason.Seal, w.Branch, featureBranch(b.stream), base)}
	tx := trace.Transaction{ExpectedVersion: b.states[subject].Version, Transition: tr,
		Events: []trace.Event{trace.Notice(masonTransitionID(unit), "unit", fmt.Sprintf("Unit %s is implementing: its mason works on it in its unit workspace on %s.", unit, w.Branch))}}
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
	return true, false, m.enqueue(ctx, b.stream, unit, mason, tr)
}

// resume queues the first turn of an implementing unit's mason when it is
// not queued yet, as after a stop between the unit's move and the turn,
// opening the unit's workspace first when it is missing. It reports whether
// the turn is queued: a unit whose workspace cannot be opened, or whose spec
// no longer matches its seal, gets no turn, and resume records why it is
// blocked.
func (m *masons) resume(ctx context.Context, stream config.WorkstreamID, unit string) (bool, error) {
	queued, err := m.queued(stream, unit)
	if err != nil || queued {
		return queued, err
	}
	moved := masonTransitionID(unit)
	waits := fmt.Sprintf("unit %s is implementing and its mason's first turn is not queued", unit)
	if _, _, err := newUnitWorkspaces(m.cfg, m.repository).open(ctx, stream, unit); err != nil {
		if ctx.Err() != nil {
			return false, err
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
	mason, err := m.bundle(ctx, stream, unit)
	if errors.Is(err, bundle.ErrStaleSpec) {
		return false, m.block(ctx, stream, unit, moved, fmt.Sprintf("%s: its mason bundle cannot be assembled: %v", waits, err))
	}
	if err != nil {
		return false, err
	}
	return true, m.enqueue(ctx, stream, unit, mason, transitions[i])
}

// blockedSubject is the workflow subject that records why a unit is
// blocked: blocked-mason-<unit>, or blocked-mason_<hash> for a unit whose
// subject is hashed.
func blockedSubject(unit string) string { return "blocked-" + masonAgent(unit) }

// block records why a unit is blocked, with a notice for the chief of staff:
// the transition <blocked-subject>-<k> moves blockedSubject to blocked-<k>. A
// reason the subject's latest transition already records is not recorded
// again, so a unit that stays blocked for the same reason is reported once.
func (m *masons) block(ctx context.Context, stream config.WorkstreamID, unit, cause, reason string) error {
	subject := blockedSubject(unit)
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
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: m.repository.Project(), Workstream: stream, Unit: unit, At: m.s.now(), Actor: masonActor, Cause: cause}
	tx := trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: h, Subject: subject, From: state.Value, To: fmt.Sprintf("blocked-%d", k), Reason: reason},
		Events: []trace.Event{trace.Notice(id, "chief", "The mason controller is blocked: "+reason+". It tries again on every pass.")}}
	if _, err := m.repository.Transact(ctx, tx); err != nil && !errors.Is(err, trace.ErrConflict) {
		return err
	}
	return nil
}

// queued reports whether the unit's mason has its first turn.
func (m *masons) queued(stream config.WorkstreamID, unit string) (bool, error) {
	t, err := m.repository.Thread(stream, masonAgent(unit))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(t.Turns, func(q trace.QueuedTurn) bool { return q.Request.TurnID == masonTurnID(unit) }), nil
}

// enqueue creates the unit's mason thread when it is missing and queues its
// first turn, caused by the unit's move to implementing and stamped with its
// time, with the unit's bundle in the prompt.
func (m *masons) enqueue(ctx context.Context, stream config.WorkstreamID, unit string, mason bundle.Mason, moved trace.Transition) error {
	agent := masonAgent(unit)
	if _, err := m.repository.Thread(stream, agent); errors.Is(err, os.ErrNotExist) {
		identity := trace.Agent{Header: trace.Header{Schema: "osmia.trace.agent", Version: trace.Version, ID: agent, Revision: 1, Project: m.repository.Project(), Workstream: stream, Unit: unit, At: moved.At, Actor: masonActor, Cause: moved.ID}, Role: masonRole, ThreadID: agent}
		if err := m.repository.CreateThread(ctx, identity); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	profile, _, err := m.s.roleExecution(m.cfg, masonRole)
	if err != nil {
		return err
	}
	turn := masonTurnID(unit)
	req := trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_" + turn, Revision: 1, Project: m.repository.Project(), Workstream: stream, Unit: unit, At: moved.At, Actor: masonActor, Cause: moved.ID, Depth: 1},
		AgentID: agent, ThreadID: agent, TurnID: turn, Profile: profile, SystemPrompt: masonSystemPrompt(m.cfg.Project), Prompt: masonPrompt(mason)}
	_, err = m.repository.EnqueueTurn(ctx, req)
	return err
}

func masonSystemPrompt(p config.Project) string {
	return fmt.Sprintf("You are a mason of the %s project (%s). You build one unit of a ratified plan in a workspace of its own, whose files are your view. You hold no version control tool: the service records your work. Build what the sealed spec says, for the criteria of your unit, and put in place the proofs the plan names for them. When your view and the spec do not settle something you must know, call %s: your unit waits for the answer, which arrives as your next turn, and your workspace is kept. If the sealed spec or plan needs to change, call %s with citations, a proposed change and a reason, then end the turn.", p.Name, p.Upstream, questions.AskTool, questions.AmendTool)
}

func masonPrompt(m bundle.Mason) string {
	return fmt.Sprintf(`Build unit %s of this workstream.

Your view holds the project's files as the feature branch had them when the unit started, with the work done on the unit since. Make each of the unit's criteria below hold, and put in place and pass the proof the plan names for it. Stay within the unit's footprint. The spec below is the one the owner ratified; build against it. When every criterion of the unit holds and its proof is in place and passing, call done with the outcome of your work and a report on every criterion of the unit: what you did, the evidence that it holds and where its proof lives. Include any new project facts you learned in learnings; leave that list empty when there are none. Add a short headline, what happened in concrete terms, and needs_you only when the owner has a specific action. Then end your turn.

%s`, m.Unit, m.Render())
}

// interruption says what ended an interrupted turn, for the prompt of the
// turn that continues it.
func interruption(q trace.QueuedTurn) string {
	if q.Response != nil && q.Response.Stop != nil {
		return "A hard pause stopped your last turn."
	}
	return "The service stopped during your last turn."
}
