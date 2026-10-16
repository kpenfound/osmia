// Package runtime persists runtime overrides independently of configuration.
package runtime

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kpenfound/osmia/internal/config"
)

const Version = 1

type Target struct {
	Scope      string              `json:"scope"`
	Project    config.ProjectID    `json:"project,omitempty"`
	Workstream config.WorkstreamID `json:"workstream,omitempty"`
	Role       string              `json:"role,omitempty"`
}
type Pause struct {
	Target Target    `json:"target"`
	Mode   string    `json:"mode"`
	Reason string    `json:"reason"`
	Source string    `json:"source"`
	SetAt  time.Time `json:"set_at"`
}

const (
	PauseOwner              = "owner"
	PauseDailyBudget        = "daily-budget"
	PauseProviderUsageLimit = "provider-usage-limit"
	PauseLoopGuard          = "loop-guard"
)

type Priority struct {
	Project     config.ProjectID      `json:"project"`
	Workstreams []config.WorkstreamID `json:"workstreams"`
}

// ArchivePublication preserves the current pull request lookup identity after
// owner archive. It grants no merge; only an observed merge releases an edge.
type ArchivePublication struct {
	Upstream    string `json:"upstream"`
	Fork        string `json:"fork"`
	Branch      string `json:"branch"`
	Commit      string `json:"commit,omitempty"`
	PullRequest int    `json:"pull_request"`
}

// Archive records a delivered or abandoned workstream the owner archived.
// State holds the outcome, delivered or abandoned. DeliveryLink is, where
// one exists, the pull request URL or the recorded upstream commit of an
// owner-recorded merge. The title, state and delivery link survive
// permanent trace cleanup; CleanedAt marks completion.
type Archive struct {
	// Workgroup and Resolved preserve verified identity and resolution before
	// owner archive. ResolutionEvidence pins merged, cancelled or unresolved
	// from retained history; an empty value grants no resolution.
	// CleanupStartedAt records authorized deletion intent.
	Workgroup          config.WorkgroupID `json:"workgroup,omitempty"`
	ResolutionEvidence string             `json:"resolution_evidence,omitempty"`
	// MergeCommit and MergeReference pin verified merge identity for dependents
	// after trace cleanup. Resolution alone does not supply a dependency commit.
	Publication      ArchivePublication  `json:"publication,omitzero"`
	MergeCommit      string              `json:"merge_commit,omitempty"`
	MergeReference   string              `json:"merge_reference,omitempty"`
	Resolved         bool                `json:"resolved,omitempty"`
	CleanupStartedAt time.Time           `json:"cleanup_started_at,omitempty"`
	Project          config.ProjectID    `json:"project"`
	Workstream       config.WorkstreamID `json:"workstream"`
	ArchivedAt       time.Time           `json:"archived_at"`
	Title            string              `json:"title,omitempty"`
	State            string              `json:"state,omitempty"`
	DeliveryLink     string              `json:"delivery_link,omitempty"`
	CleanedAt        time.Time           `json:"cleaned_at,omitempty"`
}

// ProviderLimit records a provider's blocked capacity independently of role bindings.
type ProviderLimit struct {
	Backend  string    `json:"backend"`
	Status   string    `json:"status"`
	Kind     string    `json:"kind,omitempty"`
	SetAt    time.Time `json:"set_at"`
	ResetsAt time.Time `json:"resets_at,omitempty"`
}
type State struct {
	Version        int               `json:"version"`
	Pauses         []Pause           `json:"pauses,omitempty"`
	Priorities     []Priority        `json:"priorities,omitempty"`
	Profiles       map[string]string `json:"profiles,omitempty"`
	ProviderLimits []ProviderLimit   `json:"provider_limits,omitempty"`
	Archived       []Archive         `json:"archived,omitempty"`
	// BudgetPausedOn is the local calendar day, as YYYY-MM-DD, on which the
	// daily budget last paused the factory.
	BudgetPausedOn string `json:"budget_paused_on,omitempty"`
}

// DayLayout is the layout of BudgetPausedOn.
const DayLayout = "2006-01-02"

type Diagnostic struct {
	Field  string `json:"field"`
	Reason string `json:"reason"`
}

