// Package pulls finds and opens pull requests for the service. Credentials
// stay in the service process: callers hold them in a Client and no session
// receives them.
package pulls

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// PullRequest is one pull request of a repository. Head is the head branch,
// HeadRepository the owner/repository it lives in and HeadCommit the commit
// it points at; Base is the branch the pull request targets. State is open
// or closed, and a merged pull request is closed.
type PullRequest struct {
	Number         int    `json:"number"`
	URL            string `json:"url"`
	State          string `json:"state"`
	Merged         bool   `json:"merged,omitempty"`
	Head           string `json:"head"`
	HeadRepository string `json:"head_repository"`
	HeadCommit     string `json:"head_commit"`
	Base           string `json:"base"`
	Title          string `json:"title"`
	Body           string `json:"body"`
}

// New is a pull request to open on a repository from branch Head of the
// repository HeadRepository.
type New struct {
	HeadRepository string
	Head           string
	Base           string
	Title          string
	Body           string
}

// Client finds and opens pull requests.
type Client interface {
	// Find returns every pull request of repository, open or closed, whose
	// head is branch of headRepository.
	Find(ctx context.Context, repository, headRepository, branch string) ([]PullRequest, error)
	// Create opens a pull request on repository.
	Create(ctx context.Context, repository string, pr New) (PullRequest, error)
}

// MaxResponse bounds the API response GitHub reads.
const MaxResponse = 8 << 20

// GitHub finds and opens pull requests through the GitHub REST API. Token,
// when set, is sent as a bearer token.
type GitHub struct {
	BaseURL string // defaults to https://api.github.com
	Token   string
	HTTP    *http.Client
}

type githubPull struct {
	Number   int     `json:"number"`
	HTMLURL  string  `json:"html_url"`
	State    string  `json:"state"`
	MergedAt *string `json:"merged_at"`
	Title    string  `json:"title"`
	Body     *string `json:"body"`
	Head     struct {
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
	out := PullRequest{Number: p.Number, URL: p.HTMLURL, State: p.State, Merged: p.MergedAt != nil, Head: p.Head.Ref, HeadCommit: p.Head.SHA, Base: p.Base.Ref, Title: p.Title}
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
	if err := g.do(ctx, http.MethodGet, "/repos/"+repository+"/pulls?"+query.Encode(), nil, http.StatusOK, &pulls); err != nil {
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
// pr.HeadRepository's owner.
func (g GitHub) Create(ctx context.Context, repository string, pr New) (PullRequest, error) {
	body := map[string]any{"title": pr.Title, "head": owner(pr.HeadRepository) + ":" + pr.Head, "base": pr.Base, "body": pr.Body}
	var created githubPull
	if err := g.do(ctx, http.MethodPost, "/repos/"+repository+"/pulls", body, http.StatusCreated, &created); err != nil {
		return PullRequest{}, fmt.Errorf("open a pull request on %s from %s:%s: %w", repository, pr.HeadRepository, pr.Head, err)
	}
	return created.pull(), nil
}

func (g GitHub) do(ctx context.Context, method, path string, in any, want int, out any) error {
	base := g.BaseURL
	if base == "" {
		base = "https://api.github.com"
	}
	var reader io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(base, "/")+path, reader)
	if err != nil {
		return err
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
		return fmt.Errorf("request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != want {
		return fmt.Errorf("GitHub answered %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponse+1))
	if err != nil || len(data) > MaxResponse {
		return fmt.Errorf("unreadable or oversized response")
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("invalid response")
	}
	return nil
}
