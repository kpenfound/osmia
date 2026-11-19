package fixturekit

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/kpenfound/osmia/internal/pulls"
)

// FakePulls is a pull Request host in memory, standing in for the real
// remote hosting the delivery publishes to. lose makes the next Create open
// the pull Request and then fail, as a response lost in transit does. It
// leaves unverified the real host's network behaviour, authentication and
// actual merge or close semantics.
type FakePulls struct {
	Mu       sync.Mutex
	Prs      []pulls.PullRequest
	Finds    int
	SyncHead bool
	// HeadLag counts the Finds, among those with SyncHead set, that still answer
	// with a pull request's earlier head after its branch moved, as a host does
	// while it processes a push.
	HeadLag int
	Updates int
	Creates int
	Readies int
	// Closes counts the calls that closed a pull request with a comment, and
	// Comments holds the comments they posted, as {pull request number, text}.
	Closes   int
	Comments []PullComment
	Lose     bool
	Fork    func() string
	// Branches, when set, answers the tip of the named feature branch of the
	// fork in place of Fork, for a host that holds several workstreams' pull
	// requests.
	Branches func(branch string) string
	// Statuses, when set, answers the combined status read of a commit.
	Statuses func(ctx context.Context, repository, commit string) (pulls.CommitStatuses, error)
	// findErr, when set, fails every Find as a refusing host does.
	FindErr error
	// uncredentialed reports the client holding no credential, as GitHub
	// without a token does.
	Uncredentialed bool
}

// tip is the commit the fork's branch is at.
func (c *FakePulls) tip(branch string) string {
	if c.Branches != nil {
		return c.Branches(branch)
	}
	return c.Fork()
}

func (c *FakePulls) Credentialed() bool {
	c.Mu.Lock()
	defer c.Mu.Unlock()
	return !c.Uncredentialed
}

func (c *FakePulls) Find(_ context.Context, repository, headRepository, branch string) ([]pulls.PullRequest, error) {
	c.Mu.Lock()
	defer c.Mu.Unlock()
	c.Finds++
	if c.FindErr != nil {
		return nil, c.FindErr
	}
	var out []pulls.PullRequest
	for i := range c.Prs {
		pr := &c.Prs[i]
		if pr.HeadRepository == headRepository && pr.Head == branch {
			if c.SyncHead {
				if tip := c.tip(pr.Head); tip != pr.HeadCommit && c.HeadLag > 0 {
					c.HeadLag--
				} else {
					pr.HeadCommit = tip
				}
			}
			out = append(out, *pr)
		}
	}
	return out, nil
}

func (c *FakePulls) Create(_ context.Context, repository string, n pulls.New) (pulls.PullRequest, error) {
	c.Mu.Lock()
	defer c.Mu.Unlock()
	c.Creates++
	number := 100 + len(c.Prs)
	pr := pulls.PullRequest{Number: number, URL: fmt.Sprintf("https://github.com/%s/pull/%d", repository, number), State: "open", Draft: n.Draft, Head: n.Head, HeadRepository: n.HeadRepository, HeadCommit: c.tip(n.Head), Base: n.Base, Title: n.Title, Body: n.Body}
	c.Prs = append(c.Prs, pr)
	if c.Lose {
		c.Lose = false
		return pulls.PullRequest{}, errors.New("connection reset")
	}
	return pr, nil
}

// Ready converts a draft pull Request to one ready for review, standing in
// for GitHub's markPullRequestReadyForReview mutation.
func (c *FakePulls) Ready(_ context.Context, repository string, number int) (pulls.PullRequest, error) {
	c.Mu.Lock()
	defer c.Mu.Unlock()
	c.Readies++
	for i := range c.Prs {
		if c.Prs[i].Number != number {
			continue
		}
		c.Prs[i].Draft = false
		if c.Lose {
			c.Lose = false
			return pulls.PullRequest{}, errors.New("connection reset")
		}
		return c.Prs[i], nil
	}
	return pulls.PullRequest{}, errors.New("pull request not found")
}

// PullComment is one comment posted on a pull request of the fake host.
type PullComment struct {
	Number int
	Text   string
}

