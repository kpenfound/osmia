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
	"github.com/kpenfound/osmia/internal/trace"
)

const heldApprovalMessage = "workgroup approval remains held"

// otherWorkgroup names a workgroup no fixture creates.
const otherWorkgroup config.WorkgroupID = "g_ffffffffffffffffffffffffffffffff"

// logLine is one structured log record the service wrote.
type logLine struct {
	level slog.Level
	msg   string
	attrs map[string]string
}

// logCapture keeps every record written through its handler.
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

// warnings returns the warning-or-worse records written so far.
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

// held returns the held-approval warnings that name workgroup id.
func (c *logCapture) held(id config.WorkgroupID) []logLine {
	var out []logLine
	for _, l := range c.warnings() {
		if l.msg == heldApprovalMessage && l.attrs["workgroup"] == string(id) {
			out = append(out, l)
		}
	}
	return out
}

// heldApprovalFixture is a service over two projects holding cross-repo
// workgroups whose members each live in a different project. Every workgroup
// has one member in each project, so a group-approval pass over either
// project's repository reaches it.
type heldApprovalFixture struct {
	s      *Service
	logs   *logCapture
	groups []trace.Workgroup
	// streams[i] is group i's member in each project, in project order.
	streams [][]config.WorkstreamID
	chiefs  []*trace.Repository
}

// newHeldApprovalFixture creates count workgroups. With crossListed every
// stream is also known to the other project's runtime, so an archive record
// can name a project the stream never belonged to.
func newHeldApprovalFixture(t *testing.T, count int, crossListed bool) *heldApprovalFixture {
	t.Helper()
	ctx := context.Background()
	f := newTwoProjectFixture(t)
	var repos []*trace.Repository
	for _, p := range f.cfg.Projects {
		r, err := trace.Open(f.cfg.Root, p)
		must(t, err)
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
				must(t, r.CreateWorkstream(ctx, stream, demoStart, ownerActor))
			}
			members = append(members, trace.CrossRepoMember{Project: r, Workstream: stream})
			streams = append(streams, stream)
			known[r.Project()] = append(known[r.Project()], stream)
		}
		chief, group, err := trace.CreateWorkgroupTrace(ctx, f.cfg.Root, members, demoStart, ownerActor, "fixture")
		must(t, err)
		t.Cleanup(func() { chief.Close() })
		for _, m := range members {
			must(t, m.Project.JoinCrossRepoWorkgroup(ctx, m.Workstream, group.ID, demoStart, ownerActor, "fixture"))
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
	must(t, err)
	t.Cleanup(func() { store.Close() })
	h.s = &Service{cfg: f.cfg, options: f.opts, store: store, logger: slog.New(h.logs)}
	for _, r := range repos {
		h.s.projects = append(h.s.projects, &activeProject{id: r.Project(), repository: r})
		h.s.installMemberLifecycle(r)
	}
	return h
}

// pass runs the production group-approval pass over every project.
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
		must(t, h.pass(t, context.Background()))
	}
}

// archive records group i's member j as archived in the given project under
// the given workgroup.
func (h *heldApprovalFixture) archive(t *testing.T, i, j int, project config.ProjectID, group config.WorkgroupID) {
	t.Helper()
	must(t, h.s.store.SetArchived(runtime.Archive{Project: project, Workstream: h.streams[i][j], Workgroup: group, State: AbandonedState, Title: "Archived member", Resolved: true, ResolutionEvidence: "cancelled", ArchivedAt: demoStart}))
}

// retire archives every member of group i with valid evidence.
func (h *heldApprovalFixture) retire(t *testing.T, i int) {
	t.Helper()
	for j, p := range h.s.projects {
		h.archive(t, i, j, p.id, h.groups[i].ID)
	}
}

// reach fails unless group i's streams are among the ones a pass lists, so a
// quiet pass cannot be mistaken for one that never reached the group.
func (h *heldApprovalFixture) reach(t *testing.T, i int) {
	t.Helper()
	for j, p := range h.s.projects {
		streams, err := intake.NonSessionWorkstreams(p.repository)
		must(t, err)
		if !slices.Contains(streams, h.streams[i][j]) {
			t.Fatalf("group %d member %s is not listed by project %s", i, h.streams[i][j], p.id)
		}
	}
}

// reconciled reports how many times the passes acquired group i's trace,
// which only reconciliation of a live leader does.
func (h *heldApprovalFixture) reconciled(i int) uint64 {
	return trace.WorkgroupTraceOpenCount(h.groups[i].ID)
}

// assertUntouched fails if group i's approval has any durable effect.
func (h *heldApprovalFixture) assertUntouched(t *testing.T, i int) {
	t.Helper()
	id := h.groups[i].ID
	authority, err := trace.OpenWorkgroupAuthority(h.s.current().Root, id)
	must(t, err)
	defer authority.Close()
	_, _, found, err := trace.ReadGroupApproval(authority, h.groups[i].Members[0])
	must(t, err)
	if found {
		t.Fatalf("group %d recorded an approval", i)
	}
	status, err := h.s.projects[0].repository.GroupApprovalStatusOf(id)
	must(t, err)
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
	must(t, err)
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
	// Group 0's first member is archived under another workgroup, so leader
	// discovery fails; group 1 has no archive record at all and stays live.
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
	// Each archive record names the other member's project, so no member is
	// live and the retirement check rejects both of them.
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
	release := func() { must(t, h.s.store.ClearArchived(h.streams[0][0])) }
	want := func(n int, why string) {
		t.Helper()
		if got := h.logs.held(id); len(got) != n {
			t.Fatalf("%s: %d warnings, want %d: %+v", why, len(got), n, got)
		}
	}

	mismatch()
	h.passes(t, 3)
	want(1, "first report")

	// A live leader ends the unverified state.
	release()
	h.passes(t, 3)
	want(1, "live leader")
	mismatch()
	h.passes(t, 3)
	want(2, "recurrence after a live leader")

	// Verified retirement ends it too.
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
	must(t, trace.RecordGroupApproval(context.Background(), h.chiefs[0], h.groups[0].Members[0], trace.GroupApproval{Workgroup: id, Members: pins, Complete: true}, demoStart, ownerActor))
	h.passes(t, 2)
	if _, held := heldGroupApprovals.Load(h.s.heldGroupApprovalKey(id)); held {
		t.Fatal("a completed approval stayed recorded as unverified")
	}
	if got := h.logs.held(id); len(got) != 1 {
		t.Fatalf("warnings = %+v", got)
	}
}

// cancelAfter is a context whose Err reports cancellation once it has been
// read limit times.
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
			// The pass reads its context once on entry; every later read reports
			// cancellation.
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
