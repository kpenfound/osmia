package intakefixture

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/workspace"
)

// ResolvedDone is the outcome of a drift-resolution mason that finished with
// report. status is the mason's done status.
func ResolvedDone(status string, report any) *coreadapter.Outcome {
	data, _ := json.Marshal(report)
	return &coreadapter.Outcome{Status: status, Report: string(data)}
}

// AwaitResolution fails the test unless attempt reports the drift rebase
// awaiting want. awaits is the error a waiting drift rebase returns.
func AwaitResolution(t *testing.T, attempt func() (coreadapter.OperationResult, error), awaits error, want string) {
	t.Helper()
	if result, err := attempt(); !errors.Is(err, awaits) || !strings.Contains(err.Error(), want) {
		t.Fatalf("the drift rebase did not await %q: %+v %v", want, result, err)
	}
}

// ConflictedDrift moves the feature and the upstream to conflicting
// CODEOWNERS files, requests a drift rebase and returns the feature's commit,
// the upstream's commit and the operation.
func ConflictedDrift(t *testing.T, moveFeature, advanceUpstream func(map[string]string) string, request func() coreadapter.Operation) (string, string, coreadapter.Operation) {
	t.Helper()
	before := moveFeature(map[string]string{"CODEOWNERS": "/internal/ @feature\n"})
	upstream := advanceUpstream(map[string]string{"CODEOWNERS": "/internal/ @upstream\n"})
	return before, upstream, request()
}

// BranchTip is the tip of branch in provider.
func BranchTip(t *testing.T, provider workspace.Provider, branch string) string {
	t.Helper()
	tip, _, err := provider.Branch(context.Background(), branch)
	fixturekit.Must(t, err)
	return tip
}

// ParentOf is the first parent of commit in clone.
func ParentOf(t *testing.T, clone, commit string) string {
	t.Helper()
	return strings.TrimSpace(fixturekit.DemoGit(t, filepath.Dir(clone), "-C", clone, "rev-parse", commit+"^"))
}

// FileAt is the content of path at commit in clone.
func FileAt(t *testing.T, clone, commit, path string) string {
	t.Helper()
	return fixturekit.DemoGit(t, filepath.Dir(clone), "-C", clone, "show", commit+":"+path)
}
