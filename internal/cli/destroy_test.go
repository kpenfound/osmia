package cli

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/service"
)

func openDesignSession(t *testing.T, root string, project config.ProjectID) config.WorkstreamID {
	t.Helper()
	var opened service.DesignResponse
	must(t, json.Unmarshal([]byte(successful(t, root, "design", "open", string(project), "--json")), &opened))
	return opened.Design.Workstream
}

func TestSessionDestroyReportsTheDestroyedSession(t *testing.T) {
	root, project, _ := sessionFixture(t)
	id := openSession(t, root, project).Workstream
	if out := successful(t, root, "session", "destroy", string(id)); out != "Session "+string(id)+" destroyed\n" {
		t.Fatalf("destroy text: %q", out)
	}
}

func TestSessionDestroyReportsAnEndedSessionAsJSON(t *testing.T) {
	root, project, _ := sessionFixture(t)
	id := openSession(t, root, project).Workstream
	successful(t, root, "session", "end", string(id))
	var result service.SessionDestroyResponse
	must(t, json.Unmarshal([]byte(successful(t, root, "session", "destroy", string(id), "--json")), &result))
	if !result.Destroyed || result.Project != project || result.Workstream != id {
		t.Fatalf("destroy JSON: %+v", result)
	}
}

func TestSessionDestroyRemovesTheSession(t *testing.T) {
	root, project, _ := sessionFixture(t)
	c := service.NewClient(filepath.Join(root, "osmia.sock"))
	defer c.Close()
	successful(t, root, "session", "destroy", string(openSession(t, root, project).Workstream))
	list, err := c.Sessions(context.Background(), project)
	must(t, err)
	if len(list.Sessions) != 0 {
		t.Fatalf("sessions after destroy: %+v", list.Sessions)
	}
}

func TestSessionDestroyTwiceIsNotFound(t *testing.T) {
	root, project, _ := sessionFixture(t)
	id := openSession(t, root, project).Workstream
	successful(t, root, "session", "destroy", string(id))
	code, out, diag := invoke(t, root, "session", "destroy", string(id))
	if code != 4 || out != "" || diag != "not_found: session not found\n" {
		t.Fatalf("destroy again: %d %q %q", code, out, diag)
	}
}

func TestDesignDestroyReportsTheDestroyedSession(t *testing.T) {
	root, project, _ := handInFixture(t)
	id := openDesignSession(t, root, project)
	if out := successful(t, root, "design", "destroy", string(id)); out != "Design session "+string(id)+" destroyed\n" {
		t.Fatalf("destroy text: %q", out)
	}
}

func TestDesignDestroyReportsTheDestroyedSessionAsJSON(t *testing.T) {
	root, project, _ := handInFixture(t)
	id := openDesignSession(t, root, project)
	var result service.SessionDestroyResponse
	must(t, json.Unmarshal([]byte(successful(t, root, "design", "destroy", string(id), "--json")), &result))
	if !result.Destroyed || result.Project != project || result.Workstream != id {
		t.Fatalf("destroy JSON: %+v", result)
	}
}

func TestDesignDestroyRemovesTheSession(t *testing.T) {
	root, project, _ := handInFixture(t)
	successful(t, root, "design", "destroy", string(openDesignSession(t, root, project)))
	var list service.DesignsResponse
	must(t, json.Unmarshal([]byte(successful(t, root, "design", "list", string(project), "--json")), &list))
	if len(list.Designs) != 0 {
		t.Fatalf("designs after destroy: %+v", list.Designs)
	}
}

func TestDestroyedDesignSessionIsNotFound(t *testing.T) {
	root, project, _ := handInFixture(t)
	id := openDesignSession(t, root, project)
	successful(t, root, "design", "destroy", string(id))
	for _, command := range []string{"destroy", "read"} {
		code, out, diag := invoke(t, root, "design", command, string(id))
		if code != 4 || out != "" || !strings.HasPrefix(diag, "not_found: ") {
			t.Fatalf("%s after destroy: %d %q %q", command, code, out, diag)
		}
	}
}

