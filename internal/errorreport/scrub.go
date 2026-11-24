package errorreport

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/kpenfound/osmia/internal/redact"
)

// Placeholders the scrubber leaves where it removed a value.
const (
	PlaceholderRepo  = "<repo>"
	PlaceholderPath  = "<path>"
	PlaceholderRef   = "<ref>"
	PlaceholderSHA   = "<sha>"
	PlaceholderHost  = "<host>"
	PlaceholderEmail = "<email>"
	redacted         = redact.Marker
)

// ScrubError reports why text could not be scrubbed or a report could not be
// built. Nothing is filed when it is returned; Reason is safe to show as a
// reporter diagnostic and never holds the text that failed.
type ScrubError struct {
	Reason string
}

func (e *ScrubError) Error() string { return "error report not filed: " + e.Reason }

// ScrubOptions are the plain inputs scrubbing depends on.
type ScrubOptions struct {
	// Repository is the target repository as owner/name. References to it
	// stay in scrubbed text; every other owner/name reference is replaced.
	Repository string
	// BuildCommit is the Osmia build commit. A commit SHA equal to it, or
	// an abbreviation of it, stays in scrubbed text.
	BuildCommit string
	// Secrets are secret values to remove in addition to
	// redact.KnownSecrets().
	Secrets []string
}

// Scrubber removes everything that must not reach a public repository from
// free text. It holds no state between calls and is safe for concurrent use.
type Scrubber struct {
	repo   string
	commit string
	extra  []string
	hook   func(string) (string, error)
}

// NewScrubber returns a Scrubber for opts.
func NewScrubber(opts ScrubOptions) *Scrubber {
	return &Scrubber{
		repo:   strings.ToLower(strings.TrimSpace(opts.Repository)),
		commit: strings.ToLower(strings.TrimSpace(opts.BuildCommit)),
		extra:  append([]string(nil), opts.Secrets...),
	}
}

// Scrub returns text with credentials, repository references, filesystem
// paths, branch names and git refs, commit SHAs, hosts and email addresses
// replaced by placeholders. It fails closed: an internal error or a panic
// returns an empty string and a *ScrubError.
func (s *Scrubber) Scrub(text string) (out string, err error) {
	defer func() {
		if r := recover(); r != nil {
			out, err = "", &ScrubError{Reason: fmt.Sprintf("scrubber panicked: %v", r)}
		}
	}()
	out = s.scrub(text)
	if s.hook != nil {
		var herr error
		if out, herr = s.hook(out); herr != nil {
			return "", &ScrubError{Reason: "scrubber failed: " + herr.Error()}
		}
	}
	if reason := s.leak(out); reason != "" {
		return "", &ScrubError{Reason: reason}
	}
	return out, nil
}

func (s *Scrubber) leak(text string) string {
	return leakReason(text, s.secrets()...)
}

func leakReason(text string, secrets ...string) string {
	if strings.ContainsRune(text, 0) {
		return "scrubber left an internal marker in the text"
	}
	for _, secret := range secrets {
		if secret != "" && strings.Contains(text, secret) {
			return "scrubber left a known secret in the text"
		}
	}
	if tokenShaped(text) {
		return "scrubber left a token-shaped value in the text"
	}
	return ""
}

func (s *Scrubber) secrets() []string {
	return append(redact.KnownSecrets(), s.extra...)
}

func tokenShaped(text string) bool {
	for _, rule := range redact.ShapedRules[:redact.TokenRules] {
		if rule.Re.MatchString(text) {
			return true
		}
	}
	return false
}

