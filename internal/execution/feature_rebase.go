package execution

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/kpenfound/agentcore/vcs"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/kpenfound/osmia/internal/workspace"
)

const finalRebaseDocument = "final-rebase"
const finalRebasePath = "final/rebase.json"

// FinalRebase is the document final/rebase.json: the rebase of the feature
// branch that final review Review reads. Before is the branch commit the
// review was asked for, Upstream the upstream commit it was rebased onto and
// Commit the rebased branch commit.
type FinalRebase struct {
	Review    int       `json:"review"`
	Operation string    `json:"operation"`
	Branch    string    `json:"branch"`
	Upstream  seal.Base `json:"upstream"`
	Before    string    `json:"before"`
	Commit    string    `json:"commit"`
}

// ReadFeatureRebase returns the recorded replay of final review k.
func (f *foreman) ReadFeatureRebase(stream config.WorkstreamID, k int) (FinalRebase, bool, error) {
	return ReadFeatureRebase(f.repository, stream, k)
}

// ReadFeatureRebase reads the recorded replay using only its trace repository.
func ReadFeatureRebase(repository *trace.Repository, stream config.WorkstreamID, k int) (FinalRebase, bool, error) {
	docs, err := trace.Read[trace.Document](repository, stream)
	if err != nil {
		return FinalRebase{}, false, err
	}
	for _, d := range slices.Backward(docs) {
		if d.ID != finalRebaseDocument {
			continue
		}
		var r FinalRebase
		if err := json.Unmarshal([]byte(d.Content), &r); err != nil {
			return FinalRebase{}, false, fmt.Errorf("%s revision %d: %w", d.Path, d.Revision, err)
		}
		if r.Review == k {
			return r, true, nil
		}
	}
	return FinalRebase{}, false, nil
}

// RecordFeatureRebase records the next revision of final/rebase.json.
func (f *foreman) RecordFeatureRebase(ctx context.Context, stream config.WorkstreamID, rebase FinalRebase) error {
	content, err := json.MarshalIndent(rebase, "", "  ")
	if err != nil {
		return err
	}
	revision, err := intake.NextRevision(f.repository, stream, finalRebaseDocument)
	if err != nil {
		return err
	}
	doc := trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: finalRebaseDocument, Revision: revision, Project: f.repository.Project(), Workstream: stream, At: f.s.Now(), Actor: foremanActor, Cause: rebase.Operation},
		Path: finalRebasePath, Content: string(content) + "\n"}
	return f.repository.RecordDocuments(ctx, []trace.Document{doc})
}

// PrepareFeatureRebase records an unrecorded replay before final review reads it.
// The service supplies base resolution so repository credentials stay at composition.
func (f *foreman) PrepareFeatureRebase(ctx context.Context, g workspace.Provider, stream config.WorkstreamID, rebase FinalRebase, current string, resolve, base func() (seal.Base, error)) (FinalRebase, []string, string, error) {
	before, branch, operation := rebase.Before, rebase.Branch, rebase.Operation
	if current != before {
		return rebase, nil, fmt.Sprintf("feature branch %s moved to %s since the review was asked for", branch, current), nil
	}
	requested, err := f.requestedAt(stream, operation)
	if err != nil {
		return rebase, nil, "", err
	}
	upstream, err := resolve()
	if err != nil {
		return rebase, nil, "", err
	}
	if err := f.s.Step("final-rebase-replaying"); err != nil {
		return rebase, nil, "", err
	}
	currentBase, err := base()
	if err != nil {
		return rebase, nil, "", err
	}
	rebase.Upstream = upstream
	var conflicts []string
	err = workspace.LocalStep(ctx, string(stream)+"/"+operation+"/replay", requested, []workspace.Provider{g}, func(local context.Context) error {
		commit, paths, err := g.ReplayFrom(local, currentBase.Commit, before, upstream.Commit, requested)
		if err != nil {
			return err
		}
		conflicts = paths
		if len(conflicts) > 0 {
			return nil
		}
		rebase.Commit = commit
		return f.RecordFeatureRebase(local, stream, rebase)
	})
	if err != nil {
		return rebase, nil, "", err
	}
	if len(conflicts) > 0 {
		return rebase, conflicts, fmt.Sprintf("feature branch %s does not rebase cleanly onto %s/%s at %s: %s conflicted; the branch is left at %s", branch, upstream.Remote, upstream.Branch, upstream.Commit, strings.Join(conflicts, ", "), before), nil
	}
	return rebase, nil, "", f.s.Step("final-rebase-recorded")
}

// MoveFeatureRebase advances the recorded replay, independently of the replay step.
func (f *foreman) MoveFeatureRebase(ctx context.Context, g workspace.Provider, stream config.WorkstreamID, rebase FinalRebase, current string) (string, error) {
	if current != rebase.Before && current != rebase.Commit {
		return fmt.Sprintf("feature branch %s moved to %s during the review", rebase.Branch, current), nil
	}
	err := workspace.LocalStep(ctx, string(stream)+"/"+rebase.Operation+"/move", f.s.Now(), []workspace.Provider{g}, func(local context.Context) error {
		acquired, err := g.Acquire(local, vcs.Request{Name: string(stream), Branch: rebase.Branch})
		if err != nil {
			return fmt.Errorf("feature branch %s: %w", rebase.Branch, err)
		}
		return g.Move(local, acquired.(workspace.Worktree), rebase.Before, rebase.Commit)
	})
	if err != nil {
		return "", err
	}
	return "", f.s.Step("final-rebase-moved")
}
