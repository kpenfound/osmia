package trace

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
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
// One process holds its exclusive lock until Close; calls on that handle serialize.
// External writers must not modify or move the repository while it is open.
type Repository struct {
	mu              sync.Mutex
	operationMu     sync.Mutex
	root            config.Root
	project         config.ProjectID
	directory       string
	dir             *os.Root
	lock            *os.File
	session         string
	wake            *coreadapter.WakeAdapter
	failPublication func(string) error
	observer        atomic.Pointer[func(Commit)]
	// gitMu guards what the handle remembers about the Git store: the tree
	// listing of the commit treeHead and the object files already flushed.
	gitMu    sync.Mutex
	tree     map[string]string
	treeHead string
	synced   map[string]bool
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
	base, err := os.OpenRoot(root.String())
	if err != nil {
		return nil, err
	}
	defer base.Close()
	dir, err := base.OpenRoot("projects/" + string(project.ID))
	if err != nil {
		return nil, err
	}
	return &Repository{root: root, project: project.ID, directory: directory, dir: dir, session: rand.Text(), wake: coreadapter.NewWakeups()}, nil
}
func (r *Repository) acquire() error {
	if err := r.checked(".git/osmia.lock"); err != nil {
		return err
	}
	f, err := r.dir.OpenFile(".git/osmia.lock", os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return fmt.Errorf("%w: %v", ErrLocked, err)
	}
	r.lock = f
	return nil
}

// Commit names the paths one published trace commit wrote or removed, and
// holds the content it wrote for those paths whose bytes the commit was given
// rather than read from the work tree. Observers must not modify it.
type Commit struct {
	Paths   []string
	Content map[string][]byte
}

// Observe calls f after each commit this handle publishes, replacing any
// earlier observer. f runs while the handle's lock is held, so it must return
// promptly and must not call the repository.
func (r *Repository) Observe(f func(Commit)) {
	r.observer.Store(&f)
}

// observed passes a published commit to the observer, if any.
func (r *Repository) observed(c Commit) {
	if f := r.observer.Load(); f != nil && *f != nil {
		(*f)(c)
	}
}

