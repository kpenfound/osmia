package checks

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func fakeCallDagger(t *testing.T, body string) (args, env string) {
	t.Helper()
	bin := t.TempDir()
	logs := t.TempDir()
	args, env = filepath.Join(logs, "args"), filepath.Join(logs, "env")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '" + args + "'\nenv -0 > '" + env + "'\n" + body
	must(t, os.WriteFile(filepath.Join(bin, "dagger"), []byte(script), 0700))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return args, env
}

func environmentNames(t *testing.T, file string) []string {
	t.Helper()
	raw, err := os.ReadFile(file)
	must(t, err)
	var names []string
	for _, entry := range strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00") {
		name, _, _ := strings.Cut(entry, "=")
		if name != "PWD" && name != "SHLVL" && name != "_" {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}

func TestCallPassesTheFunctionOnlyTheRunArgumentAndTheCheckClientEnvironment(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for name, value := range map[string]string{"GITHUB_TOKEN": "host-github-secret", "ANTHROPIC_API_KEY": "host-provider-secret", "SSH_AUTH_SOCK": "/host/agent", "CI": "host-ci", "OSMIA_SECRET": "host-secret"} {
		t.Setenv(name, value)
	}
	for _, name := range []string{"DOCKER_HOST", "_EXPERIMENTAL_DAGGER_RUNNER_HOST", "DAGGER_X_RELEASE", "DAGGER_CLOUD_TOKEN"} {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
	args, env := fakeCallDagger(t, `printf '{"schema":1}'
printf 'View trace at https://dagger.cloud/example/traces/run1\n' >&2
`)
	d := DaggerChecks{CloudToken: "client-environment-secret"}
	result, err := d.Call(context.Background(), t.TempDir(), "measure-wall", "0123abcd")
	must(t, err)
	if result.ExitCode != 0 || result.Stdout != `{"schema":1}` || result.TraceURL != "https://dagger.cloud/example/traces/run1" || result.Truncated {
		t.Fatalf("%+v", result)
	}
	if got := checkInvocations(t, args); !slices.Equal(got, []string{"call measure-wall --run=0123abcd"}) {
		t.Fatalf("invocations %q", got)
	}
	if got, want := environmentNames(t, env), []string{"DAGGER_CLOUD_TOKEN", "HOME", "NO_COLOR", "PATH", "TMPDIR"}; !slices.Equal(got, want) {
		t.Fatalf("environment %q, want %q", got, want)
	}
}

func TestCallWithoutATokenSendsNoCloudCredential(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GITHUB_TOKEN", "host-github-secret")
	for _, name := range []string{"DOCKER_HOST", "_EXPERIMENTAL_DAGGER_RUNNER_HOST", "DAGGER_X_RELEASE", "DAGGER_CLOUD_TOKEN"} {
		os.Unsetenv(name)
	}
	_, env := fakeCallDagger(t, "printf '{}'\n")
	_, err := (DaggerChecks{}).Call(context.Background(), t.TempDir(), "measure", "abc")
	must(t, err)
	if got, want := environmentNames(t, env), []string{"HOME", "NO_COLOR", "PATH", "TMPDIR"}; !slices.Equal(got, want) {
		t.Fatalf("environment %q, want %q", got, want)
	}
}

func TestCallReportsAFailingFunctionAsAResult(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	fakeCallDagger(t, `printf 'partial'
printf 'Error: oracle crashed with %s\n' "$DAGGER_CLOUD_TOKEN" >&2
exit 3
`)
	result, err := (DaggerChecks{CloudToken: "a-secret-token"}).Call(context.Background(), t.TempDir(), "measure", "abc")
	must(t, err)
	if result.ExitCode != 3 || result.Stdout != "partial" || !strings.Contains(result.Stderr, "oracle crashed") || strings.Contains(result.Stderr, "a-secret-token") {
		t.Fatalf("%+v", result)
	}
}

func TestCallMarksAnOutputThatOutgrewItsLimit(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	fakeCallDagger(t, "head -c 5000000 /dev/zero | tr '\\0' 'x'\n")
	result, err := (DaggerChecks{}).Call(context.Background(), t.TempDir(), "measure", "abc")
	must(t, err)
	if !result.Truncated || len(result.Stdout) > callOutputLimit {
		t.Fatalf("truncated %v, %d bytes", result.Truncated, len(result.Stdout))
	}
}

func TestCallRefusesNamesAndValuesThatCouldReadAsOptions(t *testing.T) {
	args, _ := fakeCallDagger(t, "printf '{}'\n")
	for _, c := range []struct{ function, run string }{
		{"--help", "abc"}, {"-x", "abc"}, {"", "abc"}, {"two words", "abc"}, {"measure;rm", "abc"}, {"measure", ""}, {"measure", "a b"}, {"measure", "a=b"}, {"measure", "a/b"},
	} {
		if _, err := (DaggerChecks{}).Call(context.Background(), t.TempDir(), c.function, c.run); err == nil {
			t.Errorf("accepted function %q run %q", c.function, c.run)
		}
	}
	if _, err := os.Stat(args); !os.IsNotExist(err) {
		t.Fatal("dagger ran for a refused call")
	}
	if !ValidFunctionName("measureWall") || !ValidFunctionName("measure-wall_2") || ValidFunctionName("1measure") {
		t.Fatal("function name validation")
	}
}
