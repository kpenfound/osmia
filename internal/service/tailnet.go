package service

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/kpenfound/osmia/internal/api"
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
		ctx, cancel := context.WithCancel(s.lifetime)
		httpsDone := make(chan struct{})
		go func() { defer close(httpsDone); s.serveTailnetHTTPS(ctx, node) }()
		err := s.server.Serve(tailnet.Tagged{Listener: node, Kind: tailnet.TailnetListener})
		if s.lifetime.Err() != nil || errors.Is(err, http.ErrServerClosed) {
			if s.tailnet.HTTPSEnabled {
				s.tailnet.SetHTTPS(node, api.TailnetHTTPSUnavailable, "the service is shutting down", "")
			}
			cancel()
			<-httpsDone
			// cleanupSocket leaves the node after HTTP requests have drained.
			return
		}
		s.tailnet.Set(nil, fmt.Sprintf("the tailnet listener stopped: %v", err))
		cancel()
		<-httpsDone
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

// serveTailnetHTTPS owns one node's TLS listener and its serving goroutine.
// An HTTPS failure never replaces the node or interrupts its HTTP listener.
func (s *Service) serveTailnetHTTPS(ctx context.Context, node tailnet.Listener) {
	if !s.tailnet.HTTPSEnabled {
		return
	}
	// A newly joined node can still be waiting for login. Wait for it without
	// delaying HTTP, and stop waiting when this node is replaced.
	for {
		query, cancel := context.WithTimeout(ctx, time.Second)
		state := node.State(query)
		cancel()
		if ctx.Err() != nil {
			return
		}
		if state.State == api.TailnetUp {
			break
		}
		reason := state.Reason
		if reason == "" {
			reason = "the tailnet node is " + state.State
		}
		s.tailnet.SetHTTPS(node, api.TailnetHTTPSUnavailable, reason, "")
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
	setup, cancel := context.WithTimeout(ctx, 30*time.Second)
	listener, name, err := node.HTTPS(setup)
	cancel()
	if err != nil {
		s.tailnet.SetHTTPS(node, api.TailnetHTTPSUnavailable, err.Error(), "")
		return
	}
	defer listener.Close()
	if ctx.Err() != nil {
		return
	}
	// Keep the TLS connection concrete so net/http performs its handshake and
	// fills Request.TLS. The context carries the same tag as tailnet HTTP.
	server := &http.Server{
		Handler:           s.server.Handler,
		ReadHeaderTimeout: s.server.ReadHeaderTimeout,
		ReadTimeout:       s.server.ReadTimeout,
		WriteTimeout:      s.server.WriteTimeout,
		BaseContext:       func(net.Listener) context.Context { return s.lifetime },
		ConnContext: func(ctx context.Context, _ net.Conn) context.Context {
			return tailnet.WithKind(ctx, tailnet.TailnetListener)
		},
	}
	served := make(chan error, 1)
	s.tailnet.SetHTTPS(node, api.TailnetHTTPSUp, "", "https://"+name+"/")
	go func() { served <- server.Serve(listener) }()
	select {
	case err := <-served:
		s.tailnet.SetHTTPS(node, api.TailnetHTTPSUnavailable, fmt.Sprintf("the tailnet HTTPS listener stopped: %v", err), "")
		server.Close()
	case <-ctx.Done():
		s.tailnet.SetHTTPS(node, api.TailnetHTTPSUnavailable, "the tailnet HTTPS listener is stopping", "")
		if s.lifetime.Err() != nil {
			drain, stop := context.WithTimeout(context.Background(), s.options.ShutdownTimeout)
			server.Shutdown(drain)
			stop()
		}
		server.Close()
		<-served
	}
}
