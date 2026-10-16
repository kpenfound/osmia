package questions_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestSharedChiefAssumptionToolValidatesAffectedWorkstreams(t *testing.T) {
	t.Parallel()
	f := setupCrossRepo(t)
	wg, g, err := trace.CreateWorkgroupTrace(context.Background(), f.root, []trace.CrossRepoMember{{Workstream: crossMember1, Project: f.r1}, {Workstream: crossMember2, Project: f.r2}}, start, owner, "workgroup")
	if err != nil {
		t.Fatal(err)
	}
	defer wg.Close()
	host := g.Members[0]
	scope := claimCrossRepoTurn(t, wg, wg.Project(), host, trace.ChiefOfStaff, trace.ChiefOfStaff, "", "assumptions")
	tools, err := questions.Tools(wg, trace.ChiefOfStaff, scope, func() time.Time { return start })
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools {
		if tool.Name != questions.LogAssumptionTool {
			continue
		}
		input := fmt.Sprintf(`{"text":"Reuse the retry policy.","workstreams":[%q]}`, crossMember2)
		result := callTool(t, tool, input)
		if !strings.Contains(result, `"recorded":true`) {
			t.Fatalf("valid member refused: %s", result)
		}
		if again := callTool(t, tool, input); result != again {
			t.Fatalf("replay changed the entry: %s => %s", result, again)
		}
		for _, selector := range []string{
			`[]`, `["w_00000000000000000000000000000099"]`, fmt.Sprintf(`[%q,%q]`, crossMember1, crossMember1),
		} {
			result := callTool(t, tool, `{"text":"Invalid selection.","workstreams":`+selector+`}`)
			if !strings.Contains(result, `"recorded":false`) {
				t.Fatalf("invalid selector %s accepted: %s", selector, result)
			}
		}
		docs, err := trace.Read[trace.Document](wg, host)
		if err != nil {
			t.Fatal(err)
		}
		count := 0
		for _, doc := range docs {
			if doc.Path == trace.AssumptionsPath {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("replay or invalid selector wrote another revision: %d", count)
		}
		return
	}
	t.Fatal("shared chief has no log_assumption tool")
}
