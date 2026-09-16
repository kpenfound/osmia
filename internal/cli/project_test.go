package cli

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/service"
)

// emptyFixture writes a root without a project and a local Git clone.
func emptyFixture(t *testing.T) (service.Options, string) {
	t.Helper()
	home, err := os.MkdirTemp("", "ocp-")
	must(t, err)
	t.Cleanup(func() { os.RemoveAll(home) })
	root := filepath.Join(home, ".osmia")
	must(t, os.MkdirAll(root, 0700))
	must(t, os.WriteFile(filepath.Join(root, "config.toml"), []byte(`# owner comment
version=1
active_projects=[]
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

func TestProjectCommands(t *testing.T) {
	opts, clone := emptyFixture(t)
	s, err := service.Start(context.Background(), opts)
	must(t, err)
	t.Cleanup(func() { s.Close() })
	root := opts.Config.Root
	status := successful(t, root, "status")
	if !strings.Contains(status, "Project: none configured") || !strings.Contains(status, "no_project") {
		t.Fatalf("status without a project:\n%s", status)
	}
	var st struct {
		Configuration service.ConfigResponse
	}
	must(t, json.Unmarshal([]byte(successful(t, root, "status", "--json")), &st))
	if st.Configuration.Project != nil {
		t.Fatal(st.Configuration)
	}
	for _, args := range [][]string{{"priority", "set", stream}, {"priority", "clear"}, {"pause", stream}} {
		code, out, diag := invoke(t, root, args...)
		if code != 4 || out != "" || !strings.Contains(diag, "osmia project add") {
			t.Fatalf("%v: %d %s %s", args, code, out, diag)
		}
	}
	code, out, diag := invoke(t, root, "project", "add", "dagger", "--upstream", "dagger/dagger", "--fork", "owner/dagger", "--clone", filepath.Join(clone, "missing"))
	if code != 4 || out != "" || !strings.Contains(diag, "validation: clone does not exist") {
		t.Fatalf("%d %s %s", code, out, diag)
	}
	code, out, diag = invoke(t, root, "project", "remove", project)
	if code != 4 || out != "" || !strings.Contains(diag, "no_project") {
		t.Fatalf("%d %s %s", code, out, diag)
	}
	// A relative clone resolves against the client's working directory.
	wd, err := os.Getwd()
	must(t, err)
	relative, err := filepath.Rel(wd, clone)
	must(t, err)
	var added service.ProjectResponse
	must(t, json.Unmarshal([]byte(successful(t, root, "project", "add", "dagger", "--upstream", "dagger/dagger", "--fork", "owner/dagger", "--clone", relative, "--base-branch", "develop", "--json")), &added))
	id := added.Project.ID
	if err := config.CheckProjectIDs(id); err != nil || added.Project.BaseBranch != "develop" || !strings.HasSuffix(added.Project.Clone, "clone") || !strings.Contains(added.NextStep, "charter.md") {
		t.Fatalf("%+v %v", added, err)
	}
	text, err := os.ReadFile(filepath.Join(root, "config.toml"))
	must(t, err)
	if !strings.HasPrefix(string(text), "# owner comment\n") || !strings.Contains(string(text), `active_projects=["`+string(id)+`"]`) {
		t.Fatalf("configuration:\n%s", text)
	}
	status = successful(t, root, "status")
	if !strings.Contains(status, "Project: "+string(id)+" (dagger)") || !strings.Contains(status, "Trace: "+added.Project.Trace) || strings.Contains(status, "Diagnostic") {
		t.Fatalf("status with a project:\n%s", status)
	}
	if !strings.Contains(status, "Charter: empty; write numbered rules before handing in work (0 rules, revision 1) "+added.Project.Charter) {
		t.Fatalf("status without charter rules:\n%s", status)
	}
	// Registration started the librarian's extraction; this service has no
	// runner, so it fails and status says why. A re-run is a new extraction.
	extraction := awaitExtraction(t, root)
	if extraction.Extraction != 1 || extraction.State != "failed" || !strings.Contains(extraction.Reason, "no agent runner") {
		t.Fatalf("extraction after add: %+v", extraction)
	}
	status = successful(t, root, "status")
	if !strings.Contains(status, "Knowledge base: extraction 1 failed at "+extraction.At.UTC().Format("2006-01-02T15:04:05Z")+": "+extraction.Reason) {
		t.Fatalf("status with a failed extraction:\n%s", status)
	}
	extracted := successful(t, root, "project", "extract", string(id))
	if !strings.Contains(extracted, "Extraction 2 of project "+string(id)+" (dagger) started") || !strings.Contains(extracted, "osmia status") {
		t.Fatalf("extract output:\n%s", extracted)
	}
	var er service.ExtractionResponse
	if extraction = awaitExtraction(t, root); extraction.Extraction != 2 || extraction.State != "failed" {
		t.Fatalf("second extraction: %+v", extraction)
	}
	must(t, json.Unmarshal([]byte(successful(t, root, "project", "extract", string(id), "--json")), &er))
	if er.Extraction.Extraction != 3 || er.Extraction.State != "pending" || er.Project.ID != id {
		t.Fatalf("extract JSON: %+v", er)
	}
	awaitExtraction(t, root)
	code, out, diag = invoke(t, root, "project", "extract", project)
	if code != 4 || out != "" || !strings.Contains(diag, "not_found: project "+project+" is not an active project") {
		t.Fatalf("extract unknown project: %d %s %s", code, out, diag)
	}
	code, out, diag = invoke(t, root, "handin", string(id), "design.md")
	if code != 4 || out != "" || diag != "charter_empty: project "+string(id)+" cannot take work: its charter has no rules; write numbered rules (\"1. ...\") in "+added.Project.Charter+"\n" {
		t.Fatalf("empty charter hand-in: %d %s %s", code, out, diag)
	}
	code, out, diag = invoke(t, root, "handin", project)
	if code != 4 || out != "" || !strings.Contains(diag, "not_found: project "+project+" is not an active project") {
		t.Fatalf("unknown project hand-in: %d %s %s", code, out, diag)
	}
	must(t, os.WriteFile(added.Project.Charter, []byte("# Charter\n1. Keep changes small.\n1. Test every fix.\n"), 0600))
	status = successful(t, root, "status")
	if !strings.Contains(status, "Charter: ready (2 rules, revision 2) "+added.Project.Charter+"\n  charter.md:3: rule 1 is numbered 2 times") {
		t.Fatalf("status with charter rules:\n%s", status)
	}
	var st2 struct {
		Configuration service.ConfigResponse
	}
	must(t, json.Unmarshal([]byte(successful(t, root, "status", "--json")), &st2))
	if cs := st2.Configuration.Project.CharterState; cs == nil || !cs.Ready || cs.Rules != 2 || cs.Revision != 2 || len(cs.Diagnostics) != 1 {
		t.Fatalf("status JSON: %+v", cs)
	}
	code, out, diag = invoke(t, root, "handin", string(id), "--json")
	if code != 5 || out != "" || !strings.Contains(diag, "unsupported: project "+string(id)+" has a charter with 2 rules, but hand-in is not implemented yet") {
		t.Fatalf("ready charter hand-in: %d %s %s", code, out, diag)
	}
	if !strings.Contains(successful(t, root, "priority", "clear"), "applied=true") {
		t.Fatal("priority without restart")
	}
	code, out, diag = invoke(t, root, "project", "add", "other", "--upstream", "other/repo", "--fork", "owner/repo", "--clone", clone)
	if code != 5 || out != "" || !strings.Contains(diag, "project_active: project "+string(id)) {
		t.Fatalf("%d %s %s", code, out, diag)
	}
	removed := successful(t, root, "project", "remove", string(id))
	if !strings.Contains(removed, "Project "+string(id)+" (dagger) removed") || !strings.Contains(removed, "Trace: "+added.Project.Trace) || !strings.Contains(removed, "retained") {
		t.Fatalf("remove output:\n%s", removed)
	}
	if _, err := os.Stat(added.Project.Charter); err != nil {
		t.Fatal("trace deleted", err)
	}
	if !strings.Contains(successful(t, root, "status"), "Project: none configured") {
		t.Fatal("project still shown")
	}
	readded := successful(t, root, "project", "add", "dagger", "--upstream", "dagger/dagger", "--fork", "owner/dagger", "--clone", clone)
	if !strings.Contains(readded, "added") || strings.Contains(readded, string(id)) {
		t.Fatalf("re-add reused the identity:\n%s", readded)
	}
}

// awaitExtraction polls status until the latest extraction is terminal.
func awaitExtraction(t *testing.T, root string) service.ExtractionState {
	t.Helper()
	deadline := time.Now().Add(5 * time.Minute)
	for {
		var st struct {
			Configuration service.ConfigResponse
		}
		must(t, json.Unmarshal([]byte(successful(t, root, "status", "--json")), &st))
		if p := st.Configuration.Project; p != nil && p.Extraction != nil && (p.Extraction.State == "succeeded" || p.Extraction.State == "failed") {
			return *p.Extraction
		}
		if time.Now().After(deadline) {
			t.Fatal("extraction did not finish")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestProjectUsage(t *testing.T) {
	root := fixture(t).Config.Root
	for _, args := range [][]string{
		{"project"}, {"project", "add"}, {"project", "add", "name"}, {"project", "add", "name", "--upstream", "a/b", "--fork", "c/d"},
		{"project", "add", "name", "--upstream", "a/b", "--fork", "c/d", "--clone", ""}, {"project", "remove"}, {"project", "remove", "not-an-id"},
		{"project", "remove", project, "--clone", "x"}, {"status", "--upstream", "a/b"}, {"project", "list"},
		{"handin"}, {"handin", "not-an-id"}, {"handin", project, "--clone", "x"},
		{"project", "extract"}, {"project", "extract", "not-an-id"}, {"project", "extract", project, "--hard"},
	} {
		code, out, diag := invoke(t, root, args...)
		if code != 2 || out != "" || !strings.Contains(diag, "invalid arguments") {
			t.Fatalf("%v: %d %s %s", args, code, out, diag)
		}
	}
}
