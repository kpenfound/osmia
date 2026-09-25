package service

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strings"

	"tailscale.com/tsnet"
)

// TailnetListener accepts HTTP connections from the owner's tailnet. Closing
// it leaves the tailnet.
type TailnetListener interface {
	net.Listener
	// Names lists the DNS names the node answers to besides its configured
	// hostname, such as its MagicDNS name, or nil while it has none.
	Names(context.Context) []string
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

func (l tsnetListener) Close() error {
	err := l.Listener.Close()
	if e := l.server.Close(); e != nil && !errors.Is(e, net.ErrClosed) {
		err = errors.Join(err, e)
	}
	return err
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
