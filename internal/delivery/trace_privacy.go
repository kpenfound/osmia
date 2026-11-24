package delivery

import (
	"regexp"
	"strings"
)

var cloudTraceLink = regexp.MustCompile(`https://dagger\.cloud/(?:[A-Za-z0-9_-]+/)?traces/[A-Za-z0-9_-]+/?`)

func withoutCheckTraces(text string) string {
	return strings.TrimSpace(cloudTraceLink.ReplaceAllString(text, ""))
}

func privateCheckTrace(text string) bool { return cloudTraceLink.MatchString(text) }
