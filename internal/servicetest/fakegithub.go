package servicetest

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/kpenfound/osmia/internal/pulls"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
)

// GitHubCall is one request the fake GitHub received.
type GitHubCall struct {
	Method string
	Path   string
	Body   map[string]any
}

// GitHubRepo is a repository the fake GitHub holds.
type GitHubRepo struct {
	ID            int64
	FullName      string
	SSHURL        string
	DefaultBranch string
	Private       bool
	Description   string
	AutoInit      bool
}

// FakeGitHub stands in for the GitHub REST API: a remote system whose real
// behaviour it does not reproduce. It answers the calls project creation
// makes, hands out the SSH URLs its tests set (local bare repositories, so no
// remote is ever contacted) and records every request, including any delete.
type FakeGitHub struct {
	Server *httptest.Server
	// Login is the account the token authenticates as.
	Login string
	// DefaultBranch is the default branch a created repository gets, which
	// GitHub takes from the account's own preference.
	DefaultBranch string
	// Remote returns the SSH URL a created repository is given.
	Remote func(name string) string
	// OnCreate runs before a create call is handled. When it reports handled,
	// its status and body are the answer and nothing is created.
	OnCreate func(g *FakeGitHub, body map[string]any) (handled bool, status int, response any)

	mu     sync.Mutex
	repos  map[string]*GitHubRepo
	calls  []GitHubCall
	nextID int64
}

// NewFakeGitHub starts a fake GitHub whose token belongs to login.
func NewFakeGitHub(t *testing.T, login string) *FakeGitHub {
	t.Helper()
	g := &FakeGitHub{Login: login, DefaultBranch: "main", repos: map[string]*GitHubRepo{}, nextID: 1000}
	g.Server = httptest.NewServer(http.HandlerFunc(g.serve))
	t.Cleanup(g.Server.Close)
	return g
}

// Client returns the repository client the service uses against g.
func (g *FakeGitHub) Client() pulls.Repositories {
	return pulls.GitHub{BaseURL: g.Server.URL, Token: "fake-token", HTTP: g.Server.Client()}
}

// AddRepo makes a repository exist and returns its identity.
func (g *FakeGitHub) AddRepo(fullName, sshURL, defaultBranch string) int64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.nextID++
	g.repos[strings.ToLower(fullName)] = &GitHubRepo{ID: g.nextID, FullName: fullName, SSHURL: sshURL, DefaultBranch: defaultBranch}
	return g.nextID
}

// Repo returns a copy of the repository fullName, if it exists.
func (g *FakeGitHub) Repo(fullName string) (GitHubRepo, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	r, ok := g.repos[strings.ToLower(fullName)]
	if !ok {
		return GitHubRepo{}, false
	}
	return *r, true
}

// Forget makes a repository stop existing, as when its owner renames or
// removes it on GitHub. The service itself has no way to do this.
func (g *FakeGitHub) Forget(fullName string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.repos, strings.ToLower(fullName))
}

// SetSSHURL changes the SSH URL of an existing repository.
func (g *FakeGitHub) SetSSHURL(fullName, sshURL string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.repos[strings.ToLower(fullName)].SSHURL = sshURL
}

// Calls returns every request received so far.
func (g *FakeGitHub) Calls() []GitHubCall {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]GitHubCall(nil), g.calls...)
}

// CreateCalls counts the create requests received.
func (g *FakeGitHub) CreateCalls() int {
	n := 0
	for _, c := range g.Calls() {
		if c.Method == http.MethodPost && c.Path == "/user/repos" {
			n++
		}
	}
	return n
}

// Writes returns the requests that change something: everything but reads.
func (g *FakeGitHub) Writes() []GitHubCall {
	var out []GitHubCall
	for _, c := range g.Calls() {
		if c.Method != http.MethodGet {
			out = append(out, c)
		}
	}
	return out
}

// Deleted reports whether any delete request arrived.
func (g *FakeGitHub) Deleted() bool {
	for _, c := range g.Calls() {
		if c.Method == http.MethodDelete {
			return true
		}
	}
	return false
}

