package service

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/kb"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/trace"
)

// pendingProject is the journal of one project registration in progress. It is
// written before any other file and removed last, so a restart finds every
// interrupted registration and finishes it with the same identity.
type pendingProject struct {
	Version int            `json:"version"`
	Project config.Project `json:"project"`
}

var (
	registrationActor = trace.Actor{Kind: "owner", ID: "local"}
	serviceActor      = trace.Actor{Kind: "service", ID: "osmia"}
)

func (s *Service) step(name string) error {
	if s.boundary != nil {
		return s.boundary(name)
	}
	return nil
}

func projectView(root config.Root, p config.Project) ProjectView {
	directory := filepath.Join(root.String(), "projects", string(p.ID))
	return ProjectView{ID: p.ID, Name: p.Name, Upstream: p.Upstream, Fork: p.Fork, Clone: p.Clone, BaseBranch: p.BaseBranch, Trace: directory, Charter: filepath.Join(directory, "charter.md")}
}

// validateAdd checks a request before anything is written. Every failure names
// the field at fault; the clone is read, never written.
func (s *Service) validateAdd(req ProjectAddRequest) (config.Project, *APIError) {
	invalid := func(message string) (config.Project, *APIError) {
		return config.Project{}, &APIError{Validation, message}
	}
	name := strings.TrimSpace(req.Name)
	if name == "" || strings.ContainsAny(name, "\x00\r\n") {
		return invalid("name is required and must be one line")
	}
	if !config.ValidRepository(req.Upstream) {
		return invalid("upstream must be owner/repository without a URL or .git suffix")
	}
	if !config.ValidRepository(req.Fork) {
		return invalid("fork must be owner/repository without a URL or .git suffix")
	}
	if strings.EqualFold(req.Upstream, req.Fork) {
		return invalid("fork must differ from upstream")
	}
	branch := req.BaseBranch
	if branch == "" {
		branch = "main"
	}
	if !config.ValidBranch(branch) {
		return invalid("base_branch is not a valid Git branch name")
	}
	if !filepath.IsAbs(req.Clone) {
		return invalid("clone must be an absolute path to the local clone")
	}
	clone, err := config.ResolveClone(req.Clone, s.options.Config.Home)
	if err != nil {
		return invalid("clone must be an accessible absolute path")
	}
	root := s.current().Root
	if root.Overlaps(clone) {
		return invalid("clone and Osmia root must be separate, non-nested directories")
	}
	if info, err := os.Stat(clone); err != nil || !info.IsDir() {
		return invalid("clone does not exist or is not a directory")
	}
	if _, err := os.Stat(filepath.Join(clone, ".git")); err != nil {
		return invalid("clone is not a Git repository: no .git entry")
	}
	return config.Project{Version: 1, Name: name, Upstream: req.Upstream, Fork: req.Fork, Clone: clone, BaseBranch: branch, Landing: "commit-per-unit"}, nil
}

func sameRegistration(a, b config.Project) bool {
	return a.Name == b.Name && a.Upstream == b.Upstream && a.Fork == b.Fork && a.Clone == b.Clone && a.BaseBranch == b.BaseBranch
}

// addProject registers a project. An interrupted registration is completed
// before a new one is considered. Repeating the active project's registration
// returns it; any other request while a project is active is refused.
func (s *Service) addProject(ctx context.Context, req ProjectAddRequest) (ProjectResponse, *APIError) {
	s.projectMu.Lock()
	defer s.projectMu.Unlock()
	pending, err := s.readPending()
	if err != nil {
		return ProjectResponse{}, &APIError{Internal, "the project registration journal is unreadable; inspect project-add.json under the root"}
	}
	if pending != nil {
		if active := s.current(); active.HasProject() && active.Project.ID != pending.Project.ID {
			return ProjectResponse{}, journalMismatch(pending.Project.ID, active.Project.ID)
		}
		p, err := s.complete(ctx, *pending, true)
		if err != nil {
			return ProjectResponse{}, incomplete(pending.Project.ID, err)
		}
		if requested, api := s.validateAdd(req); api != nil || !sameRegistration(requested, p) {
			return ProjectResponse{}, activeError(p.ID)
		}
		return s.added(p), nil
	}
	cfg := s.current()
	p, api := s.validateAdd(req)
	if cfg.HasProject() {
		// The same registration again is the retry of a finished add.
		if api == nil && sameRegistration(p, cfg.Project) {
			return s.added(cfg.Project), nil
		}
		return ProjectResponse{}, activeError(cfg.Project.ID)
	}
	if api != nil {
		return ProjectResponse{}, api
	}
	if err := s.reserve(&p); err != nil {
		return ProjectResponse{}, &APIError{Internal, "cannot reserve a project identity under the root; check its permissions"}
	}
	if err := s.step("journal-written"); err != nil {
		return ProjectResponse{}, incomplete(p.ID, err)
	}
	p, err = s.complete(ctx, pendingProject{Version: 1, Project: p}, true)
	if err != nil {
		return ProjectResponse{}, incomplete(p.ID, err)
	}
	return s.added(p), nil
}

