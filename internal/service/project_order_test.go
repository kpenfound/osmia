package service

import (
	"context"
	"fmt"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/projectstore"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

type seededStream struct {
	project int
	age     time.Duration
	session bool
}

type orderRoot struct {
	opts Options
	cfg  *config.Config
	ids  []config.ProjectID
}

func newOrderRoot(t *testing.T, count int, seeded []seededStream) orderRoot {
	t.Helper()
	ctx := context.Background()
	home, err := os.MkdirTemp("", "po-")
	intakefixture.Must(t, err)
	t.Cleanup(func() { os.RemoveAll(home) })
	root := filepath.Join(home, "root")
	var ids []config.ProjectID
	intakefixture.Must(t, os.MkdirAll(root, 0700))
	intakefixture.Must(t, os.WriteFile(filepath.Join(root, "config.toml"), []byte("version = 1\nworkspaces = \"git\"\n[profiles.default]\nagent = \"claude\"\nmodel = \"test\"\n"), 0600))
	for i := 0; i < count; i++ {
		id := config.ProjectID(fmt.Sprintf("p_%032x", i+1))
		ids = append(ids, id)
		fixturekit.RegisterProject(t, root, id, fmt.Sprintf("version = 1\nupstream = \"upstream/repo%d\"\nfork = \"owner/repo%d\"\nclone = %q\n", i, i, filepath.Join(home, fmt.Sprintf("clone%d", i))))
	}
	cfg, err := projectstore.LoadConfig(config.Options{Root: root})
	intakefixture.Must(t, err)
	repos := map[config.ProjectID]*trace.Repository{}
	for _, p := range cfg.Projects {
		intakefixture.Must(t, os.MkdirAll(p.Clone, 0700))
		repo, err := trace.Create(ctx, cfg.Root, p, fixturekit.DemoStart, ownerActor)
		intakefixture.Must(t, err)
		repos[p.ID] = repo
	}
	for _, w := range seeded {
		createOrderStream(t, repos[ids[w.project]], w)
	}
	for _, repo := range repos {
		intakefixture.Must(t, repo.Close())
	}
	opts := testOptions(config.Options{Root: root})
	opts.Workstreams = nil
	return orderRoot{opts: opts, cfg: cfg, ids: ids}
}

func createOrderStream(t *testing.T, repo *trace.Repository, w seededStream) {
	t.Helper()
	existing, err := repo.Workstreams()
	intakefixture.Must(t, err)
	id, err := config.NewWorkstreamID(existing...)
	intakefixture.Must(t, err)
	at := fixturekit.DemoStart.Add(w.age)
	if w.session {
		intakefixture.Must(t, repo.CreateSessionWorkstream(context.Background(), id, "default", "base-commit", "session-branch", "A session", at, ownerActor))
		return
	}
	intakefixture.Must(t, repo.CreateWorkstream(context.Background(), id, at, ownerActor))
}

func listedProjects(t *testing.T, c *Client, file []config.ProjectID) []config.ProjectID {
	t.Helper()
	ctx := context.Background()
	got, err := c.Configuration(ctx)
	intakefixture.Must(t, err)
	var ids []config.ProjectID
	for _, p := range got.Projects {
		ids = append(ids, p.ID)
	}
	var effective []config.ProjectID
	for _, id := range got.Effective.ActiveProjects {
		effective = append(effective, config.ProjectID(id))
	}
	if !reflect.DeepEqual(effective, file) {
		t.Fatalf("effective.active_projects %v, want the registry order %v", effective, file)
	}
	runtime, err := c.Runtime(ctx)
	intakefixture.Must(t, err)
	var running []config.ProjectID
	for _, p := range runtime.Projects {
		running = append(running, p.Project)
	}
	if !reflect.DeepEqual(running, ids) {
		t.Fatalf("runtime projects %v, configuration projects %v", running, ids)
	}
	return ids
}

func wantOrder(t *testing.T, got []config.ProjectID, ids []config.ProjectID, indexes ...int) {
	t.Helper()
	var want []config.ProjectID
	for _, i := range indexes {
		want = append(want, ids[i])
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("projects %v, want %v", got, want)
	}
}

// The configuration and runtime reads list the projects whose newest
// workstream was created later first, whatever order the registry lists them in.
func TestConfigListsProjectsByNewestWorkstream(t *testing.T) {
	t.Parallel()
	f := newOrderRoot(t, 3, []seededStream{
		{project: 0, age: time.Hour},
		{project: 1, age: 3 * time.Hour},
		{project: 2, age: 2 * time.Hour},
		{project: 0, age: time.Minute},
	})
	_, c := start(t, f.opts)
	wantOrder(t, listedProjects(t, c, f.ids), f.ids, 1, 2, 0)
}

// A design or exploratory session counts like a feature workstream: one
// created after another project's newest feature workstream moves its
// project ahead.
func TestConfigCountsSessionsAsWorkstreams(t *testing.T) {
	t.Parallel()
	f := newOrderRoot(t, 3, []seededStream{
		{project: 0, age: 5 * time.Hour, session: true},
		{project: 1, age: 3 * time.Hour},
		{project: 2, age: 4 * time.Hour},
		{project: 2, age: 6 * time.Hour, session: true},
	})
	_, c := start(t, f.opts)
	wantOrder(t, listedProjects(t, c, f.ids), f.ids, 2, 0, 1)
}

// Projects with the same newest creation time, and projects without a
// workstream, which come last, keep their registry order, on every read.
func TestConfigKeepsFileOrderForTiesAndEmptyProjects(t *testing.T) {
	t.Parallel()
	f := newOrderRoot(t, 6, []seededStream{
		{project: 1, age: time.Hour},
		{project: 2, age: 2 * time.Hour},
		{project: 4, age: 2 * time.Hour},
		{project: 5, age: time.Hour},
	})
	_, c := start(t, f.opts)
	for i := 0; i < 3; i++ {
		wantOrder(t, listedProjects(t, c, f.ids), f.ids, 2, 4, 1, 5, 0, 3)
	}
}

// A project listed last moves first on the read after its first workstream is
// created, and a project already first stays first until a newer one appears.
func TestConfigCountsAWorkstreamCreatedAfterTheLastRead(t *testing.T) {
	t.Parallel()
	f := newOrderRoot(t, 3, []seededStream{
		{project: 0, age: time.Hour},
		{project: 1, age: 2 * time.Hour},
	})
	s, c := start(t, f.opts)
	wantOrder(t, listedProjects(t, c, f.ids), f.ids, 1, 0, 2)
	repo, err := s.repository(f.ids[2])
	intakefixture.Must(t, err)
	createOrderStream(t, repo, seededStream{project: 2, age: 3 * time.Hour})
	wantOrder(t, listedProjects(t, c, f.ids), f.ids, 2, 1, 0)
	repo, err = s.repository(f.ids[0])
	intakefixture.Must(t, err)
	createOrderStream(t, repo, seededStream{project: 0, age: 4 * time.Hour, session: true})
	wantOrder(t, listedProjects(t, c, f.ids), f.ids, 0, 2, 1)
}

// A project whose workstreams cannot be listed sorts as if it had none; the
// read still answers with every project.
func TestConfigPlacesAnUnreadableProjectAmongTheEmptyOnes(t *testing.T) {
	t.Parallel()
	f := newOrderRoot(t, 3, []seededStream{
		{project: 0, age: time.Hour},
		{project: 1, age: 9 * time.Hour},
	})
	_, c := start(t, f.opts)
	wantOrder(t, listedProjects(t, c, f.ids), f.ids, 1, 0, 2)
	traces, err := f.cfg.Root.WorkstreamTraces(f.ids[1])
	intakefixture.Must(t, err)
	intakefixture.Must(t, os.WriteFile(filepath.Join(traces, "not-a-workstream"), nil, 0600))
	wantOrder(t, listedProjects(t, c, f.ids), f.ids, 0, 1, 2)
}
