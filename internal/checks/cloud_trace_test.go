package checks

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/redact"
)

const traceToken = "dct_fixture_cloud_token_7f3a9"

func fakeTraceCLI(t *testing.T, body string) (program, calls, environment string) {
	t.Helper()
	dir := t.TempDir()
	calls, environment = filepath.Join(dir, "calls"), filepath.Join(dir, "environment")
	script := "#!/bin/sh\nprintf 'call\\n' >> '" + calls + "'\nfor a in \"$@\"; do printf 'arg:%s\\n' \"$a\" >> '" + calls + "'; done\nenv > '" + environment + "'\n" + body
	program = filepath.Join(dir, "dagger")
	must(t, os.WriteFile(program, []byte(script), 0700))
	return program, calls, environment
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	must(t, err)
	return strings.Split(strings.TrimSpace(string(raw)), "\n")
}

func TestValidateTraceIDRefusesAnythingButLettersDigitsUnderscoreAndDash(t *testing.T) {
	t.Parallel()
	for _, id := range []string{"2f123ba77bf7bd2d4db2f70ed20613e8", "T", "a_b-c", "0", strings.Repeat("a", MaxTraceIDLength)} {
		if err := ValidateTraceID(id); err != nil {
			t.Errorf("%q refused: %v", id, err)
		}
	}
	for _, id := range []string{"", strings.Repeat("a", MaxTraceIDLength+1), "-x", "--help", "_a", "a b", " a", "a\n", "a\tb", "a/b", "a;b", "a=b", "a.b", "a$b", "é", "https://dagger.cloud/o/traces/a"} {
		if err := ValidateTraceID(id); err == nil {
			t.Errorf("%q accepted", id)
		}
	}
}

func TestValidateTraceCheckBoundsTheNameAndRefusesControlCharacters(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"test:a", "-x", "--help", "a b", "é ünï", strings.Repeat("a", MaxTraceCheckLength)} {
		if err := ValidateTraceCheck(name); err != nil {
			t.Errorf("%q refused: %v", name, err)
		}
	}
	for _, name := range []string{"", strings.Repeat("a", MaxTraceCheckLength+1), "a\nb", "a\x00b", "a\x1bb", "a\x7fb", "\xff"} {
		if err := ValidateTraceCheck(name); err == nil {
			t.Errorf("%q accepted", name)
		}
	}
}

func TestTraceFetchCommandKeepsEachValueInItsOwnSlot(t *testing.T) {
	t.Parallel()
	if got, want := TraceFetchCommand("abc123", ""), []string{"cloud", "traces", "view", "abc123"}; !slices.Equal(got, want) {
		t.Fatalf("whole trace: %q, want %q", got, want)
	}
	for _, check := range []string{"test:a", "-x", "--help", "a b", "--check=b", "x --flag"} {
		got := TraceFetchCommand("abc123", check)
		if want := []string{"cloud", "traces", "view", "--check=" + check, "abc123"}; !slices.Equal(got, want) {
			t.Fatalf("check %q: %q, want %q", check, got, want)
		}
	}
}