func journalMismatch(pending, active config.ProjectID) *APIError {
	return &APIError{Internal, fmt.Sprintf("the registration journal names project %s while %s is active; remove %s or inspect project-add.json under the root", pending, active, active)}
}
func activeError(id config.ProjectID) *APIError {
	return &APIError{ProjectActive, fmt.Sprintf("project %s is already active; single-project operation requires removing it before adding another", id)}
}
func incomplete(id config.ProjectID, err error) *APIError {
	reason := "storage failure"
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		reason = "interrupted"
	case errors.Is(err, trace.ErrLocked):
		reason = "trace locked"
	}
	return &APIError{Internal, fmt.Sprintf("registration of project %s is incomplete (%s); fix the root and run osmia project add again to finish it", id, reason)}
}
func (s *Service) added(p config.Project) ProjectResponse {
	view := projectView(s.current().Root, p)
	return ProjectResponse{Project: view, NextStep: "Write the charter in " + view.Charter + " before handing in work."}
}

// reserve generates an identity with no directory under projects and journals it.
func (s *Service) reserve(p *config.Project) error {
	root := s.current().Root
	for attempt := 0; attempt < 8; attempt++ {
		id, err := config.NewProjectID()
		if err != nil {
			return err
		}
		directory, err := root.ProjectTrace(id)
		if err != nil {
			return err
		}
		if _, err := os.Lstat(directory); !os.IsNotExist(err) {
			continue
		}
		p.ID = id
		return s.writePending(pendingProject{Version: 1, Project: *p})
	}
	return errors.New("no free project identity")
}
func (s *Service) writePending(p pendingProject) error {
	path, err := s.current().Root.PendingProject()
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(filepath.Dir(path), ".project-add-"+rand.Text()+".tmp")
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	if _, err := f.Write(append(data, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := errors.Join(f.Sync(), f.Close()); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
func (s *Service) readPending() (*pendingProject, error) {
	path, err := s.current().Root.PendingProject()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var p pendingProject
	d := json.NewDecoder(strings.NewReader(string(data)))
	d.DisallowUnknownFields()
	if err := d.Decode(&p); err != nil {
		return nil, err
	}
	if p.Version != 1 || config.CheckProjectIDs(p.Project.ID) != nil || !filepath.IsAbs(p.Project.Clone) {
		return nil, errors.New("invalid project registration journal")
	}
	return &p, nil
}

// tracePublished reports whether the trace at directory has committed history:
// creation publishes its first commit last, so the reference proves completion.
func tracePublished(directory string) bool {
	info, err := os.Lstat(filepath.Join(directory, ".git", "refs", "heads", "main"))
	return err == nil && info.Mode().IsRegular()
}

// discardUnpublished removes an interrupted trace initialization that never
// committed, keeping the configuration file. Nothing with history is touched.
func discardUnpublished(directory string) error {
	if tracePublished(directory) {
		return fmt.Errorf("%s: trace has history", directory)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Name() == "config.toml" {
			continue
		}
		if err := os.RemoveAll(filepath.Join(directory, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// complete runs every registration step that is not yet done, in order, and
// activates the project unless startup will. The journal goes last, once the
// project is listed and running. Each step inspects the disk before acting, so
// running it again after an interruption neither repeats an effect nor skips
// one. The trace's first extraction is requested before the project activates,
// so the reconciliation loop finds it on its first pass.
func (s *Service) complete(ctx context.Context, pending pendingProject, activate bool) (config.Project, error) {
	p := pending.Project
	root := s.current().Root
	directory, err := root.ProjectTrace(p.ID)
	if err != nil {
		return p, err
	}
	projectPath, err := root.ProjectConfig(p.ID)
	if err != nil {
		return p, err
	}
	journal, err := root.PendingProject()
	if err != nil {
		return p, err
	}
	if !tracePublished(directory) {
		if err := config.WriteProjectConfig(projectPath, p); err != nil {
			return p, err
		}
		if err := s.step("project-config-written"); err != nil {
			return p, err
		}
		if _, err := os.Lstat(directory); err == nil {
			if err := discardUnpublished(directory); err != nil {
				return p, err
			}
		}
		seed, err := seedTracked(ctx, p.Clone)
		if err != nil {
			return p, err
		}
		entities, err := kb.Encode(seed)
		if err != nil {
			return p, err
		}
		repository, err := trace.CreateSeeded(ctx, root, p, time.Now().UTC(), registrationActor, entities)
		if err != nil {
			return p, err
		}
		if err := repository.Close(); err != nil {
			return p, err
		}
	}
	if err := s.step("trace-created"); err != nil {
		return p, err
	}
	configPath, err := root.Config()
	if err != nil {
		return p, err
	}
	if err := config.AddActiveProject(configPath, p.ID); err != nil {
		return p, err
	}
	if err := s.step("active-project-listed"); err != nil {
		return p, err
	}
	if err := s.ensureExtraction(ctx, root, p); err != nil {
		return p, err
	}
	if activate {
		if p, err = s.activate(p.ID); err != nil {
			return p, err
		}
	}
	if err := os.Remove(journal); err != nil && !os.IsNotExist(err) {
		return p, err
	}
	return p, s.step("journal-removed")
}

// resolveRuntime points the runtime store at cfg and the workstreams of
// repository, re-reading the trace so overrides may name workstreams created
// since the last resolve. Without a repository the configured identities
// stand in. Callers hold their own ordering against configuration changes.
func (s *Service) resolveRuntime(cfg *config.Config, repository *trace.Repository) error {
	workstreams := s.options.Workstreams
	if repository != nil {
		var err error
		if workstreams, err = repository.Workstreams(); err != nil {
			return err
		}
	}
	return s.store.Resolve(runtime.Inputs{Config: cfg, Workstreams: workstreams})
}

// activate loads the project into the running configuration and opens its
// trace and reconciliation loop the way startup does.
func (s *Service) activate(id config.ProjectID) (config.Project, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active != nil {
		if s.cfg.Project.ID == id {
			return s.cfg.Project, nil
		}
		return config.Project{}, fmt.Errorf("project %s is active", s.cfg.Project.ID)
	}
	cfg, err := s.cfg.WithProject(id, s.options.Config.Home)
	if err != nil {
		return config.Project{}, err
	}
	active, err := s.open(cfg)
	if err != nil {
		return config.Project{}, err
	}
	var repository *trace.Repository
	if active != nil {
		repository = active.repository
	}
	if err := s.resolveRuntime(cfg, repository); err != nil {
		if active != nil {
			active.repository.Close()
		}
		return config.Project{}, err
	}
	s.cfg, s.active, s.pending = cfg, active, nil
	s.launch(active)
	return cfg.Project, nil
}

// recoverPending finishes an interrupted registration at startup, leaving the
// trace to open through the ordinary startup path. Failure keeps the journal
// for a later retry and starts the service with the configuration as loaded,
// which has no project unless one is already listed, reporting the problem
// through configuration diagnostics.
func (s *Service) recoverPending(ctx context.Context, cfg *config.Config) (*config.Config, error) {
	s.cfg = cfg
	pending, err := s.readPending()
	if err != nil {
		return cfg, err
	}
	if pending == nil {
		return cfg, nil
	}
	if cfg.HasProject() && cfg.Project.ID != pending.Project.ID {
		return cfg, fmt.Errorf("journal names %s while %s is active", pending.Project.ID, cfg.Project.ID)
	}
	if _, err := s.complete(ctx, *pending, false); err != nil {
		return cfg, err
	}
	loaded, err := config.Load(s.options.Config)
	if err != nil {
		return cfg, err
	}
	return loaded, nil
}

// removeProject takes the active project out of configuration and closes its
// runtime state. Its trace and the owner's clone stay where they are.
func (s *Service) removeProject(req ProjectRemoveRequest) (ProjectResponse, *APIError) {
	s.projectMu.Lock()
	defer s.projectMu.Unlock()
	cfg := s.current()
	if !cfg.HasProject() {
		return ProjectResponse{}, &APIError{NoProject, "no project is configured; add one with osmia project add"}
	}
	if err := config.CheckProjectIDs(req.Project); err != nil {
		return ProjectResponse{}, &APIError{Validation, "project must be a project ID: p_ followed by 32 lowercase hexadecimal digits"}
	}
	if req.Project != cfg.Project.ID {
		return ProjectResponse{}, &APIError{Validation, fmt.Sprintf("project %s is not active; the active project is %s", req.Project, cfg.Project.ID)}
	}
	configPath, err := cfg.Root.Config()
	if err == nil {
		err = config.RemoveActiveProject(configPath, req.Project)
	}
	if err != nil {
		return ProjectResponse{}, &APIError{Internal, "cannot edit active_projects in config.toml; check the file and its permissions"}
	}
	s.mu.Lock()
	active := s.active
	s.active = nil
	s.cfg = cfg.WithoutProject()
	s.mu.Unlock()
	if err := errors.Join(s.stop(active), s.store.Resolve(runtime.Inputs{Config: s.current()})); err != nil {
		return ProjectResponse{}, &APIError{Internal, "the project is removed from configuration but its runtime state did not close cleanly; restart the service"}
	}
	view := projectView(cfg.Root, cfg.Project)
	return ProjectResponse{Project: view, NextStep: "The trace at " + view.Trace + " and the clone are retained; adding the project again starts a new trace under a new ID."}, nil
}
