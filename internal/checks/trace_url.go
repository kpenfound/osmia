package checks

import (
	"net/url"
	"regexp"
	"strings"
)

var reportURLs = regexp.MustCompile(`https://[^\s<>"'\x1b]+`)
var tracePath = regexp.MustCompile(`^/(?:[A-Za-z0-9_-]+/)?traces/[A-Za-z0-9_-]+/?$`)

func cloudTraceURL(output string) string {
	for _, candidate := range reportURLs.FindAllString(output, -1) {
		candidate = strings.TrimRight(candidate, ").,;")
		u, err := url.Parse(candidate)
		if err == nil && u.Scheme == "https" && u.Host == "dagger.cloud" && u.User == nil && u.RawQuery == "" && u.Fragment == "" && tracePath.MatchString(u.Path) && u.RawPath == "" {
			return candidate
		}
	}
	return ""
}
