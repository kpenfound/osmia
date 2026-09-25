package service

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"tailscale.com/tsnet"
)

// TailnetListener accepts HTTP connections from the owner's tailnet. Closing
// it stops accepting and keeps accepted connections open.
type TailnetListener interface {
	net.Listener
	// Names lists the DNS names the node answers to besides its configured
	// hostname, such as its MagicDNS name, or nil while it has none.
	Names(context.Context) []string
	// State reports whether the node is up, connecting or waiting for login.
	State(context.Context) TailnetStatus
	// Leave leaves the tailnet, which ends every connection still open.
	Leave() error
}

// JoinTailnet joins the tailnet as hostname, keeping the node's state in dir,
// and listens for HTTP on its port 80.
type JoinTailnet func(hostname, dir string) (TailnetListener, error)

// joinTailscale joins through embedded Tailscale. The node reads its auth key
// from TS_AUTHKEY on first login; without one it logs a login URL to stderr
// until someone opens it.
func joinTailscale(hostname, dir string) (TailnetListener, error) {
	server := &tsnet.Server{Dir: dir, Hostname: hostname, UserLogf: log.New(os.Stderr, "osmia tailnet: ", log.LstdFlags).Printf}
	listener, err := server.Listen("tcp", ":80")
	if err != nil {
		server.Close()
		return nil, err
	}
	return tsnetListener{Listener: listener, server: server}, nil
}

type tsnetListener struct {
	net.Listener
	server *tsnet.Server
}

func (l tsnetListener) Leave() error {
	if err := l.server.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		return err
	}
	return nil
}

func (l tsnetListener) State(ctx context.Context) TailnetStatus {
	client, err := l.server.LocalClient()
	if err != nil {
		return TailnetStatus{State: TailnetDown, Reason: err.Error()}
	}
	status, err := client.StatusWithoutPeers(ctx)
	if err != nil {
		return TailnetStatus{State: TailnetDown, Reason: err.Error()}
	}
	return tailnetState(status.BackendState, status.AuthURL)
}

// tailnetState maps a Tailscale backend state and its login URL to the
// listener's state.
func tailnetState(backend, authURL string) TailnetStatus {
	switch backend {
	case "Running":
		return TailnetStatus{State: TailnetUp}
	case "NeedsLogin":
		return TailnetStatus{State: TailnetNeedsLogin, LoginURL: authURL}
	case "NeedsMachineAuth":
		return TailnetStatus{State: TailnetNeedsLogin, Reason: "the node awaits approval on the tailnet"}
	case "Stopped":
		return TailnetStatus{State: TailnetDown, Reason: "the node is stopped"}
	}
	return TailnetStatus{State: TailnetConnecting}
}

func (l tsnetListener) Names(ctx context.Context) []string {
	client, err := l.server.LocalClient()
	if err != nil {
		return nil
	}
	status, err := client.StatusWithoutPeers(ctx)
	if err != nil || status.Self == nil || status.Self.DNSName == "" {
		return nil
	}
	name := strings.TrimSuffix(status.Self.DNSName, ".")
	short, _, _ := strings.Cut(name, ".")
	return []string{name, short}
}

// listenerKind tells which listener accepted a connection.
type listenerKind int

const (
	webListener listenerKind = iota + 1
	tailnetListener
)

type listenerKindKey struct{}

// tagged marks the connections a listener accepts with its kind, which
// connContext copies into each request's context.
type tagged struct {
	net.Listener
	kind listenerKind
}

func (l tagged) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return taggedConn{Conn: conn, kind: l.kind}, nil
}

type taggedConn struct {
	net.Conn
	kind listenerKind
}

func connContext(ctx context.Context, conn net.Conn) context.Context {
	if c, ok := conn.(taggedConn); ok {
		return context.WithValue(ctx, listenerKindKey{}, c.kind)
	}
	return ctx
}

// arrivedOn reports the kind of listener r arrived on, or zero for the Unix
// socket.
func arrivedOn(r *http.Request) listenerKind {
	kind, _ := r.Context().Value(listenerKindKey{}).(listenerKind)
	return kind
}

// tailnetAllowed keeps the tailnet listener from being reached through a
// browser on another site's behalf. The Host header must be an IP address,
// the configured hostname or one of the node's names, which refuses DNS
// rebinding, and a request that is not a read must carry a JSON content type.
func tailnetAllowed(r *http.Request, hostname string, names func(context.Context) []string) bool {
	host := strings.ToLower(strings.TrimSuffix(requestHost(r), "."))
	if _, err := netip.ParseAddr(host); err != nil && host != hostname && !containsFold(names(r.Context()), host) {
		return false
	}
	return jsonOrRead(r)
}

func containsFold(names []string, host string) bool {
	for _, name := range names {
		if strings.EqualFold(strings.TrimSuffix(name, "."), host) {
			return true
		}
	}
	return false
}

// tailnetLink is the service's hold on its tailnet node. The node comes and
// goes while the service runs; the link keeps the state of a node that is not
// there.
type tailnetLink struct {
	mu   sync.Mutex
	node TailnetListener // nil while the tailnet is down
	down string          // why there is no node
}

func (l *tailnetLink) current() TailnetListener {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.node
}

func (l *tailnetLink) set(node TailnetListener, down string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.node, l.down = node, down
}

// tailnetStatus is the state of the tailnet listener, or nil when
// listen.tailnet is not configured.
func (s *Service) tailnetStatus(ctx context.Context) *TailnetStatus {
	if s.tailnet == nil {
		return nil
	}
	node := s.tailnet.current()
	if node == nil {
		s.tailnet.mu.Lock()
		defer s.tailnet.mu.Unlock()
		return &TailnetStatus{State: TailnetDown, Reason: s.tailnet.down}
	}
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	state := node.State(ctx)
	return &state
}

// tailnetNames lists the names the current node holds, or none while there is
// no node.
func (s *Service) tailnetNames(ctx context.Context) []string {
	if node := s.tailnet.current(); node != nil {
		return node.Names(ctx)
	}
	return nil
}

// superviseTailnet serves the tailnet listener until the service stops. When
// the node cannot join or its listener fails, the service keeps running
// without it and joins again after Options.TailnetRetry.
func (s *Service) superviseTailnet(root config.Root, hostname string) {
	defer close(s.tailnetDone)
	for {
		node := s.tailnet.current()
		if node == nil {
			var err error
			if node, err = s.joinTailnet(root, hostname); err != nil {
				s.tailnet.set(nil, err.Error())
				log.Printf("osmia tailnet: join %s: %v; retrying in %s", hostname, err, s.options.TailnetRetry)
				if !s.tailnetWait() {
					return
				}
				continue
			}
			s.tailnet.set(node, "")
		}
		err := s.server.Serve(tagged{Listener: node, kind: tailnetListener})
		if s.lifetime.Err() != nil || errors.Is(err, http.ErrServerClosed) {
			return
		}
		s.tailnet.set(nil, fmt.Sprintf("the tailnet listener stopped: %v", err))
		node.Close()
		node.Leave()
		log.Printf("osmia tailnet: listener stopped: %v; rejoining in %s", err, s.options.TailnetRetry)
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