var ErrValidation = errors.New("invalid runtime override")
var ErrConflict = errors.New("runtime changed outside this store")

// Inputs contains a validated config and the persisted workstream keys of its
// active projects, by project. Workstreams must come from the record
// repositories, never display names or a directory scan. The store copies
// inputs so callers can safely replace them. A reference to a project that is
// not active is stale and no project-scoped override can be stored for it.
type Inputs struct {
	Config      *config.Config
	Workstreams map[config.ProjectID][]config.WorkstreamID
}
type Store struct {
	mu    sync.RWMutex
	root  *os.Root
	input Inputs
	state State
	disk  []byte
	ops   fileOps
	// changed is called after a mutation changes the persisted state.
	changed func()
	// generation counts the mutations that have actually changed the
	// persisted state, so a caller that reads the store more than once
	// while assembling one response can tell whether a mutation landed
	// between those reads.
	generation atomic.Uint64
}

// Generation reports how many mutations have changed this store's persisted
// state. It never decreases while the store is open, and it is unchanged
// exactly when no mutation landed between two calls: a caller that combines
// a runtime read with a trace read into one response can call it, alongside
// the trace's own Generation, before and after assembling that response and
// retry when either differs, so the response never mixes runtime state from
// before a mutation with state from after it. It never blocks on mu, so it
// is safe to call around a read held up by a long-running mutation.
func (s *Store) Generation() uint64 { return s.generation.Load() }

// Observe calls f after each mutation that changes the persisted state,
// replacing any earlier observer. Set it before the store is shared. f runs
// while the store's lock is held, so it must return promptly and must not call
// the store.
func (s *Store) Observe(f func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.changed = f
}

// Open rejects malformed state. Well-formed stale references remain in Snapshot
// with diagnostics and are omitted from Effective. No file is created on load.
func Open(in Inputs) (*Store, []Diagnostic, error) {
	in, err := copyInputs(in)
	if err != nil {
		return nil, nil, err
	}
	if _, err := in.Config.Root.Runtime(); err != nil {
		return nil, nil, err
	}
	root, err := os.OpenRoot(in.Config.Root.String())
	if err != nil {
		return nil, nil, err
	}
	s := &Store{root: root, input: in, state: State{Version: Version}, ops: defaultFileOps()}
	data, err := readRuntime(root)
	if err != nil && !os.IsNotExist(err) {
		root.Close()
		return nil, nil, err
	}
	if err == nil {
		s.state = State{}
		if err := uniqueKeys(json.NewDecoder(bytes.NewReader(data))); err != nil {
			root.Close()
			return nil, nil, fmt.Errorf("runtime.json: %w", err)
		}
		d := json.NewDecoder(bytes.NewReader(data))
		d.DisallowUnknownFields()
		if err = d.Decode(&s.state); err == nil {
			var extra any
			if e := d.Decode(&extra); e != io.EOF {
				err = fmt.Errorf("runtime.json: trailing JSON")
			}
		}
		migrated := false
		if err == nil {
			for i := range s.state.Pauses {
				p := &s.state.Pauses[i]
				if p.Source != "operator" {
					continue
				}
				migrated = true
				p.Source = PauseOwner
				if strings.TrimSpace(p.Reason) == "" {
					p.Reason = "Owner requested pause"
				}
				if p.SetAt.IsZero() {
					info, statErr := root.Stat("runtime.json")
					if statErr != nil {
						err = statErr
						break
					}
					p.SetAt = info.ModTime().UTC()
				}
			}
		}
		if err == nil {
			err = validate(s.state)
		}
		if err != nil {
			root.Close()
			return nil, nil, fmt.Errorf("runtime.json: %w", err)
		}
		s.disk = bytes.Clone(data)
		if migrated {
			updated, encodeErr := s.ops.encode(s.state)
			if encodeErr != nil {
				root.Close()
				return nil, nil, encodeErr
			}
			updated = append(updated, '\n')
			if err := s.persist(updated); err != nil {
				root.Close()
				return nil, nil, err
			}
			s.disk = updated
		}
	}
	_, diagnostics := resolve(s.state, s.input)
	return s, diagnostics, nil
}
func (s *Store) Close() error { s.mu.Lock(); defer s.mu.Unlock(); return s.root.Close() }

