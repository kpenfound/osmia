package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

const heldApprovalMessage = "workgroup approval remains held"

const otherWorkgroup config.WorkgroupID = "g_ffffffffffffffffffffffffffffffff"

type logLine struct {
	level slog.Level
	msg   string
	attrs map[string]string
}

type logCapture struct {
	mu    sync.Mutex
	lines []logLine
}

func (c *logCapture) Enabled(context.Context, slog.Level) bool { return true }
func (c *logCapture) WithAttrs([]slog.Attr) slog.Handler       { return c }
func (c *logCapture) WithGroup(string) slog.Handler            { return c }
func (c *logCapture) Handle(_ context.Context, r slog.Record) error {
	line := logLine{level: r.Level, msg: r.Message, attrs: map[string]string{}}
	r.Attrs(func(a slog.Attr) bool {
		line.attrs[a.Key] = a.Value.String()
		return true
	})
	c.mu.Lock()
	c.lines = append(c.lines, line)
	c.mu.Unlock()
	return nil
}

func (c *logCapture) warnings() []logLine {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []logLine
	for _, l := range c.lines {
		if l.level >= slog.LevelWarn {
			out = append(out, l)
		}
	}
	return out
}

func (c *logCapture) held(id config.WorkgroupID) []logLine {
	var out []logLine
	for _, l := range c.warnings() {
		if l.msg == heldApprovalMessage && l.attrs["workgroup"] == string(id) {
			out = append(out, l)
		}
	}
	return out
}

type heldApprovalFixture struct {
	s       *Service
	logs    *logCapture
	groups  []trace.Workgroup
	streams [][]config.WorkstreamID
	chiefs  []*trace.Repository
}

func newHeldApprovalFixture(t *testing.T, count int, crossListed bool) *heldApprovalFixture {
	t.Helper()
	ctx := context.Background()
	f := newTwoProjectFixture(t)
	var repos []*trace.Repository
	for _, p := range f.cfg.Projects {
		r, err := trace.Open(f.cfg.Root, p)
		intakefixture.Must(t, err)
		t.Cleanup(func() { r.Close() })
		repos = append(repos, r)
	}
	h := &heldApprovalFixture{logs: &logCapture{}}
	known := map[config.ProjectID][]config.WorkstreamID{}
	for i := range count {
		var members []trace.CrossRepoMember
		var streams []config.WorkstreamID
		for j, r := range repos {
			stream := f.streams[r.Project()]
			if i > 0 {
				stream = config.WorkstreamID(fmt.Sprintf("w_%02x%030x", i, j))
				intakefixture.Must(t, r.CreateWorkstream(ctx, stream, fixturekit.DemoStart, ownerActor))
			}
			members = append(members, trace.CrossRepoMember{Project: r, Workstream: stream})
			streams = append(streams, stream)
			known[r.Project()] = append(known[r.Project()], stream)
		}
		chief, group, err := trace.CreateWorkgroupTrace(ctx, f.cfg.Root, members, fixturekit.DemoStart, ownerActor, "fixture")
		intakefixture.Must(t, err)
		t.Cleanup(func() { chief.Close() })
		for _, m := range members {
			intakefixture.Must(t, m.Project.JoinCrossRepoWorkgroup(ctx, m.Workstream, group.ID, fixturekit.DemoStart, ownerActor, "fixture"))
		}
		h.groups = append(h.groups, group)
		h.streams = append(h.streams, streams)
		h.chiefs = append(h.chiefs, chief)
	}
	if crossListed {
		all := slices.Concat(known[repos[0].Project()], known[repos[1].Project()])
		for _, r := range repos {
			known[r.Project()] = slices.Clone(all)
		}
	}
	store, _, err := runtime.Open(runtime.Inputs{Config: f.cfg, Workstreams: known})
	intakefixture.Must(t, err)
	t.Cleanup(func() { store.Close() })
	h.s = &Service{cfg: f.cfg, options: f.opts, store: store, logger: slog.New(h.logs)}
	for _, r := range repos {
		h.s.projects = append(h.s.projects, &activeProject{id: r.Project(), repository: r})
		h.s.installMemberLifecycle(r)
	}
	return h
}

func (h *heldApprovalFixture) pass(t *testing.T, ctx context.Context) error {
	t.Helper()
	for _, p := range h.s.projects {
		if err := h.s.groupApprovalPass(ctx, p.repository); err != nil {
			return err
		}
	}
	return nil
}

