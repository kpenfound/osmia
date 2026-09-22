package service

import (
	"context"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"
)

func TestClientTimeoutBudgetsAndTransportMessages(t *testing.T) {
	t.Parallel()
	socket := filepath.Join(t.TempDir(), "service.sock")
	listener, err := net.Listen("unix", socket)
	must(t, err)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(80 * time.Millisecond)
		w.Write([]byte("{}"))
	})}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close() })
	c := NewClient(socket)
	defer c.Close()
	if c.defaultTimeout != defaultClientTimeout || c.addProjectTimeout != addProjectTimeout {
		t.Fatalf("default budgets: %s, %s", c.defaultTimeout, c.addProjectTimeout)
	}
	c.defaultTimeout = 20 * time.Millisecond
	c.addProjectTimeout = 200 * time.Millisecond
	_, err = c.Health(context.Background())
	apiError(t, err, Unavailable, "no response within 20ms")
	if _, err = c.AddProject(context.Background(), ProjectAddRequest{}); err != nil {
		t.Fatalf("project add should have the heavy budget: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err = c.AddProject(ctx, ProjectAddRequest{})
	apiError(t, err, Unavailable, "no response before caller's context deadline")
	cancelled, stop := context.WithCancel(context.Background())
	stop()
	_, err = c.Health(cancelled)
	apiError(t, err, Unavailable, "cannot reach Osmia Unix socket")

	dead := NewClient(filepath.Join(t.TempDir(), "missing.sock"))
	defer dead.Close()
	_, err = dead.Health(context.Background())
	apiError(t, err, Unavailable, "cannot reach Osmia Unix socket")
}

func TestProjectAddOutlivesDefaultWriteTimeout(t *testing.T) {
	t.Parallel()
	opts, clone := projectFixture(t)
	opts.WriteTimeout = 20 * time.Millisecond
	s, c := start(t, opts)
	c.addProjectTimeout = time.Second
	entered := make(chan struct{})
	handler := s.server.Handler
	s.server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		handler.ServeHTTP(w, r)
	})
	s.projectMu.Lock()
	result := make(chan error, 1)
	go func() {
		_, err := c.AddProject(context.Background(), request(clone))
		result <- err
	}()
	<-entered
	time.Sleep(100 * time.Millisecond)
	s.projectMu.Unlock()
	if err := <-result; err != nil {
		t.Fatalf("project add after write timeout: %v", err)
	}
}
