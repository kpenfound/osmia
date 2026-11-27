package execution

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/kpenfound/osmia/internal/bundle"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/isolation"
	"github.com/kpenfound/osmia/internal/kb"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/kpenfound/osmia/internal/verification"
	"github.com/kpenfound/osmia/internal/verifytool"
	"github.com/kpenfound/osmia/internal/workspace"
)

// PlaceReviewerVerification puts into view the retained files of the verify
// runs the unit's reviewer made on the candidate its latest review returned.
// It runs no lever. A unit without a review result, or whose reviewer ran
// nothing, places nothing.
func PlaceReviewerVerification(repo *trace.Repository, stream config.WorkstreamID, unit string, view isolation.View) error {
	docs, err := trace.Read[trace.Document](repo, stream)
	if err != nil {
		return err
	}
	var latest trace.Document
	for _, d := range docs {
		if d.ID == ReviewDocument(unit) && d.Revision >= latest.Revision {
			latest = d
		}
	}
	var result UnitReviewResult
	if latest.Revision == 0 || json.Unmarshal([]byte(latest.Content), &result) != nil || result.Identity.Candidate.Revision == "" {
		return nil
	}
	runs, err := verifytool.ReviewerRuns(repo, stream, unit, result.Identity.Candidate.Revision)
	if err != nil {
		return err
	}
	for _, op := range runs {
		if _, err := verifytool.Place(repo, op.Workstream, op.RunID, view, false); err != nil {
			return err
		}
	}
	return nil
}

// UnitVerification is the document units/<unit>/verification.json: what the
// service learned about the lever when it reran a unit's declared invocations
// on a candidate, the flows the lever's list returned or why it returned none.
type UnitVerification struct {
	Unit      string `json:"unit"`
	Report    int    `json:"report"`
	Candidate string `json:"candidate"`
	Base      string `json:"base"`
	// Listed are the flows the lever lists, null when the list was not obtained.
	Listed    []string `json:"listed"`
	ListError string   `json:"list_error,omitempty"`
}

// FeatureAssociation is the document units/<unit>/features.json: the features
// the approved candidate's diff touches and their flows, read from that
// candidate's feature map, recorded when the unit passes unit review. Flags
// says whether the diff touched the lever's source or the map directory.
type FeatureAssociation struct {
	Unit      string                `json:"unit"`
	Review    int                   `json:"review"`
	Candidate string                `json:"candidate"`
	Base      string                `json:"base"`
	Features  []AssociatedFeature   `json:"features"`
	Flows     []string              `json:"flows"`
	Touched   VerificationTouched `json:"touched"`
}

// AssociatedFeature is one feature of a FeatureAssociation.
type AssociatedFeature struct {
	Feature string   `json:"feature"`
	File    string   `json:"file"`
	Flows   []string `json:"flows"`
}

// VerificationTouched says which parts of the verification setup a diff touched.
type VerificationTouched struct {
	Lever bool `json:"lever"`
	Map   bool `json:"map"`
}

func VerificationDocument(unit string) string { return trace.UnitSubject(unit) + "-verification" }

func FeaturesDocument(unit string) string { return trace.UnitSubject(unit) + "-features" }

func verificationPath(unit string) string { return "units/" + unit + "/verification.json" }

func featuresPath(unit string) string { return "units/" + unit + "/features.json" }

func latestDocumentOf(repo *trace.Repository, stream config.WorkstreamID, id string) (trace.Document, error) {
	docs, err := trace.Read[trace.Document](repo, stream)
	if err != nil {
		return trace.Document{}, err
	}
	var latest trace.Document
	for _, d := range docs {
		if d.ID == id && d.Revision >= latest.Revision {
			latest = d
		}
	}
	return latest, nil
}

// ReadFeatureAssociation returns the feature association recorded for unit's
// approved candidate, and whether one is recorded.
func ReadFeatureAssociation(repo *trace.Repository, stream config.WorkstreamID, unit string) (FeatureAssociation, bool, error) {
	doc, err := latestDocumentOf(repo, stream, FeaturesDocument(unit))
	if err != nil || doc.Revision == 0 {
		return FeatureAssociation{}, false, err
	}
	var association FeatureAssociation
	if err := json.Unmarshal([]byte(doc.Content), &association); err != nil {
		return FeatureAssociation{}, false, err
	}
	return association, true, nil
}

func readUnitVerification(repo *trace.Repository, stream config.WorkstreamID, unit, candidate string) (UnitVerification, bool, error) {
	doc, err := latestDocumentOf(repo, stream, VerificationDocument(unit))
	if err != nil || doc.Revision == 0 {
		return UnitVerification{}, false, err
	}
	var rec UnitVerification
	if err := json.Unmarshal([]byte(doc.Content), &rec); err != nil {
		return UnitVerification{}, false, err
	}
	return rec, rec.Candidate == candidate, nil
}

