package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	wire "github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest"
	"github.com/kpenfound/osmia/internal/trace"
)

func creationService(t *testing.T) (root, cloneDir string, github *servicetest.FakeGitHub) {
	t.Helper()
	t.Setenv("GITHUB_TOKEN", "fixture-token")
	opts, _ := emptyFixture(t)
	dir, err := os.MkdirTemp("", "occ-")
	must(t, err)
	t.Cleanup(func() { os.RemoveAll(dir) })
	cloneDir = filepath.Join(dir, "clones")
	remote, _ := servicetest.SeedRemote(t, dir, "newrepo", "main")
	github = servicetest.NewFakeGitHub(t, "owner")
	github.Remote = func(string) string { return remote }
	opts.Repositories = github.Client()
	root = opts.Config.Root
	path := filepath.Join(root, "config.toml")
	text, err := os.ReadFile(path)
	must(t, err)
	must(t, os.WriteFile(path, append(text, []byte(fmt.Sprintf("[projects]\nrepo_owner = \"owner\"\nclone_dir = %q\n", cloneDir))...), 0600))
	s, err := service.Start(context.Background(), opts)
	must(t, err)
	t.Cleanup(func() { s.Close() })
	return root, cloneDir, github
}

type createdProject struct {
	root, cloneDir, charter string
	github                  *servicetest.FakeGitHub
	response                wire.ProjectResponse
}

func writeCharterFile(t *testing.T) string {
	t.Helper()
	charter := filepath.Join(t.TempDir(), "charter.md")
	must(t, os.WriteFile(charter, []byte("# Charter\n\n1. Keep it small.\n"), 0600))
	return charter
}

func createNewProject(t *testing.T) createdProject {
	t.Helper()
	root, cloneDir, github := creationService(t)
	charter := writeCharterFile(t)
	c := createdProject{root: root, cloneDir: cloneDir, charter: charter, github: github}
	out := successful(t, root, "project", "create", "New Project", "--repo", "newrepo", "--description", "A new project", "--charter-file", charter, "--json")
	must(t, json.Unmarshal([]byte(out), &c.response))
	return c
}

func TestProjectCreateRefusesInvalidArgumentsBeforeReachingGitHub(t *testing.T) {
	root, cloneDir, github := creationService(t)
	charter := writeCharterFile(t)
	for _, args := range [][]string{
		{"project", "create", "New Project"},
		{"project", "create", "New Project", "--repo", "newrepo"},
		{"project", "create", "--repo", "newrepo", "--charter-file", charter},
		{"project", "add", "New Project", "--upstream", "owner/newrepo", "--clone", cloneDir, "--repo", "newrepo"},
		{"project", "create", "New Project", "--repo", "newrepo", "--charter-file", charter, "--id", "nope"},
	} {
		if code, out, diag := invoke(t, root, args...); code != 2 || out != "" || !strings.Contains(diag, "invalid arguments") {
			t.Fatalf("%v: %d %q %q", args, code, out, diag)
		}
	}
	if github.CreateCalls() != 0 {
		t.Fatal("a refused command reached GitHub")
	}
}

func TestProjectCreateRefusesAnUnreadableCharterBeforeReachingGitHub(t *testing.T) {
	root, _, github := creationService(t)
	if code, _, diag := invoke(t, root, "project", "create", "New Project", "--repo", "newrepo", "--charter-file", filepath.Join(t.TempDir(), "missing.md")); code != 1 || !strings.Contains(diag, "cannot read the charter") {
		t.Fatalf("%d %q", code, diag)
	}
	if github.CreateCalls() != 0 {
		t.Fatal("a refused command reached GitHub")
	}
}

func TestProjectCreateRegistersTheProjectOnTheNewRepository(t *testing.T) {
	c := createNewProject(t)
	created := c.response
	if created.Project.Name != "New Project" || created.Project.Upstream != "owner/newrepo" || created.Project.Fork != "" || created.Project.BaseBranch != "main" || created.Project.Clone != filepath.Join(c.cloneDir, "newrepo") {
		t.Fatalf("%+v", created)
	}
}

func TestProjectCreateMakesAPrivateRepositoryWithTheDescription(t *testing.T) {
	c := createNewProject(t)
	repo, ok := c.github.Repo("owner/newrepo")
	if !ok || !repo.Private || !repo.AutoInit || repo.Description != "A new project" {
		t.Fatalf("%+v", repo)
	}
}

