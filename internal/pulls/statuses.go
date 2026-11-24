package pulls

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// CommitStatus is the latest status GitHub holds for one context of a commit.
// ID is the GitHub status id, which differs between two postings of the same
// context; CreatedAt and UpdatedAt are the times GitHub reports for it. State
// is pending, success, failure or error.
type CommitStatus struct {
	ID          int64     `json:"id"`
	Context     string    `json:"context"`
	State       string    `json:"state"`
	Description string    `json:"description,omitempty"`
	TargetURL   string    `json:"target_url,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// CommitStatuses is one read of a commit's combined statuses: the latest status
// of each context, in the order GitHub listed them.
type CommitStatuses struct {
	Repository string         `json:"repository"`
	Commit     string         `json:"commit"`
	Statuses   []CommitStatus `json:"statuses"`
}

// Status returns the status of the named context, if the read holds one.
func (c CommitStatuses) Status(name string) (CommitStatus, bool) {
	for _, s := range c.Statuses {
		if s.Context == name {
			return s, true
		}
	}
	return CommitStatus{}, false
}

// StatusReader reads the combined statuses of a commit.
type StatusReader interface {
	// CommitStatuses returns the latest status of every context of commit in
	// repository, across all pages.
	CommitStatuses(ctx context.Context, repository, commit string) (CommitStatuses, error)
}

const (
	statusPageSize = 100
	maxStatusPages = 50
)

type githubCombinedStatus struct {
	TotalCount int            `json:"total_count"`
	Statuses   []CommitStatus `json:"statuses"`
}

// CommitStatuses reads GET /repos/{repository}/commits/{commit}/status page by
// page until every status is held. When a context appears twice, the status
// with the later update time (then the larger id) is kept.
func (g GitHub) CommitStatuses(ctx context.Context, repository, commit string) (CommitStatuses, error) {
	out := CommitStatuses{Repository: repository, Commit: commit}
	index := map[string]int{}
	seen := 0
	for page := 1; page <= maxStatusPages; page++ {
		query := url.Values{"per_page": {strconv.Itoa(statusPageSize)}, "page": {strconv.Itoa(page)}}
		path := "/repos/" + repository + "/commits/" + url.PathEscape(commit) + "/status?" + query.Encode()
		var combined githubCombinedStatus
		if err := g.doStatusRead(ctx, "commits.status", repository, path, &combined); err != nil {
			return CommitStatuses{}, fmt.Errorf("read the statuses of %s@%s: %w", repository, commit, err)
		}
		for _, s := range combined.Statuses {
			s.Description = strings.TrimSpace(s.Description)
			i, ok := index[s.Context]
			if !ok {
				index[s.Context] = len(out.Statuses)
				out.Statuses = append(out.Statuses, s)
				continue
			}
			if later(s, out.Statuses[i]) {
				out.Statuses[i] = s
			}
		}
		seen += len(combined.Statuses)
		if len(combined.Statuses) < statusPageSize || (combined.TotalCount > 0 && seen >= combined.TotalCount) {
			return out, nil
		}
	}
	return CommitStatuses{}, fmt.Errorf("read the statuses of %s@%s: more than %d pages", repository, commit, maxStatusPages)
}

func later(a, b CommitStatus) bool {
	if !a.UpdatedAt.Equal(b.UpdatedAt) {
		return a.UpdatedAt.After(b.UpdatedAt)
	}
	return a.ID > b.ID
}
