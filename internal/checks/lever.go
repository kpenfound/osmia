package checks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/kpenfound/osmia/internal/callargs"
	"github.com/kpenfound/osmia/internal/redact"
	"github.com/kpenfound/osmia/internal/verification"
)

var leverRunID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// ValidLeverRunID reports whether id can be a lever run ID: a short token of
// letters, digits, dashes and underscores that starts with a letter or digit,
// so it can never read as an option or carry a scheme or path.
func ValidLeverRunID(id string) bool { return leverRunID.MatchString(id) }

// LeverCall describes one run of a project's verification lever.
type LeverCall struct {
	// Base is the project's configured base `dagger call` arguments.
	Base []string
	// Args is the argument list the caller wrote. The service never
	// interprets it.
	Args []string
	// RunID is the run ID the service generated for this run. The lever gets
	// it as `--run-id <id>` immediately after Base.
	RunID string
}

// LeverLimitation is the error of a lever run the service refuses or cuts
// short because of a rule of the contract: arguments the preview rules refuse,
// arguments that set the run ID, an image over 5 MiB or a Directory over
// 20 MiB. No Dagger call is started for refused arguments.
type LeverLimitation struct{ Reason string }

func (e *LeverLimitation) Error() string { return e.Reason }

func limitation(format string, args ...any) error {
	return &LeverLimitation{Reason: fmt.Sprintf(format, args...)}
}

// LeverRun is a finished lever run. Dir is the exported Directory in a
// service temporary directory: the caller copies what it retains and then
// calls Close, which removes it.
type LeverRun struct {
	// Dir is the exported Directory.
	Dir string
	// Result is the validated report. A run whose report was refused holds
	// the reason in Result.Rejected and no claims.
	Result verification.Result
	// Diagnostics is the tail of the run's output, within preview's text
	// limit and with known secrets removed.
	Diagnostics string
}

// EvidencePaths are the absolute paths in the export of the files the report
// references, in first-use order.
func (r *LeverRun) EvidencePaths() []string {
	out := make([]string, len(r.Result.Evidence))
	for i, p := range r.Result.Evidence {
		out[i] = filepath.Join(r.Dir, filepath.FromSlash(p))
	}
	return out
}

// Close removes the exported Directory.
func (r *LeverRun) Close() error {
	if r.Dir == "" {
		return nil
	}
	return os.RemoveAll(filepath.Dir(r.Dir))
}

// RunLever runs `dagger call <base> --run-id <id> <args> export --path <tmp>`
// in dir, an export of the target commit, and validates the Directory the
// lever returns. It runs only Dagger clients on the host: the product's
// Service is bound inside the lever's driver, and no host port is published.
//
// The caller's arguments are checked first, with preview's agent-argument
// rules, the source-path check on dir, and a refusal of any argument that sets
// the run ID. A refusal is a *LeverLimitation and starts no Dagger call, as
// does an image over verification.MaxImageBytes or a Directory over
// verification.MaxDirectoryBytes. Known secrets are removed from the report,
// from every text file in the Directory and from the diagnostics.
func (d DaggerChecks) RunLever(ctx context.Context, dir string, in LeverCall) (*LeverRun, error) {
	if err := callargs.Validate("lever call", in.Args); err != nil {
		return nil, &LeverLimitation{Reason: err.Error()}
	}
	if callargs.SetsRunID(in.Args) {
		return nil, limitation("lever call cannot set %s: the service gives every run its own", callargs.RunIDFlag)
	}
	if err := callargs.ValidateSource("lever call", dir, in.Args); err != nil {
		return nil, &LeverLimitation{Reason: err.Error()}
	}
	if err := validateLeverBase(in.Base); err != nil {
		return nil, err
	}
	if !ValidLeverRunID(in.RunID) {
		return nil, limitation("lever run ID %q is not a plain token", in.RunID)
	}
	parent, err := os.MkdirTemp("", "osmia-lever-")
	if err != nil {
		return nil, err
	}
	keep := false
	defer func() {
		if !keep {
			os.RemoveAll(parent)
		}
	}()
	out := filepath.Join(parent, "export")
	if err := os.Mkdir(out, 0o700); err != nil {
		return nil, err
	}
	args := append([]string{"call"}, in.Base...)
	args = append(args, callargs.RunIDFlag, in.RunID)
	args = append(args, in.Args...)
	args = append(args, "export", "--path", out)
	output := &checkOutput{limit: verification.MaxDiagnosticBytes}
	exit, _, err := d.dagger(ctx, dir, Identity{}, false, output, args...)
	if err != nil {
		return nil, redactedFailure("lever call", err, output)
	}
	if exit != 0 {
		return nil, redactedFailure("lever call", fmt.Errorf("dagger lever call exited %d", exit), output)
	}
	if err := checkLeverSizes(out); err != nil {
		return nil, err
	}
	secrets := redact.KnownSecrets()
	if err := redactLeverFiles(out, secrets); err != nil {
		return nil, err
	}
	result := verification.Load(out)
	redacted := redactResult(result, secrets)
	if !sameResult(result, redacted) {
		if err := rewriteReport(out, redacted); err != nil {
			return nil, err
		}
	}
	keep = true
	return &LeverRun{Dir: out, Result: redacted, Diagnostics: redactedTail(output, secrets)}, nil
}

