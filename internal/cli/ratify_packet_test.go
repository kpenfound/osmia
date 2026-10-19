package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
)

// packetWorkgroup seeds two real members at their decision point without
// running any model or delivery operation.
func packetWorkgroup(t *testing.T) (service.Options, *config.Config, trace.Workgroup) {
	t.Helper()
	opts := fixture(t)
	const other = "p_00000000000000000000000000000002"
	root := opts.Config.Root
	path := filepath.Join(root, "config.toml")
	content, err := os.ReadFile(path)
	must(t, err)
	must(t, os.WriteFile(path, []byte(strings.Replace(string(content), fmt.Sprintf("active_projects=[%q]", project), fmt.Sprintf("active_projects=[%q,%q]", project, other), 1)), 0600))
	must(t, os.MkdirAll(filepath.Join(root, "projects", other), 0700))
	must(t, os.WriteFile(filepath.Join(root, "projects", other, "config.toml"), []byte(fmt.Sprintf("version=1\nupstream=\"owner/other\"\nfork=\"owner/other-fork\"\nclone=%q\n", filepath.Join(filepath.Dir(root), "other-clone"))), 0600))
	cfg, err := config.Load(opts.Config)
	must(t, err)
	ctx := context.Background()
	actor := trace.Actor{Kind: "owner", ID: "local"}
	var members []trace.CrossRepoMember
	for i, p := range cfg.Projects {
		id := config.WorkstreamID(stream)
		if i == 1 {
			id = "w_00000000000000000000000000000002"
		}
		repo, err := trace.CreateSeeded(ctx, cfg.Root, p, written, actor, []byte(`{"version":1,"entities":[{"id":"internal.store","name":"store","paths":["internal/store/**"]}]}`))
		must(t, err)
		defer repo.Close()
		must(t, repo.CreateWorkstream(ctx, id, written, actor))
		packet, err := shed.EncodePacket(shed.Present(1, shed.Pin{Spec: 1, Plan: 1}, false, "Debate settled.", nil))
		must(t, err)
		document := func(idoc, path, text string) trace.Document {
			return trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: idoc, Revision: 1, Project: p.ID, Workstream: id, At: written, Actor: actor, Cause: "draft"}, Path: path, Content: text}
		}
		must(t, repo.RecordDocuments(ctx, []trace.Document{document(plan.SpecDocument, plan.SpecPath, shedSpec), document(plan.PlanDocument, plan.PlanPath, shedPlan), document(shed.PacketDocumentID(1), shed.PacketPath(1), string(packet))}))
		h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "in-shed", Revision: 1, Project: p.ID, Workstream: id, At: written, Actor: actor, Cause: "debate"}
		_, err = repo.SetFeatureState(ctx, h, service.InShedState, "Debate settled.")
		must(t, err)
		h.ID = "shed-concluded-1"
		_, err = repo.Transact(ctx, trace.Transaction{Transition: trace.Transition{Header: h, Subject: "shed", To: "concluded-1", Reason: "Debate settled."}})
		must(t, err)
		members = append(members, trace.CrossRepoMember{Workstream: id, Project: repo})
	}
	wt, group, err := trace.CreateWorkgroupTrace(ctx, cfg.Root, members, written, actor, "draft")
	must(t, err)
	defer wt.Close()
	spec := trace.ProjectSpec{Behavior: "Uploads resume across the service and client.", Contracts: []trace.Contract{{Name: "resume", Text: "Persist the acknowledged offset."}}, Graph: []trace.Edge{{From: members[0].Workstream, To: members[1].Workstream, Type: trace.EdgeContract}}}
	for _, m := range members {
		must(t, m.Project.JoinCrossRepoWorkgroup(ctx, m.Workstream, group.ID, written, actor, "draft"))
		spec.Slices = append(spec.Slices, trace.Slice{Workstream: m.Workstream, Spec: shedSpec, Plan: shedPlan, ContractCriteria: []int{1}})
	}
	must(t, trace.RecordProjectSpec(ctx, wt, group, spec, written, actor, "draft"))
	return opts, cfg, group
}

func packetRatifyCommand(t *testing.T, text string) []string {
	t.Helper()
	for _, line := range strings.Split(text, "\n") {
		if command, ok := strings.CutPrefix(line, "After reading, ratify with: osmia "); ok {
			return strings.Fields(command)
		}
	}
	t.Fatalf("packet has no decision command: %s", text)
	return nil
}

