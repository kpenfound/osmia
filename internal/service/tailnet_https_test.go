package service

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
)

const httpsNodeName = "osmia-1.tail0.ts.net"
const httpsNodeAddress = "https://" + httpsNodeName + "/"

func awaitHTTPS(t *testing.T, c *Client, state string) *TailnetStatus {
	t.Helper()
	var status *TailnetStatus
	intakefixture.Soon(t, "HTTPS never became "+state, func() bool {
		h, err := c.Health(context.Background())
		if err != nil || h.Tailnet == nil {
			return false
		}
		status = h.Tailnet
		return status.HTTPSState == state
	})
	return status
}

func nodeHTTPSClient(t *testing.T, node *fixturekit.TailnetNode) *http.Client {
	t.Helper()
	pool := x509.NewCertPool()
	pool.AddCert(node.HTTPSCertificate())
	transport := &http.Transport{DisableKeepAlives: true, TLSClientConfig: &tls.Config{RootCAs: pool}, DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
		if addr != httpsNodeName+":443" {
			return nil, fmt.Errorf("unexpected TLS destination %s", addr)
		}
		return (&net.Dialer{}).DialContext(ctx, "tcp", node.HTTPSAddr())
	}}
	t.Cleanup(transport.CloseIdleConnections)
	return &http.Client{Timeout: 5 * time.Second, Transport: transport}
}

type tailnetHTTPS struct {
	s      *Service
	c      *Client
	node   *fixturekit.TailnetNode
	secure *http.Client
	status *TailnetStatus
}

// eachTailnetHTTPSSetting starts a service that serves HTTPS on the tailnet
// both by default and when tailnet_https is set, and checks each.
func eachTailnetHTTPSSetting(t *testing.T, check func(t *testing.T, h *tailnetHTTPS)) {
	for _, setting := range []string{"", "tailnet_https = true\n"} {
		t.Run(setting, func(t *testing.T) {
			t.Parallel()
			opts := fixture(t)
			withListen(t, opts, "tailnet = \"osmia\"\n"+setting)
			joined := withTailnet(t, &opts, httpsNodeName, "osmia-1")
			s, c := start(t, opts)
			status := awaitHTTPS(t, c, api.TailnetHTTPSUp)
			node := *joined
			check(t, &tailnetHTTPS{s: s, c: c, node: node, secure: nodeHTTPSClient(t, node), status: status})
		})
	}
}

func (h *tailnetHTTPS) endpoints() []struct {
	client *http.Client
	base   string
} {
	return []struct {
		client *http.Client
		base   string
	}{{h.secure, strings.TrimSuffix(httpsNodeAddress, "/")}, {intakefixture.WebHTTP(), "http://" + h.s.TailnetAddr()}}
}

func TestTailnetServesHTTPSAtTheNodeAddressByDefaultAndWhenEnabled(t *testing.T) {
	eachTailnetHTTPSSetting(t, func(t *testing.T, h *tailnetHTTPS) {
		if h.status.HTTPSAddress != httpsNodeAddress || h.status.HTTPSReason != "" {
			t.Fatalf("HTTPS: %+v", h.status)
		}
	})
}

func TestTailnetHTTPSStateIsTheSameInHealthStatusesAndConfiguration(t *testing.T) {
	eachTailnetHTTPSSetting(t, func(t *testing.T, h *tailnetHTTPS) {
		st, err := h.c.Statuses(context.Background())
		fixturekit.Must(t, err)
		cfg, err := h.c.Configuration(context.Background())
		fixturekit.Must(t, err)
		if !cfg.Effective.Listen.TailnetHTTPS || !reflect.DeepEqual(st.Tailnet, h.status) || !reflect.DeepEqual(cfg.Tailnet, h.status) {
			t.Fatalf("status/config: %+v %+v", st.Tailnet, cfg)
		}
	})
}

func TestTailnetHealthIsServedOverTLS(t *testing.T) {
	eachTailnetHTTPSSetting(t, func(t *testing.T, h *tailnetHTTPS) {
		response, err := h.secure.Get(httpsNodeAddress + "v1/health")
		fixturekit.Must(t, err)
		response.Body.Close()
		if response.StatusCode != 200 || response.TLS == nil {
			t.Fatalf("TLS health: %+v", response)
		}
	})
}

func TestTailnetEndpointsRefuseAForeignHostHeader(t *testing.T) {
	eachTailnetHTTPSSetting(t, func(t *testing.T, h *tailnetHTTPS) {
		for _, endpoint := range h.endpoints() {
			if code, body := intakefixture.Exchange(t, endpoint.client, "GET", endpoint.base+Prefix+"/health", "", "", httpsNodeName); code != 200 {
				t.Fatalf("health: %d %s", code, body)
			}
			if code, body := intakefixture.Exchange(t, endpoint.client, "GET", endpoint.base+Prefix+"/health", "", "", "foreign.example"); code != 403 {
				t.Fatalf("foreign Host: %d %s", code, body)
			}
		}
	})
}