func TestDestroyRefusesAWorkgroup(t *testing.T) {
	root, _, _ := handInFixture(t)
	for _, kind := range []string{"session", "design"} {
		code, out, diag := invoke(t, root, kind, "destroy", "g_0123456789abcdef0123456789abcdef")
		if code != 5 || out != "" || diag != "conflict: workgroups cannot be destroyed; only sessions can be destroyed\n" {
			t.Fatalf("%s workgroup: %d %q %q", kind, code, out, diag)
		}
	}
}

func TestDestroyRefusesAWorkstreamThatIsNotASessionAndKeepsIt(t *testing.T) {
	root, project, _ := handInFixture(t)
	other := handInStdin(t, root, project, "# Keep this feature\n")
	for _, kind := range []string{"session", "design"} {
		for _, flag := range [][]string{nil, {"--json"}} {
			code, out, diag := invoke(t, root, append([]string{kind, "destroy", other}, flag...)...)
			if code != 5 || out != "" || diag != "conflict: only an open design session or an exploratory session can be destroyed\n" {
				t.Fatalf("%s non-session: %d %q %q", kind, code, out, diag)
			}
		}
	}
	successful(t, root, "status", other)
}

func TestDesignDestroyRefusesWhileATurnRunsAndKeepsTheSession(t *testing.T) {
	root, project, _ := handInFixture(t)
	id := string(openDesignSession(t, root, project))
	successful(t, root, "design", "send", id, "Discuss this feature")
	code, out, diag := invoke(t, root, "design", "destroy", id)
	if code != 5 || out != "" || diag != "conflict: a turn or control is running for this session; try again once it finishes\n" {
		t.Fatalf("busy design: %d %q %q", code, out, diag)
	}
	if !strings.Contains(successful(t, root, "design", "list"), id) {
		t.Fatal("refused design disappeared")
	}
}

func TestHelpListsTheDestroyCommands(t *testing.T) {
	root, _, _ := handInFixture(t)
	help := successful(t, root, "--help")
	for _, command := range []string{"session destroy <session-id> [--json]", "design destroy <design-id> [--json]"} {
		if !strings.Contains(help, command) {
			t.Fatalf("help missing %q", command)
		}
	}
}

type heldDestroyChecks struct {
	fakeSessionChecks
	started, release chan struct{}
}

func (c heldDestroyChecks) Check(ctx context.Context, _ string, _ api.CheckIdentity, _ []string) (service.CheckResult, error) {
	close(c.started)
	select {
	case <-c.release:
		return service.CheckResult{ExitCode: 0}, nil
	case <-ctx.Done():
		return service.CheckResult{}, ctx.Err()
	}
}

type inFlightCheck struct {
	root   string
	id     config.WorkstreamID
	client *service.Client
	ctx    context.Context
	checks heldDestroyChecks
	done   chan error
}

func startInFlightCheck(t *testing.T) inFlightCheck {
	t.Helper()
	checks := heldDestroyChecks{started: make(chan struct{}), release: make(chan struct{}, 1)}
	root, project, _ := sessionFixtureWithChecks(t, checks)
	t.Cleanup(func() { close(checks.release) })
	id := openSession(t, root, project).Workstream
	c := service.NewClient(filepath.Join(root, "osmia.sock"))
	t.Cleanup(c.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	checked := make(chan error, 1)
	go func() {
		_, err := c.SessionCheck(ctx, id)
		checked <- err
	}()
	select {
	case <-checks.started:
	case <-ctx.Done():
		t.Fatal("check did not start")
	}
	return inFlightCheck{root: root, id: id, client: c, ctx: ctx, checks: checks, done: checked}
}

func TestSessionDestroyRefusesWhileACheckIsInFlightAndKeepsTheSession(t *testing.T) {
	f := startInFlightCheck(t)
	code, out, diag := invoke(t, f.root, "session", "destroy", string(f.id), "--json")
	if code != 5 || out != "" || diag != "conflict: a turn or control is running for this session; try again once it finishes\n" {
		t.Fatalf("busy session: %d %q %q", code, out, diag)
	}
	_, err := f.client.Session(f.ctx, f.id)
	must(t, err)
}

func TestSessionDestroySucceedsOnceTheCheckFinishes(t *testing.T) {
	f := startInFlightCheck(t)
	f.checks.release <- struct{}{}
	must(t, <-f.done)
	successful(t, f.root, "session", "destroy", string(f.id))
}
