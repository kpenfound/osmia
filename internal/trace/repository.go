package trace

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
)

var ErrConflict = errors.New("trace revision or identity conflict")
var ErrLocked = errors.New("trace repository already open")

// Repository is a service-owned trace handle, never a target workspace handle.
// One process holds its exclusive lock until Close. Writes on that handle
// serialize; reads of committed records never wait on a write in progress.
// External writers must not modify or move the trace while it is open.
//
// A Repository value is one trace directory and its database: either the
// project's own trace, holding only durable project knowledge (the charter,
// the knowledge base, notes and lasting project decisions), or one
// workstream's dedicated trace, holding that workstream's complete trace.
// stream is empty for the project's own instance and set to the workstream
// a workstream instance is dedicated to. The project instance alone opens
// and tracks workstream instances, lazily, in workstreams; a workstream
// instance never recurses into another. Every exported method that names a
// workstream resolves to the right instance through scoped before it takes
// any lock, so a call against one workstream never waits on another's lock,
// only ever on the project's own brief bookkeeping and that workstream's own.
type Repository struct {
	mu sync.Mutex
	// Active attempts are admitted under mu and joined before files close.
	attempts map[string]bool
	active   sync.WaitGroup
	closing  bool
	workflow bool
	// members retains fixed member identities and references to project-owned
	// handles. Lifecycle evidence is checked before resolving a member, so an
	// open shared handle never pins a deleted workstream handle as live.
	membersMu              sync.RWMutex
	members                map[config.WorkstreamID]CrossRepoMember
	memberLifecycle        atomic.Pointer[WorkgroupMemberLifecycle]
	questionCommitBoundary atomic.Pointer[func(string) error]
	questionReadBoundary   atomic.Pointer[func(config.WorkstreamID) error]
	// processLock is held, on a cross-repo workgroup's own trace instance,
	// from a successful acquireWorkgroupTrace until Close: it serializes
	// every goroutine of this one process that opens the same workgroup
	// trace, so they hand off its OS-level flock in turn through Go's own
	// scheduler instead of each polling and backing off against the
	// others, which a shared reconciliation cadence could otherwise
	// synchronize into a standing collision. nil on every other instance.
	processLock *sync.Mutex
	// shared is set only on a cross-repo workgroup's own trace instance
	// created or opened through the shared registry: it is the one registry entry
	// every caller that opens the same workgroup id while this instance is
	// still open shares, so they all call methods on this one instance,
	// through its own brief locking, instead of each taking a second
	// exclusive hold on the same directory. Close decrements its reference
	// count and only actually closes this instance once it reaches zero,
	// so a long-running caller such as a dispatched chief-of-staff turn
	// never has to finish before a short-lived caller's own open of the
	// same workgroup succeeds. nil on every other instance, where Close
	// behaves exactly as it does today.
	shared          *sharedWorkgroupTrace
	root            config.Root
	project         config.ProjectID
	stream          config.WorkstreamID
	directory       string
	lock            *os.File
	session         string
	wake            *coreadapter.WakeAdapter
	failPublication func(string) error
	observer        atomic.Pointer[func(Commit)]
	// db is the write connection and reads the read-only pool of the
	// trace's database.
	db    *sql.DB
	reads *sql.DB
	// cache holds the trace's decoded records under its own lock.
	cache recordCache
	// decoded counts the stored records this handle decoded.
	decoded atomic.Uint64
	// parent is the project instance that opened this workstream instance
	// through scoped, nil on the project's own instance. observed delegates
	// to it so that Observe, called once on the project instance, also
	// reports commits a workstream instance makes, however and whenever that
	// instance was opened.
	parent *Repository
	// workstreamsMu guards workstreams and closed, the project instance's
	// lazily opened workstream instances, by ID, and whether this instance's
	// Close has begun. Unused on a workstream instance.
	workstreamsMu sync.Mutex
	workstreams   map[config.WorkstreamID]*Repository
	// snapshotMu excludes trace deletion while a short discovery snapshot is
	// assembled. Operation admission and results proceed independently of it.
	snapshotMu sync.RWMutex
	// closed is set under workstreamsMu once Close has begun, so a scoped
	// call racing it never opens a child Close could miss.
	closed bool
	// generation counts the commits that landed in this trace.
	generation atomic.Uint64
	// workflows holds each workstream's committed workflow, under mu.
	workflows map[config.WorkstreamID]*cachedWorkflow
}

func location(root config.Root, project config.Project) (string, error) {
	directory, err := root.ProjectTrace(project.ID)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(project.Clone) {
		return "", fmt.Errorf("target clone must be an absolute configured path")
	}
	clone, err := config.ResolveRoot(project.Clone, "")
	if err != nil {
		return "", fmt.Errorf("target clone: %w", err)
	}
	contains := func(a, b string) bool {
		rel, err := filepath.Rel(a, b)
		return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
	}
	if contains(root.String(), clone.String()) || contains(clone.String(), root.String()) {
		return "", fmt.Errorf("target clone and Osmia root must be separate, non-nested directories")
	}
	return directory, nil
}
func openDirectory(root config.Root, project config.Project) (*Repository, error) {
	directory, err := location(root, project)
	if err != nil {
		return nil, err
	}
	r, err := openStoreDirectory(root, project.ID, "", directory)
	return r, err
}

