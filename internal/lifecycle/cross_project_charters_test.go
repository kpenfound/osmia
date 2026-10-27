package lifecycle_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestWorkgroupSpecDrafterReceivesEveryMemberCharter(t *testing.T) {
	f := newCrossRepoRatifyFixture(t)
	defer f.stop(t)
	checked := make(chan struct{}, 1)
	f.engine.Mu.Lock()
	original := f.engine.Turns["project-spec-draft-1"]
	f.engine.Turns["project-spec-draft-1"] = func(ctx context.Context, req agent.Request, turn *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		for _, m := range []struct {
			project config.ProjectID
			stream  config.WorkstreamID
		}{{f.project, f.stream1}, {f.project2, f.stream2}} {
			data, err := os.ReadFile(filepath.Join(req.Grants.Mounts[0].Path, "members", string(m.stream), "charter.md"))
			if err != nil {
				t.Errorf("shared charter view: %v", err)
			}
			if !strings.Contains(string(data), string(m.project)+"/charter#<n>") || !strings.Contains(string(data), "1.") {
				t.Errorf("shared drafter missing charter of %s; charter %s", m.project, data)
			}
		}
		checked <- struct{}{}
		return original(ctx, req, turn, tools)
	}
	f.engine.Mu.Unlock()
	_, api := f.s.DraftWorkgroup(context.Background(), []config.WorkstreamID{f.stream1, f.stream2})
	if api != nil {
		t.Fatal(api)
	}
	f.awaitProjectSpec(t)
	select {
	case <-checked:
	default:
		t.Fatal("shared drafter did not inspect member charters")
	}
}

func TestWorkgroupCommitteeReceivesEveryMemberCharter(t *testing.T) {
	f := newProductionDesignFixture(t)
	defer f.stop(t)
	checked := make(chan struct{}, 4)
	for i := 1; i <= 2; i++ {
		turnID := intake.RoundTurnID(1, intake.CommitteeAgent(i), 1)
		f.engine.Mu.Lock()
		original := f.engine.Turns[turnID]
		f.engine.Turns[turnID] = func(ctx context.Context, req agent.Request, turn *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
			text, err := readTool(ctx, tools, "context.md")
			if err != nil {
				return nil, err
			}
			for _, project := range []config.ProjectID{f.project, f.project2} {
				if !strings.Contains(text, string(project)+"/charter#1") {
					t.Errorf("committee context missing charter of %s: %s", project, text)
				}
			}
			if !strings.Contains(text, "workgroup spec and contracts must satisfy every charter") {
				t.Error("committee context lacks shared charter governance")
			}
			checked <- struct{}{}
			return original(ctx, req, turn, tools)
		}
		f.engine.Mu.Unlock()
	}
	f.startOwner(t)
	f.awaitPacket(t)
	if len(checked) != 4 {
		t.Fatalf("reviewed member turns = %d, want 4", len(checked))
	}
}

func TestSharedDeltaRefusesInvalidCharterCitations(t *testing.T) {
	f, stream, repo := newApprovedFixture(t, "delta-charters")
	defer repo.Close()
	f.s.setSole(runtimeFor(repo))
	ctx := context.Background()
	_, err := trace.EnsureWorkgroups(ctx, repo, []config.WorkstreamID{stream}, f.s.now(), serviceActor, "fixture")
	must(t, err)
	change, api := f.s.requestChange(ctx, string(stream), ChangeRequest{Key: "charter-delta", Kind: "amend", Scope: "workgroup", Change: "Clarify the shared behavior"})
	if api != nil {
		t.Fatal(api)
	}
	change.State = "drafting"
	must(t, f.s.saveChange(ctx, repo, &change))
	tool := f.s.deltaTool(repo, change, architectAgent, deltaDraftTool)
	for _, field := range []string{"behavior", "contract"} {
		for _, citation := range []string{"charter#999", "p_missing/charter#1"} {
			after := change.Before
			if field == "behavior" {
				after.Behavior = "Follow " + citation
			} else {
				after.Contracts = []trace.Contract{{Name: "journal", Text: "Follow " + citation}}
			}
			raw, err := json.Marshal(after)
			must(t, err)
			out, err := tool.Handle(ctx, raw)
			must(t, err)
			if !strings.Contains(string(out), `"recorded":false`) || !strings.Contains(string(out), citation) {
				t.Fatalf("invalid %s citation %s accepted: %s", field, citation, out)
			}
			latest := currentChange(t, f.s, stream, change.ID)
			if latest.State != "drafting" || latest.Revision != change.Revision {
				t.Fatalf("refused citation advanced the amendment: %+v", latest)
			}
		}
	}
	after := change.Before
	after.Behavior = "Follow charter#1."
	raw, err := json.Marshal(after)
	must(t, err)
	out, err := tool.Handle(ctx, raw)
	must(t, err)
	if !strings.Contains(string(out), `"recorded":true`) || currentChange(t, f.s, stream, change.ID).State != "debating" {
		t.Fatalf("valid charter citation refused: %s", out)
	}
}

func TestCrossProjectSharedDeltaCharterCitations(t *testing.T) {
	f := newConfiguredProductionDesignFixture(t, productionDesignConfiguration{options: func(opts *Options) {
		opts.Reconciliation.Hold = func(_ config.WorkstreamID, op coreadapter.Operation) bool { return op.Action == deltaTurnAction }
	}})
	defer f.stop(t)
	ctx := context.Background()
	f.startOwner(t)
	packet := f.awaitPacket(t)
	_, err := f.c.Ratify(ctx, f.origin, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan, packet.ProjectSpecRevision)
	must(t, err)
	f.awaitApproved(t)
	writeCharter(t, f.c, f.project, "1. Keep changes small.\n\n2. A alone has this rule.\n")
	writeCharter(t, f.c, f.project2, "1. Keep changes small.\n\n3. B alone has this rule.\n")
	change, api := f.s.requestChange(ctx, string(f.stream2), ChangeRequest{Key: "qualified-delta", Kind: "amend", Scope: "workgroup", Change: "Clarify the shared rules"})
	if api != nil {
		t.Fatal(api)
	}
	wt, _, _, close, err := f.s.changeContext(ctx, string(f.stream2))
	must(t, err)
	defer close()
	tool := f.s.deltaTool(wt, change, architectAgent, deltaDraftTool)
	for _, citation := range []string{"charter#2", string(f.project) + "/charter#3", "p_missing/charter#1"} {
		after := change.Before
		after.Behavior = "Follow " + citation
		raw, err := json.Marshal(after)
		must(t, err)
		out, err := tool.Handle(ctx, raw)
		must(t, err)
		if !strings.Contains(string(out), `"recorded":false`) || !strings.Contains(string(out), citation) {
			t.Fatalf("invalid citation %s accepted: %s", citation, out)
		}
		latest := currentChange(t, f.s, f.stream2, change.ID)
		if latest.State != change.State || latest.Revision != change.Revision {
			t.Fatalf("refusal changed journal: %+v", latest)
		}
	}
	after := change.Before
	after.Behavior = "Follow charter#3 and " + string(f.project) + "/charter#2."
	after.Contracts = append([]trace.Contract(nil), after.Contracts...)
	after.Contracts[0].Text += " Follow " + string(f.project) + "/charter#2."
	raw, err := json.Marshal(after)
	must(t, err)
	out, err := tool.Handle(ctx, raw)
	must(t, err)
	if !strings.Contains(string(out), `"recorded":true`) || currentChange(t, f.s, f.stream2, change.ID).State != "debating" {
		t.Fatalf("valid local and qualified citations refused: %s", out)
	}
}
