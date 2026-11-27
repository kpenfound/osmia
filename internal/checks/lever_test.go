package checks

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/verification"
)

func leverFixture(t *testing.T, files map[string][]byte) string {
	t.Helper()
	dir := t.TempDir()
	for name, data := range files {
		must(t, os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755))
		must(t, os.WriteFile(filepath.Join(dir, name), data, 0o644))
	}
	return dir
}

func exportingDagger(t *testing.T, fixture string) string {
	t.Helper()
	args, _ := fakeCallDagger(t, `for last; do :; done
case "$*" in
 *" list") printf '["checkout","login"]'; exit 0 ;;
esac
cp -R '`+fixture+`/.' "$last"
`)
	return args
}

const goodReport = `{"version":1,"claims":[
 {"name":"cart total","status":"pass","detail":"shows 3","evidence":["shots/cart.png","logs/console.txt"],"flow":"checkout"},
 {"name":"coupon","status":"fail","detail":"no field","evidence":["logs/console.txt"],"flow":"checkout"},
 {"name":"banner","status":"inconclusive","detail":"slow","evidence":[]}]}`

var pngHeader = []byte("\x89PNG\r\n\x1a\n")

func goodFiles() map[string][]byte {
	return map[string][]byte{
		"report.json":      []byte(goodReport),
		"shots/cart.png":   append(slices.Clone(pngHeader), make([]byte, 64)...),
		"logs/console.txt": []byte("token=none\n"),
	}
}

func TestRunLeverPassesTheRunIDAfterTheBaseArgumentsAndExportsTheDirectory(t *testing.T) {
	args := exportingDagger(t, leverFixture(t, goodFiles()))
	run, err := DaggerChecks{}.RunLever(context.Background(), t.TempDir(), LeverCall{Base: []string{"lever", "--source=."}, Args: []string{"flow", "--name=checkout"}, RunID: "run-1"})
	must(t, err)
	defer run.Close()
	got := checkInvocations(t, args)
	if len(got) != 1 || !strings.HasPrefix(got[0], "call lever --source=. --run-id run-1 flow --name=checkout export --path ") {
		t.Fatalf("invocations %q", got)
	}
	if run.Result.Rejected != "" || len(run.Result.Claims) != 3 || run.Result.Status() != verification.Fail {
		t.Fatalf("result %+v", run.Result)
	}
	if want := []string{"shots/cart.png", "logs/console.txt"}; !slices.Equal(run.Result.Evidence, want) {
		t.Fatalf("evidence %q", run.Result.Evidence)
	}
	for _, p := range run.EvidencePaths() {
		if _, err := os.Stat(p); err != nil || !strings.HasPrefix(p, run.Dir) {
			t.Fatalf("evidence path %s: %v", p, err)
		}
	}
	must(t, run.Close())
	if _, err := os.Stat(run.Dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("export not removed: %v", err)
	}
}

func TestRunLeverRefusesArgumentsWithoutStartingDagger(t *testing.T) {
	for name, args := range map[string][]string{
		"empty":             nil,
		"shorthand option":  {"flow", "-x"},
		"export step":       {"flow", "export"},
		"scheme":            {"flow", "--url=http://example.com"},
		"parent path":       {"flow", "--steps=../x"},
		"absolute path":     {"flow", "--steps=/etc/passwd"},
		"run id":            {"flow", "--run-id", "mine"},
		"run id with value": {"flow", "--run-id=mine"},
		"run id camel":      {"flow", "--runId=mine"},
		"run id underscore": {"--run_id", "mine", "flow"},
		"cli option":        {"flow", "--json"},
	} {
		t.Run(name, func(t *testing.T) {
			log := exportingDagger(t, leverFixture(t, goodFiles()))
			_, err := DaggerChecks{}.RunLever(context.Background(), t.TempDir(), LeverCall{Base: []string{"lever"}, Args: args, RunID: "run-1"})
			var limit *LeverLimitation
			if !errors.As(err, &limit) {
				t.Fatalf("error %v, want a limitation", err)
			}
			if _, err := os.Stat(log); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("dagger was started: %v", err)
			}
		})
	}
}

