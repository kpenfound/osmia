package bundle

import (
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/charter"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestBriefDoesNotGrowWithReferenceContent(t *testing.T) {
	b := Bundle{Charter: Charter{Rules: []charter.Rule{{Number: 1, Text: "No new dependencies"}}}, Decisions: []Decision{{Decision: "Preserve the owner hold"}}}
	before := b.Brief()
	for range 1000 {
		b.Knowledge = append(b.Knowledge, Prose{Content: strings.Repeat("irrelevant knowledge", 1000)})
		b.ProjectNotices = append(b.ProjectNotices, ProjectNotice{ProjectNotice: trace.ProjectNotice{Text: strings.Repeat("old notice", 1000)}})
		b.Entities.Entities = append(b.Entities.Entities, Entity{ID: "unrelated-entity"})
	}
	after := b.Brief()
	if len(after) > len(before)+20 || strings.Contains(after, "irrelevant knowledge") || strings.Contains(after, "old notice") || strings.Contains(after, "unrelated-entity") {
		t.Fatalf("reference material expanded briefing: %d -> %d", len(before), len(after))
	}
	for _, want := range []string{"No new dependencies", "Preserve the owner hold", "project_context", "1000 KB files", "1000 informational notices"} {
		if !strings.Contains(after, want) {
			t.Errorf("missing %q", want)
		}
	}
	if len(b.Knowledge) != 1000 || len(b.ProjectNotices) != 1000 {
		t.Fatal("brief removed durable input")
	}
}