func TestFetchTraceRunsTheFixedCommandWithTheTokenOnlyInItsEnvironment(t *testing.T) {
	t.Parallel()
	program, calls, environment := fakeTraceCLI(t, "printf 'trace output\\nFAIL: boom\\n'\n")
	runner := DaggerChecks{CloudToken: traceToken, Program: program}
	for _, tc := range []struct {
		check string
		argv  []string
	}{
		{"", []string{"cloud", "traces", "view", "t1"}},
		{"test:a", []string{"cloud", "traces", "view", "--check=test:a", "t1"}},
		{"-x", []string{"cloud", "traces", "view", "--check=-x", "t1"}},
		{"--help", []string{"cloud", "traces", "view", "--check=--help", "t1"}},
		{"a b;c $(d) `e` | f", []string{"cloud", "traces", "view", "--check=a b;c $(d) `e` | f", "t1"}},
	} {
		_ = os.Remove(calls)
		out, failure := runner.FetchTrace(context.Background(), "t1", tc.check)
		if failure != nil {
			t.Fatalf("check %q: %v", tc.check, failure)
		}
		if out.Output != "trace output\nFAIL: boom\n" || out.Truncated || out.Size != len(out.Output) {
			t.Fatalf("check %q: %+v", tc.check, out)
		}
		got := readLines(t, calls)
		var argv []string
		for _, line := range got[1:] {
			argv = append(argv, strings.TrimPrefix(line, "arg:"))
		}
		if got[0] != "call" || !slices.Equal(argv, tc.argv) {
			t.Fatalf("check %q ran %q, want %q", tc.check, argv, tc.argv)
		}
		if strings.Contains(strings.Join(got, "\n"), traceToken) {
			t.Fatalf("the token reached the arguments: %q", got)
		}
	}
	env := readLines(t, environment)
	var names []string
	for _, line := range env {
		name, _, _ := strings.Cut(line, "=")
		if !slices.Contains([]string{"PWD", "SHLVL", "_", "OLDPWD"}, name) {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	if want := []string{"DAGGER_CLOUD_TOKEN", "HOME", "NO_COLOR", "PATH", "TMPDIR"}; !slices.Equal(names, want) {
		t.Fatalf("the CLI's environment holds %q, want %q", names, want)
	}
	if !slices.Contains(env, "DAGGER_CLOUD_TOKEN="+traceToken) {
		t.Fatalf("the token did not reach the CLI's environment: %q", env)
	}
}

func TestFetchTraceScrubsTheTokenAndSecretShapedValues(t *testing.T) {
	t.Parallel()
	program, _, _ := fakeTraceCLI(t, "printf 'token %s\\nDAGGER_CLOUD_TOKEN=%s\\nAuthorization: Bearer abcdef0123456789\\nghp_abcdefghijklmnopqrstuvwxyz0123456789\\nclone https://user:pw@example.com/r.git\\nok\\n' \"$DAGGER_CLOUD_TOKEN\" \"$DAGGER_CLOUD_TOKEN\"\n")
	out, failure := (DaggerChecks{CloudToken: traceToken, Program: program}).FetchTrace(context.Background(), "t1", "")
	if failure != nil {
		t.Fatal(failure)
	}
	for _, leaked := range []string{traceToken, "abcdef0123456789", "ghp_abcdefghijklmnopqrstuvwxyz0123456789", "user:pw"} {
		if strings.Contains(out.Output, leaked) {
			t.Fatalf("the output holds %q:\n%s", leaked, out.Output)
		}
	}
	if !strings.Contains(out.Output, redact.Marker) || !strings.HasSuffix(out.Output, "ok\n") {
		t.Fatalf("the output lost its marker or its end:\n%s", out.Output)
	}
}

func TestFetchTraceKeepsTheEndOfOutputLargerThanTheBound(t *testing.T) {
	t.Parallel()
	program, _, _ := fakeTraceCLI(t, "i=0\nwhile [ $i -lt 20000 ]; do printf 'line %05d padding padding\\n' $i; i=$((i+1)); done\nprintf 'the failure is here\\n'\n")
	out, failure := (DaggerChecks{CloudToken: traceToken, Program: program}).FetchTrace(context.Background(), "t1", "")
	if failure != nil {
		t.Fatal(failure)
	}
	full := 20000*len("line 00000 padding padding\n") + len("the failure is here\n")
	if len(out.Output) > MaxTraceOutput || !out.Truncated || out.Size != full || !strings.HasSuffix(out.Output, "line 19999 padding padding\nthe failure is here\n") || strings.Contains(out.Output, "line 00000") {
		t.Fatalf("output of %d bytes, truncated %v, size %d (want %d)", len(out.Output), out.Truncated, out.Size, full)
	}
}

func TestFetchTraceBoundsOutputBeyondWhatItCaptures(t *testing.T) {
	t.Parallel()
	program, _, _ := fakeTraceCLI(t, "i=0\nwhile [ $i -lt 40000 ]; do printf 'line %05d %s padding padding padding\\n' $i \"$DAGGER_CLOUD_TOKEN\"; i=$((i+1)); done\nprintf 'end\\n'\n")
	out, failure := (DaggerChecks{CloudToken: traceToken, Program: program}).FetchTrace(context.Background(), "t1", "")
	if failure != nil {
		t.Fatal(failure)
	}
	if len(out.Output) > MaxTraceOutput || !out.Truncated || out.Size <= traceCapture || strings.Contains(out.Output, traceToken) || !strings.HasSuffix(out.Output, "end\n") {
		t.Fatalf("output of %d bytes, truncated %v, size %d", len(out.Output), out.Truncated, out.Size)
	}
}

func TestFetchTraceFailuresNameTheirReason(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "no-dagger")
	for _, tc := range []struct {
		name   string
		runner func(t *testing.T) DaggerChecks
		reason TraceFailureReason
	}{
		{"no token", func(t *testing.T) DaggerChecks {
			program, _, _ := fakeTraceCLI(t, "printf 'never\\n'\n")
			return DaggerChecks{Program: program}
		}, TraceNoToken},
		{"rejected token", func(t *testing.T) DaggerChecks {
			program, _, _ := fakeTraceCLI(t, "printf 'Error: 401 Unauthorized: invalid token %s\\n' \"$DAGGER_CLOUD_TOKEN\" >&2\nexit 1\n")
			return DaggerChecks{CloudToken: traceToken, Program: program}
		}, TraceTokenRejected},
		{"unknown trace", func(t *testing.T) DaggerChecks {
			program, _, _ := fakeTraceCLI(t, "printf 'Error: trace not found (token %s)\\n' \"$DAGGER_CLOUD_TOKEN\" >&2\nexit 1\n")
			return DaggerChecks{CloudToken: traceToken, Program: program}
		}, TraceUnknown},
		{"timeout", func(t *testing.T) DaggerChecks {
			program, _, _ := fakeTraceCLI(t, "exec sleep 30\n")
			return DaggerChecks{CloudToken: traceToken, Program: program, TraceTimeout: 300 * time.Millisecond}
		}, TraceTimeout},
		{"missing CLI", func(t *testing.T) DaggerChecks {
			return DaggerChecks{CloudToken: traceToken, Program: missing}
		}, TraceCLIUnavailable},
		{"unclassified failure", func(t *testing.T) DaggerChecks {
			program, _, _ := fakeTraceCLI(t, "printf 'something odd happened with %s\\n' \"$DAGGER_CLOUD_TOKEN\" >&2\nexit 2\n")
			return DaggerChecks{CloudToken: traceToken, Program: program}
		}, TraceFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			started := time.Now()
			out, failure := tc.runner(t).FetchTrace(context.Background(), "t1", "")
			if failure == nil || failure.Reason != tc.reason || out != (TraceOutput{}) {
				t.Fatalf("got %+v, %v; want reason %s", out, failure, tc.reason)
			}
			if time.Since(started) > 20*time.Second {
				t.Fatalf("the fetch held for %s", time.Since(started))
			}
			if strings.Contains(failure.Message, traceToken) || strings.Contains(failure.Error(), traceToken) {
				t.Fatalf("the failure shows the token: %q", failure.Message)
			}
			if tc.reason == TraceFailed && (!strings.Contains(failure.Message, "something odd happened") || !strings.Contains(failure.Message, redact.Marker)) {
				t.Fatalf("an untold cause quotes no scrubbed excerpt: %q", failure.Message)
			}
		})
	}
}
