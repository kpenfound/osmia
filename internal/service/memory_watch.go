package service

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/hearsay"
	"github.com/kpenfound/osmia/internal/trace"
)

func memoryWatchKey(cfg *config.Config) string {
	data, _ := json.Marshal(struct {
		Config config.Hearsay
		Scope  string
	}{cfg.Hearsay, cfg.Project.HearsayScope})
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

// watchMemory joins the project runtime's lifetime. Remote outages retain its
// cursor and never stop local work; local persistence failures stop the service.
func (s *Service) watchMemory(ctx context.Context, repo *trace.Repository) error {
	delay := time.Duration(0)
	for {
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
		cfg := s.current().For(repo.Project())
		if !cfg.HasProject() {
			return nil
		}
		delay = time.Second
		if cfg.Hearsay.URL == "" || cfg.Project.HearsayScope == "" {
			continue
		}
		key := memoryWatchKey(cfg)
		cursor, err := repo.MemoryCursor(key)
		if err != nil {
			return err
		}
		args := map[string]any{"scope": cfg.Project.HearsayScope, "filter": map[string]any{}}
		if cursor != "" {
			args["after"] = cursor
		}
		raw, err := (hearsay.Client{Config: cfg.Hearsay}).Call(ctx, trace.ChiefOfStaff, "watch", args)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			s.memory.Observe(cfg, repo.Project(), false)
			delay = 5 * time.Second
			continue
		}
		current := s.current().For(repo.Project())
		if !current.HasProject() {
			return nil
		}
		if memoryWatchKey(current) != key {
			continue
		}
		var response struct {
			Cursor        string              `json:"cursor"`
			Notifications []trace.MemoryEvent `json:"notifications"`
		}
		if json.Unmarshal(raw, &response) != nil || trace.ValidateMemoryWatch(response.Cursor, response.Notifications) != nil {
			s.memory.Observe(cfg, repo.Project(), false)
			delay = 5 * time.Second
			continue
		}
		targets, err := repo.Workstreams()
		if err != nil {
			return err
		}
		targets = slices.DeleteFunc(targets, func(id config.WorkstreamID) bool { return id == librarianWorkstream(repo.Project()) })
		if err := repo.RecordMemoryWatch(ctx, key, cursor, response.Cursor, response.Notifications, targets, s.now()); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		s.memory.Observe(cfg, repo.Project(), true)
	}
}
