package client

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/coreadapter"
)

const localMCPPath = "/mcp"

func randomToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func sameToken(got, want string) bool {
	a, b := sha256.Sum256([]byte(got)), sha256.Sum256([]byte(want))
	return subtle.ConstantTimeCompare(a[:], b[:]) == 1
}

// startMCP opens the local MCP listener the turn's sandbox reaches its tools
// by, where the service would have opened the turn's own, and forwards each
// call to the turn's relay on the server under the turn's bearer token. The
// sandbox holds a token of its own for the listener and never sees the turn's.
// It returns the endpoint to give the session and the function that closes the
// listener.
func (c *Client) startMCP(ctx context.Context, t *turn, mode string) (coreadapter.Endpoint, func(), error) {
	listener, address, err := coreadapter.ListenMCP(ctx, mode, agent.ContainerEngine)
	if err != nil {
		return coreadapter.Endpoint{}, nil, err
	}
	target, err := url.Parse(c.base + t.offer.ToolEndpoint)
	if err != nil {
		_ = listener.Close()
		return coreadapter.Endpoint{}, nil, err
	}
	local := randomToken()
	proxy := &httputil.ReverseProxy{
		Transport: c.http.Transport,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme, pr.Out.URL.Host = target.Scheme, target.Host
			pr.Out.URL.Path = strings.TrimSuffix(target.Path, "/") + strings.TrimPrefix(pr.In.URL.Path, localMCPPath)
			pr.Out.URL.RawPath = ""
			pr.Out.Host = target.Host
			pr.Out.Header.Set("Authorization", "Bearer "+t.toolToken())
			for _, h := range []string{"X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "Forwarded"} {
				pr.Out.Header.Del(h)
			}
		},
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			c.log.Warn("cannot forward a tool call to the server", "turn", t.offer.Turn, "error", err)
			http.Error(w, "the server's tool endpoint is not reachable", http.StatusBadGateway)
		},
	}
	server := &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, bearer := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !bearer || !sameToken(token, local) || r.URL.Path != localMCPPath && !strings.HasPrefix(r.URL.Path, localMCPPath+"/") {
			http.Error(w, "the request does not carry this turn's token", http.StatusUnauthorized)
			return
		}
		proxy.ServeHTTP(w, r)
	})}
	go func() { _ = server.Serve(listener) }()
	endpoint := coreadapter.Endpoint{URL: "http://" + address + localMCPPath, BearerTokenEnvironment: coreadapter.TokenEnvironment, Token: local}
	return endpoint, func() { _ = server.Close() }, nil
}
