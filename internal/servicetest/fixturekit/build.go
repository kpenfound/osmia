package fixturekit

import (
	"context"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/kb"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
)

// BuildInput pins the trace and workspace identity of a complete sealed graph.
type BuildInput struct {
	Stream                                                config.WorkstreamID
	At                                                    time.Time
	Key, Graph, Spec, Branch, Workspace, Base, BaseBranch string
}

const buildingState = "building"
const unitPlanned = "planned"
const unitReady = "ready"

func RecordBuild(t *testing.T, repository *trace.Repository, in BuildInput) config.WorkstreamID {
	t.Helper()
	ctx := context.Background()
	stream, at := in.Stream, in.At
	p, err := plan.Parse([]byte(in.Graph))
	Must(t, err)
	entities, err := kb.Load(repository)
	Must(t, err)
	footprints, unresolved := seal.Take(p, entities)
	if len(unresolved) != 0 {
		t.Fatalf("fixture has unresolved footprints: %v", unresolved)
	}
	pin := shed.Pin{Spec: 1, Plan: 1}
	sealed, err := seal.Encode(seal.Seal{
		Version: seal.Version, Seal: 1, Round: 1, Revision: pin, SpecHash: seal.SpecHash(in.Spec),
		Base:   seal.Base{Remote: "upstream", Branch: in.BaseBranch, Commit: in.Base},
		Branch: in.Branch, Workspace: in.Workspace, Footprints: footprints,
	})
	Must(t, err)
	ratification, err := shed.EncodeRatification(shed.Ratify(1, pin, nil))
	Must(t, err)
	header := func(schema, id string) trace.Header {
		return trace.Header{Schema: schema, Version: trace.Version, ID: id, Revision: 1,
			Project: repository.Project(), Workstream: stream, At: at, Actor: OwnerActor, Cause: "fixture-" + in.Key}
	}
	document := func(id, path, content string) trace.Document {
		return trace.Document{Header: header("osmia.trace.document", id), Path: path, Content: content}
	}
	txs := []trace.Transaction{{Transition: trace.Transition{
		Header: header("osmia.trace.transition", "fixture-building"), Subject: trace.FeatureSubject,
		To: buildingState, Reason: "fixture with a sealed build",
	}}}
	for _, unit := range p.Units {
		subject := trace.UnitSubject(unit.ID)
		txs = append(txs, trace.Transaction{Transition: trace.Transition{
			Header: header("osmia.trace.transition", subject+"-planned"), Subject: subject,
			To: unitPlanned, Reason: "unit in the fixture plan",
		}})
		if len(unit.DependsOn) == 0 {
			txs = append(txs, trace.Transaction{ExpectedVersion: 1, Transition: trace.Transition{
				Header: header("osmia.trace.transition", subject+"-ready"), Subject: subject,
				From: unitPlanned, To: unitReady, Reason: "fixture unit with no dependencies",
			}})
		}
	}
	_, err = repository.RecordDocumentsWith(ctx, []trace.Document{
		document(plan.SpecDocument, plan.SpecPath, in.Spec),
		document(plan.PlanDocument, plan.PlanPath, in.Graph),
		document(shed.RatificationDocumentID(1), shed.RatificationPath(1), string(ratification)),
		document(seal.DocumentID, seal.Path, string(sealed)),
	}, txs...)
	Must(t, err)
	return stream
}
