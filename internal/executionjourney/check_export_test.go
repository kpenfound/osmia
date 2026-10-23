package executionjourney

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kpenfound/agentcore/jev/jevtest"
	"github.com/kpenfound/agentcore/jev/systemone"
	"github.com/kpenfound/osmia/internal/api"
	checkrun "github.com/kpenfound/osmia/internal/checks"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

type candidateExportChecks struct {
	t        *testing.T
	identity checkrun.Identity
	mode     string
	calls    int
	listed   int
	fallback string
}

func (c *candidateExportChecks) List(ctx context.Context, dir string, identity checkrun.Identity) ([]string, error) {
	if identity != c.identity {
		return nil, fmt.Errorf("listing identity %+v, want %+v", identity, c.identity)
	}
	c.listed++
	return (checkrun.DaggerChecks{}).List(ctx, dir, identity)
}

func (c *candidateExportChecks) Check(ctx context.Context, dir string, identity checkrun.Identity, links []string) (api.CheckResult, error) {
	t := c.t
	fail := func(format string, args ...any) (api.CheckResult, error) {
		return api.CheckResult{ExitCode: -1}, fmt.Errorf(format, args...)
	}
	if identity != c.identity {
		return fail("check identity %+v, want %+v", identity, c.identity)
	}
	c.calls++
	git := func(args ...string) string {
		return strings.TrimSpace(demoGit(t, filepath.Dir(identity.Repository), append([]string{"-C", identity.Repository}, args...)...))
	}
	raw := git("cat-file", "commit", identity.Candidate)
	git("tag", "existing-candidate", identity.Candidate)
	git("tag", "other-commit", identity.Candidate+"^")
	if c.mode == "fallback" {
		identity.Repository = filepath.Join(t.TempDir(), "unavailable-clone")
	}
	result, err := (checkrun.DaggerChecks{CIIdentityExport: c.mode != "anonymous"}).Check(ctx, dir, identity, links)
	if err != nil {
		return result, err
	}
	if result.ExitCode != 0 {
		return fail("fake dagger exited %d: %s", result.ExitCode, result.Output)
	}
	c.fallback = result.CIIdentityFallback
	if (c.fallback != "") != (c.mode == "fallback") {
		return fail("fallback %q in mode %s", c.fallback, c.mode)
	}
	if c.mode == "identity" {
		for _, want := range []string{"EXPORT_HEAD\n" + identity.Candidate + "\n", "EXPORT_COMMIT\n" + raw + "\n", "EXPORT_BRANCH\n" + identity.Branch + "\n", "EXPORT_ORIGIN\n" + identity.Remote + "\n", "EXPORT_TAGS\nexisting-candidate\nEXPORT_STATUS\nEXPORT_FILES"} {
			if !strings.Contains(result.Output, want) {
				return fail("missing %q in check output:\n%s", want, result.Output)
			}
		}
	} else if !strings.Contains(result.Output, "EXPORT_ANONYMOUS\n") {
		return fail("expected anonymous export: %s", result.Output)
	}
	if !strings.Contains(result.Output, "pinned candidate tree\n") || strings.Contains(result.Output, "unchecked working files") || strings.Contains(result.Output, "newer feature tree") {
		return fail("checks read a different tree: %s", result.Output)
	}
	return result, err
}

