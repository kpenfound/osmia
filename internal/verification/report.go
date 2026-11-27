package verification

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
	"unicode/utf8"
)

// Limits of the lever contract, shared by the runner and every consumer.
const (
	// ReportFile is the file every lever Directory must hold.
	ReportFile = "report.json"
	// ReportVersion is the only report version the service accepts.
	ReportVersion = 1
	// MaxImageBytes caps each image in a lever's Directory.
	MaxImageBytes = 5 << 20
	// MaxDirectoryBytes caps the whole exported Directory.
	MaxDirectoryBytes = 20 << 20
	// MaxReportBytes caps report.json, matching the largest text preview reads.
	MaxReportBytes = 64 << 10
	// MaxDiagnosticBytes caps the diagnostics kept from a run, matching
	// preview's check output limit.
	MaxDiagnosticBytes = 16 << 10
	// MaxRerunsPerUnit bounds how many mason-declared invocations the service
	// reruns for one unit.
	MaxRerunsPerUnit = 5
)

// Status is the outcome of a claim, a flow or a run. Only Pass is a pass:
// Inconclusive is neither a pass nor a failure.
type Status string

// The statuses a claim, a flow or a run can have.
const (
	Pass         Status = "pass"
	Fail         Status = "fail"
	Inconclusive Status = "inconclusive"
)

// Valid reports whether s is one of the three statuses.
func (s Status) Valid() bool { return s == Pass || s == Fail || s == Inconclusive }

// Worse returns the worse of two statuses: fail, then inconclusive, then pass.
// A status that is not valid counts as inconclusive.
func Worse(a, b Status) Status {
	if rank(b) > rank(a) {
		a = b
	}
	if !a.Valid() {
		return Inconclusive
	}
	return a
}

func rank(s Status) int {
	switch s {
	case Fail:
		return 2
	case Pass:
		return 0
	}
	return 1
}

// Claim is one statement a lever makes about the running build.
type Claim struct {
	Name     string   `json:"name"`
	Status   Status   `json:"status"`
	Detail   string   `json:"detail"`
	Evidence []string `json:"evidence"`
	Flow     string   `json:"flow,omitempty"`
}

// Report is the version-1 report.json of a lever run.
type Report struct {
	Version int     `json:"version"`
	Claims  []Claim `json:"claims"`
}

// Result is what a lever run said. Rejected is empty for a well-formed report.
// Otherwise it is the reason the report was refused: the run is inconclusive,
// carries no claims and covers no flow.
type Result struct {
	Claims   []Claim  `json:"claims,omitempty"`
	Evidence []string `json:"evidence,omitempty"`
	Rejected string   `json:"rejected,omitempty"`
}

// Rejection is the Result of a run whose report was refused for reason.
func Rejection(reason string) Result { return Result{Rejected: reason} }

// Status is the run's overall status. A pass needs at least one claim and
// every claim passing; otherwise any failed claim makes it fail, and anything
// else, including a rejected report or no claims at all, is inconclusive.
func (r Result) Status() Status {
	if r.Rejected != "" || len(r.Claims) == 0 {
		return Inconclusive
	}
	status := Pass
	for _, c := range r.Claims {
		status = Worse(status, c.Status)
	}
	return status
}