func (g *FakeGitHub) serve(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if data, _ := io.ReadAll(r.Body); len(data) > 0 {
		_ = json.Unmarshal(data, &body)
	}
	g.mu.Lock()
	g.calls = append(g.calls, GitHubCall{Method: r.Method, Path: r.URL.Path, Body: body})
	g.mu.Unlock()
	reply := func(status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		reply(http.StatusUnauthorized, map[string]string{"message": "Requires authentication"})
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/user":
		reply(http.StatusOK, map[string]string{"login": g.Login})
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/"):
		if repo, ok := g.Repo(strings.TrimPrefix(r.URL.Path, "/repos/")); ok {
			reply(http.StatusOK, repo.json())
			return
		}
		reply(http.StatusNotFound, map[string]string{"message": "Not Found"})
	case r.Method == http.MethodPost && r.URL.Path == "/user/repos":
		g.create(reply, body)
	case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/repos/"):
		g.mu.Lock()
		repo, ok := g.repos[strings.ToLower(strings.TrimPrefix(r.URL.Path, "/repos/"))]
		if ok {
			if branch, _ := body["default_branch"].(string); branch != "" {
				repo.DefaultBranch = branch
			}
		}
		g.mu.Unlock()
		if !ok {
			reply(http.StatusNotFound, map[string]string{"message": "Not Found"})
			return
		}
		reply(http.StatusOK, repo.json())
	default:
		reply(http.StatusMethodNotAllowed, map[string]string{"message": "the fake GitHub does not serve " + r.Method + " " + r.URL.Path})
	}
}

func (r *GitHubRepo) json() map[string]any {
	return map[string]any{"id": r.ID, "full_name": r.FullName, "ssh_url": r.SSHURL, "default_branch": r.DefaultBranch, "private": r.Private}
}

func (g *FakeGitHub) create(reply func(int, any), body map[string]any) {
	if g.OnCreate != nil {
		if handled, status, response := g.OnCreate(g, body); handled {
			reply(status, response)
			return
		}
	}
	name, _ := body["name"].(string)
	full := g.Login + "/" + name
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, taken := g.repos[strings.ToLower(full)]; taken {
		reply(http.StatusUnprocessableEntity, GitHubNameTaken)
		return
	}
	g.nextID++
	repo := &GitHubRepo{ID: g.nextID, FullName: full, SSHURL: g.Remote(name), DefaultBranch: g.DefaultBranch}
	repo.Private, _ = body["private"].(bool)
	repo.Description, _ = body["description"].(string)
	repo.AutoInit, _ = body["auto_init"].(bool)
	g.repos[strings.ToLower(full)] = repo
	reply(http.StatusCreated, repo.json())
}

// GitHubNameTaken is GitHub's answer to a create whose name is taken.
var GitHubNameTaken = map[string]any{
	"message": "Repository creation failed.",
	"errors":  []map[string]string{{"resource": "Repository", "code": "custom", "field": "name", "message": "name already exists on this account"}},
}

// SeedRemote makes a bare repository at dir/name.git that stands in for the
// remote of a created repository: one README commit on branch, which is its
// HEAD. It returns the repository's path, used as the SSH URL, and the README
// commit.
func SeedRemote(t *testing.T, dir, name, branch string) (string, string) {
	t.Helper()
	remote := filepath.Join(dir, name+".git")
	work := filepath.Join(dir, name+"-seed")
	git := func(args ...string) string {
		return fixturekit.DemoGit(t, dir, append([]string{"-c", "user.name=GitHub", "-c", "user.email=noreply@github.com"}, args...)...)
	}
	git("init", "--quiet", "--bare", "-b", branch, remote)
	git("init", "--quiet", "-b", branch, work)
	fixturekit.Must(t, os.WriteFile(filepath.Join(work, "README.md"), []byte("# "+name+"\n"), 0600))
	git("-C", work, "add", "README.md")
	git("-C", work, "commit", "--quiet", "-m", "Initial commit")
	git("-C", work, "push", "--quiet", remote, branch)
	return remote, strings.TrimSpace(git("-C", work, "rev-parse", "HEAD"))
}
