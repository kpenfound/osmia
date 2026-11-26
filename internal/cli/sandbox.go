package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/kpenfound/osmia/internal/sandbox"
)

// runSandbox runs `osmia sandbox build` or `osmia sandbox check`. Neither
// needs a service, a root, a configuration file or a secret.
func runSandbox(ctx context.Context, which string, stdout, stderr io.Writer) int {
	host := sandbox.Host{Stdout: stdout}
	timeout := sandbox.CheckTimeout
	run := host.Check
	if which == "build" {
		timeout, run = sandbox.BuildTimeout, host.Build
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := run(ctx); err != nil {
		fmt.Fprintf(stderr, "osmia sandbox %s: %s\n", which, strings.Join(strings.Fields(err.Error()), " "))
		return 1
	}
	return 0
}
