package delivery

import (
	"context"
	"flag"
	"os"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

const stream = fixturekit.Stream

func TestMain(m *testing.M) {
	if !flag.Parsed() {
		flag.Parse()
	}
	fixturekit.ApplyServiceTestDefaults(flag.CommandLine)
	os.Exit(m.Run())
}

func deliveryTrace(t *testing.T) (*config.Config, *trace.Repository) {
	t.Helper()
	_, cfg := fixturekit.Conversation(t, "delivery-")
	repo, err := trace.Open(cfg.Root, cfg.Project)
	fixturekit.Must(t, err)
	t.Cleanup(func() { repo.Close() })
	var docs []trace.Document
	for _, d := range []struct{ id, path, content string }{{plan.SpecDocument, plan.SpecPath, intakefixture.ValidSpec}, {plan.PlanDocument, plan.PlanPath, intakefixture.ValidPlan}} {
		docs = append(docs, trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: d.id, Revision: 1, Project: repo.Project(), Workstream: stream, At: fixturekit.DemoStart, Actor: ownerActor, Cause: "fixture"}, Path: d.path, Content: d.content})
	}
	fixturekit.Must(t, repo.RecordDocuments(context.Background(), docs))
	return cfg, repo
}