func TestTailnetEndpointsAcceptWritesOnlyAsJSON(t *testing.T) {
	eachTailnetHTTPSSetting(t, func(t *testing.T, h *tailnetHTTPS) {
		for _, endpoint := range h.endpoints() {
			if code, body := intakefixture.Exchange(t, endpoint.client, "PUT", endpoint.base+Prefix+"/runtime/profile", "text/plain", `{"role":"mason","profile":"other"}`, httpsNodeName); code != 403 {
				t.Fatalf("non-JSON: %d %s", code, body)
			}
			if code, body := intakefixture.Exchange(t, endpoint.client, "PUT", endpoint.base+Prefix+"/runtime/profile", "application/json", `{"role":"mason","profile":"other"}`, httpsNodeName); code != 200 {
				t.Fatalf("JSON: %d %s", code, body)
			}
		}
	})
}

func TestTailnetShutdownClosesTLSAndLeavesTheNode(t *testing.T) {
	eachTailnetHTTPSSetting(t, func(t *testing.T, h *tailnetHTTPS) {
		fixturekit.Must(t, h.s.Close())
		if !h.node.HTTPSClosed() || !h.node.Left.Load() {
			t.Fatal("shutdown did not close TLS and leave the node")
		}
		if status := h.s.tailnetStatus(context.Background()); status.HTTPSState == api.TailnetHTTPSUp || status.HTTPSAddress != "" {
			t.Fatalf("status after shutdown: %+v", status)
		}
		if _, err := h.secure.Get(httpsNodeAddress + "v1/health"); err == nil {
			t.Fatal("TLS still serves after shutdown")
		}
	})
}

func TestTailnetHTTPSOptOutAndRestartRequired(t *testing.T) {
	t.Parallel()
	opts := fixture(t)
	withListen(t, opts, "tailnet = \"osmia\"\ntailnet_https = false\n")
	joined := withTailnet(t, &opts, httpsNodeName)
	files := configFiles(t, opts)
	s, c := start(t, opts)
	node := *joined
	status := awaitHTTPS(t, c, api.TailnetHTTPSOff)
	if status.HTTPSReason == "" || status.HTTPSAddress != "" || node.HTTPSCalls.Load() != 0 {
		t.Fatalf("opt-out: %+v", status)
	}
	files.write(t, strings.Replace(files.topText, "tailnet_https = false", "tailnet_https = true", 1))
	reloaded, err := c.Reload(context.Background())
	fixturekit.Must(t, err)
	if !reflect.DeepEqual(reloaded.RestartRequired, []string{"listen.tailnet_https"}) {
		t.Fatalf("restart required: %+v", reloaded)
	}
	cfg, err := c.Configuration(context.Background())
	fixturekit.Must(t, err)
	if cfg.Effective.Listen.TailnetHTTPS || !hasCode(cfg.Diagnostics, "configuration", RestartRequired) {
		t.Fatalf("effective config: %+v", cfg)
	}
	if code, body := intakefixture.Exchange(t, intakefixture.WebHTTP(), "GET", "http://"+s.TailnetAddr()+Prefix+"/health", "", "", httpsNodeName); code != 200 {
		t.Fatalf("HTTP: %d %s", code, body)
	}
	if node.HTTPSCalls.Load() != 0 {
		t.Fatal("opt-out opened HTTPS after reload")
	}
	if got := s.notifier.link(context.Background(), s.current()); got != "http://"+httpsNodeName+"/" {
		t.Fatal(got)
	}
}