var (
	keptRe   = regexp.MustCompile(`\x00(\d+)\x00`)
	urlRe    = regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*)://((?:\[[0-9a-f:.]*\])?[^\s"'<>)\]]*)`)
	emailRe  = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)+`)
	restRe   = regexp.MustCompile(`(^|[^A-Za-z0-9_./{}-])(/?(?:repos|orgs|users|repositories)/[^\s"'<>?#)\]]*)(\?[^\s"'<>)\]]*)?`)
	winPath  = regexp.MustCompile(`\b[A-Za-z]:\\[^\s"'<>]+`)
	homePath = regexp.MustCompile(`(^|[\s"'(=,\[:])(~/[^\s"'<>(),;\[\]]*)`)
	absPath  = regexp.MustCompile(`(^|[\s"'(=,\[:])(/[^\s"'<>(),;\[\]]*[^\s"'<>(),;:.\[\]/])`)

	remoteRef = regexp.MustCompile(`\b(?:refs/(?:heads|tags|remotes|pull|for|merge-requests)/[^\s"'),;]+|(?:origin|upstream)/[^\s"'),;]+)`)
	namedRef  = regexp.MustCompile("(?i)\\b(branch(?:es)?|refspec|ref|tag|revision)(\\s*[:=]\\s*|\\s+)(?:named\\s+)?(['\"`]?)([^\\s'\"`,;)\\]]+)(['\"`]?)")
	headBase  = regexp.MustCompile(`(?i)\b(head|base)(\s*[:=]\s*)([^\s'",;)\]]+)`)
	gitRemote = regexp.MustCompile(`(?i)\b((?:push|fetch|pull|checkout|merge|rebase)\s+(?:-\S+\s+)*(?:origin|upstream)\s+)([^\s'",;)\]]+)`)

	hexRun  = regexp.MustCompile(`\b[0-9a-fA-F]{7,64}\b`)
	ipv4    = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}(?::\d{1,5})?\b`)
	ipv6    = regexp.MustCompile(`(?i)(^|[^0-9a-z:])((?:[0-9a-f]{1,4}:){7}[0-9a-f]{1,4}|[0-9a-f:]*::[0-9a-f:]*[0-9a-f]|::)`)
	dotHost = regexp.MustCompile(`(?i)\b(?:[a-z0-9](?:[a-z0-9-]*[a-z0-9])?\.)+[a-z]{2,}\b`)
	dialRe  = regexp.MustCompile(`(?i)\b((?:dial|listen)\s+(?:tcp|udp)[46]?\s+)(\S+)`)
	lookup  = regexp.MustCompile(`(?i)\b(lookup\s+)(\S+)`)
	hostKV  = regexp.MustCompile(`(?i)\b(host(?:name)?\s*[:=]\s*)([^\s,;)\]]+)`)
	local   = regexp.MustCompile(`(?i)\blocalhost\b(?::\d{1,5})?`)

	uuidRe  = regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)
	mention = regexp.MustCompile(`@([A-Za-z0-9_-])`)
	spaces  = regexp.MustCompile(`\s+`)
)

var refStopwords = map[string]bool{
	"a": true, "an": true, "the": true, "is": true, "was": true, "not": true, "name": true,
	"names": true, "already": true, "does": true, "did": true, "has": true, "have": true,
	"must": true, "cannot": true, "can": true, "could": true, "should": true, "would": true,
	"will": true, "to": true, "on": true, "from": true, "for": true, "in": true, "of": true,
	"protection": true, "rules": true, "rule": true, "deleted": true, "exists": true,
	"exist": true, "found": true, "missing": true, "conflict": true, "mismatch": true,
	"update": true, "updates": true, "failed": true, "fails": true, "and": true, "or": true,
	"but": true, "with": true, "when": true, "that": true, "which": true, "because": true,
	"at": true, "by": true, "no": true, "unknown": true, "invalid": true, "lookup": true,
}

var fileExtensions = map[string]bool{
	"go": true, "json": true, "toml": true, "yaml": true, "yml": true, "md": true, "txt": true,
	"sh": true, "log": true, "tmp": true, "lock": true, "sum": true, "mod": true, "html": true,
	"css": true, "js": true, "ts": true, "py": true, "db": true, "sock": true, "pid": true,
	"jsonl": true, "sql": true, "diff": true, "patch": true, "zip": true, "tar": true,
	"gz": true, "tgz": true, "png": true, "jpg": true, "svg": true, "cfg": true, "conf": true,
	"ini": true, "env": true,
}

var actionPrefixes = map[string]bool{
	"issues": true, "pulls": true, "workspace": true, "repos": true, "checks": true,
	"git": true, "branches": true, "commits": true, "contents": true,
}

var publicHosts = map[string]bool{"github.com": true, "api.github.com": true}

type scrubRun struct {
	s    *Scrubber
	kept []string
}

func (r *scrubRun) keep(text string) string {
	r.kept = append(r.kept, text)
	return "\x00" + strconv.Itoa(len(r.kept)-1) + "\x00"
}

func (r *scrubRun) restore(text string) string {
	return keptRe.ReplaceAllStringFunc(text, func(m string) string {
		i, err := strconv.Atoi(m[1 : len(m)-1])
		if err != nil || i < 0 || i >= len(r.kept) {
			panic("invalid kept index")
		}
		return r.kept[i]
	})
}

func cleanText(text string) string {
	return strings.Map(func(c rune) rune {
		switch {
		case c == '\n' || c == '\t':
			return c
		case c < 0x20 || c == 0x7f || unicode.Is(unicode.Cf, c):
			return -1
		}
		return c
	}, text)
}

func (s *Scrubber) secretPass(text string) string {
	text = redact.Secrets(text, s.secrets()...)
	return redact.Shaped(text)
}

func (s *Scrubber) scrub(text string) string {
	r := &scrubRun{s: s}
	text = urlRe.ReplaceAllStringFunc(cleanText(text), r.url)
	text = s.secretPass(text)
	text = emailRe.ReplaceAllString(text, PlaceholderEmail)
	text = r.restPaths(text)
	text = winPath.ReplaceAllString(text, PlaceholderPath)
	text = homePath.ReplaceAllString(text, "${1}"+PlaceholderPath)
	text = absPath.ReplaceAllString(text, "${1}"+PlaceholderPath)
	text = r.refs(text)
	text = uuidRe.ReplaceAllStringFunc(text, r.keep)
	text = r.repos(text)
	text = r.shas(text)
	text = r.hosts(text)
	text = mention.ReplaceAllString(text, "(at)${1}")
	text = strings.ReplaceAll(text, "<!--", "<!-")
	text = strings.ReplaceAll(text, "-->", "->")
	text = r.restore(text)
	return strings.TrimSpace(spaces.ReplaceAllString(text, " "))
}

func (r *scrubRun) url(m string) string {
	sub := urlRe.FindStringSubmatch(m)
	scheme := strings.ToLower(sub[1])
	rest := sub[2]
	trail := ""
	for rest != "" && strings.ContainsRune(".,;:!", rune(rest[len(rest)-1])) {
		trail = rest[len(rest)-1:] + trail
		rest = rest[:len(rest)-1]
	}
	if scheme == "file" {
		return PlaceholderPath + trail
	}
	authority, path := rest, ""
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		authority, path = rest[:i], rest[i:]
	}
	if i := strings.LastIndex(authority, "@"); i >= 0 {
		authority = authority[i+1:]
	}
	host := strings.ToLower(authority)
	if !strings.HasPrefix(host, "[") {
		if i := strings.LastIndex(host, ":"); i >= 0 {
			host = host[:i]
		}
	}
	if (scheme == "http" || scheme == "https") && publicHosts[host] {
		out := scheme + "://" + host
		if host == "api.github.com" {
			if tmpl, ok := restTemplate(path); ok {
				out += tmpl
			}
		}
		return r.keep(out) + trail
	}
	return PlaceholderHost + trail
}

func (r *scrubRun) restPaths(text string) string {
	return restRe.ReplaceAllStringFunc(text, func(m string) string {
		sub := restRe.FindStringSubmatch(m)
		prefix, path := sub[1], sub[2]
		trail := ""
		for path != "" && strings.ContainsRune(".,;:!", rune(path[len(path)-1])) {
			trail = path[len(path)-1:] + trail
			path = path[:len(path)-1]
		}
		tmpl, ok := restTemplate("/" + strings.TrimPrefix(path, "/"))
		if !ok {
			return m
		}
		if !strings.HasPrefix(path, "/") {
			tmpl = strings.TrimPrefix(tmpl, "/")
		}
		return prefix + r.keep(tmpl) + trail
	})
}

var restVocabulary = map[string]bool{
	"pulls": true, "issues": true, "comments": true, "reviews": true, "commits": true,
	"branches": true, "git": true, "ref": true, "refs": true, "heads": true, "tags": true,
	"contents": true, "labels": true, "merge": true, "files": true, "statuses": true,
	"status": true, "check-runs": true, "check-suites": true, "compare": true, "events": true,
	"requested_reviewers": true, "assignees": true, "timeline": true, "reactions": true,
	"hooks": true, "releases": true, "collaborators": true, "permission": true,
	"dispatches": true, "actions": true, "runs": true, "jobs": true, "artifacts": true,
	"workflows": true, "readme": true, "trees": true, "blobs": true, "forks": true,
	"protection": true, "lock": true, "repos": true, "members": true, "teams": true,
	"installation": true, "repositories": true, "search": true, "user": true,
}

func isHex(s string) bool {
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return s != ""
}

func isDigits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return s != ""
}

func isParam(seg string) bool {
	return len(seg) > 2 && seg[0] == '{' && seg[len(seg)-1] == '}' && !strings.ContainsAny(seg[1:len(seg)-1], "{}/ ")
}

func restTemplate(path string) (string, bool) {
	if i := strings.IndexAny(path, "?#"); i >= 0 {
		path = path[:i]
	}
	segs := strings.Split(strings.Trim(path, "/"), "/")
	var fixed []string
	switch segs[0] {
	case "repos":
		fixed = []string{"{owner}", "{repo}"}
	case "orgs":
		fixed = []string{"{org}"}
	case "users":
		fixed = []string{"{user}"}
	case "repositories":
		fixed = []string{"{id}"}
	default:
		return "", false
	}
	out := []string{segs[0]}
	rest := segs[1:]
	for i, seg := range rest {
		switch {
		case seg == "":
		case i < len(fixed):
			out = append(out, fixed[i])
		case isParam(seg):
			out = append(out, seg)
		case isDigits(seg):
			out = append(out, "{n}")
		case len(seg) >= 7 && len(seg) <= 64 && isHex(seg):
			out = append(out, "{sha}")
		case restVocabulary[seg]:
			out = append(out, seg)
		default:
			out = append(out, "{id}")
		}
	}
	return "/" + strings.Join(out, "/"), true
}

func (r *scrubRun) refs(text string) string {
	text = remoteRef.ReplaceAllString(text, PlaceholderRef)
	text = gitRemote.ReplaceAllStringFunc(text, func(m string) string {
		sub := gitRemote.FindStringSubmatch(m)
		return sub[1] + refValue(sub[2])
	})
	text = headBase.ReplaceAllStringFunc(text, func(m string) string {
		sub := headBase.FindStringSubmatch(m)
		return sub[1] + sub[2] + refValue(sub[3])
	})
	return namedRef.ReplaceAllStringFunc(text, func(m string) string {
		sub := namedRef.FindStringSubmatch(m)
		value := sub[4]
		if refStopwords[strings.ToLower(value)] {
			return m
		}
		return sub[1] + sub[2] + sub[3] + refValue(value) + sub[5]
	})
}

func refValue(v string) string {
	if strings.HasPrefix(v, "<") || strings.HasPrefix(v, "\x00") || strings.HasPrefix(v, "[") {
		return v
	}
	return PlaceholderRef
}

func (r *scrubRun) shas(text string) string {
	return hexRun.ReplaceAllStringFunc(text, func(m string) string {
		lower := strings.ToLower(m)
		if r.s.commit != "" && len(lower) >= 7 && strings.HasPrefix(r.s.commit, lower) || lower == r.s.commit {
			return m
		}
		if len(m) >= 40 {
			return PlaceholderSHA
		}
		digit, letter := false, false
		for _, c := range m {
			if c >= '0' && c <= '9' {
				digit = true
			} else {
				letter = true
			}
		}
		if digit && letter {
			return PlaceholderSHA
		}
		return m
	})
}

func (r *scrubRun) hosts(text string) string {
	text = ipv4.ReplaceAllString(text, PlaceholderHost)
	text = ipv6.ReplaceAllString(text, "${1}"+PlaceholderHost)
	text = local.ReplaceAllString(text, PlaceholderHost)
	text = dotHost.ReplaceAllStringFunc(text, func(m string) string {
		lower := strings.ToLower(m)
		labels := strings.Split(lower, ".")
		switch {
		case publicHosts[lower]:
			return m
		case len(labels) == 2 && (fileExtensions[labels[1]] || actionPrefixes[labels[0]]):
			return m
		}
		return PlaceholderHost
	})
	for _, re := range []*regexp.Regexp{dialRe, lookup, hostKV} {
		text = re.ReplaceAllStringFunc(text, func(m string) string {
			sub := re.FindStringSubmatch(m)
			if strings.HasPrefix(sub[2], "<") || strings.HasPrefix(sub[2], "\x00") {
				return m
			}
			return sub[1] + PlaceholderHost
		})
	}
	return text
}

func (r *scrubRun) repos(text string) string {
	text = githubRepoRe.ReplaceAllStringFunc(text, func(m string) string {
		sub := githubRepoRe.FindStringSubmatch(m)
		if out, ok := r.repoReplacement(sub[2], sub[3]); ok {
			return sub[1] + out
		}
		return m
	})
	return repoRe.ReplaceAllStringFunc(text, func(m string) string {
		sub := repoRe.FindStringSubmatch(m)
		if out, ok := r.repoReplacement(sub[2], sub[3]); ok {
			return sub[1] + out
		}
		return m
	})
}

func (r *scrubRun) repoReplacement(owner, name string) (string, bool) {
	suffix := ""
	for name != "" && strings.ContainsRune(".-_", rune(name[len(name)-1])) {
		suffix = name[len(name)-1:] + suffix
		name = name[:len(name)-1]
	}
	if name == "" || !hasLetter(owner) || !hasLetter(name) {
		return "", false
	}
	pair := strings.ToLower(owner + "/" + strings.TrimSuffix(name, ".git"))
	if pair == r.s.repo || notRepos[pair] {
		return "", false
	}
	return PlaceholderRepo + suffix, true
}

var notRepos = map[string]bool{
	"i/o": true, "and/or": true, "read/write": true, "tcp/ip": true, "client/server": true,
	"input/output": true, "application/json": true, "text/plain": true, "text/html": true,
	"application/x-www-form-urlencoded": true,
}

var githubRepoRe = regexp.MustCompile(`(?i)(\bgithub\.com[/:])([A-Za-z0-9-]+)/([A-Za-z0-9._-]+)`)

var repoRe = regexp.MustCompile(`(^|[^A-Za-z0-9_./\-{}<])([A-Za-z0-9][A-Za-z0-9-]{0,38})/([A-Za-z0-9._-]+)`)

func hasLetter(s string) bool {
	for _, c := range s {
		if unicode.IsLetter(c) {
			return true
		}
	}
	return false
}
