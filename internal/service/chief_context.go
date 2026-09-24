package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/kpenfound/osmia/internal/bundle"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

// chiefContext renders the bundle and the durable status and open escalations
// that a fresh chief-of-staff session cannot recover from bounded replay.
func chiefContext(ctx context.Context, provider bundle.Provider, repository *trace.Repository, project config.ProjectID, stream config.WorkstreamID) (string, error) {
	b, err := provider.Assemble(ctx, project, bundle.Scope{Workstream: stream})
	if err != nil {
		return "", fmt.Errorf("context of workstream %s: %w", stream, err)
	}
	statuses, err := repository.Statuses()
	if err != nil {
		return "", fmt.Errorf("status of workstream %s: %w", stream, err)
	}
	var status *trace.Status
	for _, item := range statuses {
		if item.Workstream == stream {
			status = item.Status
			break
		}
	}
	inbox, err := repository.Inbox()
	if err != nil {
		return "", fmt.Errorf("escalations of workstream %s: %w", stream, err)
	}

	var out strings.Builder
	out.WriteString(b.Render())
	out.WriteString("\n## Latest status\n")
	if status == nil {
		out.WriteString("No status has been stored.\n")
	} else {
		fmt.Fprintf(&out, "Goal: %s\nAttention: %s\nNote: %s\n", status.Goal, emptyAsNone(status.Attention), status.Note)
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
	out.WriteString("\n## Open inbox escalations\n")
	count := 0
	for _, entry := range inbox {
		if entry.Workstream != stream || entry.State != trace.QuestionEscalated {
			continue
		}
		count++
		fmt.Fprintf(&out, "Inbox: %d\nBatch: %s\nQuestion for the owner: %s\nQuestions:\n", entry.Number, entry.Batch, entry.Rephrasing)
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
