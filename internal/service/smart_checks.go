package service

import (
	"context"

	checkrun "github.com/kpenfound/osmia/internal/checks"
	"github.com/kpenfound/osmia/internal/jev"
)

// selectChecks asks Jev which of the listed checks the change can affect.
// Every outcome but an accepted judgment runs every check. Listing waits for
// the project's modules to load, so it is skipped while the boost is off.
func (c *checkers) selectChecks(ctx context.Context, in checkInput, dir, diff string, runner ReviewChecks) CheckSelection {
	return checkrun.Select(ctx, c.s.jev, c.repository, c.scope(in), checkRequestID(in.Unit, in.Run),
		[]jev.Source{{Kind: "unit-report", ID: reportDocument(in.Unit), Revision: in.Report}}, dir, diff, runner)
}
