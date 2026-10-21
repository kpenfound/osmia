package execution

import (
	"context"
	"time"

	"github.com/kpenfound/osmia/internal/bundle"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/jev"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/trace"
)

// Dependencies supplies the service capabilities used by unit execution.
// Repository handles are the same handles owned by the running service.
type Dependencies struct {
	RefreshPendingStreams func(*trace.Repository) (map[config.WorkstreamID]bool, error)
	RefreshBudget         func(context.Context, *trace.Repository) error
	AcquireCheckSlot      func(context.Context, config.ProjectID, config.WorkstreamID, string) (func(), error)
	RefreshPending        func(repo *trace.Repository) (bool, error)

	EdgeWaiting func(repo *trace.Repository, stream config.WorkstreamID) (bool, error)

	ChangeHeld func(repo *trace.Repository, stream config.WorkstreamID) (bool, error)

	ChangeRepositoryHeld func(repo *trace.Repository, stream config.WorkstreamID, op coreadapter.Operation) (bool, error)

	WorkgroupEdgePending func(repo *trace.Repository, stream config.WorkstreamID) (bool, error)

	CarryBase func(repository *trace.Repository, stream config.WorkstreamID, onto string) (string, bool, error)

	CarriedOnto func(repository *trace.Repository, stream config.WorkstreamID, onto string) (trace.UpstreamMove, bool, error)

	UnitDiscoveries func(r *trace.Repository, stream config.WorkstreamID, unit string) (string, string, error)

	UnitDrift func(repository *trace.Repository, stream config.WorkstreamID, unit string) (trace.UpstreamMove, bool, error)

	UnitRebaseConflicts func(repository *trace.Repository, stream config.WorkstreamID, unit, base string) ([]string, error)
	Now                 func() time.Time
	Effective           func() (runtime.State, []runtime.Diagnostic)
	RoleExecution       func(*config.Config, string) (coreadapter.Profile, coreadapter.ExecutionSettings, error)
	ContextFor          func(*trace.Repository) bundle.Provider
	Step                func(string) error
	About               func(*trace.Repository) *config.Config
	Runtimes            func() (*config.Config, []*Project)
	ConversationTrace   func(string) (config.ProjectID, config.WorkstreamID, *trace.Repository, *APIError)
	Checks              ReviewChecks
	Jev                 *jev.Judge
}
type Controller struct{ Dependencies }

func New(d Dependencies) *Controller { return &Controller{d} }

type Controls struct{ Load func() *Controller }
type Project struct{ Repository *trace.Repository }
