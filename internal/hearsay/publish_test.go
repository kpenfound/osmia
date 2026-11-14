package hearsay

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type recorded struct {
	source string
	auth   string
	wire   Wire
}

type endpoint struct {
	*httptest.Server
	mu     sync.Mutex
	posts  []recorded
	status int
}

func newEndpoint(t *testing.T) *endpoint {
	t.Helper()
	e := &endpoint{status: http.StatusAccepted}
	e.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var wire Wire
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&wire); err != nil {
			t.Errorf("request is not a Wire body: %v", err)
		}
		e.mu.Lock()
		defer e.mu.Unlock()
		if e.status == http.StatusAccepted {
			e.posts = append(e.posts, recorded{strings.TrimPrefix(r.URL.Path, "/hooks/"), r.Header.Get("Authorization"), wire})
		}
		w.WriteHeader(e.status)
	}))
	t.Cleanup(e.Close)
	return e
}

func (e *endpoint) take() []recorded {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := e.posts
	e.posts = nil
	return out
}

func version(body, updated string) Wire {
	return Wire{Kind: KindDocument, Project: "p", Document: "d", Name: "D", Body: body, CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: updated}
}

func posted(res Result) map[string]Posted {
	out := map[string]Posted{}
	for _, e := range res.Entries {
		out[Ref(e.Source, e.Key)] = e.Posted
	}
	return out
}

func TestReconcilePostsParentsFirstAndNothingWhenUnchanged(t *testing.T) {
	e := newEndpoint(t)
	poster := Poster{URL: e.URL, Token: "tok"}
	project := Record{Source: "work", Key: "project/p", Tier: TierProject, Versions: []Wire{{Kind: KindProject, Project: "p", Name: "P", CreatedAt: "2026-01-01T00:00:00Z"}}}
	stream := Record{Source: "work", Key: "workstream/w", Tier: TierWorkstream, After: []string{project.Ref()}, Versions: []Wire{{Kind: KindWorkstream, Project: "p", Workstream: "w", Name: "W", CreatedAt: "2026-01-01T00:00:00Z"}}}
	doc := Record{Source: "owner", Key: "document/d", Tier: TierRecord, After: []string{stream.Ref()}, Versions: []Wire{version("one", "2026-01-02T00:00:00Z")}}

	res := Reconcile(context.Background(), poster, []Record{doc, stream, project}, nil)
	got := e.take()
	if res.Posts != 3 || len(got) != 3 || got[0].wire.Kind != KindProject || got[1].wire.Kind != KindWorkstream || got[2].wire.Kind != KindDocument || got[2].source != "owner" || got[0].auth != "Bearer tok" {
		t.Fatalf("first pass: %+v %+v", res, got)
	}
	state := posted(res)
	res = Reconcile(context.Background(), poster, []Record{doc, stream, project}, state)
	if res.Posts != 0 || len(e.take()) != 0 {
		t.Fatalf("an unchanged pass posted: %+v", res)
	}
	changed := doc
	changed.Versions = []Wire{version("two", "2026-01-03T00:00:00Z")}
	res = Reconcile(context.Background(), poster, []Record{project, stream, changed}, state)
	if got = e.take(); res.Posts != 1 || len(got) != 1 || got[0].wire.Body != "two" {
		t.Fatalf("a change posted %+v", got)
	}
}

func TestReconcilePostsEveryRevisionOfAnUnpostedRecordAndTheNewOnesAfterwards(t *testing.T) {
	e := newEndpoint(t)
	poster := Poster{URL: e.URL, Token: "tok"}
	rec := Record{Source: "owner", Key: "document/d", Tier: TierRecord, Versions: []Wire{version("a", "2026-01-02T00:00:00Z"), version("a", "2026-01-02T12:00:00Z"), version("b", "2026-01-03T00:00:00Z")}}
	res := Reconcile(context.Background(), poster, []Record{rec}, nil)
	got := e.take()
	if len(got) != 2 || got[0].wire.Body != "a" || got[1].wire.Body != "b" {
		t.Fatalf("revisions: %+v", got)
	}
	rec.Versions = append(rec.Versions, version("c", "2026-01-04T00:00:00Z"), version("d", "2026-01-05T00:00:00Z"))
	res = Reconcile(context.Background(), poster, []Record{rec}, posted(res))
	got = e.take()
	if res.Posts != 2 || len(got) != 2 || got[0].wire.Body != "c" || got[1].wire.Body != "d" {
		t.Fatalf("new revisions: %+v", got)
	}
	// A posted digest that is no revision any more leaves only the current one.
	res = Reconcile(context.Background(), poster, []Record{rec}, map[string]Posted{rec.Ref(): {Digest: "gone"}})
	if got = e.take(); len(got) != 1 || got[0].wire.Body != "d" || res.Posts != 1 {
		t.Fatalf("unknown digest: %+v", got)
	}
}

