package tailnet

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/redact"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tsnet"
)

// Listener accepts HTTP connections from the owner's tailnet. Closing
// it stops accepting and keeps accepted connections open.
type Listener interface {
	net.Listener
	// Names lists the DNS names the node answers to besides its configured
	// hostname, such as its MagicDNS name, or nil while it has none.
	Names(context.Context) []string
	// State reports whether the node is up, connecting or waiting for login.
	State(context.Context) api.TailnetStatus
	// HTTPS opens TLS on port 443 using a certificate for the full MagicDNS
	// name, returned alongside the listener. Failure leaves HTTP serving.
	HTTPS(context.Context) (net.Listener, string, error)
	// Leave leaves the tailnet, which ends every connection still open.
	Leave() error
}

// Join joins the tailnet as hostname, keeping the node's state in dir,
// and listens for HTTP on its port 80.
type Join func(hostname, dir string) (Listener, error)

// loginURLPattern finds the login URL embedded Tailscale's logger prints
// while the node waits for approval, so it can travel as a field of its own
// rather than only within the logged message text.
var loginURLPattern = regexp.MustCompile(`https?://\S+`)

// tailscaleLogf adapts logger to embedded Tailscale's logf signature: every
// line it logs becomes one structured record, with any URL the line
// mentions, such as the tailnet login URL shown while the node waits for
// approval, as its own field.
func tailscaleLogf(logger *slog.Logger) func(string, ...any) {
	return func(format string, args ...any) {
		line := strings.TrimSpace(fmt.Sprintf(format, args...))
		if line == "" {
			return
		}
		line = redact.Secrets(line, redact.KnownSecrets()...)
		attrs := []any{"component", "tailnet", "message", line}
		if url := loginURLPattern.FindString(line); url != "" {
			attrs = append(attrs, "login_url", url)
		}
		logger.Info("tailnet message", attrs...)
	}
}

// JoinTailscale joins through embedded Tailscale. The node reads its auth key
// from TS_AUTHKEY on first login; without one it logs a login URL, as a
// field of a structured record, until someone opens it.
func JoinTailscale(logger *slog.Logger) Join {
	return func(hostname, dir string) (Listener, error) {
		server := &tsnet.Server{Dir: dir, Hostname: hostname, UserLogf: tailscaleLogf(logger)}
		listener, err := server.Listen("tcp", ":80")
		if err != nil {
			server.Close()
			return nil, err
		}
		return tsnetListener{Listener: listener, server: server}, nil
	}
}

type tsnetListener struct {
	net.Listener
	server *tsnet.Server
}

func (l tsnetListener) HTTPS(ctx context.Context) (net.Listener, string, error) {
	client, err := l.server.LocalClient()
	if err != nil {
		return nil, "", err
	}
	return listenHTTPS(ctx, client, l.server.Listen)
}

type certificateClient interface {
	StatusWithoutPeers(context.Context) (*ipnstate.Status, error)
	CertPair(context.Context, string) ([]byte, []byte, error)
	GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error)
}

func listenHTTPS(ctx context.Context, client certificateClient, listen func(string, string) (net.Listener, error)) (net.Listener, string, error) {
	status, err := client.StatusWithoutPeers(ctx)
	if err != nil {
		return nil, "", err
	}
	if status.Self == nil {
		return nil, "", errors.New("the node has no MagicDNS name")
	}
	name := strings.TrimSuffix(status.Self.DNSName, ".")
	if !strings.HasSuffix(name, ".ts.net") {
		return nil, "", errors.New("the node has no full MagicDNS name")
	}
	if !containsFold(status.CertDomains, name) {
		return nil, "", errors.New("enable HTTPS certificates in the tailnet admin console for " + name)
	}
	// Fetch ahead of serving so issuance failures are visible without a handshake.
	certPEM, keyPEM, err := client.CertPair(ctx, name)
	if err == nil {
		_, err = tls.X509KeyPair(certPEM, keyPEM)
	}
	if err != nil {
		return nil, "", fmt.Errorf("certificate for %s: %w", name, err)
	}
	listener, err := listen("tcp", ":443")
	if err != nil {
		return nil, "", err
	}
	return tls.NewListener(listener, &tls.Config{GetCertificate: client.GetCertificate}), name, nil
}

func (l tsnetListener) Leave() error {
	if err := l.server.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		return err
	}
	return nil
}

func (l tsnetListener) State(ctx context.Context) api.TailnetStatus {
	client, err := l.server.LocalClient()
	if err != nil {
		return api.TailnetStatus{State: api.TailnetDown, Reason: err.Error()}
	}
	status, err := client.StatusWithoutPeers(ctx)
	if err != nil {
		return api.TailnetStatus{State: api.TailnetDown, Reason: err.Error()}
	}
	return tailnetState(status.BackendState, status.AuthURL)
}

