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
)

type Priority struct {
	Project     config.ProjectID      `json:"project"`
	Workstreams []config.WorkstreamID `json:"workstreams"`
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

// Inputs contains a validated config and the active project's persisted keys.
// Workstreams must come from the record repository, never display names or a
// directory scan. The store copies inputs so callers can safely replace them.
// Without an active project every project reference is stale and no
// project-scoped override can be stored.
type Inputs struct {
	Config      *config.Config
	Workstreams []config.WorkstreamID
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
}

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
	if c.HasProject() {
		if err := config.CheckProjectIDs(c.Project.ID); err != nil {
			return Inputs{}, err
		}
		if len(c.ActiveProjects) != 1 || c.ActiveProjects[0] != string(c.Project.ID) {
			return Inputs{}, fmt.Errorf("one matching active project required")
		}
	} else if len(c.ActiveProjects) != 0 || len(in.Workstreams) != 0 {
		return Inputs{}, fmt.Errorf("workstreams and active identities require a loaded project")
	}
	if err := config.CheckWorkstreamIDs(in.Workstreams...); err != nil {
		return Inputs{}, err
	}
	c.Profiles = maps.Clone(c.Profiles)
	c.Roles = maps.Clone(c.Roles)
	c.ActiveProjects = slices.Clone(c.ActiveProjects)
	return Inputs{&c, slices.Clone(in.Workstreams)}, nil
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
// active project's explicit ordering. An absent pause means unpaused; an absent
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
		if p.Project != in.Config.Project.ID {
			ds = append(ds, Diagnostic{fmt.Sprintf("priorities[%d]", i), "inactive project " + string(p.Project)})
			continue
		}
		valid := Priority{Project: p.Project, Workstreams: []config.WorkstreamID{}}
		for j, w := range p.Workstreams {
			if !slices.Contains(in.Workstreams, w) {
				ds = append(ds, Diagnostic{fmt.Sprintf("priorities[%d].workstreams[%d]", i, j), "unknown workstream " + string(w)})
			} else {
				valid.Workstreams = append(valid.Workstreams, w)
			}
		}
		out.Priorities = append(out.Priorities, valid)
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
	if t.Project != in.Config.Project.ID {
		return fmt.Errorf("inactive project %s", t.Project)
	}
	if t.Scope == "workstream" && !slices.Contains(in.Workstreams, t.Workstream) {
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
		if p.Source != PauseOwner && p.Source != PauseDailyBudget && p.Source != PauseProviderUsageLimit {
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
	if !unchanged && s.changed != nil {
		s.changed()
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
		if actor != PauseOwner && actor != PauseDailyBudget && actor != PauseProviderUsageLimit {
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
		if p.Project != in.Config.Project.ID {
			return fmt.Errorf("inactive project %s", p.Project)
		}
		for _, w := range p.Workstreams {
			if !slices.Contains(in.Workstreams, w) {
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
