package servicetest

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

// PageFixture starts a web listener with a four-unit build: one merged unit,
// one running mason, one mason waiting for capacity and one planned unit.
// Its chief of staff can publish status; a second workstream is owner-paused.
type PageFixture struct {
	S     *service.Service
	C     *service.Client
	Chief coreadapter.Scope
}

func NewPageFixture(t *testing.T) *PageFixture {
	t.Helper()
	ctx := context.Background()
	opts, cfg := ConversationFixture(t, "pg-")
	configFile, err := os.OpenFile(filepath.Join(opts.Config.Root, "config.toml"), os.O_APPEND|os.O_WRONLY, 0)
	fixturekit.Must(t, err)
	_, err = configFile.WriteString("[capacity]\nmasons = 1\n[listen]\nweb = \"127.0.0.1:0\"\n")
	fixturekit.Must(t, err)
	fixturekit.Must(t, configFile.Close())
	// The loop passes only when the workflow changes.
	opts.Reconciliation.Ticks = make(chan time.Time)
	at := fixturekit.SeedPage(t, cfg)

	s, c := Start(t, opts)
	f := &PageFixture{S: s, C: c}
	// The service abandons claimed chief-of-staff turns when it starts, so
	// the turns are claimed once it runs.
	live := service.InspectForTest(s).SoleRepository()
	_, err = live.EnsureChiefOfStaff(ctx, fixturekit.Stream, at, trace.Actor{Kind: "service", ID: "osmia"})
	fixturekit.Must(t, err)
	f.Chief = fixturekit.ClaimTurn(t, live, t.TempDir(), fixturekit.Stream, trace.ChiefOfStaff, trace.ChiefOfStaff, "status", at)
	fixturekit.QueueRoleTurn(t, live, fixturekit.Stream, "agent_mason_upload", "mason", "upload", at)
	_, err = live.ClaimTurn(ctx, fixturekit.Stream, "agent_mason_upload", "token_upload", t.TempDir(), at)
	fixturekit.Must(t, err)
	fixturekit.QueueRoleTurn(t, live, fixturekit.Stream, "agent_mason_audit", "mason", "audit", at)
	// Reconciliation is held, so the project's scheduler passes once to
	// publish the slot the upload's mason takes and the audit's turn waiting
	// for it.
	fixturekit.Must(t, service.InspectForTest(s).Schedule(ctx, live))
	f.Status(t, trace.StatusContent{Goal: "Ship resumable uploads.", Note: "A mason is building the upload unit.", Agents: []string{"A mason builds upload."}})
	Mutation(t, c, "PUT", "pause", api.PauseRequest{Target: runtime.Target{Scope: "workstream", Project: fixturekit.Project, Workstream: config.WorkstreamID("w_fedcba9876543210fedcba9876543210")}, Mode: "soft", Reason: "The owner is travelling", Source: runtime.PauseOwner})
	return f
}

// Status writes the next status revision through the chief of staff's turn.
func (f *PageFixture) Status(t *testing.T, content trace.StatusContent) {
	t.Helper()
	_, err := service.InspectForTest(f.S).SoleRepository().SetStatus(context.Background(), trace.ChiefOfStaff, f.Chief, content, time.Now().UTC(), func(trace.StatusContent, []string, []trace.OwnerGate) error { return nil })
	fixturekit.Must(t, err)
}
