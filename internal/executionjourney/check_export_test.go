package executionjourney

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/agentcore/jev/jevtest"
	"github.com/kpenfound/agentcore/jev/systemone"
	"github.com/kpenfound/osmia/internal/api"
	checkrun "github.com/kpenfound/osmia/internal/checks"
	"github.com/kpenfound/osmia/internal/config"
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
	traceURL string
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
		return strings.TrimSpace(fixturekit.DemoGit(t, filepath.Dir(identity.Repository), append([]string{"-C", identity.Repository}, args...)...))
	}
	raw := git("cat-file", "commit", identity.Candidate)
	git("tag", "existing-candidate", identity.Candidate)
	git("tag", "other-commit", identity.Candidate+"^")
	if c.mode == "fallback" {
		identity.Repository = filepath.Join(t.TempDir(), "unavailable-clone")
	}
	token := ""
	if c.mode != "anonymous" {
		token = "caller-cloud-output-secret"
	}
	result, err := (checkrun.DaggerChecks{CloudToken: token}).Check(ctx, dir, identity, links)
	if err != nil {
		return result, err
	}
	if result.ExitCode != 0 {
		return fail("fake dagger exited %d: %s", result.ExitCode, result.Output)
	}
	c.fallback = result.CIIdentityFallback
	c.traceURL = result.TraceURL
	expectedTrace := ""
	if token != "" {
		expectedTrace = "https://dagger.cloud/example/traces/caller123"
	}
	if c.traceURL != expectedTrace || (token != "" && strings.Contains(result.Output, token)) {
		return fail("trace capture or token redaction failed")
	}
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

