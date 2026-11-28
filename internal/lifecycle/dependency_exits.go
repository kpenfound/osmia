package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/ghlog"
	"github.com/kpenfound/osmia/internal/router"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/kpenfound/osmia/internal/workspace"
)

// The triggers of the exits from a dependency wait whose upstream ended or is
// already in the project's base branch. Each is a trigger of the route span.
const (
	TriggerUpstreamTerminal   = "dep.upstream_terminal"
	TriggerUpstreamIntegrated = "dep.upstream_integrated"
	TriggerBaseUnavailable    = "dep.base_unavailable"
)

// BaseExitPoll is the cadence of the checks that look for an exit from a
// same-repo base wait.
const BaseExitPoll = "base-exit"

// The states of the decision about one dependency.
const (
	// DependencyKept: the dependency stays as it was, and the dependent keeps
	// waiting on it.
	DependencyKept = "kept"
	// DependencyWaived: a waiver in the dependent's trace drops the dependency.
	DependencyWaived = "waived"
	// DependencyAdopted: the upstream's changes are already in the base
	// branch, so the dependent goes on without waiting for a merge.
	DependencyAdopted = "adopted"
)

// DependencySubject is the workflow subject that records what was decided
// about the dependency of a workstream on upstream, a graph edge of the given
// type or BaseEdgeType. It lives in the dependent's own trace.
func DependencySubject(upstream config.WorkstreamID, edgeType string) string {
	return "dependency-" + string(upstream) + "-" + edgeType
}

// FetchUpstreamBase fetches the project's base branch through g and returns the
// commit it is at. It is the first half of the evidence both the owner's move
// onto upstream and the exits from waiting rest on: whether a branch's changes
// are already in the base branch.
func FetchUpstreamBase(ctx context.Context, cfg *config.Config, g workspace.Provider) (seal.Base, error) {
	remote, err := g.Remote(ctx, cfg.Project.Upstream)
	if err != nil {
		return seal.Base{}, err
	}
	commit, err := g.Fetch(ctx, remote, cfg.Project.BaseBranch)
	if err != nil {
		return seal.Base{}, err
	}
	return seal.Base{Remote: remote, Branch: cfg.Project.BaseBranch, Commit: commit}, nil
}

func (s *Controller) upstreamOf(repo *trace.Repository, from config.WorkstreamID) (*trace.Repository, bool) {
	if archive, archived := s.ArchivedWorkstream(string(from)); archived {
		if !archive.CleanupStartedAt.IsZero() || !archive.CleanedAt.IsZero() {
			return nil, false
		}
		other, err := s.Repository(archive.Project)
		return other, err == nil
	}
	if streams, err := repo.Workstreams(); err == nil && slices.Contains(streams, from) {
		return repo, true
	}
	_, _, found, api := s.ConversationTrace(string(from))
	return found, api == nil
}

func (s *Controller) observeIntegration(ctx context.Context, repo *trace.Repository, edge trace.Edge, out EdgeMergeStatus) (EdgeMergeStatus, error) {
	upstream, ok := s.upstreamOf(repo, edge.From)
	if !ok {
		return out, nil
	}
	feature, err := upstream.Workflow(edge.From, trace.FeatureSubject)
	if err != nil || feature.Value != AbandonedState && feature.Value != DeliveredState {
		return out, nil
	}
	if _, _, sealed, err := seal.Latest(upstream, edge.From); err != nil || !sealed {
		return out, nil
	}
	cfg := s.About(upstream)
	g, err := featureWorkspaces(cfg, upstream).Of(edge.From)
	if err != nil {
		return out, nil
	}
	tip, exists, err := g.Branch(ctx, featureBranch(edge.From))
	if err != nil || !exists {
		return out, ctx.Err()
	}
	if feature.Value == DeliveredState {
		if tip, err = DeliveredBaseTip(upstream, edge.From, tip); err != nil {
			return out, nil
		}
	}
	if base, err := BranchBase(ctx, upstream, edge.From, g, tip); err != nil || base.Commit == tip {
		return out, ctx.Err()
	}
	base, err := FetchUpstreamBase(ctx, cfg, g)
	if err != nil {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		ghlog.Logger(ctx).Warn("cannot fetch the base branch to check whether an upstream is integrated", "workstream", string(edge.From), "error", err.Error())
		return out, nil
	}
	integrated, err := g.Integrated(ctx, tip, base.Commit)
	if err != nil {
		return out, ctx.Err()
	}
	if integrated {
		out.Integrated = base.Commit
		out.IntegratedReference = fmt.Sprintf("%s@%s", cfg.Project.Upstream, base.Commit)
	}
	return out, nil
}