func TestRunLeverRefusesASourcePathThatEscapesTheExport(t *testing.T) {
	log := exportingDagger(t, leverFixture(t, goodFiles()))
	src := t.TempDir()
	must(t, os.Symlink(t.TempDir(), filepath.Join(src, "out")))
	_, err := DaggerChecks{}.RunLever(context.Background(), src, LeverCall{Base: []string{"lever"}, Args: []string{"flow", "--source=out/x"}, RunID: "run-1"})
	var limit *LeverLimitation
	if !errors.As(err, &limit) {
		t.Fatalf("error %v, want a limitation", err)
	}
	if _, err := os.Stat(log); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dagger was started: %v", err)
	}
}

func TestRunLeverRefusesAnUnusableBaseOrRunID(t *testing.T) {
	log := exportingDagger(t, leverFixture(t, goodFiles()))
	for name, call := range map[string]LeverCall{
		"no base":       {Args: []string{"flow"}, RunID: "run-1"},
		"base run id":   {Base: []string{"lever", "--run-id=x"}, Args: []string{"flow"}, RunID: "run-1"},
		"empty run id":  {Base: []string{"lever"}, Args: []string{"flow"}},
		"option run id": {Base: []string{"lever"}, Args: []string{"flow"}, RunID: "--x"},
		"spaced run id": {Base: []string{"lever"}, Args: []string{"flow"}, RunID: "a b"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := DaggerChecks{}.RunLever(context.Background(), t.TempDir(), call)
			var limit *LeverLimitation
			if !errors.As(err, &limit) {
				t.Fatalf("error %v, want a limitation", err)
			}
		})
	}
	if _, err := os.Stat(log); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dagger was started: %v", err)
	}
}

func TestRunLeverRecordsAnInconclusiveRunWithTheReasonForARejectedReport(t *testing.T) {
	for name, tc := range map[string]struct {
		files  map[string][]byte
		reason string
	}{
		"missing report":      {map[string][]byte{"logs/a.txt": []byte("x")}, "report.json is missing"},
		"malformed json":      {map[string][]byte{"report.json": []byte("{not json")}, "not valid JSON"},
		"unsupported version": {map[string][]byte{"report.json": []byte(`{"version":2,"claims":[]}`)}, "unsupported version 2"},
		"missing version":     {map[string][]byte{"report.json": []byte(`{"claims":[]}`)}, "no version"},
		"unknown status":      {map[string][]byte{"report.json": []byte(`{"version":1,"claims":[{"name":"a","status":"maybe","detail":"","evidence":[]}]}`)}, `unknown status "maybe"`},
		"absent evidence":     {map[string][]byte{"report.json": []byte(`{"version":1,"claims":[{"name":"a","status":"pass","detail":"","evidence":["shots/none.png"]}]}`)}, "is not in the Directory"},
		"escaping evidence":   {map[string][]byte{"report.json": []byte(`{"version":1,"claims":[{"name":"a","status":"pass","detail":"","evidence":["../outside.txt"]}]}`)}, "escapes the Directory"},
		"absolute evidence":   {map[string][]byte{"report.json": []byte(`{"version":1,"claims":[{"name":"a","status":"pass","detail":"","evidence":["/etc/passwd"]}]}`)}, "relative"},
		"directory evidence":  {map[string][]byte{"report.json": []byte(`{"version":1,"claims":[{"name":"a","status":"pass","detail":"","evidence":["logs"]}]}`), "logs/a.txt": []byte("x")}, "not a regular file"},
		"oversized report":    {map[string][]byte{"report.json": []byte(`{"version":1,"claims":[{"name":"` + strings.Repeat("a", verification.MaxReportBytes) + `","status":"pass","detail":"","evidence":[]}]}`)}, "larger than"},
		"good report intact":  {goodFiles(), ""},
	} {
		t.Run(name, func(t *testing.T) {
			exportingDagger(t, leverFixture(t, tc.files))
			run, err := DaggerChecks{}.RunLever(context.Background(), t.TempDir(), LeverCall{Base: []string{"lever"}, Args: []string{"flow"}, RunID: "run-1"})
			must(t, err)
			defer run.Close()
			if tc.reason == "" {
				if run.Result.Rejected != "" {
					t.Fatalf("rejected: %s", run.Result.Rejected)
				}
				return
			}
			if !strings.Contains(run.Result.Rejected, tc.reason) || run.Result.Status() != verification.Inconclusive || len(run.Result.Claims) != 0 {
				t.Fatalf("result %+v, want rejection containing %q", run.Result, tc.reason)
			}
		})
	}
}