func TestCheckExportsPreserveCallerCandidates(t *testing.T) {
	bin := t.TempDir()
	fixturekit.Must(t, os.WriteFile(filepath.Join(bin, "dagger"), []byte(strings.Replace(fixturekit.CheckExportDagger,
		`[ -z "${CI-}${DAGGER_CLOUD_TOKEN-}${GITHUB_TOKEN-}" ] || exit 90`,
		`[ -z "${GITHUB_TOKEN-}" ] || exit 90
if [ "$1" = list ]; then
 [ -z "${DAGGER_CLOUD_TOKEN-}${CI-}" ] || exit 93
elif [ -n "${DAGGER_CLOUD_TOKEN-}" ]; then
 [ -z "${CI-}" ] || exit 94
 printf '%s\n' "$DAGGER_CLOUD_TOKEN" >&2
 echo 'View trace at https://dagger.cloud/example/traces/caller123' >&2
fi`, 1)), 0700))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	enableListing := func(s *journey.Service) {
		useJevProvider(s, &jevtest.Provider{Results: []jevtest.Result{{Response: systemone.Response{Model: "jev-test", Answers: map[string]systemone.Answer{"check-0": jevtest.Noul(0.1)}}}}})
	}

	for _, backend := range []string{config.WorkspacesGit, config.WorkspacesJujutsu} {
		for _, mode := range []string{"identity", "anonymous", "fallback"} {
			t.Run(backend+"/"+mode+"/unit", func(t *testing.T) {
				f, _ := journey.NewMasonFixtureWith(t, backend, "masons = 1\n", intakefixture.IndependentPlan, nil)
				base := strings.TrimSpace(fixturekit.DemoGit(t, f.Clone, "-C", f.Clone, "rev-parse", "HEAD"))
				stream, repo := journey.SeedBuild(t, f, "identity-unit", intakefixture.IndependentPlan, backend, base)
				journey.SeedChecking(t, f, repo, stream, "resume", journey.ResumeReport)
				report := currentReport(t, repo, stream, "resume")

				g, err := journey.NewUnitWorkspaces(f.S.Current(), repo).Of(stream)
				fixturekit.Must(t, err)
				w, _, err := journey.NewUnitWorkspaces(f.S.Current(), repo).Open(context.Background(), stream, "resume")
				fixturekit.Must(t, err)
				fixturekit.Must(t, os.WriteFile(filepath.Join(w.Path, fixturekit.MasonWrote), []byte("pinned candidate tree\n"), 0600))
				candidate, err := g.Snapshot(context.Background(), w, report.Base)
				fixturekit.Must(t, err)

				docs := intakefixture.StreamDocuments(t, repo, stream, execution.ReportDocument("resume"))
				doc := docs[len(docs)-1]
				report.Candidate = candidate
				data, err := json.Marshal(report)
				fixturekit.Must(t, err)
				doc.Content = string(data)
				doc.Revision++
				fixturekit.Must(t, repo.RecordDocuments(context.Background(), []trace.Document{doc}))
				fixturekit.Must(t, os.WriteFile(filepath.Join(w.Path, fixturekit.MasonWrote), []byte("unchecked working files\n"), 0600))
				cfg := f.S.Current()
				checker := &candidateExportChecks{t: t, mode: mode, identity: checkrun.Identity{Candidate: candidate, Branch: execution.UnitBranch(stream, "resume"), Remote: "https://github.com/" + cfg.Project.Upstream, Repository: f.Clone}}
				f.S.Options.ReviewChecks = checker
				enableListing(f.S)
				journey.RunChecks(t, f.S, repo, stream)
				run := onlyRun(t, repo, stream)
				if checker.calls != 1 || checker.listed != 1 || run.Candidate != candidate || run.Status != checkrun.ChecksPassed || run.CIIdentityFallback != checker.fallback || run.TraceURL != checker.traceURL {
					t.Fatalf("persisted unit run %+v, runner %+v", run, checker)
				}
				assertCheckTraceRedacted(t, repo, stream)
			})
		}
	}
	for _, mode := range []string{"identity", "anonymous", "fallback"} {
		t.Run(mode+"/session", func(t *testing.T) {
			s, client, _, project, stream, _, clone := newSessionControlsFixture(t)
			worktree := sessionWorktreePath(s.Cfg.Root.String(), project, stream)
			fixturekit.Must(t, os.WriteFile(filepath.Join(worktree, "candidate.txt"), []byte("pinned candidate tree\n"), 0600))
			committed, err := client.SessionCommit(context.Background(), stream, "candidate title", "identity-commit")
			fixturekit.Must(t, err)
			fixturekit.Must(t, os.WriteFile(filepath.Join(worktree, "candidate.txt"), []byte("unchecked working files\n"), 0600))
			cfg := s.Current().For(project)
			checker := &candidateExportChecks{t: t, mode: mode, identity: checkrun.Identity{Candidate: committed.Commit, Branch: service.SessionBranch(stream), Remote: "https://github.com/" + cfg.Project.Upstream, Repository: clone}}
			service.InspectForTest(s.Core()).SetReviewChecks(checker)
			enableListing(s)
			run, err := client.SessionCheck(context.Background(), stream)
			fixturekit.Must(t, err)
			repo, err := s.Repository(project)
			fixturekit.Must(t, err)
			docs := intakefixture.StreamDocuments(t, repo, stream, fmt.Sprintf("session-check-%d", run.Run))
			var saved api.SessionCheckRun
			if len(docs) != 1 {
				t.Fatalf("session check records: %+v", docs)
			}
			fixturekit.Must(t, json.Unmarshal([]byte(docs[0].Content), &saved))
			if checker.calls != 1 || checker.listed != 1 || run.Commit != committed.Commit || run.Status != checkrun.ChecksPassed || run.CIIdentityFallback != checker.fallback || saved.CIIdentityFallback != checker.fallback || saved.TraceURL != checker.traceURL || run.TraceURL != checker.traceURL {
				t.Fatalf("persisted session run %+v, runner %+v", saved, checker)
			}
			assertCheckTraceRedacted(t, repo, stream)
		})
	}
}

func assertCheckTraceRedacted(t *testing.T, repo *trace.Repository, stream config.WorkstreamID) {
	t.Helper()
	docs, err := trace.Read[trace.Document](repo, stream)
	fixturekit.Must(t, err)
	for _, doc := range docs {
		if strings.Contains(doc.Content, "caller-cloud-output-secret") {
			t.Fatalf("Cloud token leaked into %s", doc.Path)
		}
	}
}