func TestPacketReadBindsCLIRatificationAcrossOtherMemberRefresh(t *testing.T) {
	opts, cfg, group := packetWorkgroup(t)
	ctx := context.Background()
	s, err := service.Start(ctx, opts)
	must(t, err)
	defer func() { s.Close() }()
	root := opts.Config.Root
	first := successful(t, root, "packet", stream)
	for _, displayed := range []string{"Project spec revision 1", "Uploads resume across the service and client.", "Contract resume: Persist the acknowledged offset.", "contract criteria [1]", shedSpec, shedPlan} {
		if !strings.Contains(first, displayed) {
			t.Fatalf("packet omitted %q: %s", displayed, first)
		}
	}
	oldCommand := packetRatifyCommand(t, first)
	var old service.PacketResponse
	must(t, json.Unmarshal([]byte(successful(t, root, "packet", stream, "--json")), &old))
	if old.ProjectSpecRevision != 1 || old.Packet.Revision != (shed.Pin{Spec: 1, Plan: 1}) {
		t.Fatalf("first packet: %+v", old)
	}
	// Only B changes; A's spec and plan pins remain the ones the owner read.
	edit := filepath.Join(root, "edited-spec.md")
	updatedSpec := strings.Replace(shedSpec, "# Uploads", "# Uploads\n\nReport progress while resuming.", 1)
	must(t, os.WriteFile(edit, []byte(updatedSpec), 0600))
	successful(t, root, "edit", "spec", string(group.Members[2]), "--from", edit)
	c := service.NewClient(filepath.Join(root, "osmia.sock"))
	defer c.Close()
	deadline := time.Now().Add(30 * time.Second)
	var refreshed service.PacketResponse
	for {
		refreshed, err = c.Packet(ctx, stream)
		must(t, err)
		if refreshed.ProjectSpecRevision == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("refresh did not land: %+v", refreshed)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if refreshed.Packet.Revision != old.Packet.Revision {
		t.Fatal("editing B changed A's pins")
	}
	for _, command := range [][]string{oldCommand, {"ratify", stream}} {
		code, out, diag := invoke(t, root, command...)
		if code != 5 || out != "" || !strings.Contains(strings.ToLower(diag), "read the packet again") {
			t.Fatalf("stale or missing project revision approved: %v: %d %q %q", command, code, out, diag)
		}
	}
	must(t, s.Close())
	wt, err := trace.OpenWorkgroupAuthority(cfg.Root, group.ID)
	must(t, err)
	_, _, found, err := trace.ReadGroupApproval(wt, group.Members[0])
	must(t, err)
	if found {
		t.Fatal("refused CLI decisions recorded a group approval")
	}
	must(t, wt.Close())
	for i, p := range cfg.Projects {
		repo, err := trace.Open(cfg.Root, p)
		must(t, err)
		docs, err := trace.Read[trace.Document](repo, group.Members[i+1])
		must(t, err)
		for _, doc := range docs {
			if doc.Path == shed.RatificationPath(1) || doc.Path == seal.Path {
				t.Fatalf("refused CLI decisions recorded %s", doc.Path)
			}
		}
		_, _, found, err = seal.PreparedLatest(repo, group.Members[i+1])
		must(t, err)
		if found {
			t.Fatal("refused CLI decisions prepared a seal")
		}
		must(t, repo.Close())
	}
	// An explicit reread supplies P2 and succeeds, including across a restart.
	s, err = service.Start(ctx, opts)
	must(t, err)
	second := successful(t, root, "packet", stream)
	if !strings.Contains(second, "Project spec revision 2") || !strings.Contains(second, updatedSpec) {
		t.Fatalf("reread did not show the refreshed slice: %s", second)
	}
	command := packetRatifyCommand(t, second)
	if !reflect.DeepEqual(command, []string{"ratify", stream, "--project-spec-revision", strconv.Itoa(refreshed.ProjectSpecRevision), "--spec-revision", "1", "--plan-revision", "1"}) {
		t.Fatalf("reread command: %v", command)
	}
	if out := successful(t, root, command...); !strings.Contains(out, "Project spec revision 2 approved.") {
		t.Fatalf("ratify omitted the approved project revision: %s", out)
	}
	must(t, s.Close())
	wt, err = trace.OpenWorkgroupAuthority(cfg.Root, group.ID)
	must(t, err)
	defer wt.Close()
	approval, _, found, err := trace.ReadGroupApproval(wt, group.Members[0])
	must(t, err)
	if !found || approval.ProjectSpecRevision != 2 || !reflect.DeepEqual(approval.ProjectSpec, *refreshed.ProjectSpec) {
		t.Fatalf("approval did not pin the reread project spec: %+v", approval)
	}
	if len(approval.Members) != 2 {
		t.Fatalf("approval lost members: %+v", approval)
	}
	for _, member := range approval.Members {
		wantSpec := 1
		if member.Workstream == group.Members[2] {
			wantSpec = 2
		}
		if member.Spec != wantSpec || member.Plan != 1 {
			t.Fatalf("approval did not pin the displayed member revisions: %+v", member)
		}
	}
}
