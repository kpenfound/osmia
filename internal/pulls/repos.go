package pulls

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/kpenfound/osmia/internal/ghlog"
)

// Repository is one GitHub repository. ID is GitHub's numeric identity for
// it, which stays the same when the repository is renamed or transferred and
// differs for any other repository later created under the same name.
type Repository struct {
	ID            int64
	FullName      string
	SSHURL        string
	DefaultBranch string
}

// NewRepository is a repository to create for the authenticated user. The
// repository is initialised with a README, so it has a first commit.
type NewRepository struct {
	Name        string
	Description string
	Private     bool
}

// Repositories reads and creates repositories of the authenticated user.
type Repositories interface {
	// Login is the account the token authenticates as.
	Login(ctx context.Context) (string, error)
	// Repository reads owner/name. A repository that does not exist is
	// reported as not found, not as an error.
	Repository(ctx context.Context, owner, name string) (repository Repository, found bool, err error)
	// CreateRepository creates the repository under the authenticated user.
	// Its error is a *CreateError.
	CreateRepository(ctx context.Context, repository NewRepository) (Repository, error)
	// SetDefaultBranch makes branch the default branch of owner/name.
	SetDefaultBranch(ctx context.Context, owner, name, branch string) error
}

// CreateOutcome says what a failed create call established.
type CreateOutcome string

const (
	// CreateNameTaken is GitHub's definite refusal because a repository of
	// that name already exists.
	CreateNameTaken CreateOutcome = "name_taken"
	// CreateRefused is GitHub's definite refusal for any other reason: the
	// repository was not created.
	CreateRefused CreateOutcome = "refused"
	// CreateUnknown means the repository may or may not have been created: the
	// request got no answer, GitHub answered with a server error, or its
	// answer could not be read.
	CreateUnknown CreateOutcome = "unknown"
)

// CreateError is the failure of a create call, classified by what it proves.
type CreateError struct {
	Outcome CreateOutcome
	// Status is GitHub's HTTP status, zero when it did not answer.
	Status int
	Err    error
}

func (e *CreateError) Error() string { return "create repository: " + e.Err.Error() }
func (e *CreateError) Unwrap() error { return e.Err }

// CreateOutcomeOf is the outcome of a failed create call. An error that is not
// a CreateError proves nothing about GitHub, so it is reported as unknown.
func CreateOutcomeOf(err error) CreateOutcome {
	var create *CreateError
	if errors.As(err, &create) {
		return create.Outcome
	}
	return CreateUnknown
}

type githubRepository struct {
	ID            int64  `json:"id"`
	FullName      string `json:"full_name"`
	SSHURL        string `json:"ssh_url"`
	DefaultBranch string `json:"default_branch"`
}

func (r githubRepository) repository() Repository {
	return Repository(r)
}

// Login reads the account the token authenticates as.
func (g GitHub) Login(ctx context.Context) (string, error) {
	var user struct {
		Login string `json:"login"`
	}
	if err := g.do(ctx, "user.read", ghlog.Read, "", http.MethodGet, "/user", nil, http.StatusOK, &user); err != nil {
		return "", fmt.Errorf("read the authenticated account: %w", err)
	}
	if user.Login == "" {
		return "", errors.New("read the authenticated account: GitHub named no account")
	}
	return user.Login, nil
}

func repositoryPath(owner, name string) string {
	return "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name)
}

// Repository reads owner/name; a 404 means no repository of that name is
// visible to the token.
func (g GitHub) Repository(ctx context.Context, owner, name string) (Repository, bool, error) {
	var repo githubRepository
	err := g.do(ctx, "repos.read", ghlog.Read, owner+"/"+name, http.MethodGet, repositoryPath(owner, name), nil, http.StatusOK, &repo)
	var response *ResponseError
	if errors.As(err, &response) && response.Status == http.StatusNotFound {
		return Repository{}, false, nil
	}
	if err != nil {
		return Repository{}, false, fmt.Errorf("read repository %s/%s: %w", owner, name, err)
	}
	return repo.repository(), true, nil
}

// CreateRepository creates a repository under the authenticated user. Every
// error is a *CreateError whose Outcome says whether GitHub refused the
// request or the result is unknown.
func (g GitHub) CreateRepository(ctx context.Context, repository NewRepository) (Repository, error) {
	body := map[string]any{"name": repository.Name, "description": repository.Description, "private": repository.Private, "auto_init": true}
	var created githubRepository
	err := g.do(ctx, "repos.create", ghlog.Write, repository.Name, http.MethodPost, "/user/repos", body, http.StatusCreated, &created)
	if err != nil {
		return Repository{}, classifyCreate(err)
	}
	if created.ID == 0 || created.FullName == "" || created.SSHURL == "" {
		return Repository{}, &CreateError{Outcome: CreateUnknown, Status: http.StatusCreated, Err: errors.New("GitHub's answer did not identify the repository")}
	}
	return created.repository(), nil
}

// classifyCreate decides what a failed create call proves. Only a 4xx answer
// shows that nothing was created; anything else may have created it.
func classifyCreate(err error) *CreateError {
	var response *ResponseError
	if !errors.As(err, &response) {
		return &CreateError{Outcome: CreateUnknown, Err: err}
	}
	switch {
	case response.Status == http.StatusUnprocessableEntity && nameExists(response):
		return &CreateError{Outcome: CreateNameTaken, Status: response.Status, Err: err}
	case response.Status >= 400 && response.Status < 500 && response.Status != http.StatusRequestTimeout:
		return &CreateError{Outcome: CreateRefused, Status: response.Status, Err: err}
	}
	return &CreateError{Outcome: CreateUnknown, Status: response.Status, Err: err}
}

// nameExists reports whether a 422 is the name conflict. GitHub says so in its
// per-field messages; a 422 that carries none is taken to be the conflict,
// since the create call sends a valid name and the conflict is the refusal it
// expects.
func nameExists(response *ResponseError) bool {
	if len(response.Details) == 0 {
		return true
	}
	for _, d := range response.Details {
		if strings.Contains(strings.ToLower(d), "already exists") {
			return true
		}
	}
	return false
}

// SetDefaultBranch changes the default branch with the repository update
// endpoint, which needs only the repository's administration permission.
func (g GitHub) SetDefaultBranch(ctx context.Context, owner, name, branch string) error {
	var repo githubRepository
	err := g.do(ctx, "repos.default_branch", ghlog.Write, owner+"/"+name, http.MethodPatch, repositoryPath(owner, name), map[string]any{"default_branch": branch}, http.StatusOK, &repo)
	if err != nil {
		return fmt.Errorf("make %s the default branch of %s/%s: %w", branch, owner, name, err)
	}
	return nil
}

var _ Repositories = GitHub{}