func TestTailnetHTTPSUnavailableKeepsHTTP(t *testing.T) {
	for _, reason := range []string{"enable HTTPS certificates in the tailnet admin console", "certificate retrieval failed: issuer unavailable"} {
		t.Run(reason, func(t *testing.T) {
			t.Parallel()
			opts := fixture(t)
			withListen(t, opts, "tailnet = \"osmia\"\n")
			opts.TailnetHTTPSRetry = time.Hour
			joined := withTailnet(t, &opts, httpsNodeName)
			join := opts.JoinTailnet
			opts.JoinTailnet = func(host, dir string) (TailnetListener, error) {
				node, err := join(host, dir)
				if err == nil {
					node.(*fixturekit.TailnetNode).SetHTTPSFailure(errors.New(reason))
				}
				return node, err
			}
			s, c := start(t, opts)
			want := reason + "; retrying in 1h0m0s"
			intakefixture.Soon(t, "HTTPS failure not reported", func() bool { return s.tailnetStatus(context.Background()).HTTPSReason == want })
			status := awaitHTTPS(t, c, api.TailnetHTTPSUnavailable)
			st, err := c.Statuses(context.Background())
			fixturekit.Must(t, err)
			if !reflect.DeepEqual(st.Tailnet, status) || status.State != TailnetUp || status.HTTPSReason != want || status.HTTPSAddress != "" || (*joined).Left.Load() {
				t.Fatalf("failure: %+v", status)
			}
			if code, body := intakefixture.Exchange(t, intakefixture.WebHTTP(), "GET", "http://"+s.TailnetAddr()+Prefix+"/health", "", "", httpsNodeName); code != 200 {
				t.Fatalf("HTTP: %d %s", code, body)
			}
			if got := s.notifier.link(context.Background(), s.current()); got != "http://"+httpsNodeName+"/" {
				t.Fatal(got)
			}
		})
	}
}

// A failed certificate request is retried while the node stays joined, so
// HTTPS comes up once issuance succeeds, without a restart.
func TestTailnetHTTPSRetriesUntilTheCertificateIsIssued(t *testing.T) {
	t.Parallel()
	opts := fixture(t)
	withListen(t, opts, "tailnet = \"osmia\"\n")
	opts.TailnetHTTPSRetry = 10 * time.Millisecond
	joined := withTailnet(t, &opts, httpsNodeName)
	join := opts.JoinTailnet
	failure := "certificate for " + httpsNodeName + ": context deadline exceeded"
	opts.JoinTailnet = func(host, dir string) (TailnetListener, error) {
		node, err := join(host, dir)
		if err == nil {
			node.(*fixturekit.TailnetNode).SetHTTPSFailure(errors.New(failure))
		}
		return node, err
	}
	s, c := start(t, opts)
	status := awaitHTTPS(t, c, api.TailnetHTTPSUnavailable)
	intakefixture.Soon(t, "failed request not reported with its retry", func() bool {
		status = s.tailnetStatus(context.Background())
		return strings.HasPrefix(status.HTTPSReason, failure+"; retrying in ")
	})
	node := *joined
	intakefixture.Soon(t, "certificate request not retried", func() bool { return node.HTTPSCalls.Load() >= 3 })
	node.SetHTTPSFailure(nil)
	status = awaitHTTPS(t, c, api.TailnetHTTPSUp)
	if status.HTTPSAddress != httpsNodeAddress || status.HTTPSReason != "" || node.Left.Load() {
		t.Fatalf("HTTPS after retry: %+v", status)
	}
	if code, body := intakefixture.Exchange(t, nodeHTTPSClient(t, node), "GET", httpsNodeAddress+"v1/health", "", "", httpsNodeName); code != 200 {
		t.Fatalf("TLS: %d %s", code, body)
	}
}

// A first issuance can take minutes. The request gets that long, status says
// it is in progress, and HTTPS comes up when it finishes.
func TestTailnetHTTPSWaitsForASlowIssuance(t *testing.T) {
	t.Parallel()
	opts := fixture(t)
	withListen(t, opts, "tailnet = \"osmia\"\n")
	joined := withTailnet(t, &opts, httpsNodeName)
	join := opts.JoinTailnet
	var release func()
	opts.JoinTailnet = func(host, dir string) (TailnetListener, error) {
		node, err := join(host, dir)
		if err == nil {
			release = node.(*fixturekit.TailnetNode).HoldHTTPS()
		}
		return node, err
	}
	s, c := start(t, opts)
	node := *joined
	intakefixture.Soon(t, "certificate request not reported in progress", func() bool {
		return node.HTTPSCalls.Load() == 1 && s.tailnetStatus(context.Background()).HTTPSReason == "requesting the HTTPS certificate"
	})
	if deadline := node.HTTPSDeadline(); !deadline.IsZero() && time.Until(deadline) < 5*time.Minute {
		t.Fatalf("certificate request has only %s to finish", time.Until(deadline))
	}
	if code, body := intakefixture.Exchange(t, intakefixture.WebHTTP(), "GET", "http://"+s.TailnetAddr()+Prefix+"/health", "", "", httpsNodeName); code != 200 {
		t.Fatalf("HTTP during issuance: %d %s", code, body)
	}
	release()
	status := awaitHTTPS(t, c, api.TailnetHTTPSUp)
	if status.HTTPSAddress != httpsNodeAddress || node.HTTPSCalls.Load() != 1 {
		t.Fatalf("HTTPS after slow issuance: %+v, %d requests", status, node.HTTPSCalls.Load())
	}
}