func copyInputs(in Inputs) (Inputs, error) {
	if in.Config == nil {
		return Inputs{}, fmt.Errorf("configuration required")
	}
	c := *in.Config
	if len(c.ActiveProjects) != len(c.Projects) {
		return Inputs{}, fmt.Errorf("every active identity requires a loaded project")
	}
	for i, p := range c.Projects {
		if err := config.CheckProjectIDs(p.ID); err != nil {
			return Inputs{}, err
		}
		if c.ActiveProjects[i] != string(p.ID) {
			return Inputs{}, fmt.Errorf("loaded projects must match the active identities")
		}
	}
	workstreams := map[config.ProjectID][]config.WorkstreamID{}
	for project, streams := range in.Workstreams {
		if !c.Active(project) {
			return Inputs{}, fmt.Errorf("workstreams require a loaded project")
		}
		if err := config.CheckWorkstreamIDs(streams...); err != nil {
			return Inputs{}, err
		}
		workstreams[project] = slices.Clone(streams)
	}
	c.Profiles = maps.Clone(c.Profiles)
	c.Roles = maps.Clone(c.Roles)
	c.ActiveProjects = slices.Clone(c.ActiveProjects)
	c.Projects = slices.Clone(c.Projects)
	return Inputs{&c, workstreams}, nil
}

// Resolve replaces resolver input only; it never rewrites or drops overrides.
// Root changes require reopening the store. Callers supply validated config.
func (s *Store) Resolve(in Inputs) error {
	in, err := copyInputs(in)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if in.Config.Root.String() != s.input.Config.Root.String() {
		return fmt.Errorf("root change requires reopening runtime store")
	}
	s.input = in
	return nil
}
func clone(st State) State {
	st.Pauses = slices.Clone(st.Pauses)
	st.ProviderLimits = slices.Clone(st.ProviderLimits)
	st.Archived = slices.Clone(st.Archived)
	st.Profiles = maps.Clone(st.Profiles)
	st.Priorities = slices.Clone(st.Priorities)
	for i := range st.Priorities {
		st.Priorities[i].Workstreams = slices.Clone(st.Priorities[i].Workstreams)
	}
	return st
}
func (s *Store) Snapshot() (State, []Diagnostic) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, d := resolve(s.state, s.input)
	return clone(s.state), d
}

