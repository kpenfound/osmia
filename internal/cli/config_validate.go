package cli

import (
	"fmt"
	"io"
	"path/filepath"

	"github.com/kpenfound/osmia/internal/config"
)

// runConfigValidate loads one top-level file under the rules the service
// applies to it and reports the result: exit 0 when it loads, 1 when it does
// not. It is offline. It contacts no service, opens no trace or other state,
// creates nothing (the root included), reads no file but the top-level one and
// never reads the value of an environment variable a setting names. file is
// the --config path; empty, it validates the file root resolves to. A load
// error goes to stderr as the loader words it, naming the file and field and
// quoting no value from the file; load warnings go to stderr too.
func runConfigValidate(root, file string, stdout, stderr io.Writer) int {
	resolved, err := config.ResolveRoot(root, "")
	if err != nil {
		fmt.Fprintln(stderr, "invalid root; choose an accessible directory with --root")
		return 2
	}
	options := resolved.Options("")
	if file != "" {
		if file, err = filepath.Abs(file); err != nil {
			fmt.Fprintln(stderr, "invalid config path")
			return 2
		}
		options.File = file
	}
	cfg, err := config.LoadTopLevel(options)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	for _, warning := range cfg.Warnings {
		fmt.Fprintln(stderr, "warning: "+warning)
	}
	path := options.File
	if path == "" {
		path, err = resolved.Config()
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintf(stdout, "%s: valid\n", path)
	return 0
}