func dependencyDecided(repo *trace.Repository, stream config.WorkstreamID, subject, cause string) (bool, error) {
	transitions, err := trace.Read[trace.Transition](repo, stream)
	if err != nil {
		return false, err
	}
	var last trace.Transition
	for _, t := range transitions {
		if t.Subject == subject {
			last = t
		}
	}
	return last.Subject != "" && last.Cause == cause, nil
}

func (s *Controller) raiseEdgeExits(ctx context.Context, repo *trace.Repository, stream config.WorkstreamID, observed EdgeObservation) error {
	feature, err := repo.Workflow(stream, trace.FeatureSubject)
	if err != nil || feature.Value == AbandonedState {
		return err
	}
	edges, err := EffectiveIncomingEdges(repo, stream)
	if err != nil {
		return err
	}
	for _, e := range edges {
		if feature.Value == DeliveredState && e.Type != trace.EdgeMergeOrder {
			continue
		}
		i := slices.IndexFunc(observed.Edges, func(m EdgeMergeStatus) bool { return m.Upstream == e.From && m.Type == e.Type })
		if i < 0 || observed.Edges[i].Merged {
			continue
		}
		st := observed.Edges[i]
		trigger, cause := "", ""
		switch {
		case st.Integrated != "":
			trigger, cause = TriggerUpstreamIntegrated, "upstream-integrated"
		case st.Ended != "":
			trigger, cause = TriggerUpstreamTerminal, "upstream-"+st.Ended
		default:
			continue
		}
		subject := DependencySubject(e.From, e.Type)
		if decided, err := dependencyDecided(repo, stream, subject, cause); err != nil {
			return err
		} else if decided {
			continue
		}
		state, err := repo.Workflow(stream, subject)
		if err != nil {
			return err
		}
		x := s.dependencyExit(repo, stream, state, dependencyCase{Upstream: e.From, Type: e.Type, Trigger: trigger, Cause: cause, Ended: st.Ended, Integrated: st.Integrated != "", Edge: &e, Status: st})
		if _, err := s.routeExit(ctx, x); err != nil {
			return err
		}
	}
	return nil
}

func (s *Controller) raiseBaseExit(ctx context.Context, repo *trace.Repository, stream config.WorkstreamID) error {
	if _, archived := s.ArchivedWorkstream(string(stream)); archived {
		return nil
	}
	if session, err := isSession(repo, stream); err != nil || session {
		return err
	}
	wait, err := repo.Workflow(stream, BaseWaitSubject)
	if err != nil || wait.Value != "waiting" {
		return err
	}
	feature, err := repo.Workflow(stream, trace.FeatureSubject)
	if err != nil || feature.Value == AbandonedState || feature.Value == DeliveredState {
		return err
	}
	dependency, err := repo.WorkstreamBase(stream)
	if err != nil || dependency.Base == "" {
		return err
	}
	parent, err := repo.Workflow(dependency.Base, trace.FeatureSubject)
	if err != nil || parent.Value != AbandonedState {
		return err
	}
	now := s.Now()
	cfg := s.About(repo)
	if last, ok := s.Polls.Last(repo.Project(), stream, BaseExitPoll); ok && now.Sub(last.At) < cfg.Project.PullRequestPollInterval() {
		return nil
	}
	s.Polls.Record(repo.Project(), stream, BaseExitPoll, PullRequestAttempt{At: now})
	g, err := featureWorkspaces(cfg, repo).Of(stream)
	if err != nil {
		return err
	}
	held, api := HeldBaseCommit(ctx, repo, g, stream, dependency.Base)
	if api != nil {
		return nil
	}
	upstream, err := FetchUpstreamBase(ctx, cfg, g)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		ghlog.Logger(ctx).Warn("cannot fetch the base branch to check whether an abandoned base is integrated", "workstream", string(stream), "error", err.Error())
		return nil
	}
	integrated := false
	if held != "" {
		if integrated, err = g.Integrated(ctx, held, upstream.Commit); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			integrated = false
		}
	}
	c := dependencyCase{Upstream: dependency.Base, Type: BaseEdgeType, Trigger: TriggerBaseUnavailable, Cause: "base-abandoned", Ended: EndedAbandoned, Wait: wait, BaseUpstream: &upstream, Held: held}
	if integrated {
		c.Trigger, c.Cause, c.Integrated = TriggerBaseIntegrated, "base-integrated", true
	}
	subject := DependencySubject(c.Upstream, c.Type)
	if decided, err := dependencyDecided(repo, stream, subject, c.Cause); err != nil || decided {
		return err
	}
	state, err := repo.Workflow(stream, subject)
	if err != nil {
		return err
	}
	_, err = s.routeExit(ctx, s.dependencyExit(repo, stream, state, c))
	return err
}

