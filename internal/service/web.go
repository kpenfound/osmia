package service

import (
	"mime"
	"net"
	"net/http"
	"strings"

	"github.com/kpenfound/osmia/internal/config"
)

// overWeb reports whether r arrived on the TCP web listener rather than the
// Unix socket.
func overWeb(r *http.Request) bool {
	_, tcp := r.Context().Value(http.LocalAddrContextKey).(*net.TCPAddr)
	return tcp
}

// webAllowed keeps a loopback listener from being reached through a browser
// on another site's behalf. The Host header must name a loopback host, which
// refuses DNS rebinding, and a request that is not a read must carry a JSON
// content type, which a cross-site form cannot send without a preflight the
// service never grants.
func webAllowed(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.Host)
	if err != nil {
		host = strings.TrimSuffix(strings.TrimPrefix(r.Host, "["), "]")
	}
	if !config.LoopbackHost(host) {
		return false
	}
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return true
	}
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return err == nil && media == "application/json"
}
