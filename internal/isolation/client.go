package isolation

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/kpenfound/osmia/internal/coreadapter"
)

// View is a turn's files as the tools that act on them see them. A local
// turn's View is the FileView the service built; a remote turn's View carries
// out each operation on the client's own view.
type View interface {
	// Read returns a file's contents, or a directory's entry names one per
	// line with a trailing slash on each directory.
	Read(name string) ([]byte, error)
	// Write writes a file for the session's own file_write tool: the role must
	// be allowed to write.
	Write(name string, data []byte) error
	// Put writes a file the service itself adds to the view, such as a preview
	// image, whatever the role may write.
	Put(name string, data []byte) error
}

// Skills reads files inside a turn's prepared skill directories at their
// absolute paths.
type Skills interface {
	Read(ctx context.Context, path string) ([]byte, error)
}

var (
	_ View   = (*FileView)(nil)
	_ Skills = (*SkillReader)(nil)
)

// Files are what the tools that act on a turn's view and skills operate on.
type Files struct {
	View   View
	Skills Skills
}

// StartView is the starting point of a view a client builds itself: the files
// at Paths of the project's clone at Commit.
type StartView struct {
	Project string
	Commit  string
	Paths   []string
}

// ClientTurn is a turn handed to a client to run its session. The service has
// already chosen the role's grants and hosted its tools; the client supplies
// the machine.
type ClientTurn struct {
	// Prepared is the turn with its profile, prompts, resumed session, outcome
	// policy and sandbox settings. For a local turn it also holds the verified
	// isolation and the MCP endpoint; for a remote turn those are Capabilities,
	// Environment and Endpoint.
	Prepared     coreadapter.PreparedTurn
	Capabilities coreadapter.Capabilities
	Environment  map[string]string
	// Endpoint is the turn's MCP endpoint on the service, which a remote turn's
	// tool calls reach through the relay under the same bearer token.
	Endpoint coreadapter.Endpoint
	// View is the view the service built: the one a local turn runs in, or the
	// source of a remote turn's view archive. Nil when a remote client builds
	// the view from Start.
	View *FileView
	// Start is where a remote client builds its view from when it holds the
	// project's commit; nil when the view comes as an archive.
	Start *StartView
	// Overlay holds the files the service seeded for a turn whose remote client
	// builds its view from Start: the client places them over the view it
	// builds. Nil when the turn has none, and always nil with Start nil.
	Overlay *FileView
	// SkillRefs are the skill references the turn is given.
	SkillRefs []string
	// ReturnView says whether the finished view comes back and is captured.
	ReturnView bool
	// RunsDagger says whether the turn's sandbox runs Dagger, so its view holds
	// the empty Git repository Dagger needs at its root.
	RunsDagger bool
	// Fallbacks are the profiles that follow Prepared.Profile in its chain.
	Fallbacks []string
	// Pinned are the directories of the revisions the service's view was made
	// from, which a local session may not write.
	Pinned []string
}

// ClientRun is what a client returns for a turn.
type ClientRun struct {
	Result coreadapter.SessionResult
	// View is the finished view the service captures from. It is nil when the
	// turn returned none, such as a remote turn that was lost before it sent its
	// view, and the run then also holds no files to capture.
	View *FileView
}

// Client runs the model session of a turn on one machine. The service's own
// machine is the built-in local client, which runs in this process; every
// other client is a machine on the tailnet that connected to the service.
// Choosing the client belongs to the service's placement.
type Client interface {
	// ID names the client in the trace: coreadapter.LocalClient for the
	// service's own machine, a tailnet node's stable ID otherwise.
	ID() string
	// Remote reports whether the client builds its own view and runs the
	// session on another machine.
	Remote() bool
	// Files returns what the turn's view and skill tools act on.
	Files(ctx context.Context, t ClientTurn) (Files, error)
	// Run runs the turn's session and returns its result and finished view.
	Run(ctx context.Context, t ClientTurn) (ClientRun, error)
}

// Placer chooses the client a turn runs on.
type Placer func(ctx context.Context, scope coreadapter.Scope, profile coreadapter.Profile) (Client, error)

// Local is the built-in client: it runs a turn's session in this process with
// engine, in the view the service built.
func Local(engine coreadapter.Engine) Client { return localClient{engine} }

type localClient struct{ engine coreadapter.Engine }

func (localClient) ID() string   { return coreadapter.LocalClient }
func (localClient) Remote() bool { return false }

func (c localClient) Files(_ context.Context, t ClientTurn) (Files, error) {
	files := Files{View: t.View}
	if skills := NewSkillReader(c.engine, t.SkillRefs); skills != nil {
		files.Skills = skills
	}
	return files, nil
}

func (c localClient) Run(ctx context.Context, t ClientTurn) (ClientRun, error) {
	if t.View == nil {
		return ClientRun{}, errors.New("the local client runs a turn in the service's view")
	}
	executor := coreadapter.CoreExecutor{Required: t.Prepared.Sandbox.Verified, Runner: c.engine, Pinned: t.Pinned}
	result, err := (&coreadapter.TurnRunner{Executor: executor}).Run(ctx, t.Prepared)
	return ClientRun{Result: result, View: t.View}, err
}

// AdoptView makes dir, a directory of files the service holds, a view whose
// files are captured and whose release removes dir. It is how a view a client
// returned becomes a view the service captures from.
func AdoptView(dir string, access coreadapter.Access) (*FileView, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(abs)
	if err != nil {
		return nil, err
	}
	return &FileView{root: root, workspace: coreadapter.Workspace{ID: filepath.Base(abs), Directory: abs, Access: access}}, nil
}

// Put writes a file the service adds to the view, creating its directories. It
// refuses what Write refuses except that the role need not be allowed to write.
func (v *FileView) Put(name string, data []byte) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed {
		return os.ErrClosed
	}
	if err := checkPath(v.root, name, true); err != nil {
		return err
	}
	if err := v.root.MkdirAll(filepath.Dir(name), 0700); err != nil {
		return err
	}
	return v.root.WriteFile(name, data, 0600)
}

// Detach closes the view and leaves its directory and files in place. A client
// detaches the view of a finished turn to keep it on disk until the service
// has answered for it.
func (v *FileView) Detach() error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed {
		return nil
	}
	v.closed = true
	return v.root.Close()
}