// openWorkstreamDirectory opens stream's own dedicated trace directory,
// under the Osmia root and outside every target repository.
func openWorkstreamDirectory(root config.Root, project config.ProjectID, stream config.WorkstreamID) (*Repository, error) {
	directory, err := root.WorkstreamTrace(project, stream)
	if err != nil {
		return nil, err
	}
	return openStoreDirectory(root, project, stream, directory)
}

func openStoreDirectory(root config.Root, project config.ProjectID, stream config.WorkstreamID, directory string) (*Repository, error) {
	if _, err := filepath.Rel(root.String(), directory); err != nil {
		return nil, err
	}
	info, err := os.Lstat(directory)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s: trace must be a directory", directory)
	}
	return &Repository{root: root, project: project, stream: stream, directory: directory, session: rand.Text(), wake: coreadapter.NewWakeups()}, nil
}

// scoped returns the Repository instance that owns the files of the given
// workstream of a project's own trace instance, opening and recovering its
// dedicated trace on this handle's first use of it. An empty id
// returns r itself: the project's own instance. Calling scoped on a
// workstream instance, or with a workstream not already known to this
// project instance, is a programming error the caller must rule out first
// (for example by checking Workstreams or an earlier scan); it refuses
// rather than silently creating a workstream's trace. This call only briefly
// takes r's own bookkeeping lock and, the first time, the opened
// workstream's lock: it never waits on another workstream's lock, and a
// later use of the same id reuses the open instance without taking r's lock
// again. It refuses once r's Close has begun, so it never opens a child that
// Close could miss and leave with its flock held past this process's
// lifetime.
//
// On a cross-repo workgroup's own trace instance, id naming one of its
// real members (a key of r.members) returns that member's own, already
// open instance directly, never opening or locking a second handle on it;
// id naming anything else, including the trace's own host workstream, is
// resolved the ordinary way, inside the workgroup trace itself.
func (r *Repository) scoped(id config.WorkstreamID) (*Repository, error) {
	if id == "" || id == r.stream {
		return r, nil
	}
	if m, ok := r.memberBinding(id); ok {
		retired, err := r.MemberRetired(id)
		if err != nil {
			return nil, err
		}
		if retired {
			return nil, &RetiredMemberError{Project: m.Project.Project(), Workstream: id}
		}
		member, liveErr := liveMemberRepository(m)
		if liveErr != nil {
			retired, err := r.MemberRetired(id)
			if err != nil {
				return nil, err
			}
			if retired {
				return nil, &RetiredMemberError{Project: m.Project.Project(), Workstream: id}
			}
		}
		return member, liveErr
	}
	if r.stream != "" {
		return nil, fmt.Errorf("a workstream trace instance has no workstreams of its own")
	}
	if err := config.CheckWorkstreamIDs(id); err != nil {
		return nil, err
	}
	r.workstreamsMu.Lock()
	defer r.workstreamsMu.Unlock()
	if r.closed {
		return nil, fmt.Errorf("%w: trace repository is closed", ErrLocked)
	}
	if s := r.workstreams[id]; s != nil {
		return s, nil
	}
	s, err := openWorkstreamDirectory(r.root, r.project, id)
	if err != nil {
		return nil, err
	}
	// Every workstream shares the project instance's wake signal, so a wait
	// on the project's WaitWorkflow wakes for any workstream's durable write,
	// not only the project's own.
	s.wake = r.wake
	s.parent = r
	ok := false
	defer func() {
		if !ok {
			// The instance was never published and has no attempts to join.
			s.closeFiles()
		}
	}()
	if err := s.openDatabase(false); err != nil {
		return nil, err
	}
	if r.workstreams == nil {
		r.workstreams = map[config.WorkstreamID]*Repository{}
	}
	r.workstreams[id] = s
	ok = true
	return s, nil
}

// ProjectFor resolves the repo handle holding stream's knowledge base and
// entity map and charter. r must be unscoped. Ordinary repo receivers return
// themselves; a cross-repo workgroup returns the member's borrowed repo
// handle, even if its workstream history is retired. The shared chief host
// uses the first live member's repo context. Project data is never duplicated
// into the workgroup trace.
func (r *Repository) ProjectFor(stream config.WorkstreamID) (*Repository, error) {
	if r.stream != "" {
		return nil, fmt.Errorf("a workstream trace instance has no project-level data of its own")
	}
	m, ok := r.memberBinding(stream)
	if !ok {
		if r.memberBindings() == nil {
			return r, nil
		}
		groups, _, err := Workgroups(r)
		if err != nil {
			return nil, err
		}
		group, found := WorkgroupOf(groups, stream)
		if !found {
			return nil, fmt.Errorf("workstream %s has no workgroup project context", stream)
		}
		for _, id := range group.Members {
			member, bound := r.memberBinding(id)
			if !bound {
				continue
			}
			retired, err := r.MemberRetired(id)
			if err != nil {
				return nil, err
			}
			if !retired {
				return member.Project, nil
			}
		}
		return nil, fmt.Errorf("workgroup %s has no live repo context", group.ID)
	}
	if _, err := r.MemberRetired(stream); err != nil {
		return nil, err
	}
	return m.Project, nil
}

