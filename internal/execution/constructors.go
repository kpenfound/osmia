package execution

import (
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/trace"
)

type Masons = masons
type Foreman = foreman
type Reviewers = reviewers
type Checkers = checkers
type Rebaser = rebaser
type UnitWorkspaces = unitWorkspaces

func NewMasons(c *Controller, cfg *config.Config, r *trace.Repository) *Masons {
	return &masons{s: c, cfg: cfg, repository: r}
}
func NewForeman(m *Masons) *Foreman     { return &foreman{masons: m} }
func NewReviewers(m *Masons) *Reviewers { return &reviewers{masons: m} }
func NewCheckers(m *Masons) *Checkers   { return &checkers{masons: m} }
func NewRebaser(f *Foreman) Rebaser     { return rebaser{f} }
func NewRetriedTurns(t coreadapter.Turns, u UnitWorkspaces, r *trace.Repository) *RetriedTurns {
	return &retriedTurns{Turns: t, units: u, repository: r}
}
func NewReportingTurns(t coreadapter.Turns, reports *MasonReports) *ReportingTurns {
	return &reportingTurns{Turns: t, reports: reports}
}
func NewVerdictTurns(t coreadapter.Turns, reports *ReviewerReports) *VerdictTurns {
	return &verdictTurns{Turns: t, reports: reports}
}

func (c *Controls) ResolveContested(r *trace.Repository, s coreadapter.Scope) coreadapter.Tool {
	return c.resolveContested(r, s)
}

var Checkpointed = checkpointed
var CaptureFiles = captureFiles

// AcceptedForTest reads the report accepted by a local fixture turn.
func (r *masonReports) AcceptedForTest(key string) reportedDone {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.accepted[key]
}

// Units binds dispatch and candidate state to the service-owned project trace.
func (c *Controller) Units(r *trace.Repository) *Masons { return NewMasons(c, c.About(r), r) }

// Landing binds serial landing and rebase operations to the same project trace.
func (c *Controller) Landing(r *trace.Repository) *Foreman { return NewForeman(c.Units(r)) }

// AllMerged reports whether every unit in a sealed build has landed.
func AllMerged(b Building) bool {
	for _, u := range b.plan.Units {
		if b.states[trace.UnitSubject(u.ID)].Value != UnitMerged {
			return false
		}
	}
	return len(b.plan.Units) > 0
}
func (b building) Started() time.Time { return b.started }

func (o checkOperation) Input() CheckInput { return o.input }
