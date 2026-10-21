package service

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/trace"
)

func TestChangeApplicationRechecksRetirementAfterObserverRead(t *testing.T) {
	for _, retire := range []bool{true, false} {
		name := "retired"
		if !retire {
			name = "live"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			gate := &changeRaceGate{Step: "change-before-application-lock", Reached: make(chan struct{}), Released: make(chan struct{})}
			t.Cleanup(gate.Release)
			f := resolutionApprovedFixture(t, productionDesignConfiguration{wait: lifecycleAwait, checkpoint: gate.Checkpoint}, trace.EdgeContract, nil)
			defer f.stop(t)
			deliverResolutionMembers(t, f)
			for _, m := range resolutionMembers(f) {
				repo, err := f.s.repository(m.project)
				must(t, err)
				recordResolutionMerge(t, repo, m.stream)
			}
			awaitMemberMergeResolution(t, f)

			change, api := f.s.requestChange(ctx, string(f.origin), ChangeRequest{Key: "retirement-observer", Kind: "cancel", Scope: "workgroup", Change: "Keep the approved decision held when every member retires."})
			if api != nil {
				t.Fatal(api)
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
			decided, api := f.s.decideChange(ctx, string(f.origin), change.ID, ChangeDecision{Revision: change.Revision, Decision: "approve", PullRequests: "draft"})
			if api != nil {
				t.Fatal(api)
			}
			if decided.State != "approved" || decided.Decision == nil {
				t.Fatal("owner approval was not recorded")
			}
			gate.Await(t)
			// The observer holds group coordination after reading the approved
			// journal, but has not attempted the archive/application guard.
			coordination := f.s.groupApprovalLock(change.Workgroup)
			if coordination.TryLock() {
				coordination.Unlock()
				t.Fatal("observer did not retain its locked journal read")
			}
			wt, err := trace.OpenWorkgroupAuthority(f.s.current().Root, change.Workgroup)
			must(t, err)
			defer wt.Close()
			journal := func() []byte {
				docs, err := trace.Read[trace.Document](wt, change.Host)
				must(t, err)
				docs = slices.DeleteFunc(docs, func(doc trace.Document) bool {
					return !strings.HasPrefix(doc.ID, "workgroup-change-")
				})
				data, err := json.Marshal(docs)
				must(t, err)
				return data
			}
			before := journal()
			if retire {
				for _, m := range resolutionMembers(f) {
					_, err := f.c.Archive(ctx, m.stream)
					must(t, err)
				}
			}
			unlock()
			gate.Release()
			// Joining this lock establishes that the held observer returned;
			// an unchanged journal alone cannot prove it ran after retirement.
			finished := make(chan struct{})
			go func() {
				coordination.Lock()
				close(finished)
				coordination.Unlock()
			}()
			select {
			case <-finished:
			case <-time.After(demoTimeout):
				t.Fatal("held change observer did not finish")
			}
			if retire {
				if !bytes.Equal(before, journal()) {
					t.Fatal("retired observer changed the journal bytes or revision history")
				}
				changes, err := readChanges(wt, change.Host)
				must(t, err)
				if len(changes) != 1 || !equalJSONTest(changes[0], decided) {
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
