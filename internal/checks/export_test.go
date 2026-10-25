package checks

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kpenfound/agentcore/vcs"
	"github.com/kpenfound/osmia/internal/ghlog"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/workspace"
)

// serial: installs a fake dagger executable through PATH.
func TestDaggerCandidateExport(t *testing.T) {
	bin := t.TempDir()
	must(t, os.WriteFile(filepath.Join(bin, "dagger"), []byte(fixturekit.CheckExportDagger), 0700))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	git := func(dir string, args ...string) string {
		return strings.TrimSpace(fixturekit.DemoGit(t, bin, append([]string{"-C", dir}, args...)...))
	}
	for _, backend := range []string{"git", "jujutsu"} {
		t.Run(backend, func(t *testing.T) {
			clone := t.TempDir()
			git(clone, "init", "--quiet")
			git(clone, "config", "user.name", "Original Author")
			git(clone, "config", "user.email", "author@example.invalid")
			must(t, os.WriteFile(filepath.Join(clone, "candidate.txt"), []byte("parent tree\n"), 0600))
			git(clone, "add", ".")
			git(clone, "commit", "--quiet", "-m", "parent")
			parent := git(clone, "rev-parse", "HEAD")
			var provider workspace.Provider = &workspace.Git{Clone: clone}
			candidate := ""
			if backend == "jujutsu" {
				if _, err := exec.LookPath("jj"); err != nil {
					if os.Getenv("OSMIA_REQUIRE_JJ") != "" {
						t.Fatal(err)
					}
					t.Skip("jj is unavailable")
				}
				j := &workspace.Jujutsu{Clone: clone, Directory: filepath.Join(t.TempDir(), "branches")}
				provider = j
				acquired, err := j.Acquire(context.Background(), vcs.Request{Name: "candidate", Ref: parent, Branch: "osmia-unit/test/unit"})
				must(t, err)
				w := acquired.(workspace.Worktree)
				must(t, os.WriteFile(filepath.Join(w.Path, "candidate.txt"), []byte("pinned candidate tree\n"), 0600))
				candidate, err = j.Snapshot(context.Background(), w, parent)
				must(t, err)
			} else {
				must(t, os.WriteFile(filepath.Join(clone, "candidate.txt"), []byte("pinned candidate tree\n"), 0600))
				git(clone, "add", ".")
				git(clone, "-c", "user.name=Original Committer", "-c", "user.email=committer@example.invalid", "commit", "--quiet", "--author=Original Author <author@example.invalid>", "-m", "candidate title\n\noriginal message")
				candidate = git(clone, "rev-parse", "HEAD")
			}
			raw := git(clone, "cat-file", "commit", candidate)
			if !strings.Contains(raw, "parent "+parent+"\n") {
				t.Fatalf("candidate has no original parent: %s", raw)
			}
			git(clone, "tag", "candidate-lightweight", candidate)
			git(clone, "tag", "-a", "candidate-annotated", "-m", "real release", candidate)
			git(clone, "tag", "other-commit", parent)
			identity := Identity{Candidate: candidate, Branch: "osmia-unit/test/unit", Remote: "https://github.com/upstream/repo", Repository: clone}
			for _, mode := range []string{"identity", "anonymous", "unresolved", "change-id", "tree-mismatch", "invalid-branch", "credentialed-remote"} {
				t.Run(mode, func(t *testing.T) {
					dir := t.TempDir()
					must(t, provider.Export(context.Background(), candidate, dir))
					id := identity
					switch mode {
					case "unresolved":
						id.Candidate = strings.Repeat("a", 40)
					case "change-id":
						id.Candidate = "zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"
					case "tree-mismatch":
						must(t, os.WriteFile(filepath.Join(dir, "candidate.txt"), []byte("different pinned export\n"), 0600))
					case "invalid-branch":
						id.Branch = "invalid..branch"
					case "credentialed-remote":
						id.Remote = "https://secret@github.com/upstream/repo"
					}
					before, err := os.ReadFile(filepath.Join(dir, "candidate.txt"))
					must(t, err)
					var logs bytes.Buffer
					ctx := ghlog.WithLogger(context.Background(), slog.New(slog.NewJSONHandler(&logs, nil)))
					d := DaggerChecks{CIIdentityExport: mode != "anonymous"}
					_, err = d.List(ctx, dir, id)
					must(t, err)
					var output bytes.Buffer
					exit, prep, err := d.dagger(ctx, dir, id, d.CIIdentityExport, &output, "check", "--progress=report", "--fail-fast")
					must(t, err)
					if exit != 0 {
						t.Fatalf("fake check exited %d: %s", exit, output.String())
					}
					if prep.CIIdentity != (mode == "identity") {
						t.Fatalf("preparation %+v", prep)
					}
					fallback := mode != "identity" && mode != "anonymous"
					if (prep.FallbackReason != "") != fallback || strings.Contains(logs.String(), `"level":"WARN"`) != fallback {
						t.Fatalf("preparation %+v, log %s", prep, logs.String())
					}
					if strings.Contains(logs.String(), "secret") {
						t.Fatalf("remote credentials in warning: %s", logs.String())
					}
					if mode == "identity" {
						for _, want := range []string{"EXPORT_HEAD\n" + candidate + "\n", "EXPORT_COMMIT\n" + raw + "\n", "EXPORT_BRANCH\n" + id.Branch + "\n", "EXPORT_ORIGIN\n" + id.Remote + "\n", "EXPORT_TAGS\ncandidate-annotated\ncandidate-lightweight\nEXPORT_STATUS\nEXPORT_FILES"} {
							if !strings.Contains(output.String(), want) {
								t.Fatalf("missing %q in %s", want, output.String())
							}
						}
						for _, tag := range []string{"candidate-lightweight", "candidate-annotated"} {
							if git(dir, "rev-parse", "refs/tags/"+tag) != git(clone, "rev-parse", "refs/tags/"+tag) {
								t.Fatal("tag object was recreated")
							}
						}
					} else if !strings.Contains(output.String(), "EXPORT_ANONYMOUS\n") {
						t.Fatalf("fallback retained identity: %s", output.String())
					}
					if !strings.Contains(output.String(), string(before)) {
						t.Fatalf("fake check did not read the pinned files: %s", output.String())
					}
					after, err := os.ReadFile(filepath.Join(dir, "candidate.txt"))
					must(t, err)
					if !bytes.Equal(before, after) {
						t.Fatal("preparation changed the exported files")
					}
					// The public result must carry the preparation fallback too.
					result, err := d.Check(ctx, dir, id, nil)
					must(t, err)
					if result.ExitCode != 0 || (result.CIIdentityFallback != "") != fallback {
						t.Fatalf("result %+v", result)
					}
					if mode == "identity" {
						missing := id
						missing.Repository = filepath.Join(t.TempDir(), "missing")
						result, err = d.Check(ctx, dir, missing, nil)
						must(t, err)
						if result.ExitCode != 0 || result.CIIdentityFallback == "" || !strings.Contains(result.Output, "EXPORT_ANONYMOUS\n") || !strings.Contains(result.Output, string(before)) {
							t.Fatalf("fallback retained an earlier identity or changed files: %+v", result)
						}
					}
				})
			}
			if got := git(clone, "tag", "--list"); got != "candidate-annotated\ncandidate-lightweight\nother-commit" {
				t.Fatalf("source tags changed: %s", got)
			}
		})
	}
}
