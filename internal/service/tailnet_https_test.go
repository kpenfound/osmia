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
)

const httpsNodeName = "osmia-1.tail0.ts.net"
const httpsNodeAddress = "https://" + httpsNodeName + "/"

func awaitHTTPS(t *testing.T, c *Client, state string) *TailnetStatus {
	t.Helper()
	var status *TailnetStatus
	eventually(t, "HTTPS never became "+state, func() bool {
		h, err := c.Health(context.Background())
		if err != nil || h.Tailnet == nil {
			return false
		}
		status = h.Tailnet
		return status.HTTPSState == state
	})
	return status
}

// The URL uses the full MagicDNS name and default port; only dialing is
// replaced by loopback. TLS verifies the fake node's certificate and name.
func nodeHTTPSClient(t *testing.T, node *fakeTailnet) *http.Client {
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

func TestTailnetHTTPSDefaultAndExplicitEnable(t *testing.T) {
	for _, setting := range []string{"", "tailnet_https = true\n"} {
		t.Run(setting, func(t *testing.T) {
			t.Parallel()
			opts := fixture(t)
			withListen(t, opts, "tailnet = \"osmia\"\n"+setting)
			joined := withTailnet(t, &opts, httpsNodeName, "osmia-1")
			s, c := start(t, opts)
			status := awaitHTTPS(t, c, api.TailnetHTTPSUp)
			if status.HTTPSAddress != httpsNodeAddress || status.HTTPSReason != "" {
				t.Fatalf("HTTPS: %+v", status)
			}
			st, err := c.Statuses(context.Background())
			must(t, err)
			cfg, err := c.Configuration(context.Background())
			must(t, err)
			if !cfg.Effective.Listen.TailnetHTTPS || !reflect.DeepEqual(st.Tailnet, status) || !reflect.DeepEqual(cfg.Tailnet, status) {
				t.Fatalf("status/config: %+v %+v", st.Tailnet, cfg)
			}
			node := *joined
			secure := nodeHTTPSClient(t, node)
			response, err := secure.Get(httpsNodeAddress + "v1/health")
			must(t, err)
			response.Body.Close()
			if response.StatusCode != 200 || response.TLS == nil {
				t.Fatalf("TLS health: %+v", response)
			}
			for _, endpoint := range []struct {
				client *http.Client
				base   string
			}{{secure, strings.TrimSuffix(httpsNodeAddress, "/")}, {webHTTP(), "http://" + s.TailnetAddr()}} {
				if code, body := exchange(t, endpoint.client, "GET", endpoint.base+Prefix+"/health", "", "", httpsNodeName); code != 200 {
					t.Fatalf("health: %d %s", code, body)
				}
				if code, body := exchange(t, endpoint.client, "GET", endpoint.base+Prefix+"/health", "", "", "foreign.example"); code != 403 {
					t.Fatalf("foreign Host: %d %s", code, body)
				}
				if code, body := exchange(t, endpoint.client, "PUT", endpoint.base+Prefix+"/runtime/profile", "text/plain", `{"role":"mason","profile":"other"}`, httpsNodeName); code != 403 {
					t.Fatalf("non-JSON: %d %s", code, body)
				}
				if code, body := exchange(t, endpoint.client, "PUT", endpoint.base+Prefix+"/runtime/profile", "application/json", `{"role":"mason","profile":"other"}`, httpsNodeName); code != 200 {
					t.Fatalf("JSON: %d %s", code, body)
				}
			}
			must(t, s.Close())
			if !node.HTTPSClosed() || !node.Left.Load() {
				t.Fatal("shutdown did not close TLS and leave the node")
			}
			if status := s.tailnetStatus(context.Background()); status.HTTPSState == api.TailnetHTTPSUp || status.HTTPSAddress != "" {
				t.Fatalf("status after shutdown: %+v", status)
			}
			if _, err := secure.Get(httpsNodeAddress + "v1/health"); err == nil {
				t.Fatal("TLS still serves after shutdown")
			}
		})
	}
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
	files.write(t, strings.Replace(files.topText, "tailnet_https = false", "tailnet_https = true", 1), files.projectText)
	reloaded, err := c.Reload(context.Background())
	must(t, err)
	if !reflect.DeepEqual(reloaded.RestartRequired, []string{"listen.tailnet_https"}) {
		t.Fatalf("restart required: %+v", reloaded)
	}
	cfg, err := c.Configuration(context.Background())
	must(t, err)
	if cfg.Effective.Listen.TailnetHTTPS || !hasCode(cfg.Diagnostics, "configuration", RestartRequired) {
		t.Fatalf("effective config: %+v", cfg)
	}
	if code, body := exchange(t, webHTTP(), "GET", "http://"+s.TailnetAddr()+Prefix+"/health", "", "", httpsNodeName); code != 200 {
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
			joined := withTailnet(t, &opts, httpsNodeName)
			join := opts.JoinTailnet
			opts.JoinTailnet = func(host, dir string) (TailnetListener, error) {
				node, err := join(host, dir)
				if err == nil {
					node.(*fakeTailnet).HTTPSFailure = errors.New(reason)
				}
				return node, err
			}
			s, c := start(t, opts)
			eventually(t, "HTTPS failure not reported", func() bool { return s.tailnetStatus(context.Background()).HTTPSReason == reason })
			status := awaitHTTPS(t, c, api.TailnetHTTPSUnavailable)
			st, err := c.Statuses(context.Background())
			must(t, err)
			if !reflect.DeepEqual(st.Tailnet, status) || status.State != TailnetUp || status.HTTPSReason != reason || status.HTTPSAddress != "" || (*joined).Left.Load() {
				t.Fatalf("failure: %+v", status)
			}
			if code, body := exchange(t, webHTTP(), "GET", "http://"+s.TailnetAddr()+Prefix+"/health", "", "", httpsNodeName); code != 200 {
				t.Fatalf("HTTP: %d %s", code, body)
			}
			if got := s.notifier.link(context.Background(), s.current()); got != "http://"+httpsNodeName+"/" {
				t.Fatal(got)
			}
		})
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
	// Wait for the notifier's first pass before adding the first new entry.
	eventually(t, "webhook was not enabled", func() bool { return readLedger(t, opts).Enabled })
	secure := nodeHTTPSClient(t, *joined)
	if code, body := exchange(t, secure, "GET", httpsNodeAddress+"v1/health", "", "", httpsNodeName); code != 200 {
		t.Fatalf("TLS: %d %s", code, body)
	}
	inbox.set(entryOf(InboxEscalation, 1))
	if got := hook.await(t, 1)[0]; !strings.HasSuffix(got, "Open: "+httpsNodeAddress+"\n") {
		t.Fatal(got)
	}
	node, httpAddr := *joined, s.TailnetAddr()
	node.DropHTTPS()
	status := awaitHTTPS(t, c, api.TailnetHTTPSUnavailable)
	if status.State != TailnetUp || !strings.Contains(status.HTTPSReason, "HTTPS listener stopped") || status.HTTPSAddress != "" || node.Left.Load() || s.TailnetAddr() != httpAddr {
		t.Fatalf("TLS failure: %+v", status)
	}
	if code, body := exchange(t, webHTTP(), "GET", "http://"+httpAddr+Prefix+"/health", "", "", httpsNodeName); code != 200 {
		t.Fatalf("HTTP after TLS failure: %d %s", code, body)
	}
	inbox.set(entryOf(InboxEscalation, 1), entryOf(InboxEscalation, 2))
	if got := hook.await(t, 2)[1]; !strings.HasSuffix(got, "Open: http://"+httpsNodeName+"/\n") {
		t.Fatal(got)
	}
}

