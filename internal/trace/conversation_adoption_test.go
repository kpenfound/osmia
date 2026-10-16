package trace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
)

func TestPrepareDesignWorkgroupFailureAndRetry(t *testing.T) {
	t.Parallel()
	for _, failure := range []string{"create-cancelled", "host-cancelled", "open-damaged", "directory-file"} {
		t.Run(failure, func(t *testing.T) {
			t.Parallel()
			root, r1, r2, _, _ := crossRepoFixture(t)
			id, err := config.NewWorkgroupID()
			if err != nil {
				t.Fatal(err)
			}
			host, err := config.NewWorkstreamID(crossMember1, crossMember2)
			if err != nil {
				t.Fatal(err)
			}
			origin := ConversationOrigin{Project: crossProject1, Workstream: host}
			members := []CrossRepoMember{{crossMember1, r1}, {crossMember2, r2}}
			directory, err := root.WorkgroupTrace(id)
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			var restore []byte
			if failure == "host-cancelled" || failure == "open-damaged" {
				// The committed group directory survives an interruption before
				// its host and membership record are published.
				r, err := createWorkgroupTraceDirectory(ctx, root, id, at, owner, "design-start")
				if err != nil {
					t.Fatal(err)
				}
				if err := r.Close(); err != nil {
					t.Fatal(err)
				}
				if failure == "open-damaged" {
					restore, err = os.ReadFile(filepath.Join(directory, "project.json"))
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(directory, "project.json"), []byte("{not json}"), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			if failure == "directory-file" {
				if err := os.MkdirAll(filepath.Dir(directory), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(directory, []byte("blocked"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if failure == "create-cancelled" || failure == "host-cancelled" {
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelled
			}
			prepare := func(ctx context.Context) (*Repository, Workgroup, error) {
				t.Helper()
				type result struct {
					repo  *Repository
					group Workgroup
					err   error
				}
				done := make(chan result, 1)
				go func() {
					r, g, err := PrepareDesignWorkgroup(ctx, root, id, origin, members, at, owner)
					done <- result{r, g, err}
				}()
				select {
				case got := <-done:
					return got.repo, got.group, got.err
				case <-time.After(5 * time.Second):
					t.Fatal("preparation or retry blocked on a leaked workgroup handle")
					return nil, Workgroup{}, nil
				}
			}
			r, g, err := prepare(ctx)
			if err == nil || r != nil || !reflect.DeepEqual(g, Workgroup{}) {
				t.Fatalf("failed preparation returned usable state: %v %+v %v", r, g, err)
			}
			if (failure == "create-cancelled" || failure == "host-cancelled") && !errors.Is(err, context.Canceled) {
				t.Fatalf("creation cancellation was lost: %v", err)
			}
			shared := sharedWorkgroupTraceFor(id)
			shared.Lock()
			leaked := shared.repo != nil || shared.refs != 0
			shared.Unlock()
			if leaked {
				t.Fatal("failed preparation retained a shared handle")
			}
			if restore != nil {
				// Repair the damaged bytes without discarding committed history.
				if err := os.WriteFile(filepath.Join(directory, "project.json"), restore, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if failure == "directory-file" {
				if err := os.Remove(directory); err != nil {
					t.Fatal(err)
				}
			}
			r, g, err = prepare(context.Background())
			if err != nil {
				t.Fatalf("retry failed: %v", err)
			}
			t.Cleanup(func() { r.Close() })
			retried, again, err := prepare(context.Background())
			if err != nil {
				t.Fatalf("idempotent preparation: %v", err)
			}
			if err := retried.Close(); err != nil {
				t.Fatal(err)
			}
			groups, revision, err := Workgroups(r)
			if err != nil || revision != 1 || len(groups) != 1 || !reflect.DeepEqual(groups[0], g) || !reflect.DeepEqual(again, g) {
				t.Fatalf("retry duplicated membership: %+v revision=%d err=%v", groups, revision, err)
			}
			if _, err := r.Thread(host, ChiefOfStaff); err != nil {
				t.Fatalf("retry did not reconcile the host: %v", err)
			}
		})
	}
}
