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

func (s *Service) tailnetStatus(ctx context.Context) *TailnetStatus { return s.tailnet.Status(ctx) }
func (s *Service) tailnetNames(ctx context.Context) []string        { return s.tailnet.Names(ctx) }

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

const (
	tailnetHTTPSAttempt  = 10 * time.Minute
	tailnetHTTPSMaxRetry = 15 * time.Minute
)

func (s *Service) requestTailnetHTTPS(ctx context.Context, node tailnet.Listener) (net.Listener, string, bool) {
	retry := s.options.TailnetHTTPSRetry
	for {
		s.tailnet.SetHTTPS(node, api.TailnetHTTPSUnavailable, "requesting the HTTPS certificate", "")
		attempt, cancel := context.WithTimeout(ctx, tailnetHTTPSAttempt)
		listener, name, err := node.HTTPS(attempt)
		cancel()
		if err == nil {
			return listener, name, true
		}
		if ctx.Err() != nil {
			return nil, "", false
		}
		reason := redact.Secrets(err.Error(), redact.KnownSecrets()...)
		s.tailnet.SetHTTPS(node, api.TailnetHTTPSUnavailable, fmt.Sprintf("%s; retrying in %s", reason, retry), "")
		s.logger.Warn("tailnet HTTPS unavailable; retrying", "component", "tailnet", "error", reason, "retry", retry.String())
		timer := time.NewTimer(retry)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, "", false
		case <-timer.C:
		}
		retry = min(retry*2, tailnetHTTPSMaxRetry)
	}
}

func (s *Service) serveTailnetHTTPS(ctx context.Context, node tailnet.Listener) {
	if !s.tailnet.HTTPSEnabled {
		return
	}
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
	listener, name, ok := s.requestTailnetHTTPS(ctx, node)
	if !ok {
		return
	}
	defer listener.Close()
	if ctx.Err() != nil {
		return
	}
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
