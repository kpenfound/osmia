// Package cli implements the local operator commands over the service API.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"path/filepath"
	"slices"
	"strings"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/service"
)

const usage = `Usage: osmia <command> [--root PATH]
  serve
  status [--json]
  pause <all|project-id|workstream-id> [--hard] [--reason TEXT] [--json]
  resume <all|project-id|workstream-id> [--json]
  priority set <workstream-id>... | priority clear [--json]
  profiles [set <role> <profile>|clear <role>] [--json]
Client commands also accept --socket PATH (relative to root).
Only these M1 commands are available; serve runs in the foreground.
`

type options struct {
	root, socket, reason        string
	json, hard, reasonSet, help bool
	args                        []string
}

func parse(args []string) (o options, err error) {
	seen := map[string]bool{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			o.args = append(o.args, a)
			continue
		}
		key, value, has := strings.Cut(a, "=")
		if seen[key] {
			return o, errors.New("duplicate flag")
		}
		seen[key] = true
		switch key {
		case "--root", "--socket", "--reason":
			if !has {
				i++
				if i >= len(args) {
					return o, errors.New("missing flag value")
				}
				value = args[i]
			}
			if value == "" && key != "--reason" {
				return o, errors.New("empty path")
			}
			switch key {
			case "--root":
				o.root = value
			case "--socket":
				o.socket = value
			case "--reason":
				o.reason = value
				o.reasonSet = true
			}
		case "--json", "--hard", "--help", "-h":
			if has {
				return o, errors.New("boolean flags take no value")
			}
			switch key {
			case "--json":
				o.json = true
			case "--hard":
				o.hard = true
			default:
				o.help = true
			}
		default:
			return o, errors.New("unknown flag")
		}
	}
	return o, nil
}

// Run writes results to stdout, diagnostics to stderr, and returns a documented
// exit code. Client execution never loads configuration or opens runtime files.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	o, err := parse(args)
	invalid := func() int { fmt.Fprintln(stderr, "invalid arguments; use osmia --help"); return 2 }
	if err != nil {
		return invalid()
	}
	if o.help {
		fmt.Fprint(stdout, usage)
		return 0
	}
	if len(o.args) == 0 {
		return invalid()
	}
	cmd := o.args[0]
	a := o.args[1:]
	valid := false
	switch cmd {
	case "serve", "status":
		valid = len(a) == 0
	case "pause", "resume":
		valid = len(a) == 1
	case "priority":
		valid = len(a) >= 2 && a[0] == "set" || len(a) == 1 && a[0] == "clear"
	case "profiles":
		valid = len(a) == 0 || len(a) == 3 && a[0] == "set" || len(a) == 2 && a[0] == "clear"
	}
	if !valid || cmd != "pause" && (o.hard || o.reasonSet) || cmd == "serve" && (o.json || o.socket != "") {
		return invalid()
	}
	root, err := config.ResolveRoot(o.root, "")
	if err != nil {
		fmt.Fprintln(stderr, "invalid root; choose an accessible directory with --root")
		return 2
	}
	if cmd == "serve" {
		if err := service.Run(ctx, service.Options{Config: config.Options{Root: root.String()}}); err != nil {
			// Startup errors may contain raw TOML values or paths; do not echo them.
			fmt.Fprintln(stderr, "service startup failed: check root/configuration/runtime permissions and validity; stop any existing owner before starting; socket must be unused or stale")
			return 6
		}
		return 0
	}
	socket := o.socket
	if socket == "" {
		socket = "osmia.sock"
	}
	if !filepath.IsAbs(socket) {
		socket = filepath.Join(root.String(), socket)
	}
	c := service.NewClient(socket)
	defer c.Close()
	fail := func(err error) int { return report(stderr, err) }
	if cmd == "status" {
		h, err := c.Health(ctx)
		if err != nil {
			return fail(err)
		}
		cfg, err := c.Configuration(ctx)
		if err != nil {
			return fail(err)
		}
		rt, err := c.Runtime(ctx)
		if err != nil {
			return fail(err)
		}
		if o.json {
			return output(stdout, stderr, struct {
				Health        service.HealthResponse  `json:"health"`
				Configuration service.ConfigResponse  `json:"configuration"`
				Runtime       service.RuntimeResponse `json:"runtime"`
			}{h, cfg, rt})
		}
		fmt.Fprintf(stdout, "Service: %s ready=%t API=%d\nConfiguration: %s (%s)\n", h.Service, h.Ready, h.APIVersion, cfg.Digest, cfg.Root)
		diagnostics(stdout, cfg.Diagnostics)
		showRuntime(stdout, rt)
		return 0
	}
	if cmd == "profiles" && len(a) == 0 {
		rt, err := c.Runtime(ctx)
		if err != nil {
			return fail(err)
		}
		if o.json {
			return output(stdout, stderr, rt)
		}
		showRuntime(stdout, rt)
		return 0
	}
	var input any
	method, kind, scope := "PUT", "", ""
	switch cmd {
	case "pause", "resume":
		target := service.ClearPauseRequest{Scope: "factory"}
		if a[0] != "all" {
			if p, err := config.ParseProjectID(a[0]); err == nil {
				target.Scope = "project"
				target.Project = p
			} else if w, err := config.ParseWorkstreamID(a[0]); err == nil {
				cfg, err := c.Configuration(ctx)
				if err != nil {
					return fail(err)
				}
				if cfg.Effective == nil {
					return fail(errors.New("missing configuration"))
				}
				target.Scope = "workstream"
				target.Project = cfg.Effective.Project.ID
				target.Workstream = w
			} else {
				return invalid()
			}
		}
		kind = "pause"
		scope = a[0]
		if cmd == "resume" {
			method = "DELETE"
			input = target
		} else {
			mode := "soft"
			if o.hard {
				mode = "hard"
			}
			input = service.PauseRequest{Target: target, Mode: mode, Reason: o.reason, Source: "operator"}
		}
	case "priority":
		ids := []config.WorkstreamID{}
		if a[0] == "set" {
			for _, s := range a[1:] {
				id, err := config.ParseWorkstreamID(s)
				if err != nil {
					return invalid()
				}
				ids = append(ids, id)
			}
		}
		cfg, err := c.Configuration(ctx)
		if err != nil {
			return fail(err)
		}
		if cfg.Effective == nil {
			return fail(errors.New("missing configuration"))
		}
		project := cfg.Effective.Project.ID
		scope = string(project)
		kind = "priority"
		if a[0] == "clear" {
			method = "DELETE"
			input = service.ClearPriorityRequest{Project: project}
		} else {
			input = service.PriorityRequest{Project: project, Workstreams: ids}
		}
	case "profiles":
		kind = "profile"
		scope = a[1]
		if a[0] == "clear" {
			method = "DELETE"
			input = service.ClearProfileRequest{Role: a[1]}
		} else {
			input = service.ProfileRequest{Role: a[1], Profile: a[2]}
		}
	}
	var ack service.MutationResponse
	if err := c.Do(ctx, method, service.Prefix+"/runtime/"+kind, input, &ack); err != nil {
		return fail(err)
	}
	rt, err := c.Runtime(ctx)
	if err != nil {
		fmt.Fprintln(stderr, "mutation acknowledged; effective-state refresh failed; run status before retrying")
		return fail(err)
	}
	if o.json {
		return output(stdout, stderr, struct {
			Mutation service.MutationResponse `json:"mutation"`
			Runtime  service.RuntimeResponse  `json:"runtime"`
		}{ack, rt})
	}
	fmt.Fprintf(stdout, "%s %s: applied=%t\n", cmd, scope, ack.Applied)
	showRuntime(stdout, rt)
	return 0
}