func TestTailnetHTTPSFailureFallsBackInWebhook(t *testing.T) {
	t.Parallel()
	hook := newFakeWebhook(t)
	opts, inbox := notifyFixture(t, hook.hook())
	withListen(t, opts, "tailnet = \"osmia\"\n")
	joined := withTailnet(t, &opts, httpsNodeName)
	s, c := startNotifying(t, opts, inbox)
	awaitHTTPS(t, c, api.TailnetHTTPSUp)
	intakefixture.Soon(t, "webhook was not enabled", func() bool { return readLedger(t, opts).Enabled })
	secure := nodeHTTPSClient(t, *joined)
	if code, body := intakefixture.Exchange(t, secure, "GET", httpsNodeAddress+"v1/health", "", "", httpsNodeName); code != 200 {
		t.Fatalf("TLS: %d %s", code, body)
	}
	inbox.set(fixturekit.NotificationEntry(InboxEscalation, 1))
	if got := hook.await(t, 1)[0]; !strings.HasSuffix(got, "Open: "+httpsNodeAddress+"\n") {
		t.Fatal(got)
	}
	node, httpAddr := *joined, s.TailnetAddr()
	node.DropHTTPS()
	status := awaitHTTPS(t, c, api.TailnetHTTPSUnavailable)
	if status.State != TailnetUp || !strings.Contains(status.HTTPSReason, "HTTPS listener stopped") || status.HTTPSAddress != "" || node.Left.Load() || s.TailnetAddr() != httpAddr {
		t.Fatalf("TLS failure: %+v", status)
	}
	if code, body := intakefixture.Exchange(t, intakefixture.WebHTTP(), "GET", "http://"+httpAddr+Prefix+"/health", "", "", httpsNodeName); code != 200 {
		t.Fatalf("HTTP after TLS failure: %d %s", code, body)
	}
	inbox.set(fixturekit.NotificationEntry(InboxEscalation, 1), fixturekit.NotificationEntry(InboxEscalation, 2))
	if got := hook.await(t, 2)[1]; !strings.HasSuffix(got, "Open: http://"+httpsNodeName+"/\n") {
		t.Fatal(got)
	}
}

func TestTailnetHTTPSRejoinClosesSupersededListener(t *testing.T) {
	t.Parallel()
	opts := fixture(t)
	withListen(t, opts, "tailnet = \"osmia\"\n")
	opts.TailnetRetry = time.Millisecond
	var joined atomic.Pointer[fixturekit.TailnetNode]
	join, _ := fixturekit.TailnetJoiner(httpsNodeName)
	opts.JoinTailnet = func(host, dir string) (TailnetListener, error) {
		node, err := join(host, dir)
		if err == nil {
			joined.Store(node)
		}
		return node, err
	}
	s, c := start(t, opts)
	awaitHTTPS(t, c, api.TailnetHTTPSUp)
	first := joined.Load()
	pool := x509.NewCertPool()
	pool.AddCert(first.HTTPSCertificate())
	conn, err := tls.Dial("tcp", first.HTTPSAddr(), &tls.Config{RootCAs: pool, ServerName: httpsNodeName})
	fixturekit.Must(t, err)
	defer conn.Close()
	_, err = fmt.Fprintf(conn, "GET /v1/health HTTP/1.1\r\nHost: %s\r\n\r\n", httpsNodeName)
	fixturekit.Must(t, err)
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	fixturekit.Must(t, err)
	resp.Body.Close()
	fixturekit.Must(t, first.Close())
	intakefixture.Soon(t, "node never replaced", func() bool { return joined.Load() != first && s.TailnetAddr() != "" })
	awaitHTTPS(t, c, api.TailnetHTTPSUp)
	if !first.HTTPSClosed() || !first.Left.Load() {
		t.Fatal("superseded TLS listener not closed")
	}
	conn.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("superseded TLS connection remains open")
	}
	s.tailnet.SetHTTPS(first, api.TailnetHTTPSUnavailable, "old failure", "")
	if got := s.tailnetStatus(context.Background()); got.HTTPSAddress != httpsNodeAddress || got.HTTPSState != api.TailnetHTTPSUp {
		t.Fatalf("new node status: %+v", got)
	}
	second := joined.Load()
	if code, body := intakefixture.Exchange(t, nodeHTTPSClient(t, second), "GET", httpsNodeAddress+"v1/health", "", "", httpsNodeName); code != 200 {
		t.Fatalf("replacement TLS: %d %s", code, body)
	}
	fixturekit.Must(t, s.Close())
	if !second.HTTPSClosed() || !second.Left.Load() {
		t.Fatal("current TLS listener not closed on shutdown")
	}
}