func TestRunLeverRejectsEvidenceThatLeavesTheDirectoryThroughASymlink(t *testing.T) {
	outside := t.TempDir()
	must(t, os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("host data"), 0o644))
	fixture := leverFixture(t, map[string][]byte{"report.json": []byte(`{"version":1,"claims":[{"name":"a","status":"pass","detail":"","evidence":["link/secret.txt"]}]}`)})
	must(t, os.Symlink(outside, filepath.Join(fixture, "link")))
	exportingDagger(t, fixture)
	run, err := DaggerChecks{}.RunLever(context.Background(), t.TempDir(), LeverCall{Base: []string{"lever"}, Args: []string{"flow"}, RunID: "run-1"})
	must(t, err)
	defer run.Close()
	if run.Result.Rejected == "" || run.Result.Status() != verification.Inconclusive {
		t.Fatalf("result %+v", run.Result)
	}
}

func TestRunLeverRefusesAnImageOverTheCapAndADirectoryOverTheTotalCap(t *testing.T) {
	t.Run("image", func(t *testing.T) {
		files := goodFiles()
		files["shots/cart.png"] = append(slices.Clone(pngHeader), make([]byte, verification.MaxImageBytes)...)
		exportingDagger(t, leverFixture(t, files))
		_, err := DaggerChecks{}.RunLever(context.Background(), t.TempDir(), LeverCall{Base: []string{"lever"}, Args: []string{"flow"}, RunID: "run-1"})
		var limit *LeverLimitation
		if !errors.As(err, &limit) || !strings.Contains(err.Error(), "shots/cart.png") {
			t.Fatalf("error %v, want an image limitation", err)
		}
	})
	t.Run("image at the cap", func(t *testing.T) {
		files := goodFiles()
		files["shots/cart.png"] = append(slices.Clone(pngHeader), make([]byte, verification.MaxImageBytes-len(pngHeader))...)
		exportingDagger(t, leverFixture(t, files))
		run, err := DaggerChecks{}.RunLever(context.Background(), t.TempDir(), LeverCall{Base: []string{"lever"}, Args: []string{"flow"}, RunID: "run-1"})
		must(t, err)
		run.Close()
	})
	t.Run("directory", func(t *testing.T) {
		files := goodFiles()
		for _, name := range []string{"a", "b", "c", "d"} {
			files["logs/"+name+".txt"] = bytes.Repeat([]byte("x"), 5<<20+1)
		}
		exportingDagger(t, leverFixture(t, files))
		_, err := DaggerChecks{}.RunLever(context.Background(), t.TempDir(), LeverCall{Base: []string{"lever"}, Args: []string{"flow"}, RunID: "run-1"})
		var limit *LeverLimitation
		if !errors.As(err, &limit) || !strings.Contains(err.Error(), "20 MiB") {
			t.Fatalf("error %v, want a Directory limitation", err)
		}
	})
}