// Effective includes configured role bindings, valid runtime pauses and the
// active projects' explicit orderings. An absent pause means unpaused; an absent
// priority means no ordering preference. The store performs no scheduling.
func (s *Store) Effective() (State, []Diagnostic) {
	return s.EffectiveAt(time.Now().UTC())
}
func (s *Store) EffectiveAt(at time.Time) (State, []Diagnostic) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return resolveAt(s.state, s.input, at)
}
func resolve(st State, in Inputs) (State, []Diagnostic) {
	return resolveAt(st, in, time.Now().UTC())
}
func resolveAt(st State, in Inputs, at time.Time) (State, []Diagnostic) {
	out := State{Version: Version, Profiles: map[string]string{}}
	var ds []Diagnostic
	for r, b := range in.Config.Roles {
		out.Profiles[r] = b.Profile
	}
	for i, p := range st.Pauses {
		if err := targetReference(p.Target, in); err != nil {
			ds = append(ds, Diagnostic{fmt.Sprintf("pauses[%d]", i), err.Error()})
		} else {
			out.Pauses = append(out.Pauses, p)
		}
	}
	for i, p := range st.Priorities {
		if !in.Config.Active(p.Project) {
			ds = append(ds, Diagnostic{fmt.Sprintf("priorities[%d]", i), "inactive project " + string(p.Project)})
			continue
		}
		valid := Priority{Project: p.Project, Workstreams: []config.WorkstreamID{}}
		for j, w := range p.Workstreams {
			if !slices.Contains(in.Workstreams[p.Project], w) {
				ds = append(ds, Diagnostic{fmt.Sprintf("priorities[%d].workstreams[%d]", i, j), "unknown workstream " + string(w)})
			} else {
				valid.Workstreams = append(valid.Workstreams, w)
			}
		}
		out.Priorities = append(out.Priorities, valid)
	}
	for i, a := range st.Archived {
		if a.Title != "" && in.Config.Active(a.Project) {
			out.Archived = append(out.Archived, a)
			continue
		}
		if err := targetReference(Target{Scope: "workstream", Project: a.Project, Workstream: a.Workstream}, in); err != nil {
			ds = append(ds, Diagnostic{fmt.Sprintf("archived[%d]", i), err.Error()})
		} else {
			out.Archived = append(out.Archived, a)
		}
	}
	for _, r := range slices.Sorted(maps.Keys(st.Profiles)) {
		if err := profileReference(r, st.Profiles[r], in); err != nil {
			ds = append(ds, Diagnostic{"profiles." + r, err.Error()})
		} else {
			out.Profiles[r] = st.Profiles[r]
		}
	}
	for _, limit := range st.ProviderLimits {
		if !limit.ResetsAt.IsZero() && !at.Before(limit.ResetsAt) {
			continue
		}
		out.ProviderLimits = append(out.ProviderLimits, limit)
	}
	for role, binding := range in.Config.Roles {
		if _, overridden := st.Profiles[role]; overridden && out.Profiles[role] == st.Profiles[role] {
			continue
		}
		name := binding.Profile
		if !limitedBackend(out.ProviderLimits, in.Config.Profiles[name].Agent) {
			continue
		}
		for name != "" && limitedBackend(out.ProviderLimits, in.Config.Profiles[name].Agent) {
			name = in.Config.Profiles[name].Fallback
		}
		if name != "" {
			out.Profiles[role] = name
			continue
		}
		limit := out.ProviderLimits[0]
		for _, candidate := range out.ProviderLimits {
			if candidate.Backend == in.Config.Profiles[binding.Profile].Agent {
				limit = candidate
				break
			}
		}
		out.Pauses = append(out.Pauses, Pause{Target: Target{Scope: "role", Role: role}, Mode: "soft", Source: PauseProviderUsageLimit, Reason: "Provider " + limit.Backend + " usage limit (" + limit.Status + ")", SetAt: limit.SetAt})
	}
	return out, ds
}
func limitedBackend(limits []ProviderLimit, backend string) bool {
	for _, limit := range limits {
		if limit.Backend == backend {
			return true
		}
	}
	return false
}
func targetReference(t Target, in Inputs) error {
	if t.Scope == "factory" {
		return nil
	}
	if t.Scope == "role" {
		if _, ok := in.Config.Roles[t.Role]; !ok {
			return fmt.Errorf("unknown role %s", t.Role)
		}
		return nil
	}
	if !in.Config.Active(t.Project) {
		return fmt.Errorf("inactive project %s", t.Project)
	}
	if t.Scope == "workstream" && !slices.Contains(in.Workstreams[t.Project], t.Workstream) {
		return fmt.Errorf("unknown workstream %s", t.Workstream)
	}
	return nil
}
func profileReference(role, profile string, in Inputs) error {
	r, ok := in.Config.Roles[role]
	if !ok {
		return fmt.Errorf("unknown role %s", role)
	}
	seen := map[string]bool{}
	for p := profile; p != ""; p = in.Config.Profiles[p].Fallback {
		v, ok := in.Config.Profiles[p]
		if !ok {
			return fmt.Errorf("unknown profile %s", p)
		}
		if seen[p] {
			return fmt.Errorf("fallback cycle at %s", p)
		}
		seen[p] = true
		if r.Sandbox == "claude" && v.Agent != "claude" {
			return fmt.Errorf("profile %s incompatible with claude sandbox", p)
		}
	}
	return nil
}
func validateTarget(t Target) error {
	switch t.Scope {
	case "factory":
		if t.Project != "" || t.Workstream != "" || t.Role != "" {
			return fmt.Errorf("factory target has identity")
		}
	case "role":
		if strings.TrimSpace(t.Role) == "" || t.Project != "" || t.Workstream != "" {
			return fmt.Errorf("invalid role target")
		}
	case "project", "workstream":
		if t.Role != "" {
			return fmt.Errorf("project or workstream target has role")
		}
		if err := config.CheckProjectIDs(t.Project); err != nil {
			return err
		}
		if t.Scope == "project" && t.Workstream != "" {
			return fmt.Errorf("project target has workstream")
		}
		if t.Scope == "workstream" {
			return config.CheckWorkstreamIDs(t.Workstream)
		}
	default:
		return fmt.Errorf("unknown pause scope %q", t.Scope)
	}
	return nil
}
func validate(st State) error {
	if st.Version != Version {
		return fmt.Errorf("unsupported version %d", st.Version)
	}
	seen := map[Target]bool{}
	for _, p := range st.Pauses {
		if err := validateTarget(p.Target); err != nil {
			return err
		}
		if seen[p.Target] {
			return fmt.Errorf("duplicate pause target")
		}
		seen[p.Target] = true
		if p.Mode != "soft" && p.Mode != "hard" {
			return fmt.Errorf("invalid pause mode %q", p.Mode)
		}
		if p.Source != PauseOwner && p.Source != PauseDailyBudget && p.Source != PauseProviderUsageLimit && p.Source != PauseLoopGuard {
			return fmt.Errorf("invalid pause source %q", p.Source)
		}
		if strings.TrimSpace(p.Reason) == "" {
			return fmt.Errorf("pause reason must be non-empty")
		}
		if p.SetAt.IsZero() {
			return fmt.Errorf("pause set time must be non-zero")
		}
	}
	projects := map[config.ProjectID]bool{}
	for _, p := range st.Priorities {
		if err := config.CheckProjectIDs(p.Project); err != nil {
			return err
		}
		if projects[p.Project] {
			return fmt.Errorf("duplicate priority project")
		}
		projects[p.Project] = true
		if p.Workstreams == nil {
			return fmt.Errorf("priority workstreams must be an array")
		}
		if err := config.CheckWorkstreamIDs(p.Workstreams...); err != nil {
			return err
		}
	}
	archived := map[config.WorkstreamID]bool{}
	for _, a := range st.Archived {
		if err := config.CheckProjectIDs(a.Project); err != nil {
			return err
		}
		if err := config.CheckWorkstreamIDs(a.Workstream); err != nil {
			return err
		}
		if archived[a.Workstream] {
			return fmt.Errorf("duplicate archived workstream")
		}
		if a.Workgroup != "" {
			if err := config.CheckWorkgroupIDs(a.Workgroup); err != nil {
				return err
			}
		}
		switch a.ResolutionEvidence {
		case "":
		case "unresolved":
			if a.Resolved {
				return fmt.Errorf("unresolved archive cannot grant resolution")
			}
		case "merged":
			if !a.Resolved || a.State != "delivered" {
				return fmt.Errorf("invalid archived merge evidence")
			}
		case "cancelled":
			if !a.Resolved || a.State != "abandoned" {
				return fmt.Errorf("invalid archived cancel evidence")
			}
		default:
			return fmt.Errorf("invalid archive resolution evidence")
		}
		if a.Publication != (ArchivePublication{}) && (a.Publication.PullRequest <= 0 || a.Publication.Upstream == "" || a.Publication.Fork == "" || a.Publication.Branch == "") {
			return fmt.Errorf("invalid archived publication identity")
		}
		if a.MergeCommit != "" || a.MergeReference != "" {
			if a.MergeCommit == "" || a.MergeReference == "" || !a.Resolved || a.ResolutionEvidence != "merged" {
				return fmt.Errorf("invalid archived merge identity")
			}
			p := a.Publication
			prReference := fmt.Sprintf("%s#%d", p.Upstream, p.PullRequest)
			ownerSuffix := "@" + a.MergeCommit
			ownerReference := strings.HasSuffix(a.MergeReference, ownerSuffix) && len(a.MergeReference) > len(ownerSuffix)
			if !ownerReference && (p == (ArchivePublication{}) || a.MergeReference != prReference) {
				return fmt.Errorf("archive merge reference does not match its publication")
			}
		}
		if a.Resolved && a.State != "delivered" && a.State != "abandoned" {
			return fmt.Errorf("resolved archive must be terminal")
		}
		if !a.CleanupStartedAt.IsZero() && a.Title == "" {
			return fmt.Errorf("cleanup intent requires preserved metadata")
		}
		archived[a.Workstream] = true
		if a.ArchivedAt.IsZero() {
			return fmt.Errorf("archive time must be non-zero")
		}
	}
	for r, p := range st.Profiles {
		if strings.TrimSpace(r) == "" || strings.TrimSpace(p) == "" {
			return fmt.Errorf("empty role or profile")
		}
	}
	seenLimits := map[string]bool{}
	for _, limit := range st.ProviderLimits {
		if strings.TrimSpace(limit.Backend) == "" || strings.TrimSpace(limit.Status) == "" || limit.SetAt.IsZero() || seenLimits[limit.Backend] {
			return fmt.Errorf("invalid provider limit")
		}
		seenLimits[limit.Backend] = true
	}
	if st.BudgetPausedOn != "" {
		if _, err := time.Parse(DayLayout, st.BudgetPausedOn); err != nil {
			return fmt.Errorf("budget pause day must be YYYY-MM-DD")
		}
	}
	return nil
}
func (s *Store) mutate(f func(*State, Inputs) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := clone(s.state)
	if err := f(&next, s.input); err != nil {
		return fmt.Errorf("%w: %v", ErrValidation, err)
	}
	if err := validate(next); err != nil {
		return fmt.Errorf("%w: %v", ErrValidation, err)
	}
	slices.SortFunc(next.Pauses, func(a, b Pause) int {
		return strings.Compare(a.Target.Scope+string(a.Target.Project)+string(a.Target.Workstream), b.Target.Scope+string(b.Target.Project)+string(b.Target.Workstream))
	})
	slices.SortFunc(next.Priorities, func(a, b Priority) int { return strings.Compare(string(a.Project), string(b.Project)) })
	slices.SortFunc(next.Archived, func(a, b Archive) int {
		return strings.Compare(string(a.Project)+string(a.Workstream), string(b.Project)+string(b.Workstream))
	})
	data, err := s.ops.encode(next)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err = s.persist(data); err != nil {
		return err
	}
	unchanged := bytes.Equal(data, s.disk)
	s.state = next
	s.disk = data
	if !unchanged {
		s.generation.Add(1)
		if s.changed != nil {
			s.changed()
		}
	}
	return nil
}
func (s *Store) SetPause(p Pause) error {
	if p.SetAt.IsZero() {
		p.SetAt = time.Now().UTC()
	}
	return s.mutate(func(st *State, in Inputs) error { return setPause(st, in, p) })
}

