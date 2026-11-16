package delivery

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/ghlog"
	"github.com/kpenfound/osmia/internal/pulls"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/kpenfound/osmia/internal/workspace"
)

// Push is one completed automatic push of a workstream's feature branch to
// its pull request: the final review whose rebased branch it signs, the
// unsigned rebased commit (Reviewed) and the signed commit that reached the
// branch (Commit), when it did, and the pull request it belongs to. Number
// identifies the push among the workstream's pushes, from 1, so later
// records can name the push they concern.
type Push struct {
	Number      int
	Review      int
	Operation   string
	Reviewed    string
	Commit      string
	At          time.Time
	PullRequest int
	URL         string
	Upstream    string
	Fork        string
	Branch      string
	Base        string
	Draft       bool
}

func pushOf(p DeliveryPublication) Push {
	return Push{Number: p.Push, Review: p.Review, Operation: p.Operation, Reviewed: p.Reviewed, Commit: p.Commit, At: p.PushedAt, PullRequest: p.PullRequest, URL: p.URL, Upstream: p.Upstream, Fork: p.Fork, Branch: p.Branch, Base: p.Base, Draft: p.Draft}
}

// Pushes returns the workstream's completed automatic pushes, oldest first:
// those whose commit reached the feature branch of the push repository and
// whose pull request is recorded.
func Pushes(repository *trace.Repository, stream config.WorkstreamID) ([]Push, error) {
	records, err := Publications(repository, stream)
	if err != nil {
		return nil, err
	}
	var out []Push
	for _, p := range records {
		if p.Push > 0 && p.Status == PublicationOpened && p.PullRequest > 0 {
			out = append(out, pushOf(p))
		}
	}
	return out, nil
}

// CurrentPush returns the workstream's latest completed automatic push, and
// whether one is recorded.
func CurrentPush(repository *trace.Repository, stream config.WorkstreamID) (Push, bool, error) {
	pushes, err := Pushes(repository, stream)
	if err != nil || len(pushes) == 0 {
		return Push{}, false, err
	}
	return pushes[len(pushes)-1], true, nil
}

// PushOfCommit returns the latest completed automatic push that signed the
// rebased commit reviewed, and whether there is one.
func PushOfCommit(repository *trace.Repository, stream config.WorkstreamID, reviewed string) (Push, bool, error) {
	pushes, err := Pushes(repository, stream)
	if err != nil {
		return Push{}, false, err
	}
	for _, p := range slices.Backward(pushes) {
		if p.Reviewed == reviewed {
			return p, true, nil
		}
	}
	return Push{}, false, nil
}

