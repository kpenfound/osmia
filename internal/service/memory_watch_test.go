package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/hearsay"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestMemoryWatchResumesCursorAndDeduplicatesAfterRestart(t *testing.T) {
	t.Setenv("WATCH_OWNER_TOKEN", "owner-secret")
	t.Setenv("WATCH_AGENT_TOKEN", "agent-secret")
	_, cfg := conversationFixture(t, "memory-watch-")
	var pass atomic.Int32
	requests := make(chan string, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer owner-secret" || r.Header.Get("Hearsay-Agent-Token") != "agent-secret" {
			t.Error("watch did not use delegated authentication")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var args struct {
			After string `json:"after"`
		}
		if err := json.NewDecoder(r.Body).Decode(&args); err != nil {
			t.Error(err)
			return
		}
		requests <- args.After
		n := pass.Add(1)
		if n > 2 {
			<-r.Context().Done()
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"cursor": map[int32]string{1: "cursor-1", 2: "cursor-2"}[n], "notifications": []trace.MemoryEvent{{Event: "event-1", Document: "document-1", Source: "discord", Kind: "message", Time: fixturekit.DemoStart}}})
	}))
	defer server.Close()
	cfg.Hearsay = config.Hearsay{URL: server.URL, Principal: "owner", TokenEnv: "WATCH_OWNER_TOKEN", Agents: map[string]config.HearsayAgent{"orchestrator": {ID: "chief", TokenEnv: "WATCH_AGENT_TOKEN"}}}
	cfg.Projects[0].HearsayScope = "project"
	cfg.Project = cfg.Projects[0]
	repo, err := trace.Open(cfg.Root, cfg.Project)
	intakefixture.Must(t, err)
	s := &Service{cfg: cfg, memory: hearsay.Health{}}
	run := func(want string) {
		t.Helper()
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- s.watchMemory(ctx, repo) }()
		intakefixture.Soon(t, "the memory cursor to reach "+want, func() bool {
			cursor, err := repo.MemoryCursor(memoryWatchKey(cfg))
			intakefixture.Must(t, err)
			return cursor == want
		})
		cancel()
		intakefixture.Must(t, <-done)
	}
	run("cursor-1")
	intakefixture.Must(t, repo.Close())
	repo, err = trace.Open(cfg.Root, cfg.Project)
	intakefixture.Must(t, err)
	defer repo.Close()
	run("cursor-2")
	if first, second := <-requests, <-requests; first != "" || second != "cursor-1" {
		t.Fatalf("watch cursors %q %q", first, second)
	}
	events, err := repo.Outbox(intakefixture.Stream)
	intakefixture.Must(t, err)
	count := 0
	for _, event := range events {
		if strings.HasPrefix(event.TransitionID, "watch-notice-") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("%d notices for replayed event", count)
	}
}