// SetProviderLimit records the latest blocked report for a provider.
func (s *Store) SetProviderLimit(limit ProviderLimit) error {
	return s.mutate(func(st *State, _ Inputs) error {
		st.ProviderLimits = slices.DeleteFunc(st.ProviderLimits, func(v ProviderLimit) bool { return v.Backend == limit.Backend })
		st.ProviderLimits = append(st.ProviderLimits, limit)
		return nil
	})
}

// ClearProviderLimit removes one provider's limit, including a limit without a reset time.
func (s *Store) ClearProviderLimit(backend string) error {
	return s.mutate(func(st *State, _ Inputs) error {
		if strings.TrimSpace(backend) == "" {
			return fmt.Errorf("provider required")
		}
		st.ProviderLimits = slices.DeleteFunc(st.ProviderLimits, func(v ProviderLimit) bool { return v.Backend == backend })
		return nil
	})
}

// SetBudgetPause records the daily budget's pause p and day, the local
// calendar day it is set on, in one write. It follows SetPause's rules.
func (s *Store) SetBudgetPause(p Pause, day string) error {
	return s.mutate(func(st *State, in Inputs) error {
		if p.Source != PauseDailyBudget {
			return fmt.Errorf("budget pause source must be %s", PauseDailyBudget)
		}
		if err := setPause(st, in, p); err != nil {
			return err
		}
		st.BudgetPausedOn = day
		return nil
	})
}
func setPause(st *State, in Inputs, p Pause) error {
	if err := validateTarget(p.Target); err != nil {
		return err
	}
	if err := targetReference(p.Target, in); err != nil {
		return err
	}
	for _, current := range st.Pauses {
		if current.Target == p.Target && p.Source != PauseOwner && current.Source != p.Source {
			return fmt.Errorf("%s cannot replace %s pause; only the owner may replace it", p.Source, current.Source)
		}
	}
	st.Pauses = slices.DeleteFunc(st.Pauses, func(v Pause) bool { return v.Target == p.Target })
	st.Pauses = append(st.Pauses, p)
	return nil
}