// CloseWithComment closes an open pull request with a comment, as the host's
// comment and close calls together do. It leaves a merged or closed pull
// request alone and posts a comment text only once per pull request.
func (c *FakePulls) CloseWithComment(_ context.Context, repository string, number int, comment string) (pulls.PullRequest, error) {
	c.Mu.Lock()
	defer c.Mu.Unlock()
	for i := range c.Prs {
		pr := &c.Prs[i]
		if pr.Number != number {
			continue
		}
		if pr.State != "open" || pr.Merged {
			return *pr, nil
		}
		posted := false
		for _, existing := range c.Comments {
			posted = posted || existing.Number == number && existing.Text == comment
		}
		if !posted {
			c.Comments = append(c.Comments, PullComment{Number: number, Text: comment})
		}
		pr.State = "closed"
		c.Closes++
		if c.Lose {
			c.Lose = false
			return pulls.PullRequest{}, errors.New("connection reset")
		}
		return *pr, nil
	}
	return pulls.PullRequest{}, errors.New("pull request not found")
}

func ConfigureDeliverySigner(t *testing.T, clone string) string {
	t.Helper()
	signer := filepath.Join(t.TempDir(), "fake-gpg")
	Must(t, os.WriteFile(signer, []byte("#!/bin/sh\ncat >/dev/null\nprintf '%s\\n' '-----BEGIN PGP SIGNATURE-----' 'fixture signature' '-----END PGP SIGNATURE-----'\nprintf '%s\\n' '[GNUPG:] SIG_CREATED D 1 10 00 0 FAKE' >&2\n"), 0700))
	for k, v := range map[string]string{"user.name": "Delivery Owner", "user.email": "delivery@example.invalid", "user.signingkey": "fixture-key", "gpg.format": "openpgp", "gpg.program": signer, "commit.gpgsign": "false"} {
		DemoGit(t, filepath.Dir(clone), "-C", clone, "config", k, v)
	}
	return signer
}

func (c *FakePulls) Update(_ context.Context, repository string, number int, change pulls.Update) (pulls.PullRequest, error) {
	c.Mu.Lock()
	defer c.Mu.Unlock()
	c.Updates++
	for i := range c.Prs {
		if c.Prs[i].Number != number {
			continue
		}
		c.Prs[i].Base, c.Prs[i].Title, c.Prs[i].Body = change.Base, change.Title, change.Body
		if c.Lose {
			c.Lose = false
			return pulls.PullRequest{}, errors.New("connection reset")
		}
		return c.Prs[i], nil
	}
	return pulls.PullRequest{}, errors.New("pull request not found")
}

type RepositoryPulls map[string]*FakePulls

func (r RepositoryPulls) Find(ctx context.Context, repository, head, branch string) ([]pulls.PullRequest, error) {
	return r[repository].Find(ctx, repository, head, branch)
}
func (r RepositoryPulls) Create(ctx context.Context, repository string, n pulls.New) (pulls.PullRequest, error) {
	return r[repository].Create(ctx, repository, n)
}

func (r RepositoryPulls) Ready(ctx context.Context, repository string, number int) (pulls.PullRequest, error) {
	return r[repository].Ready(ctx, repository, number)
}

func (r RepositoryPulls) CloseWithComment(ctx context.Context, repository string, number int, comment string) (pulls.PullRequest, error) {
	return r[repository].CloseWithComment(ctx, repository, number, comment)
}

func (r RepositoryPulls) Update(ctx context.Context, repository string, number int, change pulls.Update) (pulls.PullRequest, error) {
	return r[repository].Update(ctx, repository, number, change)
}

// PublicationFork prepares a local bare delivery remote and its fake host.
func PublicationFork(t *testing.T, clone string, tip func() string) (string, *FakePulls) {
	t.Helper()
	home := filepath.Dir(clone)
	fork := filepath.Join(home, "remotes", "owner", "dagger.git")
	Must(t, os.MkdirAll(filepath.Dir(fork), 0700))
	DemoGit(t, home, "init", "--quiet", "--bare", fork)
	DemoGit(t, home, "-C", clone, "remote", "add", "origin", fork)
	return fork, &FakePulls{Fork: tip}
}
