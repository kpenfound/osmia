package service

import (
	"flag"
	"time"
)

// serviceTestTimeoutCap bounds this package's -test.timeout below the
// service's 15-minute dagger check limit (internal/service/review_checks.go,
// DaggerChecks.Check; internal/config's DefaultChecksTimeout), leaving
// headroom for compiling and the other packages in the same check. Recent
// full check runs of the whole Go test suite, dominated by this package,
// finished in about 4 to 5 minutes: units/git-fixtures/checks-2.json ran
// 14:09Z to 14:13Z, and units/bk-turns/checks-10.json reports "CHECKS ✔ 37
// passed" over 5m2s. 10 minutes keeps roughly double that headroom while
// staying long enough for a normal run.
const serviceTestTimeoutCap = 10 * time.Minute

// applyServiceTestDefaults turns on test shuffling on fs unless the caller
// set -test.shuffle explicitly, and lowers -test.timeout to limit when it is
// zero or larger than limit, keeping a smaller caller value.
func applyServiceTestDefaults(fs *flag.FlagSet, limit time.Duration) {
	explicit := map[string]bool{}
	fs.Visit(func(f *flag.Flag) {
		explicit[f.Name] = true
	})

	if !explicit["test.shuffle"] {
		fs.Set("test.shuffle", "on")
	}

	if timeout := fs.Lookup("test.timeout"); timeout != nil {
		if getter, ok := timeout.Value.(flag.Getter); ok {
			if d, ok := getter.Get().(time.Duration); ok && (d == 0 || d > limit) {
				fs.Set("test.timeout", limit.String())
			}
		}
	}
}
