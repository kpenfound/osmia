package doctor

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// account is a fake GitHub API whose token authenticates as login. A nil
// scopes sends no X-OAuth-Scopes header, as GitHub does for a fine-grained
// token.
func account(t *testing.T, login string, scopes *string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer good" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path != "/user" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if scopes != nil {
			w.Header().Set("X-OAuth-Scopes", *scopes)
		}
		fmt.Fprintf(w, `{"login":%q}`, login)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func withProjects(t *testing.T, dir, table string) {
	t.Helper()
	top, err := os.ReadFile(filepath.Join(dir, "config.toml"))
	must(t, err)
	must(t, os.WriteFile(filepath.Join(dir, "config.toml"), append(top, "\n"+table...), 0o600))
}

func scopes(s string) *string { return &s }

func TestCreationNotConfiguredWithEitherKeyMissing(t *testing.T) {
	for name, table := range map[string]string{
		"neither":    "",
		"no owner":   "[projects]\nclone_dir = \"%s\"\n",
		"no clone":   "[projects]\nrepo_owner = \"owner\"\n",
		"empty both": "[projects]\n",
	} {
		t.Run(name, func(t *testing.T) {
			o, dir := fixture(t)
			if table != "" {
				if name == "no owner" {
					table = fmt.Sprintf(table, filepath.Join(filepath.Dir(dir), "clones"))
				}
				withProjects(t, dir, table)
			}
			o.GitHubAPI = account(t, "owner", scopes("repo"))
			checks := Run(context.Background(), o)
			expect(t, find(t, checks, "project creation"), Pass, "not configured")
			absent(t, checks, "creation account")
			absent(t, checks, "creation permission")
		})
	}
}

func configured(t *testing.T, login string, header *string) []Check {
	t.Helper()
	o, dir := fixture(t)
	withProjects(t, dir, fmt.Sprintf("[projects]\nrepo_owner = \"Owner\"\nclone_dir = %q\n", filepath.Join(filepath.Dir(dir), "clones")))
	o.GitHubAPI = account(t, login, header)
	return Run(context.Background(), o)
}

func TestCreationAccountMustBeRepoOwner(t *testing.T) {
	checks := configured(t, "owner", scopes("repo"))
	expect(t, find(t, checks, "project creation"), Pass, "configured")
	expect(t, find(t, checks, "creation account"), Pass, "owner")

	checks = configured(t, "someone-else", scopes("repo"))
	c := find(t, checks, "creation account")
	expect(t, c, Warn, "someone-else")
	expect(t, c, Warn, "projects.repo_owner is Owner")
}

func TestCreationPermissionFromScopes(t *testing.T) {
	expect(t, find(t, configured(t, "owner", scopes("gist, repo, workflow")), "creation permission"), Pass, "repo scope")
	expect(t, find(t, configured(t, "owner", scopes("gist, read:org")), "creation permission"), Warn, "do not include repo")
	// public_repo and repo:status are not the repo scope.
	expect(t, find(t, configured(t, "owner", scopes("public_repo, repo:status")), "creation permission"), Warn, "do not include repo")
	expect(t, find(t, configured(t, "owner", nil), "creation permission"), Warn, "cannot confirm")
	expect(t, find(t, configured(t, "owner", scopes("")), "creation permission"), Warn, "cannot confirm")
}

func TestCreationWithoutAUsableToken(t *testing.T) {
	o, dir := fixture(t)
	withProjects(t, dir, fmt.Sprintf("[projects]\nrepo_owner = \"owner\"\nclone_dir = %q\n", filepath.Join(filepath.Dir(dir), "clones")))
	o.Token = ""
	checks := Run(context.Background(), o)
	expect(t, find(t, checks, "creation account"), Warn, "cannot confirm")
	expect(t, find(t, checks, "creation permission"), Warn, "cannot confirm")
	o.Token = "bad"
	checks = Run(context.Background(), o)
	expect(t, find(t, checks, "creation account"), Warn, "cannot confirm")
	expect(t, find(t, checks, "creation permission"), Warn, "cannot confirm")
}