func TestReconcileKeepsUpdateTimesIncreasingAndRepostsClearedRecordsIdentically(t *testing.T) {
	e := newEndpoint(t)
	poster := Poster{URL: e.URL, Token: "tok"}
	rec := Record{Source: "work", Key: "k", Versions: []Wire{version("a", "2026-01-02T00:00:00Z")}}
	res := Reconcile(context.Background(), poster, []Record{rec}, nil)
	first := e.take()[0].wire
	// The next revision carries an older evidence time yet is later than the posted one.
	rec.Versions = []Wire{version("b", "2026-01-01T12:00:00Z")}
	Reconcile(context.Background(), poster, []Record{rec}, posted(res))
	second := e.take()[0].wire
	a, _ := time.Parse(time.RFC3339Nano, first.UpdatedAt)
	b, _ := time.Parse(time.RFC3339Nano, second.UpdatedAt)
	if !b.After(a) {
		t.Fatalf("update times %s then %s", first.UpdatedAt, second.UpdatedAt)
	}
	// A cleared digest reposts what was first posted, byte for byte.
	rec.Versions = []Wire{version("a", "2026-01-02T00:00:00Z")}
	Reconcile(context.Background(), poster, []Record{rec}, map[string]Posted{rec.Ref(): {UpdatedAt: b}})
	again := e.take()[0].wire
	if again.Digest() != first.Digest() || again.UpdatedAt != first.UpdatedAt {
		t.Fatalf("repost %+v differs from %+v", again, first)
	}
}

func TestReconcileDoesNotAttemptChildrenOfRefusedParentsAndStopsWhenUnreachable(t *testing.T) {
	e := newEndpoint(t)
	e.status = http.StatusConflict
	poster := Poster{URL: e.URL, Token: "tok"}
	parent := Record{Source: "work", Key: "project/p", Tier: TierProject, Versions: []Wire{{Kind: KindProject, Project: "p", Name: "P", CreatedAt: "2026-01-01T00:00:00Z"}}}
	child := Record{Source: "work", Key: "workstream/w", Tier: TierWorkstream, After: []string{parent.Ref()}, Versions: []Wire{{Kind: KindWorkstream, Project: "p", Workstream: "w", Name: "W", CreatedAt: "2026-01-01T00:00:00Z"}}}
	res := Reconcile(context.Background(), poster, []Record{child, parent}, nil)
	if res.Posts != 0 || len(res.Failures) != 2 || res.Unreachable || len(res.Entries) != 0 {
		t.Fatalf("refused: %+v", res)
	}
	e.Close()
	res = Reconcile(context.Background(), poster, []Record{parent, child}, nil)
	if !res.Unreachable || res.Posts != 0 || len(res.Entries) != 0 {
		t.Fatalf("unreachable: %+v", res)
	}
}

func TestPostNeverFollowsRedirectsWithTheToken(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("the redirect target was contacted with %q", r.Header.Get("Authorization"))
	}))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	err := Poster{URL: redirect.URL, Token: "tok"}.Post(context.Background(), "work", Wire{Kind: KindProject, Project: "p", Name: "P", CreatedAt: "2026-01-01T00:00:00Z"})
	var status *StatusError
	if err == nil || !errors.As(err, &status) || status.Code != http.StatusTemporaryRedirect {
		t.Fatalf("redirect: %v", err)
	}
}

func TestValidIDFollowsHearsaysGrammar(t *testing.T) {
	for id, want := range map[string]bool{"p_ab12": true, "a-b.c/d": true, "": false, "a#b": false, "a:b": false, "a@b": false, strings.Repeat("a", 129): false, strings.Repeat("a", 128): true} {
		if ValidID(id) != want {
			t.Errorf("ValidID(%q) = %v", id, !want)
		}
	}
}

func TestDigestIgnoresOnlyTheUpdateTime(t *testing.T) {
	a := version("x", "2026-01-02T00:00:00Z")
	b := a
	b.UpdatedAt = "2026-02-02T00:00:00Z"
	c := a
	c.Body = "y"
	if a.Digest() != b.Digest() || a.Digest() == c.Digest() {
		t.Fatal("digest does not cover exactly the content")
	}
}

func TestUnpostedKeepsOnlyWhatIsStillToPost(t *testing.T) {
	rec := Record{Source: "owner", Key: "document/d", Tier: TierRecord, Versions: []Wire{version("a", "2026-01-02T00:00:00Z"), version("b", "2026-01-03T00:00:00Z"), version("c", "2026-01-04T00:00:00Z")}}
	if got, ok := Unposted(rec, Posted{}); !ok || len(got.Versions) != 3 {
		t.Fatalf("never posted: %+v %v", got, ok)
	}
	if got, ok := Unposted(rec, Posted{Digest: rec.Versions[0].Digest()}); !ok || len(got.Versions) != 2 || got.Versions[0].Body != "b" {
		t.Fatalf("first posted: %+v %v", got, ok)
	}
	if got, ok := Unposted(rec, Posted{Digest: "unknown"}); !ok || len(got.Versions) != 1 || got.Versions[0].Body != "c" {
		t.Fatalf("an unknown digest posts the current version: %+v %v", got, ok)
	}
	if _, ok := Unposted(rec, Posted{Digest: rec.Current()}); ok {
		t.Fatal("a record posted at its current digest has nothing left")
	}
}
