package service

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/kpenfound/osmia/internal/verification"
	"github.com/kpenfound/osmia/internal/verifytool"
)

func installListingLever(t *testing.T, report, listed string) {
	t.Helper()
	bin, out := t.TempDir(), t.TempDir()
	fixturekit.Must(t, os.WriteFile(filepath.Join(out, "report.json"), []byte(report), 0o600))
	script := fmt.Sprintf(`#!/bin/sh
case "$*" in
  *" list") printf '%%s' %q; exit 0;;
esac
for last; do :; done
cp -R %q/. "$last"
`, listed, out)
	fixturekit.Must(t, os.WriteFile(filepath.Join(bin, "dagger"), []byte(script), 0o700))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func enableVerification(t *testing.T, f *shedFixture, repo *trace.Repository) *config.Config {
	t.Helper()
	t.Setenv("OSMIA_VERIFY_TOKEN", "synthetic-verify-cloud-token")
	cfg := f.s.about(repo)
	cfg.Dagger = config.DaggerConfig{Engine: "tcp://verify-engine:1234", CloudComputeTokenEnv: "OSMIA_VERIFY_TOKEN"}
	cfg.Project.Verification = verification.Setting{Call: []string{"lever"}, MapDir: verification.DefaultMapDir}
	return cfg
}

const (
	sweepIndex  = "# Verification feature map\n\nFeatures, in the order of a full regression sweep:\n\n1. [notes](notes.md): adding notes.\n2. [home](home.md): the landing page.\n"
	sweepHome   = "---\nfeature: home\npaths:\n  - app/server.js\nflows:\n  - home-page\n---\n\n# Home\n"
	sweepNotes  = "---\nfeature: notes\npaths: [app/notes/**]\nflows: [add-note, home-page]\n---\n\n# Notes\n"
	sweepBroken = "no header here\n"
)

func latestMeasure(t *testing.T, repo *trace.Repository, stream config.WorkstreamID, review int) (verification.FinalMeasure, bool) {
	t.Helper()
	docs, err := trace.Read[trace.Document](repo, stream)
	fixturekit.Must(t, err)
	var latest trace.Document
	for _, d := range docs {
		if d.ID == delivery.FinalMeasureDocument(review) && d.Revision >= latest.Revision {
			latest = d
		}
	}
	if latest.Revision == 0 {
		return verification.FinalMeasure{}, false
	}
	if latest.Path != verification.FinalMeasurePath(review) {
		t.Fatalf("the measure is at %s", latest.Path)
	}
	var m verification.FinalMeasure
	fixturekit.Must(t, json.Unmarshal([]byte(latest.Content), &m))
	return m, true
}

func sweepPrompt(t *testing.T, f *shedFixture, repo *trace.Repository, stream config.WorkstreamID, report FinalReport) string {
	t.Helper()
	ctx := context.Background()
	fixturekit.Must(t, f.s.delivery().EnsureCommittee(ctx, repo, stream, 1))
	finals := &delivery.FinalReviewer{S: f.s.delivery(), Repository: repo}
	report.Review = 3
	in := delivery.FinalReviewInput{Review: 3, Commit: report.Commit}
	fixturekit.Must(t, finals.Enqueue(ctx, f.s.about(repo), stream, in, report, delivery.FinalCheckRun{}, committeeAgent(1), 1))
	th, err := repo.Thread(stream, committeeAgent(1))
	fixturekit.Must(t, err)
	for _, q := range th.Turns {
		if q.Request.TurnID == delivery.FinalTurnID(3, committeeAgent(1), 1) {
			return q.Request.Prompt
		}
	}
	t.Fatal("the final review turn was not queued")
	return ""
}

func TestTheFinalReviewersPromptListsTheUnionOfTheMapsFlowsOnceEachInReadmeOrderAndTellsItToRunThemWithVerify(t *testing.T) {
	f, stream, repo, report := deliveryFixture(t)
	enableVerification(t, f, repo)
	installListingLever(t, verifyReport, `["home-page"]`)
	report.Commit = moveFeature(t, f, stream, map[string]string{
		"docs/verification/README.md": sweepIndex, "docs/verification/home.md": sweepHome,
		"docs/verification/notes.md": sweepNotes, "docs/verification/broken.md": sweepBroken,
	})

	prompt := sweepPrompt(t, f, repo, stream, report)
	for _, want := range []string{
		"## Verification: regression sweep", "run each one with verify on the feature-branch head", report.Commit,
		"1. add-note (notes)", "2. home-page (notes, home)",
		"docs/verification/broken.md: the header does not parse",
		`docs/verification/notes.md: flow "add-note" is not in the lever's list`,
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the prompt lacks %q:\n%s", want, prompt)
		}
	}
	if strings.Count(prompt, "home-page (") != 1 || strings.Contains(prompt, "3. ") && strings.Contains(prompt, "3. home-page") {
		t.Errorf("a flow is listed more than once:\n%s", prompt)
	}
}

