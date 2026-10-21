package fixturekit

import (
	"flag"
	"testing"
	"time"
)

// newTestFlags builds a fresh FlagSet with shuffle and timeout flags,
// so each case starts from a clean, unparsed set.
func newTestFlags() *flag.FlagSet {
	fs := flag.NewFlagSet("testmain-defaults", flag.ContinueOnError)
	fs.String("test.shuffle", "off", "")
	fs.Duration("test.timeout", 0, "")
	return fs
}

func TestApplyServiceTestDefaults(t *testing.T) {
	t.Parallel()

	t.Run("unset shuffle becomes on", func(t *testing.T) {
		fs := newTestFlags()
		ApplyServiceTestDefaults(fs)
		if got := fs.Lookup("test.shuffle").Value.String(); got != "on" {
			t.Fatalf("test.shuffle = %q, want %q", got, "on")
		}
	})

	for _, shuffle := range []string{"on", "off", "1700000000000000000"} {
		t.Run("explicit shuffle "+shuffle+" is kept", func(t *testing.T) {
			fs := newTestFlags()
			if err := fs.Parse([]string{"-test.shuffle=" + shuffle}); err != nil {
				t.Fatalf("parsing flags: %v", err)
			}
			ApplyServiceTestDefaults(fs)
			if got := fs.Lookup("test.shuffle").Value.String(); got != shuffle {
				t.Fatalf("test.shuffle = %q, want %q", got, shuffle)
			}
		})
	}

	t.Run("30m is kept", func(t *testing.T) {
		fs := newTestFlags()
		if err := fs.Parse([]string{"-test.timeout=30m"}); err != nil {
			t.Fatalf("parsing flags: %v", err)
		}
		ApplyServiceTestDefaults(fs)
		if got := fs.Lookup("test.timeout").Value.String(); got != (30 * time.Minute).String() {
			t.Fatalf("test.timeout = %q, want %q", got, (30 * time.Minute).String())
		}
	})

	t.Run("default zero timeout is kept", func(t *testing.T) {
		fs := newTestFlags()
		ApplyServiceTestDefaults(fs)
		if got := fs.Lookup("test.timeout").Value.String(); got != "0s" {
			t.Fatalf("test.timeout = %q, want %q", got, "0s")
		}
	})

	t.Run("explicit zero timeout is kept", func(t *testing.T) {
		fs := newTestFlags()
		if err := fs.Parse([]string{"-test.timeout=0"}); err != nil {
			t.Fatalf("parsing flags: %v", err)
		}
		ApplyServiceTestDefaults(fs)
		if got := fs.Lookup("test.timeout").Value.String(); got != "0s" {
			t.Fatalf("test.timeout = %q, want %q", got, "0s")
		}
	})

	t.Run("a smaller timeout is kept", func(t *testing.T) {
		fs := newTestFlags()
		if err := fs.Parse([]string{"-test.timeout=5m"}); err != nil {
			t.Fatalf("parsing flags: %v", err)
		}
		ApplyServiceTestDefaults(fs)
		if got := fs.Lookup("test.timeout").Value.String(); got != (5 * time.Minute).String() {
			t.Fatalf("test.timeout = %q, want %q", got, (5 * time.Minute).String())
		}
	})
}
