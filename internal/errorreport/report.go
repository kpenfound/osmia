// Package errorreport holds what the error reporter decides without touching
// the network or the service: it turns a captured error event into a
// public-safe report, groups reports by fingerprint, renders the issue and
// comment text, keeps the repository-scoped state of what has been filed and
// decides what to do with each occurrence.
package errorreport

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Source says where an error came from.
type Source string

const (
	// SourceAPI is Osmia's own HTTP API answering a 5xx status.
	SourceAPI Source = "api"
	// SourceOperation is a failed service operation.
	SourceOperation Source = "operation"
	// SourceGitHub is a failed GitHub REST call.
	SourceGitHub Source = "github"
)

// Valid reports whether s is a known source.
func (s Source) Valid() bool {
	return s == SourceAPI || s == SourceOperation || s == SourceGitHub
}

// MaxMessageRunes bounds the scrubbed message shown in a report.
const MaxMessageRunes = 500

// Event is a captured error. Only the fields other than Attributes can reach
// a report, and every free-text field is scrubbed first.
type Event struct {
	Source Source
	// Template is the action or the endpoint route template, never a
	// concrete path.
	Template string
	// Status is the HTTP status, or zero when there was none.
	Status int
	// Code is the error code, if the error has one.
	Code    string
	Message string
	// ProjectID, WorkstreamID and UnitID are opaque Osmia IDs.
	ProjectID    string
	WorkstreamID string
	UnitID       string
	Time         time.Time
	// Attributes are the free-form attributes of the log record. They are
	// never reported.
	Attributes map[string]string
}

// Report is an Event reduced to the fields a public issue may hold.
type Report struct {
	Source       Source    `json:"source"`
	Template     string    `json:"template"`
	Status       int       `json:"status,omitempty"`
	Code         string    `json:"code,omitempty"`
	Message      string    `json:"message"`
	ProjectID    string    `json:"project_id,omitempty"`
	WorkstreamID string    `json:"workstream_id,omitempty"`
	UnitID       string    `json:"unit_id,omitempty"`
	Time         time.Time `json:"time"`
	Fingerprint  string    `json:"fingerprint"`
}

var (
	idRe       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,95}$`)
	codeRe     = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,64}$`)
	actionRe   = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.:-]{0,63}$`)
	methodRe   = regexp.MustCompile(`^(GET|HEAD|POST|PUT|PATCH|DELETE|OPTIONS)$`)
	segmentRe  = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9._~-]*$`)
	instanceRe = regexp.MustCompile(`[0-9]|^[a-z]{1,4}_[A-Za-z0-9]+$`)
)

// Report scrubs ev and returns the report to file. It fails closed: when
// anything in ev cannot be shown safely, or the scrubber fails or panics, it
// returns a *ScrubError and no report.
func (s *Scrubber) Report(ev Event) (rep Report, err error) {
	defer func() {
		if r := recover(); r != nil {
			rep, err = Report{}, &ScrubError{Reason: fmt.Sprintf("building the report panicked: %v", r)}
		}
	}()
	if !ev.Source.Valid() {
		return Report{}, &ScrubError{Reason: "unknown error source"}
	}
	template, err := s.scrubTemplate(ev.Template)
	if err != nil {
		return Report{}, err
	}
	code, err := s.scrubCode(ev.Code)
	if err != nil {
		return Report{}, err
	}
	message, err := s.Scrub(ev.Message)
	if err != nil {
		return Report{}, err
	}
	for _, id := range []string{ev.ProjectID, ev.WorkstreamID, ev.UnitID} {
		if id != "" && !idRe.MatchString(id) {
			return Report{}, &ScrubError{Reason: "an ID is not an opaque Osmia ID"}
		}
	}
	if ev.Status < 0 || ev.Status > 999 {
		return Report{}, &ScrubError{Reason: "status is not an HTTP status"}
	}
	rep = Report{
		Source:       ev.Source,
		Template:     template,
		Status:       ev.Status,
		Code:         code,
		Message:      truncate(message, MaxMessageRunes),
		ProjectID:    ev.ProjectID,
		WorkstreamID: ev.WorkstreamID,
		UnitID:       ev.UnitID,
		Time:         ev.Time.UTC(),
	}
	rep.Fingerprint = Fingerprint(ev.Source, template, ev.Status, code, message)
	return rep, nil
}

func truncate(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "…"
}