func TestTheFinalReviewersPromptOfAProjectWithoutTheSettingHasNoSweep(t *testing.T) {
	f, stream, repo, report := deliveryFixture(t)
	report.Commit = moveFeature(t, f, stream, map[string]string{"docs/verification/home.md": sweepHome})
	prompt := sweepPrompt(t, f, repo, stream, report)
	if strings.Contains(prompt, "regression sweep") || strings.Contains(prompt, "Verification:") {
		t.Fatalf("the prompt carries verification content:\n%s", prompt)
	}
}

func TestASweepWhoseLeverListCannotBeReadIsStillGivenWithANote(t *testing.T) {
	f, stream, repo, report := deliveryFixture(t)
	enableVerification(t, f, repo)
	installListingLever(t, verifyReport, `not json`)
	report.Commit = moveFeature(t, f, stream, map[string]string{"docs/verification/home.md": sweepHome})
	prompt := sweepPrompt(t, f, repo, stream, report)
	if !strings.Contains(prompt, "1. home-page (home)") || !strings.Contains(prompt, "the lever's list could not be read") {
		t.Fatalf("%s", prompt)
	}
}

func claimsReport(claims ...string) string {
	return `{"version":1,"claims":[` + strings.Join(claims, ",") + `]}`
}

func flowClaim(flow, status string) string {
	tag := ""
	if flow != "" {
		tag = `,"flow":"` + flow + `"`
	}
	return fmt.Sprintf(`{"name":"claim of %s","status":%q,"detail":"d","evidence":[]%s}`, flow, status, tag)
}

func associate(t *testing.T, repo *trace.Repository, stream config.WorkstreamID, f *shedFixture, unit string, flows ...string) {
	t.Helper()
	data, err := json.Marshal(execution.FeatureAssociation{Unit: unit, Review: 1, Candidate: "c-" + unit, Base: "b", Features: []execution.AssociatedFeature{}, Flows: flows})
	fixturekit.Must(t, err)
	h := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: execution.FeaturesDocument(unit), Revision: 1, Project: repo.Project(), Workstream: stream, Unit: unit, At: f.s.now(), Actor: serviceActor, Cause: "review"}
	fixturekit.Must(t, repo.RecordDocuments(context.Background(), []trace.Document{{Header: h, Path: "units/" + unit + "/features.json", Content: string(data) + "\n"}}))
}

func finalRound(t *testing.T, review int, claims ...string) (*shedFixture, config.WorkstreamID, *trace.Repository, verification.FinalMeasure, bool) {
	t.Helper()
	f, stream, repo, report := deliveryFixture(t)
	cfg := enableVerification(t, f, repo)
	installListingLever(t, claimsReport(claims...), `[]`)
	associate(t, repo, stream, f, "alpha", "x")
	associate(t, repo, stream, f, "beta", "y")
	return runFinalRound(t, f, repo, stream, cfg, report, review)
}

func runFinalRound(t *testing.T, f *shedFixture, repo *trace.Repository, stream config.WorkstreamID, cfg *config.Config, report FinalReport, review int) (*shedFixture, config.WorkstreamID, *trace.Repository, verification.FinalMeasure, bool) {
	t.Helper()
	controls := &runtimeControls{}
	controls.service.Store(f.s)
	scope := verifyThread(t, repo, stream, committeeRole, "reader", "", fmt.Sprintf("final-%d-reader-1", review), f.s.now)
	g, err := featureWorkspaces(cfg, repo).Of(stream)
	fixturekit.Must(t, err)
	tool := verifytool.Tool(verifytool.Env{Config: cfg, Repo: repo, Scope: scope, View: previewView(t), Now: f.s.now, Slots: controls.verifySlots,
		Resolve: func(context.Context, verifytool.Input) (verifytool.Target, string, error) {
			return verifytool.Target{Repo: repo, Stream: stream, Config: cfg, Commit: report.Commit, Export: g.ExportValidated}, "", nil
		}})
	if result := verifyInvoke(t, tool, `{"call":["flow","--name=sweep"]}`); result.Limitation != "" {
		t.Fatalf("%+v", result)
	}
	before, err := repo.Workflow(stream, trace.FeatureSubject)
	fixturekit.Must(t, err)
	recordFinalReview(t, f, repo, stream, report, review)
	after, err := repo.Workflow(stream, trace.FeatureSubject)
	fixturekit.Must(t, err)
	if before.Value != after.Value {
		t.Fatalf("recording the final review moved the workstream from %s to %s", before.Value, after.Value)
	}
	m, ok := latestMeasure(t, repo, stream, review)
	return f, stream, repo, m, ok
}

