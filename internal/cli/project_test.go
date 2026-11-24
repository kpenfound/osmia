package cli

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	wire "github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/trace"
)

func emptyFixture(t *testing.T) (service.Options, string) {
	t.Helper()
	home, err := os.MkdirTemp("", "ocp-")
	must(t, err)
	t.Cleanup(func() { os.RemoveAll(home) })
	root := filepath.Join(home, ".osmia")
	must(t, os.MkdirAll(root, 0700))
	must(t, os.WriteFile(filepath.Join(root, "config.toml"), []byte(`# owner comment
version=1
[profiles.default]
agent="claude"
model="test"
`), 0600))
	clone := filepath.Join(home, "clone")
	must(t, os.Mkdir(clone, 0700))
	cmd := exec.Command("git", "init", "--quiet", clone)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	return service.Options{Config: config.Options{Root: root}}, clone
}

func startEmpty(t *testing.T) (root, clone string) {
	t.Helper()
	opts, clone := emptyFixture(t)
	s, err := service.Start(context.Background(), opts)
	must(t, err)
	t.Cleanup(func() { s.Close() })
	return opts.Config.Root, clone
}

func addDagger(t *testing.T, root, clone string, extra ...string) wire.ProjectResponse {
	t.Helper()
	args := append([]string{"project", "add", "dagger", "--upstream", "dagger/dagger", "--fork", "owner/dagger", "--clone", clone}, extra...)
	var added wire.ProjectResponse
	must(t, json.Unmarshal([]byte(successful(t, root, append(args, "--json")...)), &added))
	return added
}

func TestStatusWithoutAProjectNamesNone(t *testing.T) {
	root, _ := startEmpty(t)
	status := successful(t, root, "status")
	if !strings.Contains(status, "Project: none configured") || !strings.Contains(status, "no_project") {
		t.Fatalf("status without a project:\n%s", status)
	}
	var st struct {
		Configuration wire.ConfigResponse
	}
	must(t, json.Unmarshal([]byte(successful(t, root, "status", "--json")), &st))
	if st.Configuration.Project != nil {
		t.Fatal(st.Configuration)
	}
}

func TestProjectCommandsWithoutAProjectPointToProjectAdd(t *testing.T) {
	root, _ := startEmpty(t)
	for _, args := range [][]string{{"priority", "set", stream}, {"priority", "clear"}, {"pause", stream}} {
		code, out, diag := invoke(t, root, args...)
		if code != 4 || out != "" || !strings.Contains(diag, "osmia project add") {
			t.Fatalf("%v: %d %s %s", args, code, out, diag)
		}
	}
}

func TestProjectAddRefusesAMissingClone(t *testing.T) {
	root, clone := startEmpty(t)
	code, out, diag := invoke(t, root, "project", "add", "dagger", "--upstream", "dagger/dagger", "--fork", "owner/dagger", "--clone", filepath.Join(clone, "missing"))
	if code != 4 || out != "" || !strings.Contains(diag, "validation: clone does not exist") {
		t.Fatalf("%d %s %s", code, out, diag)
	}
}

func TestProjectRemoveWithoutAProjectIsRefused(t *testing.T) {
	root, _ := startEmpty(t)
	code, out, diag := invoke(t, root, "project", "remove", project)
	if code != 4 || out != "" || !strings.Contains(diag, "no_project") {
		t.Fatalf("%d %s %s", code, out, diag)
	}
}

func TestProjectAddRegistersTheProjectAndLeavesTheConfigurationFileAlone(t *testing.T) {
	root, clone := startEmpty(t)
	wd, err := os.Getwd()
	must(t, err)
	relative, err := filepath.Rel(wd, clone)
	must(t, err)
	added := addDagger(t, root, relative, "--base-branch", "develop")
	id := added.Project.ID
	if err := config.CheckProjectIDs(id); err != nil || added.Project.BaseBranch != "develop" || !strings.HasSuffix(added.Project.Clone, "clone") || !strings.Contains(added.NextStep, "osmia edit charter "+string(id)) {
		t.Fatalf("%+v %v", added, err)
	}
	text, err := os.ReadFile(filepath.Join(root, "config.toml"))
	must(t, err)
	if !strings.HasPrefix(string(text), "# owner comment\n") || strings.Contains(string(text), string(id)) {
		t.Fatalf("configuration:\n%s", text)
	}
	status := successful(t, root, "status")
	if !strings.Contains(status, "Project: "+string(id)+" (dagger)") || !strings.Contains(status, "Trace: "+added.Project.Trace) || strings.Contains(status, "Diagnostic") {
		t.Fatalf("status with a project:\n%s", status)
	}
	if !strings.Contains(status, "Charter: 0 rules, revision 1; edit it with osmia edit charter "+string(id)+"\n") {
		t.Fatalf("status with the template charter:\n%s", status)
	}
}

