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

	wire "github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/projectstore"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
)

func packetWorkgroup(t *testing.T) (service.Options, *config.Config, trace.Workgroup) {
	t.Helper()
	opts := fixture(t)
	const other = "p_00000000000000000000000000000002"
	root := opts.Config.Root
	register(t, root, other, fmt.Sprintf("version=1\nupstream=\"owner/other\"\nfork=\"owner/other-fork\"\nclone=%q\n", filepath.Join(filepath.Dir(root), "other-clone")))
	cfg, err := projectstore.LoadConfig(opts.Config)
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

type packetRoot struct {
	root        string
	cfg         *config.Config
	group       trace.Workgroup
	service     *service.Service
	stopped     bool
	first       string
	old         wire.PacketResponse
	oldCommand  []string
	updatedSpec string
	refreshed   wire.PacketResponse
}

func startPacketRoot(t *testing.T) *packetRoot {
	t.Helper()
	opts, cfg, group := packetWorkgroup(t)
	s, err := service.Start(t.Context(), opts)
	must(t, err)
	p := &packetRoot{root: opts.Config.Root, cfg: cfg, group: group, service: s}
	t.Cleanup(p.stop)
	p.first = successful(t, p.root, "packet", stream)
	p.oldCommand = packetRatifyCommand(t, p.first)
	must(t, json.Unmarshal([]byte(successful(t, p.root, "packet", stream, "--json")), &p.old))
	return p
}

func (p *packetRoot) stop() {
	if !p.stopped {
		p.stopped = true
		p.service.Close()
	}
}

func (p *packetRoot) refreshFromAnotherMember(t *testing.T) {
	t.Helper()
	edit := filepath.Join(p.root, "edited-spec.md")
	p.updatedSpec = strings.Replace(shedSpec, "# Uploads", "# Uploads\n\nReport progress while resuming.", 1)
	must(t, os.WriteFile(edit, []byte(p.updatedSpec), 0600))
	successful(t, p.root, "edit", "spec", string(p.group.Members[2]), "--from", edit)
	c := service.NewClient(filepath.Join(p.root, "osmia.sock"))
	defer c.Close()
	eventually(t, 30*time.Second, func() (bool, string) {
		var err error
		p.refreshed, err = c.Packet(t.Context(), stream)
		must(t, err)
		return p.refreshed.ProjectSpecRevision == 2, fmt.Sprintf("refresh did not land: %+v", p.refreshed)
	})
}

func (p *packetRoot) refuseStaleRatifications(t *testing.T) {
	t.Helper()
	for _, command := range [][]string{p.oldCommand, {"ratify", stream}} {
		code, out, diag := invoke(t, p.root, command...)
		if code != 5 || out != "" || !strings.Contains(strings.ToLower(diag), "read the packet again") {
			t.Fatalf("stale or missing project revision approved: %v: %d %q %q", command, code, out, diag)
		}
	}
}

func (p *packetRoot) ratifyRereadPacket(t *testing.T) string {
	t.Helper()
	return successful(t, p.root, packetRatifyCommand(t, successful(t, p.root, "packet", stream))...)
}

func (p *packetRoot) groupApproval(t *testing.T) trace.GroupApproval {
	t.Helper()
	p.stop()
	wt, err := trace.OpenWorkgroupAuthority(p.cfg.Root, p.group.ID)
	must(t, err)
	defer wt.Close()
	approval, _, found, err := trace.ReadGroupApproval(wt, p.group.Members[0])
	must(t, err)
	if !found {
		t.Fatal("no group approval was recorded")
	}
	return approval
}

func TestPacketShowsTheWorkgroupSpecAndEveryMembersDocuments(t *testing.T) {
	p := startPacketRoot(t)
	for _, displayed := range []string{"Workgroup spec revision 1", "Uploads resume across the service and client.", "Contract resume: Persist the acknowledged offset.", "contract criteria [1]", shedSpec, shedPlan} {
		if !strings.Contains(p.first, displayed) {
			t.Fatalf("packet omitted %q: %s", displayed, p.first)
		}
	}
}

func TestPacketJSONPinsTheProjectSpecAndMemberRevisions(t *testing.T) {
	p := startPacketRoot(t)
	if p.old.ProjectSpecRevision != 1 || p.old.Packet.Revision != (shed.Pin{Spec: 1, Plan: 1}) {
		t.Fatalf("first packet: %+v", p.old)
	}
}

func TestAnotherMembersRefreshLeavesThisMembersPinsAlone(t *testing.T) {
	p := startPacketRoot(t)
	p.refreshFromAnotherMember(t)
	if p.refreshed.Packet.Revision != p.old.Packet.Revision {
		t.Fatalf("editing another member changed this member's pins: %+v", p.refreshed.Packet.Revision)
	}
}

func TestRatifyRefusesAProjectRevisionThatIsStaleOrMissing(t *testing.T) {
	p := startPacketRoot(t)
	p.refreshFromAnotherMember(t)
	p.refuseStaleRatifications(t)
}

func TestRefusedRatificationRecordsNoApprovalRatificationOrSeal(t *testing.T) {
	p := startPacketRoot(t)
	p.refreshFromAnotherMember(t)
	p.refuseStaleRatifications(t)
	p.stop()
	wt, err := trace.OpenWorkgroupAuthority(p.cfg.Root, p.group.ID)
	must(t, err)
	_, _, found, err := trace.ReadGroupApproval(wt, p.group.Members[0])
	must(t, err)
	if found {
		t.Fatal("refused CLI decisions recorded a group approval")
	}
	must(t, wt.Close())
	for i, project := range p.cfg.Projects {
		repo, err := trace.Open(p.cfg.Root, project)
		must(t, err)
		docs, err := trace.Read[trace.Document](repo, p.group.Members[i+1])
		must(t, err)
		for _, doc := range docs {
			if doc.Path == shed.RatificationPath(1) || doc.Path == seal.Path {
				t.Fatalf("refused CLI decisions recorded %s", doc.Path)
			}
		}
		_, _, found, err = seal.PreparedLatest(repo, p.group.Members[i+1])
		must(t, err)
		if found {
			t.Fatal("refused CLI decisions prepared a seal")
		}
		must(t, repo.Close())
	}
}

func TestRereadPacketShowsTheRefreshedSlice(t *testing.T) {
	p := startPacketRoot(t)
	p.refreshFromAnotherMember(t)
	second := successful(t, p.root, "packet", stream)
	if !strings.Contains(second, "Workgroup spec revision 2") || !strings.Contains(second, p.updatedSpec) {
		t.Fatalf("reread did not show the refreshed slice: %s", second)
	}
}

func TestRereadPacketNamesTheCommandThatPinsTheDisplayedRevisions(t *testing.T) {
	p := startPacketRoot(t)
	p.refreshFromAnotherMember(t)
	command := packetRatifyCommand(t, successful(t, p.root, "packet", stream))
	if !reflect.DeepEqual(command, []string{"ratify", stream, "--project-spec-revision", strconv.Itoa(p.refreshed.ProjectSpecRevision), "--spec-revision", "1", "--plan-revision", "1"}) {
		t.Fatalf("reread command: %v", command)
	}
}

func TestRatifyWithTheRereadCommandNamesTheApprovedProjectRevision(t *testing.T) {
	p := startPacketRoot(t)
	p.refreshFromAnotherMember(t)
	if out := p.ratifyRereadPacket(t); !strings.Contains(out, "Workgroup spec revision 2 approved.") {
		t.Fatalf("ratify omitted the approved project revision: %s", out)
	}
}

func TestGroupApprovalPinsTheRereadProjectSpec(t *testing.T) {
	p := startPacketRoot(t)
	p.refreshFromAnotherMember(t)
	p.ratifyRereadPacket(t)
	approval := p.groupApproval(t)
	if approval.ProjectSpecRevision != 2 || !reflect.DeepEqual(approval.ProjectSpec, *p.refreshed.ProjectSpec) {
		t.Fatalf("approval did not pin the reread project spec: %+v", approval)
	}
}

func TestGroupApprovalPinsTheDisplayedMemberRevisions(t *testing.T) {
	p := startPacketRoot(t)
	p.refreshFromAnotherMember(t)
	p.ratifyRereadPacket(t)
	approval := p.groupApproval(t)
	if len(approval.Members) != 2 {
		t.Fatalf("approval lost members: %+v", approval)
	}
	for _, member := range approval.Members {
		wantSpec := 1
		if member.Workstream == p.group.Members[2] {
			wantSpec = 2
		}
		if member.Spec != wantSpec || member.Plan != 1 {
			t.Fatalf("approval did not pin the displayed member revisions: %+v", member)
		}
	}
}
