package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	wire "github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/bundle"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/kb"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/trace"
)

// The project onboarding demonstration: a project is added from a local fixture
// clone, registration records the entity map seeded from its tracked files as
// the only knowledge, the owner writes the charter, and the project's local
// context resolves without any network.

const onboardingCharter = `# Charter

1. Keep the trace append-only.
2. Every change ships with a test.
`

// onboardingGit runs git with no user or system configuration.
func onboardingGit(t *testing.T, home string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// treeHash hashes every path, mode and file content below dir, VCS metadata
// included.
func treeHash(t *testing.T, dir string) string {
	t.Helper()
	h := sha256.New()
	must(t, filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		fmt.Fprintf(h, "%s %v\n", rel, info.Mode())
		if entry.Type().IsRegular() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			fmt.Fprintf(h, "%x\n", sha256.Sum256(data))
		}
		return nil
	}))
	return hex.EncodeToString(h.Sum(nil))
}

func onboardingStatus(t *testing.T, root string) wire.ConfigResponse {
	t.Helper()
	var st struct {
		Configuration wire.ConfigResponse
	}
	must(t, json.Unmarshal([]byte(successful(t, root, "status", "--json")), &st))
	return st.Configuration
}

func TestProjectOnboarding(t *testing.T) {
	for _, name := range []string{"GITHUB_TOKEN", "GH_TOKEN"} {
		t.Setenv(name, "ghp_onboarding_credential_must_not_be_used")
	}
	ctx := context.Background()

	// 1. A fresh root with no project, and a fixture clone with CODEOWNERS
	// and a few subsystems.
	opts, clone := emptyFixture(t)
	home := filepath.Dir(clone)
	for path, content := range map[string]string{
		"CODEOWNERS":                "* @core\n/cmd/ @tools\n/internal/trace/ @core @storage\n",
		"README.md":                 "# demo\n",
		"cmd/demo/main.go":          "package main\n",
		"internal/service/serve.go": "package service\n",
		"internal/trace/git.go":     "package trace\n",
	} {
		must(t, os.MkdirAll(filepath.Join(clone, filepath.Dir(path)), 0700))
		must(t, os.WriteFile(filepath.Join(clone, path), []byte(content), 0600))
	}
	onboardingGit(t, home, "-C", clone, "add", ".")
	onboardingGit(t, home, "-C", clone, "-c", "user.name=Owner", "-c", "user.email=owner@example.invalid", "commit", "-qm", "base")
	cloneBefore := treeHash(t, clone)
	opts.ShutdownTimeout = 100 * time.Millisecond
	root := opts.Config.Root
	s, err := service.Start(ctx, opts)
	must(t, err)
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			s.Close()
		}
	})
	if cfg := onboardingStatus(t, root); cfg.Project != nil {
		t.Fatalf("fresh root has a project: %+v", cfg.Project)
	}

	// 2. project add registers the project; the trace lives under the root.
	var added wire.ProjectResponse
	must(t, json.Unmarshal([]byte(successful(t, root, "project", "add", "demo", "--upstream", "owner/demo", "--fork", "fork/demo", "--clone", clone, "--json")), &added))
	id := added.Project.ID
	traceDir := added.Project.Trace
	resolvedRoot, err := config.ResolveRoot(root, "")
	must(t, err)
	if traceDir != filepath.Join(resolvedRoot.String(), "projects", string(id)) || strings.HasPrefix(traceDir, added.Project.Clone) {
		t.Fatalf("trace %s, clone %s", traceDir, added.Project.Clone)
	}
	db := trace.TraceDatabase(traceDir)

	// 3. Registration recorded the seeded entity map as the owner's first
	// revision and nothing else: no knowledge prose and no further map.
	seeded, err := kb.Seed(clone)
	must(t, err)
	want, err := kb.Encode(seeded)
	must(t, err)
	docs := traceDocuments(t, db, "documents.jsonl")
	if entities := docs["kb-entities"]; len(entities) != 1 || entities[0].Revision != 1 || entities[0].Content != string(want) || entities[0].Actor != (trace.Actor{Kind: "owner", ID: "local"}) {
		t.Fatalf("entity map revisions: %+v", entities)
	}
	for doc := range docs {
		if doc != "charter" && doc != "kb-entities" {
			t.Fatalf("registration recorded %s", doc)
		}
	}
	if status := successful(t, root, "status"); strings.Contains(status, "Knowledge base") {
		t.Fatalf("status reports a knowledge base:\n%s", status)
	}
	if treeHash(t, clone) != cloneBefore {
		t.Fatal("onboarding changed the clone")
	}

	// 4. The owner writes rules with osmia edit; status shows them and the
	// context is file-based.
	setCharter(t, root, id, onboardingCharter)
	status := successful(t, root, "status")
	for _, want := range []string{
		"Charter: 2 rules, revision 2; edit it with osmia edit charter " + string(id) + "\n",
		"Context: " + string(id) + " context_mode=file",
	} {
		if !strings.Contains(status, want) {
			t.Fatalf("status lacks %q:\n%s", want, status)
		}
	}
	charters := traceDocuments(t, db, "documents.jsonl")["charter"]
	if edit := charters[len(charters)-1]; len(charters) != 2 || edit.Revision != 2 || edit.Actor != (trace.Actor{Kind: "owner", ID: "local"}) || edit.Cause != "owner-edit" || edit.Content != onboardingCharter {
		t.Fatalf("charter revisions: %+v", charters)
	}
	if commits := commitCount(t, db); commits < 2 {
		t.Fatalf("trace history: %d commits", commits)
	}

	// 5. Resolution reads the local entity map alone.
	local, err := kb.Parse([]byte(latestDocument(t, db, "documents.jsonl", trace.EntitiesPath).Content))
	must(t, err)
	fp := local.ResolveEntities([]string{"trace", "internal"})
	entities := slices.Sorted(slices.Values(fp.Entities))
	if !slices.Equal(entities, []string{"internal", "internal.service", "internal.trace"}) || !slices.Equal(slices.Sorted(slices.Values(fp.Paths)), []string{"internal", "internal/service", "internal/trace"}) || len(fp.Unresolved) != 0 {
		t.Fatalf("entities to paths: %+v", fp)
	}
	loc := local.ResolvePaths([]string{"internal/trace/git.go", "cmd/demo/main.go", "docs/unknown.md"})
	if want := []kb.PathMatch{{Path: "internal/trace/git.go", Entities: []string{"internal", "internal.trace"}}, {Path: "cmd/demo/main.go", Entities: []string{"cmd", "cmd.demo"}}}; len(loc.Matches) != len(want) || !slices.Equal(loc.Unresolved, []string{"docs/unknown.md"}) {
		t.Fatalf("paths to entities: %+v", loc)
	}
	// The service's file provider assembles a footprint's bundle.
	provider := s.Context()
	if provider.Mode(id) != bundle.ModeFile {
		t.Fatalf("mode %s", provider.Mode(id))
	}
	b, err := provider.Assemble(ctx, id, bundle.Scope{Entities: []string{"trace"}})
	must(t, err)
	if b.Charter.Revision != 2 || len(b.Charter.Rules) != 2 || len(b.Decisions) != 0 || b.Entities.Revision != 1 {
		t.Fatalf("bundle: %+v", b)
	}
	if len(b.Entities.Entities) != 1 || b.Entities.Entities[0].ID != "internal.trace" {
		t.Fatalf("bundle entities: %+v", b.Entities)
	}
	rendered := b.Render()
	for _, want := range []string{"context mode: file", "- charter#1 [Charter]: Keep the trace append-only.", "- charter#2 [Charter]: Every change ships with a test.", "No rulings are recorded."} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("rendered bundle lacks %q:\n%s", want, rendered)
		}
	}

	// 6. Removing the project keeps the trace and the clone.
	traceBefore := commitCount(t, db)
	removed := successful(t, root, "project", "remove", string(id))
	if !strings.Contains(removed, string(id)) {
		t.Fatalf("remove output:\n%s", removed)
	}
	if cfg := onboardingStatus(t, root); cfg.Project != nil {
		t.Fatalf("project still active: %+v", cfg.Project)
	}
	if commits := commitCount(t, db); commits != traceBefore {
		t.Fatalf("trace moved on removal: %d commits, %d before", commits, traceBefore)
	}
	if got := latestDocument(t, db, "documents.jsonl", "charter.md").Content; got != onboardingCharter {
		t.Fatalf("charter after removal: %q", got)
	}
	if treeHash(t, clone) != cloneBefore {
		t.Fatal("removal changed the clone")
	}
	must(t, s.Close())
	stopped = true
}
