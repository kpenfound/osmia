package kb

import (
	"context"
	"errors"
	"io/fs"
	"reflect"
	"sync"
	"testing"
	"testing/fstest"
	"time"
)

// fakeSource serves trees by commit and counts the reads made of it.
type fakeSource struct {
	mu        sync.Mutex
	trees     map[string]fs.FS
	ancestors map[[2]string]bool
	treeErr   error
	// hold, when set, blocks every Tree until it is closed.
	hold chan struct{}

	treeReads, ancestorReads int
}

func (s *fakeSource) Tree(_ context.Context, commit string) (fs.FS, error) {
	s.mu.Lock()
	s.treeReads++
	hold, err, tree := s.hold, s.treeErr, s.trees[commit]
	s.mu.Unlock()
	if hold != nil {
		<-hold
	}
	if err != nil {
		return nil, err
	}
	if tree == nil {
		return nil, errors.New("unknown commit " + commit)
	}
	return tree, nil
}

func (s *fakeSource) Ancestor(_ context.Context, commit, tip string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ancestorReads++
	return s.ancestors[[2]string{commit, tip}], nil
}

func (s *fakeSource) reads() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.treeReads, s.ancestorReads
}

func (s *fakeSource) failWith(err error) {
	s.mu.Lock()
	s.treeErr = err
	s.mu.Unlock()
}

// fakeLedger keeps the recorded revisions in memory and counts its reads.
type fakeLedger struct {
	mu       sync.Mutex
	revision [][]byte
	causes   []string
	probes   int
}

func (l *fakeLedger) Latest(context.Context) ([]byte, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.probes++
	if len(l.revision) == 0 {
		return nil, false, nil
	}
	return l.revision[len(l.revision)-1], true, nil
}

func (l *fakeLedger) Record(_ context.Context, content []byte, cause string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.revision = append(l.revision, content)
	l.causes = append(l.causes, cause)
	return nil
}

func (l *fakeLedger) counts() (probes, revisions int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.probes, len(l.revision)
}

func dirs(names ...string) fs.FS {
	tree := fstest.MapFS{}
	for _, name := range names {
		tree[name+"/file"] = &fstest.MapFile{Data: []byte("x")}
	}
	return tree
}

func paths(t *testing.T, content []byte) []string {
	t.Helper()
	m, err := Parse(content)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range m.Entities {
		out = append(out, e.Primary())
	}
	return out
}

func TestReseedSeedsAnUpstreamHeadOnceForEveryTrigger(t *testing.T) {
	src := &fakeSource{trees: map[string]fs.FS{"c1": dirs("api", "web")}, hold: make(chan struct{})}
	ledger := &fakeLedger{}
	var reseeder Reseeder
	ctx := context.Background()

	const workstreams = 8
	var wg, started sync.WaitGroup
	errs := make(chan error, workstreams)
	for range workstreams {
		wg.Add(1)
		started.Add(1)
		go func() {
			defer wg.Done()
			started.Done()
			errs <- reseeder.Reseed(ctx, "c1", src, ledger)
		}()
	}
	started.Wait()
	time.Sleep(50 * time.Millisecond)
	close(src.hold)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	for range workstreams {
		if err := reseeder.Reseed(ctx, "c1", src, ledger); err != nil {
			t.Fatal(err)
		}
	}

	if trees, _ := src.reads(); trees != 1 {
		t.Fatalf("%d seed computations for one head, want 1", trees)
	}
	if _, revisions := ledger.counts(); revisions != 1 || ledger.causes[0] != "reseed-c1" {
		t.Fatalf("%d revisions with causes %v, want one caused by reseed-c1", revisions, ledger.causes)
	}
	if got := paths(t, ledger.revision[0]); !reflect.DeepEqual(got, []string{"api", "**", "web"}) {
		t.Fatalf("recorded map has %v", got)
	}
}

