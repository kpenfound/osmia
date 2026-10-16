// Package pulls finds, opens and updates pull requests for the service. Credentials
// stay in the service process: callers hold them in a Client and no session
// receives them.
package pulls

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/kpenfound/osmia/internal/ghlog"
)

// PullRequest is one pull request of a repository. Head is the head branch,
// HeadRepository the owner/repository it lives in and HeadCommit the commit
// it points at; Base is the branch the pull request targets. State is open
// or closed, and a merged pull request is closed. Draft is a pull request
// opened, or still held, as a draft: one the dependent of a merge-order
// workgroup edge publishes before its upstream merges.
type PullRequest struct {
	Number         int    `json:"number"`
	URL            string `json:"url"`
	State          string `json:"state"`
	Merged         bool   `json:"merged,omitempty"`
	MergeCommit    string `json:"merge_commit,omitempty"`
	Draft          bool   `json:"draft,omitempty"`
	Head           string `json:"head"`
	HeadRepository string `json:"head_repository"`
	HeadCommit     string `json:"head_commit"`
	Base           string `json:"base"`
	Title          string `json:"title"`
	Body           string `json:"body"`
}

// New is a pull request to open on a repository from branch Head of the
// repository HeadRepository. Draft opens it as a draft pull request.
type New struct {
	HeadRepository string
	Head           string
	Base           string
	Title          string
	Body           string
	Draft          bool
}

// Update describes an approved change to a pull request's target and text.
type Update struct {
	Base  string `json:"base"`
	Title string `json:"title"`
	Body  string `json:"body"`
}

// Client finds, opens and updates pull requests.
type Client interface {
	// Find returns every pull request of repository, open or closed, whose
	// head is branch of headRepository.
	Find(ctx context.Context, repository, headRepository, branch string) ([]PullRequest, error)
	// Create opens a pull request on repository.
	Create(ctx context.Context, repository string, pr New) (PullRequest, error)
	// Update changes the base branch, title and body of an existing request.
	Update(ctx context.Context, repository string, number int, change Update) (PullRequest, error)
	// Ready converts a draft pull request to one ready for review. It is
	// never called to open or change anything else about the request: only
	// to drop its draft status once the dependency it drafted for resolves
	// and a fresh delivery approval authorises marking it ready.
	Ready(ctx context.Context, repository string, number int) (PullRequest, error)
}

// Credentialed reports whether client holds the credential that opening pull
// requests needs. A client that does not report it is assumed to hold one.
func Credentialed(client Client) bool {
	if c, ok := client.(interface{ Credentialed() bool }); ok {
		return c.Credentialed()
	}
	return client != nil
}

// MaxResponse bounds the API response GitHub reads.
const MaxResponse = 8 << 20

// GitHub finds, opens and updates pull requests through the GitHub REST API. Token,
// when set, is sent as a bearer token.
type GitHub struct {
	BaseURL string // defaults to https://api.github.com
	Token   string
	HTTP    *http.Client
}

// Credentialed reports whether g has a token; without one GitHub reads public
// repositories but refuses to open a pull request.
func (g GitHub) Credentialed() bool { return g.Token != "" }