// ParseReport decodes and validates report.json text. Evidence paths must be
// clean, relative and inside the Directory; exists reports whether a path is a
// regular file there. It returns the claims and the distinct evidence paths in
// first-use order.
func ParseReport(data []byte, exists func(path string) error) (Result, error) {
	if len(data) > MaxReportBytes {
		return Result{}, fmt.Errorf("%s is larger than %d bytes", ReportFile, MaxReportBytes)
	}
	if !utf8.Valid(data) {
		return Result{}, fmt.Errorf("%s is not UTF-8 text", ReportFile)
	}
	var head struct {
		Version *int `json:"version"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return Result{}, fmt.Errorf("%s is not valid JSON of the report shape: %s", ReportFile, jsonProblem(err))
	}
	if head.Version == nil {
		return Result{}, fmt.Errorf("%s has no version", ReportFile)
	}
	if *head.Version != ReportVersion {
		return Result{}, fmt.Errorf("%s has unsupported version %d, expected %d", ReportFile, *head.Version, ReportVersion)
	}
	var raw rawReport
	if err := json.Unmarshal(data, &raw); err != nil {
		return Result{}, fmt.Errorf("%s is not valid JSON of the report shape: %s", ReportFile, jsonProblem(err))
	}
	if raw.Claims == nil {
		return Result{}, fmt.Errorf("%s has no claims list", ReportFile)
	}
	var result Result
	seen := map[string]bool{}
	for i, c := range *raw.Claims {
		label := fmt.Sprintf("claim %d", i+1)
		if c.Name == nil || strings.TrimSpace(*c.Name) == "" {
			return Result{}, fmt.Errorf("%s has no name", label)
		}
		label = fmt.Sprintf("claim %d (%s)", i+1, boundedText(*c.Name))
		if c.Status == nil {
			return Result{}, fmt.Errorf("%s has no status", label)
		}
		status := Status(*c.Status)
		if !status.Valid() {
			return Result{}, fmt.Errorf("%s has unknown status %q, expected pass, fail or inconclusive", label, boundedText(*c.Status))
		}
		claim := Claim{Name: *c.Name, Status: status, Evidence: []string{}}
		if c.Detail != nil {
			claim.Detail = *c.Detail
		}
		if c.Flow != nil {
			if strings.TrimSpace(*c.Flow) == "" {
				return Result{}, fmt.Errorf("%s has an empty flow tag", label)
			}
			claim.Flow = *c.Flow
		}
		if c.Evidence != nil {
			for _, p := range *c.Evidence {
				clean, err := EvidencePath(p)
				if err != nil {
					return Result{}, fmt.Errorf("%s evidence %q: %w", label, boundedText(p), err)
				}
				if err := exists(clean); err != nil {
					return Result{}, fmt.Errorf("%s evidence %q: %w", label, boundedText(p), err)
				}
				claim.Evidence = append(claim.Evidence, clean)
				if !seen[clean] {
					seen[clean] = true
					result.Evidence = append(result.Evidence, clean)
				}
			}
		}
		result.Claims = append(result.Claims, claim)
	}
	return result, nil
}

type rawClaim struct {
	Name     *string   `json:"name"`
	Status   *string   `json:"status"`
	Detail   *string   `json:"detail"`
	Evidence *[]string `json:"evidence"`
	Flow     *string   `json:"flow"`
}

type rawReport struct {
	Version *int        `json:"version"`
	Claims  *[]rawClaim `json:"claims"`
}

func jsonProblem(err error) string {
	var syntax *json.SyntaxError
	var typed *json.UnmarshalTypeError
	switch {
	case errors.As(err, &syntax):
		return fmt.Sprintf("syntax error at byte %d", syntax.Offset)
	case errors.As(err, &typed):
		return fmt.Sprintf("field %s has the wrong type", typed.Field)
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "unexpected end of input"
	}
	return boundedText(err.Error())
}

func boundedText(s string) string {
	if len(s) <= 80 {
		return s
	}
	cut := 80
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "..."
}

// EvidencePath returns the clean form of an evidence path, which must be
// relative, non-empty, free of control characters and inside the Directory.
func EvidencePath(p string) (string, error) {
	switch {
	case p == "":
		return "", errors.New("path is empty")
	case strings.ContainsFunc(p, func(r rune) bool { return r < 0x20 || r == 0x7f }):
		return "", errors.New("path has control characters")
	case strings.Contains(p, `\`):
		return "", errors.New("path must use / separators")
	case path.IsAbs(p) || strings.HasPrefix(p, "~"):
		return "", errors.New("path must be relative to the Directory")
	}
	clean := path.Clean(p)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", errors.New("path escapes the Directory")
	}
	if clean == ReportFile {
		return "", errors.New("path names the report itself")
	}
	return clean, nil
}

// Load reads and validates report.json in an exported Directory. Reading goes
// through an os.Root, so no path, report or symlink reaches outside dir. A
// missing, oversized, malformed or unsupported report, or one naming evidence
// that is absent, is not a regular file or escapes dir, is returned as a
// rejection: an inconclusive run carrying the reason.
func Load(dir string) Result {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return Rejection("the exported Directory cannot be opened")
	}
	defer root.Close()
	f, err := root.Open(ReportFile)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Rejection(ReportFile + " is missing from the Directory")
		}
		return Rejection(ReportFile + " cannot be read from the Directory")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return Rejection(ReportFile + " is not a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxReportBytes+1))
	if err != nil {
		return Rejection(ReportFile + " cannot be read from the Directory")
	}
	result, err := ParseReport(data, func(p string) error {
		info, err := root.Stat(p)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return errors.New("is not in the Directory")
		case err != nil:
			return errors.New("cannot be resolved inside the Directory")
		case !info.Mode().IsRegular():
			return errors.New("is not a regular file")
		}
		return nil
	})
	if err != nil {
		return Rejection(err.Error())
	}
	return result
}