func (r *Repository) Close() error {
	r.operationMu.Lock()
	defer r.operationMu.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	var err error
	if r.lock != nil {
		// A child process forked but not yet executed shares the open file,
		// so closing alone would leave the lock held until it executes.
		err = errors.Join(syscall.Flock(int(r.lock.Fd()), syscall.LOCK_UN), r.lock.Close())
		r.lock = nil
	}
	return errors.Join(err, r.dir.Close())
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

// ownerActor records edits the owner made to project documents on disk.
var ownerActor = Actor{Kind: "owner", ID: "local"}

func charterRevision(project config.ProjectID, at time.Time, actor Actor, cause string, revision int, content string) Document {
	return Document{Header: Header{Schema: "osmia.trace.document", Version: Version, ID: "charter", Revision: revision, Project: project, At: at, Actor: actor, Cause: cause}, Path: "charter.md", Content: content}
}

// Charter returns the latest recorded charter revision. Creation records the
// template as the first. When charter.md differs from the latest revision, or
// none is recorded, the file is first recorded as a new revision by the owner,
// so every owner edit is versioned before it is used. A read with no edit
// records nothing.
func (r *Repository) Charter(ctx context.Context, at time.Time) (Document, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Document{}, err
	}
	data, err := r.readFile("charter.md")
	if err != nil {
		return Document{}, err
	}
	records, _, err := r.scan()
	if err != nil {
		return Document{}, err
	}
	next := charterRevision(r.project, at, ownerActor, "owner-edit", 1, string(data))
	if latest, ok := latestCharter(records); ok {
		if latest.Content == next.Content {
			return latest, nil
		}
		next.ID, next.Revision = latest.ID, latest.Revision+1
	}
	// The file already holds this content; rewriting it could overwrite a
	// newer edit, which the next read then records. The commit takes the
	// recorded bytes, not the file.
	if err := r.append(ctx, next, false); err != nil {
		return Document{}, err
	}
	return next, nil
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

// Create initializes a dedicated local repository in an existing configuration
// directory or a new project directory. It refuses any existing trace or Git
// metadata. The entity map file starts as "{}" with no document revision.
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
	documents, err := json.Marshal(charterRevision(project.ID, at, actor, "project-create", 1, CharterTemplate))
	if err != nil {
		return nil, err
	}
	documents = append(documents, '\n')
	if entities == nil {
		entities = []byte("{}\n")
	} else {
		d := Document{Header: Header{Schema: "osmia.trace.document", Version: Version, ID: EntitiesDocument, Revision: 1, Project: project.ID, At: at, Actor: actor, Cause: "project-create"}, Path: EntitiesPath, Content: string(entities)}
		if err := validate(d); err != nil {
			return nil, err
		}
		line, err := json.Marshal(d)
		if err != nil {
			return nil, err
		}
		documents = append(documents, append(line, '\n')...)
	}
	if err := os.MkdirAll(root.String(), 0700); err != nil {
		return nil, err
	}
	if _, err := root.ProjectTrace(project.ID); err != nil {
		return nil, err
	}
	base, err := os.OpenRoot(root.String())
	if err != nil {
		return nil, err
	}
	err = base.MkdirAll("projects/"+string(project.ID), 0700)
	base.Close()
	if err != nil {
		return nil, err
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
	// Only the project's existing configuration may precede trace initialization.
	entries, err := fs.ReadDir(r.dir.FS(), ".")
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.Name() != "config.toml" {
			return nil, fmt.Errorf("%w: %s already contains %s", ErrConflict, directory, e.Name())
		}
		if err := r.checked(e.Name()); err != nil {
			return nil, err
		}
	}
	// Mkdir reserves initialization across competing creators before any Git write.
	if err := r.dir.Mkdir(".git", 0700); err != nil {
		return nil, err
	}
	for _, name := range []string{".git/objects", ".git/refs/heads", "kb", "notes", "workstreams"} {
		if err := r.mkdir(name); err != nil {
			return nil, err
		}
	}
	for _, f := range []struct{ name, data string }{{".git/config", gitConfig}, {".git/HEAD", "ref: refs/heads/main\n"}, {".git/osmia.lock", ""}} {
		if err := r.writeFile(f.name, []byte(f.data)); err != nil {
			return nil, err
		}
	}
	if err := r.acquire(); err != nil {
		return nil, err
	}
	manifest := Header{Schema: "osmia.trace.project", Version: Version, ID: string(project.ID), Revision: 1, Project: project.ID, At: at, Actor: actor, Cause: "project-create"}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, err
	}
	for _, f := range []struct {
		name string
		data []byte
	}{{"project.json", append(data, '\n')}, {"charter.md", []byte(CharterTemplate)}, {EntitiesPath, entities}, {"documents.jsonl", documents}} {
		if err := r.writeFile(f.name, f.data); err != nil {
			return nil, err
		}
	}
	if err := r.commit(ctx, []string{"project.json", "charter.md", EntitiesPath, "documents.jsonl"}, "Create project trace"); err != nil {
		return nil, err
	}
	ok = true
	return r, nil
}

// Open finishes journaled workflow publication without rewriting committed history.
// If records are corrupt it returns both a handle and a diagnostic error; callers
// must Close that handle. Read returns valid records with corruption diagnostics.
func Open(root config.Root, project config.Project) (*Repository, error) {
	r, err := openDirectory(root, project)
	if err != nil {
		return nil, err
	}
	if err := r.checkGit(); err != nil {
		r.Close()
		return nil, err
	}
	if err := r.acquire(); err != nil {
		r.Close()
		return nil, err
	}
	if err := r.recoverPublication(context.Background()); err != nil {
		return r, err
	}
	_, streams, err := r.scan()
	return r, errors.Join(err, r.checkHistory(context.Background()), r.checkWorkflows(streams))
}
func (r *Repository) manifest(name, schema string, stream config.WorkstreamID) error {
	data, err := r.readFile(name)
	if err != nil {
		return err
	}
	var h Header
	if err := decode(data, &h); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	id := string(r.project)
	if stream != "" {
		id = string(stream)
	}
	if h.Schema != schema || h.Version != Version || h.Project != r.project || h.Workstream != stream || h.ID != id || h.Revision != 1 || h.At.IsZero() || !validActor(h.Actor) || !present(h.Cause) || h.Depth < 0 {
		return fmt.Errorf("%s: invalid manifest schema, identity or provenance", name)
	}
	return nil
}

