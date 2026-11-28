package lifecycle_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestWorkgroupMembershipAcrossArchiveKindsAndRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := journey.NewConfiguredProductionDesignFixture(t, journey.ProductionDesignConfiguration{
		Wait: journey.LifecycleAwait,
		Options: func(opts *journey.Options) {
			opts.Reconciliation.Hold = func(_ config.WorkstreamID, op coreadapter.Operation) bool {
				return op.Boundary == coreadapter.ContainerBoundary
			}
		},
	})
	defer f.Stop(t)
	f.StartOwner(t)
	packet := f.AwaitPacket(t)
	_, err := f.C.Ratify(ctx, f.Origin, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan, packet.ProjectSpecRevision)
	fixturekit.Must(t, err)
	f.AwaitApproved(t)
	f.CheckContinuation(t)
	repo, err := f.S.Repository(f.Project)
	fixturekit.Must(t, err)
	sharedID, err := repo.CrossRepoWorkgroupOf(f.Stream1)
	fixturekit.Must(t, err)
	target, host, _, closeTarget, err := f.S.Lifecycle().ManagerRepository(ctx, repo, f.Stream1)
	fixturekit.Must(t, err)
	manager, err := target.ManagerThread(host)
	closeTarget()
	fixturekit.Must(t, err)

	f.Stop(t)
	live := config.WorkstreamID("w_00000000000000000000000000000101")
	retained := config.WorkstreamID("w_00000000000000000000000000000102")
	cleaned := config.WorkstreamID("w_00000000000000000000000000000103")
	session := config.WorkstreamID("w_00000000000000000000000000000104")
	cfg := loadRegistered(t, f.Opts.Config)
	repo, err = trace.Open(cfg.Root, cfg.For(f.Project).Project)
	fixturekit.Must(t, err)
	for _, stream := range []config.WorkstreamID{live, retained} {
		fixturekit.Must(t, repo.CreateWorkstream(ctx, stream, fixturekit.DemoStart, intake.RegistrationActor))
	}
	_, err = repo.SetFeatureState(ctx, trace.Header{
		Schema: "osmia.trace.transition", Version: trace.Version, ID: "retained-cancelled", Revision: 1,
		Project: f.Project, Workstream: retained, At: fixturekit.DemoStart, Actor: intake.RegistrationActor, Cause: "fixture",
	}, intake.AbandonedState, "cancelled")
	fixturekit.Must(t, err)
	blockArchiveCleanup(t, repo, f.Project, retained, fixturekit.DemoStart, intake.RegistrationActor)
	fixturekit.Must(t, repo.CreateSessionWorkstream(ctx, session, "default", f.Repos[0].Main, "osmia/session/"+string(session), "Explore uploads", fixturekit.DemoStart, intake.RegistrationActor))
	originalManagers := map[config.WorkstreamID]trace.Thread{}
	for _, stream := range []config.WorkstreamID{live, retained} {
		originalManagers[stream], err = repo.ManagerThread(stream)
		fixturekit.Must(t, err)
	}
	sessionPath := filepath.Join(f.Opts.Config.Root, "workstreams", string(f.Project), string(session))
	sessionBefore := fixturekit.TraceCommits(t, sessionPath)
	repo.Close()
	path := filepath.Join(f.Opts.Config.Root, "runtime.json")
	data, err := os.ReadFile(path)
	state := runtime.State{Version: runtime.Version}
	if err == nil {
		fixturekit.Must(t, json.Unmarshal(data, &state))
	} else if !os.IsNotExist(err) {
		fixturekit.Must(t, err)
	}
	archives := []runtime.Archive{
		{Project: f.Project, Workstream: retained, ArchivedAt: fixturekit.DemoStart, Title: "Retained request", State: intake.AbandonedState, ResolutionEvidence: "cancelled", Resolved: true},
		{Project: f.Project, Workstream: cleaned, ArchivedAt: fixturekit.DemoStart, CleanedAt: fixturekit.DemoStart.Add(time.Minute), Title: "Cleaned request", State: intake.DeliveredState},
	}
	state.Archived = append(state.Archived, archives...)
	data, err = json.Marshal(state)
	fixturekit.Must(t, err)
	fixturekit.Must(t, os.WriteFile(path, data, 0600))

	type membership struct {
		repo   config.ProjectID
		stream config.WorkstreamID
	}
	var before map[membership][]config.WorkgroupID
	for restart := range 2 {
		f.Start(t)
		memberships := map[membership][]config.WorkgroupID{}
		for _, project := range []config.ProjectID{f.Project, f.Project2} {
			if f.S.Current().For(project).Project.ID != project {
				t.Fatalf("repo %s is not active", project)
			}
			r, err := f.S.Repository(project)
			fixturekit.Must(t, err)
			groups, _, err := trace.Workgroups(r)
			fixturekit.Must(t, err)
			for _, g := range groups {
				if len(g.Members) != 1 {
					t.Fatalf("unexpected local group: %+v", g)
				}
				for _, stream := range g.Members {
					key := membership{project, stream}
					memberships[key] = append(memberships[key], g.ID)
				}
			}
		}
		authority, err := trace.OpenWorkgroupAuthority(f.S.Current().Root, sharedID)
		fixturekit.Must(t, err)
		groups, _, err := trace.Workgroups(authority)
		fixturekit.Must(t, err)
		if len(groups) != 1 || groups[0].ID != sharedID || !slices.Equal(groups[0].Members, []config.WorkstreamID{f.Origin, f.Stream1, f.Stream2}) {
			t.Fatalf("shared authority lost exact membership: %+v", groups)
		}
		sharedManager, err := authority.ManagerThread(f.Origin)
		authority.Close()
		fixturekit.Must(t, err)
		if !reflect.DeepEqual(sharedManager.Identity, manager.Identity) {
			t.Fatal("startup replaced the shared manager thread")
		}
		for _, member := range []membership{{f.Project, f.Stream1}, {f.Project2, f.Stream2}} {
			r, err := f.S.Repository(member.repo)
			fixturekit.Must(t, err)
			id, err := r.CrossRepoWorkgroupOf(member.stream)
			fixturekit.Must(t, err)
			if id != sharedID {
				t.Fatalf("member %v points to %s, want %s", member, id, sharedID)
			}
			memberships[member] = append(memberships[member], sharedID)
			if _, err := r.ManagerThread(member.stream); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("member %v has an independent manager: %v", member, err)
			}
		}
		want := []membership{{f.Project, live}, {f.Project, retained}, {f.Project, cleaned}, {f.Project, f.Stream1}, {f.Project2, f.Stream2}}
		if len(memberships) != len(want) {
			t.Fatalf("unexpected memberships: %+v", memberships)
		}
		for _, member := range want {
			if len(memberships[member]) != 1 {
				t.Fatalf("member %v belongs to %v, want exactly one group", member, memberships[member])
			}
		}
		if restart > 0 && !reflect.DeepEqual(before, memberships) {
			t.Fatalf("restart changed membership IDs: before=%v after=%v", before, memberships)
		}
		before = memberships
		var sharedEntries []api.ConversationEntry
		for i, stream := range []config.WorkstreamID{f.Stream1, f.Stream2} {
			conversation, err := f.C.Conversation(ctx, stream)
			fixturekit.Must(t, err)
			if i == 0 {
				sharedEntries = conversation.Entries
			} else if !reflect.DeepEqual(sharedEntries, conversation.Entries) {
				t.Fatal("member conversations no longer resolve to the shared manager")
			}
		}
		r, err := f.S.Repository(f.Project)
		fixturekit.Must(t, err)
		for stream, original := range originalManagers {
			current, err := r.ManagerThread(stream)
			fixturekit.Must(t, err)
			if !reflect.DeepEqual(current.Identity, original.Identity) {
				t.Fatalf("original manager changed for %s", stream)
			}
		}
		if fixturekit.TraceCommits(t, sessionPath) != sessionBefore {
			t.Fatal("startup changed the exploratory session")
		}
		if _, err := r.ManagerThread(cleaned); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("cleaned archive gained a manager thread: %v", err)
		}
		if _, err := f.C.Conversation(ctx, cleaned); !intakefixture.Failed(err, api.Conflict) {
			t.Fatalf("cleaned archive gained a conversation: %v", err)
		}
		if _, err := os.Lstat(filepath.Join(f.Opts.Config.Root, "workstreams", string(f.Project), string(cleaned))); !os.IsNotExist(err) {
			t.Fatalf("cleaned archive history was recreated: %v", err)
		}
		for _, original := range archives {
			current, found := intakefixture.ArchiveRecord(t, f.S.Store, original.Workstream)
			if !found || !reflect.DeepEqual(original, current) {
				t.Fatalf("archive changed: before=%+v after=%+v", original, current)
			}
		}
		if restart == 0 {
			f.Stop(t)
		}
	}
}
