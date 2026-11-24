package lifecycle_test

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestChangeApplicationRechecksRetirementAfterObserverRead(t *testing.T) {
	t.Parallel()
	for _, retire := range []bool{true, false} {
		name := "retired"
		if !retire {
			name = "live"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			gate := &intakefixture.ChangeRaceGate{Step: "change-before-application-lock", Reached: make(chan struct{})}
			t.Cleanup(gate.Release)
			f := resolutionApprovedFixture(t, journey.ProductionDesignConfiguration{Wait: journey.LifecycleAwait, Checkpoint: gate.Checkpoint}, trace.EdgeContract, nil)
			defer f.Stop(t)
			deliverResolutionMembers(t, f)
			for _, m := range resolutionMembers(f) {
				repo, err := f.S.Repository(m.project)
				fixturekit.Must(t, err)
				journey.RecordResolutionMerge(t, repo, m.stream)
			}
			awaitMemberMergeResolution(t, f)

			change, apiFault := f.S.Lifecycle().RequestChange(ctx, string(f.Origin), lifecycle.ChangeRequest{Key: "retirement-observer", Kind: "cancel", Scope: "workgroup", Change: "Keep the approved decision held when every member retires."})
			if apiFault != nil {
				t.Fatal(apiFault)
			}
			journey.LifecycleAwait(t, f, "presented decision", func() bool {
				change = journey.CurrentChange(t, f.S, f.Origin, change.ID)
				return change.State == "presented"
			})
			lock := f.S.Lifecycle().ChangeApplicationLock(change.Workgroup)
			lock.RLock()
			unlock := sync.OnceFunc(lock.RUnlock)
			defer unlock()
			gate.Armed.Store(true)
			decided, apiFault := f.S.Lifecycle().DecideChange(ctx, string(f.Origin), change.ID, lifecycle.ChangeDecision{Revision: change.Revision, Decision: "approve", PullRequests: "draft"})
			if apiFault != nil {
				t.Fatal(apiFault)
			}
			if decided.State != "approved" || decided.Decision == nil {
				t.Fatal("owner approval was not recorded")
			}
			gate.Await(t)
			coordination := journey.InspectForTest(f.S).GroupApprovalLock(change.Workgroup)
			if coordination.TryLock() {
				coordination.Unlock()
				t.Fatal("observer did not retain its locked journal read")
			}
			wt, err := trace.OpenWorkgroupAuthority(f.S.Current().Root, change.Workgroup)
			fixturekit.Must(t, err)
			defer wt.Close()
			journal := func() []byte {
				docs, err := trace.Read[trace.Document](wt, change.Host)
				fixturekit.Must(t, err)
				docs = slices.DeleteFunc(docs, func(doc trace.Document) bool {
					return !strings.HasPrefix(doc.ID, "workgroup-change-")
				})
				data, err := json.Marshal(docs)
				fixturekit.Must(t, err)
				return data
			}
			before := journal()
			if retire {
				for _, m := range resolutionMembers(f) {
					_, err := f.C.Archive(ctx, m.stream)
					fixturekit.Must(t, err)
				}
			}
			unlock()
			gate.Release()
			finished := make(chan struct{})
			go func() {
				coordination.Lock()
				close(finished)
				coordination.Unlock()
			}()
			select {
			case <-finished:
			case <-time.After(fixturekit.DemoTimeout):
				t.Fatal("held change observer did not finish")
			}
			if retire {
				if !bytes.Equal(before, journal()) {
					t.Fatal("retired observer changed the journal bytes or revision history")
				}
				changes, err := lifecycle.ReadChanges(wt, change.Host)
				fixturekit.Must(t, err)
				if len(changes) != 1 || !intakefixture.EqualJSONTest(changes[0], decided) {
					t.Fatal("retired observer changed the approved decision or its exact pins")
				}
			} else {
				journey.LifecycleAwait(t, f, "live group applies through the same gate", func() bool {
					return journey.CurrentChange(t, f.S, f.Origin, change.ID).State == "applied"
				})
				if bytes.Equal(before, journal()) {
					t.Fatal("live observer did not record application")
				}
			}
		})
	}
}
