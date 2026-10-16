package service

import "flag"

// applyServiceTestDefaults turns on test shuffling on fs unless the caller
// set -test.shuffle explicitly.
func applyServiceTestDefaults(fs *flag.FlagSet) {
	explicit := map[string]bool{}
	fs.Visit(func(f *flag.Flag) {
		explicit[f.Name] = true
	})

	if !explicit["test.shuffle"] {
		fs.Set("test.shuffle", "on")
	}
}
