package kb

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"sync"
	"time"

	"github.com/kpenfound/osmia/internal/trace"
)

// ReseedCausePrefix starts the cause of every entity map revision a re-seed
// records; the source commit follows it.
const ReseedCausePrefix = "reseed-"

// ReseedActor records the entity map revisions a re-seed makes.
var ReseedActor = trace.Actor{Kind: "service", ID: "reseed"}

// ReseedCause is the cause of the revision a re-seed from commit records.
func ReseedCause(commit string) string { return ReseedCausePrefix + commit }

// ReseedCommit returns the source commit of a revision's cause, and whether
// a re-seed recorded the revision.
func ReseedCommit(cause string) (string, bool) {
	commit, ok := strings.CutPrefix(cause, ReseedCausePrefix)
	return commit, ok && commit != ""
}

// reseedTimeout bounds one seed computation, which runs apart from the
// callers waiting for it.
const reseedTimeout = 2 * time.Minute

// maxReported bounds the failure reasons a Reseeder remembers.
const maxReported = 128

// Source is the repository a re-seed reads: the files of a commit, and
// which commits come before others.
type Source interface {
	// Tree reads the files of a commit without checking them out.
	Tree(ctx context.Context, commit string) (fs.FS, error)
	// Ancestor reports whether commit is reachable from tip.
	Ancestor(ctx context.Context, commit, tip string) (bool, error)
}

// Ledger is where a re-seed finds the latest recorded entity map and records
// the next one.
type Ledger interface {
	// Latest returns the encoded latest map, and false when none is recorded.
	Latest(ctx context.Context) (content []byte, found bool, err error)
	// Record appends content as the next revision of the map.
	Record(ctx context.Context, content []byte, cause string) error
}

// TraceLedger is the Ledger of the entity map in a project trace.
type TraceLedger struct {
	Repository *trace.Repository
	Actor      trace.Actor
	Now        func() time.Time
}

// Latest reads the latest revision of kb/entities.json with one indexed
// lookup, never a scan of the trace's documents.
func (l TraceLedger) Latest(context.Context) ([]byte, bool, error) {
	d, found, err := l.Repository.LatestProjectDocument(trace.EntitiesDocument, trace.EntitiesPath)
	return []byte(d.Content), found, err
}

// Record appends content as the revision after the latest one.
func (l TraceLedger) Record(ctx context.Context, content []byte, cause string) error {
	d, _, err := l.Repository.LatestProjectDocument(trace.EntitiesDocument, trace.EntitiesPath)
	if err != nil {
		return err
	}
	now := time.Now
	if l.Now != nil {
		now = l.Now
	}
	return l.Repository.Append(ctx, trace.Document{
		Header:  trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: trace.EntitiesDocument, Revision: d.Revision + 1, Project: l.Repository.Project(), At: now(), Actor: l.Actor, Cause: cause},
		Path:    trace.EntitiesPath,
		Content: string(content),
	})
}

// Reseeder regenerates the entity map of one project from the commits its
// callers hand it. It seeds each commit at most once, never seeds a commit
// that comes before the last one it seeded, and records a revision only when
// the seeded map differs from the latest recorded one.
//
// Seeds run one at a time, and callers handing the same commit share the one
// attempt in flight, including its failure. A failure is not remembered as a
// seed, so the next call for the commit tries again.
type Reseeder struct {
	// Log receives each distinct reason a re-seed or a fetch for it failed,
	// once. A nil Log drops them.
	Log func(reason string)

	project sync.Mutex // serialises seeds

	mu       sync.Mutex // guards the rest
	last     string
	flights  map[string]*reseedFlight
	reported map[string]bool
}

type reseedFlight struct {
	done chan struct{}
	err  error
}

// Reseed makes the recorded map the seed of commit. A commit equal to the
// last one seeded returns at once, touching neither src nor ledger. An
// ancestor of the last one seeded is not seeded. An error is the failure of
// the attempt, shared by every caller waiting on it, and is reported to Log
// once per distinct reason.
func (r *Reseeder) Reseed(ctx context.Context, commit string, src Source, ledger Ledger) error {
	if commit == "" {
		return errors.New("re-seed has no commit")
	}
	r.mu.Lock()
	if r.last == commit {
		r.mu.Unlock()
		return nil
	}
	flight, running := r.flights[commit]
	if !running {
		flight = &reseedFlight{done: make(chan struct{})}
		if r.flights == nil {
			r.flights = map[string]*reseedFlight{}
		}
		r.flights[commit] = flight
	}
	r.mu.Unlock()
	if !running {
		// The attempt belongs to every caller waiting on it, not to the one
		// that started it, so its cancellation does not fail the others.
		go r.run(ctx, flight, commit, src, ledger)
	}
	select {
	case <-flight.done:
		return flight.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *Reseeder) run(ctx context.Context, flight *reseedFlight, commit string, src Source, ledger Ledger) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), reseedTimeout)
	defer cancel()
	r.project.Lock()
	seeded, err := r.seed(ctx, commit, src, ledger)
	r.project.Unlock()
	if err != nil {
		err = fmt.Errorf("re-seed of the entity map from %s: %w", commit, err)
		r.Warn(err.Error())
	}
	r.mu.Lock()
	if err == nil && seeded {
		r.last = commit
	}
	delete(r.flights, commit)
	r.mu.Unlock()
	flight.err = err
	close(flight.done)
}

// seed computes and records the map of commit. It reports false, with no
// error, for a commit that comes before the last one seeded.
func (r *Reseeder) seed(ctx context.Context, commit string, src Source, ledger Ledger) (bool, error) {
	r.mu.Lock()
	last := r.last
	r.mu.Unlock()
	if last == commit {
		return true, nil
	}
	if last != "" {
		before, err := src.Ancestor(ctx, commit, last)
		if err != nil {
			return false, err
		}
		if before {
			return false, nil
		}
	}
	tree, err := src.Tree(ctx, commit)
	if err != nil {
		return false, err
	}
	seeded, err := SeedFS(tree)
	if err != nil {
		return false, err
	}
	data, err := Encode(seeded)
	if err != nil {
		return false, err
	}
	recorded, found, err := ledger.Latest(ctx)
	if err != nil {
		return false, err
	}
	if found && bytes.Equal(recorded, data) {
		return true, nil
	}
	return true, ledger.Record(ctx, data, ReseedCause(commit))
}

// Warn reports a reason a re-seed or the fetch for it failed to Log, once for
// each distinct reason.
func (r *Reseeder) Warn(reason string) {
	r.mu.Lock()
	if r.reported[reason] {
		r.mu.Unlock()
		return
	}
	if r.reported == nil || len(r.reported) >= maxReported {
		r.reported = map[string]bool{}
	}
	r.reported[reason] = true
	r.mu.Unlock()
	if r.Log != nil {
		r.Log(reason)
	}
}