type dependencyCase struct {
	Upstream     config.WorkstreamID
	Type         string
	Trigger      string
	Cause        string
	Ended        string
	Integrated   bool
	Edge         *trace.Edge
	Status       EdgeMergeStatus
	Wait         trace.WorkflowState
	BaseUpstream *seal.Base
	Held         string
	Authority    *router.Authority
	Actor        trace.Actor
	Note         string
}

const ownerEnded = "owner"

const managerEnded = trace.Manager

// storedManagerDropCause is a stored name of the manager role: the cause of
// the exit that drops a dependency on the manager's request. Saved route
// records are named from an occurrence key that holds it
// (router.OccurrenceKey), and router.Router.Route reads the record of that key
// to return a drop it already decided instead of deciding it again, so the
// value must not change.
const storedManagerDropCause = "chief-drop"

func (c dependencyCase) base() bool { return c.Type == BaseEdgeType }

func (s *Controller) dependencyExit(repo *trace.Repository, stream config.WorkstreamID, state trace.WorkflowState, c dependencyCase) router.Exit {
	subject := DependencySubject(c.Upstream, c.Type)
	id := fmt.Sprintf("%s-%d", subject, state.Version+1)
	what := fmt.Sprintf("hard_block edge from %s", c.Upstream)
	switch {
	case c.base():
		what = fmt.Sprintf("base workstream %s", c.Upstream)
	case c.Type == trace.EdgeMergeOrder:
		what = fmt.Sprintf("merge_order edge from %s", c.Upstream)
	}
	actor := c.Actor
	if actor == (trace.Actor{}) {
		actor = foremanActor
	}
	keep := router.Target{Name: DependencyKept, Stay: true,
		Description: "Keep the dependency: the dependent keeps waiting on it, as before.",
		Reason:      fmt.Sprintf("the %s stays: its upstream ended (%s) and nothing decided to drop it", what, trace.DisplayRole(c.Ended))}
	waive := router.Target{Name: DependencyWaived, Bar: router.BarStrict,
		Description: "Drop the dependency: write a waiver in the dependent's own trace, so this one dependency stops blocking it. The dependent's other dependencies and the shared project spec stay unchanged.",
		Reason:      fmt.Sprintf("the %s was dropped: its upstream ended (%s) and the dependent goes on without it", what, trace.DisplayRole(c.Ended)),
		Corroborate: []router.Condition{{Question: "works_without", AtLeast: router.LevelStrict}}}
	adopt := router.Target{Name: DependencyAdopted,
		Description: "Go on: the upstream's changes are already in the project's base branch, so the dependent no longer waits for a merge.",
		Reason:      fmt.Sprintf("the %s is satisfied: the upstream's changes are already in the base branch", what)}
	intent, entries, facts := s.dependencyIntent(repo, stream, c)
	facts["dependency_type"], facts["ended"], facts["integrated"] = c.Type, trace.DisplayRole(c.Ended), c.Integrated
	exit := router.Exit{
		Repo: repo, Project: repo.Project(), Workstream: stream, Machine: DependencyMachine,
		Subject: subject, From: state.Value, Version: state.Version, Trigger: c.Trigger, Cause: c.Cause, HeaderCause: c.Cause, Actor: actor, Note: c.Note,
		At:           s.Now(),
		TransitionID: func(router.Target) string { return id },
		Authority:    c.Authority,
		Evidence: router.Evidence{
			Intent:  intent,
			Facts:   facts,
			Records: append([]router.Entry{{Kind: "dependency", ID: string(c.Upstream), Text: fmt.Sprintf("the %s: %s", what, c.evidence())}}, entries...),
		},
		Prereq: router.Prerequisites{Other: func(ctx context.Context, t router.Target) (string, error) {
			if t.Name == DependencyKept {
				return "", nil
			}
			return s.dependencyUnmet(repo, stream, c)
		}},
	}
	switch {
	case c.Integrated:
		exit.Default = DependencyAdopted
		keep.Bar = router.BarModest
		keep.Reason = fmt.Sprintf("Jev kept the dependent waiting on the %s although the upstream's changes look integrated", what)
		keep.Corroborate = []router.Condition{{Question: "integration_in_doubt", AtLeast: router.LevelModest}}
		keep.Description = "Keep the dependency: the records show the changes in the base branch are not what the dependent needs, so it keeps waiting."
		exit.Targets = []router.Target{adopt, keep}
		exit.Questions = []router.Question{question("integration_in_doubt", "The records show that the base branch holds a different version of the upstream's changes than the dependent builds on: they were reverted, or only part of them reached it.")}
		exit.Enter = c.adopt(s, repo, stream)
	default:
		exit.Default = DependencyKept
		exit.Targets = []router.Target{keep, waive}
		exit.Questions = []router.Question{
			question("works_without", "The dependent's slice can be built and delivered without what this dependency would have brought, judging from its contracts, its plan and the records of the upstream."),
			question("replaced", "Another workstream or pull request in the records delivers what the upstream was to deliver."),
		}
		exit.Entering = map[string]router.EffectFunc{DependencyWaived: c.waive(s, repo, stream)}
	}
	return exit
}

