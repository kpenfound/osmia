package cli

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/service"
)

func TestSessionDestroy(t *testing.T) {
	root, project, _ := sessionFixture(t)
	c := service.NewClient(filepath.Join(root, "osmia.sock"))
	defer c.Close()
	for _, ended := range []bool{false, true} {
		var opened service.SessionResponse
		must(t, json.Unmarshal([]byte(successful(t, root, "session", "open", string(project), "--profile", "default", "--json")), &opened))
		id := opened.Session.Workstream
		args := []string{"session", "destroy", string(id)}
		if ended {
			successful(t, root, "session", "end", string(id))
			args = append(args, "--json")
		}
		out := successful(t, root, args...)
		if ended {
			var result service.SessionDestroyResponse
			must(t, json.Unmarshal([]byte(out), &result))
			if !result.Destroyed || result.Project != project || result.Workstream != id {
				t.Fatalf("destroy JSON: %+v", result)
			}
		} else if out != "Session "+string(id)+" destroyed\n" {
			t.Fatalf("destroy text: %q", out)
		}
		list, err := c.Sessions(context.Background(), project)
		must(t, err)
		if len(list.Sessions) != 0 {
			t.Fatalf("sessions after destroy: %+v", list.Sessions)
		}
		code, out, diag := invoke(t, root, args...)
		if code != 4 || out != "" || diag != "not_found: session not found\n" {
			t.Fatalf("destroy again: %d %q %q", code, out, diag)
		}
	}
}

func TestDesignDestroy(t *testing.T) {
	root, project, _ := handInFixture(t)
	for _, asJSON := range []bool{false, true} {
		var opened service.DesignResponse
		must(t, json.Unmarshal([]byte(successful(t, root, "design", "open", string(project), "--json")), &opened))
		id := opened.Design.Workstream
		args := []string{"design", "destroy", string(id)}
		if asJSON {
			args = append(args, "--json")
		}
		out := successful(t, root, args...)
		if asJSON {
			var result service.SessionDestroyResponse
			must(t, json.Unmarshal([]byte(out), &result))
			if !result.Destroyed || result.Project != project || result.Workstream != id {
				t.Fatalf("destroy JSON: %+v", result)
			}
		} else if out != "Design session "+string(id)+" destroyed\n" {
			t.Fatalf("destroy text: %q", out)
		}
		var list service.DesignsResponse
		must(t, json.Unmarshal([]byte(successful(t, root, "design", "list", string(project), "--json")), &list))
		if len(list.Designs) != 0 {
			t.Fatalf("designs after destroy: %+v", list.Designs)
		}
		for _, command := range []string{"destroy", "read"} {
			code, out, diag := invoke(t, root, "design", command, string(id))
			if code != 4 || out != "" || !strings.HasPrefix(diag, "not_found: ") {
				t.Fatalf("%s after destroy: %d %q %q", command, code, out, diag)
			}
		}
	}
}

func TestDestroyRefusals(t *testing.T) {
	root, project, _ := handInFixture(t)
	other := handInStdin(t, root, project, "# Keep this feature\n")
	for _, kind := range []string{"session", "design"} {
		code, out, diag := invoke(t, root, kind, "destroy", "g_0123456789abcdef0123456789abcdef")
		if code != 5 || out != "" || diag != "conflict: workgroups cannot be destroyed; only sessions can be destroyed\n" {
			t.Fatalf("%s workgroup: %d %q %q", kind, code, out, diag)
		}
		for _, flag := range [][]string{nil, {"--json"}} {
			code, out, diag := invoke(t, root, append([]string{kind, "destroy", other}, flag...)...)
			if code != 5 || out != "" || diag != "conflict: only an open design session or an exploratory session can be destroyed\n" {
				t.Fatalf("%s non-session: %d %q %q", kind, code, out, diag)
			}
		}
	}
	successful(t, root, "status", other)
	var opened service.DesignResponse
	must(t, json.Unmarshal([]byte(successful(t, root, "design", "open", string(project), "--json")), &opened))
	id := string(opened.Design.Workstream)
	successful(t, root, "design", "send", id, "Discuss this feature")
	code, out, diag := invoke(t, root, "design", "destroy", id)
	if code != 5 || out != "" || diag != "conflict: a turn or control is running for this session; try again once it finishes\n" {
		t.Fatalf("busy design: %d %q %q", code, out, diag)
	}
	if !strings.Contains(successful(t, root, "design", "list"), id) {
		t.Fatal("refused design disappeared")
	}
	for _, command := range []string{"session destroy <session-id> [--json]", "design destroy <design-id> [--json]"} {
		if !strings.Contains(successful(t, root, "--help"), command) {
			t.Fatalf("help missing %q", command)
		}
	}
}

type heldDestroyChecks struct {
	fakeSessionChecks
	started, release chan struct{}
}

func (c heldDestroyChecks) Check(ctx context.Context, _ string, _ []string) (service.CheckResult, error) {
	close(c.started)
	select {
	case <-c.release:
		return service.CheckResult{ExitCode: 0}, nil
	case <-ctx.Done():
		return service.CheckResult{}, ctx.Err()
	}
}

func TestSessionDestroyRefusesInFlightCheck(t *testing.T) {
	checks := heldDestroyChecks{started: make(chan struct{}), release: make(chan struct{}, 1)}
	root, project, _ := sessionFixtureWithChecks(t, checks)
	// Release the checker before the fixture closes, even on a failed assertion.
	defer close(checks.release)
	var opened service.SessionResponse
	must(t, json.Unmarshal([]byte(successful(t, root, "session", "open", string(project), "--profile", "default", "--json")), &opened))
	id := opened.Session.Workstream
	c := service.NewClient(filepath.Join(root, "osmia.sock"))
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
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
	code, out, diag := invoke(t, root, "session", "destroy", string(id), "--json")
	if code != 5 || out != "" || diag != "conflict: a turn or control is running for this session; try again once it finishes\n" {
		t.Fatalf("busy session: %d %q %q", code, out, diag)
	}
	_, err := c.Session(ctx, id)
	must(t, err)
	checks.release <- struct{}{}
	must(t, <-checked)
	successful(t, root, "session", "destroy", string(id))
}