// Clear operations also accept stale keys, allowing explicit removal.
func (s *Store) ClearPause(t Target, actor string) error {
	return s.mutate(func(st *State, _ Inputs) error {
		if err := validateTarget(t); err != nil {
			return err
		}
		if actor != PauseOwner && actor != PauseDailyBudget && actor != PauseProviderUsageLimit && actor != PauseLoopGuard {
			return fmt.Errorf("invalid pause clearing source %q", actor)
		}
		for _, p := range st.Pauses {
			if p.Target == t && actor != PauseOwner && actor != p.Source {
				return fmt.Errorf("%s cannot clear %s pause; only the owner or %s may clear it", actor, p.Source, p.Source)
			}
		}
		st.Pauses = slices.DeleteFunc(st.Pauses, func(p Pause) bool { return p.Target == t })
		return nil
	})
}
func (s *Store) SetPriority(p Priority) error {
	return s.mutate(func(st *State, in Inputs) error {
		if !in.Config.Active(p.Project) {
			return fmt.Errorf("inactive project %s", p.Project)
		}
		for _, w := range p.Workstreams {
			if !slices.Contains(in.Workstreams[p.Project], w) {
				return fmt.Errorf("unknown workstream %s", w)
			}
		}
		st.Priorities = slices.DeleteFunc(st.Priorities, func(v Priority) bool { return v.Project == p.Project })
		p.Workstreams = slices.Clone(p.Workstreams)
		st.Priorities = append(st.Priorities, p)
		return nil
	})
}
func (s *Store) ClearPriority(p config.ProjectID) error {
	return s.mutate(func(st *State, _ Inputs) error {
		if err := config.CheckProjectIDs(p); err != nil {
			return err
		}
		st.Priorities = slices.DeleteFunc(st.Priorities, func(v Priority) bool { return v.Project == p })
		return nil
	})
}