// PublishDraft signs the rebased commit of report with the drafted messages
// and pushes it to the workstream's feature branch, then opens the pull
// request as a draft if the workstream has none, with no approval. Every
// later final review pushes its rebased commit to the same pull request,
// draft or ready. It returns why nothing may be pushed (the pull request is
// merged or closed, or the branch or pull request is not the workstream's),
// and an error for anything a retry can settle.
//
// final/publication.json records each push before it happens, with its
// number, and records it opened with the pull request and the time the
// commit reached the branch once the pull request is verified at the pushed
// commit. A retry reads those records, the remote branch and the pull
// requests again, so a push and a pull request are made once. The service
// holding no pull request credential pushes nothing.
func (a *FinalReviewer) PublishDraft(ctx context.Context, cfg *config.Config, stream config.WorkstreamID, op coreadapter.Operation, report FinalReport) (string, error) {
	client := a.S.PullRequests
	if !pulls.Credentialed(client) {
		return "", nil
	}
	if report.Upstream == nil {
		return "final review records no delivery base", nil
	}
	ctx = ghlog.WithScope(ctx, ghlog.Scope{Project: string(a.Repository.Project()), Workstream: string(stream)})
	branch, fork, style := report.Branch, cfg.Project.PushRepository(), cfg.Project.Landing
	upstream, base := cfg.Project.Upstream, cfg.Project.BaseBranch
	if report.Upstream.Workstream != "" {
		upstream, base = fork, report.Upstream.Branch
	}
	if branch == cfg.Project.BaseBranch {
		return "delivery cannot push to the project base branch", nil
	}
	prior, err := Publications(a.Repository, stream)
	if err != nil {
		return "", err
	}
	number := 1
	var mine *DeliveryPublication
	for i, d := range prior {
		number = max(number, d.Push+1)
		if d.Push > 0 && d.Operation == op.ID {
			mine = &prior[i]
		}
	}
	if mine != nil {
		if mine.Status == PublicationOpened {
			return "", nil
		}
		number = mine.Push
	}
	g, err := featureWorkspaces(cfg, a.Repository).Of(stream)
	if err != nil {
		return "", err
	}
	remote, err := g.Remote(ctx, fork)
	if err != nil {
		return "", err
	}
	if report.Upstream.Workstream != "" {
		baseTip, exists, err := g.RemoteBranch(ctx, remote, base)
		if err != nil {
			return "", err
		}
		if !exists || baseTip != report.Upstream.Commit {
			return "", fmt.Errorf("the published base branch %s of %s is not at the reviewed base %s yet; the draft pull request waits for it", base, fork, report.Upstream.Commit)
		}
	}

	// A merged or closed pull request is found before anything is pushed, so
	// a push cannot hide it.
	pr, reason, err := a.OwnPullRequest(ctx, client, stream, op.ID, upstream, fork, base, branch)
	if err != nil || reason != "" {
		return reason, err
	}
	tip, exists, err := g.RemoteBranch(ctx, remote, branch)
	if err != nil {
		return "", err
	}

	title, body, _, err := a.S.draftDelivery(cfg, a.Repository, stream, report)
	if err != nil {
		return "", err
	}
	published := ""
	if mine != nil {
		published = mine.Commit
	} else {
		if last, found, err := PushOfCommit(a.Repository, stream, report.Commit); err != nil {
			return "", err
		} else if found && pr != nil && pr.Number == last.PullRequest && exists && tip == last.Commit {
			// The branch already holds this reviewed commit's signed push.
			return "", nil
		}
		if strings.TrimSpace(report.Summary) == "" {
			report.Summary = title
		}
		messages, err := DeliveryMessages(ctx, cfg, report, style)
		if err != nil {
			return "", err
		}
		published, err = (&workspace.Git{Clone: cfg.Project.Clone}).SignDelivery(ctx, op.ID, report.Upstream.Commit, report.Commit, messages, style == SquashStyle)
		if err != nil {
			return "pre-publication amendment failed: " + err.Error(), nil
		}
	}
	if exists && tip != published && !slices.ContainsFunc(prior, func(d DeliveryPublication) bool { return d.Commit == tip && d.Branch == branch && d.Fork == fork }) {
		return fmt.Sprintf("feature branch %s of %s is at %s, which no push of this workstream made", branch, fork, tip), nil
	}

	record := DeliveryPublication{BaseWorkstream: report.Upstream.Workstream, Push: number, Review: report.Review, Operation: op.ID, Status: PublicationPushing, Style: style, Fork: fork, Remote: remote, Branch: branch, Reviewed: report.Commit, Commit: published,
		Upstream: upstream, Base: base, Draft: true, Title: title, Description: body, DescriptionHash: DescriptionHash(body)}
	if pr != nil {
		record.Title, record.Description, record.DescriptionHash, record.Draft = pr.Title, pr.Body, DescriptionHash(pr.Body), pr.Draft
	}
	publisher := &Publisher{S: a.S, Repository: a.Repository}
	if mine == nil {
		if err := publisher.RecordPublication(ctx, stream, record); err != nil {
			return "", err
		}
	}
	if err := a.S.Step("draft-recorded"); err != nil {
		return "", err
	}
	if !exists || tip != published {
		if err := g.Push(ctx, remote, published, branch, tip); err != nil {
			return "", fmt.Errorf("push %s to %s of %s: %w", published, branch, fork, err)
		}
	}
	pushedAt := a.S.Now()
	if err := a.S.Step("draft-pushed"); err != nil {
		return "", err
	}

	found, err := client.Find(ctx, upstream, fork, branch)
	if err != nil {
		return "", err
	}
	var current pulls.PullRequest
	if pr != nil {
		i := slices.IndexFunc(found, func(p pulls.PullRequest) bool { return p.Number == pr.Number })
		if i < 0 {
			return fmt.Sprintf("pull request #%d of %s is missing", pr.Number, upstream), nil
		}
		current = found[i]
	} else if adopted, reason := adoptablePullRequest(found, fork, base, branch); reason != "" {
		return reason, nil
	} else if adopted != nil {
		current = *adopted
	} else {
		created, err := client.Create(ctx, upstream, pulls.New{HeadRepository: fork, Head: branch, Base: base, Title: title, Body: body, Draft: true})
		if err != nil {
			return "", err
		}
		if err := a.S.Step("draft-opened"); err != nil {
			return "", err
		}
		found, err = client.Find(ctx, upstream, fork, branch)
		if err != nil {
			return "", err
		}
		if len(found) != 1 || found[0].Number != created.Number {
			return "", fmt.Errorf("cannot verify pull request from %s:%s after creation", fork, branch)
		}
		current = found[0]
	}
	if outcome, finished := PullRequestOutcomeOf(current); finished {
		if err := RecordOwnPullRequestOutcome(ctx, a.Repository, a.S.Now(), stream, op.ID, current.Number, outcome); err != nil {
			return "", err
		}
		return fmt.Sprintf("pull request #%d of %s is %s, and %s was pushed to %s before that was seen", current.Number, upstream, outcome, published, branch), nil
	}
	if current.HeadCommit != published {
		if slices.ContainsFunc(prior, func(d DeliveryPublication) bool {
			return d.Commit == current.HeadCommit && d.Branch == branch && d.Fork == fork
		}) {
			return "", fmt.Errorf("pull request #%d of %s is still at %s after %s was pushed to %s", current.Number, upstream, current.HeadCommit, published, branch)
		}
		return fmt.Sprintf("pull request #%d of %s is at %s, not at the pushed %s", current.Number, upstream, current.HeadCommit, published), nil
	}
	record.Title, record.Description, record.DescriptionHash = current.Title, current.Body, DescriptionHash(current.Body)
	record.Status, record.PullRequest, record.URL, record.Draft, record.PushedAt = PublicationOpened, current.Number, current.URL, current.Draft, pushedAt
	if err := publisher.RecordPublication(ctx, stream, record); err != nil {
		return "", err
	}
	// The push supersedes the earlier ones: a question the owner was asked
	// about the checks of one of them is closed.
	if err := a.S.closeObsoleteChecksChoices(ctx, a.Repository, stream); err != nil {
		return "", err
	}
	return "", a.S.Step("draft-published")
}

