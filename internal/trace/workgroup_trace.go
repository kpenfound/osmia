package trace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kpenfound/osmia/internal/config"
)

func crossRepoProject(id config.WorkgroupID) config.ProjectID {
	return config.ProjectID("p_" + strings.TrimPrefix(string(id), "g_"))
}

// WorkgroupProjectID inverts crossRepoProject: it reports the workgroup ID
// that project would name if it is a cross-repo workgroup's own synthetic
// project identity. Every ordinary project's own identity matches the same
// "p_" followed by 32 hex digits pattern, so a true match is this
// transform's own reversibility alone, not a guarantee that project is
// really a workgroup's own identity: a caller must still open or stat that
// workgroup's own trace to know whether it exists.
func WorkgroupProjectID(project config.ProjectID) (config.WorkgroupID, bool) {
	id := config.WorkgroupID("g_" + strings.TrimPrefix(string(project), "p_"))
	if config.CheckWorkgroupIDs(id) != nil {
		return "", false
	}
	return id, true
}

// CrossRepoMember names one real member of a cross-repo workgroup being
// created or reopened: its own workstream, and that workstream's own
// project's already open, project-level repository instance (for example,
// the value trace.Open or trace.Create returned for it), never a handle
// already scoped to the workstream itself. The workgroup trace resolves
// each member's own instance from this directly, so it never opens a
// second, competing handle on a member's trace.
type CrossRepoMember struct {
	Workstream config.WorkstreamID
	Project    *Repository
}

func crossRepoMembers(members []CrossRepoMember, group ...config.WorkgroupID) (map[config.WorkstreamID]CrossRepoMember, []config.WorkstreamID, error) {
	if len(members) == 0 {
		return nil, nil, fmt.Errorf("a workgroup needs at least one member")
	}
	byStream := make(map[config.WorkstreamID]CrossRepoMember, len(members))
	order := make([]config.WorkstreamID, len(members))
	for i, m := range members {
		if m.Project == nil {
			return nil, nil, fmt.Errorf("member %s has no open repository", m.Workstream)
		}
		if err := config.CheckWorkstreamIDs(m.Workstream); err != nil {
			return nil, nil, err
		}
		if m.Project.stream != "" {
			return nil, nil, fmt.Errorf("member %s's repository must be its project's own instance, not one already scoped to a workstream", m.Workstream)
		}
		if _, ok := byStream[m.Workstream]; ok {
			return nil, nil, fmt.Errorf("workstream %s listed twice", m.Workstream)
		}
		retired := false
		if len(group) != 0 {
			var err error
			retired, err = m.Project.workgroupMemberRetired(group[0], m.Workstream)
			if err != nil {
				return nil, nil, err
			}
		}
		if !retired {
			if _, liveErr := liveMemberRepository(m); liveErr != nil {
				if len(group) != 0 {
					retired, err := m.Project.workgroupMemberRetired(group[0], m.Workstream)
					if err != nil {
						return nil, nil, err
					}
					if retired {
						byStream[m.Workstream], order[i] = m, m.Workstream
						continue
					}
				}
				return nil, nil, fmt.Errorf("member %s: %w", m.Workstream, liveErr)
			}
		}
		byStream[m.Workstream] = m
		order[i] = m.Workstream
	}
	return byStream, order, nil
}

