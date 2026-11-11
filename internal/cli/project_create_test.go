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

// creationService starts a service with an empty root, a [projects] table and a
// fake GitHub whose created repository is a local bare repository.
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

func TestProjectCreateCommand(t *testing.T) {
	root, cloneDir, github := creationService(t)
	charter := filepath.Join(t.TempDir(), "charter.md")
	must(t, os.WriteFile(charter, []byte("# Charter\n\n1. Keep it small.\n"), 0600))

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
	if code, _, diag := invoke(t, root, "project", "create", "New Project", "--repo", "newrepo", "--charter-file", filepath.Join(t.TempDir(), "missing.md")); code != 1 || !strings.Contains(diag, "cannot read the charter") {
		t.Fatalf("%d %q", code, diag)
	}
	if github.CreateCalls() != 0 {
		t.Fatal("a refused command reached GitHub")
	}

	var created wire.ProjectResponse
	out := successful(t, root, "project", "create", "New Project", "--repo", "newrepo", "--description", "A new project", "--charter-file", charter, "--json")
	must(t, json.Unmarshal([]byte(out), &created))
	if created.Project.Name != "New Project" || created.Project.Upstream != "owner/newrepo" || created.Project.Fork != "" || created.Project.BaseBranch != "main" || created.Project.Clone != filepath.Join(cloneDir, "newrepo") {
		t.Fatalf("%+v", created)
	}
	repo, ok := github.Repo("owner/newrepo")
	if !ok || !repo.Private || !repo.AutoInit || repo.Description != "A new project" {
		t.Fatalf("%+v", repo)
	}
	// The charter is the project's charter.
	client := service.NewClient(filepath.Join(root, "osmia.sock"))
	defer client.Close()
	var doc trace.Document
	must(t, client.Do(context.Background(), "GET", wire.Prefix+"/projects/charter/"+string(created.Project.ID), nil, &doc))
	if doc.Revision != 1 || doc.Content != "# Charter\n\n1. Keep it small.\n" {
		t.Fatalf("charter: %+v", doc)
	}
	// Repeating the command with the same identity returns the project.
	again := successful(t, root, "project", "create", "New Project", "--repo", "newrepo", "--description", "A new project", "--charter-file", charter, "--id", string(created.Project.ID), "--json")
	var repeated wire.ProjectResponse
	must(t, json.Unmarshal([]byte(again), &repeated))
	if repeated.Project.ID != created.Project.ID || github.CreateCalls() != 1 {
		t.Fatalf("%+v, %d create calls", repeated, github.CreateCalls())
	}
	github.AddRepo("owner/other", "elsewhere", "main")
	code, text, diag := invoke(t, root, "project", "create", "Again", "--repo", "other", "--charter-file", charter)
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
