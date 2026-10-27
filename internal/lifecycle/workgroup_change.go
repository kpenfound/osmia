package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"

	"github.com/kpenfound/osmia/internal/amendment"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/trace"
)

const ChangeBoundarySubject = "change-boundary"
const RequestChangeTool = "request_change"
const DecideChangeTool = "decide_change"
const ChangeGuidance = "When the owner asks to amend or cancel, call request_change with a stable key, kind amend or cancel, scope workgroup or workstream, and the requested change. The committee debates only the delta. Present the single impact summary; call decide_change only from an explicit owner approval or rejection of its current revision. For cancellation ask at that time whether open PRs are closed or left as drafts. Merged work stays intact."

// ChangeRequest is an owner request in the shared chat. Key makes a retried
// Request idempotent; scope selects this member or the entire workgroup.
type ChangeRequest struct {
	Key        string              `json:"key"`
	Kind       string              `json:"kind"`
	Scope      string              `json:"scope"`
	Workstream config.WorkstreamID `json:"workstream,omitempty"`
	Change     string              `json:"change"`
}

// ChangeDecision approves precisely the presented impact. PR disposition is
// selected at cancellation, and never authorizes a push or a merge.
type ChangeDecision struct {
	Revision     int    `json:"revision"`
	Decision     string `json:"decision"`
	PullRequests string `json:"pull_requests,omitempty"`
}
type DeltaAssessment struct {
	Member     string   `json:"member"`
	Objections []string `json:"objections"`

	Note string `json:"note"`
}

// WorkgroupChange is the journal of the one delta debate and owner decision.
// Before is immutable approved text; After is proposed text until approval.
// Applied members reconcile once before the shared project spec is published.
type WorkgroupChange struct {
	ID        string                                      `json:"id"`
	Workgroup config.WorkgroupID                          `json:"workgroup"`
	Origin    config.WorkstreamID                         `json:"origin"`
	Requester *trace.ProjectAmendment                     `json:"requester,omitempty"`
	Host      config.WorkstreamID                         `json:"host"`
	Request   ChangeRequest                               `json:"request"`
	State     string                                      `json:"state"`
	Revision  int                                         `json:"revision"`
	Before    trace.ProjectSpec                           `json:"before"`
	After     trace.ProjectSpec                           `json:"after"`
	Result    *trace.ProjectSpec                          `json:"result,omitempty"`
	Members   []amendment.Member                          `json:"members"`
	Impact    amendment.Impact                            `json:"impact"`
	Debate    []DeltaAssessment                           `json:"debate,omitempty"`
	Reviewers int                                         `json:"reviewers"`
	Reply     string                                      `json:"reply,omitempty"`
	Decision  *ChangeDecision                             `json:"decision,omitempty"`
	Applied   []config.WorkstreamID                       `json:"applied,omitempty"`
	Followups map[config.WorkstreamID]config.WorkstreamID `json:"followups,omitempty"`
	Failure   string                                      `json:"failure,omitempty"`
}

func WorkgroupChangePath(id string) string {
	return "tools/workgroup-change-" + id + ".json"
}
func WorkgroupChangeDocument(id string) string {
	return "workgroup-change-" + id
}
func ChangeIdentity(id string) string {
	if len(id) <= 128 {
		return id
	}
	return trace.EventID("workgroup-change", id)
}
func ChangeID(key string) string {
	if len(key) <= 32 {
		return key
	}
	return strings.TrimPrefix(trace.EventID("change-key", key), "event_")[:32]
}
func ReadChanges(repo *trace.Repository, host config.WorkstreamID) ([]WorkgroupChange, error) {
	docs, err := trace.Read[trace.Document](repo, host)
	if err != nil {
		return nil, err
	}
	latest := map[string]trace.Document{}
	order := []string{}
	for _, d := range docs {
		if strings.HasPrefix(d.ID, "workgroup-change-") {
			if _, ok := latest[d.ID]; !ok {
				order = append(order, d.ID)
			}
			if d.Revision > latest[d.ID].Revision {
				latest[d.ID] = d
			}
		}
	}
	out := []WorkgroupChange{}
	for _, id := range order {
		var c WorkgroupChange
		if err := json.Unmarshal([]byte(latest[id].Content), &c); err != nil {
			return nil, err
		}
		c.Revision = latest[id].Revision
		out = append(out, c)
	}
	return out, nil
}
func (s *Controller) SaveChange(ctx context.Context, repo *trace.Repository, c *WorkgroupChange) error {
	c.Revision++
	data, err := json.Marshal(c)
	if err != nil {
		return err
	}
	err = repo.RecordDocuments(ctx, []trace.Document{{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: WorkgroupChangeDocument(c.ID), Revision: c.Revision, Project: repo.Project(), Workstream: c.Host, At: s.Now(), Actor: amendmentActor, Cause: WorkgroupChangeDocument(c.ID)}, Path: WorkgroupChangePath(c.ID), Content: string(data)}})
	if err != nil {
		c.Revision--
	}
	return err
}