func (h *heldApprovalFixture) passes(t *testing.T, n int) {
	t.Helper()
	for range n {
		intakefixture.Must(t, h.pass(t, context.Background()))
	}
}

func (h *heldApprovalFixture) archive(t *testing.T, i, j int, project config.ProjectID, group config.WorkgroupID) {
	t.Helper()
	intakefixture.Must(t, h.s.store.SetArchived(runtime.Archive{Project: project, Workstream: h.streams[i][j], Workgroup: group, State: AbandonedState, Title: "Archived member", Resolved: true, ResolutionEvidence: "cancelled", ArchivedAt: fixturekit.DemoStart}))
}

func (h *heldApprovalFixture) retire(t *testing.T, i int) {
	t.Helper()
	for j, p := range h.s.projects {
		h.archive(t, i, j, p.id, h.groups[i].ID)
	}
}

func (h *heldApprovalFixture) reach(t *testing.T, i int) {
	t.Helper()
	for j, p := range h.s.projects {
		streams, err := intake.NonSessionWorkstreams(p.repository)
		intakefixture.Must(t, err)
		if !slices.Contains(streams, h.streams[i][j]) {
			t.Fatalf("group %d member %s is not listed by project %s", i, h.streams[i][j], p.id)
		}
	}
}

func (h *heldApprovalFixture) reconciled(i int) uint64 {
	return trace.WorkgroupTraceOpenCount(h.groups[i].ID)
}

func (h *heldApprovalFixture) assertUntouched(t *testing.T, i int) {
	t.Helper()
	id := h.groups[i].ID
	authority, err := trace.OpenWorkgroupAuthority(h.s.current().Root, id)
	intakefixture.Must(t, err)
	defer authority.Close()
	_, _, found, err := trace.ReadGroupApproval(authority, h.groups[i].Members[0])
	intakefixture.Must(t, err)
	if found {
		t.Fatalf("group %d recorded an approval", i)
	}
	status, err := h.s.projects[0].repository.GroupApprovalStatusOf(id)
	intakefixture.Must(t, err)
	if status.Complete {
		t.Fatalf("group %d approval completed", i)
	}
	for j, p := range h.s.projects {
		if _, found, err := latestRatification(p.repository, h.streams[i][j]); err != nil || found {
			t.Fatalf("group %d member %d ratification found=%v err=%v", i, j, found, err)
		}
	}
}

func TestGroupApprovalPassIsQuietForFullyRetiredWorkgroup(t *testing.T) {
	t.Parallel()
	h := newHeldApprovalFixture(t, 1, false)
	h.retire(t, 0)
	h.reach(t, 0)
	retired, err := h.s.workgroupFullyRetired(context.Background(), h.groups[0])
	intakefixture.Must(t, err)
	if !retired {
		t.Fatal("fixture workgroup is not verifiably retired")
	}
	h.passes(t, 5)
	if got := h.logs.warnings(); len(got) != 0 {
		t.Fatalf("a fully retired workgroup logged %+v", got)
	}
	h.assertUntouched(t, 0)
}

func TestGroupApprovalPassReportsMismatchedArchiveOnceAndReconcilesOtherGroups(t *testing.T) {
	t.Parallel()
	h := newHeldApprovalFixture(t, 2, false)
	h.archive(t, 0, 0, h.s.projects[0].id, otherWorkgroup)
	h.reach(t, 0)
	h.reach(t, 1)
	if _, err := h.s.liveWorkgroupLeader(h.groups[0]); err == nil {
		t.Fatal("leader discovery accepted an archive record for another workgroup")
	}
	for round := range 4 {
		before := h.reconciled(1)
		h.passes(t, 1)
		if h.reconciled(1) == before {
			t.Fatalf("round %d did not reconcile the live workgroup", round)
		}
	}
	held := h.logs.held(h.groups[0].ID)
	if len(held) != 1 || held[0].attrs["error"] == "" {
		t.Fatalf("mismatched archive warnings = %+v", held)
	}
	if got := h.logs.warnings(); len(got) != 1 {
		t.Fatalf("warnings = %+v", got)
	}
	h.assertUntouched(t, 0)
}

