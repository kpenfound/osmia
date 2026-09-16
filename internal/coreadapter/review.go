package coreadapter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"github.com/kpenfound/busybees/core/review"
)

// ReviewReference binds core artifacts to caller-owned identity and exact bytes.
type ReviewReference struct {
	Subject    string
	Candidate  Candidate
	DiffSHA256 string
}

func (r ReviewReference) String() string      { return r.Subject }
func (r ReviewReference) URL() string         { return "" }
func (r ReviewReference) ReviewScope() string { return r.Subject }

// ReviewAdapter runs core's evidence pipeline through the prepared-turn port.
// Turns must be concurrency-safe: core runs the selected angles concurrently.
// ArtifactDirectory must not exist; its parent must exist. Review owns this
// directory, and transfers template cleanup leases only once after all phases.
type ReviewAdapter struct{ Turns Turns }

var _ Reviews = (*ReviewAdapter)(nil)

func (a *ReviewAdapter) Review(ctx context.Context, req ReviewRequest) (out ReviewResult, err error) {
	sum := sha256.Sum256([]byte(req.Diff))
	ref := ReviewReference{req.Subject, req.Candidate, hex.EncodeToString(sum[:])}
	out.Subject, out.Candidate, out.DiffSHA256 = ref.Subject, ref.Candidate, ref.DiffSHA256
	defer func() {
		for i := len(req.Turn.Cleanup) - 1; i >= 0; i-- {
			if lease := req.Turn.Cleanup[i]; lease != nil {
				err = errors.Join(err, lease.Release(context.WithoutCancel(ctx)))
			}
		}
		if req.Turn.WorkspaceLease != nil && !req.Turn.RetainWorkspace && req.Turn.WorkspaceLease.Lease != nil {
			err = errors.Join(err, req.Turn.WorkspaceLease.Lease.Release(context.WithoutCancel(ctx)))
		}
		out.Partial = out.Partial || err != nil
	}()
	if err = ctx.Err(); err != nil {
		return
	}
	if a.Turns == nil {
		return out, unsupported("review execution", "no prepared-turn runner supplied")
	}
	if req.Subject == "" || req.ArtifactDirectory == "" || len(req.Angles) == 0 {
		return out, errors.New("review requires subject, artifact directory and angles")
	}
	seen := map[string]bool{}
	for _, angle := range req.Angles {
		if !slices.Contains(review.BuiltinAngles, angle) {
			return out, unsupported("review angle", angle)
		}
		if seen[angle] {
			return out, errors.New("duplicate review angle")
		}
		seen[angle] = true
	}
	iso := req.Turn.Sandbox.Verified
	if iso.Workspace.Access != ReadOnly || iso.Capabilities.WriteFiles || iso.Capabilities.Execute || iso.Capabilities.Network || !iso.DenyVCS || !iso.DenyInheritedEnvironment || !iso.DenyDeliveryCredentials {
		return out, unsupported("review isolation", "requires a verified read-only boundary without execution or network")
	}
	if req.Turn.Resume != nil {
		return out, unsupported("review resume", "each evidence phase requires its own session")
	}
	if _, err = translateTurn(req.Turn); err != nil {
		return
	}
	// Core may remove the artifact directory when distillation fails. Never give
	// it a preexisting directory containing caller-owned files.
	if err = os.Mkdir(req.ArtifactDirectory, 0o700); err != nil {
		return
	}
	bridge := &reviewTurns{turns: a.Turns, template: req.Turn, diff: req.Diff, sessions: map[string]SessionResult{}}
	bundle := &review.Bundle[ReviewReference]{Ref: ref}
	for _, item := range req.Context {
		bundle.Items = append(bundle.Items, review.Item{Source: item.Source, Content: item.Content})
		if item.SkippedReason != "" {
			bundle.Skipped = append(bundle.Skipped, item.Source+": "+item.SkippedReason)
		}
	}
	sized := map[string][]string{}
	for _, size := range review.Sizes {
		sized[size] = slices.Clone(req.Angles)
	}
	dir := iso.Workspace.Directory
	runner := review.Runner[ReviewReference]{Distiller: &review.Distiller[ReviewReference]{Agent: bridge, Dir: dir}, Angles: &review.Angles[ReviewReference]{Agent: bridge, Dir: dir, Provider: req.Turn.Profile.Backend, Model: req.Turn.Profile.Model, Sized: sized}}
	artifact, runErr := runner.Run(ctx, req.ArtifactDirectory, bundle, req.Diff)
	err = errors.Join(runErr, ctx.Err())
	if artifact == nil {
		// A late pipeline error can leave useful phase artifacts on disk.
		if _, statErr := os.Stat(filepath.Join(req.ArtifactDirectory, review.BriefFile)); statErr == nil {
			var readErr error
			artifact, readErr = review.ReadArtifact[ReviewReference](req.ArtifactDirectory)
			err = errors.Join(err, readErr)
		}
	}
	if artifact != nil {
		for _, run := range artifact.Runs {
			out.Partial = out.Partial || run.Failed()
		}
		if artifact.Findings != nil {
			out.Partial = out.Partial || len(artifact.Findings.Skipped) > 0
			for _, f := range artifact.Findings.Items {
				out.Findings = append(out.Findings, Finding{Category: f.Category, Severity: f.Severity, Path: f.File, Side: f.Side, Summary: f.Comment(), Evidence: f.Evidence, StartLine: f.Lines.Start, EndLine: f.Lines.End})
			}
		} else {
			out.Partial = true
		}
	}
	names := []string{review.DistillerName}
	for _, name := range review.BuiltinAngles {
		if seen[name] {
			names = append(names, name)
		}
	}
	for _, name := range names {
		if session, ok := bridge.sessions[name]; ok {
			out.Sessions = append(out.Sessions, session)
		}
	}
	// Preserve the exact input, including skipped items in acquisition order,
	// even when core cannot produce a brief. No execution credentials are stored.
	input := struct {
		Reference ReviewReference
		Context   []ContextItem
		Angles    []string
		Diff      string
	}{ref, req.Context, req.Angles, req.Diff}
	data, marshalErr := json.MarshalIndent(input, "", "  ")
	err = errors.Join(err, marshalErr)
	if mkdirErr := os.MkdirAll(req.ArtifactDirectory, 0o700); mkdirErr != nil {
		return out, errors.Join(err, mkdirErr)
	}
	if marshalErr == nil {
		err = errors.Join(err, os.WriteFile(filepath.Join(req.ArtifactDirectory, "input.json"), data, 0o600))
	}
	walkErr := filepath.WalkDir(req.ArtifactDirectory, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		relative, relErr := filepath.Rel(req.ArtifactDirectory, path)
		if relErr != nil {
			return relErr
		}
		out.Artifacts = append(out.Artifacts, Artifact{Kind: relative, Path: path})
		return nil
	})
	return out, errors.Join(err, walkErr)
}

