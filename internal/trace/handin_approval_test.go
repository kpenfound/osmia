package trace

import (
	"context"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
)

func TestSharedConversationWithOneSelectedWorkstreamUsesIndividualApproval(t *testing.T) {
	for _, multiple := range []bool{false, true} {
		name := "single"
		if multiple {
			name = "multiple"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root, r1, r2, _, _ := crossRepoFixture(t)
			ctx := context.Background()
			required, err := r2.GroupApprovalRequired(crossMember2)
			if err != nil || required {
				t.Fatalf("ordinary approval: %v %v", required, err)
			}
			id, err := config.NewWorkgroupID()
			if err != nil {
				t.Fatal(err)
			}
			origin, err := config.NewWorkstreamID(crossMember1, crossMember2)
			if err != nil {
				t.Fatal(err)
			}
			members := []CrossRepoMember{{crossMember2, r2}}
			if multiple {
				members = append(members, CrossRepoMember{crossMember1, r1})
			}
			authority, _, err := PrepareDesignWorkgroup(ctx, root, id, ConversationOrigin{Project: crossProject1, Workstream: origin}, members, at, owner)
			if err != nil {
				t.Fatal(err)
			}
			open := true
			defer func() {
				if open {
					_ = authority.Close()
				}
			}()
			if err := r2.RecordCrossRepoWorkgroup(ctx, crossMember2, id, at, owner, "selection"); err != nil {
				t.Fatal(err)
			}
			required, err = r2.GroupApprovalRequired(crossMember2)
			if err != nil || required != multiple {
				t.Fatalf("shared approval requirement: %v %v", required, err)
			}
			approved, err := r2.GroupApproved(crossMember2)
			if err != nil || approved == multiple {
				t.Fatalf("group completion barrier: %v %v", approved, err)
			}
			// No group completion substitutes for the member's own owner decision.
			state, err := r2.Workflow(crossMember2, FeatureSubject)
			if err != nil || state.Value != "" {
				t.Fatalf("selection granted member ratification: %+v %v", state, err)
			}
			if err := authority.Close(); err != nil {
				t.Fatal(err)
			}
			open = false
			required, err = r2.GroupApprovalRequired(crossMember2)
			if err != nil || required != multiple {
				t.Fatalf("cold authority approval requirement: %v %v", required, err)
			}
		})
	}
}