func TestGroupApprovalPassReportsOnceWhenSeveralMembersAreInvalid(t *testing.T) {
	t.Parallel()
	h := newHeldApprovalFixture(t, 1, true)
	h.archive(t, 0, 0, h.s.projects[1].id, h.groups[0].ID)
	h.archive(t, 0, 1, h.s.projects[0].id, h.groups[0].ID)
	h.reach(t, 0)
	if leader, err := h.s.liveWorkgroupLeader(h.groups[0]); err != nil || leader != "" {
		t.Fatalf("leader = %q, %v; want no live member", leader, err)
	}
	reasons := map[string]bool{}
	for range 64 {
		retired, err := h.s.workgroupFullyRetired(context.Background(), h.groups[0])
		if retired || err == nil {
			t.Fatalf("retirement verified = %v, %v", retired, err)
		}
		reasons[err.Error()] = true
	}
	if len(reasons) < 2 {
		t.Fatalf("the retirement check named one failing member: %v", reasons)
	}
	h.passes(t, 40)
	if held := h.logs.held(h.groups[0].ID); len(held) != 1 || held[0].attrs["error"] == "" {
		t.Fatalf("invalid evidence warnings = %+v", held)
	}
	if got := h.logs.warnings(); len(got) != 1 {
		t.Fatalf("warnings = %+v", got)
	}
	h.assertUntouched(t, 0)
}

func TestGroupApprovalPassReportsAgainWhenUnverifiedStateRecurs(t *testing.T) {
	t.Parallel()
	h := newHeldApprovalFixture(t, 1, false)
	id := h.groups[0].ID
	mismatch := func() { h.archive(t, 0, 0, h.s.projects[0].id, otherWorkgroup) }
	release := func() { intakefixture.Must(t, h.s.store.ClearArchived(h.streams[0][0])) }
	want := func(n int, why string) {
		t.Helper()
		if got := h.logs.held(id); len(got) != n {
			t.Fatalf("%s: %d warnings, want %d: %+v", why, len(got), n, got)
		}
	}

	mismatch()
	h.passes(t, 3)
	want(1, "first report")

	release()
	h.passes(t, 3)
	want(1, "live leader")
	mismatch()
	h.passes(t, 3)
	want(2, "recurrence after a live leader")

	release()
	h.retire(t, 0)
	h.passes(t, 3)
	want(2, "verified retirement")
	release()
	mismatch()
	h.passes(t, 3)
	want(3, "recurrence after verified retirement")
}

func TestGroupApprovalPassReleasesHeldWorkgroupWhenApprovalCompletes(t *testing.T) {
	t.Parallel()
	h := newHeldApprovalFixture(t, 1, false)
	id := h.groups[0].ID
	h.archive(t, 0, 0, h.s.projects[0].id, otherWorkgroup)
	h.passes(t, 2)
	if _, held := heldGroupApprovals.Load(h.s.heldGroupApprovalKey(id)); !held {
		t.Fatal("the unverified workgroup was not recorded")
	}
	pins := []trace.GroupMemberApproval{
		{Project: h.s.projects[0].id, Workstream: h.streams[0][0]},
		{Project: h.s.projects[1].id, Workstream: h.streams[0][1]},
	}
	intakefixture.Must(t, trace.RecordGroupApproval(context.Background(), h.chiefs[0], h.groups[0].Members[0], trace.GroupApproval{Workgroup: id, Members: pins, Complete: true}, fixturekit.DemoStart, ownerActor))
	h.passes(t, 2)
	if _, held := heldGroupApprovals.Load(h.s.heldGroupApprovalKey(id)); held {
		t.Fatal("a completed approval stayed recorded as unverified")
	}
	if got := h.logs.held(id); len(got) != 1 {
		t.Fatalf("warnings = %+v", got)
	}
}

type cancelAfter struct {
	context.Context
	reads atomic.Int32
	limit int32
}

func (c *cancelAfter) Err() error {
	if c.reads.Add(1) > c.limit {
		return context.Canceled
	}
	return nil
}

func TestGroupApprovalPassStopsOnCancellationWhileCheckingRetirement(t *testing.T) {
	t.Parallel()
	cases := map[string]func(*testing.T, *heldApprovalFixture){
		"leader discovery": func(t *testing.T, h *heldApprovalFixture) {
			h.archive(t, 0, 0, h.s.projects[0].id, otherWorkgroup)
		},
		"retirement check": func(t *testing.T, h *heldApprovalFixture) { h.retire(t, 0) },
	}
	for name, arrange := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newHeldApprovalFixture(t, 1, false)
			arrange(t, h)
			ctx := &cancelAfter{Context: context.Background(), limit: 1}
			err := h.s.groupApprovalPass(ctx, h.s.projects[0].repository)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("pass = %v, want cancellation", err)
			}
			if got := h.logs.warnings(); len(got) != 0 {
				t.Fatalf("a canceled pass logged %+v", got)
			}
		})
	}
}
