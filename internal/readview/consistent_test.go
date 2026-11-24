package readview

import (
	"context"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/projectstore"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestConsistentReadRetriesAcrossARacingCommit(t *testing.T) {
	t.Parallel()
	opts := fixturekit.ConfigAt(t, t.TempDir())
	cfg, err := projectstore.LoadConfig(opts)
	fixturekit.Must(t, err)
	repository, err := trace.Create(context.Background(), cfg.Root, cfg.Project, fixturekit.DemoStart, fixturekit.ServiceActor)
	fixturekit.Must(t, err)
	t.Cleanup(func() { repository.Close() })
	fixturekit.Must(t, repository.CreateWorkstream(context.Background(), fixturekit.Stream, fixturekit.DemoStart, fixturekit.ServiceActor))

	const racingStream = config.WorkstreamID("w_1111111111111111111111111111aaaa")
	raced := false
	attempts := 0
	type snapshot struct{ before, after int }
	result, err := Consistent(func() (snapshot, error) {
		attempts++
		before, err := repository.Workstreams()
		if err != nil {
			return snapshot{}, err
		}
		if !raced {
			raced = true
			if err := repository.CreateWorkstream(context.Background(), racingStream, fixturekit.DemoStart, fixturekit.ServiceActor); err != nil {
				return snapshot{}, err
			}
		}
		after, err := repository.Workstreams()
		if err != nil {
			return snapshot{}, err
		}
		return snapshot{len(before), len(after)}, nil
	}, repository.Generation)
	fixturekit.Must(t, err)
	if attempts < 2 {
		t.Fatalf("the racing commit did not force a retry: attempts=%d", attempts)
	}
	if result.before != result.after {
		t.Fatalf("consistentRead returned a torn result: %+v", result)
	}
	if result.before != 2 {
		t.Fatalf("consistentRead did not settle on the latest generation: %+v", result)
	}
}

func TestConsistentReadReturnsWithoutARacingWrite(t *testing.T) {
	t.Parallel()
	opts := fixturekit.ConfigAt(t, t.TempDir())
	cfg, err := projectstore.LoadConfig(opts)
	fixturekit.Must(t, err)
	repository, err := trace.Create(context.Background(), cfg.Root, cfg.Project, fixturekit.DemoStart, fixturekit.ServiceActor)
	fixturekit.Must(t, err)
	t.Cleanup(func() { repository.Close() })

	attempts := 0
	_, err = Consistent(func() (uint64, error) {
		attempts++
		return repository.Generation(), nil
	}, repository.Generation)
	fixturekit.Must(t, err)
	if attempts != 1 {
		t.Fatalf("fn ran %d times with nothing racing it", attempts)
	}
}
