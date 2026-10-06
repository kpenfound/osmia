package service

import (
	"flag"
	"testing"
	"time"
)

// newTestFlags builds a fresh FlagSet with the same two flags TestMain
// applies defaults to, so each case starts from a clean, unparsed set.
func newTestFlags() *flag.FlagSet {
	fs := flag.NewFlagSet("testmain-defaults", flag.ContinueOnError)
	fs.String("test.shuffle", "off", "")
	fs.Duration("test.timeout", 0, "")
	return fs
}

func TestApplyServiceTestDefaults(t *testing.T) {
	t.Parallel()
	const limit = 10 * time.Minute

	t.Run("unset shuffle becomes on", func(t *testing.T) {
		fs := newTestFlags()
		applyServiceTestDefaults(fs, limit)
		if got := fs.Lookup("test.shuffle").Value.String(); got != "on" {
			t.Fatalf("test.shuffle = %q, want %q", got, "on")
		}
	})

	t.Run("explicit seed is kept", func(t *testing.T) {
		fs := newTestFlags()
		if err := fs.Parse([]string{"-test.shuffle=1700000000000000000"}); err != nil {
			t.Fatalf("parsing flags: %v", err)
		}
		applyServiceTestDefaults(fs, limit)
		if got := fs.Lookup("test.shuffle").Value.String(); got != "1700000000000000000" {
			t.Fatalf("test.shuffle = %q, want the explicit seed kept", got)
		}
	})

	t.Run("30m is lowered to the cap", func(t *testing.T) {
		fs := newTestFlags()
		if err := fs.Parse([]string{"-test.timeout=30m"}); err != nil {
			t.Fatalf("parsing flags: %v", err)
		}
		applyServiceTestDefaults(fs, limit)
		if got := fs.Lookup("test.timeout").Value.String(); got != limit.String() {
			t.Fatalf("test.timeout = %q, want %q", got, limit.String())
		}
	})

	t.Run("0 is lowered to the cap", func(t *testing.T) {
		fs := newTestFlags()
		applyServiceTestDefaults(fs, limit)
		if got := fs.Lookup("test.timeout").Value.String(); got != limit.String() {
			t.Fatalf("test.timeout = %q, want %q", got, limit.String())
		}
	})

	t.Run("a smaller timeout is kept", func(t *testing.T) {
		fs := newTestFlags()
		if err := fs.Parse([]string{"-test.timeout=5m"}); err != nil {
			t.Fatalf("parsing flags: %v", err)
		}
		applyServiceTestDefaults(fs, limit)
		if got := fs.Lookup("test.timeout").Value.String(); got != (5 * time.Minute).String() {
			t.Fatalf("test.timeout = %q, want %q", got, (5 * time.Minute).String())
		}
	})
}
