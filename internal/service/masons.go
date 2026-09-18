package service

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/bundle"
	"github.com/kpenfound/osmia/internal/config"
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
// masons' questions, and starts ready units: in each building workstream
// with no implementing or waiting unit, while fewer units than
// capacity.masons are implementing, it moves the first ready unit in the
// plan's dependency order to implementing, and queues its mason's first turn
// with the unit's bundle in the unit's own workspace. The scheduler then runs
// that turn.
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
}

// Pass follows the implementing and waiting units' questions, then starts
// the ready units capacity allows, the highest-priority workstream first and,
// among equals, the one that started a unit least recently. A waiting unit
// takes no mason slot, and its workstream starts no other unit. A paused
// workstream starts none, and its implementing unit takes no mason slot. A unit that cannot start, or an implementing unit whose
// first turn cannot be queued, is blocked: the reason is recorded, the unit
// takes no mason slot and its workstream starts nothing else, and the other
// workstreams go on.
func (m *masons) Pass(ctx context.Context) error {
	streams, err := m.repository.Workstreams()
	if err != nil {
		return err
	}
	librarian := librarianWorkstream(m.repository.Project())
	state, _ := m.s.store.Effective()
	paused := func(stream config.WorkstreamID) bool { return scheduler.Paused(state.Pauses, m.cfg.Project.ID, stream) }
	implementing := 0
	var idle []building
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
		busy := false
		for _, u := range b.plan.Units {
			state := b.states[trace.UnitSubject(u.ID)]
			if state.Value != UnitImplementing && state.Value != UnitWaiting {
				continue
			}
			busy = true
			value, err := m.follow(ctx, stream, u.ID, state)
			if err != nil {
				return fmt.Errorf("workstream %s unit %s: %w", stream, u.ID, err)
			}
			if value != UnitImplementing {
				continue
			}
			queued, err := m.resume(ctx, stream, u.ID)
			if err != nil {
				return fmt.Errorf("workstream %s unit %s: %w", stream, u.ID, err)
			}
			if queued && !paused(stream) {
				implementing++
			}
		}
		if !busy && !paused(stream) {
			idle = append(idle, b)
		}
	}
	for _, b := range startOrder(idle, state.Priorities, m.cfg.Project.ID) {
		if implementing >= m.cfg.Capacity.Masons {
			return nil
		}
		unit, ok := nextReady(b)
		if !ok {
			continue
		}
		started, err := m.start(ctx, b, unit)
		if err != nil {
			return fmt.Errorf("workstream %s unit %s: %w", b.stream, unit, err)
		}
		if started {
			implementing++
		}
	}
	return nil
}

// follow moves a unit between implementing and waiting as its mason's thread
// says, and returns the unit's state. An implementing unit whose mason's
// latest turn asked a question moves to waiting; a waiting
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
		q := askedBy(asked, th.Identity.ThreadID, last.Request.TurnID)
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

// read returns the workstream's unit states and sealed plan when it is
// building, and when it last started a unit.
func (m *masons) read(stream config.WorkstreamID) (building, bool, error) {
	feature, err := m.repository.Workflow(stream, trace.FeatureSubject)
	if err != nil || feature.Value != BuildingState {
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
	rank := map[config.WorkstreamID]int{}
	for _, p := range priorities {
		if p.Project == project {
			for i, stream := range p.Workstreams {
				rank[stream] = i + 1
			}
		}
	}
	order := func(stream config.WorkstreamID) int {
		if r, ok := rank[stream]; ok {
			return r
		}
		return len(rank) + 1
	}
	slices.SortFunc(streams, func(a, b building) int {
		return cmp.Or(cmp.Compare(order(a.stream), order(b.stream)), a.started.Compare(b.started), strings.Compare(string(a.stream), string(b.stream)))
	})
	return streams
}

// nextReady returns the first ready unit of the workstream in its plan's
// dependency order.
func nextReady(b building) (string, bool) {
	for _, u := range dependencyOrder(b.plan) {
		if b.states[trace.UnitSubject(u.ID)].Value == UnitReady {
			return u.ID, true
		}
	}
	return "", false
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
// turn. The unit's bundle is assembled and its workspace opened first: a
// unit whose spec no longer matches its seal, or whose workspace cannot be
// opened, stays ready, and start records why it is blocked and reports false.
// A unit whose state moved since it was read is left to the next pass.
func (m *masons) start(ctx context.Context, b building, unit string) (bool, error) {
	subject := trace.UnitSubject(unit)
	stays := fmt.Sprintf("unit %s stays ready", unit)
	mason, err := m.bundle(ctx, b.stream, unit)
	if errors.Is(err, bundle.ErrStaleSpec) {
		return false, m.block(ctx, b.stream, unit, subject+"-"+UnitReady, fmt.Sprintf("%s: its mason bundle cannot be assembled: %v", stays, err))
	}
	if err != nil {
		return false, err
	}
	w, base, err := newUnitWorkspaces(m.cfg).open(ctx, b.stream, unit)
	if err != nil && ctx.Err() == nil {
		return false, m.block(ctx, b.stream, unit, subject+"-"+UnitReady, fmt.Sprintf("%s: its workspace cannot be opened: %v", stays, err))
	}
	if err != nil {
		return false, err
	}
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: masonTransitionID(unit), Revision: 1, Project: m.repository.Project(), Workstream: b.stream, Unit: unit, At: m.s.now(), Actor: masonActor, Cause: subject + "-" + UnitReady}
	tr := trace.Transition{Header: h, Subject: subject, From: UnitReady, To: UnitImplementing,
		Reason: fmt.Sprintf("unit %s is the next ready unit of the plan of seal %d; its mason works in the unit's workspace on %s, created from %s at %s", unit, mason.Seal, w.Branch, featureBranch(b.stream), base)}
	if _, err := m.repository.Transact(ctx, trace.Transaction{ExpectedVersion: b.states[subject].Version, Transition: tr}); errors.Is(err, trace.ErrConflict) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	return true, m.enqueue(ctx, b.stream, unit, mason, tr)
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
	if _, _, err := newUnitWorkspaces(m.cfg).open(ctx, stream, unit); err != nil {
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
	return fmt.Sprintf("You are a mason of the %s project (%s). You build one unit of a ratified plan in a workspace of its own, whose files are your view. You hold no version control tool: the service records your work. Build what the sealed spec says, for the criteria of your unit, and put in place the proofs the plan names for them. When your view and the spec do not settle something you must know, call %s: your unit waits for the answer, which arrives as your next turn, and your workspace is kept.", p.Name, p.Upstream, questions.AskTool)
}

func masonPrompt(m bundle.Mason) string {
	return fmt.Sprintf(`Build unit %s of this workstream.

Your view holds the project's files as the feature branch had them when the unit started, with the work done on the unit since. Make each of the unit's criteria below hold, and put in place and pass the proof the plan names for it. Stay within the unit's footprint. The spec below is the one the owner ratified; build against it.

%s`, m.Unit, m.Render())
}
