package fixturekit

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/trace"
)

// PublicationForkBranch returns the fork's delivery commit and whether it exists.
func PublicationForkBranch(t *testing.T, fork, branch string) (string, bool) {
	t.Helper()
	out := strings.TrimSpace(DemoGit(t, filepath.Dir(fork), "-C", fork, "for-each-ref", "--format=%(objectname)", "refs/heads/"+branch))
	return out, out != ""
}

// ApprovePublication records approval of the presented draft or the given description.
func ApprovePublication(t *testing.T, stream config.WorkstreamID, description *string,
	present func(context.Context, string) (api.DeliveryPresentation, *api.APIError),
	approve func(context.Context, string, api.DeliveryDecision) (api.DeliveryApproval, *api.APIError),
) api.DeliveryApproval {
	t.Helper()
	presented, apiErr := present(context.Background(), string(stream))
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	approval, apiErr := approve(context.Background(), string(stream), api.DeliveryDecision{Review: presented.Report.Review, ReviewRevision: presented.ReviewRevision, Commit: presented.Report.Commit, DraftHash: presented.DraftHash, Description: description})
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	return approval
}

// PublicationOperations returns publish operations in request order.
func PublicationOperations(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, action string) []coreadapter.Operation {
	t.Helper()
	ops, err := repository.Operations(stream)
	Must(t, err)
	ops = slices.DeleteFunc(ops, func(o trace.OperationRecord) bool { return o.Operation.Action != action })
	slices.SortFunc(ops, func(a, b trace.OperationRecord) int { return a.Transition.At.Compare(b.Transition.At) })
	var out []coreadapter.Operation
	for _, o := range ops {
		out = append(out, o.Operation)
	}
	return out
}

// RequestPublication runs a pass and asserts that it requests one more publication.
func RequestPublication(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, action string, pass func(context.Context) error) coreadapter.Operation {
	t.Helper()
	before := len(PublicationOperations(t, repository, stream, action))
	Must(t, pass(context.Background()))
	ops := PublicationOperations(t, repository, stream, action)
	if len(ops) != before+1 {
		t.Fatalf("the pass asked for %d publications, want one more than %d", len(ops), before)
	}
	return ops[len(ops)-1]
}

func PublicationFeature(t *testing.T, repository *trace.Repository, stream config.WorkstreamID) string {
	t.Helper()
	state, err := repository.Workflow(stream, trace.FeatureSubject)
	Must(t, err)
	return state.Value
}

// ReopenPublication closes and reopens the trace as a restarted service does.
// The caller binds the returned repository to its service.
func ReopenPublication(t *testing.T, repository *trace.Repository, root config.Root, project config.Project) *trace.Repository {
	t.Helper()
	repository.Close()
	return OpenTrace(t, root, project)
}
