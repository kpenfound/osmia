// Package cli implements the local operator commands over the service API.
package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/service"
)

const usage = `Usage: osmia <command> [--root PATH]
  serve
  status [workstream-id] [--json]
  project add <name> --upstream OWNER/REPO --fork OWNER/REPO --clone PATH [--base-branch NAME] [--json]
  project remove <project-id> [--json]
  project extract <project-id> [--json]
  handin <project-id> <path|issue-url|-> [--json]
  abandon <workstream-id> <reason> [--json]
  send <workstream-id> <message> [--json]
  conversation <workstream-id> [--json]
  inbox [--json]
  answer <inbox-number> <ruling> [--json]
  pause <all|project-id|workstream-id> [--hard] [--reason TEXT] [--json]
  resume <all|project-id|workstream-id> [--json]
  priority set <workstream-id>... | priority clear [--json]
  profiles [set <role> <profile>|clear <role>] [--json]
Client commands also accept --socket PATH (relative to root).
Only these commands are available; serve runs in the foreground.
`

type options struct {
	root, socket, reason                string
	upstream, fork, clone, baseBranch   string
	json, hard, reasonSet, help, target bool
	args                                []string
}

func parse(args []string) (o options, err error) {
	seen := map[string]bool{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") || a == "-" {
			o.args = append(o.args, a)
			continue
		}
		key, value, has := strings.Cut(a, "=")
		if seen[key] {
			return o, errors.New("duplicate flag")
		}
		seen[key] = true
		switch key {
		case "--root", "--socket", "--reason", "--upstream", "--fork", "--clone", "--base-branch":
			if !has {
				i++
				if i >= len(args) {
					return o, errors.New("missing flag value")
				}
				value = args[i]
			}
			if value == "" && key != "--reason" {
				return o, errors.New("empty value")
			}
			switch key {
			case "--root":
				o.root = value
			case "--socket":
				o.socket = value
			case "--reason":
				o.reason = value
				o.reasonSet = true
			case "--upstream":
				o.upstream = value
				o.target = true
			case "--fork":
				o.fork = value
				o.target = true
			case "--clone":
				o.clone = value
				o.target = true
			case "--base-branch":
				o.baseBranch = value
				o.target = true
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

// Run reads hand-in input from stdin, writes results to stdout and
// diagnostics to stderr, and returns a documented exit code. Client execution
// never loads configuration or opens runtime files.
func Run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
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
	case "serve":
		valid = len(a) == 0
	case "status":
		valid = len(a) <= 1
	case "send", "abandon":
		valid = len(a) == 2
	case "conversation":
		valid = len(a) == 1
	case "inbox":
		valid = len(a) == 0
	case "answer":
		valid = len(a) == 2
	case "pause", "resume":
		valid = len(a) == 1
	case "handin":
		valid = len(a) == 2
	case "priority":
		valid = len(a) >= 2 && a[0] == "set" || len(a) == 1 && a[0] == "clear"
	case "profiles":
		valid = len(a) == 0 || len(a) == 3 && a[0] == "set" || len(a) == 2 && a[0] == "clear"
	case "project":
		valid = len(a) == 2 && (a[0] == "add" && o.upstream != "" && o.fork != "" && o.clone != "" || a[0] == "remove" || a[0] == "extract")
	}
	addingProject := cmd == "project" && len(a) > 0 && a[0] == "add"
	if !valid || cmd != "pause" && (o.hard || o.reasonSet) || !addingProject && o.target || cmd == "serve" && (o.json || o.socket != "") {
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
	fail := func(err error) int {
		return report(stderr, err, cmd == "project" || cmd == "handin" || cmd == "abandon" || cmd == "send" || cmd == "conversation" || cmd == "inbox" || cmd == "answer" || cmd == "status" && len(a) == 1)
	}
	noProject := func() int {
		fmt.Fprintln(stderr, "no project is configured; add one with osmia project add")
		return 4
	}
	if cmd == "project" && a[0] == "extract" {
		id, err := config.ParseProjectID(a[1])
		if err != nil {
			return invalid()
		}
		result, err := c.ExtractProject(ctx, id)
		if err != nil {
			return fail(err)
		}
		if o.json {
			return output(stdout, stderr, result)
		}
		fmt.Fprintf(stdout, "Extraction %d of project %s (%s) started\nFollow it with osmia status\n", result.Extraction.Extraction, result.Project.ID, result.Project.Name)
		return 0
	}
	if cmd == "project" {
		var result service.ProjectResponse
		if a[0] == "add" {
			clone, err := filepath.Abs(o.clone)
			if err != nil {
				return invalid()
			}
			result, err = c.AddProject(ctx, service.ProjectAddRequest{Name: a[1], Upstream: o.upstream, Fork: o.fork, Clone: clone, BaseBranch: o.baseBranch})
			if err != nil {
				return fail(err)
			}
		} else {
			id, err := config.ParseProjectID(a[1])
			if err != nil {
				return invalid()
			}
			result, err = c.RemoveProject(ctx, id)
			if err != nil {
				return fail(err)
			}
		}
		if o.json {
			return output(stdout, stderr, result)
		}
		verb := "added"
		if a[0] == "remove" {
			verb = "removed"
		}
		fmt.Fprintf(stdout, "Project %s (%s) %s\nUpstream: %s fork: %s clone: %s\nTrace: %s\nNext: %s\n", result.Project.ID, result.Project.Name, verb, result.Project.Upstream, result.Project.Fork, result.Project.Clone, result.Project.Trace, result.NextStep)
		return 0
	}
	if cmd == "handin" {
		id, err := config.ParseProjectID(a[0])
		if err != nil {
			return invalid()
		}
		key, err := handInKey()
		if err != nil {
			fmt.Fprintln(stderr, "cannot generate a hand-in key")
			return 1
		}
		req := service.HandInRequest{Project: id, Key: key}
		switch input := a[1]; {
		case input == "-":
			data, err := io.ReadAll(io.LimitReader(stdin, service.MaxHandedBytes+1))
			if err != nil {
				fmt.Fprintln(stderr, "cannot read the hand-in from stdin")
				return 1
			}
			if len(data) > service.MaxHandedBytes {
				fmt.Fprintf(stderr, "stdin is larger than %d bytes; hand in a smaller input\n", service.MaxHandedBytes)
				return 4
			}
			if !utf8.Valid(data) {
				fmt.Fprintln(stderr, "stdin is not UTF-8 text; hand in a text input")
				return 4
			}
			text := string(data)
			req.Stdin = &text
		case strings.HasPrefix(input, "https://") || strings.HasPrefix(input, "http://"):
			req.URL = input
		case input == "":
			return invalid()
		default:
			abs, err := filepath.Abs(input)
			if err != nil {
				return invalid()
			}
			req.Path = abs
		}
		result, err := c.HandIn(ctx, req)
		if err != nil {
			return fail(err)
		}
		if o.json {
			return output(stdout, stderr, result)
		}
		fmt.Fprintf(stdout, "Workstream %s handed in to project %s\nState: %s\nHanded: %s\nSource: %s\n", result.Workstream, result.Project, result.State, result.Handed, result.Source)
		return 0
	}
	if cmd == "abandon" {
		id, err := config.ParseWorkstreamID(a[0])
		if err != nil {
			return invalid()
		}
		result, err := c.Abandon(ctx, id, a[1])
		if err != nil {
			return fail(err)
		}
		if o.json {
			return output(stdout, stderr, result)
		}
		fmt.Fprintf(stdout, "Workstream %s abandoned\nReason: %s\n", result.Workstream, result.Reason)
		return 0
	}
	if cmd == "send" || cmd == "conversation" {
		id, err := config.ParseWorkstreamID(a[0])
		if err != nil {
			return invalid()
		}
		if cmd == "send" {
			entry, err := c.Send(ctx, id, a[1])
			if err != nil {
				return fail(err)
			}
			if o.json {
				return output(stdout, stderr, entry)
			}
			fmt.Fprintf(stdout, "Message %s sent to the chief of staff of %s: %s\n", entry.Turn, id, entry.State)
			return 0
		}
		list, err := c.Conversation(ctx, id)
		if err != nil {
			return fail(err)
		}
		if o.json {
			return output(stdout, stderr, list)
		}
		showConversation(stdout, list)
		return 0
	}
	if cmd == "inbox" {
		list, err := c.Inbox(ctx)
		if err != nil {
			return fail(err)
		}
		if o.json {
			return output(stdout, stderr, list)
		}
		showInbox(stdout, list)
		return 0
	}
	if cmd == "answer" {
		number, err := strconv.Atoi(a[0])
		if err != nil || number < 1 {
			return invalid()
		}
		result, err := c.Answer(ctx, number, a[1])
		if err != nil {
			return fail(err)
		}
		if o.json {
			return output(stdout, stderr, result)
		}
		fmt.Fprintf(stdout, "Ruling recorded on inbox entry %d (%s, questions %s)\nThe chief of staff relays it to the askers.\n", result.Number, result.Workstream, strings.Join(result.Questions, ", "))
		return 0
	}
	if cmd == "status" && len(a) == 1 {
		id, err := config.ParseWorkstreamID(a[0])
		if err != nil {
			return invalid()
		}
		st, err := c.Status(ctx, id)
		if err != nil {
			return fail(err)
		}
		if o.json {
			return output(stdout, stderr, st)
		}
		showStatus(stdout, st)
		return 0
	}
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
		all, err := c.Statuses(ctx)
		if err != nil {
			return fail(err)
		}
		if o.json {
			return output(stdout, stderr, struct {
				Health        service.HealthResponse  `json:"health"`
				Configuration service.ConfigResponse  `json:"configuration"`
				Runtime       service.RuntimeResponse `json:"runtime"`
				Status        service.StatusResponse  `json:"status"`
			}{h, cfg, rt, all})
		}
		fmt.Fprintf(stdout, "Service: %s ready=%t API=%d\nConfiguration: %s (%s)\n", h.Service, h.Ready, h.APIVersion, cfg.Digest, cfg.Root)
		showProject(stdout, cfg.Project)
		for _, p := range rt.Projects {
			fmt.Fprintf(stdout, "Context: %s context_mode=%s\n", p.Project, p.ContextMode)
		}
		diagnostics(stdout, cfg.Diagnostics)
		showRuntime(stdout, rt)
		showWorkstreams(stdout, all)
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
				if cfg.Project == nil {
					return noProject()
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
		if cfg.Project == nil {
			return noProject()
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
func showProject(w io.Writer, p *service.ProjectView) {
	if p == nil {
		fmt.Fprintln(w, "Project: none configured; add one with osmia project add <name> --upstream OWNER/REPO --fork OWNER/REPO --clone PATH")
		return
	}
	fmt.Fprintf(w, "Project: %s (%s) upstream=%s fork=%s clone=%s\nTrace: %s\n", p.ID, p.Name, p.Upstream, p.Fork, p.Clone, p.Trace)
	if c := p.CharterState; c != nil {
		state := "ready"
		if !c.Ready {
			state = "empty; write numbered rules before handing in work"
		}
		fmt.Fprintf(w, "Charter: %s (%d rules, revision %d) %s\n", state, c.Rules, c.Revision, p.Charter)
		for _, d := range c.Diagnostics {
			fmt.Fprintf(w, "  charter.md:%d: %s\n", d.Line, d.Message)
		}
	}
	if x := p.Extraction; x != nil {
		line := fmt.Sprintf("Knowledge base: extraction %d %s at %s", x.Extraction, x.State, x.At.UTC().Format(time.RFC3339))
		if x.Reason != "" {
			line += ": " + x.Reason
		}
		fmt.Fprintln(w, line)
	}
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

// showWorkstreams prints each workstream's goal and attention.
func showWorkstreams(w io.Writer, all service.StatusResponse) {
	fmt.Fprintln(w, "Workstreams:")
	if len(all.Workstreams) == 0 && len(all.Diagnostics) == 0 {
		fmt.Fprintln(w, "  none")
	}
	defer diagnostics(w, all.Diagnostics)
	for _, st := range all.Workstreams {
		fmt.Fprintf(w, "  %s %s\n", st.Workstream, facts(st))
		if st.Status == nil {
			fmt.Fprintln(w, "    no status yet")
			continue
		}
		fmt.Fprintf(w, "    Goal: %s\n    Attention: %s\n", st.Status.Goal, attention(st.Status.Attention))
	}
}

// showStatus prints one workstream's full status.
func showStatus(w io.Writer, st service.WorkstreamStatus) {
	fmt.Fprintf(w, "Workstream: %s %s\n", st.Workstream, facts(st))
	if st.Status == nil {
		fmt.Fprintln(w, "Status: none yet; the chief of staff has not written one")
		return
	}
	s := st.Status
	fmt.Fprintf(w, "Goal: %s\nAttention: %s\nNote: %s\nAgents:\n", s.Goal, attention(s.Attention), s.Note)
	if len(s.Agents) == 0 {
		fmt.Fprintln(w, "  none active")
	}
	for _, a := range s.Agents {
		fmt.Fprintf(w, "  %s\n", a)
	}
	fmt.Fprintf(w, "Updated: %s (revision %d)\n", s.UpdatedAt.Format(time.RFC3339), s.Revision)
}

// showConversation prints each entry's time, author, turn and state, then its
// text indented.
func showConversation(w io.Writer, list service.ConversationResponse) {
	fmt.Fprintf(w, "Conversation: %s\n", list.Workstream)
	if len(list.Entries) == 0 {
		fmt.Fprintf(w, "  no messages yet; send one with osmia send %s \"...\"\n", list.Workstream)
	}
	for _, e := range list.Entries {
		author := "owner"
		if e.Kind == "response" {
			author = "chief of staff"
		}
		fmt.Fprintf(w, "%s %s [%s %s]\n", e.At.Format(time.RFC3339), author, e.Turn, e.State)
		for _, line := range strings.Split(strings.TrimRight(e.Text, "\n"), "\n") {
			fmt.Fprintf(w, "  %s\n", line)
		}
	}
}

// showInbox prints each entry's number and rephrased question, then what it
// blocks, the options, the recommendation and the questions as asked.
func showInbox(w io.Writer, list service.InboxResponse) {
	if len(list.Entries) == 0 {
		fmt.Fprintln(w, "Inbox: no questions are waiting for you")
		return
	}
	fmt.Fprintf(w, "Inbox: %d waiting; answer one with osmia answer <number> \"...\"\n", len(list.Entries))
	block := func(label, text string) {
		lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
		fmt.Fprintf(w, "  %s: %s\n", label, lines[0])
		for _, line := range lines[1:] {
			fmt.Fprintf(w, "    %s\n", line)
		}
	}
	for _, e := range list.Entries {
		fmt.Fprintf(w, "\n[%d] %s %s (%s)\n", e.Number, e.EscalatedAt.Format(time.RFC3339), e.Workstream, e.Batch)
		block("Question", e.Question)
		block("Blocked", e.Blocked)
		for i, option := range e.Options {
			block(fmt.Sprintf("Option %d", i+1), option)
		}
		block("Recommendation", e.Recommendation)
		for _, q := range e.Asked {
			block(fmt.Sprintf("Asked by %s as question %s", q.AskedBy, q.ID), q.Question)
		}
	}
}

func facts(st service.WorkstreamStatus) string {
	state := "not recorded"
	if st.State != nil {
		state = *st.State
	}
	return fmt.Sprintf("state=%s open_questions=%d context_mode=%s", state, st.OpenQuestions, st.ContextMode)
}

func attention(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// handInKey identifies one hand-in command, so the service creates one
// workstream however often the request reaches it.
func handInKey() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "cli-" + hex.EncodeToString(b[:]), nil
}

// report maps an API error to exit code and guidance. Project operations
// compose their own messages from the caller's fields and identities, so those
// are shown; other messages are fixed to keep raw file and parser text out.
func report(w io.Writer, err error, project bool) int {
	var api *service.APIError
	if !errors.As(err, &api) {
		fmt.Fprintln(w, "invalid service response or internal failure; check service and client versions")
		return 1
	}
	code, msg := 5, "service operation failed; check service status"
	switch api.Code {
	case service.NoProject:
		code, msg = 4, "no project is configured; add one with osmia project add"
	case service.ProjectActive:
		msg = "a project is already active; remove it before adding another"
	case service.NotFound:
		code, msg = 4, "project not found; check the project ID with status"
	case service.CharterEmpty:
		code, msg = 4, "the project's charter has no rules; write the charter before handing in work"
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
	if project && api.Code != service.Unavailable && api.Code != service.Malformed && api.Message != "" {
		msg = api.Message
	}
	fmt.Fprintf(w, "%s: %s\n", api.Code, msg)
	return code
}
