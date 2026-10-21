package fixturekit

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
)

func SeedPage(t *testing.T, cfg *config.Config) time.Time {
	t.Helper()
	ctx := context.Background()
	at := time.Now().UTC()
	header := func(schema, id string, revision int) trace.Header {
		return trace.Header{Schema: schema, Version: trace.Version, ID: id, Revision: revision, Project: Project, Workstream: Stream, At: at, Actor: trace.Actor{Kind: "owner", ID: "local"}, Cause: "planted"}
	}

	repo, err := trace.Open(cfg.Root, cfg.Project)
	Must(t, err)
	units := []struct{ id, state string }{{"resume", "merged"}, {"upload", "implementing"}, {"audit", "ready"}, {"index", "planned"}}
	var graph strings.Builder
	graph.WriteString(`{"version": 1, "units": [`)
	for i, u := range units {
		if i > 0 {
			graph.WriteString(", ")
		}
		fmt.Fprintf(&graph, `{"id": %q, "title": "Build %s", "addresses": [{"criterion": "spec#1", "proof": {"kind": "new-test", "name": "TestWork"}}], "depends_on": [], "footprint": []}`, u.id, u.id)
	}
	graph.WriteString("]}\n")
	_, err = plan.Parse([]byte(graph.String()))
	Must(t, err)
	sealed, err := seal.Encode(seal.Seal{Version: seal.Version, Seal: 1, Round: 1, Revision: shed.Pin{Spec: 1, Plan: 1}, SpecHash: seal.SpecHash(ValidSpec),
		Base: seal.Base{Remote: "upstream", Branch: "main", Commit: "0123456789abcdef0123456789abcdef01234567"}, Branch: "osmia/" + string(Stream), Workspace: "/planted"})
	Must(t, err)
	Must(t, repo.RecordDocuments(ctx, []trace.Document{
		{Header: header("osmia.trace.document", plan.PlanDocument, 1), Path: plan.PlanPath, Content: graph.String()},
		{Header: header("osmia.trace.document", seal.DocumentID, 1), Path: seal.Path, Content: string(sealed)},
	}))
	_, err = repo.SetFeatureState(ctx, header("osmia.trace.transition", "planted-building", 1), "building", "planted")
	Must(t, err)
	for _, u := range units {
		subject := trace.UnitSubject(u.id)
		state, err := repo.Workflow(Stream, subject)
		Must(t, err)
		h := header("osmia.trace.transition", subject+"-"+u.state, 1)
		h.Unit = u.id
		_, err = repo.Transact(ctx, trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: h, Subject: subject, From: state.Value, To: u.state, Reason: "planted"}})
		Must(t, err)
	}
	Must(t, repo.Close())

	return at
}
