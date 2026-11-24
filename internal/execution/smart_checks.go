package execution

import (
	"context"

	checkrun "github.com/kpenfound/osmia/internal/checks"
	"github.com/kpenfound/osmia/internal/jev"
)

func (c *checkers) selectChecks(ctx context.Context, in checkInput, dir, diff string, runner ReviewChecks) CheckSelection {
	return checkrun.Select(ctx, c.s.Jev, c.repository, c.Scope(in), checkRequestID(in.Unit, in.Run),
		[]jev.Source{{Kind: "unit-report", ID: ReportDocument(in.Unit), Revision: in.Report}}, dir, diff, c.checkIdentity(in), runner)
}

func (c *checkers) checkIdentity(in checkInput) checkrun.Identity {
	return checkrun.Identity{Candidate: in.Candidate, Branch: UnitBranch(in.Workstream, in.Unit), Remote: "https://github.com/" + c.cfg.Project.Upstream, Repository: c.cfg.Project.Clone}
}