// OwnPullRequest finds the workstream's own pull request: the one a
// publication recorded, or else the only open one whose head is the feature
// branch. It returns nil, with no reason, when the workstream has none yet.
// A recorded pull request that is merged or closed is recorded as the
// outcome and returned as the reason nothing may be pushed, as is a pull
// request that is not the workstream's to push to.
func (a *FinalReviewer) OwnPullRequest(ctx context.Context, client pulls.Client, stream config.WorkstreamID, cause, upstream, fork, base, branch string) (*pulls.PullRequest, string, error) {
	found, err := client.Find(ctx, upstream, fork, branch)
	if err != nil {
		return nil, "", err
	}
	recorded, hasRecord, err := CurrentOwnPublication(a.Repository, stream)
	if err != nil {
		return nil, "", err
	}
	number := 0
	if hasRecord {
		number = recorded.PullRequest
	}
	if err := RecordOwnPullRequestOutcomeFromFind(ctx, a.Repository, a.S.Now(), stream, cause, number, found); err != nil {
		return nil, "", err
	}
	if number > 0 {
		i := slices.IndexFunc(found, func(p pulls.PullRequest) bool { return p.Number == number })
		if i < 0 {
			return nil, fmt.Sprintf("pull request #%d of %s is missing", number, upstream), nil
		}
		pr := found[i]
		if outcome, finished := PullRequestOutcomeOf(pr); finished {
			return nil, fmt.Sprintf("pull request #%d of %s is %s, so nothing was pushed to it", pr.Number, upstream, outcome), nil
		}
		return &pr, "", nil
	}
	pr, reason := adoptablePullRequest(found, fork, base, branch)
	return pr, reason, nil
}

// adoptablePullRequest returns the one open pull request among found, which
// are those with the feature branch as head, when it is against base from
// the push repository, and why it cannot be the workstream's when it is not,
// or when a pull request of the branch is already merged or closed.
func adoptablePullRequest(found []pulls.PullRequest, fork, base, branch string) (*pulls.PullRequest, string) {
	var open []pulls.PullRequest
	var seen []string
	for _, pr := range found {
		seen = append(seen, fmt.Sprintf("#%d (%s, against %s at %s)", pr.Number, pr.State, pr.Base, pr.HeadCommit))
		if _, finished := PullRequestOutcomeOf(pr); !finished {
			open = append(open, pr)
		}
	}
	switch {
	case len(found) == 0:
		return nil, ""
	case len(open) != 1 || len(found) != 1:
		return nil, fmt.Sprintf("%s of %s already has pull requests %s, and none is the one open against %s that this workstream pushes to", branch, fork, strings.Join(seen, ", "), base)
	case open[0].Base != base || open[0].HeadRepository != fork || open[0].Head != branch:
		return nil, fmt.Sprintf("%s of %s already has pull request %s, which is not the one open against %s from the push repository", branch, fork, seen[0], base)
	}
	return &open[0], ""
}