type githubPull struct {
	Number      int     `json:"number"`
	NodeID      string  `json:"node_id"`
	HTMLURL     string  `json:"html_url"`
	State       string  `json:"state"`
	MergedAt    *string `json:"merged_at"`
	MergeCommit string  `json:"merge_commit_sha"`
	Draft       bool    `json:"draft"`
	Title       string  `json:"title"`
	Body        *string `json:"body"`
	Head        struct {
		Ref  string `json:"ref"`
		SHA  string `json:"sha"`
		Repo *struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

func (p githubPull) pull() PullRequest {
	out := PullRequest{Number: p.Number, URL: p.HTMLURL, State: p.State, Merged: p.MergedAt != nil, MergeCommit: p.MergeCommit, Draft: p.Draft, Head: p.Head.Ref, HeadCommit: p.Head.SHA, Base: p.Base.Ref, Title: p.Title}
	if p.Head.Repo != nil {
		out.HeadRepository = p.Head.Repo.FullName
	}
	if p.Body != nil {
		out.Body = *p.Body
	}
	return out
}

func owner(repository string) string {
	o, _, _ := strings.Cut(repository, "/")
	return o
}

// Find lists the pull requests whose head is owner:branch, where owner is
// headRepository's owner, and keeps those whose head repository is
// headRepository.
func (g GitHub) Find(ctx context.Context, repository, headRepository, branch string) ([]PullRequest, error) {
	query := url.Values{"head": {owner(headRepository) + ":" + branch}, "state": {"all"}, "per_page": {"100"}}
	var pulls []githubPull
	if err := g.do(ctx, "pulls.find", ghlog.Read, repository, http.MethodGet, "/repos/"+repository+"/pulls?"+query.Encode(), nil, http.StatusOK, &pulls); err != nil {
		return nil, fmt.Errorf("find pull requests of %s from %s:%s: %w", repository, headRepository, branch, err)
	}
	var out []PullRequest
	for _, p := range pulls {
		pr := p.pull()
		if strings.EqualFold(pr.HeadRepository, headRepository) && pr.Head == branch {
			out = append(out, pr)
		}
	}
	return out, nil
}

// Create opens the pull request from owner:head, where owner is
// pr.HeadRepository's owner. Within one repository, head is unqualified.
func (g GitHub) Create(ctx context.Context, repository string, pr New) (PullRequest, error) {
	body := map[string]any{"title": pr.Title, "head": owner(pr.HeadRepository) + ":" + pr.Head, "base": pr.Base, "body": pr.Body, "draft": pr.Draft}
	if strings.EqualFold(repository, pr.HeadRepository) {
		body["head"] = pr.Head
	}
	var created githubPull
	if err := g.do(ctx, "pulls.create", ghlog.Write, repository, http.MethodPost, "/repos/"+repository+"/pulls", body, http.StatusCreated, &created); err != nil {
		return PullRequest{}, fmt.Errorf("open a pull request on %s from %s:%s: %w", repository, pr.HeadRepository, pr.Head, err)
	}
	return created.pull(), nil
}

// Update retargets an existing pull request and applies its approved text.
func (g GitHub) Update(ctx context.Context, repository string, number int, change Update) (PullRequest, error) {
	var updated githubPull
	if err := g.do(ctx, "pulls.update", ghlog.Write, repository, http.MethodPatch, fmt.Sprintf("/repos/%s/pulls/%d", repository, number), change, http.StatusOK, &updated); err != nil {
		return PullRequest{}, fmt.Errorf("update pull request %s#%d: %w", repository, number, err)
	}
	return updated.pull(), nil
}

// Ready converts a draft pull request to one ready for review. GitHub's REST
// API has no field for this, so Ready first reads the pull request's
// GraphQL node ID over REST, runs the markPullRequestReadyForReview mutation
// against it, then reads the pull request again over REST for the result in
// the same shape every other method returns.
func (g GitHub) Ready(ctx context.Context, repository string, number int) (PullRequest, error) {
	current, err := g.get(ctx, repository, number)
	if err != nil {
		return PullRequest{}, fmt.Errorf("read pull request %s#%d: %w", repository, number, err)
	}
	if !current.Draft {
		return current.pull(), nil
	}
	var resp struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	query := `mutation($id: ID!) { markPullRequestReadyForReview(input: {pullRequestId: $id}) { clientMutationId } }`
	in := map[string]any{"query": query, "variables": map[string]string{"id": current.NodeID}}
	if err := g.do(ctx, "pulls.ready", ghlog.Write, repository, http.MethodPost, "/graphql", in, http.StatusOK, &resp); err != nil {
		return PullRequest{}, fmt.Errorf("mark pull request %s#%d ready for review: %w", repository, number, err)
	}
	if len(resp.Errors) > 0 {
		return PullRequest{}, fmt.Errorf("mark pull request %s#%d ready for review: %s", repository, number, resp.Errors[0].Message)
	}
	updated, err := g.get(ctx, repository, number)
	if err != nil {
		return PullRequest{}, fmt.Errorf("read pull request %s#%d after marking it ready: %w", repository, number, err)
	}
	return updated.pull(), nil
}

// get reads one pull request's current state over the REST API.
func (g GitHub) get(ctx context.Context, repository string, number int) (githubPull, error) {
	var out githubPull
	err := g.do(ctx, "pulls.get", ghlog.Read, repository, http.MethodGet, fmt.Sprintf("/repos/%s/pulls/%d", repository, number), nil, http.StatusOK, &out)
	return out, err
}

// do performs one GitHub REST request and records one ghlog entry for it,
// whether it succeeds or fails.
func (g GitHub) do(ctx context.Context, action, access, repository, method, path string, in any, want int, out any) error {
	start := time.Now()
	status, err := g.request(ctx, method, path, in, want, out)
	ghlog.Log(ctx, start, ghlog.Entry{Action: action, Access: access, Repository: repository, Method: method, Path: path, Status: status, NoResponse: status == 0, Err: err})
	return err
}

func (g GitHub) request(ctx context.Context, method, path string, in any, want int, out any) (status int, err error) {
	base := g.BaseURL
	if base == "" {
		base = "https://api.github.com"
	}
	var reader io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return 0, err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(base, "/")+path, reader)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if g.Token != "" {
		req.Header.Set("Authorization", "Bearer "+g.Token)
	}
	client := g.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("request failed")
	}
	defer resp.Body.Close()
	status = resp.StatusCode
	if status != want {
		return status, statusError(resp)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponse+1))
	if err != nil || len(data) > MaxResponse {
		return status, fmt.Errorf("unreadable or oversized response")
	}
	if err := json.Unmarshal(data, out); err != nil {
		return status, fmt.Errorf("invalid response")
	}
	return status, nil
}

