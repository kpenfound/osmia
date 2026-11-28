package cli

import (
	"fmt"
	"io"
	"path/filepath"

	"github.com/kpenfound/osmia/internal/config"
)

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
	for _, w := range cfg.Warnings {
		fmt.Fprintln(stderr, "warning: "+w.String())
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