func output(w, stderr io.Writer, v any) int {
	if err := json.NewEncoder(w).Encode(v); err != nil {
		fmt.Fprintln(stderr, "cannot write output")
		return 1
	}
	return 0
}
func diagnostics(w io.Writer, ds []service.Diagnostic) {
	for _, d := range ds {
		fmt.Fprintf(w, "Diagnostic: %s: %s: %s\n", d.Field, d.Code, d.Message)
	}
}
func showRuntime(w io.Writer, rt service.RuntimeResponse) {
	fmt.Fprintln(w, "Pauses (absent scopes are unpaused):")
	for _, p := range rt.Effective.Pauses {
		fmt.Fprintf(w, "  %s %s %s: %s source=%s reason=%q\n", p.Target.Scope, p.Target.Project, p.Target.Workstream, p.Mode, p.Source, p.Reason)
	}
	fmt.Fprintln(w, "Priority (absent projects have no preference):")
	for _, p := range rt.Effective.Priorities {
		fmt.Fprintf(w, "  %s: %v\n", p.Project, p.Workstreams)
	}
	fmt.Fprintln(w, "Profiles:")
	for _, r := range slices.Sorted(maps.Keys(rt.Effective.Profiles)) {
		fmt.Fprintf(w, "  %s: %s\n", r, rt.Effective.Profiles[r])
	}
	diagnostics(w, rt.Diagnostics)
}
func report(w io.Writer, err error) int {
	var api *service.APIError
	if !errors.As(err, &api) {
		fmt.Fprintln(w, "invalid service response or internal failure; check service and client versions")
		return 1
	}
	code, msg := 5, "service operation failed; check service status"
	switch api.Code {
	case service.Malformed:
		code, msg = 4, "request rejected; check client and service API versions"
	case service.Validation:
		code, msg = 4, "invalid override; check target, role, profile and workstream IDs with status"
	case service.Conflict:
		msg = "runtime conflict; restore externally edited state or restart the service"
	case service.Unsupported:
		msg = "operation unavailable in M1; use osmia --help for supported commands"
	case service.RestartRequired:
		msg = "restart required; stop the service and run osmia serve again"
	case service.Unavailable:
		code, msg = 3, "cannot reach service; run osmia serve with the same --root and check --socket and permissions"
	}
	fmt.Fprintf(w, "%s: %s\n", api.Code, msg)
	return code
}
