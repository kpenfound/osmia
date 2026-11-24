package intakejourney

import (
	"context"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"slices"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestUnselectedHandedDesignStartWaitsForTheOwnersProjectChoice(t *testing.T) {
	t.Parallel()
	f := newHandInProductionFixture(t)
	ctx := context.Background()
	r, err := f.S.Repository(f.Project)
	fixturekit.Must(t, err)
	lock := intake.DesignLock(r, f.Origin)
	lock.Lock()
	fixturekit.Must(t, r.CreateWorkstreamOn(ctx, f.Origin, config.WorkspacesGit, f.Clock.Now(), intake.OwnerActor))
	h := intake.OwnerHeader(intake.HandedDocument, f.Project, f.Origin, intake.DesignStartTransition, f.Clock.Now())
	h.Schema = "osmia.trace.document"
	handed := trace.Document{Header: h, Path: intake.DesignBriefPath, Source: "design:" + string(f.Origin), Content: intakefixture.ProductionBrief}
	fixturekit.Must(t, r.Append(ctx, handed))
	_, err = r.SetFeatureState(ctx, intake.OwnerHeader(intake.DesignStartTransition, f.Project, f.Origin, intake.DesignStartTransition, f.Clock.Now()), intake.HandedState, "Owner started the design")
	fixturekit.Must(t, err)
	lock.Unlock()
	f.Stop(t)
	f.Start(t)
	var entry api.InboxEntry
	f.Wait(t, "owner chooses projects for unselected start", func() bool {
		inbox, err := f.C.Inbox(ctx)
		fixturekit.Must(t, err)
		for _, e := range inbox.Entries {
			if e.Kind == "project-selection" && e.Workstream == f.Origin {
				entry = e
				return true
			}
		}
		return false
	})
	for _, project := range []config.ProjectID{f.Project, f.Project2} {
		r, err := f.S.Repository(project)
		fixturekit.Must(t, err)
		streams, err := r.Workstreams()
		fixturekit.Must(t, err)
		if slices.Contains(streams, f.Stream1) || slices.Contains(streams, f.Stream2) {
			t.Fatal("unselected start allocated before the owner's answer")
		}
	}
	for _, list := range [][]config.ProjectID{{}, {f.Project, f.Project}, {"p_0123456789abcdef0123456789abcdef"}} {
		err := f.C.Do(ctx, entry.Answer.Method, entry.Answer.Path, api.ProjectSelectionRequest{Projects: list}, nil)
		if !intakefixture.Failed(err, api.Validation) {
			t.Fatalf("invalid project selection %v: %v", list, err)
		}
	}
	var out api.DesignStartResponse
	fixturekit.Must(t, f.C.Do(ctx, entry.Answer.Method, entry.Answer.Path, api.ProjectSelectionRequest{Projects: []config.ProjectID{f.Project, f.Project2}}, &out))
	if out.Workstream != f.Origin {
		t.Fatalf("continued at wrong address: %+v", out)
	}
	f.AwaitPacket(t)
	inbox, err := f.C.Inbox(ctx)
	fixturekit.Must(t, err)
	if slices.ContainsFunc(inbox.Entries, func(e api.InboxEntry) bool { return e.Kind == "project-selection" && e.Workstream == f.Origin }) {
		t.Fatal("answered project choice stayed open")
	}
	err = f.C.Do(ctx, entry.Answer.Method, entry.Answer.Path, api.ProjectSelectionRequest{Projects: []config.ProjectID{f.Project}}, nil)
	if !intakefixture.Failed(err, api.Conflict) {
		t.Fatalf("changed selected start: %v", err)
	}
	r, err = f.S.Repository(f.Project)
	fixturekit.Must(t, err)
	docs, err := trace.Read[trace.Document](r, f.Origin)
	fixturekit.Must(t, err)
	for _, d := range docs {
		if d.ID == intake.HandedDocument && d.Content != handed.Content {
			t.Fatal("the selection rewrote the handed text")
		}
	}
}

func TestRecordedProjectSelectionIsKeptWhenAHandedDesignStartResumes(t *testing.T) {
	t.Parallel()
	f := newHandInProductionFixture(t)
	ctx := context.Background()
	f.SharedDesign().RecordSelection(t, f.Project, f.Project2)
	r, err := f.S.Repository(f.Project)
	fixturekit.Must(t, err)
	lock := intake.DesignLock(r, f.Origin)
	lock.Lock()
	h := intake.OwnerHeader(intake.HandedDocument, f.Project, f.Origin, intake.DesignStartTransition, f.Clock.Now())
	h.Schema = "osmia.trace.document"
	fixturekit.Must(t, r.Append(ctx, trace.Document{Header: h, Path: intake.DesignBriefPath, Source: "design:" + string(f.Origin), Content: intakefixture.ProductionBrief}))
	_, err = r.SetFeatureState(ctx, intake.OwnerHeader(intake.DesignStartTransition, f.Project, f.Origin, intake.DesignStartTransition, f.Clock.Now()), intake.HandedState, "Owner started the design")
	fixturekit.Must(t, err)
	lock.Unlock()
	f.Stop(t)
	f.Start(t)
	f.AwaitPacket(t)
	r, err = f.S.Repository(f.Project)
	fixturekit.Must(t, err)
	selected, found, err := intake.DesignSelectionOf(r, f.Origin)
	fixturekit.Must(t, err)
	if !found || len(selected.Members) != 2 || selected.Members[0].Workstream != f.Stream1 || selected.Members[1].Workstream != f.Stream2 {
		t.Fatalf("recorded selection changed: %+v", selected)
	}
}

func TestHandedDesignInputResumesBeforeTheSessionCloses(t *testing.T) {
	for _, recorded := range []bool{false, true} {
		name := "owner-answer-interrupted"
		if recorded {
			name = "selection-recorded"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newHandInProductionFixture(t)
			ctx := context.Background()
			r, err := f.S.Repository(f.Project)
			fixturekit.Must(t, err)
			lock := intake.DesignLock(r, f.Origin)
			lock.Lock()
			fixturekit.Must(t, r.CreateWorkstreamOn(ctx, f.Origin, config.WorkspacesGit, f.Clock.Now(), intake.OwnerActor))
			_, err = r.Transact(ctx, trace.Transaction{Transition: trace.Transition{Header: intake.OwnerHeader(intake.DesignOpenTransition, f.Project, f.Origin, intake.DesignOpenTransition, f.Clock.Now()), Subject: intake.DesignSessionSubject, To: intake.DesignOpenValue, Reason: "Owner opened a design session"}})
			fixturekit.Must(t, err)
			h := intake.OwnerHeader(intake.HandedDocument, f.Project, f.Origin, intake.DesignStartTransition, f.Clock.Now())
			h.Schema = "osmia.trace.document"
			fixturekit.Must(t, r.Append(ctx, trace.Document{Header: h, Path: intake.DesignBriefPath, Source: "design:" + string(f.Origin), Content: intakefixture.ProductionBrief}))
			if recorded {
				if apiErr := f.S.Intake().RecordOwnerProjects(ctx, r, f.Origin, []config.ProjectID{f.Project, f.Project2}, ""); apiErr != nil {
					t.Fatal(apiErr)
				}
			}
			lock.Unlock()
			f.Stop(t)
			f.Start(t)
			if !recorded {
				var entry api.InboxEntry
				f.Wait(t, "unselected project choice before session close", func() bool {
					inbox, err := f.C.Inbox(ctx)
					fixturekit.Must(t, err)
					for _, e := range inbox.Entries {
						if e.Kind == "project-selection" && e.Workstream == f.Origin {
							entry = e
							return true
						}
					}
					return false
				})
				hook := &intakefixture.DesignInterruption{Step: "design-selection-recorded", Nth: 1, Reached: make(chan struct{})}
				f.Hook.Store(hook)
				answered := make(chan error, 1)
				go func() {
					answered <- f.C.Do(ctx, entry.Answer.Method, entry.Answer.Path, api.ProjectSelectionRequest{Projects: []config.ProjectID{f.Project, f.Project2}}, nil)
				}()
				select {
				case <-hook.Reached:
				case <-time.After(fixturekit.DemoTimeout):
					f.Timeout(t, "interrupted owner selection")
				}
				f.Stop(t)
				<-answered
				f.Hook.Store(nil)
				f.Start(t)
			}
			f.AwaitPacket(t)
			r, err = f.S.Repository(f.Project)
			fixturekit.Must(t, err)
			state, err := r.Workflow(f.Origin, intake.DesignSessionSubject)
			fixturekit.Must(t, err)
			if state.Value != intake.DesignClosedValue {
				t.Fatalf("handed design session remained open: %+v", state)
			}
		})
	}
}