// ChangeContext uses the existing chief routing so the original design address
// and every member resolve to exactly the same authoritative journal and chat.
func (s *Controller) ChangeContext(ctx context.Context, raw string) (*trace.Repository, trace.Workgroup, config.WorkstreamID, func(), error) {
	_, stream, repo, api := s.ConversationTrace(raw)
	if api != nil {
		return nil, trace.Workgroup{}, "", func() {
		}, errors.New(api.Message)
	}
	wt, host, _, close, err := s.ChiefRepository(ctx, repo, stream)
	if err != nil {
		return nil, trace.Workgroup{}, "", close, err
	}
	if err := s.Step("change-context-routed:" + string(stream)); err != nil {
		close()
		return nil, trace.Workgroup{}, "", func() {
		}, err
	}
	groups, _, err := trace.Workgroups(wt)
	if err != nil {
		close()
		return nil, trace.Workgroup{}, "", func() {
		}, err
	}
	g, ok := trace.WorkgroupOf(groups, host)
	if !ok {
		close()
		// A join may publish its member locator and retract the old group
		// after chiefRepository selected the local route. Resolve afresh only
		// when that locator exists; shared binding validation still applies.
		id, err := repo.CrossRepoWorkgroupOf(stream)
		if err != nil {
			return nil, g, "", func() {
			}, err
		}
		pending, err := repo.PendingCrossRepoWorkgroup(stream)
		if err != nil {
			return nil, g, "", func() {
			}, err
		}
		if (id != "" || pending != "") && wt == repo {
			wt, host, _, close, err = s.ChiefRepository(ctx, repo, stream)
			if err != nil {
				return nil, g, "", close, err
			}
			groups, _, err = trace.Workgroups(wt)
			if err == nil {
				g, ok = trace.WorkgroupOf(groups, host)
			}
			if err == nil && ok {
				return wt, g, stream, close, nil
			}
			close()
			if err != nil {
				return nil, g, "", func() {
				}, err
			}
		}
		return nil, g, "", func() {
		}, fmt.Errorf("workstream has no workgroup")
	}
	return wt, g, stream, close, nil
}
func ChangeSharedRepository(g trace.Workgroup, repo *trace.Repository) bool {
	id, valid := trace.WorkgroupProjectID(repo.Project())
	return valid && id == g.ID
}
func (s *Controller) ChangeMembers(g trace.Workgroup, repo *trace.Repository) []config.WorkstreamID {
	if ChangeSharedRepository(g, repo) {
		return slices.Clone(g.Members[1:])
	}
	return slices.Clone(g.Members)
}

// ChangeMemberRepository grants effects only to live bindings. Journal access
// and pinned snapshots never substitute for unavailable member prerequisites.
func (s *Controller) ChangeMemberRepository(wt *trace.Repository, stream config.WorkstreamID) (*trace.Repository, error) {
	retiredError := func() error {
		retired, err := wt.MemberRetired(stream)
		if err != nil {
			return err
		}
		if retired {
			return fmt.Errorf("%w: workstream %s", trace.ErrMemberRetired, stream)
		}
		return nil
	}
	if err := retiredError(); err != nil {
		return nil, err
	}
	_, _, repo, api := s.ConversationTrace(string(stream))
	if api != nil {
		if err := retiredError(); err != nil {
			return nil, err
		}
		return nil, errors.New(api.Message)
	}
	return repo, nil
}

var ChangeApplicationLocks sync.Map

func (s *Controller) ChangeApplicationLock(group config.WorkgroupID) *sync.RWMutex {
	key := s.Current().Root.String() + ":" + string(group)
	value, _ := ChangeApplicationLocks.LoadOrStore(key, &sync.RWMutex{})
	return value.(*sync.RWMutex)
}

