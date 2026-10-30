package tailnet

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net"
	"net/http/httptest"
	"strings"
	"testing"

	"tailscale.com/ipn/ipnstate"
)

type fakeCertificateClient struct {
	status    *ipnstate.Status
	cert      tls.Certificate
	err       error
	requested string
}

func (c *fakeCertificateClient) StatusWithoutPeers(context.Context) (*ipnstate.Status, error) {
	return c.status, nil
}
func (c *fakeCertificateClient) CertPair(_ context.Context, name string) ([]byte, []byte, error) {
	c.requested = name
	if c.err != nil {
		return nil, nil, c.err
	}
	key, err := x509.MarshalPKCS8PrivateKey(c.cert.PrivateKey)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.cert.Certificate[0]}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}), nil
}
func (c *fakeCertificateClient) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return &c.cert, nil
}

func TestHTTPSChecksIssuanceBeforeOpeningPort443(t *testing.T) {
	t.Parallel()
	// httptest provides test key material; no Tailscale client is used.
	server := httptest.NewTLSServer(nil)
	cert := server.TLS.Certificates[0]
	server.Close()
	const name = "osmia.tail0.ts.net"
	for _, tc := range []struct {
		domains []string
		failure error
		want    string
	}{
		{nil, nil, "enable HTTPS certificates"},
		{[]string{"different.tail0.ts.net"}, nil, "enable HTTPS certificates"},
		{[]string{name}, errors.New("issuer unavailable"), "issuer unavailable"},
		{[]string{name}, nil, ""},
	} {
		client := &fakeCertificateClient{status: &ipnstate.Status{Self: &ipnstate.PeerStatus{DNSName: name + "."}, CertDomains: tc.domains}, cert: cert, err: tc.failure}
		opened := false
		listener, fullName, err := listenHTTPS(context.Background(), client, func(network, address string) (net.Listener, error) {
			opened = true
			if network != "tcp" || address != ":443" || client.requested != name {
				t.Fatalf("opened %s %s after requesting %s", network, address, client.requested)
			}
			return net.Listen("tcp", "127.0.0.1:0")
		})
		if tc.want != "" {
			if err == nil || !strings.Contains(err.Error(), tc.want) || opened || listener != nil {
				t.Fatalf("failure %v; opened %v", err, opened)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		listener.Close()
		if !opened || fullName != name {
			t.Fatalf("opened=%v name=%s", opened, fullName)
		}
	}
}
