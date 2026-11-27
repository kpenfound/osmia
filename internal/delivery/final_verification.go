package delivery

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/kpenfound/osmia/internal/verification"
	"github.com/kpenfound/osmia/internal/verifytool"
)

// FinalMeasureDocument is the trace record ID of final review k's verification
// measure.
func FinalMeasureDocument(k int) string { return fmt.Sprintf("final-verification-%d", k) }

func (a *FinalReviewer) finalMeasure(stream config.WorkstreamID, report FinalReport) (trace.Document, bool) {
	cfg := a.S.About(a.Repository)
	if cfg == nil || !cfg.HasProject() || !cfg.Project.Verification.Enabled() {
		return trace.Document{}, false
	}
	runs, err := verifytool.FinalRuns(a.Repository, stream, committeeRole, report.Review, report.Commit)
	if err != nil {
		return trace.Document{}, false
	}
	associations, err := execution.ReadFeatureAssociations(a.Repository, stream)
	if err != nil {
		return trace.Document{}, false
	}
	var approved []verification.Association
	for _, association := range associations {
		approved = append(approved, verification.Association{Unit: association.Unit, Flows: association.Flows})
	}
	results, limited := verifytool.FinalResults(runs)
	measure := verification.AttributeFinal(report.Review, report.Commit, results, limited, approved)
	revision, err := nextRevision(a.Repository, stream, FinalMeasureDocument(report.Review))
	if err != nil {
		return trace.Document{}, false
	}
	content, err := json.MarshalIndent(measure, "", "  ")
	if err != nil {
		return trace.Document{}, false
	}
	return trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: FinalMeasureDocument(report.Review), Revision: revision, Project: a.Repository.Project(), Workstream: stream, At: a.S.Now(), Actor: FinalReviewActor, Cause: report.Operation},
		Path: verification.FinalMeasurePath(report.Review), Content: string(content) + "\n"}, true
}

func (a *FinalReviewer) sweep(ctx context.Context, cfg *config.Config, stream config.WorkstreamID, report FinalReport) string {
	if !cfg.Project.Verification.Enabled() {
		return ""
	}
	setting := cfg.Project.Verification
	in := verifytool.SweepInput{Dir: setting.Directory(), Commit: report.Commit}
	g, err := featureWorkspaces(cfg, a.Repository).Of(stream)
	if err != nil {
		in.Notes = append(in.Notes, verification.Note{Message: "the feature-branch head could not be read, so the map was not read: " + verifytool.Diagnostic(err.Error())})
		return verifytool.RenderSweep(in)
	}
	tree, err := g.Tree(ctx, report.Commit)
	if err != nil {
		in.Notes = append(in.Notes, verification.Note{Message: "the feature-branch head could not be read, so the map was not read: " + verifytool.Diagnostic(err.Error())})
		return verifytool.RenderSweep(in)
	}
	featureMap := verification.ReadMap(tree, setting.Directory())
	index, _ := verification.ReadIndex(tree, setting.Directory())
	in.Features = featureMap.Ordered(index)
	listed, reason := a.listFlows(ctx, cfg, g.ExportValidated, stream, report.Commit)
	in.Notes = featureMap.Check(listed)
	if reason != "" {
		in.Notes = append(in.Notes, verification.Note{Message: reason})
	}
	return verifytool.RenderSweep(in)
}

func (a *FinalReviewer) listFlows(ctx context.Context, cfg *config.Config, export func(context.Context, string, string, func(string) error) error, stream config.WorkstreamID, commit string) ([]string, string) {
	if a.S.VerifySlots == nil {
		return nil, "the service could not read the lever's list, so flows were not compared with it"
	}
	ctx, cancel := context.WithTimeout(ctx, verifytool.DefaultTimeout)
	defer cancel()
	release, err := a.S.VerifySlots(ctx, a.Repository.Project(), stream)
	if err != nil {
		return nil, "the service could not read the lever's list, so flows were not compared with it: " + verifytool.Diagnostic(err.Error())
	}
	defer release()
	root := filepath.Join(cfg.Root.String(), "checks")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, "the lever's list could not be read, so flows were not compared with it: " + verifytool.Diagnostic(err.Error())
	}
	dir, err := os.MkdirTemp(root, "verify-")
	if err != nil {
		return nil, "the lever's list could not be read, so flows were not compared with it: " + verifytool.Diagnostic(err.Error())
	}
	defer os.RemoveAll(dir)
	if err := export(ctx, commit, dir, nil); err != nil {
		return nil, "the lever's list could not be read, so flows were not compared with it: " + verifytool.Diagnostic(err.Error())
	}
	flows, err := verifytool.Dagger(cfg).LeverFlows(ctx, dir, cfg.Project.Verification.Call)
	if err != nil {
		return nil, "the lever's list could not be read, so flows were not compared with it: " + verifytool.Diagnostic(err.Error())
	}
	return flows, ""
}
