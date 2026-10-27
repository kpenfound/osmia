package intakejourney

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestDesignUpgradeHoldsUnselectedStartUntilOwnerChooses(t *testing.T) {
	t.Parallel()
	f := newHandInProductionFixture(t)
	ctx := context.Background()
	r, err := f.s.repository(f.project)
	must(t, err)
	lock := intake.DesignLock(r, f.origin)
	lock.Lock()
	must(t, r.CreateWorkstreamOn(ctx, f.origin, config.WorkspacesGit, f.clock.Now(), ownerActor))
	h := intake.OwnerHeader(intake.HandedDocument, f.project, f.origin, intake.DesignStartTransition, f.clock.Now())
	h.Schema = "osmia.trace.document"
	handed := trace.Document{Header: h, Path: intake.DesignBriefPath, Source: "design:" + string(f.origin), Content: productionBrief}
	must(t, r.Append(ctx, handed))
	_, err = r.SetFeatureState(ctx, intake.OwnerHeader(intake.DesignStartTransition, f.project, f.origin, intake.DesignStartTransition, f.clock.Now()), HandedState, "Owner started the design")
	must(t, err)
	lock.Unlock()
	f.stop(t)
	f.start(t)
	var entry InboxEntry
	f.wait(t, "owner chooses projects for legacy start", func() bool {
		inbox, err := f.c.Inbox(ctx)
		must(t, err)
		for _, e := range inbox.Entries {
			if e.Kind == "project-selection" && e.Workstream == f.origin {
				entry = e
				return true
			}
		}
		return false
	})
	for _, project := range []config.ProjectID{f.project, f.project2} {
		r, err := f.s.repository(project)
		must(t, err)
		streams, err := r.Workstreams()
		must(t, err)
		if slices.Contains(streams, f.stream1) || slices.Contains(streams, f.stream2) {
			t.Fatal("legacy start allocated before the owner's answer")
		}
	}
	for _, list := range [][]config.ProjectID{{}, {f.project, f.project}, {"p_0123456789abcdef0123456789abcdef"}} {
		err := f.c.Do(ctx, entry.Answer.Method, entry.Answer.Path, api.ProjectSelectionRequest{Projects: list}, nil)
		if !failed(err, Validation) {
			t.Fatalf("invalid upgrade selection %v: %v", list, err)
		}
	}
	var out api.DesignStartResponse
	must(t, f.c.Do(ctx, entry.Answer.Method, entry.Answer.Path, api.ProjectSelectionRequest{Projects: []config.ProjectID{f.project, f.project2}}, &out))
	if out.Workstream != f.origin {
		t.Fatalf("continued at wrong address: %+v", out)
	}
	f.awaitPacket(t)
	inbox, err := f.c.Inbox(ctx)
	must(t, err)
	if slices.ContainsFunc(inbox.Entries, func(e InboxEntry) bool { return e.Kind == "project-selection" && e.Workstream == f.origin }) {
		t.Fatal("answered project choice stayed open")
	}
	err = f.c.Do(ctx, entry.Answer.Method, entry.Answer.Path, api.ProjectSelectionRequest{Projects: []config.ProjectID{f.project}}, nil)
	if !failed(err, Conflict) {
		t.Fatalf("changed selected start: %v", err)
	}
	r, err = f.s.repository(f.project)
	must(t, err)
	docs, err := trace.Read[trace.Document](r, f.origin)
	must(t, err)
	for _, d := range docs {
		if d.ID == intake.HandedDocument && d.Content != handed.Content {
			t.Fatal("upgrade rewrote the handed text")
		}
	}
}