// CreateWorkgroupTrace creates a new workgroup-level trace, outside every
// one of members' own repos and outside every target repository, for a
// workgroup whose members span more than one repo. It creates the trace's
// own host workstream, which carries the one shared chief-of-staff thread
// and is never one of the real members, and records the workgroup itself,
// host first and then every real member in the given order, in the same
// commit CreateWorkstreamOn makes for the host. The caller must Close the
// returned repository, and must not also close any member's own repository
// through it: the workgroup trace never owns a member's lifecycle, only a
// direct reference to it.
//
// A workgroup confined to one repo keeps using CreateWorkgroup or
// EnsureWorkgroups directly on that repo's own trace; this is only for a
// workgroup whose members live in more than one.
func CreateWorkgroupTrace(ctx context.Context, root config.Root, members []CrossRepoMember, at time.Time, actor Actor, cause string) (*Repository, Workgroup, error) {
	byStream, order, err := crossRepoMembers(members)
	if err != nil {
		return nil, Workgroup{}, err
	}
	id, err := config.NewWorkgroupID()
	if err != nil {
		return nil, Workgroup{}, err
	}
	host, err := config.NewWorkstreamID(order...)
	if err != nil {
		return nil, Workgroup{}, err
	}
	r, err := createWorkgroupTraceDirectory(ctx, root, id, at, actor, cause)
	if err != nil {
		return nil, Workgroup{}, err
	}
	if err := r.bindMembers(byStream); err != nil {
		r.Close()
		return nil, Workgroup{}, err
	}
	ok := false
	defer func() {
		if !ok {
			r.Close()
		}
	}()
	if err := r.CreateWorkstreamOn(ctx, host, "", at, actor); err != nil {
		return nil, Workgroup{}, err
	}
	g := Workgroup{ID: id, Members: append([]config.WorkstreamID{host}, order...)}
	if err := recordWorkgroup(ctx, r, g, at, actor, cause); err != nil {
		return nil, Workgroup{}, err
	}
	ok = true
	return r, g, nil
}

func recordWorkgroup(ctx context.Context, r *Repository, g Workgroup, at time.Time, actor Actor, cause string) error {
	existing, revision, err := Workgroups(r)
	if err != nil {
		return err
	}
	next := append(slices.Clone(existing), g)
	data, err := json.Marshal(workgroupsFile{Version: 1, Workgroups: next})
	if err != nil {
		return err
	}
	return r.Append(ctx, Document{
		Header:  Header{Schema: "osmia.trace.document", Version: Version, ID: WorkgroupsDocument, Revision: revision + 1, Project: r.Project(), At: at, Actor: actor, Cause: cause},
		Path:    WorkgroupsPath,
		Content: string(data),
	})
}

// OpenWorkgroupTrace reopens a cross-repo workgroup's own trace, previously
// created by CreateWorkgroupTrace, with members giving each of its real
// members' own, already open repository instance so its scoped lookups
// reach each one directly. While any other caller in this process already
// has the same workgroup id open, this call reuses that one instance
// instead of taking a second exclusive hold on the same directory
// (openSharedWorkgroupTrace), so it answers promptly even while a long
// call such as a dispatched chief-of-staff turn is in flight on it. The
// caller must Close the returned repository exactly once.
func OpenWorkgroupTrace(root config.Root, id config.WorkgroupID, members []CrossRepoMember) (*Repository, error) {
	byStream, _, err := crossRepoMembers(members, id)
	if err != nil {
		return nil, err
	}
	r, err := openSharedWorkgroupTrace(root, id, nil)
	if err != nil {
		return r, err
	}
	if err := r.validateMemberBindings(id, byStream); err != nil {
		r.Close()
		return nil, err
	}
	if err := r.bindMembers(byStream); err != nil {
		r.Close()
		return nil, err
	}
	return r, nil
}

type sharedWorkgroupTrace struct {
	sync.Mutex
	repo               *Repository
	refs               int
	resolved           atomic.Bool
	opens              atomic.Uint64
	diskOpens          atomic.Uint64
	projectSpecReads   atomic.Uint64
	approvalDecodes    atomic.Uint64
	approval           approvalCache
	projectSpecDecodes atomic.Uint64
	projectSpec        projectSpecCache
}

var workgroupTraces sync.Map

func sharedWorkgroupTraceFor(id config.WorkgroupID) *sharedWorkgroupTrace {
	v, _ := workgroupTraces.LoadOrStore(id, &sharedWorkgroupTrace{})
	return v.(*sharedWorkgroupTrace)
}

func openSharedWorkgroupTrace(root config.Root, id config.WorkgroupID, members map[config.WorkstreamID]CrossRepoMember) (*Repository, error) {
	shared := sharedWorkgroupTraceFor(id)
	shared.opens.Add(1)
	shared.Lock()
	defer shared.Unlock()
	if shared.repo != nil {
		if shared.repo.root.String() != root.String() {
			return nil, fmt.Errorf("%w: workgroup %s is open under another root", ErrConflict, id)
		}
		if err := shared.repo.bindMembers(members); err != nil {
			return nil, err
		}
		shared.refs++
		return shared.repo, nil
	}
	directory, err := root.WorkgroupTrace(id)
	if err != nil {
		return nil, err
	}
	r, err := openStoreDirectory(root, crossRepoProject(id), "", directory)
	if err != nil {
		return nil, err
	}
	r.members = members
	if err := checkStoreFiles(r.directory); err != nil {
		r.Close()
		return nil, err
	}
	if err := acquireWorkgroupTrace(r); err != nil {
		r.Close()
		return nil, err
	}
	if err := r.openLocked(false); err != nil {
		return r, err
	}
	if _, _, err := r.scan(); err != nil {
		return r, err
	}
	r.shared = shared
	shared.repo, shared.refs = r, 1
	shared.diskOpens.Add(1)
	return r, nil
}