// ChangeArchiveGuard serializes retirement with approved member effects.
// Callers hold no member, trace, store or operation lock. Application may
// Reconcile external operations, so archive refuses an active application.
func (s *Controller) ChangeArchiveGuard(group config.WorkgroupID) (func(), *APIError) {
	lock := s.ChangeApplicationLock(group)
	if !lock.TryRLock() {
		return func() {
		}, &APIError{Code: Conflict, Message: "an approved change is being applied to this workgroup; retry"}
	}
	return lock.RUnlock, nil
}
func (s *Controller) HoldChange(ctx context.Context, wt *trace.Repository, c *WorkgroupChange, reason string) error {
	if c.Failure == reason {
		return nil
	}
	c.Failure = reason
	return s.SaveChange(ctx, wt, c)
}
func ChangeProjectSpec(g trace.Workgroup, repo *trace.Repository) (trace.ProjectSpec, bool, error) {
	var spec trace.ProjectSpec
	var found bool
	var err error
	if ChangeSharedRepository(g, repo) {
		spec, _, found, err = repo.CrossRepoProjectSpec(g.ID)
	} else {
		spec, _, found, err = repo.ProjectSpecOf(g.Members[len(g.Members)-1])
	}
	return spec, found, err
}
func (s *Controller) SnapshotChange(g trace.Workgroup, repo *trace.Repository) (trace.ProjectSpec, []amendment.Member, error) {
	spec, found, err := ChangeProjectSpec(g, repo)
	if err != nil {
		return spec, nil, err
	}
	members := []amendment.Member{}
	for _, id := range s.ChangeMembers(g, repo) {
		r, err := s.ChangeMemberRepository(repo, id)
		if err != nil {
			return spec, nil, err
		}
		state, err := r.Workflow(id, trace.FeatureSubject)
		if err != nil {
			return spec, nil, err
		}
		states, err := r.WorkflowStates(id)
		if err != nil {
			return spec, nil, err
		}
		m := amendment.Member{Workstream: id, State: state.Value, Units: map[string]string{}}
		if _, merged, err := deliveryMerge(r, id); err != nil {
			return spec, nil, err
		} else if merged {
			m.Merged = true
		}
		if p, published, err := currentOwnPublication(r, id); err != nil {
			return spec, nil, err
		} else if published {
			m.PullRequest = p.PullRequest
			outcome, rev, err := ownPullRequestOutcomeAt(r, id)
			if err != nil {
				return spec, nil, err
			}
			if rev > 0 && outcome.PullRequest == p.PullRequest && outcome.Outcome == ownPullRequestOutcomeMerged {
				m.Merged = true
			}
		}
		if !found {
			text, graph, err := MemberSpecAndPlan(r, id)
			// Cancellation during planning has no spec to compare.
			if err != nil {
				text = ""
				graph = `{"version":2,"units":[]}`
			}
			spec.Behavior = "Workstream brief"
			spec.Slices = append(spec.Slices, trace.Slice{Workstream: id, Spec: text, Plan: graph})
		}
		if sl, ok := trace.SliceFor(spec, id); ok {
			p, err := plan.Parse([]byte(sl.Plan))
			if err != nil {
				return spec, nil, err
			}
			for _, u := range p.Units {
				m.Units[u.ID] = states[trace.UnitSubject(u.ID)].Value
			}
		}
		members = append(members, m)
	}
	return spec, members, nil
}
func (s *Controller) RequestChange(ctx context.Context, raw string, req ChangeRequest) (WorkgroupChange, *APIError) {
	if !handInKey.MatchString(req.Key) || strings.TrimSpace(req.Change) == "" || (req.Kind != "amend" && req.Kind != "cancel") || (req.Scope != "workgroup" && req.Scope != "workstream") {
		return WorkgroupChange{}, &APIError{Code: Validation, Message: "key, amend or cancel kind, workgroup or workstream scope, and change are required"}
	}
	if req.Kind == "cancel" {
		_, stream, _, api := s.ConversationTrace(raw)
		if api != nil {
			return WorkgroupChange{}, api
		}
		// Until join completes, a member may still route to its temporary
		// singleton. Neither cancellation scope may act on that grouping.
		for _, target := range []config.WorkstreamID{stream, req.Workstream} {
			if target == "" {
				continue
			}
			if joining, err := s.Intake().SelectionJoiningMember(target); err != nil {
				return WorkgroupChange{}, &APIError{Code: Internal, Message: err.Error()}
			} else if joining {
				return WorkgroupChange{}, &APIError{Code: Conflict, Message: "the workgroup is still joining its owner-selected members; cancel the whole workgroup after it has joined"}
			}
		}
	}
	wt, g, origin, close, err := s.ChangeContext(ctx, raw)
	if err != nil {
		return WorkgroupChange{}, &APIError{Code: Conflict, Message: err.Error()}
	}
	defer close()
	if req.Workstream != "" {
		if !slices.Contains(s.ChangeMembers(g, wt), req.Workstream) {
			return WorkgroupChange{}, &APIError{Code: Validation, Message: "target is not a member"}
		}
		origin = req.Workstream
	}
	lock := s.GroupApprovalLock(g.ID)
	lock.Lock()
	defer lock.Unlock()
	changes, err := ReadChanges(wt, g.Members[0])
	if err != nil {
		return WorkgroupChange{}, &APIError{Code: Internal, Message: err.Error()}
	}
	for _, c := range changes {
		if c.Request.Key == req.Key {
			if !reflect.DeepEqual(req, c.Request) {
				return c, &APIError{Code: Conflict, Message: "this key already names another change"}
			}
			return c, nil
		}
		if c.State != "applied" && c.State != "rejected" && c.State != "failed" {
			return c, &APIError{Code: Conflict, Message: "decide the pending workgroup change first"}
		}
	}
	before, members, err := s.SnapshotChange(g, wt)
	if err != nil {
		return WorkgroupChange{}, &APIError{Code: Internal, Message: err.Error()}
	}
	if req.Kind == "amend" || req.Scope == "workstream" && len(members) > 1 {
		for _, m := range members {
			_, _, r, api := s.ConversationTrace(string(m.Workstream))
			if api != nil {
				return WorkgroupChange{}, api
			}
			if _, _, approved, err := seal.Latest(r, m.Workstream); err != nil {
				return WorkgroupChange{}, &APIError{Code: Internal, Message: err.Error()}
			} else if !approved {
				if req.Kind == "cancel" {
					return WorkgroupChange{}, &APIError{Code: Conflict, Message: FixedProjectsMessage}
				}
				return WorkgroupChange{}, &APIError{Code: Conflict, Message: "ratify the project spec before requesting a delta amendment"}
			}
		}
	}
	c := WorkgroupChange{ID: ChangeID(req.Key), Workgroup: g.ID, Origin: origin, Host: g.Members[0], Request: req, State: "drafting", Before: before, Members: members, Reviewers: s.Current().Capacity.Committee, Followups: map[config.WorkstreamID]config.WorkstreamID{}}
	if req.Kind == "cancel" {
		target := []config.WorkstreamID{origin}
		if req.Scope == "workgroup" {
			target = s.ChangeMembers(g, wt)
		}
		if !slices.Contains(s.ChangeMembers(g, wt), origin) && req.Scope == "workstream" {
			return c, &APIError{Code: Validation, Message: "select a member workstream to cancel"}
		}
		c.Impact = amendment.CancelImpact(target, before.Graph, members)
		c.State = "presenting"
	}
	if err := s.SaveChange(ctx, wt, &c); err != nil {
		return c, &APIError{Code: Internal, Message: err.Error()}
	}
	return c, nil
}
func (s *Controller) DecideChange(ctx context.Context, raw, id string, d ChangeDecision) (WorkgroupChange, *APIError) {
	wt, g, _, close, err := s.ChangeContext(ctx, raw)
	if err != nil {
		return WorkgroupChange{}, &APIError{Code: Conflict, Message: err.Error()}
	}
	defer close()
	lock := s.GroupApprovalLock(g.ID)
	lock.Lock()
	defer lock.Unlock()
	changes, err := ReadChanges(wt, g.Members[0])
	if err != nil {
		return WorkgroupChange{}, &APIError{Code: Internal, Message: err.Error()}
	}
	for _, c := range changes {
		if c.ID == id {
			if c.Decision != nil && reflect.DeepEqual(*c.Decision, d) {
				return c, nil
			}
			if c.State != "presented" || c.Revision != d.Revision {
				return c, &APIError{Code: Conflict, Message: "approve the current presented impact summary"}
			}
			if d.Decision != "approve" && d.Decision != "reject" {
				return c, &APIError{Code: Validation, Message: "decision must be approve or reject"}
			}
			if c.Request.Kind == "cancel" && d.Decision == "approve" && d.PullRequests != "close" && d.PullRequests != "draft" {
				return c, &APIError{Code: Validation, Message: "choose close or draft for open pull requests at cancel time"}
			}
			// Approval compares authoritative text with the exact reviewed
			// snapshot; it never replaces pinned member state with a new read.
			if c.Request.Kind == "amend" {
				current, found, err := ChangeProjectSpec(g, wt)
				if err == nil && !found {
					current, _, err = s.SnapshotChange(g, wt)
				}
				if err != nil {
					return c, &APIError{Code: Internal, Message: err.Error()}
				}
				if !SameProjectSpec(current, c.Before) {
					return c, &APIError{Code: Conflict, Message: "approved project spec changed; request a new delta"}
				}
			}
			c.Decision = &d
			c.State = "approved"
			if d.Decision == "reject" {
				c.State = "rejected"
			}
			if err := s.SaveChange(ctx, wt, &c); err != nil {
				return c, &APIError{Code: Internal, Message: err.Error()}
			}
			return c, nil
		}
	}
	return WorkgroupChange{}, &APIError{Code: NotFound, Message: "change not found"}
}
func (c *Controls) ChangeControl(repository *trace.Repository, scope coreadapter.Scope, decide bool) coreadapter.Tool {
	name := RequestChangeTool
	schema := `{"type":"object","properties":{"key":{"type":"string"},"kind":{"type":"string","enum":["amend","cancel"]},"scope":{"type":"string","enum":["workgroup","workstream"]},"workstream":{"type":"string"},"change":{"type":"string"}},"required":["key","kind","scope","change"],"additionalProperties":false}`
	if decide {
		name = DecideChangeTool
		schema = `{"type":"object","properties":{"id":{"type":"string"},"revision":{"type":"integer"},"decision":{"type":"string","enum":["approve","reject"]},"pull_requests":{"type":"string","enum":["close","draft"]}},"required":["id","revision","decision"],"additionalProperties":false}`
	}
	return coreadapter.Tool{Name: name, Effect: coreadapter.ToolMemory, Description: "Record the owner's amend or cancel request, or the owner's decision on the one impact summary. Cancel asks whether open PRs are closed or left as drafts. Never publish or merge.", InputSchema: json.RawMessage(schema), Handle: func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
		_, owner, err := repository.OwnerTurn(trace.ChiefOfStaff, scope)
		if err != nil {
			return nil, err
		}
		if !owner {
			return priorityRefusal("only an owner message can request or decide a workgroup change")
		}
		s := c.Load()
		if s == nil {
			return nil, errors.New("change controls unavailable")
		}
		var out WorkgroupChange
		var api *APIError
		address := scope.Workstream
		groups, _, err := trace.Workgroups(repository)
		if err != nil {
			return nil, err
		}
		g, ok := trace.WorkgroupOf(groups, config.WorkstreamID(scope.Workstream))
		if !ok {
			return nil, errors.New("workgroup unavailable")
		}
		if ChangeSharedRepository(g, repository) {
			if g.Conversation != nil {
				address = string(g.Conversation.Workstream)
			} else {
				member, err := s.FirstLiveWorkgroupMember(g)
				if err != nil {
					return nil, err
				}
				if member == "" {
					return priorityRefusal("the workgroup has no live member for this change")
				}
				address = string(member)
			}
		}
		if decide {
			var in struct {
				ID string `json:"id"`
				ChangeDecision
			}
			if err := DecodeToolInput(raw, &in); err != nil {
				return nil, err
			}
			out, api = s.DecideChange(ctx, address, in.ID, in.ChangeDecision)
		} else {
			var in ChangeRequest
			if err := DecodeToolInput(raw, &in); err != nil {
				return nil, err
			}
			out, api = s.RequestChange(ctx, address, in)
		}
		if api != nil {
			return priorityRefusal(api.Message)
		}
		return json.Marshal(out)
	}}
}

