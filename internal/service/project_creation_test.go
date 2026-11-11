package service

import (
	"slices"
	"strings"
	"testing"
)

func TestCreationRoutesAreListed(t *testing.T) {
	want := map[string]string{
		"POST " + Prefix + "/projects/create":          "",
		"POST " + Prefix + "/projects/create/identity": "",
		"GET " + Prefix + "/projects/create/{id}":      "",
	}
	for _, route := range Routes {
		delete(want, route.Method+" "+route.Path)
	}
	if len(want) != 0 {
		t.Fatalf("routes missing from Routes: %v", want)
	}
}

func TestCreationGitEnvironmentDropsOnlyTheToken(t *testing.T) {
	env := creationGitEnvironment([]string{"PATH=/bin", "GITHUB_TOKEN=secret", "HOME=/home/owner", "GITHUB_TOKEN_FILE=/keep", "SSH_AUTH_SOCK=/agent"})
	want := []string{"PATH=/bin", "HOME=/home/owner", "GITHUB_TOKEN_FILE=/keep", "SSH_AUTH_SOCK=/agent", "GIT_TERMINAL_PROMPT=0"}
	if !slices.Equal(env, want) {
		t.Fatalf("%v", env)
	}
}

func TestRepositoryNames(t *testing.T) {
	for name, valid := range map[string]bool{
		"project": true, "my-project": true, "my_project.v2": true, "a": true, strings.Repeat("a", 100): true,
		"": false, ".": false, "..": false, ".github": false, "-lead": false, "has space": false, "a/b": false,
		"repo.git": false, strings.Repeat("a", 101): false, "naïve": false,
	} {
		if got := validRepoName("owner", name); got != valid {
			t.Errorf("validRepoName(%q) = %v", name, got)
		}
	}
}