// maxErrorBody bounds the error response statusError reads, and
// maxErrorText each value it keeps from it.
const (
	maxErrorBody = 64 << 10
	maxErrorText = 500
)

// statusError describes an unexpected answer: its status, GitHub's message
// and documentation link, the token permissions GitHub says the request
// needs, and the rate limit when it is exhausted or GitHub asks to retry
// later.
func statusError(resp *http.Response) error {
	var body struct {
		Message          string `json:"message"`
		DocumentationURL string `json:"documentation_url"`
	}
	data, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	_ = json.Unmarshal(data, &body)
	text := fmt.Sprintf("GitHub answered %d", resp.StatusCode)
	if message := errorText(body.Message); message != "" {
		text += ": " + message
	}
	if permissions := errorText(resp.Header.Get("X-Accepted-GitHub-Permissions")); permissions != "" {
		text += "; the request needs the token permissions " + permissions
	}
	if resp.Header.Get("X-RateLimit-Remaining") == "0" {
		text += "; the rate limit is exhausted"
		if reset, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			text += " until " + time.Unix(reset, 0).UTC().Format(time.RFC3339)
		}
	}
	if after, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && after >= 0 {
		text += fmt.Sprintf("; GitHub asks to retry after %ds", after)
	}
	if doc := errorText(body.DocumentationURL); doc != "" {
		text += "; see " + doc
	}
	return errors.New(text)
}

// errorText is one value of an error response, on one line and bounded.
func errorText(s string) string {
	s = strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s))
	if r := []rune(s); len(r) > maxErrorText {
		s = string(r[:maxErrorText]) + "…"
	}
	return s
}

// CancellationClient changes only the disposition of an existing unmerged PR.
// The service calls it only for an owner-approved cancellation choice.
type CancellationClient interface {
	Close(context.Context, string, int) (PullRequest, error)
	Draft(context.Context, string, int) (PullRequest, error)
}

func (g GitHub) Close(ctx context.Context, repository string, number int) (PullRequest, error) {
	current, err := g.get(ctx, repository, number)
	if err != nil {
		return PullRequest{}, err
	}
	if current.MergedAt != nil || current.State == "closed" {
		return current.pull(), nil
	}
	var result githubPull
	err = g.do(ctx, "pulls.close", ghlog.Write, repository, http.MethodPatch, "/repos/"+repository+"/pulls/"+strconv.Itoa(number), map[string]any{"state": "closed"}, http.StatusOK, &result)
	return result.pull(), err
}

func (g GitHub) Draft(ctx context.Context, repository string, number int) (PullRequest, error) {
	current, err := g.get(ctx, repository, number)
	if err != nil {
		return PullRequest{}, err
	}
	if current.MergedAt != nil || current.State != "open" || current.Draft {
		return current.pull(), nil
	}
	var result struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	err = g.do(ctx, "pulls.draft", ghlog.Write, repository, http.MethodPost, "/graphql", map[string]any{"query": `mutation($id: ID!) { convertPullRequestToDraft(input: {pullRequestId: $id}) { clientMutationId } }`, "variables": map[string]any{"id": current.NodeID}}, http.StatusOK, &result)
	if err != nil {
		return PullRequest{}, err
	}
	if len(result.Errors) > 0 {
		return PullRequest{}, fmt.Errorf("convert pull request %s#%d to draft: %s", repository, number, result.Errors[0].Message)
	}
	current, err = g.get(ctx, repository, number)
	return current.pull(), err
}
