package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/runtime"
)

func TestProviderLimitStatusAndOwnerClear(t *testing.T) {
	opts := fixture(t)
	path := filepath.Join(opts.Config.Root, "config.toml")
	data, err := os.ReadFile(path)
	must(t, err)
	must(t, os.WriteFile(path, []byte(strings.Replace(string(data), "model = \"test\"", "model = \"test\"\nfallback = \"other\"", 1)), 0600))
	s, client := start(t, opts)
	at := s.now()
	must(t, s.recordProviderLimit(coreadapter.Profile{Backend: "claude"}, coreadapter.ProviderLimit{Status: "blocked", Kind: "five_hour", ResetsAt: at.Add(time.Hour)}))
	status, err := client.Statuses(context.Background())
	must(t, err)
	if status.Profiles["mason"].Name != "other" || status.Profiles["mason"].Source != "provider_fallback" || len(status.ProviderLimits) != 1 {
		t.Fatalf("fallback status: %+v", status)
	}
	rt, err := client.Runtime(context.Background())
	must(t, err)
	if rt.Effective.Profiles["mason"] != "other" {
		t.Fatalf("runtime: %+v", rt)
	}
	must(t, s.store.SetProviderLimit(runtime.ProviderLimit{Backend: "codex", Status: "blocked", SetAt: at}))
	status, err = client.Statuses(context.Background())
	must(t, err)
	if status.Profiles["mason"].Source != "provider_pause" || status.Profiles["mason"].Name != "" || !strings.Contains(status.Profiles["mason"].Reason, "Provider") || s.admitRole("mason") {
		t.Fatalf("pause status: %+v", status.Profiles["mason"])
	}
	must(t, s.store.SetProfile("mason", "other"))
	if !s.admitRole("mason") {
		t.Fatal("override did not resume role")
	}
	must(t, s.store.ClearProfile("mason"))
	var ack MutationResponse
	must(t, client.Do(context.Background(), "DELETE", Prefix+"/runtime/provider-limit", ClearProviderLimitRequest{Backend: "codex"}, &ack))
	if !ack.Applied || !s.admitRole("mason") {
		t.Fatal("owner clear did not resume fallback")
	}
}
