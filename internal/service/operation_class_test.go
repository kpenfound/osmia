package service

import (
	"encoding/json"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestOperationClassKeepsWorkflowOrderSeparateFromWorkers(t *testing.T) {
	for _, action := range []string{DraftAction, ReplyAction, RedraftAction, AmendmentDraftAction, AmendmentReplyAction, "seal", "build", "land", "rebase", "drift", "publish"} {
		for _, boundary := range []coreadapter.OperationBoundary{coreadapter.RunnerBoundary, coreadapter.RepositoryBoundary} {
			if got := operationClass(coreadapter.Operation{Action: action, Boundary: boundary}); got != trace.WorkflowOperation {
				t.Fatalf("%s %s class=%s", boundary, action, got)
			}
		}
	}
	for _, action := range []string{RoundAction, AmendmentRoundAction, CheckAction, deltaTurnAction} {
		if got := operationClass(coreadapter.Operation{Action: action, Boundary: coreadapter.RunnerBoundary}); got != trace.WorkerOperation {
			t.Fatalf("%s class=%s", action, got)
		}
	}
	for _, agent := range []string{trace.ChiefOfStaff, sessionAgentID, "mason", "reviewer", "committee"} {
		input, _ := json.Marshal(thread.TurnInput{Workstream: config.WorkstreamID("w_0123456789abcdef0123456789abcdef"), Agent: agent, Turn: "turn"})
		want := trace.WorkerOperation
		if agent == trace.ChiefOfStaff || agent == sessionAgentID {
			want = trace.ConversationOperation
		}
		if got := operationClass(coreadapter.Operation{Action: thread.TurnAction, Boundary: coreadapter.RunnerBoundary, Input: input}); got != want {
			t.Fatalf("%s class=%s, want=%s", agent, got, want)
		}
	}
}