func TestTailnetHTTPSRejoinClosesSupersededListener(t *testing.T) {
	t.Parallel()
	opts := fixture(t)
	withListen(t, opts, "tailnet = \"osmia\"\n")
	opts.TailnetRetry = time.Millisecond
	var joined atomic.Pointer[fakeTailnet]
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
	must(t, err)
	defer conn.Close()
	_, err = fmt.Fprintf(conn, "GET /v1/health HTTP/1.1\r\nHost: %s\r\n\r\n", httpsNodeName)
	must(t, err)
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	must(t, err)
	resp.Body.Close()
	// Fail HTTP acceptance without closing accepted TLS connections ourselves.
	must(t, first.Close())
	eventually(t, "node never replaced", func() bool { return joined.Load() != first && s.TailnetAddr() != "" })
	awaitHTTPS(t, c, api.TailnetHTTPSUp)
	if !first.HTTPSClosed() || !first.Left.Load() {
		t.Fatal("superseded TLS listener not closed")
	}
	conn.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("superseded TLS connection remains open")
	}
	// A late update from the old generation cannot overwrite the new state.
	s.tailnet.SetHTTPS(first, api.TailnetHTTPSUnavailable, "old failure", "")
	if got := s.tailnetStatus(context.Background()); got.HTTPSAddress != httpsNodeAddress || got.HTTPSState != api.TailnetHTTPSUp {
		t.Fatalf("new node status: %+v", got)
	}
	second := joined.Load()
	if code, body := exchange(t, nodeHTTPSClient(t, second), "GET", httpsNodeAddress+"v1/health", "", "", httpsNodeName); code != 200 {
		t.Fatalf("replacement TLS: %d %s", code, body)
	}
	must(t, s.Close())
	if !second.HTTPSClosed() || !second.Left.Load() {
		t.Fatal("current TLS listener not closed on shutdown")
	}
}