func (c dependencyCase) evidence() string {
	switch {
	case c.Integrated && c.base():
		return fmt.Sprintf("it was abandoned and the base branch already holds its changes at %s", c.Held)
	case c.Integrated:
		return fmt.Sprintf("its changes are already in the base branch at %s", c.Status.Integrated)
	case c.Ended == EndedClosed:
		return "its pull request was closed without merging"
	case c.Ended == ownerEnded:
		return "the owner dropped it"
	case c.Ended == managerEnded:
		return "the manager dropped it"
	}
	return "it was abandoned without being delivered"
}

func (s *Controller) dependencyUnmet(repo *trace.Repository, stream config.WorkstreamID, c dependencyCase) (string, error) {
	feature, err := repo.Workflow(stream, trace.FeatureSubject)
	if err != nil {
		return "", err
	}
	if feature.Value == AbandonedState {
		return "the dependent was abandoned", nil
	}
	if waived, err := EdgeWaived(repo, stream, c.Upstream, c.Type); err != nil || waived {
		return "the dependency was already dropped", err
	}
	if c.base() {
		wait, err := repo.Workflow(stream, BaseWaitSubject)
		if err != nil {
			return "", err
		}
		if wait.Value != "waiting" {
			return "the dependent is no longer parked on its base", nil
		}
		return "", nil
	}
	observed, _, _, err := EdgeObservationAt(repo, stream)
	if err != nil {
		return "", err
	}
	if i := slices.IndexFunc(observed.Edges, func(m EdgeMergeStatus) bool { return m.Upstream == c.Upstream && m.Type == c.Type }); i >= 0 && observed.Edges[i].Merged {
		return "the upstream merged since", nil
	}
	edges, err := EffectiveIncomingEdges(repo, stream)
	if err != nil {
		return "", err
	}
	if !slices.ContainsFunc(edges, func(e trace.Edge) bool { return e.From == c.Upstream && e.Type == c.Type }) {
		return "the workgroup no longer names this dependency", nil
	}
	return "", nil
}