func (m *masons) verifying() bool { return m.cfg != nil && m.cfg.Project.Verification.Enabled() }

func (m *masons) verificationBrief(ctx context.Context, stream config.WorkstreamID, unit string, footprint []string) (*bundle.Verification, error) {
	setting := m.cfg.Project.Verification
	brief := &bundle.Verification{MapDir: setting.Directory()}
	g, err := NewUnitWorkspaces(m.cfg, m.repository).Of(stream)
	if err != nil {
		return brief, nil
	}
	report, revision, err := unitReport(m.repository, stream, unit)
	if err != nil {
		return nil, err
	}
	var commit string
	var changed []string
	if revision > 0 && report.Candidate != "" && report.Base != "" {
		commit = report.Candidate
		changed, _ = g.ChangedPaths(ctx, report.Base, report.Candidate)
	} else if tip, exists, err := g.Branch(ctx, featureBranch(stream)); err == nil && exists {
		commit = tip
	}
	if commit == "" {
		return brief, nil
	}
	tree, err := g.Tree(ctx, commit)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return brief, nil
	}
	entities, _, err := kb.LoadRevision(m.repository)
	if err != nil {
		return brief, nil
	}
	resolved := entities.ResolveEntities(footprint)
	patterns := slices.Clone(resolved.Paths)
	if slices.Contains(resolved.Entities, kb.RootID) {
		patterns = append(patterns, "**")
	}
	features := verification.ReadMap(tree, setting.Directory()).Applicable(tree, patterns, changed)
	brief.Features = bundle.Pointers(features)
	return brief, nil
}

func (m *masons) verificationSections(ctx context.Context, g workspace.Provider, stream config.WorkstreamID, unit string, report UnitReport) (string, error) {
	setting := m.cfg.Project.Verification
	changed, err := g.ChangedPaths(ctx, report.Base, report.Candidate)
	if err != nil {
		return "", fmt.Errorf("candidate changed files: %w", err)
	}
	in := verifytool.ReviewInput{Declared: len(report.Invocations), Flags: touchedFlags(setting, changed)}
	tree, err := g.Tree(ctx, report.Candidate)
	if err != nil {
		return "", fmt.Errorf("candidate tree: %w", err)
	}
	featureMap := verification.ReadMap(tree, setting.Directory())
	in.Features = featureMap.MatchFiles(changed)
	record, listed, err := readUnitVerification(m.repository, stream, unit, report.Candidate)
	if err != nil {
		return "", err
	}
	if listed {
		in.Listed = record.Listed
	}
	in.Notes = featureMap.Check(in.Listed)
	switch {
	case !listed:
		in.Notes = append(in.Notes, verification.Note{Message: "the service did not read the lever's list for this candidate, so flows were not compared with it"})
	case record.ListError != "":
		in.Notes = append(in.Notes, verification.Note{Message: "the lever's list could not be read, so flows were not compared with it: " + record.ListError})
	}
	if in.Reruns, err = verifytool.Reruns(m.repository, stream, unit, report.Candidate); err != nil {
		return "", err
	}
	if in.Runs, err = verifytool.RunsOn(m.repository, stream, unit, report.Candidate); err != nil {
		return "", err
	}
	return verifytool.RenderReview(in), nil
}

func touchedFlags(setting verification.Setting, changed []string) []string {
	lever, mapDir := touched(setting, changed)
	var flags []string
	if len(lever) != 0 {
		flags = append(flags, "the diff touches the lever's source ("+strings.Join(lever, ", ")+"): look closely at whether the lever still drives the behaviour it claims to")
	}
	if len(mapDir) != 0 {
		flags = append(flags, "the diff touches the feature map directory "+setting.Directory()+" ("+strings.Join(mapDir, ", ")+"): look closely at whether the map still describes the behaviour")
	}
	return flags
}

func touched(setting verification.Setting, changed []string) (lever, mapDir []string) {
	dir := setting.Directory()
	for _, file := range changed {
		if file == dir || strings.HasPrefix(file, dir+"/") {
			mapDir = append(mapDir, file)
		}
		if slices.ContainsFunc(setting.LeverPaths, func(glob string) bool { return verification.MatchGlob(glob, file) }) {
			lever = append(lever, file)
		}
	}
	return lever, mapDir
}