func TestAFinalRoundWhereOneFlowFailsRecordsOnlyTheUnitAssociatedWithItAsFailingAtFinalReview(t *testing.T) {
	_, _, _, m, ok := finalRound(t, 2, flowClaim("x", "pass"), flowClaim("y", "fail"))
	if !ok {
		t.Fatal("no measure was recorded")
	}
	if !slices.Equal(m.UnitsFailing, []string{"beta"}) || len(m.UnitsInconclusive) != 0 {
		t.Fatalf("%+v", m)
	}
	if len(m.Failed) != 1 || m.Failed[0].Flow != "y" || m.Failed[0].Shared || m.Failed[0].Unattributed || m.Unattributed() != 0 {
		t.Fatalf("%+v", m.Failed)
	}
	if m.Review != 2 || m.Runs.Fail != 1 || m.Runs.Pass != 0 {
		t.Fatalf("%+v", m)
	}
}

func TestAnInconclusiveFinalFlowIsRecordedApartFromFailures(t *testing.T) {
	_, _, _, m, ok := finalRound(t, 2, flowClaim("x", "inconclusive"), flowClaim("y", "pass"))
	if !ok {
		t.Fatal("no measure was recorded")
	}
	if len(m.UnitsFailing) != 0 || len(m.Failed) != 0 || !slices.Equal(m.UnitsInconclusive, []string{"alpha"}) || len(m.Inconclusive) != 1 || m.Inconclusive[0].Flow != "x" || m.Runs.Pass != 0 {
		t.Fatalf("%+v", m)
	}
}

func TestAFlowFailingForTwoUnitsIsOneSharedRecordAndAFlowNoUnitNamesIsUnattributed(t *testing.T) {
	f, stream, repo, report := deliveryFixture(t)
	cfg := enableVerification(t, f, repo)
	installListingLever(t, claimsReport(flowClaim("y", "fail"), flowClaim("orphan", "fail"), flowClaim("", "fail")), `[]`)
	associate(t, repo, stream, f, "alpha", "y")
	associate(t, repo, stream, f, "beta", "y")
	_, _, _, m, ok := runFinalRound(t, f, repo, stream, cfg, report, 2)
	if !ok {
		t.Fatal("no measure was recorded")
	}
	shared := slices.IndexFunc(m.Failed, func(a verification.FlowAttribution) bool { return a.Flow == "y" })
	orphan := slices.IndexFunc(m.Failed, func(a verification.FlowAttribution) bool { return a.Flow == "orphan" })
	if len(m.Failed) != 2 || shared < 0 || orphan < 0 {
		t.Fatalf("%+v", m.Failed)
	}
	if got := m.Failed[shared]; !got.Shared || !slices.Equal(got.Units, []string{"alpha", "beta"}) || got.Unattributed {
		t.Fatalf("%+v", got)
	}
	if got := m.Failed[orphan]; !got.Unattributed || got.Shared || len(got.Units) != 0 {
		t.Fatalf("%+v", got)
	}
	if !slices.Equal(m.UnitsFailing, []string{"alpha", "beta"}) || len(m.UnattributedClaims) != 1 || m.Unattributed() != 2 {
		t.Fatalf("%+v", m)
	}
}

func TestAProjectWithoutTheSettingRecordsNoFinalMeasure(t *testing.T) {
	f, stream, repo, report := deliveryFixture(t)
	recordFinalReview(t, f, repo, stream, report, 2)
	if _, ok := latestMeasure(t, repo, stream, 2); ok {
		t.Fatal("a measure was recorded")
	}
}

func TestOnlyTheFinalReviewersRunsOfTheRoundCountTowardItsMeasure(t *testing.T) {
	f, stream, repo, report := deliveryFixture(t)
	cfg := enableVerification(t, f, repo)
	installListingLever(t, claimsReport(flowClaim("y", "fail")), `[]`)
	associate(t, repo, stream, f, "beta", "y")
	controls := &runtimeControls{}
	controls.service.Store(f.s)
	g, err := featureWorkspaces(cfg, repo).Of(stream)
	fixturekit.Must(t, err)
	for _, tc := range []struct{ role, thread, unit, turn string }{
		{reviewerRole, "verify_reviewer", "beta", "review-1"},
		{committeeRole, "reader", "", "final-1-reader-1"},
	} {
		scope := verifyThread(t, repo, stream, tc.role, tc.thread, tc.unit, tc.turn, f.s.now)
		tool := verifytool.Tool(verifytool.Env{Config: cfg, Repo: repo, Scope: scope, View: previewView(t), Now: f.s.now, Slots: controls.verifySlots,
			Resolve: func(context.Context, verifytool.Input) (verifytool.Target, string, error) {
				return verifytool.Target{Repo: repo, Stream: stream, Config: cfg, Unit: tc.unit, Commit: report.Commit, Export: g.ExportValidated}, "", nil
			}})
		verifyInvoke(t, tool, `{"call":["flow"]}`)
	}
	recordFinalReview(t, f, repo, stream, report, 2)
	m, ok := latestMeasure(t, repo, stream, 2)
	if !ok || m.Runs.Total() != 0 || len(m.Failed) != 0 {
		t.Fatalf("the reviewer's run or another round's run counted: %+v", m)
	}
}
