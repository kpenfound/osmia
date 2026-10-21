package fixturekit

import (
	"flag"
)

// ApplyServiceTestDefaults turns on test shuffling on fs unless the caller
// set -test.shuffle explicitly.
func ApplyServiceTestDefaults(fs *flag.FlagSet) {
	explicit := map[string]bool{}
	fs.Visit(func(f *flag.Flag) {
		explicit[f.Name] = true
	})

	if !explicit["test.shuffle"] {
		fs.Set("test.shuffle", "on")
	}
}