func TestReseedOfTheLastSeededCommitReadsAndWritesNothing(t *testing.T) {
	src := &fakeSource{trees: map[string]fs.FS{"c1": dirs("api")}}
	ledger := &fakeLedger{}
	var reseeder Reseeder
	ctx := context.Background()
	if err := reseeder.Reseed(ctx, "c1", src, ledger); err != nil {
		t.Fatal(err)
	}
	trees, ancestors := src.reads()
	probes, revisions := ledger.counts()

	for range 3 {
		if err := reseeder.Reseed(ctx, "c1", src, ledger); err != nil {
			t.Fatal(err)
		}
	}
	if t2, a2 := src.reads(); t2 != trees || a2 != ancestors {
		t.Fatalf("git reads went from %d/%d to %d/%d", trees, ancestors, t2, a2)
	}
	if p2, r2 := ledger.counts(); p2 != probes || r2 != revisions {
		t.Fatalf("trace reads/writes went from %d/%d to %d/%d", probes, revisions, p2, r2)
	}
}

func TestReseedNeverSeedsAnAncestorOfTheLastSeededCommit(t *testing.T) {
	src := &fakeSource{
		trees:     map[string]fs.FS{"old": dirs("api"), "new": dirs("api", "web")},
		ancestors: map[[2]string]bool{{"old", "new"}: true},
	}
	ledger := &fakeLedger{}
	var reseeder Reseeder
	ctx := context.Background()
	if err := reseeder.Reseed(ctx, "new", src, ledger); err != nil {
		t.Fatal(err)
	}
	trees, _ := src.reads()
	for range 2 {
		if err := reseeder.Reseed(ctx, "old", src, ledger); err != nil {
			t.Fatal(err)
		}
	}
	if t2, _ := src.reads(); t2 != trees {
		t.Fatalf("the ancestor was read: %d tree reads, want %d", t2, trees)
	}
	if _, revisions := ledger.counts(); revisions != 1 {
		t.Fatalf("%d revisions, want the one seeded from the newer commit", revisions)
	}
	if got := paths(t, ledger.revision[0]); !reflect.DeepEqual(got, []string{"api", "**", "web"}) {
		t.Fatalf("map has %v", got)
	}
	// A commit that is not an ancestor moves the map forward.
	src.trees["next"] = dirs("api")
	if err := reseeder.Reseed(ctx, "next", src, ledger); err != nil {
		t.Fatal(err)
	}
	if _, revisions := ledger.counts(); revisions != 2 {
		t.Fatalf("%d revisions, want 2", revisions)
	}
}

func TestReseedRecordsNothingWhenTheBytesAreUnchanged(t *testing.T) {
	seeded, err := SeedFS(dirs("api", "web"))
	if err != nil {
		t.Fatal(err)
	}
	data, err := Encode(seeded)
	if err != nil {
		t.Fatal(err)
	}
	src := &fakeSource{trees: map[string]fs.FS{"c1": dirs("api", "web"), "c2": dirs("api", "web")}}
	ledger := &fakeLedger{revision: [][]byte{data}, causes: []string{"registration"}}
	var reseeder Reseeder
	ctx := context.Background()
	for _, commit := range []string{"c1", "c2"} {
		if err := reseeder.Reseed(ctx, commit, src, ledger); err != nil {
			t.Fatal(err)
		}
	}
	// Each new commit costs one probe of the latest revision and no write.
	if probes, revisions := ledger.counts(); probes != 2 || revisions != 1 {
		t.Fatalf("%d probes and %d revisions, want 2 and 1", probes, revisions)
	}
}

func TestReseedIsPure(t *testing.T) {
	src := &fakeSource{trees: map[string]fs.FS{"c1": dirs("api", "web"), "c2": dirs("api")}}
	ledger := &fakeLedger{}
	var reseeder Reseeder
	ctx := context.Background()
	for _, commit := range []string{"c1", "c2"} {
		if err := reseeder.Reseed(ctx, commit, src, ledger); err != nil {
			t.Fatal(err)
		}
	}
	if got := paths(t, ledger.revision[1]); !reflect.DeepEqual(got, []string{"api", "**"}) {
		t.Fatalf("the entity of the removed directory stayed: %v", got)
	}
}

