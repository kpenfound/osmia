package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/pulls"
)

// creationGitTimeout bounds each git command a creation runs.
const creationGitTimeout = 45 * time.Second

// createProjectTimeout bounds one create request: GitHub calls, the clone and
// the registration.
const createProjectTimeout = 3 * time.Minute

// maxDescription is the longest repository description GitHub accepts.
const maxDescription = 350

var repoName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,99}$`)

// projectCreation is the part of the registration journal that carries a
// project creation. The plan is written before any GitHub write; the
// repository's identity is written as soon as GitHub answers. Every later step
// inspects the repository and the clone before acting, so Step only reports
// how far the creation got.
type projectCreation struct {
	Owner       string `json:"owner"`
	Repo        string `json:"repo"`
	Visibility  string `json:"visibility"`
	Description string `json:"description,omitempty"`
	Charter     string `json:"charter"`
	// CreateSent records that a create call is about to be, or was, sent. The
	// repository is this creation's only when RepositoryID is also recorded.
	CreateSent    bool   `json:"create_sent"`
	RepositoryID  int64  `json:"repository_id,omitempty"`
	FullName      string `json:"full_name,omitempty"`
	SSHURL        string `json:"ssh_url,omitempty"`
	DefaultBranch string `json:"default_branch,omitempty"`
	Step          string `json:"step"`
	// Stuck is why the last attempt could not go on without the owner.
	Stuck *api.ProjectCreationStuck `json:"stuck,omitempty"`
}

func (c projectCreation) repository() string { return c.Owner + "/" + c.Repo }

// creationStuck is a failure that retrying alone does not fix.
type creationStuck struct{ api.ProjectCreationStuck }

func (e *creationStuck) Error() string { return e.Reason }

func stuck(kind, format string, args ...any) *creationStuck {
	return &creationStuck{api.ProjectCreationStuck{Kind: kind, Reason: fmt.Sprintf(format, args...)}}
}

// creationError is the failure of a creation finished at startup, kept apart
// from a registration failure so diagnostics tell the owner which command
// finishes it.
type creationError struct {
	ID  config.ProjectID
	Err error
}

func (e *creationError) Error() string { return fmt.Sprintf("project creation %s: %v", e.ID, e.Err) }
func (e *creationError) Unwrap() error { return e.Err }

// repositories returns the GitHub client creation uses.
func (s *Service) repositories() pulls.Repositories {
	if s.options.Repositories != nil {
		return s.options.Repositories
	}
	return pulls.GitHub{Token: os.Getenv("GITHUB_TOKEN"), HTTP: &http.Client{Timeout: 30 * time.Second}}
}

// ReserveProjectCreation returns a creation identity: the ID a project
// created under it will have. Nothing is recorded under it, so a caller can
// record it durably before any GitHub write and pass it to CreateProject.
func (s *Service) ReserveProjectCreation() (config.ProjectID, error) {
	return s.freshIdentity()
}

// freshIdentity generates an identity that names no project directory.
func (s *Service) freshIdentity() (config.ProjectID, error) {
	root := s.current().Root
	for attempt := 0; attempt < 8; attempt++ {
		id, err := config.NewProjectID()
		if err != nil {
			return "", err
		}
		directory, err := root.ProjectTrace(id)
		if err != nil {
			return "", err
		}
		if _, err := os.Lstat(directory); os.IsNotExist(err) {
			return id, nil
		}
	}
	return "", errors.New("no free project identity")
}

// ProjectCreation reports where the creation under id stands. A journal
// naming id is pending, with the step it reached and why it is stuck. Without
// one, the registered project with that ID is complete, which stays true after
// the journal is removed.
func (s *Service) ProjectCreation(id config.ProjectID) (api.ProjectCreationState, *APIError) {
	if err := config.CheckProjectIDs(id); err != nil {
		return api.ProjectCreationState{}, &APIError{Code: Validation, Message: "id must be a project ID: p_ followed by 32 lowercase hexadecimal digits"}
	}
	s.projectMu.Lock()
	defer s.projectMu.Unlock()
	pending, err := s.readPending()
	if err != nil {
		return api.ProjectCreationState{}, &APIError{Code: Internal, Message: "the project registration journal is unreadable; inspect project-add.json under the root"}
	}
	state := api.ProjectCreationState{ID: id, State: api.CreationNotStarted}
	if pending != nil && pending.Project.ID == id {
		state.State = api.CreationPending
		state.Step = "registration"
		if c := pending.Creation; c != nil {
			state.Step, state.Repository, state.Stuck = c.Step, c.repository(), c.Stuck
		}
		return state, nil
	}
	if cfg := s.current(); cfg.Active(id) {
		view := s.viewProject(cfg.For(id).Project)
		state.State, state.Project = api.CreationComplete, &view
	}
	return state, nil
}

// CreateProject creates the project under the creation identity in req, or
// finishes the creation already begun under it. It validates before writing
// anything, saves the creation plan in the registration journal, then creates
// the repository, clones it, makes sure it has a main branch and registers it,
// removing the journal last.
func (s *Service) CreateProject(ctx context.Context, req ProjectCreateRequest) (ProjectResponse, *APIError) {
	s.projectMu.Lock()
	defer s.projectMu.Unlock()
	if req.ID != "" {
		if err := config.CheckProjectIDs(req.ID); err != nil {
			return ProjectResponse{}, &APIError{Code: Validation, Message: "id must be a project ID: p_ followed by 32 lowercase hexadecimal digits"}
		}
	}
	pending, err := s.readPending()
	if err != nil {
		return ProjectResponse{}, &APIError{Code: Internal, Message: "the project registration journal is unreadable; inspect project-add.json under the root"}
	}
	if pending != nil {
		if pending.Creation == nil || pending.Project.ID != req.ID {
			return ProjectResponse{}, pendingRegistrationError(*pending)
		}
		if refusal := samePlan(*pending, req); refusal != nil {
			return ProjectResponse{}, refusal
		}
		return s.finishCreation(ctx, pending)
	}
	cfg := s.current()
	if req.ID != "" && cfg.Active(req.ID) {
		return s.added(cfg.For(req.ID).Project), nil
	}
	plan, refusal := s.planCreation(ctx, req)
	if refusal != nil {
		return ProjectResponse{}, refusal
	}
	if err := s.writePending(*plan); err != nil {
		return ProjectResponse{}, &APIError{Code: Internal, Message: "cannot save the creation plan under the root; check its permissions"}
	}
	if err := s.step("journal-written"); err != nil {
		return ProjectResponse{}, creationIncomplete(plan.Project.ID, err)
	}
	return s.finishCreation(ctx, plan)
}

// planCreation runs the checks that write nothing and returns the plan to
// save. Every refusal names what is wrong.
func (s *Service) planCreation(ctx context.Context, req ProjectCreateRequest) (*pendingProject, *APIError) {
	invalid := func(format string, args ...any) (*pendingProject, *APIError) {
		return nil, &APIError{Code: Validation, Message: fmt.Sprintf(format, args...)}
	}
	cfg := s.current()
	creation := cfg.ProjectCreation
	if !creation.CreationEnabled() {
		var missing []string
		if creation.RepoOwner == "" {
			missing = append(missing, "projects.repo_owner")
		}
		if creation.CloneDir == "" {
			missing = append(missing, "projects.clone_dir")
		}
		return invalid("project creation is off: %s is not set; creation needs both projects.repo_owner and projects.clone_dir in config.toml", strings.Join(missing, " and "))
	}
	name := strings.TrimSpace(req.Name)
	if name == "" || strings.ContainsAny(name, "\x00\r\n") {
		return invalid("name is required and must be one line")
	}
	if !validRepoName(creation.RepoOwner, req.Repo) {
		return invalid("repo is not a valid GitHub repository name: use up to 100 letters, digits, '.', '-' and '_', starting with a letter or digit, without a .git suffix")
	}
	if len(req.Description) > maxDescription || strings.ContainsAny(req.Description, "\x00\r\n") {
		return invalid("description must be one line of at most %d characters", maxDescription)
	}
	visibility := req.Visibility
	if visibility == "" {
		visibility = api.VisibilityPrivate
	}
	if visibility != api.VisibilityPrivate && visibility != api.VisibilityPublic {
		return invalid("visibility must be private or public")
	}
	if strings.TrimSpace(req.Charter) == "" || strings.ContainsRune(req.Charter, 0) {
		return invalid("charter is required")
	}
	if os.Getenv("GITHUB_TOKEN") == "" {
		return invalid("GITHUB_TOKEN is not set in the service's environment; creating a repository needs it")
	}
	clone := filepath.Join(creation.CloneDir, req.Repo)
	if _, err := os.Lstat(clone); err == nil {
		return invalid("clone path %s already exists; choose another repo name or move it", clone)
	} else if !os.IsNotExist(err) {
		return nil, &APIError{Code: Internal, Message: fmt.Sprintf("cannot inspect clone path %s: %v", clone, err)}
	}
	repos := s.repositories()
	login, err := repos.Login(ctx)
	if err != nil {
		return nil, &APIError{Code: Unavailable, Message: fmt.Sprintf("cannot confirm which GitHub account GITHUB_TOKEN is for: %v", err)}
	}
	if !strings.EqualFold(login, creation.RepoOwner) {
		return invalid("GITHUB_TOKEN is for the account %s, but projects.repo_owner is %s; repositories are created under the token's own account", login, creation.RepoOwner)
	}
	_, taken, err := repos.Repository(ctx, creation.RepoOwner, req.Repo)
	if err != nil {
		return nil, &APIError{Code: Unavailable, Message: fmt.Sprintf("cannot check whether %s/%s is free: %v", creation.RepoOwner, req.Repo, err)}
	}
	if taken {
		return invalid("the repository %s/%s already exists on GitHub; choose another repo name", creation.RepoOwner, req.Repo)
	}
	id := req.ID
	if id == "" {
		id, err = s.freshIdentity()
	} else if directory, derr := s.current().Root.ProjectTrace(id); derr != nil {
		err = derr
	} else if _, derr := os.Lstat(directory); derr == nil {
		return invalid("project %s already exists; creation identities are used once", id)
	} else if !os.IsNotExist(derr) {
		err = derr
	}
	if err != nil {
		return nil, &APIError{Code: Internal, Message: "cannot reserve a project identity under the root; check its permissions"}
	}
	return &pendingProject{
		Version: 1,
		Project: config.Project{Version: 1, ID: id, Name: name, Upstream: creation.RepoOwner + "/" + req.Repo, Clone: clone, BaseBranch: "main", Landing: "commit-per-unit"},
		Creation: &projectCreation{
			Owner: creation.RepoOwner, Repo: req.Repo, Visibility: visibility, Description: req.Description,
			Charter: req.Charter, CreateSent: true, Step: api.CreationPlanned,
		},
	}, nil
}

// validRepoName reports whether name is a repository name GitHub accepts.
func validRepoName(owner, name string) bool {
	return repoName.MatchString(name) && name != "." && name != ".." && !strings.HasSuffix(name, ".git") && config.ValidRepository(owner+"/"+name)
}

// samePlan checks a repeated request against the plan saved under its
// identity. A request carrying only the identity finishes the plan as saved.
func samePlan(pending pendingProject, req ProjectCreateRequest) *APIError {
	c := pending.Creation
	if req.Name == "" && req.Repo == "" && req.Description == "" && req.Visibility == "" && req.Charter == "" {
		return nil
	}
	visibility := req.Visibility
	if visibility == "" {
		visibility = api.VisibilityPrivate
	}
	if strings.TrimSpace(req.Name) != pending.Project.Name || req.Repo != c.Repo || req.Description != c.Description || visibility != c.Visibility || req.Charter != c.Charter {
		return &APIError{Code: Conflict, Message: fmt.Sprintf("project creation %s already holds a different plan for %s; repeat that request, or send only the id to finish it", pending.Project.ID, c.repository())}
	}
	return nil
}

func pendingRegistrationError(pending pendingProject) *APIError {
	id := pending.Project.ID
	if c := pending.Creation; c != nil {
		message := fmt.Sprintf("project creation %s for %s is pending at step %s; finish it with osmia project create --id %s before registering or creating another project", id, c.repository(), c.Step, id)
		if c.Stuck != nil {
			message += ": " + c.Stuck.Reason
		}
		return &APIError{Code: Conflict, Message: message}
	}
	return &APIError{Code: Conflict, Message: fmt.Sprintf("registration of project %s is pending; finish it with osmia project add before creating a project", id)}
}

// finishCreation runs the creation's remaining steps and registers the
// project. The caller holds projectMu.
func (s *Service) finishCreation(ctx context.Context, pending *pendingProject) (ProjectResponse, *APIError) {
	p, err := s.runCreation(ctx, pending, true)
	if err != nil {
		return ProjectResponse{}, creationIncomplete(pending.Project.ID, err)
	}
	return s.added(p), nil
}

func creationIncomplete(id config.ProjectID, err error) *APIError {
	var blocked *creationStuck
	if errors.As(err, &blocked) {
		return &APIError{Code: Conflict, Message: fmt.Sprintf("project creation %s is stuck (%s): %s; resolve it, then run osmia project create --id %s to finish it", id, blocked.Kind, blocked.Reason, id)}
	}
	return &APIError{Code: Internal, Message: fmt.Sprintf("project creation %s is incomplete (%v); run osmia project create --id %s to finish it", id, err, id)}
}

// runCreation does every creation step that is not yet done, then registers
// the project through the registration steps. Each step inspects the
// repository and the clone before acting.
func (s *Service) runCreation(ctx context.Context, pending *pendingProject, activate bool) (config.Project, error) {
	p := pending.Project
	if err := s.advanceCreation(ctx, pending); err != nil {
		var blocked *creationStuck
		if errors.As(err, &blocked) {
			pending.Creation.Stuck = &blocked.ProjectCreationStuck
			err = errors.Join(err, s.writePending(*pending))
		}
		return p, err
	}
	if conflict := pendingCloneConflict(s.current(), p); conflict != nil {
		return p, conflict
	}
	return s.complete(ctx, *pending, activate)
}

func (s *Service) advanceCreation(ctx context.Context, pending *pendingProject) error {
	if pending.Creation.Stuck != nil {
		pending.Creation.Stuck = nil
		if err := s.writePending(*pending); err != nil {
			return err
		}
	}
	if err := s.ensureRepository(ctx, pending); err != nil {
		return err
	}
	if err := s.ensureClone(ctx, pending); err != nil {
		return err
	}
	return s.ensureMainBranch(ctx, pending)
}

// advance moves the recorded step forward and saves it.
func (s *Service) advance(pending *pendingProject, step string) error {
	order := []string{api.CreationPlanned, api.CreationRepository, api.CreationCloned, api.CreationMain}
	index := func(name string) int {
		for i, n := range order {
			if n == name {
				return i
			}
		}
		return 0
	}
	if index(step) <= index(pending.Creation.Step) {
		return nil
	}
	pending.Creation.Step = step
	return s.writePending(*pending)
}

// ensureRepository finds this creation's repository or creates it. A
// repository is this creation's only when its identity is the one GitHub
// returned to this creation's own create call; having asked for the name is not
// enough. The create is sent at most once for the plan.
func (s *Service) ensureRepository(ctx context.Context, pending *pendingProject) error {
	c := pending.Creation
	repos := s.repositories()
	name := c.repository()
	existing, found, err := repos.Repository(ctx, c.Owner, c.Repo)
	if err != nil {
		return err
	}
	const resolve = "Rename or remove the other repository on GitHub, then retry"
	switch {
	case c.RepositoryID != 0 && found && existing.ID == c.RepositoryID:
		before := *c
		c.FullName, c.SSHURL, c.DefaultBranch = existing.FullName, firstNonEmpty(c.SSHURL, existing.SSHURL), existing.DefaultBranch
		if c.Step == api.CreationPlanned {
			c.Step = api.CreationRepository
		}
		if *c == before {
			return nil
		}
		return s.writePending(*pending)
	case c.RepositoryID != 0 && found:
		return stuck(api.StuckOwnership, "the repository %s on GitHub has identity %d, not the %d of the repository this creation made, so its ownership could not be established. %s", name, existing.ID, c.RepositoryID, resolve)
	case c.RepositoryID != 0 && !strings.EqualFold(c.FullName, name):
		return stuck(api.StuckMismatch, "GitHub created %s (identity %d) instead of %s, so the repository's ownership could not be established. Nothing was cloned or registered", c.FullName, c.RepositoryID, name)
	case c.RepositoryID != 0:
		return stuck(api.StuckOwnership, "the repository this creation made (%s, identity %d) is no longer found as %s, so its ownership could not be established", c.FullName, c.RepositoryID, name)
	case found:
		return stuck(api.StuckOwnership, "a repository named %s exists on GitHub and the saved plan holds no evidence that this creation made it, so its ownership could not be established. %s", name, resolve)
	}
	created, err := repos.CreateRepository(ctx, pulls.NewRepository{Name: c.Repo, Description: c.Description, Private: c.Visibility != api.VisibilityPublic})
	if err != nil {
		switch pulls.CreateOutcomeOf(err) {
		case pulls.CreateNameTaken:
			return stuck(api.StuckOwnership, "GitHub refused to create %s because the name is taken, so the repository's ownership could not be established. %s", name, resolve)
		case pulls.CreateRefused:
			return stuck(api.StuckRefused, "GitHub refused to create %s: %v", name, err)
		}
		return fmt.Errorf("the outcome of creating %s is unknown: %w", name, err)
	}
	c.RepositoryID, c.FullName, c.SSHURL, c.DefaultBranch = created.ID, created.FullName, created.SSHURL, created.DefaultBranch
	if strings.EqualFold(created.FullName, name) {
		c.Step = api.CreationRepository
	}
	if err := s.writePending(*pending); err != nil {
		return err
	}
	if !strings.EqualFold(created.FullName, name) {
		return stuck(api.StuckMismatch, "GitHub created %s (identity %d) instead of %s, so the repository's ownership could not be established. Nothing was cloned or registered", created.FullName, created.ID, name)
	}
	return s.step("repository-created")
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// ensureClone clones the repository into the project's clone path from the SSH
// URL GitHub returned. The clone is made beside its destination and renamed
// into place, so the destination exists only once the clone is whole.
func (s *Service) ensureClone(ctx context.Context, pending *pendingProject) error {
	c, clone := pending.Creation, pending.Project.Clone
	if _, err := os.Lstat(clone); err == nil {
		origin, oerr := creationGit(ctx, clone, "config", "--get", "remote.origin.url")
		_, herr := creationGit(ctx, clone, "rev-parse", "--verify", "HEAD^{commit}")
		if oerr != nil || herr != nil || strings.TrimSpace(origin) != c.SSHURL {
			return stuck(api.StuckClone, "%s already exists and is not a clone of %s; move it away, then retry", clone, c.repository())
		}
		return s.advance(pending, api.CreationCloned)
	} else if !os.IsNotExist(err) {
		return err
	}
	staging := filepath.Join(filepath.Dir(clone), "."+filepath.Base(clone)+".clone-"+string(pending.Project.ID))
	if err := os.RemoveAll(staging); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(clone), 0755); err != nil {
		return err
	}
	if _, err := creationGit(ctx, "", "clone", "--quiet", "--", c.SSHURL, staging); err != nil {
		os.RemoveAll(staging)
		return stuck(api.StuckClone, "cannot clone %s: %v. The host's git credential (SSH key, agent or credential helper) is the likely cause; check that the service user can run git clone %s, then retry", c.repository(), err, c.SSHURL)
	}
	if err := os.Rename(staging, clone); err != nil {
		return err
	}
	if err := s.advance(pending, api.CreationCloned); err != nil {
		return err
	}
	return s.step("repository-cloned")
}

// ensureMainBranch makes main the repository's default branch and checks it out
// in the clone. When GitHub initialised the repository on another branch, the
// README commit is pushed to main with the host's git and the repository's
// default branch is set to main; the account's own default-branch preference
// is not touched.
func (s *Service) ensureMainBranch(ctx context.Context, pending *pendingProject) error {
	c, clone := pending.Creation, pending.Project.Clone
	branch := func(err error, what string) error {
		return stuck(api.StuckBranch, "cannot %s in %s: %v", what, c.repository(), err)
	}
	if c.DefaultBranch == "" {
		return branch(errors.New("GitHub reported no default branch"), "find the default branch")
	}
	if c.DefaultBranch != "main" {
		source := "refs/remotes/origin/" + c.DefaultBranch
		if _, err := creationGit(ctx, clone, "push", "origin", source+":refs/heads/main"); err != nil {
			return branch(err, "put the README commit on main")
		}
		if err := s.repositories().SetDefaultBranch(ctx, c.Owner, c.Repo, "main"); err != nil {
			return branch(err, "make main the default branch")
		}
		c.DefaultBranch = "main"
	}
	if head, err := creationGit(ctx, clone, "symbolic-ref", "--short", "-q", "HEAD"); err != nil || strings.TrimSpace(head) != "main" {
		if _, err := creationGit(ctx, clone, "checkout", "--quiet", "-B", "main", "refs/remotes/origin/main"); err != nil {
			return branch(err, "check out main")
		}
		if _, err := creationGit(ctx, clone, "branch", "--quiet", "--set-upstream-to=origin/main", "main"); err != nil {
			return branch(err, "track origin/main")
		}
	}
	if err := s.advance(pending, api.CreationMain); err != nil {
		return err
	}
	return s.step("main-branch-ready")
}

// creationGit runs git for a creation in dir, or in the current directory when
// dir is empty. The command inherits the service's environment and so the
// host's git and SSH configuration, with GITHUB_TOKEN removed: the token is for
// GitHub's API only.
func creationGit(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, creationGitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = creationGitEnvironment(os.Environ())
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if cause := ctx.Err(); cause != nil {
			return "", fmt.Errorf("git %s: %w", args[0], cause)
		}
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		return stdout.String(), fmt.Errorf("git %s: %s", args[0], message)
	}
	return stdout.String(), nil
}

// creationGitEnvironment is env without GITHUB_TOKEN, and with prompting for
// a credential off so a missing one fails instead of waiting.
func creationGitEnvironment(env []string) []string {
	out := make([]string, 0, len(env)+1)
	for _, entry := range env {
		if name, _, _ := strings.Cut(entry, "="); name != "GITHUB_TOKEN" {
			out = append(out, entry)
		}
	}
	return append(out, "GIT_TERMINAL_PROMPT=0")
}
