package trace

import (
	"encoding/json"
	"testing"
	"time"
)

func proposalRevision(t *testing.T, question string, revision int, at time.Time, p CharterProposal) Document {
	t.Helper()
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return Document{Header: Header{ID: CharterProposalID(question), Revision: revision, At: at}, Path: CharterProposalPath(question), Content: string(data)}
}

func TestCharterDecisionsReturnsOnlyDecidedProposalsAtTheirDecisionRevision(t *testing.T) {
	start := time.Date(2026, 9, 16, 9, 0, 0, 0, time.UTC)
	docs := []Document{
		proposalRevision(t, "1", 1, start, CharterProposal{Question: "1", Rule: "Resume uploads."}),
		proposalRevision(t, "1", 2, start.Add(time.Hour), CharterProposal{Question: "1", Rule: "Resume uploads.", Decision: CharterRatify, Note: "Yes."}),
		proposalRevision(t, "1", 3, start.Add(2*time.Hour), CharterProposal{Question: "1", Rule: "Resume uploads.", Decision: CharterRatify, Note: "Yes.", Number: 4, Charter: 5}),
		proposalRevision(t, "2", 1, start, CharterProposal{Question: "2", Rule: "Waiting."}),
		proposalRevision(t, "3", 1, start, CharterProposal{Question: "3", Rule: "No caps."}),
		proposalRevision(t, "3", 2, start.Add(30*time.Minute), CharterProposal{Question: "3", Rule: "No caps.", Decision: CharterDecline, Note: "Too broad."}),
		{Header: Header{ID: "spec", Revision: 1, At: start}, Path: "spec.md", Content: "# Spec"},
	}
	got, err := CharterDecisions(docs)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("decisions: %+v", got)
	}
	// Oldest decision first: the decline, then the ratification.
	if got[0].Proposal.Question != "3" || got[0].Proposal.Decision != CharterDecline || got[0].Proposal.Note != "Too broad." || !got[0].Proposed.At.Equal(start) || !got[0].Decided.At.Equal(start.Add(30*time.Minute)) {
		t.Fatalf("declined: %+v", got[0])
	}
	// The ratification is the revision that recorded the decision, not the later one that recorded the charter number.
	if got[1].Proposal.Question != "1" || got[1].Decided.Revision != 2 || got[1].Proposal.Number != 0 || !got[1].Proposed.At.Equal(start) {
		t.Fatalf("ratified: %+v", got[1])
	}
}