func waiverDocument(repo *trace.Repository, stream config.WorkstreamID, p router.Plan, c dependencyCase) (trace.Document, error) {
	waivers, revision, err := EdgeWaiversAt(repo, stream)
	if err != nil {
		return trace.Document{}, err
	}
	if waivers.Waives(c.Upstream, c.Type) {
		return trace.Document{}, router.Refusal("the dependency was already dropped")
	}
	waivers.Waivers = append(waivers.Waivers, EdgeWaiver{Upstream: c.Upstream, Type: c.Type, Ended: c.Ended, Reason: p.Reason,
		DecidedBy: p.Routing.DecidedBy, Judgment: p.Routing.Judgment, Trigger: p.Exit.Trigger, At: p.At})
	data, err := json.Marshal(waivers)
	if err != nil {
		return trace.Document{}, err
	}
	h := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: EdgeWaiverDocument, Revision: revision + 1, Project: repo.Project(), Workstream: stream, At: p.At, Actor: p.Exit.Actor, Cause: p.TransitionID}
	return trace.Document{Header: h, Path: EdgeWaiverPath, Content: string(data)}, nil
}

func (c dependencyCase) waive(s *Controller, repo *trace.Repository, stream config.WorkstreamID) router.EffectFunc {
	return func(ctx context.Context, p router.Plan) (router.Effect, error) {
		doc, err := waiverDocument(repo, stream, p, c)
		if err != nil {
			return router.Effect{}, err
		}
		effect := router.Effect{Documents: []trace.Document{doc}, Events: []trace.Event{trace.Notice(p.TransitionID, waitKind(c), p.Reason)}}
		if c.base() {
			if c.BaseUpstream == nil {
				return router.Effect{}, router.Refusal("the base branch to continue from is not known")
			}
			observation, err := baseObservationDocument(repo, stream, *c.BaseUpstream, p)
			if err != nil {
				return router.Effect{}, err
			}
			effect.Documents = append(effect.Documents, observation)
			effect.With, err = releaseBaseWait(repo, stream, p)
			return effect, err
		}
		observed, _, _, err := EdgeObservationAt(repo, stream)
		if err != nil {
			return router.Effect{}, err
		}
		effect.With, err = releaseEdgeWait(repo, stream, c, observed, p)
		return effect, err
	}
}

func (c dependencyCase) adopt(s *Controller, repo *trace.Repository, stream config.WorkstreamID) router.EffectFunc {
	return func(ctx context.Context, p router.Plan) (router.Effect, error) {
		effect := router.Effect{Events: []trace.Event{trace.Notice(p.TransitionID, waitKind(c), p.Reason)}}
		if c.base() {
			if c.BaseUpstream == nil {
				return router.Effect{}, router.Refusal("the base branch to continue from is not known")
			}
			observation, err := baseObservationDocument(repo, stream, *c.BaseUpstream, p)
			if err != nil {
				return router.Effect{}, err
			}
			effect.Documents = []trace.Document{observation}
			effect.With, err = releaseBaseWait(repo, stream, p)
			return effect, err
		}
		observed, revision, _, err := EdgeObservationAt(repo, stream)
		if err != nil {
			return router.Effect{}, err
		}
		merged := EdgeMergeStatus{Upstream: c.Upstream, Type: c.Type, Merged: true, Reference: c.Status.IntegratedReference, Commit: c.Status.Integrated}
		var next EdgeObservation
		replaced := false
		for _, m := range observed.Edges {
			if m.Upstream == c.Upstream && m.Type == c.Type {
				next.Edges, replaced = append(next.Edges, merged), true
				continue
			}
			next.Edges = append(next.Edges, m)
		}
		if !replaced {
			next.Edges = append(next.Edges, merged)
		}
		data, err := json.Marshal(next)
		if err != nil {
			return router.Effect{}, err
		}
		h := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: EdgeObservationDocument, Revision: revision + 1, Project: repo.Project(), Workstream: stream, At: p.At, Actor: p.Exit.Actor, Cause: p.TransitionID}
		effect.Documents = []trace.Document{{Header: h, Path: EdgeObservationPath, Content: string(data)}}
		effect.With, err = releaseEdgeWait(repo, stream, c, next, p)
		return effect, err
	}
}

func waitKind(c dependencyCase) string {
	if c.base() {
		return "base"
	}
	return "edge"
}