func TestRunLeverRedactsKnownSecretsFromReportEvidenceAndDiagnostics(t *testing.T) {
	const secret = "ghp_leverSecretValue123"
	t.Setenv("GITHUB_TOKEN", secret)
	files := goodFiles()
	files["report.json"] = []byte(`{"version":1,"claims":[{"name":"leak ` + secret + `","status":"fail","detail":"saw ` + secret + `","evidence":["logs/console.txt"],"flow":"checkout"}]}`)
	files["logs/console.txt"] = []byte("Authorization: " + secret + "\n")
	fixture := leverFixture(t, files)
	fakeCallDagger(t, `for last; do :; done
printf 'progress %s\n' '`+secret+`' >&2
cp -R '`+fixture+`/.' "$last"
`)
	run, err := DaggerChecks{}.RunLever(context.Background(), t.TempDir(), LeverCall{Base: []string{"lever"}, Args: []string{"flow"}, RunID: "run-1"})
	must(t, err)
	defer run.Close()
	if strings.Contains(run.Diagnostics, secret) || !strings.Contains(run.Diagnostics, "[redacted]") {
		t.Fatalf("diagnostics %q", run.Diagnostics)
	}
	claim := run.Result.Claims[0]
	if strings.Contains(claim.Name+claim.Detail, secret) || !strings.Contains(claim.Detail, "[redacted]") {
		t.Fatalf("claim %+v", claim)
	}
	for _, name := range []string{"report.json", "logs/console.txt"} {
		data, err := os.ReadFile(filepath.Join(run.Dir, name))
		must(t, err)
		if strings.Contains(string(data), secret) || !strings.Contains(string(data), "[redacted]") {
			t.Fatalf("%s: %s", name, data)
		}
	}
	reloaded := verification.Load(run.Dir)
	if reloaded.Rejected != "" || len(reloaded.Claims) != 1 {
		t.Fatalf("redacted report no longer loads: %+v", reloaded)
	}
}

func TestRunLeverFailureCarriesRedactedOutputAndRemovesTheExport(t *testing.T) {
	const secret = "ghp_leverFailureSecret456"
	t.Setenv("GITHUB_TOKEN", secret)
	fakeCallDagger(t, "printf 'boom "+secret+"\\n' >&2\nexit 3\n")
	before, _ := filepath.Glob(filepath.Join(os.TempDir(), "osmia-lever-*"))
	_, err := DaggerChecks{}.RunLever(context.Background(), t.TempDir(), LeverCall{Base: []string{"lever"}, Args: []string{"flow"}, RunID: "run-1"})
	if err == nil || !strings.Contains(err.Error(), "exited 3") || strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "[redacted]") {
		t.Fatalf("error %v", err)
	}
	var limit *LeverLimitation
	if errors.As(err, &limit) {
		t.Fatalf("a failed run is not a limitation: %v", err)
	}
	after, _ := filepath.Glob(filepath.Join(os.TempDir(), "osmia-lever-*"))
	if len(after) != len(before) {
		t.Fatalf("temporary exports left behind: %v", after)
	}
}

func TestLeverFlowsCallsListWithTheBaseArgumentsAndParsesTheJSON(t *testing.T) {
	args := exportingDagger(t, leverFixture(t, nil))
	flows, err := DaggerChecks{}.LeverFlows(context.Background(), t.TempDir(), []string{"lever", "--source=."})
	must(t, err)
	if !slices.Equal(flows, []string{"checkout", "login"}) {
		t.Fatalf("flows %q", flows)
	}
	if got := checkInvocations(t, args); !slices.Equal(got, []string{"call lever --source=. list"}) {
		t.Fatalf("invocations %q", got)
	}
}

func TestLeverFlowsReportsABadListWithoutPanicking(t *testing.T) {
	for name, body := range map[string]string{
		"not json":  "printf 'checkout\\nlogin'\n",
		"not array": "printf '{\"a\":1}'\n",
		"duplicate": "printf '[\"a\",\"a\"]'\n",
		"exit":      "printf 'nope' >&2\nexit 2\n",
	} {
		t.Run(name, func(t *testing.T) {
			fakeCallDagger(t, body)
			if _, err := (DaggerChecks{}).LeverFlows(context.Background(), t.TempDir(), []string{"lever"}); err == nil {
				t.Fatal("no error")
			}
		})
	}
}
