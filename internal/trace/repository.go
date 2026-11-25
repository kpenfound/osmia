package trace

import (
	"bytes"
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
	"github.com/kpenfound/osmia/internal/kinds"
	"github.com/kpenfound/osmia/internal/coreadapter"
)

var ErrConflict = errors.New("trace revision or identity conflict")
var ErrLocked = errors.New("trace repository already open")

// Repository is a service-owned trace handle, never a target workspace handle.
// One process holds its exclusive lock until Close. Writes on that handle
// serialize; reads of committed records never wait on a write in progress.
// External writers must not modify or move the trace while it is open.
//
// A Repository is either the project's own trace, holding durable project
// knowledge (the charter, the entity map and lasting project decisions), or
// one workstream's dedicated trace. A call that names a workstream resolves
// to that workstream's trace, so it never waits on another workstream's lock.
type Repository struct {
	mu                     sync.Mutex
	attempts               map[string]bool
	active                 sync.WaitGroup
	closing                bool
	workflow               bool
	membersMu              sync.RWMutex
	members                map[config.WorkstreamID]CrossRepoMember
	memberLifecycle        atomic.Pointer[WorkgroupMemberLifecycle]
	questionCommitBoundary atomic.Pointer[func(string) error]
	questionReadBoundary   atomic.Pointer[func(config.WorkstreamID) error]
	processLock            *sync.Mutex
	shared                 *sharedWorkgroupTrace
	root                   config.Root
	project                config.ProjectID
	stream                 config.WorkstreamID
	directory              string
	lock                   *os.File
	session                string
	wake                   *coreadapter.WakeAdapter
	failPublication        func(string) error
	observer               atomic.Pointer[func(Commit)]
	spanObserver           atomic.Pointer[func(CommitDetail)]
	db                     *sql.DB
	reads                  *sql.DB
	cache                  recordCache
	createdMu              sync.Mutex
	created                map[config.WorkstreamID]time.Time
	manifestsMu            sync.Mutex
	manifests              map[string]checkedManifest
	manifestDecodes        atomic.Uint64
	decoded                atomic.Uint64
	workgroupReads         atomic.Uint64
	groupTracesMu          sync.Mutex
	groupTraces            map[config.WorkgroupID]*Repository
	groupTracesClosed      bool
	parent                 *Repository
	held                   atomic.Pointer[func(string) bool]
	workstreamsMu          sync.Mutex
	workstreams            map[config.WorkstreamID]*Repository
	snapshotMu             sync.RWMutex
	closed                 bool
	generation             atomic.Uint64
	workflows              map[config.WorkstreamID]*cachedWorkflow
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
	s.wake = r.wake
	s.parent = r
	ok := false
	defer func() {
		if !ok {
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

func (r *Repository) observed(c Commit) {
	if f := r.observer.Load(); f != nil && *f != nil {
		(*f)(c)
	}
	if r.parent != nil {
		r.parent.observed(c)
	}
}

// Close closes this instance and, on the project's own instance, every
// workstream instance it opened.
//
// On a cross-repo workgroup's own trace instance opened through the shared
// registry, Close instead decrements the shared reference count and returns
// at once while another caller still holds it; only the call that brings the
// count to zero closes the trace.
func (r *Repository) Close() error {
	if r.shared != nil {
		r.shared.Lock()
		r.shared.refs--
		if r.shared.refs > 0 {
			r.shared.Unlock()
			return nil
		}
		r.shared.repo = nil
		r.shared.approval.reset()
		r.shared.projectSpec.reset()
		r.shared.Unlock()
	}
	r.workstreamsMu.Lock()
	children := r.workstreams
	r.workstreams = nil
	r.closed = true
	r.workstreamsMu.Unlock()
	var errs []error
	errs = append(errs, r.releaseWorkgroupTraces())
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

func (r *Repository) closeFiles() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cache.mu.Lock()
	defer r.cache.mu.Unlock()
	var errs []error
	errs = append(errs, r.closeDatabases())
	if r.lock != nil {
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
// recorded as the charter's first revision. It is one HTML comment, so a new
// charter holds no guidance and no rules until the owner writes some.
const CharterTemplate = `<!--
This charter is optional. Write it like an AGENTS.md: Markdown guidance for
every agent working on this project, in addition to the repository's own
contributor documents (AGENTS.md, CONTRIBUTING, CLAUDE.md and similar), which
always apply.

A numbered list item, "1. Rule text", is a rule reviewers can cite as
charter#1. Keep each number unique and never reuse a retired one.

Budgets, capacity, models and other factory settings belong in configuration,
not here.
-->
`

// EntitiesPath is the project document holding the local entity map, and
// EntitiesDocument the record ID of its revisions.
const (
	EntitiesPath     = "kb/entities.json"
	EntitiesDocument = "kb-entities"
)

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
	return createTrace(ctx, root, project, at, actor, nil, CharterTemplate)
}

// CreateSeeded is Create with entities, already validated by the caller, as
// revision 1 of the entity map document, committed with the rest of the trace.
func CreateSeeded(ctx context.Context, root config.Root, project config.Project, at time.Time, actor Actor, entities []byte) (*Repository, error) {
	if len(entities) == 0 {
		return nil, fmt.Errorf("entity map seed required")
	}
	return createTrace(ctx, root, project, at, actor, entities, CharterTemplate)
}

// CreateSeededWithCharter is CreateSeeded with charter, already written by
// the owner, as the first charter revision in place of the template.
func CreateSeededWithCharter(ctx context.Context, root config.Root, project config.Project, at time.Time, actor Actor, entities []byte, charter string) (*Repository, error) {
	if len(entities) == 0 {
		return nil, fmt.Errorf("entity map seed required")
	}
	if strings.TrimSpace(charter) == "" {
		return nil, fmt.Errorf("charter required")
	}
	return createTrace(ctx, root, project, at, actor, entities, charter)
}

func createTrace(ctx context.Context, root config.Root, project config.Project, at time.Time, actor Actor, entities []byte, charter string) (*Repository, error) {
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
	records = append(records, charterRevision(project.ID, at, actor, "project-create", 1, charter))
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
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, err
	}
	if len(entries) > 0 {
		return nil, fmt.Errorf("%w: %s already contains %s", ErrConflict, directory, entries[0].Name())
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
	return r.openAll()
}

// OpenByID opens the trace of the project id as Open does, from the project's
// ID alone. It serves a project that is no longer active, whose configuration
// and clone are not at hand.
func OpenByID(root config.Root, id config.ProjectID) (*Repository, error) {
	directory, err := root.ProjectTrace(id)
	if err != nil {
		return nil, err
	}
	r, err := openStoreDirectory(root, id, "", directory)
	if err != nil {
		return nil, err
	}
	return r.openAll()
}

func (r *Repository) openAll() (*Repository, error) {
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

// SessionKind is the Kind a session workstream's manifest records.
const SessionKind = "session"

// FeatureKind is the workstream kind a stored empty kind reads as.
const FeatureKind = kinds.Feature

type manifestRecord struct {
	Header
	Workspaces string `json:"workspaces,omitempty"`
	Kind       string `json:"kind,omitempty"`
	Profile    string `json:"profile,omitempty"`
	Base       string `json:"base,omitempty"`
	Branch     string `json:"branch,omitempty"`
	Title      string `json:"title,omitempty"`
}

type checkedManifest struct {
	data           []byte
	schema, stream string
	manifest       manifestRecord
}

func (r *Repository) readManifest(name, schema string, stream config.WorkstreamID) (manifestRecord, error) {
	data, err := r.file(name)
	if err != nil {
		return manifestRecord{}, err
	}
	r.manifestsMu.Lock()
	c, ok := r.manifests[name]
	r.manifestsMu.Unlock()
	if ok && c.schema == schema && c.stream == string(stream) && bytes.Equal(c.data, data) {
		return c.manifest, nil
	}
	m, err := r.decodeManifest(name, data, schema, stream)
	if err != nil {
		return manifestRecord{}, err
	}
	r.manifestDecodes.Add(1)
	r.manifestsMu.Lock()
	if r.manifests == nil {
		r.manifests = map[string]checkedManifest{}
	}
	r.manifests[name] = checkedManifest{data: bytes.Clone(data), schema: schema, stream: string(stream), manifest: m}
	r.manifestsMu.Unlock()
	return m, nil
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

func validWorkspaces(backend string) bool {
	return backend == "" || backend == config.WorkspacesGit || backend == config.WorkspacesJujutsu
}

func validSessionFields(stream config.WorkstreamID, m manifestRecord) error {
	if stream == "" {
		if m.Kind != "" || m.Profile != "" || m.Base != "" || m.Branch != "" || m.Title != "" {
			return fmt.Errorf("only a session workstream may record kind, profile, base, branch or title")
		}
		return nil
	}
	if m.Kind != SessionKind {
		if m.Profile != "" || m.Base != "" || m.Branch != "" || m.Title != "" {
			return fmt.Errorf("only a session workstream may record profile, base, branch or title")
		}
		if m.Kind != "" && !key(m.Kind) {
			return fmt.Errorf("invalid workstream kind %q", m.Kind)
		}
		return nil
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

// Kind returns a workstream's kind: SessionKind for a session, otherwise the
// workstream kind it was created with. A stored empty kind is FeatureKind. A
// kind this build does not register is returned as stored; the kinds package
// decides what it means.
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
	if m.Kind == "" {
		return FeatureKind, nil
	}
	return m.Kind, nil
}

// RecordedKind returns the kind a workstream's manifest records, empty when
// it records none.
func (r *Repository) RecordedKind(stream config.WorkstreamID) (string, error) {
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
	return m.Kind, err
}

// CreatedAt returns when a workstream's trace was created.
func (r *Repository) CreatedAt(stream config.WorkstreamID) (time.Time, error) {
	if err := config.CheckWorkstreamIDs(stream); err != nil {
		return time.Time{}, err
	}
	r, err := r.scoped(stream)
	if err != nil {
		return time.Time{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	m, err := r.workstreamManifest(stream)
	return m.Header.At, err
}

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
	return r.CreateWorkstreamKind(ctx, id, workspaces, "", at, actor)
}

// CreateWorkstreamKind is CreateWorkstreamOn that records the workstream's
// kind in the commit that creates it. An empty kind records none and reads as
// FeatureKind. SessionKind is refused: CreateSessionWorkstream creates
// sessions.
func (r *Repository) CreateWorkstreamKind(ctx context.Context, id config.WorkstreamID, workspaces, kind string, at time.Time, actor Actor) error {
	if kind == SessionKind || kind != "" && !key(kind) {
		return fmt.Errorf("invalid workstream kind %q", kind)
	}
	if r.stream != "" {
		return fmt.Errorf("a workstream trace instance creates no workstreams of its own")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := createWorkstreamTrace(ctx, r.root, r.project, id, manifestRecord{Workspaces: workspaces, Kind: kind}, "workstream-create", "Create workstream trace", at, actor); err != nil {
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

// WithWorkstreamDiscovery lists workstreams and reads their bookkeeping while
// trace deletion is excluded. The callback must obey WithWorkstreamSnapshot's
// restrictions; scheduling and external work run after this method returns.
func (r *Repository) WithWorkstreamDiscovery(read func([]config.WorkstreamID) error) error {
	return r.WithWorkstreamSnapshot(func() error {
		streams, err := r.Workstreams()
		if err != nil {
			return err
		}
		return read(streams)
	})
}

// Generation reports how many commits landed in the trace through this
// handle. It never decreases while the handle is open, and it is unchanged
// exactly when no write landed between two calls: a caller that assembles a
// response from several separate reads can call it before and after and retry
// the assembly when it differs. It never blocks on a write. On the project's
// own instance it sums its own generation with that of every workstream
// instance opened so far.
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

func (r *Repository) append(ctx context.Context, v Record) error {
	if _, ok := v.(Image); ok && !r.imageStorageScope() {
		return fmt.Errorf("images belong only to member or session traces")
	}
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

// ErrKindRecorded is returned when a workstream already records a different
// kind than the one a caller sets: a workstream's kind is set once.
var ErrKindRecorded = fmt.Errorf("%w: the workstream's kind is already recorded", ErrConflict)

// AppendWithKind records one revision of a workstream and the workstream's
// kind in the same commit. A workstream that records no kind, or records the
// same one, has it set; a workstream that records another kind is refused
// with ErrKindRecorded and nothing is written.
func (r *Repository) AppendWithKind(ctx context.Context, v Record, workKind string) error {
	stream := v.header().Workstream
	if stream == "" || workKind == "" || workKind == SessionKind || !key(workKind) {
		return fmt.Errorf("a workstream record and a workstream kind are required")
	}
	r, err := r.scoped(stream)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	m, err := r.workstreamManifest(stream)
	if err != nil {
		return err
	}
	if m.Kind == SessionKind || m.Kind != "" && m.Kind != workKind {
		return fmt.Errorf("%w: %q, not %q", ErrKindRecorded, m.Kind, workKind)
	}
	if m.Kind == workKind {
		return r.append(ctx, v)
	}
	if err := validate(v); err != nil {
		return err
	}
	if v.header().Project != r.project {
		return fmt.Errorf("record belongs to a different project")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := r.checkRevisions(ctx, []Record{v}); err != nil {
		return err
	}
	m.Kind = workKind
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	c := change{records: []Record{v}, message: fmt.Sprintf("Record %s %s revision %d and workstream kind %s", kind(v), v.header().ID, v.header().Revision, workKind)}
	c.setFile("workstreams/"+string(stream)+"/workstream.json", append(data, '\n'))
	return r.publish(ctx, c)
}

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