// LeverFlows runs `dagger call <base> list` in dir and returns the flow names
// the lever lists, parsed from its JSON output. The run ID is not passed.
func (d DaggerChecks) LeverFlows(ctx context.Context, dir string, base []string) ([]string, error) {
	if err := validateLeverBase(base); err != nil {
		return nil, err
	}
	stdout := &checkOutput{limit: verification.MaxFlowListBytes}
	stderr := &checkOutput{limit: verification.MaxDiagnosticBytes}
	args := append(append([]string{"call"}, base...), "list")
	exit, _, err := d.daggerOutput(ctx, dir, Identity{}, false, stdout, stderr, args...)
	if err != nil {
		return nil, redactedFailure("lever list", err, stderr)
	}
	if exit != 0 {
		stderr.Write([]byte(stdout.String()))
		return nil, redactedFailure("lever list", fmt.Errorf("dagger lever list exited %d", exit), stderr)
	}
	if stdout.truncated {
		return nil, fmt.Errorf("lever list: output is larger than %d bytes", verification.MaxFlowListBytes)
	}
	flows, err := verification.ParseFlowList([]byte(stdout.String()))
	if err != nil {
		return nil, fmt.Errorf("lever list: %w", err)
	}
	return flows, nil
}

func validateLeverBase(base []string) error {
	if len(base) == 0 {
		return limitation("the project has no verification setting")
	}
	if err := callargs.Validate("verification call", base); err != nil {
		return &LeverLimitation{Reason: err.Error()}
	}
	if callargs.SetsRunID(base) {
		return limitation("verification call cannot set %s", callargs.RunIDFlag)
	}
	return nil
}

var imageExtensions = map[string]bool{".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true, ".bmp": true, ".ico": true, ".avif": true, ".svg": true}

func isImage(name string, head []byte) bool {
	return imageExtensions[strings.ToLower(filepath.Ext(name))] || strings.HasPrefix(http.DetectContentType(head), "image/")
}

// IsImageFile reports whether a lever evidence file is an image, by its
// extension or the type its first bytes show.
func IsImageFile(name string, head []byte) bool { return isImage(name, head) }

func readLeverHead(name string) ([]byte, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	head := make([]byte, 512)
	n, err := io.ReadFull(f, head)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, err
	}
	return head[:n], nil
}

func checkLeverSizes(dir string) error {
	var total int64
	return filepath.WalkDir(dir, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		if total > verification.MaxDirectoryBytes {
			return limitation("the lever's Directory exceeds %d MiB", verification.MaxDirectoryBytes>>20)
		}
		if info.Size() > verification.MaxImageBytes {
			head, err := readLeverHead(name)
			if err != nil {
				return err
			}
			if isImage(name, head) {
				rel, _ := filepath.Rel(dir, name)
				return limitation("image %s exceeds %d MiB", filepath.ToSlash(rel), verification.MaxImageBytes>>20)
			}
		}
		return nil
	})
}

func redactLeverFiles(dir string, secrets []string) error {
	return filepath.WalkDir(dir, func(name string, entry fs.DirEntry, err error) error {
		if err != nil || !entry.Type().IsRegular() {
			return err
		}
		data, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		if isImage(name, data[:min(len(data), 512)]) || !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
			return nil
		}
		clean := redact.Secrets(string(data), secrets...)
		if clean == string(data) {
			return nil
		}
		if err := os.Chmod(name, 0o600); err != nil {
			return err
		}
		return os.WriteFile(name, []byte(clean), 0o600)
	})
}

func redactResult(r verification.Result, secrets []string) verification.Result {
	out := verification.Result{Evidence: r.Evidence, Rejected: redact.Secrets(r.Rejected, secrets...)}
	for _, c := range r.Claims {
		c.Name = redact.Secrets(c.Name, secrets...)
		c.Detail = redact.Secrets(c.Detail, secrets...)
		c.Flow = redact.Secrets(c.Flow, secrets...)
		out.Claims = append(out.Claims, c)
	}
	return out
}

func sameResult(a, b verification.Result) bool {
	if a.Rejected != b.Rejected || len(a.Claims) != len(b.Claims) {
		return false
	}
	for i := range a.Claims {
		x, y := a.Claims[i], b.Claims[i]
		if x.Name != y.Name || x.Detail != y.Detail || x.Flow != y.Flow {
			return false
		}
	}
	return true
}

func rewriteReport(dir string, r verification.Result) error {
	if r.Rejected != "" {
		return nil
	}
	data, err := json.Marshal(verification.Report{Version: verification.ReportVersion, Claims: r.Claims})
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, verification.ReportFile), data, 0o600)
}
