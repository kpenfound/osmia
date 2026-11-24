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
	f := newConfiguredProductionDesignFixture(t, productionDesignConfiguration{
		wait: lifecycleAwait,
		options: func(opts *Options) {
			opts.Reconciliation.Hold = func(_ config.WorkstreamID, op coreadapter.Operation) bool {
				return op.Boundary == coreadapter.ContainerBoundary
			}
		},
	})
	defer f.stop(t)
	f.startOwner(t)
	packet := f.awaitPacket(t)
	_, err := f.c.Ratify(ctx, f.origin, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan, packet.ProjectSpecRevision)
	intakefixture.Must(t, err)
	f.awaitApproved(t)
	f.checkContinuation(t)
	repo, err := f.s.repository(f.project)
	intakefixture.Must(t, err)
	sharedID, err := repo.CrossRepoWorkgroupOf(f.stream1)
	intakefixture.Must(t, err)
	target, host, _, closeTarget, err := f.s.chiefRepository(ctx, repo, f.stream1)
	intakefixture.Must(t, err)
	chief, err := target.ChiefOfStaffThread(host)
	closeTarget()
	intakefixture.Must(t, err)

	f.stop(t)
	live := config.WorkstreamID("w_00000000000000000000000000000101")
	retained := config.WorkstreamID("w_00000000000000000000000000000102")
	cleaned := config.WorkstreamID("w_00000000000000000000000000000103")
	session := config.WorkstreamID("w_00000000000000000000000000000104")
	cfg := loadRegistered(t, f.opts.Config)
	repo, err = trace.Open(cfg.Root, cfg.For(f.project).Project)
	intakefixture.Must(t, err)
	for _, stream := range []config.WorkstreamID{live, retained} {
		intakefixture.Must(t, repo.CreateWorkstream(ctx, stream, fixturekit.DemoStart, intake.RegistrationActor))
	}
	_, err = repo.SetFeatureState(ctx, trace.Header{
		Schema: "osmia.trace.transition", Version: trace.Version, ID: "retained-cancelled", Revision: 1,
		Project: f.project, Workstream: retained, At: fixturekit.DemoStart, Actor: intake.RegistrationActor, Cause: "fixture",
	}, intake.AbandonedState, "cancelled")
	intakefixture.Must(t, err)
	blockArchiveCleanup(t, repo, f.project, retained, fixturekit.DemoStart, intake.RegistrationActor)
	intakefixture.Must(t, repo.CreateSessionWorkstream(ctx, session, "default", f.repos[0].Main, "osmia/session/"+string(session), "Explore uploads", fixturekit.DemoStart, intake.RegistrationActor))
	legacyChiefs := map[config.WorkstreamID]trace.Thread{}
	for _, stream := range []config.WorkstreamID{live, retained} {
		legacyChiefs[stream], err = repo.ChiefOfStaffThread(stream)
		intakefixture.Must(t, err)
	}
	sessionPath := filepath.Join(f.opts.Config.Root, "workstreams", string(f.project), string(session))
	sessionBefore := fixturekit.TraceCommits(t, sessionPath)
	repo.Close()
	path := filepath.Join(f.opts.Config.Root, "runtime.json")
	data, err := os.ReadFile(path)
	state := runtime.State{Version: runtime.Version}
	if err == nil {
		intakefixture.Must(t, json.Unmarshal(data, &state))
	} else if !os.IsNotExist(err) {
		intakefixture.Must(t, err)
	}
	archives := []runtime.Archive{
		{Project: f.project, Workstream: retained, ArchivedAt: fixturekit.DemoStart, Title: "Retained request", State: intake.AbandonedState, ResolutionEvidence: "cancelled", Resolved: true},
		{Project: f.project, Workstream: cleaned, ArchivedAt: fixturekit.DemoStart, CleanedAt: fixturekit.DemoStart.Add(time.Minute), Title: "Cleaned request", State: intake.DeliveredState},
	}
	state.Archived = append(state.Archived, archives...)
	data, err = json.Marshal(state)
	intakefixture.Must(t, err)
	intakefixture.Must(t, os.WriteFile(path, data, 0600))

	type membership struct {
		repo   config.ProjectID
		stream config.WorkstreamID
	}
	var before map[membership][]config.WorkgroupID
	for restart := range 2 {
		f.start(t)
		memberships := map[membership][]config.WorkgroupID{}
		for _, project := range []config.ProjectID{f.project, f.project2} {
			if f.s.current().For(project).Project.ID != project {
				t.Fatalf("repo %s is not active", project)
			}
			r, err := f.s.repository(project)
			intakefixture.Must(t, err)
			groups, _, err := trace.Workgroups(r)
			intakefixture.Must(t, err)
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
		authority, err := trace.OpenWorkgroupAuthority(f.s.current().Root, sharedID)
		intakefixture.Must(t, err)
		groups, _, err := trace.Workgroups(authority)
		intakefixture.Must(t, err)
		if len(groups) != 1 || groups[0].ID != sharedID || !slices.Equal(groups[0].Members, []config.WorkstreamID{f.origin, f.stream1, f.stream2}) {
			t.Fatalf("shared authority lost exact membership: %+v", groups)
		}
		sharedChief, err := authority.ChiefOfStaffThread(f.origin)
		authority.Close()
		intakefixture.Must(t, err)
		if !reflect.DeepEqual(sharedChief.Identity, chief.Identity) {
			t.Fatal("startup replaced the shared chief thread")
		}
		for _, member := range []membership{{f.project, f.stream1}, {f.project2, f.stream2}} {
			r, err := f.s.repository(member.repo)
			intakefixture.Must(t, err)
			id, err := r.CrossRepoWorkgroupOf(member.stream)
			intakefixture.Must(t, err)
			if id != sharedID {
				t.Fatalf("member %v points to %s, want %s", member, id, sharedID)
			}
			memberships[member] = append(memberships[member], sharedID)
			if _, err := r.ChiefOfStaffThread(member.stream); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("member %v has an independent chief: %v", member, err)
			}
		}
		want := []membership{{f.project, live}, {f.project, retained}, {f.project, cleaned}, {f.project, f.stream1}, {f.project2, f.stream2}}
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
		for i, stream := range []config.WorkstreamID{f.stream1, f.stream2} {
			conversation, err := f.c.Conversation(ctx, stream)
			intakefixture.Must(t, err)
			if i == 0 {
				sharedEntries = conversation.Entries
			} else if !reflect.DeepEqual(sharedEntries, conversation.Entries) {
				t.Fatal("member conversations no longer resolve to the shared chief")
			}
		}
		r, err := f.s.repository(f.project)
		intakefixture.Must(t, err)
		for stream, original := range legacyChiefs {
			current, err := r.ChiefOfStaffThread(stream)
			intakefixture.Must(t, err)
			if !reflect.DeepEqual(current.Identity, original.Identity) {
				t.Fatalf("legacy chief changed for %s", stream)
			}
		}
		if fixturekit.TraceCommits(t, sessionPath) != sessionBefore {
			t.Fatal("startup changed the exploratory session")
		}
		if _, err := r.ChiefOfStaffThread(cleaned); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("cleaned archive gained a chief thread: %v", err)
		}
		if _, err := f.c.Conversation(ctx, cleaned); !intakefixture.Failed(err, api.Conflict) {
			t.Fatalf("cleaned archive gained a conversation: %v", err)
		}
		if _, err := os.Lstat(filepath.Join(f.opts.Config.Root, "workstreams", string(f.project), string(cleaned))); !os.IsNotExist(err) {
			t.Fatalf("cleaned archive history was recreated: %v", err)
		}
		for _, original := range archives {
			current, found := intakefixture.ArchiveRecord(t, f.s.store, original.Workstream)
			if !found || !reflect.DeepEqual(original, current) {
				t.Fatalf("archive changed: before=%+v after=%+v", original, current)
			}
		}
		if restart == 0 {
			f.stop(t)
		}
	}
}