// SetArchived records a workstream of an active project as archived. The
// store does not know feature states: the caller checks that the workstream
// is delivered or abandoned. Archiving an archived workstream keeps the time
// it was first archived.
func (s *Store) SetArchived(a Archive) error {
	if a.ArchivedAt.IsZero() {
		a.ArchivedAt = time.Now().UTC()
	}
	return s.mutate(func(st *State, in Inputs) error {
		if err := targetReference(Target{Scope: "workstream", Project: a.Project, Workstream: a.Workstream}, in); err != nil {
			return err
		}
		if slices.ContainsFunc(st.Archived, func(v Archive) bool { return v.Workstream == a.Workstream }) {
			return nil
		}
		st.Archived = append(st.Archived, a)
		return nil
	})
}

// ClearArchived returns a workstream to the owner's list of work. Like the
// other clear operations it accepts a stale workstream.
func (s *Store) ClearArchived(w config.WorkstreamID) error {
	return s.mutate(func(st *State, _ Inputs) error {
		if err := config.CheckWorkstreamIDs(w); err != nil {
			return err
		}
		st.Archived = slices.DeleteFunc(st.Archived, func(v Archive) bool { return v.Workstream == w })
		return nil
	})
}

func (s *Store) SetProfile(role, profile string) error {
	return s.mutate(func(st *State, in Inputs) error {
		if err := profileReference(role, profile, in); err != nil {
			return err
		}
		if st.Profiles == nil {
			st.Profiles = map[string]string{}
		}
		st.Profiles[role] = profile
		return nil
	})
}
func (s *Store) ClearProfile(role string) error {
	return s.mutate(func(st *State, _ Inputs) error { delete(st.Profiles, role); return nil })
}

// uniqueKeys rejects ambiguous objects before decoding into typed state.
func uniqueKeys(d *json.Decoder) error {
	token, err := d.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok {
				return fmt.Errorf("expected object key")
			}
			if seen[name] {
				return fmt.Errorf("duplicate JSON key %q", name)
			}
			seen[name] = true
			if err := uniqueKeys(d); err != nil {
				return err
			}
		}
	case '[':
		for d.More() {
			if err := uniqueKeys(d); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("unexpected delimiter %q", delim)
	}
	_, err = d.Token()
	return err
}