// serial: installs a fake dagger executable through PATH. Both workspace
// providers produce Git commit identities; sessions use their Git branch head.
func TestCheckExportsPreserveCallerCandidates(t *testing.T) {
	bin := t.TempDir()
	must(t, os.WriteFile(filepath.Join(bin, "dagger"), []byte(fixturekit.CheckExportDagger), 0700))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	enableListing := func(s *Service) {
		boostJev(s, &jevtest.Provider{Results: []jevtest.Result{{Response: systemone.Response{Model: "jev-test", Answers: map[string]systemone.Answer{"check-0": jevtest.Noul(0.1)}}}}})
	}

	for _, backend := range []string{config.WorkspacesGit, config.WorkspacesJujutsu} {
		for _, mode := range []string{"identity", "anonymous", "fallback"} {
			t.Run(backend+"/"+mode+"/unit", func(t *testing.T) {
				f, _ := newMasonFixtureWith(t, backend, "masons = 1\n", independentPlan, "", nil)
				base := strings.TrimSpace(demoGit(t, f.clone, "-C", f.clone, "rev-parse", "HEAD"))
				stream, repo := seedBuild(t, f, "identity-unit", independentPlan, backend, base)
				seedChecking(t, f, repo, stream, "resume", resumeReport)
				report := currentReport(t, repo, stream, "resume")
				// Snapshot the fixture's pinned candidate file through the actual provider.
				g, err := newUnitWorkspaces(f.s.current(), repo).Of(stream)
				must(t, err)
				w, _, err := newUnitWorkspaces(f.s.current(), repo).open(context.Background(), stream, "resume")
				must(t, err)
				must(t, os.WriteFile(filepath.Join(w.Path, masonWrote), []byte("pinned candidate tree\n"), 0600))
				candidate, err := g.Snapshot(context.Background(), w, report.Base)
				must(t, err)
				// Bind the existing check fixture to this new candidate before requesting it.
				docs := streamDocuments(t, repo, stream, reportDocument("resume"))
				doc := docs[len(docs)-1]
				report.Candidate = candidate
				data, err := json.Marshal(report)
				must(t, err)
				doc.Content = string(data)
				doc.Revision++
				must(t, repo.RecordDocuments(context.Background(), []trace.Document{doc}))
				must(t, os.WriteFile(filepath.Join(w.Path, masonWrote), []byte("unchecked working files\n"), 0600))
				cfg := f.s.current()
				checks := &candidateExportChecks{t: t, mode: mode, identity: checkrun.Identity{Candidate: candidate, Branch: unitBranch(stream, "resume"), Remote: "https://github.com/" + cfg.Project.Upstream, Repository: f.clone}}
				f.s.options.reviewChecks = checks
				enableListing(f.s)
				runChecks(t, f.s, repo, stream)
				run := onlyRun(t, repo, stream)
				if checks.calls != 1 || checks.listed != 1 || run.Candidate != candidate || run.Status != ChecksPassed || run.CIIdentityFallback != checks.fallback {
					t.Fatalf("persisted unit run %+v, runner %+v", run, checks)
				}
			})
			t.Run(backend+"/"+mode+"/final", func(t *testing.T) {
				f, _ := newMasonFixtureWith(t, backend, "masons = 1\n", validPlan, "", nil)
				base := strings.TrimSpace(demoGit(t, f.clone, "-C", f.clone, "rev-parse", "HEAD"))
				stream, repo := seedBuild(t, f, "identity-final", validPlan, backend, base)
				candidate := moveFeature(t, f, stream, map[string]string{"candidate.txt": "pinned candidate tree\n"})
				moveFeature(t, f, stream, map[string]string{"candidate.txt": "newer feature tree\n"})
				cfg := f.s.current()
				checks := &candidateExportChecks{t: t, mode: mode, identity: checkrun.Identity{Candidate: candidate, Branch: featureBranch(stream), Remote: "https://github.com/" + cfg.Project.Upstream, Repository: f.clone}}
				f.s.options.reviewChecks = checks
				enableListing(f.s)
				a := &finalReviewer{s: f.s, repository: repo}
				run, err := a.finalChecks(context.Background(), cfg, stream, FinalReport{Review: 1, Operation: "check-export", Commit: candidate, Upstream: &seal.Base{Commit: base}})
				must(t, err)
				var saved FinalCheckRun
				docs := streamDocuments(t, repo, stream, finalChecksDocument(1))
				if len(docs) != 1 {
					t.Fatalf("final check records: %+v", docs)
				}
				must(t, json.Unmarshal([]byte(docs[0].Content), &saved))
				if checks.calls != 1 || checks.listed != 1 || saved.Commit != candidate || saved.Status != ChecksPassed || run.CIIdentityFallback != checks.fallback || saved.CIIdentityFallback != checks.fallback {
					t.Fatalf("persisted final run %+v, runner %+v", saved, checks)
				}
			})
		}
	}
	for _, mode := range []string{"identity", "anonymous", "fallback"} {
		t.Run(mode+"/session", func(t *testing.T) {
			s, client, _, project, stream, _, clone := newSessionControlsFixture(t)
			worktree := sessionWorktreePath(s.cfg.Root.String(), project, stream)
			must(t, os.WriteFile(filepath.Join(worktree, "candidate.txt"), []byte("pinned candidate tree\n"), 0600))
			committed, err := client.SessionCommit(context.Background(), stream, "candidate title", "identity-commit")
			must(t, err)
			must(t, os.WriteFile(filepath.Join(worktree, "candidate.txt"), []byte("unchecked working files\n"), 0600))
			cfg := s.current().For(project)
			checks := &candidateExportChecks{t: t, mode: mode, identity: checkrun.Identity{Candidate: committed.Commit, Branch: sessionBranch(stream), Remote: "https://github.com/" + cfg.Project.Upstream, Repository: clone}}
			service.InspectForTest(s.core()).SetReviewChecks(checks)
			enableListing(s)
			run, err := client.SessionCheck(context.Background(), stream)
			must(t, err)
			repo, err := s.repository(project)
			must(t, err)
			docs := streamDocuments(t, repo, stream, fmt.Sprintf("session-check-%d", run.Run))
			var saved SessionCheckRun
			if len(docs) != 1 {
				t.Fatalf("session check records: %+v", docs)
			}
			must(t, json.Unmarshal([]byte(docs[0].Content), &saved))
			if checks.calls != 1 || checks.listed != 1 || run.Commit != committed.Commit || run.Status != ChecksPassed || run.CIIdentityFallback != checks.fallback || saved.CIIdentityFallback != checks.fallback {
				t.Fatalf("persisted session run %+v, runner %+v", saved, checks)
			}
		})
	}
}
