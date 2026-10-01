package cli

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/doctor"
)

// doctorOptions is what doctor checks the root through; tests replace the
// host boundaries.
var doctorOptions = func(root config.Root) doctor.Options {
	return doctor.Options{Root: root, Token: os.Getenv("GITHUB_TOKEN")}
}

// runDoctor prints every check grouped by area, or the checks as JSON, and
// exits 1 when one failed.
func runDoctor(ctx context.Context, root config.Root, asJSON bool, stdout, stderr io.Writer) int {
	checks := doctor.Run(ctx, doctorOptions(root))
	code := 0
	if doctor.Failed(checks) {
		code = 1
	}
	if asJSON {
		if out := output(stdout, stderr, checks); out != 0 {
			return out
		}
		return code
	}
	width := 0
	for _, c := range checks {
		width = max(width, len(c.Name))
	}
	counts := map[doctor.Status]int{}
	group := ""
	for _, c := range checks {
		if c.Group != group {
			if group != "" {
				fmt.Fprintln(stdout)
			}
			group = c.Group
			fmt.Fprintln(stdout, group)
		}
		counts[c.Status]++
		mark := map[doctor.Status]string{doctor.Pass: "✓", doctor.Warn: "!", doctor.Fail: "✗"}[c.Status]
		fmt.Fprintf(stdout, "  %s %-*s  %s\n", mark, width, c.Name, c.Detail)
		if c.Remediation != "" {
			fmt.Fprintf(stdout, "      → %s\n", c.Remediation)
		}
	}
	fmt.Fprintf(stdout, "\n%d checks: %d passed, %d warnings, %d failed\n", len(checks), counts[doctor.Pass], counts[doctor.Warn], counts[doctor.Fail])
	return code
}