// CreateWorkstream commits a new workstream and then creates its
// chief-of-staff thread. An error from the thread write leaves the workstream
// committed; EnsureChiefOfStaff creates the missing thread. An existing
// workstream is refused with ErrConflict.
func (r *Repository) CreateWorkstream(ctx context.Context, id config.WorkstreamID, at time.Time, actor Actor) error {
	if err := config.CheckWorkstreamIDs(id); err != nil {
		return err
	}
	if at.IsZero() || !validActor(actor) {
		return fmt.Errorf("timestamp and actor required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := r.checkHistory(ctx); err != nil {
		return err
	}
	_, _, err := r.scan()
	if err != nil {
		return err
	}
	prefix := "workstreams/" + string(id)
	if err := r.checked(prefix); err != nil {
		return err
	}
	if _, err := r.dir.Lstat(prefix); !os.IsNotExist(err) {
		return fmt.Errorf("%w: workstream %s already exists or is inaccessible", ErrConflict, id)
	}
	for _, sub := range []string{"handed", "shed", "amendments", "questions", "units", "agents"} {
		if err := r.mkdir(prefix + "/" + sub); err != nil {
			return err
		}
	}
	h := Header{Schema: "osmia.trace.workstream", Version: Version, ID: string(id), Revision: 1, Project: r.project, Workstream: id, At: at, Actor: actor, Cause: "workstream-create"}
	data, err := json.MarshalIndent(h, "", "  ")
	if err != nil {
		return err
	}
	if err := r.writeFile(prefix+"/workstream.json", append(data, '\n')); err != nil {
		return err
	}
	paths := []string{prefix + "/workstream.json"}
	for _, name := range []string{"documents.jsonl", "events.jsonl", "ledger.jsonl"} {
		name = prefix + "/" + name
		if err := r.writeFile(name, nil); err != nil {
			return err
		}
		paths = append(paths, name)
	}
	if err := r.commit(ctx, paths, "Create workstream trace"); err != nil {
		return err
	}
	_, err = r.ensureChiefOfStaff(ctx, id, at, actor)
	return err
}

func (r *Repository) Workstreams() ([]config.WorkstreamID, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, streams, err := r.scan()
	return streams, err
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

func (r *Repository) scan() ([]Record, []config.WorkstreamID, error) {
	if err := r.recoverPublication(context.Background()); err != nil {
		return nil, nil, err
	}
	if err := r.manifest("project.json", "osmia.trace.project", ""); err != nil {
		return nil, nil, err
	}
	if err := r.checked("workstreams"); err != nil {
		return nil, nil, err
	}
	entries, err := fs.ReadDir(r.dir.FS(), "workstreams")
	if err != nil {
		return nil, nil, err
	}
	var diagnostics []error
	var streams []config.WorkstreamID
	known := map[config.WorkstreamID]bool{}
	for _, entry := range entries {
		id, err := config.ParseWorkstreamID(entry.Name())
		name := "workstreams/" + entry.Name()
		if err == nil {
			err = r.checked(name)
		}
		if err == nil && !entry.IsDir() {
			err = fmt.Errorf("expected workstream directory")
		}
		if err == nil {
			err = r.manifest(name+"/workstream.json", "osmia.trace.workstream", id)
		}
		if err != nil {
			diagnostics = append(diagnostics, fmt.Errorf("%s: %w", name, err))
			continue
		}
		known[id] = true
		streams = append(streams, id)
	}
	required := []string{"documents.jsonl"}
	for _, id := range streams {
		for _, name := range []string{"documents.jsonl", "events.jsonl", "ledger.jsonl"} {
			required = append(required, "workstreams/"+string(id)+"/"+name)
		}
	}
	for _, name := range required {
		if _, err := r.readFile(name); err != nil {
			diagnostics = append(diagnostics, fmt.Errorf("%s: %w", name, err))
		}
	}
	var records []Record
	latest := map[string]Record{}
	documents := map[string]string{}
	err = r.walk(func(name string, entry fs.DirEntry) error {
		if entry.IsDir() || !strings.HasSuffix(name, ".jsonl") {
			return nil
		}
		parts := strings.Split(name, "/")
		if len(parts) == 4 && parts[0] == "workstreams" && parts[2] == "handed" {
			return nil // Handed documents retain their original name and content.
		}
		data, err := r.readFile(name)
		if err != nil {
			diagnostics = append(diagnostics, fmt.Errorf("%s: %w", name, err))
			return nil
		}
		lines := bytes.Split(data, []byte{'\n'})
		for i, line := range lines {
			if i == len(lines)-1 && len(line) == 0 {
				continue
			}
			v, err := decodeRecord(line)
			if err == nil && i == len(lines)-1 {
				err = fmt.Errorf("incomplete JSONL record (missing newline)")
			}
			if err == nil {
				err = validate(v)
			}
			if err == nil {
				h := v.header()
				if h.Project != r.project || (h.Workstream != "" && !known[h.Workstream]) || recordPath(v) != name {
					err = fmt.Errorf("record identity does not match its trace path")
				}
			}
			if err == nil {
				err = revision(latest[recordKey(v)], v)
			}
			if err == nil {
				if d, ok := v.(Document); ok {
					p := scopePath(d.Header) + d.Path
					if id, ok := documents[p]; ok && id != d.ID {
						err = fmt.Errorf("document path has multiple identities")
					}
					documents[p] = d.ID
				}
			}
			if err != nil {
				diagnostics = append(diagnostics, fmt.Errorf("%s:%d: %w", name, i+1, err))
				continue
			}
			latest[recordKey(v)] = v
			records = append(records, v)
		}
		return nil
	})
	diagnostics = append(diagnostics, err)
	return records, streams, errors.Join(diagnostics...)
}

// Append retains every revision in JSONL and commits the affected ordinary files.
// A file or Git failure is reported without claiming a workflow transaction;
// records already written remain available for later reconciliation. A
// project charter revision is refused with ErrConflict unless charter.md
// matches the latest recorded revision; Charter records owner edits. Status
// and priority records are refused with ErrConflict: SetStatus and
// SetPriority are their only writers.
func (r *Repository) Append(ctx context.Context, v Record) error {
	switch v.(type) {
	case Status:
		return fmt.Errorf("%w: status records require SetStatus", ErrConflict)
	case PriorityChange:
		return fmt.Errorf("%w: priority records require SetPriority", ErrConflict)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.append(ctx, v, true)
}

// append requires r.mu. writeDocument false leaves a document's ordinary file
// as it is on disk.
func (r *Repository) append(ctx context.Context, v Record, writeDocument bool) error {
	if err := validate(v); err != nil {
		return err
	}
	if v.header().Project != r.project {
		return fmt.Errorf("record belongs to a different project")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := r.checkHistory(ctx); err != nil {
		return err
	}
	records, streams, err := r.scan()
	if err != nil {
		return err
	}
	if id := v.header().Workstream; id != "" {
		found := false
		for _, s := range streams {
			found = found || s == id
		}
		if !found {
			return fmt.Errorf("unknown workstream %s", id)
		}
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
	var previous Record
	for _, old := range records {
		if recordKey(old) == recordKey(v) {
			previous = old
		}
		if d, ok := v.(Document); ok {
			if p, ok := old.(Document); ok && p.Workstream == d.Workstream && p.Path == d.Path && p.ID != d.ID {
				return fmt.Errorf("%w: document path already has an identity", ErrConflict)
			}
		}
	}
	if err := revision(previous, v); err != nil {
		return err
	}
	// The owner edits charter.md directly. Writing over an edit Charter has
	// not recorded would lose it.
	if d, ok := v.(Document); ok && writeDocument && d.Workstream == "" && d.Path == "charter.md" {
		latest, _ := latestCharter(records)
		current, err := r.readFile("charter.md")
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if err == nil && string(current) != latest.Content {
			return fmt.Errorf("%w: charter.md has an unrecorded owner edit; read the charter first", ErrConflict)
		}
	}
	name := recordPath(v)
	paths := []string{name}
	if d, ok := v.(Document); ok {
		paths = append(paths, scopePath(d.Header)+d.Path)
	}
	// Validate every destination before the first write.
	for _, name := range paths {
		if err := r.checked(name); err != nil {
			return err
		}
	}
	data, err := r.readFile(name)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	line, err := json.Marshal(v)
	if err != nil {
		return err
	}
	data = append(data, append(line, '\n')...)
	if err := r.writeFile(name, data); err != nil {
		return err
	}
	if d, ok := v.(Document); ok && writeDocument {
		if err := r.writeFile(paths[1], []byte(d.Content)); err != nil {
			return err
		}
	}
	content := map[string][]byte{}
	if d, ok := v.(Document); ok {
		content[paths[1]] = []byte(d.Content)
	}
	return r.commitContent(ctx, paths, content, fmt.Sprintf("Record %s %s revision %d", kind(v), v.header().ID, v.header().Revision))
}

// Read returns all valid revisions of T in file order, with path/line diagnostics
// for any damaged history. Callers must not ignore an accompanying error.
func Read[T Record](r *Repository, stream config.WorkstreamID) ([]T, error) {
	if stream != "" {
		if err := config.CheckWorkstreamIDs(stream); err != nil {
			return nil, err
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	records, streams, err := r.scan()
	if stream != "" {
		found := false
		for _, id := range streams {
			found = found || id == stream
		}
		if !found {
			err = errors.Join(err, fmt.Errorf("unknown workstream %s", stream))
		}
	}
	var result []T
	for _, v := range records {
		if t, ok := v.(T); ok && v.header().Workstream == stream {
			result = append(result, t)
		}
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