type reviewTurns struct {
	turns    Turns
	template PreparedTurn
	diff     string
	mu       sync.Mutex
	sessions map[string]SessionResult
}

func (a *reviewTurns) Run(ctx context.Context, req review.AgentRequest) (*review.AgentResult, error) {
	turn := a.template
	turn.Cleanup = nil
	turn.RetainWorkspace = true
	turn.Scope.Turn += "/" + req.Name
	turn.SessionDirectory = filepath.Join(turn.SessionDirectory, req.Name)
	// Core omits a diff file for caller-owned checkouts. Inline exact bytes so
	// both distillation and angles receive the candidate without checkout writes.
	turn.Prompt = turn.Prompt + "\n\n" + req.Prompt + "\n\nExact candidate diff:\n" + a.diff
	result, err := a.turns.Run(ctx, turn)
	a.mu.Lock()
	a.sessions[req.Name] = result
	a.mu.Unlock()
	if err == nil && (result.IsError || result.Cancelled || result.TimedOut || result.ExitCode != 0 || result.Signal != 0) {
		err = fmt.Errorf("review phase %s failed: %s", req.Name, result.ErrorSubtype)
	}
	return &review.AgentResult{ID: result.Session.ID, Text: result.FinalResponse, Turns: result.Usage.Turns, CostUSD: result.Usage.CostUSD}, err
}