func TestStatusShowsTheCharterRulesAndTheirNumberingDiagnostics(t *testing.T) {
	root, clone := startEmpty(t)
	id := addDagger(t, root, clone).Project.ID
	setCharter(t, root, id, "# Charter\n1. Keep changes small.\n1. Test every fix.\n")
	status := successful(t, root, "status")
	if !strings.Contains(status, "Charter: 2 rules, revision 2; edit it with osmia edit charter "+string(id)+"\n  charter.md:3: rule 1 is numbered 2 times") {
		t.Fatalf("status with charter rules:\n%s", status)
	}
	var st struct {
		Configuration wire.ConfigResponse
	}
	must(t, json.Unmarshal([]byte(successful(t, root, "status", "--json")), &st))
	if cs := st.Configuration.Project.CharterState; cs == nil || cs.Rules != 2 || cs.Revision != 2 || len(cs.Diagnostics) != 1 {
		t.Fatalf("status JSON: %+v", cs)
	}
}

func TestHandInRefusesAnInactiveProject(t *testing.T) {
	root, clone := startEmpty(t)
	addDagger(t, root, clone)
	code, out, diag := invoke(t, root, "handin", project, "design.md")
	if code != 4 || out != "" || !strings.Contains(diag, "not_found: project "+project+" is not an active project") {
		t.Fatalf("unknown project hand-in: %d %s %s", code, out, diag)
	}
}

func TestHandInRefusesAMissingFile(t *testing.T) {
	root, clone := startEmpty(t)
	id := addDagger(t, root, clone).Project.ID
	code, out, diag := invoke(t, root, "handin", string(id), "missing.md")
	if code != 4 || out != "" || !strings.Contains(diag, "validation: cannot read ") || !strings.Contains(diag, "missing.md") {
		t.Fatalf("missing file hand-in: %d %s %s", code, out, diag)
	}
}

func TestProjectAddRefusesASecondActiveProject(t *testing.T) {
	root, clone := startEmpty(t)
	id := addDagger(t, root, clone).Project.ID
	code, out, diag := invoke(t, root, "project", "add", "other", "--upstream", "other/repo", "--fork", "owner/repo", "--clone", clone)
	if code != 5 || out != "" || !strings.Contains(diag, "project_active: project "+string(id)) {
		t.Fatalf("%d %s %s", code, out, diag)
	}
}

func TestProjectRemoveRetainsTheTrace(t *testing.T) {
	root, clone := startEmpty(t)
	added := addDagger(t, root, clone)
	id := added.Project.ID
	removed := successful(t, root, "project", "remove", string(id))
	if !strings.Contains(removed, "Project "+string(id)+" (dagger) removed") || !strings.Contains(removed, "Trace: "+added.Project.Trace) || !strings.Contains(removed, "retained") {
		t.Fatalf("remove output:\n%s", removed)
	}
	if _, err := os.Stat(trace.TraceDatabase(added.Project.Trace)); err != nil {
		t.Fatal("trace deleted", err)
	}
	if !strings.Contains(successful(t, root, "status"), "Project: none configured") {
		t.Fatal("project still shown")
	}
}

func TestProjectAddAfterRemoveMintsANewIdentity(t *testing.T) {
	root, clone := startEmpty(t)
	id := addDagger(t, root, clone).Project.ID
	successful(t, root, "project", "remove", string(id))
	readded := successful(t, root, "project", "add", "dagger", "--upstream", "dagger/dagger", "--fork", "owner/dagger", "--clone", clone)
	if !strings.Contains(readded, "added") || strings.Contains(readded, string(id)) {
		t.Fatalf("re-add reused the identity:\n%s", readded)
	}
}

func TestProjectUsage(t *testing.T) {
	root := fixture(t).Config.Root
	for _, args := range [][]string{
		{"project"}, {"project", "add"}, {"project", "add", "name"}, {"project", "add", "name", "--upstream", "a/b", "--fork", "c/d"},
		{"project", "add", "name", "--upstream", "a/b", "--fork", "c/d", "--clone", ""}, {"project", "remove"}, {"project", "remove", "not-an-id"},
		{"project", "remove", project, "--clone", "x"}, {"status", "--upstream", "a/b"}, {"project", "list"},
		{"handin"}, {"handin", "not-an-id"}, {"handin", project}, {"handin", project, "a.md", "b.md"}, {"handin", "not-an-id", "a.md"},
		{"handin", project, ""}, {"handin", project, "a.md", "--clone", "x"}, {"handin", project, "a.md", "--hard"},
		{"project", "rebase"}, {"project", "rebase", "not-an-id"}, {"project", "rebase", project, "--hard"},
	} {
		code, out, diag := invoke(t, root, args...)
		if code != 2 || out != "" || !strings.Contains(diag, "invalid arguments") {
			t.Fatalf("%v: %d %s %s", args, code, out, diag)
		}
	}
}
