package service

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/kpenfound/osmia/internal/bundle"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

// chiefContext renders the bundle and the durable status and open escalations
// that a fresh chief-of-staff session cannot recover from bounded replay.
// stream is the member the caller named; the chief of staff is shared by its
// whole workgroup, so status and open escalations are shown for every
// member, each naming the workstream it is about whenever that is not
// stream itself.
//
// closeRepository releases the caller's shared reference after trace reads.
// Provider assembly may independently open that same refcounted authority.
func chiefContext(ctx context.Context, provider bundle.Provider, repository *trace.Repository, closeRepository func(), project config.ProjectID, stream config.WorkstreamID) (string, error) {
	members, err := repository.WorkgroupMembers(stream)
	if err != nil {
		closeRepository()
		return "", fmt.Errorf("workgroup of workstream %s: %w", stream, err)
	}
	statuses, err := repository.Statuses()
	if err != nil {
		closeRepository()
		return "", fmt.Errorf("status of workstream %s: %w", stream, err)
	}
	inbox, err := repository.Inbox()
	if err != nil {
		closeRepository()
		return "", fmt.Errorf("escalations of workstream %s: %w", stream, err)
	}
	closeRepository()

	b, err := provider.Assemble(ctx, project, bundle.Scope{Role: "chief_of_staff", Workstream: stream})
	if err != nil {
		return "", fmt.Errorf("context of workstream %s: %w", stream, err)
	}

	var out strings.Builder
	out.WriteString(b.Brief())
	out.WriteString("\n## Latest status\n")
	none := true
	for _, item := range statuses {
		if !slices.Contains(members, item.Workstream) || item.Status == nil {
			continue
		}
		none = false
		status := item.Status
		label := ""
		if item.Workstream != stream {
			label = fmt.Sprintf(" (workstream %s)", item.Workstream)
		}
		fmt.Fprintf(&out, "Goal%s: %s\nAttention: %s\nNote: %s\n", label, status.Goal, emptyAsNone(status.Attention), status.Note)
		if len(status.Agents) == 0 {
			out.WriteString("Agents: none\n")
		} else {
			out.WriteString("Agents:\n")
			for _, agent := range status.Agents {
				fmt.Fprintf(&out, "- %s\n", agent)
			}
		}
		fmt.Fprintf(&out, "Revision: %d\n", status.Revision)
	}
	if none {
		out.WriteString("No status has been stored.\n")
	}
	out.WriteString("\n## Open inbox escalations\n")
	count := 0
	for _, entry := range inbox {
		if !slices.Contains(members, entry.Workstream) || (entry.State != trace.QuestionEscalated && entry.Hold == "") {
			continue
		}
		count++
		label := ""
		if entry.Workstream != stream {
			label = fmt.Sprintf(" (workstream %s)", entry.Workstream)
		}
		fmt.Fprintf(&out, "Inbox: %d%s\nBatch: %s\nQuestion for the owner: %s\nQuestions:\n", entry.Number, label, entry.Batch, entry.Rephrasing)
		if entry.Hold != "" {
			fmt.Fprintf(&out, "Recoverable hold: %s\n", entry.Hold)
		}
		for _, question := range entry.Questions {
			fmt.Fprintf(&out, "- %s: %s\n", question.Asked.ID, question.Asked.Question)
		}
	}
	if count == 0 {
		out.WriteString("None.\n")
	}
	return out.String(), nil
}

func emptyAsNone(value string) string {
	if value == "" {
		return "None"
	}
	return value
}
