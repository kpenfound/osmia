package questions_test

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestCrossRepoContractToolRoutesNonHostRequestToProject(t *testing.T) {
	for _, role := range []string{"mason", "reviewer"} {
		t.Run(role, func(t *testing.T) {
			f := setupCrossRepo(t)
			ctx := context.Background()
			wg, g, err := trace.CreateWorkgroupTrace(ctx, f.root, []trace.CrossRepoMember{
				{Workstream: crossMember1, Project: f.r1}, {Workstream: crossMember2, Project: f.r2},
			}, start, owner, "test-workgroup")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { wg.Close() })
			for _, member := range []struct {
				repo   *trace.Repository
				stream config.WorkstreamID
			}{{f.r1, crossMember1}, {f.r2, crossMember2}} {
				if err := member.repo.RecordCrossRepoWorkgroup(ctx, member.stream, g.ID, start, owner, "join"); err != nil {
					t.Fatal(err)
				}
			}
			spec := trace.ProjectSpec{Behavior: "The repos share a retry contract.", Slices: []trace.Slice{
				{Workstream: crossMember1, Spec: "spec.md", Plan: "plan.json"},
				{Workstream: crossMember2, Spec: "spec.md", Plan: "plan.json", ContractCriteria: []int{1}},
			}}
			if err := trace.RecordProjectSpec(ctx, wg, g, spec, start, owner, "ratified"); err != nil {
				t.Fatal(err)
			}
			header := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "building", Revision: 1, Project: crossProject2, Workstream: crossMember2, At: start, Actor: owner, Cause: "test"}
			if _, err := f.r2.SetFeatureState(ctx, header, "building", "test"); err != nil {
				t.Fatal(err)
			}
			header.ID, header.Unit = "implementing", "retry"
			state := "implementing"
			if role == "reviewer" {
				state = "reviewing"
			}
			if _, err := f.r2.Transact(ctx, trace.Transaction{Transition: trace.Transition{Header: header, Subject: trace.UnitSubject("retry"), To: state, Reason: "test"}}); err != nil {
				t.Fatal(err)
			}
			document := func(id, path, content string) trace.Document {
				return trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: id, Revision: 1, Project: crossProject2, Workstream: crossMember2, At: start, Actor: owner, Cause: "ratified"}, Path: path, Content: content}
			}
			if err := f.r2.RecordDocuments(ctx, []trace.Document{
				document("spec", "spec.md", "# Retries\n\n## Acceptance criteria\n\n1. Retries use a bounded window.\n"),
				document("seal", "seal.json", `{"seal":1,"spec_hash":"sha256:test"}`),
			}); err != nil {
				t.Fatal(err)
			}
			before, err := trace.Read[trace.Document](f.r2, crossMember2)
			if err != nil {
				t.Fatal(err)
			}
			scope := claimCrossRepoTurn(t, f.r2, crossProject2, crossMember2, "worker", role, "retry", "build")
			_, err = f.r2.FileAmendment(ctx, "worker", scope, trace.AmendmentRequest{
				Citations: []string{"spec#1"}, Change: "Widen the retry window", Reason: "The consumer needs more time",
				Seal: 1, SealRevision: 1, SpecHash: "sha256:test",
			}, start)
			if err == nil || !strings.Contains(err.Error(), "read-only inside a workstream") {
				t.Fatalf("local contract edit was not refused: %v", err)
			}
			workerTools, err := questions.Tools(f.r2, "worker", scope, func() time.Time { return start })
			if err != nil {
				t.Fatal(err)
			}
			find := func(tools []coreadapter.Tool, name string) coreadapter.Tool {
				for _, tool := range tools {
					if tool.Name == name {
						return tool
					}
				}
				t.Fatalf("missing tool %s", name)
				return coreadapter.Tool{}
			}
			ask := callTool(t, find(workerTools, questions.AskTool), `{"question":"May I widen the retry contract?"}`)
			if !strings.Contains(ask, `"question":"1"`) {
				t.Fatalf("ask: %s", ask)
			}
			chiefScope := claimCrossRepoTurn(t, wg, wg.Project(), g.Members[0], trace.ChiefOfStaff, trace.ChiefOfStaff, "", "chief")
			chiefTools, err := questions.Tools(wg, trace.ChiefOfStaff, chiefScope, func() time.Time { return start })
			if err != nil {
				t.Fatal(err)
			}
			input := `{"question":"` + string(crossMember2) + `:1","citations":["spec#1"],"change":"Widen the retry window","reason":"The consumer needs more time"}`
			route := find(chiefTools, questions.RouteAmendmentTool)
			result := callTool(t, route, input)
			if !strings.Contains(result, `"project_amendment":"1"`) || !strings.Contains(result, string(g.ID)) {
				t.Fatalf("contract routing: %s", result)
			}
			if again := callTool(t, route, input); again != result {
				t.Fatalf("retry changed routing: %s; first %s", again, result)
			}
			amendments, _, err := trace.ProjectAmendments(wg)
			if err != nil || len(amendments) != 1 || amendments[0].Origin != crossMember2 || amendments[0].Requester.ID != "worker" || amendments[0].Workgroup != g.ID {
				t.Fatalf("authoritative project amendment: %+v, %v", amendments, err)
			}
			for _, target := range []struct {
				repo   *trace.Repository
				stream config.WorkstreamID
			}{{f.r1, crossMember1}, {f.r2, crossMember2}, {wg, g.Members[0]}} {
				local, err := trace.Read[trace.Amendment](target.repo, target.stream)
				if err != nil || len(local) != 0 {
					t.Fatalf("contract escaped into a local amendment: %+v, %v", local, err)
				}
			}
			after, err := trace.Read[trace.Document](f.r2, crossMember2)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("routing changed the sealed slice: %+v, %v", after, err)
			}
			qs, err := f.r2.Questions(crossMember2)
			if err != nil || len(qs) != 1 || qs[0].State != trace.QuestionRouted {
				t.Fatalf("asking member question: %+v, %v", qs, err)
			}
		})
	}
}
