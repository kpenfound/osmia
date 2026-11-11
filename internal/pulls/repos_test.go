package pulls

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// repositoryServer stands in for the GitHub REST API: a remote system whose
// real behaviour this fake does not reproduce. It checks the bearer token on
// every request and hands the request to handle.
func repositoryServer(t *testing.T, handle func(w http.ResponseWriter, r *http.Request, body map[string]any)) GitHub {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("%s %s sent authorization %q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		var body map[string]any
		if data, _ := io.ReadAll(r.Body); len(data) > 0 {
			if err := json.Unmarshal(data, &body); err != nil {
				t.Error(err)
			}
		}
		handle(w, r, body)
	}))
	t.Cleanup(server.Close)
	return GitHub{BaseURL: server.URL, Token: "secret", HTTP: server.Client()}
}

func TestLoginReportsTheAuthenticatedAccount(t *testing.T) {
	t.Parallel()
	g := repositoryServer(t, func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		if r.Method != http.MethodGet || r.URL.Path != "/user" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		io.WriteString(w, `{"login": "octo-cat"}`)
	})
	if login, err := g.Login(context.Background()); err != nil || login != "octo-cat" {
		t.Fatalf("login %q: %v", login, err)
	}
	refused := repositoryServer(t, func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	if _, err := refused.Login(context.Background()); err == nil {
		t.Fatal("a refused token reported an account")
	}
}

func TestRepositoryReportsFreeAndTakenNames(t *testing.T) {
	t.Parallel()
	g := repositoryServer(t, func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		switch r.URL.Path {
		case "/repos/octo/taken":
			io.WriteString(w, `{"id": 4242, "full_name": "octo/taken", "ssh_url": "git@github.com:octo/taken.git", "default_branch": "trunk", "private": true}`)
		case "/repos/octo/free":
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"message": "Not Found"}`)
		case "/repos/octo/broken":
			w.WriteHeader(http.StatusBadGateway)
		default:
			t.Errorf("unexpected %s", r.URL.Path)
		}
	})
	ctx := context.Background()
	repo, found, err := g.Repository(ctx, "octo", "taken")
	want := Repository{ID: 4242, FullName: "octo/taken", SSHURL: "git@github.com:octo/taken.git", DefaultBranch: "trunk"}
	if err != nil || !found || repo != want {
		t.Fatalf("taken: %+v found=%v %v", repo, found, err)
	}
	if repo, found, err := g.Repository(ctx, "octo", "free"); err != nil || found || repo != (Repository{}) {
		t.Fatalf("free: %+v found=%v %v", repo, found, err)
	}
	if _, found, err := g.Repository(ctx, "octo", "broken"); err == nil || found {
		t.Fatalf("a server error is neither free nor taken: found=%v %v", found, err)
	}
}

func TestCreateRepositorySendsTheRequestedRepository(t *testing.T) {
	t.Parallel()
	for _, private := range []bool{true, false} {
		var got map[string]any
		g := repositoryServer(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
			if r.Method != http.MethodPost || r.URL.Path != "/user/repos" {
				t.Errorf("%s %s", r.Method, r.URL.Path)
			}
			got = body
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{"id": 77, "full_name": "octo/widgets", "ssh_url": "git@github.com:octo/widgets.git", "default_branch": "master"}`)
		})
		repo, err := g.CreateRepository(context.Background(), NewRepository{Name: "widgets", Description: "Widgets, all of them", Private: private})
		want := Repository{ID: 77, FullName: "octo/widgets", SSHURL: "git@github.com:octo/widgets.git", DefaultBranch: "master"}
		if err != nil || repo != want {
			t.Fatalf("created %+v: %v", repo, err)
		}
		if len(got) != 4 || got["name"] != "widgets" || got["description"] != "Widgets, all of them" || got["private"] != private || got["auto_init"] != true {
			t.Fatalf("private=%v sent %v", private, got)
		}
	}
}

func TestCreateRepositoryTellsRefusalFromUnknownOutcome(t *testing.T) {
	t.Parallel()
	slow := make(chan struct{})
	t.Cleanup(func() { close(slow) })
	for name, tc := range map[string]struct {
		handle  http.HandlerFunc
		timeout time.Duration
		want    CreateOutcome
		status  int
	}{
		"name conflict": {func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnprocessableEntity)
			io.WriteString(w, `{"message": "Repository creation failed.", "errors": [{"resource": "Repository", "code": "custom", "field": "name", "message": "name already exists on this account"}]}`)
		}, 0, CreateNameTaken, http.StatusUnprocessableEntity},
		"bare 422": {func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnprocessableEntity)
		}, 0, CreateNameTaken, http.StatusUnprocessableEntity},
		"other validation failure": {func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnprocessableEntity)
			io.WriteString(w, `{"message": "Repository creation failed.", "errors": [{"resource": "Repository", "code": "custom", "field": "description", "message": "description is too long"}]}`)
		}, 0, CreateRefused, http.StatusUnprocessableEntity},
		"forbidden": {func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, `{"message": "Resource not accessible by personal access token"}`)
		}, 0, CreateRefused, http.StatusForbidden},
		"unauthorized": {func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		}, 0, CreateRefused, http.StatusUnauthorized},
		"server error": {func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
		}, 0, CreateUnknown, http.StatusBadGateway},
		"created with an unreadable answer": {func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `not json`)
		}, 0, CreateUnknown, http.StatusCreated},
		"created without an identity": {func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{}`)
		}, 0, CreateUnknown, http.StatusCreated},
		"timeout": {func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-slow:
			case <-r.Context().Done():
			}
		}, 50 * time.Millisecond, CreateUnknown, 0},
	} {
		t.Run(name, func(t *testing.T) {
			g := repositoryServer(t, func(w http.ResponseWriter, r *http.Request, _ map[string]any) { tc.handle(w, r) })
			g.HTTP = &http.Client{Timeout: tc.timeout}
			_, err := g.CreateRepository(context.Background(), NewRepository{Name: "widgets"})
			var create *CreateError
			if !errors.As(err, &create) || create.Outcome != tc.want || create.Status != tc.status || CreateOutcomeOf(err) != tc.want {
				t.Fatalf("got %v, want outcome %s with status %d", err, tc.want, tc.status)
			}
		})
	}
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	_, err := GitHub{BaseURL: closed.URL, Token: "secret"}.CreateRepository(context.Background(), NewRepository{Name: "widgets"})
	if CreateOutcomeOf(err) != CreateUnknown {
		t.Fatalf("a refused connection proves nothing: %v", err)
	}
}

func TestSetDefaultBranchPatchesTheRepository(t *testing.T) {
	t.Parallel()
	var got map[string]any
	g := repositoryServer(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		if r.Method != http.MethodPatch || r.URL.Path != "/repos/octo/widgets" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		got = body
		io.WriteString(w, `{"id": 77, "full_name": "octo/widgets", "default_branch": "main"}`)
	})
	if err := g.SetDefaultBranch(context.Background(), "octo", "widgets", "main"); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got["default_branch"] != "main" {
		t.Fatalf("sent %v", got)
	}
	refused := repositoryServer(t, func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
		w.WriteHeader(http.StatusForbidden)
	})
	if err := refused.SetDefaultBranch(context.Background(), "octo", "widgets", "main"); err == nil {
		t.Fatal("a refused change reported success")
	}
}