func TestDesignUpgradeKeepsRecordedSelection(t *testing.T) {
	t.Parallel()
	f := newHandInProductionFixture(t)
	ctx := context.Background()
	f.sharedDesign().RecordSelection(t, f.project, f.project2)
	r, err := f.s.repository(f.project)
	must(t, err)
	lock := intake.DesignLock(r, f.origin)
	lock.Lock()
	h := intake.OwnerHeader(intake.HandedDocument, f.project, f.origin, intake.DesignStartTransition, f.clock.Now())
	h.Schema = "osmia.trace.document"
	must(t, r.Append(ctx, trace.Document{Header: h, Path: intake.DesignBriefPath, Source: "design:" + string(f.origin), Content: productionBrief}))
	_, err = r.SetFeatureState(ctx, intake.OwnerHeader(intake.DesignStartTransition, f.project, f.origin, intake.DesignStartTransition, f.clock.Now()), HandedState, "Owner started the design")
	must(t, err)
	lock.Unlock()
	f.stop(t)
	f.start(t)
	f.awaitPacket(t)
	r, err = f.s.repository(f.project)
	must(t, err)
	selected, found, err := intake.DesignSelectionOf(r, f.origin)
	must(t, err)
	if !found || len(selected.Members) != 2 || selected.Members[0].Workstream != f.stream1 || selected.Members[1].Workstream != f.stream2 {
		t.Fatalf("recorded legacy selection changed: %+v", selected)
	}
}

func TestDesignUpgradeResumesHandedInputBeforeSessionClose(t *testing.T) {
	for _, recorded := range []bool{false, true} {
		name := "owner-answer-interrupted"
		if recorded {
			name = "selection-recorded"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newHandInProductionFixture(t)
			ctx := context.Background()
			r, err := f.s.repository(f.project)
			must(t, err)
			lock := intake.DesignLock(r, f.origin)
			lock.Lock()
			must(t, r.CreateWorkstreamOn(ctx, f.origin, config.WorkspacesGit, f.clock.Now(), ownerActor))
			_, err = r.Transact(ctx, trace.Transaction{Transition: trace.Transition{Header: intake.OwnerHeader(intake.DesignOpenTransition, f.project, f.origin, intake.DesignOpenTransition, f.clock.Now()), Subject: intake.DesignSessionSubject, To: intake.DesignOpenValue, Reason: "Owner opened a design session"}})
			must(t, err)
			h := intake.OwnerHeader(intake.HandedDocument, f.project, f.origin, intake.DesignStartTransition, f.clock.Now())
			h.Schema = "osmia.trace.document"
			must(t, r.Append(ctx, trace.Document{Header: h, Path: intake.DesignBriefPath, Source: "design:" + string(f.origin), Content: productionBrief}))
			if recorded {
				if api := f.s.intake().RecordOwnerProjects(ctx, r, f.origin, []config.ProjectID{f.project, f.project2}, ""); api != nil {
					t.Fatal(api)
				}
			}
			lock.Unlock()
			f.stop(t)
			f.start(t)
			if !recorded {
				var entry InboxEntry
				f.wait(t, "legacy project choice before session close", func() bool {
					inbox, err := f.c.Inbox(ctx)
					must(t, err)
					for _, e := range inbox.Entries {
						if e.Kind == "project-selection" && e.Workstream == f.origin {
							entry = e
							return true
						}
					}
					return false
				})
				hook := &designInterruption{Step: "design-selection-recorded", Nth: 1, Reached: make(chan struct{})}
				f.hook.Store(hook)
				answered := make(chan error, 1)
				go func() {
					answered <- f.c.Do(ctx, entry.Answer.Method, entry.Answer.Path, api.ProjectSelectionRequest{Projects: []config.ProjectID{f.project, f.project2}}, nil)
				}()
				select {
				case <-hook.Reached:
				case <-time.After(demoTimeout):
					f.timeout(t, "interrupted owner selection")
				}
				f.stop(t)
				<-answered
				f.hook.Store(nil)
				f.start(t)
			}
			f.awaitPacket(t)
			r, err = f.s.repository(f.project)
			must(t, err)
			state, err := r.Workflow(f.origin, intake.DesignSessionSubject)
			must(t, err)
			if state.Value != intake.DesignClosedValue {
				t.Fatalf("handed design session remained open: %+v", state)
			}
		})
	}
}
