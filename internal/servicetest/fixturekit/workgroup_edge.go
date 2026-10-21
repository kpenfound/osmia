package fixturekit

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
)

func CrossRepoEdgeDependent(t *testing.T, cfg *config.Config, clone string, repository *trace.Repository, stream config.WorkstreamID, now func() time.Time, owner, registration trace.Actor, branch func(config.WorkstreamID) string, bind func(config.Project, *trace.Repository), draft func(context.Context, []config.WorkstreamID) (config.WorkgroupID, *api.APIError)) (*trace.Repository, config.WorkstreamID) {
	t.Helper()
	ctx := context.Background()

	home := filepath.Dir(clone)
	clone2 := filepath.Join(home, "other")
	Must(t, os.MkdirAll(clone2, 0700))
	otherID, err := config.NewProjectID()
	Must(t, err)
	p2 := config.Project{ID: otherID, Version: 1, Name: "other", Upstream: "owner/other", Fork: "owner/other-fork", Clone: clone2, BaseBranch: "main"}
	repo2, err := trace.Create(ctx, cfg.Root, p2, now(), owner)
	Must(t, err)
	t.Cleanup(func() { repo2.Close() })
	upstream := config.WorkstreamID("w_000000000000000000000000000000c1")
	Must(t, repo2.CreateWorkstream(ctx, upstream, now(), registration))

	bind(p2, repo2)

	workgroupID, api := draft(ctx, []config.WorkstreamID{stream, upstream})
	if api != nil {
		t.Fatalf("draft workgroup: %v", api)
	}
	wt, err := trace.OpenWorkgroupTrace(cfg.Root, workgroupID, []trace.CrossRepoMember{{Workstream: upstream, Project: repo2}, {Workstream: stream, Project: repository}})
	Must(t, err)
	spec := trace.ProjectSpec{Behavior: "Downstream depends on upstream.", Contracts: []trace.Contract{{Name: "dependency", Text: "Use the upstream interface."}}, Graph: []trace.Edge{{From: upstream, To: stream, Type: trace.EdgeMergeOrder}}}

	prepared, _, found, err := seal.PreparedLatest(repository, stream)
	Must(t, err)
	if !found {
		t.Fatal("dependent fixture has no seal")
	}
	specDoc, err := trace.Get[trace.Document](repository, stream, plan.SpecDocument, prepared.Revision.Spec)
	Must(t, err)
	planDoc, err := trace.Get[trace.Document](repository, stream, plan.PlanDocument, prepared.Revision.Plan)
	Must(t, err)
	for _, doc := range []trace.Document{specDoc, planDoc} {
		doc.Project, doc.Workstream, doc.Actor, doc.Cause = repo2.Project(), upstream, owner, "fixture"
		Must(t, repo2.RecordDocuments(ctx, []trace.Document{doc}))
	}
	upstreamSeal := prepared
	upstreamSeal.Branch, upstreamSeal.Workspace = branch(upstream), clone2
	encoded, err := seal.Encode(upstreamSeal)
	Must(t, err)
	Must(t, repo2.RecordDocuments(ctx, []trace.Document{{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: seal.DocumentID, Revision: 1, Project: repo2.Project(), Workstream: upstream, At: now(), Actor: owner, Cause: "fixture"}, Path: seal.Path, Content: string(encoded)}}))
	approval := trace.GroupApproval{Workgroup: workgroupID, Complete: true}
	for _, member := range []trace.CrossRepoMember{{Workstream: stream, Project: repository}, {Workstream: upstream, Project: repo2}} {
		spec.Slices = append(spec.Slices, trace.Slice{Workstream: member.Workstream, Spec: specDoc.Content, Plan: planDoc.Content})
		record, err := shed.EncodeRatification(shed.Ratify(prepared.Round, prepared.Revision, nil))
		Must(t, err)
		approval.Members = append(approval.Members, trace.GroupMemberApproval{Project: member.Project.Project(), Workstream: member.Workstream, Spec: prepared.Revision.Spec, Plan: prepared.Revision.Plan, Round: prepared.Round, Record: record})
	}
	approval.ProjectSpec = spec
	Must(t, trace.RecordProjectSpec(ctx, wt, trace.Workgroup{ID: workgroupID, Members: []config.WorkstreamID{stream, upstream}}, spec, now(), owner, "fixture"))
	groups, _, err := trace.Workgroups(wt)
	Must(t, err)
	Must(t, trace.RecordGroupApproval(ctx, wt, groups[0].Members[0], approval, now(), owner))
	Must(t, wt.Close())
	return repo2, upstream
}
func UpstreamOwnPullRequest(t *testing.T, repo2 *trace.Repository, upstream config.WorkstreamID, publication api.DeliveryPublication, at time.Time, actor trace.Actor, publicationID, publicationFile string) {
	t.Helper()
	ctx := context.Background()
	data, err := json.Marshal(publication)
	Must(t, err)
	h := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: publicationID, Revision: 1, Project: repo2.Project(), Workstream: upstream, At: at, Actor: actor, Cause: "fixture"}
	Must(t, repo2.RecordDocuments(ctx, []trace.Document{{Header: h, Path: publicationFile, Content: string(data)}}))
	deliver := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "deliver", Revision: 1, Project: repo2.Project(), Workstream: upstream, At: at, Actor: actor, Cause: "fixture"}
	_, err = repo2.SetFeatureState(ctx, deliver, "delivered", "fixture delivery")
	Must(t, err)
}