// scrubCode scrubs an error code. Codes are short identifiers; anything else
// goes through full scrubbing and is shortened.
func (s *Scrubber) scrubCode(code string) (string, error) {
	code = strings.TrimSpace(code)
	if code == "" {
		return "", nil
	}
	if codeRe.MatchString(code) {
		out, err := s.checked(s.secretPass(code))
		if err != nil {
			return "", err
		}
		return out, nil
	}
	out, err := s.Scrub(code)
	if err != nil {
		return "", err
	}
	return truncate(out, 64), nil
}

// scrubTemplate scrubs an action or an endpoint route template. Route
// parameters in braces stay; a path segment that looks like instance data
// becomes {id}, so a concrete path never reaches a report.
func (s *Scrubber) scrubTemplate(template string) (string, error) {
	fields := strings.Fields(cleanText(template))
	if len(fields) == 0 {
		return "", &ScrubError{Reason: "the error has no action or endpoint template"}
	}
	out := make([]string, 0, len(fields))
	for _, field := range fields {
		switch {
		case methodRe.MatchString(field):
			out = append(out, field)
		case strings.HasPrefix(field, "/") || strings.HasPrefix(field, "repos/"):
			out = append(out, templatePath(field))
		case actionRe.MatchString(field):
			out = append(out, field)
		default:
			scrubbed, err := s.Scrub(field)
			if err != nil {
				return "", err
			}
			out = append(out, scrubbed)
		}
	}
	return s.checked(s.secretPass(strings.Join(out, " ")))
}

// checked applies the scrubber's result hook and final checks to text that a
// narrower pass produced.
func (s *Scrubber) checked(text string) (out string, err error) {
	defer func() {
		if r := recover(); r != nil {
			out, err = "", &ScrubError{Reason: fmt.Sprintf("scrubber panicked: %v", r)}
		}
	}()
	if s.hook != nil {
		var herr error
		if text, herr = s.hook(text); herr != nil {
			return "", &ScrubError{Reason: "scrubber failed: " + herr.Error()}
		}
	}
	if reason := s.leak(text); reason != "" {
		return "", &ScrubError{Reason: reason}
	}
	return text, nil
}

func templatePath(path string) string {
	if i := strings.IndexAny(path, "?#"); i >= 0 {
		path = path[:i]
	}
	if tmpl, ok := restTemplate("/" + strings.TrimPrefix(path, "/")); ok {
		return tmpl
	}
	segs := strings.Split(strings.Trim(path, "/"), "/")
	for i, seg := range segs {
		switch {
		case isParam(seg) || seg == "*":
		case segmentRe.MatchString(seg) && !instanceRe.MatchString(seg) && !(len(seg) >= 7 && isHex(seg)):
		default:
			segs[i] = "{id}"
		}
	}
	return "/" + strings.Join(segs, "/")
}

var (
	timestampRe = regexp.MustCompile(`\d{4}-\d{2}-\d{2}(?:[T ]\d{2}:\d{2}(?::\d{2}(?:\.\d+)?)?(?:Z|[+-]\d{2}:?\d{2})?)?|\b\d{1,2}:\d{2}:\d{2}(?:\.\d+)?\b`)
	opaqueIDRe  = regexp.MustCompile(`\b[a-z]{1,4}_[0-9a-f]{8,}\b`)
	durationRe  = regexp.MustCompile(`\b(?:\d+(?:\.\d+)?(?:ns|us|µs|ms|s|m|h))+\b`)
	numberRe    = regexp.MustCompile(`\d+(?:\.\d+)?`)
	hashRe      = regexp.MustCompile(`\b[0-9a-fA-F]{7,}\b`)
)

// Normalize reduces a scrubbed message to the form the fingerprint hashes:
// timestamps, IDs, hashes, durations and numbers become fixed tokens and
// whitespace collapses.
func Normalize(message string) string {
	message = timestampRe.ReplaceAllString(message, "<time>")
	message = uuidRe.ReplaceAllString(message, "<id>")
	message = opaqueIDRe.ReplaceAllString(message, "<id>")
	message = hashRe.ReplaceAllStringFunc(message, func(m string) string {
		for _, c := range m {
			if c >= '0' && c <= '9' {
				if isDigits(m) {
					return m
				}
				return "<hash>"
			}
		}
		return m
	})
	message = durationRe.ReplaceAllString(message, "<n>")
	message = numberRe.ReplaceAllString(message, "<n>")
	return strings.Join(strings.Fields(message), " ")
}

// Fingerprint groups occurrences of one error: it hashes the source, the
// template, the status, the code and the normalised scrubbed message.
func Fingerprint(source Source, template string, status int, code, scrubbedMessage string) string {
	h := sha256.New()
	for _, part := range []string{string(source), template, strconv.Itoa(status), code, Normalize(scrubbedMessage)} {
		h.Write([]byte(part))
		h.Write([]byte{0x1f})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}