// tailnetState maps a Tailscale backend state and its login URL to the
// listener's state.
func tailnetState(backend, authURL string) api.TailnetStatus {
	switch backend {
	case "Running":
		return api.TailnetStatus{State: api.TailnetUp}
	case "NeedsLogin":
		return api.TailnetStatus{State: api.TailnetNeedsLogin, LoginURL: authURL}
	case "NeedsMachineAuth":
		return api.TailnetStatus{State: api.TailnetNeedsLogin, Reason: "the node awaits approval on the tailnet"}
	case "Stopped":
		return api.TailnetStatus{State: api.TailnetDown, Reason: "the node is stopped"}
	}
	return api.TailnetStatus{State: api.TailnetConnecting}
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

// ListenerKind tells which listener accepted a connection.
type ListenerKind int

const (
	WebListener ListenerKind = iota + 1
	TailnetListener
)

type listenerKindKey struct{}

// Tagged marks the connections a listener accepts with its kind, which
// connContext copies into each request's context.
type Tagged struct {
	net.Listener
	Kind ListenerKind
}

func (l Tagged) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return taggedConn{Conn: conn, Kind: l.Kind}, nil
}

type taggedConn struct {
	net.Conn
	Kind ListenerKind
}

func ConnContext(ctx context.Context, conn net.Conn) context.Context {
	if c, ok := conn.(taggedConn); ok {
		return WithKind(ctx, c.Kind)
	}
	return ctx
}

// WithKind tags a connection context with the listener that accepted it.
func WithKind(ctx context.Context, kind ListenerKind) context.Context {
	return context.WithValue(ctx, listenerKindKey{}, kind)
}

// ArrivedOn reports the kind of listener r arrived on, or zero for the Unix
// socket.
func ArrivedOn(r *http.Request) ListenerKind {
	kind, _ := r.Context().Value(listenerKindKey{}).(ListenerKind)
	return kind
}

// Allowed keeps the tailnet listener from being reached through a
// browser on another site's behalf. The Host header must be an IP address,
// the configured hostname or one of the node's names, which refuses DNS
// rebinding, and a request that is not a read must carry a JSON content type.
func Allowed(r *http.Request, hostname string, names func(context.Context) []string) bool {
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

// Link is the service's hold on its tailnet node. The node comes and
// goes while the service runs; the link keeps the state of a node that is not
// there.
type Link struct {
	mu   sync.Mutex
	node Listener // nil while the tailnet is down
	down string   // why there is no node
	// HTTPSEnabled is fixed at startup.
	HTTPSEnabled                          bool
	httpsState, httpsReason, httpsAddress string
}

func (l *Link) Current() Listener {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.node
}

func (l *Link) Set(node Listener, down string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.node, l.down = node, down
	l.httpsState, l.httpsAddress = api.TailnetHTTPSUnavailable, ""
	l.httpsReason = "HTTPS is being set up"
	if node == nil {
		l.httpsReason = "the tailnet node is unavailable: " + down
	}
	if !l.HTTPSEnabled {
		l.httpsState, l.httpsReason = api.TailnetHTTPSOff, "disabled by listen.tailnet_https"
	}
}

// Status returns the current node state, bounding the node query to one second.
func (l *Link) Status(ctx context.Context) *api.TailnetStatus {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	state := api.TailnetStatus{State: api.TailnetDown, Reason: l.down}
	if l.node != nil {
		ctx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		state = l.node.State(ctx)
	}
	state.HTTPSState, state.HTTPSReason, state.HTTPSAddress = l.httpsState, l.httpsReason, l.httpsAddress
	return &state
}

// SetHTTPS records a listener's state only while its node is current.
func (l *Link) SetHTTPS(node Listener, state, reason, address string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.node == node {
		l.httpsState, l.httpsReason, l.httpsAddress = state, reason, address
	}
}

// Names returns the names of the current node, or none while it is down.
func (l *Link) Names(ctx context.Context) []string {
	if node := l.Current(); node != nil {
		return node.Names(ctx)
	}
	return nil
}

// WebAllowed keeps a loopback listener from being reached through a browser
// on another site's behalf. The Host header must name a loopback host, which
// refuses DNS rebinding, and a request that is not a read must carry a JSON
// content type, which a cross-site form cannot send without a preflight the
// service never grants.
func WebAllowed(r *http.Request) bool {
	return config.LoopbackHost(requestHost(r)) && jsonOrRead(r)
}

// requestHost is r's Host header without its port or IPv6 brackets.
func requestHost(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.Host)
	if err != nil {
		host = strings.TrimSuffix(strings.TrimPrefix(r.Host, "["), "]")
	}
	return host
}

// jsonOrRead reports whether r is a GET or HEAD, or carries a JSON content
// type.
func jsonOrRead(r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return true
	}
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return err == nil && media == "application/json"
}
