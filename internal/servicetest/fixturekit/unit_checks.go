package fixturekit

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"github.com/kpenfound/osmia/internal/api"
)

// CheckOutcome is one scripted check result. Cancel, when set, is called
// before the check returns, as a service stopping mid-run does. Block waits
// for the check's context to end, as a run that never finishes does.
type CheckOutcome struct {
	Result api.CheckResult
	Err    error
	Cancel context.CancelFunc
	Block  bool
}

// FakeChecks lists links and returns its scripted outcomes in order,
// repeating the last, recording the links of each run and the candidate file
// it saw. It stands in for the real dagger check execution boundary,
// leaving the real CLI's link discovery, exit codes and output unverified.
type FakeChecks struct {
	Links    []string
	ListErr  error
	Outcomes []CheckOutcome

	Mu    sync.Mutex
	Ran   [][]string
	Dirs  []string
	Files []string
}

func (f *FakeChecks) List(context.Context, string, api.CheckIdentity) ([]string, error) {
	return f.Links, f.ListErr
}

func (f *FakeChecks) Check(ctx context.Context, dir string, _ api.CheckIdentity, links []string) (api.CheckResult, error) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	data, _ := os.ReadFile(filepath.Join(dir, MasonWrote))
	f.Ran, f.Dirs, f.Files = append(f.Ran, links), append(f.Dirs, dir), append(f.Files, string(data))
	o := f.Outcomes[min(len(f.Ran), len(f.Outcomes))-1]
	if o.Block {
		<-ctx.Done()
		return api.CheckResult{ExitCode: -1}, ctx.Err()
	}
	if o.Cancel != nil {
		o.Cancel()
		return api.CheckResult{ExitCode: -1}, ctx.Err()
	}
	return o.Result, o.Err
}

func (f *FakeChecks) Runs() [][]string {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	return slices.Clone(f.Ran)
}

const PassedReport = "== CHECKS ==  ✔ 2 passed\n✔ dag://go/packages/tests/test 2.0s OK\n✔ dag://release/version-round-trip 1.0s OK\n"

func FailedReport(link string) string {
	return fmt.Sprintf("== TRACE ==  ✘ FAILED\n\n== CHECKS ==  ✘ 1 failed\n✘ %s 1.0s ERROR\n    ✘ TestBuilt FAIL\n        built_test.go:9: wrong chunk\n", link)
}

func Passing() []CheckOutcome { return []CheckOutcome{{Result: api.CheckResult{Output: PassedReport}}} }