// CheckDisk detects external edits without replacing the acknowledged view.
func (s *Store) CheckDisk() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.checkDisk()
}

// UpdateArchive preserves a title, outcome or delivery link read from the
// trace before it is deleted, and records cleanup's completion once the
// trace is gone. It accepts an absent trace because a restart may follow
// the deletion.
func (s *Store) UpdateArchive(a Archive) error {
	return s.mutate(func(st *State, _ Inputs) error {
		for i, old := range st.Archived {
			if old.Project == a.Project && old.Workstream == a.Workstream {
				// Cleanup may have read history before a dependent durably observed
				// this exact publication's merge. Preserve that monotonic evidence
				// while applying cleanup's metadata and progress.
				if old.ResolutionEvidence == "merged" && old.Publication.PullRequest > 0 && (old.MergeReference == "" || old.MergeReference == fmt.Sprintf("%s#%d", old.Publication.Upstream, old.Publication.PullRequest)) &&
					a.State == old.State && a.Publication == old.Publication && a.ResolutionEvidence == "unresolved" && !a.Resolved {
					a.Resolved, a.ResolutionEvidence = old.Resolved, old.ResolutionEvidence
					a.MergeCommit, a.MergeReference = old.MergeCommit, old.MergeReference
				}
				if a.Title == "" {
					return fmt.Errorf("archive title is required")
				}
				if old.Workgroup != "" && a.Workgroup != old.Workgroup {
					return fmt.Errorf("archive workgroup identity cannot change")
				}
				if old.ResolutionEvidence != "" && old.Resolved && (!a.Resolved || a.ResolutionEvidence != old.ResolutionEvidence) {
					return fmt.Errorf("archive resolution cannot change")
				}
				if old.Publication != (ArchivePublication{}) && a.Publication != old.Publication {
					return fmt.Errorf("archive publication identity cannot change")
				}
				if old.MergeCommit != "" && (a.MergeCommit != old.MergeCommit || a.MergeReference != old.MergeReference) {
					return fmt.Errorf("archive merge identity cannot change")
				}
				if !old.CleanupStartedAt.IsZero() {
					a.CleanupStartedAt = old.CleanupStartedAt
				}
				a.ArchivedAt = old.ArchivedAt
				st.Archived[i] = a
				st.Pauses = slices.DeleteFunc(st.Pauses, func(p Pause) bool { return p.Target.Workstream == a.Workstream })
				for i := range st.Priorities {
					st.Priorities[i].Workstreams = slices.DeleteFunc(st.Priorities[i].Workstreams, func(w config.WorkstreamID) bool { return w == a.Workstream })
				}
				return nil
			}
		}
		return fmt.Errorf("archive does not exist")
	})
}

// RecordArchivePullRequestMerge reconciles an observed merge of the exact
// pinned publication without overwriting concurrent cleanup or archive metadata.
func (s *Store) RecordArchivePullRequestMerge(project config.ProjectID, stream config.WorkstreamID, publication ArchivePublication, commit string) error {
	return s.mutate(func(st *State, _ Inputs) error {
		for i := range st.Archived {
			a := &st.Archived[i]
			if a.Project != project || a.Workstream != stream {
				continue
			}
			if a.State != "delivered" || publication.PullRequest <= 0 || a.Publication != publication {
				return fmt.Errorf("archive merge does not match its delivered publication")
			}
			reference := fmt.Sprintf("%s#%d", publication.Upstream, publication.PullRequest)
			if a.ResolutionEvidence == "cancelled" || a.MergeCommit != "" && commit != "" && (a.MergeCommit != commit || a.MergeReference != reference) {
				return fmt.Errorf("archive merge identity cannot change")
			}
			a.Resolved, a.ResolutionEvidence = true, "merged"
			if commit != "" {
				a.MergeCommit, a.MergeReference = commit, reference
			}
			return nil
		}
		return fmt.Errorf("archive does not exist")
	})
}