func TestProjectCreateRecordsTheCharterFile(t *testing.T) {
	c := createNewProject(t)
	client := service.NewClient(filepath.Join(c.root, "osmia.sock"))
	defer client.Close()
	var doc trace.Document
	must(t, client.Do(context.Background(), "GET", wire.Prefix+"/projects/charter/"+string(c.response.Project.ID), nil, &doc))
	if doc.Revision != 1 || doc.Content != "# Charter\n\n1. Keep it small.\n" {
		t.Fatalf("charter: %+v", doc)
	}
}

func TestProjectCreateAgainWithTheSameIdentityCreatesNothingNew(t *testing.T) {
	c := createNewProject(t)
	again := successful(t, c.root, "project", "create", "New Project", "--repo", "newrepo", "--description", "A new project", "--charter-file", c.charter, "--id", string(c.response.Project.ID), "--json")
	var repeated wire.ProjectResponse
	must(t, json.Unmarshal([]byte(again), &repeated))
	if repeated.Project.ID != c.response.Project.ID || c.github.CreateCalls() != 1 {
		t.Fatalf("%+v, %d create calls", repeated, c.github.CreateCalls())
	}
}

func TestProjectCreateRefusesARepositoryNameThatIsTaken(t *testing.T) {
	root, _, github := creationService(t)
	github.AddRepo("owner/other", "elsewhere", "main")
	code, text, diag := invoke(t, root, "project", "create", "Again", "--repo", "other", "--charter-file", writeCharterFile(t))
	if code != 4 || text != "" || !strings.Contains(diag, "already exists on GitHub") {
		t.Fatalf("a taken name: %d %q %q", code, text, diag)
	}
}

func TestProjectCreateReadsTheCharterFromStandardInput(t *testing.T) {
	root, _, github := creationService(t)
	var out, diag bytes.Buffer
	code := Run(context.Background(), []string{"--root", root, "project", "create", "Piped", "--repo", "newrepo", "--public", "--charter-file", "-"}, strings.NewReader("# Charter\n\n1. Piped rule.\n"), &out, &diag)
	if code != 0 || diag.Len() != 0 {
		t.Fatalf("%d %q %q", code, out.String(), diag.String())
	}
	for _, want := range []string{"Project p_", "(Piped) created", "Upstream: owner/newrepo"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output lacks %q:\n%s", want, out.String())
		}
	}
	if repo, _ := github.Repo("owner/newrepo"); repo.Private {
		t.Fatalf("--public created a private repository: %+v", repo)
	}
}

func TestProjectCreateWithAnIdentityRefusesInvalidArguments(t *testing.T) {
	root, _, _ := creationService(t)
	for _, args := range [][]string{
		{"project", "create", "--id", "nope"},
		{"project", "create", "--id", "p_0123456789abcdef0123456789abcdef", "--repo", "newrepo"},
		{"project", "create", "--id", "p_0123456789abcdef0123456789abcdef", "--public"},
		{"project", "create"},
	} {
		if code, out, diag := invoke(t, root, args...); code != 2 || out != "" || !strings.Contains(diag, "invalid arguments") {
			t.Fatalf("%v: %d %q %q", args, code, out, diag)
		}
	}
}

func TestProjectCreateWithOnlyAnIdentityRepeatsTheExistingProject(t *testing.T) {
	c := createNewProject(t)
	again := successful(t, c.root, "project", "create", "--id", string(c.response.Project.ID), "--json")
	var repeated wire.ProjectResponse
	must(t, json.Unmarshal([]byte(again), &repeated))
	if repeated.Project.ID != c.response.Project.ID || c.github.CreateCalls() != 1 {
		t.Fatalf("%+v, %d create calls", repeated, c.github.CreateCalls())
	}
}

func TestProjectCreateWithAnUnknownIdentityAndNoPlanFails(t *testing.T) {
	root, _, _ := creationService(t)
	code, out, diag := invoke(t, root, "project", "create", "--id", "p_0123456789abcdef0123456789abcdef")
	if code == 0 || out != "" || diag == "" {
		t.Fatalf("an identity with no plan: %d %q %q", code, out, diag)
	}
}
