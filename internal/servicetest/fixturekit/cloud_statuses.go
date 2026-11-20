package fixturekit

import (
	"context"
	"time"

	"github.com/kpenfound/osmia/internal/pulls"
)

// NoPostPushWait is a FinalChecksWait that reads the statuses at once after a
// push, which fixtures with a fixed clock need.
func NoPostPushWait() *time.Duration { d := time.Duration(0); return &d }

// PassingStatuses is the combined status read of a commit whose module loaded
// and whose "Run Dagger Checks" status is a finished run in which each of its
// three checks passed.
func PassingStatuses(repository, commit string) pulls.CommitStatuses {
	at := DemoStart
	return pulls.CommitStatuses{Repository: repository, Commit: commit, Statuses: []pulls.CommitStatus{
		{ID: 1, Context: "load", State: "success", CreatedAt: at, UpdatedAt: at},
		{ID: 2, Context: "Run Dagger Checks", State: "success", TargetURL: "https://dagger.cloud/checks/fixture", CreatedAt: at, UpdatedAt: at},
		{ID: 3, Context: "lint", State: "success", CreatedAt: at, UpdatedAt: at},
		{ID: 4, Context: "test", State: "success", CreatedAt: at, UpdatedAt: at},
		{ID: 5, Context: "build", State: "success", CreatedAt: at, UpdatedAt: at},
	}}
}

// CommitStatuses reads the statuses the host shows for a commit: Statuses
// when it is set, else a run in which every check passed.
func (c *FakePulls) CommitStatuses(ctx context.Context, repository, commit string) (pulls.CommitStatuses, error) {
	c.Mu.Lock()
	read := c.Statuses
	c.Mu.Unlock()
	if read != nil {
		return read(ctx, repository, commit)
	}
	return PassingStatuses(repository, commit), nil
}

// CommitStatuses reads the statuses of the repository's host.
func (r RepositoryPulls) CommitStatuses(ctx context.Context, repository, commit string) (pulls.CommitStatuses, error) {
	return r[repository].CommitStatuses(ctx, repository, commit)
}