// diskStreams lists every workstream that has its own trace directory under
// the project's workstream traces, the way Workstreams and every aggregate
// read discover the project's workstreams: from the storage that is
// authoritative for their existence, not from anything the project trace
// itself records. On a cross-repo workgroup's own trace instance it
// instead lists live real members. Retirement needs durable lifecycle
// evidence; historical membership stays in Workgroups. The host carrying
// the shared chief thread is never a real member.
func (r *Repository) diskStreams() ([]config.WorkstreamID, error) {
	if members := r.memberBindings(); members != nil {
		ids := make([]config.WorkstreamID, 0, len(members))
		for id := range members {
			retired, err := r.MemberRetired(id)
			if err != nil {
				return nil, err
			}
			if !retired {
				ids = append(ids, id)
			}
		}
		slices.Sort(ids)
		return ids, nil
	}
	base, err := r.root.WorkstreamTraces(r.project)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(base)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ids []config.WorkstreamID
	for _, entry := range entries {
		id, err := config.ParseWorkstreamID(entry.Name())
		if err != nil || !entry.IsDir() {
			return nil, fmt.Errorf("%s: not a workstream trace directory", entry.Name())
		}
		pending, err := SessionDestroyPending(r.root, r.project, id)
		if err != nil {
			return nil, err
		}
		if !pending {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// scanAll returns every record of the project trace and of every workstream
// that has its own trace directory, and those workstreams, as scan does for
// one store. Each workstream's own scan is cached by its own instance and
// taken under its own lock alone, so one slow or damaged workstream's scan
// does not hold the project's lock any longer than copying its result, and
// never holds another workstream's lock at all.
func (r *Repository) scanAll() ([]Record, []config.WorkstreamID, error) {
	r.mu.Lock()
	records, _, err := r.scan()
	r.mu.Unlock()
	if err != nil {
		return nil, nil, err
	}
	ids, err := r.diskStreams()
	if err != nil {
		return nil, nil, err
	}
	streams := make([]config.WorkstreamID, 0, len(ids))
	for _, id := range ids {
		s, err := r.scoped(id)
		if err != nil {
			retired, checkedErr := r.retiredRead(id, err)
			if retired {
				continue
			}
			err = checkedErr
		}
		if err != nil {
			return nil, nil, err
		}
		s.mu.Lock()
		retired, lifecycleErr := r.MemberRetired(id)
		if retired || lifecycleErr != nil {
			s.mu.Unlock()
			if lifecycleErr != nil {
				return nil, nil, lifecycleErr
			}
			continue
		}
		recs, _, err := s.scan()
		s.mu.Unlock()
		if err != nil {
			return nil, nil, fmt.Errorf("workstream %s: %w", id, err)
		}
		records = append(records, recs...)
		streams = append(streams, id)
	}
	return records, streams, nil
}

// Observe calls f after each commit this handle publishes, replacing any
// earlier observer. f runs while the handle's lock is held, so it must return
// promptly and must not call the repository.
func (r *Repository) Observe(f func(Commit)) {
	r.observer.Store(&f)
}

// observed passes a published commit to the observer, if any, and to the
// project instance's observer when this is a workstream instance: Observe is
// called once, on the project instance, and must still hear every workstream
// instance's commits, however and whenever each was opened.
func (r *Repository) observed(c Commit) {
	if f := r.observer.Load(); f != nil && *f != nil {
		(*f)(c)
	}
	if r.parent != nil {
		r.parent.observed(c)
	}
}

// Close closes this instance and, on the project's own instance, every
// workstream instance it opened. Setting closed under the same lock that
// captures the children to close means a scoped call that arrives after
// this point finds no workstream to reuse and opens none of its own, so
// Close never misses a child and leaks its flock past this process's
// lifetime.
//
// On a cross-repo workgroup's own trace instance created or opened through
// the shared registry, Close instead decrements the shared reference
// count it was opened under and returns at once while another caller
// still holds it: the instance, and every workstream it has opened, stays
// exactly as open and usable as it was, and only the call that brings the
// count to zero runs the rest of this method.
func (r *Repository) Close() error {
	if r.shared != nil {
		r.shared.Lock()
		r.shared.refs--
		if r.shared.refs > 0 {
			r.shared.Unlock()
			return nil
		}
		r.shared.repo = nil
		r.shared.Unlock()
	}
	r.workstreamsMu.Lock()
	children := r.workstreams
	r.workstreams = nil
	r.closed = true
	r.workstreamsMu.Unlock()
	var errs []error
	for _, s := range children {
		errs = append(errs, s.Close())
	}
	r.mu.Lock()
	r.closing = true
	r.mu.Unlock()
	r.active.Wait()
	errs = append(errs, r.closeFiles())
	return errors.Join(errs...)
}

// closeFiles releases this instance's flock and database after attempts join,
// or on the failure path of an instance that has not been published.
func (r *Repository) closeFiles() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cache.mu.Lock()
	defer r.cache.mu.Unlock()
	var errs []error
	errs = append(errs, r.closeDatabases())
	if r.lock != nil {
		// A child process forked but not yet executed shares the open file,
		// so closing alone would leave the lock held until it executes.
		errs = append(errs, syscall.Flock(int(r.lock.Fd()), syscall.LOCK_UN), r.lock.Close())
		r.lock = nil
	}
	if r.processLock != nil {
		r.processLock.Unlock()
		r.processLock = nil
	}
	r.resetCache()
	r.workflows = nil
	return errors.Join(errs...)
}
func (r *Repository) Project() config.ProjectID { return r.project }

// CharterTemplate is the initial charter.md content of a new project trace,
// recorded as the charter's first revision. It holds guidance only and no
// rules, so a new charter is empty until the owner writes one.
const CharterTemplate = `# Charter

<!--
This charter holds your rules as a contributor to this project. Plans, reviews
and the committee cite them as charter#<n>.

A rule is a numbered Markdown list item, "1. Rule text", under any heading.
Rules are cited by the number you write, counted across the whole document, so
keep each number unique and never reuse a retired one. Headings, plain text and
comments like this one are guidance, not rules. Osmia refuses hand-in while the
charter has no rules.

Budgets, capacity, models and other factory settings belong in configuration,
not here.
-->

The repository's own contributor documents (CONTRIBUTING, AGENTS.md, CLAUDE.md
and similar) are binding. Rules here add to them.

## Scope

<!-- What changes belong in this project, and what must be proposed upstream
first or not at all. -->

## Dependencies

<!-- When a new dependency is acceptable, which ones are preferred or banned,
and how versions are pinned. -->

## Testing

<!-- How a change must be tested: which suites run, what a new test must show,
and which checks must pass before a pull request. -->

## Pull requests

<!-- The shape of a pull request: size, commit structure, description,
changelog and sign-off. -->
`

// EntitiesPath is the project document holding the local entity map, and
// EntitiesDocument the record ID of its revisions.
const (
	EntitiesPath     = "kb/entities.json"
	EntitiesDocument = "kb-entities"
)

// ownerActor records the owner's edits of project and workstream documents.
var ownerActor = Actor{Kind: "owner", ID: "local"}

func charterRevision(project config.ProjectID, at time.Time, actor Actor, cause string, revision int, content string) Document {
	return Document{Header: Header{Schema: "osmia.trace.document", Version: Version, ID: "charter", Revision: revision, Project: project, At: at, Actor: actor, Cause: cause}, Path: "charter.md", Content: content}
}

// Charter returns the latest recorded charter revision. Creation records the
// template as the first revision on each project.
// A charter with no recorded revision is fs.ErrNotExist.
func (r *Repository) Charter(ctx context.Context) (Document, error) {
	r, err := r.CharterRepository()
	if err != nil {
		return Document{}, err
	}
	if err := ctx.Err(); err != nil {
		return Document{}, err
	}
	records, err := r.records(func(v Record) bool {
		d, ok := v.(Document)
		return ok && d.Workstream == "" && d.Path == "charter.md"
	})
	if err != nil {
		return Document{}, err
	}
	latest, ok := latestCharter(records)
	if !ok {
		return Document{}, fmt.Errorf("charter.md: %w", fs.ErrNotExist)
	}
	return latest, nil
}

func latestCharter(records []Record) (Document, bool) {
	var latest Document
	found := false
	for _, v := range records {
		if d, ok := v.(Document); ok && d.Workstream == "" && d.Path == "charter.md" {
			latest, found = d, true
		}
	}
	return latest, found
}

// Create initializes a project's trace database in an existing configuration
// directory or a new project directory. It refuses any existing trace.
func Create(ctx context.Context, root config.Root, project config.Project, at time.Time, actor Actor) (*Repository, error) {
	return createTrace(ctx, root, project, at, actor, nil)
}

// CreateSeeded is Create with entities, already validated by the caller, as
// revision 1 of the entity map document, committed with the rest of the trace.
func CreateSeeded(ctx context.Context, root config.Root, project config.Project, at time.Time, actor Actor, entities []byte) (*Repository, error) {
	if len(entities) == 0 {
		return nil, fmt.Errorf("entity map seed required")
	}
	return createTrace(ctx, root, project, at, actor, entities)
}

func createTrace(ctx context.Context, root config.Root, project config.Project, at time.Time, actor Actor, entities []byte) (*Repository, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if at.IsZero() || !validActor(actor) {
		return nil, fmt.Errorf("timestamp and actor required")
	}
	directory, err := location(root, project)
	if err != nil {
		return nil, err
	}
	var records []Record
	records = append(records, charterRevision(project.ID, at, actor, "project-create", 1, CharterTemplate))
	if entities != nil {
		d := Document{Header: Header{Schema: "osmia.trace.document", Version: Version, ID: EntitiesDocument, Revision: 1, Project: project.ID, At: at, Actor: actor, Cause: "project-create"}, Path: EntitiesPath, Content: string(entities)}
		if err := validate(d); err != nil {
			return nil, err
		}
		records = append(records, d)
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
	// Only the project's existing configuration may precede trace creation.
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.Name() != "config.toml" {
			return nil, fmt.Errorf("%w: %s already contains %s", ErrConflict, directory, e.Name())
		}
	}
	r, err := openDirectory(root, project)
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			r.Close()
		}
	}()
	if err := r.openDatabase(true); err != nil {
		return nil, err
	}
	manifest := Header{Schema: "osmia.trace.project", Version: Version, ID: string(project.ID), Revision: 1, Project: project.ID, At: at, Actor: actor, Cause: "project-create"}
	if err := r.createManifest(ctx, "project.json", manifest, records, "Create project trace"); err != nil {
		return nil, err
	}
	ok = true
	return r, nil
}

