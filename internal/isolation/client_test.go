package isolation

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	a "github.com/kpenfound/osmia/internal/coreadapter"
)

func TestTurnsRunOnTheBuiltInLocalClientByDefaultAndTheResultNamesIt(t *testing.T) {
	r, p, _, _, input := fixture(t, "mason", "none")
	var captured int
	r.Capture = func(_ context.Context, scope a.Scope, view *FileView, _ a.SessionResult) error {
		captured++
		data, err := view.Read("src/file")
		if err != nil || string(data) != "original" {
			t.Fatalf("capture: %q %v", data, err)
		}
		return nil
	}
	result, err := r.Run(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if result.Client != a.LocalClient || captured != 1 || p.acquired != 1 {
		t.Fatalf("client %q, captured %d, acquired %d", result.Client, captured, p.acquired)
	}
}

func TestAPlacerChoosesTheClientAndALocalClientReturnedByItRunsTheSameWay(t *testing.T) {
	r, p, _, engine, input := fixture(t, "mason", "none")
	placed := 0
	r.Clients = func(_ context.Context, scope a.Scope, profile a.Profile) (Client, error) {
		placed++
		if scope.Role != "mason" || profile.Backend != "claude" {
			t.Fatalf("placement was asked about %+v %+v", scope, profile)
		}
		return Local(engine), nil
	}
	result, err := r.Run(context.Background(), input)
	if err != nil || result.Client != a.LocalClient || placed != 1 {
		t.Fatalf("%+v %v %d", result, err, placed)
	}
	r.Clients = func(context.Context, a.Scope, a.Profile) (Client, error) {
		return nil, errors.New("no client can run the turn")
	}
	acquired := p.acquired
	if _, err := r.Run(context.Background(), input); err == nil || p.acquired != acquired {
		t.Fatalf("a turn no client can run took a workspace or ran: %v", err)
	}
}

type memView struct {
	files map[string]string
	puts  []string
}

func (m *memView) Read(name string) ([]byte, error) {
	if data, ok := m.files[name]; ok {
		return []byte(data), nil
	}
	return nil, os.ErrNotExist
}

func (m *memView) Write(name string, data []byte) error {
	m.files[name] = string(data)
	return nil
}

func (m *memView) Put(name string, data []byte) error {
	m.puts = append(m.puts, name)
	return m.Write(name, data)
}

type remoteStub struct {
	view   *memView
	turn   ClientTurn
	finish func(ClientTurn) (ClientRun, error)
}

func (remoteStub) ID() string   { return "node-1" }
func (remoteStub) Remote() bool { return true }
func (s *remoteStub) Files(context.Context, ClientTurn) (Files, error) {
	return Files{View: s.view}, nil
}
func (s *remoteStub) Run(_ context.Context, t ClientTurn) (ClientRun, error) {
	s.turn = t
	return s.finish(t)
}

func TestARemoteClientBuildsItsOwnViewAndTheToolsActOnIt(t *testing.T) {
	t.Setenv("OSMIA_TEST_CLOUD_TOKEN", "")
	r, p, h, engine, input := fixture(t, "mason", "none")
	stub := &remoteStub{view: &memView{files: map[string]string{"src/file": "the client's own copy"}}}
	r.Select = func(context.Context, a.Scope) (Selection, error) {
		return Selection{Paths: []string{"src"}, Commit: "abc123", Execution: a.ExecutionSettings{Mode: "none", DaggerCloudTokenEnv: "OSMIA_TEST_CLOUD_TOKEN"}}, nil
	}
	r.Chain = func(profile string) []string { return []string{"fallback"} }
	r.Clients = func(context.Context, a.Scope, a.Profile) (Client, error) { return stub, nil }
	input.Scope.Project = "p_one"
	input.Profile = a.Profile{Name: "default", Backend: "claude"}
	released := false
	stub.finish = func(turn ClientTurn) (ClientRun, error) {
		if turn.View != nil || turn.Start == nil || turn.Start.Commit != "abc123" || turn.Start.Project != "p_one" || !slices.Equal(turn.Start.Paths, []string{"src"}) {
			t.Fatalf("a client that holds the commit was given a service view: %+v", turn)
		}
		if !slices.Equal(turn.Fallbacks, []string{"fallback"}) || !turn.ReturnView || !turn.RunsDagger || turn.Prepared.Prompt != input.Prompt || turn.Prepared.Execution.DaggerCloudTokenEnv != "" {
			t.Fatalf("turn: %+v", turn)
		}
		for _, tool := range h.requests[0].Tools {
			switch tool.Name {
			case "file_read":
				out, err := tool.Handle(context.Background(), json.RawMessage(`{"path":"src/file"}`))
				if err != nil || string(out) != `"the client's own copy"` {
					t.Fatalf("file_read: %s %v", out, err)
				}
			case "file_write":
				if _, err := tool.Handle(context.Background(), json.RawMessage(`{"path":"src/new","content":"written"}`)); err != nil {
					t.Fatalf("file_write: %v", err)
				}
			}
		}
		returned, err := (Views{Directory: t.TempDir()}).Create(context.Background(), a.Workspace{Directory: p.directory, Access: a.ReadWrite}, []string{"src"})
		if err != nil {
			t.Fatal(err)
		}
		released = true
		return ClientRun{Result: a.SessionResult{Session: a.BackendSession{Backend: "claude", ID: "s"}, SessionDirectory: "/the/client/path"}, View: returned}, nil
	}
	var captured *FileView
	r.Capture = func(_ context.Context, _ a.Scope, view *FileView, result a.SessionResult) error {
		captured = view
		return nil
	}
	result, err := r.Run(context.Background(), input)
	if err != nil || !released {
		t.Fatalf("%+v %v", result, err)
	}
	if result.Client != "node-1" || result.SessionDirectory != input.SessionDirectory || result.ToolCounts["file_read"] != 1 || result.ToolCounts["file_write"] != 1 {
		t.Fatalf("result: %+v", result)
	}
	if stub.view.files["src/new"] != "written" {
		t.Fatalf("a tool write did not reach the client's view: %v", stub.view.files)
	}
	if captured == nil {
		t.Fatal("the returned view was not captured")
	}
	if _, err := os.Stat(captured.Workspace().Directory); !os.IsNotExist(err) {
		t.Fatalf("the returned view was not released after its capture: %v", err)
	}
	if len(engine.Requests) != 0 {
		t.Fatal("the service ran a session for a turn placed on a client")
	}
	if len(h.requests) != 1 || !reflectZero(h.requests[0].Execution) {
		t.Fatalf("the tools were hosted for the client: %+v", h.requests)
	}
	entries, err := os.ReadDir(filepath.Dir(captured.Workspace().Directory))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("a view the service built is left behind: %v", entries)
	}
}

func reflectZero(settings a.ExecutionSettings) bool {
	return settings.Mode == "" && settings.Image == "" && len(settings.Skills) == 0 && settings.Dagger == nil && settings.DaggerCloudTokenEnv == ""
}
