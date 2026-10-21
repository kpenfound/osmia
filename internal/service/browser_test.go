package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

// pageFixture is a started service with a web listener whose trace holds
// stream building four units, with a running mason, a mason waiting for the
// one mason slot and a chief of staff in a turn that writes the status, and
// quiet, paused by the owner, with no status.
type pageFixture struct {
	s     *Service
	c     *Client
	chief coreadapter.Scope
}

func newPageFixture(t *testing.T) *pageFixture {
	t.Helper()
	ctx := context.Background()
	opts, cfg := conversationFixture(t, "pg-")
	configFile, err := os.OpenFile(filepath.Join(opts.Config.Root, "config.toml"), os.O_APPEND|os.O_WRONLY, 0)
	must(t, err)
	_, err = configFile.WriteString("[capacity]\nmasons = 1\n[listen]\nweb = \"127.0.0.1:0\"\n")
	must(t, err)
	must(t, configFile.Close())
	// The loop passes only when the workflow changes.
	opts.Reconciliation.Ticks = make(chan time.Time)
	at := fixturekit.SeedPage(t, cfg)

	s, c := start(t, opts)
	f := &pageFixture{s: s, c: c}
	// The service abandons claimed chief-of-staff turns when it starts, so
	// the turns are claimed once it runs.
	live := s.sole().repository
	_, err = live.EnsureChiefOfStaff(ctx, stream, at, serviceActor)
	must(t, err)
	f.chief = claimTurn(t, live, t.TempDir(), stream, trace.ChiefOfStaff, trace.ChiefOfStaff, "status", at)
	queueRoleTurn(t, live, stream, "agent_mason_upload", masonRole, "upload", at)
	_, err = live.ClaimTurn(ctx, stream, "agent_mason_upload", "token_upload", t.TempDir(), at)
	must(t, err)
	queueRoleTurn(t, live, stream, "agent_mason_audit", masonRole, "audit", at)
	// The loop does not pass, so the project's scheduler passes once to
	// publish the slot the upload's mason takes and the audit's turn waiting
	// for it.
	dispatch, err := s.dispatcher(s.about(live), live)
	must(t, err)
	must(t, dispatch.Pass(ctx))
	f.status(t, trace.StatusContent{Goal: "Ship resumable uploads.", Note: "A mason is building the upload unit.", Agents: []string{"A mason builds upload."}})
	mutation(t, c, "PUT", "pause", PauseRequest{Target: runtime.Target{Scope: "workstream", Project: project, Workstream: quiet}, Mode: "soft", Reason: "The owner is travelling", Source: runtime.PauseOwner})
	return f
}

// status writes the next status revision through the chief of staff's turn.
func (f *pageFixture) status(t *testing.T, content trace.StatusContent) {
	t.Helper()
	_, err := f.s.sole().repository.SetStatus(context.Background(), trace.ChiefOfStaff, f.chief, content, time.Now().UTC(), func(trace.StatusContent, []string, []trace.OwnerGate) error { return nil })
	must(t, err)
}