// ChangeHeld is a durable admission barrier. Running turns finish their current
// unit-branch work; no new operation or landing is admitted across the barrier.
func ChangeHeld(repo *trace.Repository, stream config.WorkstreamID) (bool, error) {
	st, err := repo.Workflow(stream, ChangeBoundarySubject)
	return st.Value != "" && st.Value != "released", err
}
func (s *Controller) ChangeBoundary(ctx context.Context, repo *trace.Repository, stream config.WorkstreamID, c WorkgroupChange, release bool) error {
	st, err := repo.Workflow(stream, ChangeBoundarySubject)
	if err != nil {
		return err
	}
	to := c.ID
	if release {
		if st.Value == "" {
			return nil
		}
		to = "released"
	}
	if st.Value == to {
		return nil
	}
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: fmt.Sprintf("change-%s-boundary-%t", c.ID, release), Revision: 1, Project: repo.Project(), Workstream: stream, At: s.Now(), Actor: amendmentActor, Cause: WorkgroupChangeDocument(c.ID)}
	_, err = repo.Transact(ctx, trace.Transaction{ExpectedVersion: st.Version, Transition: trace.Transition{Header: h, Subject: ChangeBoundarySubject, From: st.Value, To: to, Reason: "owner-approved change stops at a unit boundary"}})
	return err
}
func ChangeIdle(repo *trace.Repository, stream config.WorkstreamID) (bool, error) {
	threads, err := repo.Threads(stream)
	if err != nil {
		return false, err
	}
	for _, t := range threads {
		if t.Identity.Role != trace.ChiefOfStaff && t.Active != "" {
			for _, q := range t.Turns {
				if q.Request.TurnID != t.Active {
					continue
				}
				current, err := repo.CurrentTurnClaim(stream, q.Claim)
				if err != nil {
					return false, err
				}
				if current || q.Response != nil {
					return false, nil
				}
			}
		}
	}
	ops, err := repo.Operations(stream)
	if err != nil {
		return false, err
	}
	for _, op := range ops {
		if op.Result == nil && op.Operation.Boundary == coreadapter.RepositoryBoundary && ChangeEffectStarted(op) {
			return false, nil
		}
	}
	return true, nil
}

