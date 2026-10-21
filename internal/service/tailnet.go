package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/redact"
	"github.com/kpenfound/osmia/internal/tailnet"
)

// tailnetStatus is the state of the tailnet listener, or nil when
// listen.tailnet is not configured.
func (s *Service) tailnetStatus(ctx context.Context) *TailnetStatus { return s.tailnet.Status(ctx) }
func (s *Service) tailnetNames(ctx context.Context) []string        { return s.tailnet.Names(ctx) }

// superviseTailnet serves the tailnet listener until the service stops. When
// the node cannot join or its listener fails, the service keeps running
// without it and joins again after Options.TailnetRetry.
func (s *Service) superviseTailnet(root config.Root, hostname string) {
	defer close(s.tailnetDone)
	for {
		node := s.tailnet.Current()
		if node == nil {
			var err error
			if node, err = s.joinTailnet(root, hostname); err != nil {
				s.tailnet.Set(nil, err.Error())
				s.logger.Warn("tailnet join failed; retrying", "component", "tailnet", "hostname", hostname, "error", redact.Secrets(err.Error(), redact.KnownSecrets()...), "retry", s.options.TailnetRetry.String())
				if !s.tailnetWait() {
					return
				}
				continue
			}
			s.tailnet.Set(node, "")
			s.logger.Info("tailnet joined", "component", "tailnet", "hostname", hostname, "state", "up")
		}
		err := s.server.Serve(tailnet.Tagged{Listener: node, Kind: tailnet.TailnetListener})
		if s.lifetime.Err() != nil || errors.Is(err, http.ErrServerClosed) {
			return
		}
		s.tailnet.Set(nil, fmt.Sprintf("the tailnet listener stopped: %v", err))
		node.Close()
		node.Leave()
		s.logger.Warn("tailnet listener stopped; rejoining", "component", "tailnet", "error", redact.Secrets(err.Error(), redact.KnownSecrets()...), "retry", s.options.TailnetRetry.String())
		if !s.tailnetWait() {
			return
		}
	}
}

// tailnetWait waits out the retry delay and reports whether the service is
// still running.
func (s *Service) tailnetWait() bool {
	timer := time.NewTimer(s.options.TailnetRetry)
	defer timer.Stop()
	select {
	case <-s.lifetime.Done():
		return false
	case <-timer.C:
		return true
	}
}
