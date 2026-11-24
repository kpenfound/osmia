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
			gate := &intakefixture.ChangeRaceGate{Step: "change-before-application-lock", Reached: make(chan struct{}), Released: make(chan struct{})}
			t.Cleanup(gate.Release)
			f := resolutionApprovedFixture(t, productionDesignConfiguration{wait: lifecycleAwait, checkpoint: gate.Checkpoint}, trace.EdgeContract, nil)
			defer f.stop(t)
			deliverResolutionMembers(t, f)
			for _, m := range resolutionMembers(f) {
				repo, err := f.s.repository(m.project)
				intakefixture.Must(t, err)
				recordResolutionMerge(t, repo, m.stream)
			}
			awaitMemberMergeResolution(t, f)

			change, apiFault := f.s.requestChange(ctx, string(f.origin), lifecycle.ChangeRequest{Key: "retirement-observer", Kind: "cancel", Scope: "workgroup", Change: "Keep the approved decision held when every member retires."})
			if apiFault != nil {
				t.Fatal(apiFault)
			}
			lifecycleAwait(t, f, "presented decision", func() bool {
				change = currentChange(t, f.s, f.origin, change.ID)
				return change.State == "presented"
			})
			lock := f.s.changeApplicationLock(change.Workgroup)
			lock.RLock()
			unlock := sync.OnceFunc(lock.RUnlock)
			defer unlock()
			gate.Armed.Store(true)
			decided, apiFault := f.s.decideChange(ctx, string(f.origin), change.ID, lifecycle.ChangeDecision{Revision: change.Revision, Decision: "approve", PullRequests: "draft"})
			if apiFault != nil {
				t.Fatal(apiFault)
			}
			if decided.State != "approved" || decided.Decision == nil {
				t.Fatal("owner approval was not recorded")
			}
			gate.Await(t)
			coordination := f.s.groupApprovalLock(change.Workgroup)
			if coordination.TryLock() {
				coordination.Unlock()
				t.Fatal("observer did not retain its locked journal read")
			}
			wt, err := trace.OpenWorkgroupAuthority(f.s.current().Root, change.Workgroup)
			intakefixture.Must(t, err)
			defer wt.Close()
			journal := func() []byte {
				docs, err := trace.Read[trace.Document](wt, change.Host)
				intakefixture.Must(t, err)
				docs = slices.DeleteFunc(docs, func(doc trace.Document) bool {
					return !strings.HasPrefix(doc.ID, "workgroup-change-")
				})
				data, err := json.Marshal(docs)
				intakefixture.Must(t, err)
				return data
			}
			before := journal()
			if retire {
				for _, m := range resolutionMembers(f) {
					_, err := f.c.Archive(ctx, m.stream)
					intakefixture.Must(t, err)
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
			case <-time.After(intakefixture.DemoTimeout):
				t.Fatal("held change observer did not finish")
			}
			if retire {
				if !bytes.Equal(before, journal()) {
					t.Fatal("retired observer changed the journal bytes or revision history")
				}
				changes, err := lifecycle.ReadChanges(wt, change.Host)
				intakefixture.Must(t, err)
				if len(changes) != 1 || !intakefixture.EqualJSONTest(changes[0], decided) {
					t.Fatal("retired observer changed the approved decision or its exact pins")
				}
			} else {
				lifecycleAwait(t, f, "live group applies through the same gate", func() bool {
					return currentChange(t, f.s, f.origin, change.ID).State == "applied"
				})
				if bytes.Equal(before, journal()) {
					t.Fatal("live observer did not record application")
				}
			}
		})
	}
}