func releaseEdgeWait(repo *trace.Repository, stream config.WorkstreamID, c dependencyCase, observed EdgeObservation, p router.Plan) ([]trace.Transaction, error) {
	state, err := repo.Workflow(stream, EdgeWaitSubject)
	if err != nil || state.Value != "waiting" {
		return nil, err
	}
	edges, err := EffectiveIncomingEdges(repo, stream)
	if err != nil {
		return nil, err
	}
	var remaining []trace.Edge
	for _, e := range edges {
		if e.From != c.Upstream || e.Type != c.Type {
			remaining = append(remaining, e)
		}
	}
	if _, found := FirstUnresolvedEdge(EdgesOfType(remaining, trace.EdgeHardBlock), observed); found {
		return nil, nil
	}
	id := fmt.Sprintf("%s-%d", EdgeWaitSubject, state.Version+1)
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: repo.Project(), Workstream: stream, At: p.At, Actor: foremanActor, Cause: "edge-availability"}
	reason := "the workstream's blocking edges resolved"
	return []trace.Transaction{{ExpectedVersion: state.Version, Transition: codeDefault(trace.Transition{Header: h, Subject: EdgeWaitSubject, From: state.Value, To: "available", Reason: reason}), Events: []trace.Event{trace.Notice(id, "edge", reason)}}}, nil
}

func releaseBaseWait(repo *trace.Repository, stream config.WorkstreamID, p router.Plan) ([]trace.Transaction, error) {
	state, err := repo.Workflow(stream, BaseWaitSubject)
	if err != nil || state.Value != "waiting" {
		return nil, err
	}
	id := fmt.Sprintf("%s-%d", BaseWaitSubject, state.Version+1)
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: repo.Project(), Workstream: stream, At: p.At, Actor: foremanActor, Cause: "base-availability"}
	reason := "the workstream base is available again"
	return []trace.Transaction{{ExpectedVersion: state.Version, Transition: codeDefault(trace.Transition{Header: h, Subject: BaseWaitSubject, From: state.Value, To: "available", Reason: reason}), Events: []trace.Event{trace.Notice(id, "base", reason)}}}, nil
}

func baseObservationDocument(repo *trace.Repository, stream config.WorkstreamID, upstream seal.Base, p router.Plan) (trace.Document, error) {
	data, err := json.Marshal(BaseObservation{Base: upstream})
	if err != nil {
		return trace.Document{}, err
	}
	_, revision, err := BaseObservationAt(repo, stream)
	if err != nil {
		return trace.Document{}, err
	}
	h := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: BaseObservationDocument, Revision: revision + 1, Project: repo.Project(), Workstream: stream, At: p.At, Actor: p.Exit.Actor, Cause: p.TransitionID}
	return trace.Document{Header: h, Path: "base-observation.json", Content: string(data)}, nil
}

// DropDependencyRequest is the owner's decision to drop one dependency of a
// workstream: the upstream workstream it names and, when the upstream is
// ambiguous, the type of the dependency (an edge type of the graph, or "base").
type DropDependencyRequest struct {
	Upstream config.WorkstreamID `json:"upstream"`
	Type     string              `json:"type,omitempty"`
}

// DropDependencyResponse says which dependency was dropped.
type DropDependencyResponse struct {
	Project    config.ProjectID    `json:"project"`
	Workstream config.WorkstreamID `json:"workstream"`
	Upstream   config.WorkstreamID `json:"upstream"`
	Type       string              `json:"type"`
}

// DropDependency is the owner's decision to drop a dependency of the workstream
// named by raw. It is a pinned move: the router applies the drop exactly as it
// applies Jev's, through the same effect, without asking Jev, and records the
// owner as the decider. It never changes the shared project spec or the
// workgroup. It is refused unless the dependency still binds the workstream.
func (s *Controller) DropDependency(ctx context.Context, raw string, req DropDependencyRequest) (DropDependencyResponse, *APIError) {
	project, stream, repo, api := s.ConversationTrace(raw)
	if api != nil {
		return DropDependencyResponse{}, api
	}
	return s.DropDependencyOf(ctx, project, stream, repo, req)
}

// DropDependencyOf is DropDependency for a workstream whose project and trace
// are known.
func (s *Controller) DropDependencyOf(ctx context.Context, project config.ProjectID, stream config.WorkstreamID, repo *trace.Repository, req DropDependencyRequest) (DropDependencyResponse, *APIError) {
	return s.dropDependency(ctx, project, stream, repo, req, ownerActor, &router.Authority{Kind: router.AuthorityOwner, Target: DependencyWaived, Ref: "owner request to drop the dependency"})
}