var workgroupLocks sync.Map

func workgroupProcessLock(id config.WorkgroupID) *sync.Mutex {
	v, _ := workgroupLocks.LoadOrStore(id, &sync.Mutex{})
	return v.(*sync.Mutex)
}

func acquireWorkgroupTrace(r *Repository) error {
	id, ok := WorkgroupProjectID(r.project)
	if !ok {
		return r.acquire()
	}
	lock := workgroupProcessLock(id)
	lock.Lock()
	ok = false
	defer func() {
		if !ok {
			lock.Unlock()
		}
	}()
	const budget = 50 * time.Millisecond
	delay := 2 * time.Millisecond
	deadline := time.Now().Add(budget)
	for {
		err := r.acquire()
		if err == nil {
			r.processLock = lock
			ok = true
			return nil
		}
		if !errors.Is(err, ErrLocked) || time.Now().After(deadline) {
			return err
		}
		time.Sleep(delay)
		if delay *= 2; delay > 10*time.Millisecond {
			delay = 10 * time.Millisecond
		}
	}
}

func createWorkgroupTraceDirectory(ctx context.Context, root config.Root, id config.WorkgroupID, at time.Time, actor Actor, cause string) (*Repository, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if at.IsZero() || !validActor(actor) || !present(cause) {
		return nil, fmt.Errorf("timestamp, actor and cause required")
	}
	shared := sharedWorkgroupTraceFor(id)
	shared.Lock()
	defer shared.Unlock()
	if shared.repo != nil {
		return nil, fmt.Errorf("%w: workgroup %s already open", ErrConflict, id)
	}
	project := crossRepoProject(id)
	directory, err := root.WorkgroupTrace(id)
	if err != nil {
		return nil, err
	}
	if _, err := os.Lstat(directory); !os.IsNotExist(err) {
		return nil, fmt.Errorf("%w: workgroup %s already exists or is inaccessible", ErrConflict, id)
	}
	if err := os.MkdirAll(root.String(), 0700); err != nil {
		return nil, err
	}
	rel, err := filepath.Rel(root.String(), directory)
	if err != nil {
		return nil, err
	}
	base, err := os.OpenRoot(root.String())
	if err != nil {
		return nil, err
	}
	err = base.MkdirAll(rel, 0700)
	base.Close()
	if err != nil {
		return nil, err
	}
	r, err := openStoreDirectory(root, project, "", directory)
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			r.Close()
		}
	}()
	if err := acquireWorkgroupTrace(r); err != nil {
		return nil, err
	}
	if err := r.openLocked(true); err != nil {
		return nil, err
	}
	manifest := Header{Schema: "osmia.trace.project", Version: Version, ID: string(project), Revision: 1, Project: project, At: at, Actor: actor, Cause: cause}
	if err := r.createManifest(ctx, "project.json", manifest, nil, "Create workgroup trace"); err != nil {
		return nil, err
	}
	r.shared = shared
	shared.repo, shared.refs = r, 1
	ok = true
	return r, nil
}

// WorkgroupResolutionCurrent reports a resolution observed in this process.
// Unknown groups return false until a resolution pass reads their authority.
// Reopening a group or adding a member invalidates this hint immediately.
func WorkgroupResolutionCurrent(id config.WorkgroupID) bool {
	return sharedWorkgroupTraceFor(id).resolved.Load()
}

func (r *Repository) cacheWorkgroupResolution(id config.WorkgroupID, resolved bool) {
	if r.Project() == crossRepoProject(id) {
		sharedWorkgroupTraceFor(id).resolved.Store(resolved)
	}
}
