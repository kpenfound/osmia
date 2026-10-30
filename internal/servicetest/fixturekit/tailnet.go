package fixturekit

import (
	context "context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	api "github.com/kpenfound/osmia/internal/api"
	"math/big"
	net "net"
	os "os"
	filepath "path/filepath"
	"strings"
	sync "sync"
	atomic "sync/atomic"
	testing "testing"
	"time"
)

// TailnetNode stands in for the embedded Tailscale node with a loopback TCP
// listener, so no test reaches a real tailnet. Leaving ends the connections
// it accepted, as leaving a real tailnet does.
type TailnetNode struct {
	net.Listener
	Hostname, Directory string
	NodeNames           []string
	Closed, Left        atomic.Bool
	mu                  sync.Mutex
	conns               []net.Conn
	state               api.TailnetStatus
	certificate         *x509.Certificate
	// HTTPSFailure simulates certificate availability or issuance failure.
	// Set it before the node is returned by the joiner.
	HTTPSFailure error
	HTTPSCalls   atomic.Int32
	tlsListener  net.Listener
	tlsClosed    atomic.Bool
}

func (f *TailnetNode) State(context.Context) api.TailnetStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.state.State == "" {
		return api.TailnetStatus{State: api.TailnetUp}
	}
	return f.state
}

func (f *TailnetNode) SetState(state api.TailnetStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state = state
}

// Drop takes the tailnet away under the service: accepting fails and the
// connections it accepted end.
func (f *TailnetNode) Drop() {
	f.Close()
	f.Leave()
}

func (f *TailnetNode) Names(context.Context) []string { return f.NodeNames }

func (f *TailnetNode) Accept() (net.Conn, error) {
	conn, err := f.Listener.Accept()
	if err == nil {
		f.mu.Lock()
		f.conns = append(f.conns, conn)
		f.mu.Unlock()
	}
	return conn, err
}

func (f *TailnetNode) Close() error {
	f.Closed.Store(true)
	return f.Listener.Close()
}

func (f *TailnetNode) Leave() error {
	f.Left.Store(true)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.tlsListener != nil {
		f.tlsListener.Close()
		f.tlsClosed.Store(true)
	}
	for _, conn := range f.conns {
		conn.Close()
	}
	return nil
}

// TailnetJoiner returns a fake tailnet join function and the node it joins.
func TailnetJoiner(names ...string) (func(string, string) (*TailnetNode, error), **TailnetNode) {
	joined := new(*TailnetNode)
	join := func(hostname, dir string) (*TailnetNode, error) {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		*joined = &TailnetNode{Listener: l, Hostname: hostname, Directory: dir, NodeNames: names}
		return *joined, nil
	}
	return join, joined
}

// WithListen appends a [listen] table with the given body to the fixture's
// top-level configuration file.
func WithListen(t *testing.T, root, body string) {
	t.Helper()
	path := filepath.Join(root, "config.toml")
	top, err := os.ReadFile(path)
	Must(t, err)
	Must(t, os.WriteFile(path, append(top, []byte("[listen]\n"+body)...), 0600))
}

// HTTPS serves real TLS with a self-signed certificate for the full node name.
// Browsers can map that name to HTTPSAddr and trust this test certificate.
func (f *TailnetNode) HTTPS(ctx context.Context) (net.Listener, string, error) {
	f.HTTPSCalls.Add(1)
	if f.HTTPSFailure != nil {
		return nil, "", f.HTTPSFailure
	}
	if len(f.NodeNames) == 0 || !strings.HasSuffix(strings.TrimSuffix(f.NodeNames[0], "."), ".ts.net") {
		return nil, "", errors.New("the node has no full MagicDNS name")
	}
	name := strings.TrimSuffix(f.NodeNames[0], ".")
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, "", err
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name},
		DNSNames: []string{name}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, "", err
	}
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, "", err
	}
	listener := &tailnetTLSListener{Listener: tls.NewListener(raw, &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}), node: f}
	f.mu.Lock()
	f.tlsListener = listener
	f.certificate, _ = x509.ParseCertificate(der)
	f.mu.Unlock()
	return listener, name, nil
}

// HTTPSAddr is the local address standing in for the node's tailnet port 443.
func (f *TailnetNode) HTTPSAddr() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.tlsListener == nil {
		return ""
	}
	return f.tlsListener.Addr().String()
}

func (f *TailnetNode) HTTPSClosed() bool { return f.tlsClosed.Load() }

// DropHTTPS fails only the TLS listener; the node and HTTP stay available.
func (f *TailnetNode) DropHTTPS() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.tlsListener != nil {
		f.tlsListener.Close()
	}
}

type tailnetTLSListener struct {
	net.Listener
	node *TailnetNode
}

func (l *tailnetTLSListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err == nil {
		l.node.mu.Lock()
		l.node.conns = append(l.node.conns, conn)
		l.node.mu.Unlock()
	}
	return conn, err
}

func (l *tailnetTLSListener) Close() error {
	l.node.tlsClosed.Store(true)
	return l.Listener.Close()
}

// HTTPSCertificate is the certificate a test client or browser can trust.
func (f *TailnetNode) HTTPSCertificate() *x509.Certificate {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.certificate
}