func (s *Controller) dropDependency(ctx context.Context, project config.ProjectID, stream config.WorkstreamID, repo *trace.Repository, req DropDependencyRequest, actor trace.Actor, authority *router.Authority) (DropDependencyResponse, *APIError) {
	failed := &APIError{Code: Internal, Message: fmt.Sprintf("cannot drop the dependency of workstream %s; check the trace repository", stream)}
	feature, err := repo.Workflow(stream, trace.FeatureSubject)
	if err != nil {
		return DropDependencyResponse{}, failed
	}
	if feature.Value == AbandonedState {
		return DropDependencyResponse{}, &APIError{Code: Conflict, Message: fmt.Sprintf("workstream %s was abandoned", stream)}
	}
	cause, ended := "owner-drop", ownerEnded
	if authority.Kind == router.AuthorityManager {
		cause, ended = storedManagerDropCause, managerEnded
	}
	c := dependencyCase{Upstream: req.Upstream, Type: req.Type, Trigger: TriggerUpstreamTerminal, Cause: cause, Ended: ended, Actor: actor, Authority: authority}
	if dependency, err := repo.WorkstreamBase(stream); err != nil {
		return DropDependencyResponse{}, failed
	} else if dependency.Base != "" && dependency.Base == req.Upstream && (req.Type == "" || req.Type == BaseEdgeType) {
		c.Type = BaseEdgeType
	}
	if c.base() {
		wait, err := repo.Workflow(stream, BaseWaitSubject)
		if err != nil {
			return DropDependencyResponse{}, failed
		}
		if wait.Value != "waiting" {
			return DropDependencyResponse{}, &APIError{Code: Conflict, Message: fmt.Sprintf("workstream %s is not parked on its base", stream)}
		}
		cfg := s.About(repo)
		g, err := featureWorkspaces(cfg, repo).Of(stream)
		if err != nil {
			return DropDependencyResponse{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot open the workspaces of workstream %s; check the clone", stream)}
		}
		upstream, err := FetchUpstreamBase(ctx, cfg, g)
		if err != nil {
			return DropDependencyResponse{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot fetch %s from %s; check the clone's remotes", cfg.Project.BaseBranch, cfg.Project.Upstream)}
		}
		c.Wait, c.BaseUpstream = wait, &upstream
	} else {
		edges, err := EffectiveIncomingEdges(repo, stream)
		if err != nil {
			return DropDependencyResponse{}, failed
		}
		observed, _, _, err := EdgeObservationAt(repo, stream)
		if err != nil {
			return DropDependencyResponse{}, failed
		}
		var edge *trace.Edge
		for _, e := range edges {
			if e.From == req.Upstream && (req.Type == "" || e.Type == req.Type) && !AllEdgesResolved(observed, []trace.Edge{e}) {
				edge = &e
				break
			}
		}
		if edge == nil {
			return DropDependencyResponse{}, &APIError{Code: Conflict, Message: fmt.Sprintf("workstream %s has no unresolved dependency on %s; read the inbox again", stream, req.Upstream)}
		}
		c.Edge, c.Type = edge, edge.Type
		if i := slices.IndexFunc(observed.Edges, func(m EdgeMergeStatus) bool { return m.Upstream == edge.From && m.Type == edge.Type }); i >= 0 {
			c.Status = observed.Edges[i]
			if c.Status.Ended != "" {
				c.Ended = c.Status.Ended
			}
		}
	}
	state, err := repo.Workflow(stream, DependencySubject(c.Upstream, c.Type))
	if err != nil {
		return DropDependencyResponse{}, failed
	}
	res, err := s.routeExit(ctx, s.dependencyExit(repo, stream, state, c))
	switch {
	case err != nil && errors.Is(err, trace.ErrConflict), err == nil && !res.Committed:
		return DropDependencyResponse{}, &APIError{Code: Conflict, Message: fmt.Sprintf("workstream %s changed while dropping its dependency on %s; read the inbox again", stream, req.Upstream)}
	case err != nil:
		return DropDependencyResponse{}, failed
	}
	return DropDependencyResponse{Project: project, Workstream: stream, Upstream: c.Upstream, Type: c.Type}, nil
}