func (c *checkers) rerunDeclared(ctx context.Context, in checkInput, g workspace.Provider) error {
	cfg := c.cfg
	if about := c.s.About(c.repository); about != nil {
		cfg = about
	}
	if !cfg.Project.Verification.Enabled() {
		return nil
	}
	report, revision, err := unitReport(c.repository, in.Workstream, in.Unit)
	if err != nil {
		return err
	}
	if report.Candidate != in.Candidate || revision != in.Report {
		return nil
	}
	root := filepath.Join(cfg.Root.String(), "checks")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	dir, err := os.MkdirTemp(root, "verify-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	if err := g.ExportValidated(ctx, in.Candidate, dir, nil); err != nil {
		return fmt.Errorf("candidate export for verification: %w", err)
	}
	if _, listed, err := readUnitVerification(c.repository, in.Workstream, in.Unit, in.Candidate); err != nil {
		return err
	} else if !listed {
		record := UnitVerification{Unit: in.Unit, Report: revision, Candidate: in.Candidate, Base: in.Base}
		flows, err := verifytool.Dagger(cfg).LeverFlows(ctx, dir, cfg.Project.Verification.Call)
		switch {
		case err == nil:
			record.Listed = flows
		case ctx.Err() != nil:
			return ctx.Err()
		default:
			record.ListError = verifytool.Diagnostic(err.Error())
		}
		if err := c.writeUnitDocument(ctx, in, VerificationDocument(in.Unit), verificationPath(in.Unit), record); err != nil {
			return err
		}
	}
	if len(report.Invocations) == 0 {
		return nil
	}
	return verifytool.RerunDeclared(ctx, verifytool.RerunInput{Repo: c.repository, Stream: in.Workstream, Config: cfg, Scope: c.Scope(in), Unit: in.Unit, Report: revision,
		Commit: in.Candidate, Dir: dir, Declared: report.Invocations, Now: c.s.Now})
}

func (c *checkers) writeUnitDocument(ctx context.Context, in checkInput, id, path string, content any) error {
	data, err := json.MarshalIndent(content, "", "  ")
	if err != nil {
		return err
	}
	latest, err := latestDocumentOf(c.repository, in.Workstream, id)
	if err != nil {
		return err
	}
	h := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: id, Revision: latest.Revision + 1, Project: c.repository.Project(), Workstream: in.Workstream, Unit: in.Unit, At: c.s.Now(), Actor: ChecksActor, Cause: checkRequestID(in.Unit, in.Run)}
	return c.repository.RecordDocuments(ctx, []trace.Document{{Header: h, Path: path, Content: string(data) + "\n"}})
}

func (r *reviewers) recordFeatureAssociation(ctx context.Context, stream config.WorkstreamID, unit string, result UnitReviewResult) error {
	if !r.verifying() {
		return nil
	}
	candidate, base := result.Identity.Candidate.Revision, result.Identity.Candidate.BaseRevision
	if prior, ok, err := ReadFeatureAssociation(r.repository, stream, unit); err != nil {
		return err
	} else if ok && prior.Candidate == candidate && prior.Base == base {
		return nil
	}
	g, err := NewUnitWorkspaces(r.cfg, r.repository).Of(stream)
	if err != nil {
		return err
	}
	changed, err := g.ChangedPaths(ctx, base, candidate)
	if err != nil {
		return fmt.Errorf("candidate changed files: %w", err)
	}
	tree, err := g.Tree(ctx, candidate)
	if err != nil {
		return fmt.Errorf("candidate tree: %w", err)
	}
	setting := r.cfg.Project.Verification
	features := verification.ReadMap(tree, setting.Directory()).MatchFiles(changed)
	revision, err := r.reviewRevision(stream, unit)
	if err != nil {
		return err
	}
	lever, mapDir := touched(setting, changed)
	association := FeatureAssociation{Unit: unit, Review: revision, Candidate: candidate, Base: base, Features: []AssociatedFeature{}, Flows: []string{}, Touched: VerificationTouched{Lever: len(lever) != 0, Map: len(mapDir) != 0}}
	for _, f := range features {
		association.Features = append(association.Features, AssociatedFeature{Feature: f.Name, File: f.File, Flows: slices.Clone(f.Flows)})
	}
	if flows := verification.UnionFlows(features); flows != nil {
		association.Flows = flows
	}
	data, err := json.MarshalIndent(association, "", "  ")
	if err != nil {
		return err
	}
	latest, err := latestDocumentOf(r.repository, stream, FeaturesDocument(unit))
	if err != nil {
		return err
	}
	h := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: FeaturesDocument(unit), Revision: latest.Revision + 1, Project: r.repository.Project(), Workstream: stream, Unit: unit, At: r.s.Now(), Actor: ReviewerActor, Cause: ReviewDocument(unit)}
	return r.repository.RecordDocuments(ctx, []trace.Document{{Header: h, Path: featuresPath(unit), Content: string(data) + "\n"}})
}