// createManifest requires a new, empty trace. It commits the trace's
// manifest, m under header h, at name together with records.
func (r *Repository) createManifest(ctx context.Context, name string, m any, records []Record, message string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	var one int
	if err := r.db.QueryRowContext(ctx, "SELECT 1 FROM files WHERE path = ?", name).Scan(&one); err == nil {
		return fmt.Errorf("%w: %s already holds a trace", ErrConflict, r.directory)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	c := change{records: records, message: message}
	c.setFile(name, append(data, '\n'))
	return r.publish(ctx, c)
}

// Open opens the project's trace and every workstream that has its own
// trace directory, checking each database's integrity. If records are corrupt
// it returns both a handle and a diagnostic error; callers must Close that
// handle. Read returns valid records with corruption diagnostics.
func Open(root config.Root, project config.Project) (*Repository, error) {
	r, err := openDirectory(root, project)
	if err != nil {
		return nil, err
	}
	if err := r.openDatabase(false); err != nil {
		r.Close()
		return nil, err
	}
	if _, _, err := r.scan(); err != nil {
		return r, err
	}
	ids, err := r.diskStreams()
	if err != nil {
		return r, err
	}
	var errs []error
	for _, id := range ids {
		s, err := r.scoped(id)
		if err != nil {
			errs = append(errs, fmt.Errorf("workstream %s: %w", id, err))
			continue
		}
		s.mu.Lock()
		_, streams, err := s.scan()
		if err == nil {
			err = s.checkWorkflows(streams)
		}
		s.mu.Unlock()
		if err != nil {
			errs = append(errs, fmt.Errorf("workstream %s: %w", id, err))
		}
	}
	return r, errors.Join(errs...)
}

// SessionKind is the Kind a session workstream's manifest records. An
// ordinary workstream's Kind is empty.
const SessionKind = "session"

// manifestRecord is a project or workstream manifest. Workspaces is the
// workspace backend a workstream was created on; it is empty in the project
// manifest and in a workstream manifest that records none. Kind, Profile,
// Base, Branch and Title are set only for a session workstream: its kind,
// its fixed profile, the base commit it opened from, its branch and its
// optional title.
type manifestRecord struct {
	Header
	Workspaces string `json:"workspaces,omitempty"`
	Kind       string `json:"kind,omitempty"`
	Profile    string `json:"profile,omitempty"`
	Base       string `json:"base,omitempty"`
	Branch     string `json:"branch,omitempty"`
	Title      string `json:"title,omitempty"`
}

func (r *Repository) readManifest(name, schema string, stream config.WorkstreamID) (manifestRecord, error) {
	data, err := r.file(name)
	if err != nil {
		return manifestRecord{}, err
	}
	return r.decodeManifest(name, data, schema, stream)
}

func (r *Repository) decodeManifest(name string, data []byte, schema string, stream config.WorkstreamID) (manifestRecord, error) {
	var m manifestRecord
	if err := decode(data, &m); err != nil {
		return manifestRecord{}, fmt.Errorf("%s: %w", name, err)
	}
	h := m.Header
	id := string(r.project)
	if stream != "" {
		id = string(stream)
	}
	if h.Schema != schema || h.Version != Version || h.Project != r.project || h.Workstream != stream || h.ID != id || h.Revision != 1 || h.At.IsZero() || !validActor(h.Actor) || !present(h.Cause) || h.Depth < 0 {
		return manifestRecord{}, fmt.Errorf("%s: invalid manifest schema, identity or provenance", name)
	}
	if !validWorkspaces(m.Workspaces) || stream == "" && m.Workspaces != "" {
		return manifestRecord{}, fmt.Errorf("%s: invalid workspace backend %q", name, m.Workspaces)
	}
	if err := validSessionFields(stream, m); err != nil {
		return manifestRecord{}, fmt.Errorf("%s: %w", name, err)
	}
	return m, nil
}

// validWorkspaces reports whether a workstream manifest may record the
// backend: Git, Jujutsu or none.
func validWorkspaces(backend string) bool {
	return backend == "" || backend == config.WorkspacesGit || backend == config.WorkspacesJujutsu
}

// validSessionFields checks a manifest's Kind, Profile, Base, Branch and
// Title against each other: the project manifest and an ordinary
// workstream's carry none of them; a session workstream's Kind is
// SessionKind with a non-empty profile, base commit and branch.
func validSessionFields(stream config.WorkstreamID, m manifestRecord) error {
	if stream == "" || m.Kind == "" {
		if m.Kind != "" || m.Profile != "" || m.Base != "" || m.Branch != "" || m.Title != "" {
			return fmt.Errorf("only a session workstream may record kind, profile, base, branch or title")
		}
		return nil
	}
	if m.Kind != SessionKind {
		return fmt.Errorf("unknown workstream kind %q", m.Kind)
	}
	if !key(m.Profile) || !present(m.Base) || !present(m.Branch) {
		return fmt.Errorf("a session requires a profile, base commit and branch")
	}
	return nil
}

// Workspaces returns the workspace backend the workstream was created on,
// config.WorkspacesGit or config.WorkspacesJujutsu. A workstream whose
// manifest records none is on Git.
func (r *Repository) Workspaces(stream config.WorkstreamID) (string, error) {
	if err := config.CheckWorkstreamIDs(stream); err != nil {
		return "", err
	}
	r, err := r.scoped(stream)
	if err != nil {
		return "", err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.workspaces(stream)
}

func (r *Repository) workspaces(stream config.WorkstreamID) (string, error) {
	m, err := r.workstreamManifest(stream)
	if err != nil {
		return "", err
	}
	if m.Workspaces == "" {
		return config.WorkspacesGit, nil
	}
	return m.Workspaces, nil
}

// Kind returns a workstream's kind: SessionKind for a session, or "" for an
// ordinary workstream.
func (r *Repository) Kind(stream config.WorkstreamID) (string, error) {
	if err := config.CheckWorkstreamIDs(stream); err != nil {
		return "", err
	}
	r, err := r.scoped(stream)
	if err != nil {
		return "", err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	m, err := r.workstreamManifest(stream)
	if err != nil {
		return "", err
	}
	return m.Kind, nil
}

// workstreamManifest reads a workstream's creation manifest, whose Header.At
// is the workstream's creation time.
func (r *Repository) workstreamManifest(stream config.WorkstreamID) (manifestRecord, error) {
	name := "workstreams/" + string(stream) + "/workstream.json"
	return r.readManifest(name, "osmia.trace.workstream", stream)
}

// CreateWorkstream commits a new workstream that records no workspace
// backend, as CreateWorkstreamOn does.
func (r *Repository) CreateWorkstream(ctx context.Context, id config.WorkstreamID, at time.Time, actor Actor) error {
	return r.CreateWorkstreamOn(ctx, id, "", at, actor)
}

// CreateWorkstreamOn commits a new workstream in its own dedicated trace
// repository, under the Osmia root and outside every target repository,
// whose manifest records the workspace backend it is created on,
// config.WorkspacesGit, config.WorkspacesJujutsu or none, and then creates
// its chief-of-staff thread. An error from the thread write leaves the
// workstream committed; EnsureChiefOfStaff creates the missing thread. An
// existing workstream is refused with ErrConflict.
func (r *Repository) CreateWorkstreamOn(ctx context.Context, id config.WorkstreamID, workspaces string, at time.Time, actor Actor) error {
	if r.stream != "" {
		return fmt.Errorf("a workstream trace instance creates no workstreams of its own")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := createWorkstreamTrace(ctx, r.root, r.project, id, manifestRecord{Workspaces: workspaces}, "workstream-create", "Create workstream trace", at, actor); err != nil {
		return err
	}
	s, err := r.scoped(id)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err = s.ensureChiefOfStaff(ctx, id, at, actor)
	return err
}

// createWorkstreamTrace creates and commits id's own dedicated trace, whose
// manifest is m under a header recording cause. It refuses an existing
// directory with ErrConflict.
//
// The trace is built under a staging directory outside WorkstreamTraces and
// only moved into its final place, by one atomic rename, once its database
// holds the manifest and is closed. A directory listing of WorkstreamTraces
// (diskStreams, which every concurrent read and background pass uses to
// discover a project's workstreams) therefore never observes the workstream
// before its trace is complete.
func createWorkstreamTrace(ctx context.Context, root config.Root, project config.ProjectID, id config.WorkstreamID, m manifestRecord, cause, message string, at time.Time, actor Actor) error {
	if err := config.CheckWorkstreamIDs(id); err != nil {
		return err
	}
	if pending, err := SessionDestroyPending(root, project, id); err != nil {
		return err
	} else if pending {
		return fmt.Errorf("%w: session is being destroyed", os.ErrNotExist)
	}

	if !validWorkspaces(m.Workspaces) {
		return fmt.Errorf("unknown workspace backend %q", m.Workspaces)
	}
	if at.IsZero() || !validActor(actor) {
		return fmt.Errorf("timestamp and actor required")
	}
	directory, err := root.WorkstreamTrace(project, id)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(directory); !os.IsNotExist(err) {
		return fmt.Errorf("%w: workstream %s already exists or is inaccessible", ErrConflict, id)
	}
	base, err := root.WorkstreamTraces(project)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(base, 0700); err != nil {
		return err
	}
	staging, err := root.WorkstreamStaging()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(staging, 0700); err != nil {
		return err
	}
	build, err := os.MkdirTemp(staging, "ws-"+string(id)+"-")
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		if !ok {
			os.RemoveAll(build)
		}
	}()
	s, err := openStoreDirectory(root, project, id, build)
	if err != nil {
		return err
	}
	if err := func() error {
		defer s.Close()
		if err := s.openDatabase(true); err != nil {
			return err
		}
		m.Header = Header{Schema: "osmia.trace.workstream", Version: Version, ID: string(id), Revision: 1, Project: project, Workstream: id, At: at, Actor: actor, Cause: cause}
		return s.createManifest(ctx, "workstreams/"+string(id)+"/workstream.json", m, nil, message)
	}(); err != nil {
		return err
	}
	if err := syncDirectory(build); err != nil {
		return err
	}
	if err := os.Rename(build, directory); err != nil {
		if _, statErr := os.Lstat(directory); statErr == nil {
			return fmt.Errorf("%w: workstream %s already exists or is inaccessible", ErrConflict, id)
		}
		return err
	}
	if err := syncDirectory(base); err != nil {
		return err
	}
	ok = true
	return nil
}

// CreateSessionWorkstream commits a new session workstream in its own
// dedicated trace: its kind, its fixed profile, the base commit it
// opened from, its branch and its optional title. It creates no
// chief-of-staff thread: a session has none. An existing workstream is
// refused with ErrConflict.
func (r *Repository) CreateSessionWorkstream(ctx context.Context, id config.WorkstreamID, profile, base, branch, title string, at time.Time, actor Actor) error {
	if r.stream != "" {
		return fmt.Errorf("a workstream trace instance creates no workstreams of its own")
	}
	if !key(profile) || !present(base) || !present(branch) {
		return fmt.Errorf("a session requires a profile, base commit and branch")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	m := manifestRecord{Kind: SessionKind, Profile: profile, Base: base, Branch: branch, Title: title}
	return createWorkstreamTrace(ctx, r.root, r.project, id, m, "session-open", "Create session workstream", at, actor)
}

// Workstreams lists every workstream that has its own trace directory under
// the project's trace, which is authoritative for their existence.
func (r *Repository) Workstreams() ([]config.WorkstreamID, error) {
	return r.diskStreams()
}

// WithWorkstreamSnapshot protects a short scan of workstreams and their trace
// records against trace deletion. The callback only reads bookkeeping state;
// it must not delete traces or run scheduling, model, Git or network work.
// Operation claims and result writes proceed independently of this guard.
func (r *Repository) WithWorkstreamSnapshot(fn func() error) error {
	r.snapshotMu.RLock()
	defer r.snapshotMu.RUnlock()
	return fn()
}

// Generation reports how many commits landed in the trace through this
// handle. It never decreases while the handle is open, and it is
// unchanged exactly when no write landed between two calls: a caller that
// assembles a response from several separate reads can call it before and
// after and retry the assembly when it differs, so the response never mixes
// state from before a commit with state from after it. It never blocks on
// mu, so it is safe to call around a read held up by a long-running write.
// On the project's own instance it sums its own generation with that of
// every workstream instance opened so far, so it also advances when a
// workstream's trace changes; a workstream not yet opened by this handle
// has contributed no writes of its own to observe.
func (r *Repository) Generation() uint64 {
	total := r.generation.Load()
	r.workstreamsMu.Lock()
	children := make([]*Repository, 0, len(r.workstreams))
	for _, s := range r.workstreams {
		children = append(children, s)
	}
	r.workstreamsMu.Unlock()
	for _, s := range children {
		total += s.generation.Load()
	}
	return total
}
func recordKey(v Record) string {
	h := v.header()
	return string(h.Workstream) + "/" + kind(v) + "/" + h.ID
}
func revision(previous, next Record) error {
	h := next.header()
	if previous == nil {
		if h.Revision != 1 {
			return fmt.Errorf("%w: first revision must be 1", ErrConflict)
		}
		return nil
	}
	if h.Revision != previous.header().Revision+1 || recordPath(next) != recordPath(previous) {
		return fmt.Errorf("%w: revisions must be consecutive with stable identity", ErrConflict)
	}
	switch v := next.(type) {
	case Document:
		if v.Path != previous.(Document).Path || strings.HasPrefix(v.Path, "handed/") {
			return fmt.Errorf("%w: document path is stable and handed input is immutable", ErrConflict)
		}
	case Agent:
		p := previous.(Agent)
		if v.ThreadID != p.ThreadID || v.Role != p.Role {
			return fmt.Errorf("%w: agent thread and role are stable", ErrConflict)
		}
	}
	return nil
}

// Append records one revision as its own commit. A project charter revision
// belongs to the charter's project. Status and priority records are refused
// with ErrConflict: SetStatus and SetPriority are their only writers.
func (r *Repository) Append(ctx context.Context, v Record) error {
	switch v.(type) {
	case Status:
		return fmt.Errorf("%w: status records require SetStatus", ErrConflict)
	case PriorityChange:
		return fmt.Errorf("%w: priority records require SetPriority", ErrConflict)
	}
	r, err := r.scoped(v.header().Workstream)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.append(ctx, v)
}

// append requires r.mu. It checks v against the committed records of its
// identity and path alone, and commits it.
func (r *Repository) append(ctx context.Context, v Record) error {
	if err := validate(v); err != nil {
		return err
	}
	if v.header().Project != r.project {
		return fmt.Errorf("record belongs to a different project")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if id := v.header().Workstream; id != "" && id != r.stream {
		return fmt.Errorf("unknown workstream %s", id)
	}
	var agentID string
	switch rec := v.(type) {
	case Agent:
		agentID = rec.ID
	case TurnRequest:
		agentID = rec.AgentID
	case TurnResponse:
		agentID = rec.AgentID
	}
	if agentID != "" {
		log, _, err := r.loadWorkflow(v.header().Workstream)
		if err != nil {
			return err
		}
		if _, managed := log.Threads[agentID]; managed {
			return fmt.Errorf("%w: thread records require queue transactions", ErrConflict)
		}
	}
	if t, ok := v.(Transition); ok {
		_, view, err := r.loadWorkflow(t.Workstream)
		if err != nil {
			return err
		}
		if _, managed := view.transactions[t.ID]; managed {
			return fmt.Errorf("%w: workflow transitions are immutable", ErrConflict)
		}
	}
	if err := r.checkRevisions(ctx, []Record{v}); err != nil {
		return err
	}
	return r.publish(ctx, change{records: []Record{v}, message: fmt.Sprintf("Record %s %s revision %d", kind(v), v.header().ID, v.header().Revision)})
}

// checkRevisions requires r.mu. It checks that each of records, in order,
// is the next revision of its record with a stable identity, and that a
// document's path has no other identity, against the committed records of
// those identities and paths alone.
func (r *Repository) checkRevisions(ctx context.Context, records []Record) error {
	pending := map[string]Record{}
	paths := map[string]string{}
	for _, v := range records {
		k := recordKey(v)
		previous, ok := pending[k]
		if !ok {
			var err error
			if previous, err = r.latestRevision(ctx, r.db, v); err != nil {
				return err
			}
		}
		if err := revision(previous, v); err != nil {
			return err
		}
		if d, ok := v.(Document); ok {
			p := scopePath(d.Header) + d.Path
			id, known := paths[p]
			if !known {
				var err error
				if id, known, err = documentIdentity(ctx, r.db, d.Workstream, d.Path); err != nil {
					return err
				}
			}
			if known && id != d.ID {
				return fmt.Errorf("%w: document path already has an identity", ErrConflict)
			}
			paths[p] = d.ID
		}
		pending[k] = v
	}
	return nil
}

// Read returns all valid revisions of T in seq order, with diagnostics naming
// the path and row of any damaged record. Callers must not ignore an
// accompanying error. It never waits on a write in progress.
func Read[T Record](r *Repository, stream config.WorkstreamID) ([]T, error) {
	if stream != "" {
		if err := config.CheckWorkstreamIDs(stream); err != nil {
			return nil, err
		}
	}
	target, err := r.questionRead(stream)
	if err != nil {
		return nil, err
	}
	records, err := target.records(func(v Record) bool {
		_, ok := v.(T)
		return ok && v.header().Workstream == stream
	})
	if stream != "" && target.stream != stream {
		err = errors.Join(err, fmt.Errorf("unknown workstream %s", stream))
	}
	result := make([]T, 0, len(records))
	for _, v := range records {
		result = append(result, v.(T))
	}
	if len(result) == 0 {
		result = nil
	}
	return result, err
}
func Get[T Record](r *Repository, stream config.WorkstreamID, id string, revision int) (T, error) {
	var zero T
	if !key(id) || revision < 1 {
		return zero, fmt.Errorf("invalid record identity or revision")
	}
	records, err := Read[T](r, stream)
	for _, v := range records {
		if v.header().ID == id && v.header().Revision == revision {
			return v, err
		}
	}
	return zero, errors.Join(err, fs.ErrNotExist)
}
