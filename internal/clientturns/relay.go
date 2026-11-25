package clientturns

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"

	"github.com/kpenfound/osmia/internal/api"
)

type relayTarget struct {
	upstream *url.URL
	token    string
	proxy    *httputil.ReverseProxy
}

// Relay passes a remote turn's MCP traffic from the tailnet API to the turn's
// own MCP endpoint. It answers only calls that carry the turn's bearer token,
// and forwards them with that same token, so the endpoint handles and audits
// each call as it does a local turn's.
type Relay struct {
	mu      sync.Mutex
	targets map[string]*relayTarget
}

// NewRelay returns a relay with no turns.
func NewRelay() *Relay { return &Relay{targets: map[string]*relayTarget{}} }

// Register makes the turn's relay path forward to endpoint under token and
// returns the function that ends it.
func (r *Relay) Register(turn, endpoint, token string) (release func(), err error) {
	upstream, err := url.Parse(endpoint)
	if err != nil {
		return nil, err
	}
	target := &relayTarget{upstream: upstream, token: token}
	target.proxy = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			_, rest, _ := api.MCPRoute(pr.In.URL.Path)
			pr.Out.URL.Scheme, pr.Out.URL.Host = upstream.Scheme, upstream.Host
			pr.Out.URL.Path = strings.TrimSuffix(upstream.Path, "/") + rest
			pr.Out.URL.RawPath = ""
			pr.Out.Host = upstream.Host
			pr.Out.Header.Set("Authorization", "Bearer "+token)
			for _, h := range []string{"X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "Forwarded"} {
				pr.Out.Header.Del(h)
			}
		},
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			http.Error(w, "the turn's tool endpoint is not reachable", http.StatusBadGateway)
		},
	}
	r.mu.Lock()
	r.targets[turn] = target
	r.mu.Unlock()
	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.targets[turn] == target {
			delete(r.targets, turn)
		}
	}, nil
}

// Serve forwards one request to the turn's MCP endpoint. It returns the
// refusal to send instead when the turn has no relay or the request does not
// carry the turn's bearer token.
func (r *Relay) Serve(w http.ResponseWriter, req *http.Request, turn string) *api.APIError {
	r.mu.Lock()
	target := r.targets[turn]
	r.mu.Unlock()
	token, bearer := strings.CutPrefix(req.Header.Get("Authorization"), "Bearer ")
	if target == nil || !bearer || !sameToken(token, target.token) {
		return &api.APIError{Code: api.Forbidden, Message: "the request does not carry the turn's bearer token"}
	}
	target.proxy.ServeHTTP(w, req)
	return nil
}

func sameToken(got, want string) bool {
	a, b := sha256.Sum256([]byte(got)), sha256.Sum256([]byte(want))
	return subtle.ConstantTimeCompare(a[:], b[:]) == 1
}