// A repository operation that reached an effect must reconcile that whole
// effect before changing intent, even after a crash or an infrastructure retry.
func ChangeEffectStarted(op trace.OperationRecord) bool {
	return slices.ContainsFunc(op.History, func(a trace.OperationAction) bool {
		return a.Kind == "effect"
	})
}
func ChangeRepositoryHeld(repo *trace.Repository, stream config.WorkstreamID, op coreadapter.Operation) (bool, error) {
	boundary, err := repo.Workflow(stream, ChangeBoundarySubject)
	if err != nil || boundary.Value == "" || boundary.Value == "released" {
		return false, err
	}
	ops, err := repo.Operations(stream)
	if err != nil {
		return true, err
	}
	for _, pending := range ops {
		if pending.Operation.ID == op.ID && ChangeEffectStarted(pending) {
			return false, nil
		}
	}
	if op.Action == SealAction {
		docs, err := trace.Read[trace.Document](repo, stream)
		if err != nil {
			return true, err
		}
		// Only a seal preparing this already-approved delta may start across
		// the barrier. Ordinary queued seal/build/landing/publication waits.
		for _, doc := range docs {
			if doc.ID == "change-approved-"+boundary.Value {
				return false, nil
			}
		}
	}
	return true, nil
}
func (s *Controller) ChangeNotice(ctx context.Context, repo *trace.Repository, c WorkgroupChange) error {
	_, err := repo.EnsureChiefOfStaff(ctx, c.Host, s.Now(), serviceActor)
	if err != nil {
		return err
	}
	th, err := repo.Thread(c.Host, trace.ChiefOfStaff)
	if err != nil {
		return err
	}
	id := "change-" + c.ID + "-impact"
	if queuedTurn(th, id) {
		return nil
	}
	body := c.Summary() + "\n\nThe owner approves this impact summary once. Later publication requires each workstream's own fresh delivery approval."
	if c.Request.Kind == "cancel" {
		body += "\n\nAt cancel time choose whether open PRs are closed or left as drafts. Merged work is untouched. Dependents shown as cancelled stop in the same operation."
	}
	profile, _, err := s.RoleExecution(s.Current(), trace.ChiefOfStaff)
	if err != nil {
		return err
	}
	_, err = repo.EnqueueTurn(ctx, trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_" + id, Revision: 1, Project: repo.Project(), Workstream: c.Host, At: s.Now(), Actor: amendmentActor, Cause: WorkgroupChangeDocument(c.ID)}, AgentID: trace.ChiefOfStaff, ThreadID: th.Identity.ThreadID, TurnID: id, Profile: profile, SystemPrompt: "The impact summary is already visible as one action in this conversation. Acknowledge it briefly; do not repeat the table or approval question.", Prompt: body})
	return err
}

// Summary presents the actual proposed intent with its mechanical impact.
// Unchanged sections stay out of the delta so there is one reviewable gate.
func (c WorkgroupChange) Summary() string {
	var b strings.Builder
	b.WriteString(c.Impact.Summary())
	b.WriteString("\nRequested change: " + c.Request.Change + "\n")
	if c.Failure != "" {
		b.WriteString("\nChange held: " + c.Failure + "\n")
	}
	if c.Request.Kind != "amend" {
		return b.String()
	}
	section := func(name, before, after string) {
		if before == after {
			return
		}
		fmt.Fprintf(&b, "\n%s\n\nApproved text:\n%s\n\nProposed text:\n%s\n", name, before, after)
	}
	section("Project behaviour", c.Before.Behavior, c.After.Behavior)
	contracts := slices.Clone(c.Before.Contracts)
	for _, next := range c.After.Contracts {
		if !slices.ContainsFunc(contracts, func(old trace.Contract) bool {
			return old.Name == next.Name
		}) {
			contracts = append(contracts, trace.Contract{Name: next.Name})
		}
	}
	for _, old := range contracts {
		text := "(removed)"
		for _, next := range c.After.Contracts {
			if next.Name == old.Name {
				text = next.Text
			}
		}
		section("Contract: "+old.Name, old.Text, text)
	}
	for _, sl := range c.After.Slices {
		old, _ := trace.SliceFor(c.Before, sl.Workstream)
		section("Slice: "+string(sl.Workstream), old.Spec, sl.Spec)
		p, err := plan.Parse([]byte(sl.Plan))
		if err != nil {
			continue
			// Validated proposals have a parsed plan.
		}
		for _, row := range c.Impact.Streams {
			if row.Workstream != sl.Workstream {
				continue
			}
			for _, impact := range row.Units {
				if impact.Action == amendment.Leave || impact.Action == amendment.Remove {
					continue
				}
				if unit, found := p.Unit(impact.Unit); found {
					fmt.Fprintf(&b, "\nAssignment %s: %s\n\n", unit.ID, unit.Task)
					for _, acceptance := range unit.Acceptance {
						fmt.Fprintf(&b, "- %s\n", acceptance)
					}
				}
			}
		}
	}
	return b.String()
}

// FixedProjectsMessage explains how the owner can replace a selection.
const FixedProjectsMessage = "the project set is fixed until ratification; cancel the whole workgroup to abandon the work and hand it in again"

// MemberRemovalGate protects the owner's project set for direct abandonment
// as well as moves relayed by the chief. Whole-workgroup cancellation uses its
// durable change journal and does not pass through individual abandonment.
func (s *Controller) MemberRemovalGate(ctx context.Context, raw string) *APIError {
	_, stream, repo, api := s.ConversationTrace(raw)
	if api != nil {
		return api
	}
	id, err := repo.CrossRepoWorkgroupOf(stream)
	if err != nil {
		return &APIError{Code: Internal, Message: err.Error()}
	}
	if id == "" {
		return nil
	}
	approval, found, err := repo.GroupApprovalOf(stream)
	if err != nil {
		return &APIError{Code: Internal, Message: err.Error()}
	}
	if found && approval.Complete {
		return nil
	}
	wt, g, _, close, err := s.ChangeContext(ctx, raw)
	if err != nil {
		return &APIError{Code: Conflict, Message: err.Error()}
	}
	defer close()
	members := s.ChangeMembers(g, wt)
	if len(members) < 2 {
		return nil
	}
	archives := s.MemberArchives()
	for _, member := range members {
		if slices.ContainsFunc(archives, func(a runtime.Archive) bool { return a.Workstream == member }) {
			continue
		}
		_, _, r, api := s.ConversationTrace(string(member))
		if api != nil {
			return api
		}
		if _, _, approved, err := seal.Latest(r, member); err != nil {
			return &APIError{Code: Internal, Message: err.Error()}
		} else if !approved {
			return &APIError{Code: Conflict, Message: FixedProjectsMessage}
		}
	}
	return nil
}
