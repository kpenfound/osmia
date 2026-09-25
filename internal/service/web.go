package service

import (
	"mime"
	"net"
	"net/http"
	"strings"

	"github.com/kpenfound/osmia/internal/config"
)

// webAllowed keeps a loopback listener from being reached through a browser
// on another site's behalf. The Host header must name a loopback host, which
// refuses DNS rebinding, and a request that is not a read must carry a JSON
// content type, which a cross-site form cannot send without a preflight the
// service never grants.
func webAllowed(r *http.Request) bool {
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