func TestReseedFailureIsSharedLoggedOnceAndRetried(t *testing.T) {
	boom := errors.New("tree unreadable")
	src := &fakeSource{trees: map[string]fs.FS{"c1": dirs("api")}, hold: make(chan struct{})}
	src.failWith(boom)
	ledger := &fakeLedger{}
	var logged []string
	var mu sync.Mutex
	reseeder := Reseeder{Log: func(reason string) { mu.Lock(); logged = append(logged, reason); mu.Unlock() }}
	ctx := context.Background()

	const callers = 6
	var wg, started sync.WaitGroup
	errs := make(chan error, callers)
	for range callers {
		wg.Add(1)
		started.Add(1)
		go func() {
			defer wg.Done()
			started.Done()
			errs <- reseeder.Reseed(ctx, "c1", src, ledger)
		}()
	}
	started.Wait()
	time.Sleep(50 * time.Millisecond)
	close(src.hold)
	wg.Wait()
	close(errs)
	for err := range errs {
		if !errors.Is(err, boom) {
			t.Fatalf("a caller sharing the failed attempt got %v", err)
		}
	}
	if trees, _ := src.reads(); trees != 1 {
		t.Fatalf("%d attempts for callers sharing one, want 1", trees)
	}

	// Later triggers at the same commit retry, and the same reason is logged once.
	for range 3 {
		if err := reseeder.Reseed(ctx, "c1", src, ledger); !errors.Is(err, boom) {
			t.Fatalf("retry: %v", err)
		}
	}
	if trees, _ := src.reads(); trees != 4 {
		t.Fatalf("%d attempts after three later triggers, want 4", trees)
	}
	mu.Lock()
	count := len(logged)
	mu.Unlock()
	if count != 1 {
		t.Fatalf("logged %d times for one reason: %v", count, logged)
	}
	if _, revisions := ledger.counts(); revisions != 0 {
		t.Fatalf("a failed attempt recorded %d revisions", revisions)
	}

	// Once the read works, the same commit records its map.
	src.failWith(nil)
	if err := reseeder.Reseed(ctx, "c1", src, ledger); err != nil {
		t.Fatal(err)
	}
	if _, revisions := ledger.counts(); revisions != 1 {
		t.Fatalf("%d revisions after the retry, want 1", revisions)
	}
}

func TestWarnLogsEachDistinctReasonOnce(t *testing.T) {
	var logged []string
	reseeder := Reseeder{Log: func(reason string) { logged = append(logged, reason) }}
	for _, reason := range []string{"a", "b", "a", "a", "b", "c"} {
		reseeder.Warn(reason)
	}
	if !reflect.DeepEqual(logged, []string{"a", "b", "c"}) {
		t.Fatalf("logged %v", logged)
	}
}

func TestReseedCause(t *testing.T) {
	if commit, ok := ReseedCommit(ReseedCause("abc123")); !ok || commit != "abc123" {
		t.Fatalf("got %q, %v", commit, ok)
	}
	for _, cause := range []string{"", "reseed-", "extract-1", "project-create"} {
		if _, ok := ReseedCommit(cause); ok {
			t.Errorf("%q reads as a re-seed", cause)
		}
	}
}

func TestReseedRecordsRevisionsInTheTrace(t *testing.T) {
	r, _, _ := traceFixture(t, nil)
	ledger := TraceLedger{Repository: r, Actor: ReseedActor, Now: func() time.Time { return at }}
	src := &fakeSource{trees: map[string]fs.FS{"c1": dirs("api"), "c2": dirs("api"), "c3": dirs("api", "web")}}
	var reseeder Reseeder
	ctx := context.Background()
	for _, commit := range []string{"c1", "c2", "c3"} {
		if err := reseeder.Reseed(ctx, commit, src, ledger); err != nil {
			t.Fatal(err)
		}
	}
	docs, err := entityRevisions(r)
	if err != nil {
		t.Fatal(err)
	}
	// The map of c1 and the map of c3; c2 changed nothing.
	if len(docs) != 2 || docs[0].Cause != "reseed-c1" || docs[1].Cause != "reseed-c3" || docs[1].Revision != 2 || docs[1].Actor != ReseedActor {
		t.Fatalf("revisions: %+v", docs)
	}
	m, revision, err := LoadRevision(r)
	if err != nil || revision != 2 || len(m.Entities) != 3 {
		t.Fatalf("loaded revision %d: %+v, %v", revision, m, err)
	}
}
