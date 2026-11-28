package verifytool

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/kpenfound/osmia/internal/callargs"
	"github.com/kpenfound/osmia/internal/checks"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/isolation"
	"github.com/kpenfound/osmia/internal/redact"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/kpenfound/osmia/internal/verification"
)

// The timeout a call takes unless it names one, and the range it may name.
const (
	ToolName       = "verify"
	DefaultTimeout = 5 * time.Minute
	MinTimeout     = time.Minute
	MaxTimeout     = 30 * time.Minute
)

var serviceActor = trace.Actor{Kind: "service", ID: "osmia"}

// Input is the tool's input. Workstream and Unit are the manager's: a
// reviewer verifies its own candidate and the final reviewer its own review's
// head.
type Input struct {
	Call       []string            `json:"call"`
	Timeout    string              `json:"timeout,omitempty"`
	Workstream config.WorkstreamID `json:"workstream,omitempty"`
	Unit       string              `json:"unit,omitempty"`
}

// Claim is one claim of a run as the caller reads it. Evidence holds the view
// paths of the files the claim references.
type Claim struct {
	Name     string              `json:"name"`
	Status   verification.Status `json:"status"`
	Detail   string              `json:"detail,omitempty"`
	Flow     string              `json:"flow,omitempty"`
	Evidence []string            `json:"evidence,omitempty"`
}

// Result is what a verify call returns. A run whose report was rejected is
// inconclusive and carries its reason in Rejected. A call that produced no run
// result carries a Limitation instead. Nothing in it moves a unit or a
// workstream: the caller decides.
type Result struct {
	RunID       string                    `json:"run_id,omitempty"`
	Workstream  config.WorkstreamID       `json:"workstream,omitempty"`
	Commit      string                    `json:"commit,omitempty"`
	Status      verification.Status       `json:"status,omitempty"`
	Rejected    string                    `json:"rejected,omitempty"`
	Claims      []Claim                   `json:"claims,omitempty"`
	Flows       []verification.FlowStatus `json:"flows,omitempty"`
	Untagged    int                       `json:"untagged_claims,omitempty"`
	Report      string                    `json:"report,omitempty"`
	Images      []string                  `json:"images,omitempty"`
	Diagnostics string                    `json:"diagnostics,omitempty"`
	Limitation  string                    `json:"limitation,omitempty"`
}

// Operation is the trace record of one verify call, keyed by the tool call. It
// holds the target commit, the arguments and the run ID from the moment the
// call is first recorded, so a replay reuses the run ID and runs nothing.
type Operation struct {
	ID         string              `json:"id"`
	Scope      coreadapter.Scope   `json:"scope"`
	Call       json.RawMessage     `json:"call"`
	Workstream config.WorkstreamID `json:"workstream"`
	Unit       string              `json:"unit,omitempty"`
	Commit     string              `json:"commit"`
	Args       []string            `json:"args"`
	Timeout    string              `json:"timeout"`
	RunID      string              `json:"run_id"`
	Outcome    *Outcome            `json:"outcome,omitempty"`
	// Finished is set once the call's result was handed back to its caller.
	// A call with the same arguments in the same turn after that is a new call
	// with a run of its own; until then it is the same call, and a replay of
	// it returns what was recorded and runs nothing.
	Finished bool `json:"finished,omitempty"`
	// Rerun is set for a service rerun of an invocation a mason declared: the
	// operation is then recorded by the service, not by a tool call.
	Rerun *Rerun `json:"rerun,omitempty"`
}

// BuildResult is the result a recorded operation gives its caller. It reads
// only the record, so the same operation always gives the same result.
func BuildResult(op Operation) Result {
	r := Result{RunID: op.RunID, Workstream: op.Workstream, Commit: op.Commit}
	o := op.Outcome
	if o == nil {
		return r
	}
	if o.Limitation != "" {
		r.Limitation = o.Limitation
		return r
	}
	res := o.Result
	cov := verification.Attribute(res, nil)
	r.Status, r.Rejected, r.Flows, r.Untagged, r.Diagnostics = res.Status(), res.Rejected, cov.Flows, len(cov.Untagged), o.Diagnostics
	for _, c := range res.Claims {
		claim := Claim{Name: c.Name, Status: c.Status, Detail: c.Detail, Flow: c.Flow}
		for _, p := range c.Evidence {
			claim.Evidence = append(claim.Evidence, Ref(op.RunID, p))
		}
		r.Claims = append(r.Claims, claim)
	}
	for _, f := range o.Files {
		if f == verification.ReportFile {
			r.Report = Ref(op.RunID, f)
		}
	}
	for _, f := range o.Images {
		r.Images = append(r.Images, ImagePath(op.RunID, f))
	}
	return r
}

// Target is what one call runs against.
type Target struct {
	// Repo and Stream hold the evidence of the run: the workstream whose
	// commit is verified.
	Repo   *trace.Repository
	Stream config.WorkstreamID
	// Config is the configuration of the target's project, which holds the
	// verification setting the run uses.
	Config *config.Config
	// Unit is the unit whose candidate is verified, empty for a feature head.
	Unit string
	// Commit is the commit to export and verify.
	Commit string
	// Export writes the commit's files into dir. The lever's own arguments are
	// checked against what it writes.
	Export func(ctx context.Context, commit, dir string, validate func(string) error) error
}

// Resolver chooses the target of a new call. A refusal is returned to the
// caller as a limitation and starts nothing.
type Resolver func(ctx context.Context, in Input) (Target, string, error)

// Slots holds a checks slot for a run on stream until the returned release is
// called. It returns an error when the wait is cancelled.
type Slots func(ctx context.Context, project config.ProjectID, stream config.WorkstreamID) (release func(), err error)

// Env is what one holder of the tool needs.
type Env struct {
	// Config is the caller's configuration. The tool is available only when
	// its project has the verification setting.
	Config *config.Config
	Repo   *trace.Repository
	Scope  coreadapter.Scope
	View   isolation.View
	Now    func() time.Time
	Slots  Slots
	// Resolve chooses the target of a new call.
	Resolve Resolver
	// About describes what the call verifies for this holder, in the tool
	// description.
	About string
}

const description = "Run the project's verification lever on the service and record the result as evidence. " +
	"call is the lever argument list you write: a named flow, or ad-hoc steps the mason never ran, as the project's verification documentation and feature map describe them. " +
	"Only module arguments and paths within the exported source are allowed; CLI options, providers, environment names, a run ID and host-writing or interactive steps are refused. The service never interprets the arguments. " +
	"Every new call gets its own run ID and starts the app and its driver anew, so repeating a call is a new run; replaying a call already recorded returns its record and runs nothing. " +
	"timeout defaults to 5m and must be from 1m to 30m. " +
	"The result lists the run's claims with their status (pass, fail or inconclusive), detail, flow tag and evidence references, and its status for each flow the claims name. " +
	"Inconclusive is never a pass. The report and every file it references are in this read-only view under .osmia-verify/<run_id>/ at the paths the result gives, and images also under images/. " +
	"A result is evidence for you to judge, not a verdict: it never changes the unit or the workstream. Try paths the mason did not, and treat a missing, failed or inconclusive result on a user-visible acceptance item as a finding. " +
	"Limitations do not fail your turn. Recovered results from an interrupted turn are in .osmia-verify/results.json; read them before calling again."

// Tool builds the verify tool for one holder.
func Tool(e Env) coreadapter.Tool {
	var mu sync.Mutex
	properties := `"call":{"type":"array","items":{"type":"string"},"minItems":1},"timeout":{"type":"string"}`
	text := description
	if e.Scope.Role == trace.Manager {
		properties += `,"workstream":{"type":"string"},"unit":{"type":"string"}`
		text += " Set workstream to the member to verify (required for a shared manager). It verifies that member's feature-branch head, or its recorded candidate when you set unit."
	} else if e.About != "" {
		text += " It verifies " + e.About + "."
	}
	tool := coreadapter.Tool{Name: ToolName, Effect: coreadapter.ToolMemory, Description: text,
		InputSchema: json.RawMessage(`{"type":"object","properties":{` + properties + `},"required":["call"],"additionalProperties":false}`)}
	tool.Handle = func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
		mu.Lock()
		defer mu.Unlock()
		return e.handle(ctx, tool, raw)
	}
	return tool
}

func (e Env) handle(ctx context.Context, tool coreadapter.Tool, raw json.RawMessage) (json.RawMessage, error) {
	limitation := func(message string) (json.RawMessage, error) {
		return json.Marshal(Result{Limitation: Diagnostic(message)})
	}
	if e.Config == nil || !e.Config.Project.Verification.Enabled() {
		return limitation("this project has no verification setting")
	}
	var in Input
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&in); err != nil {
		return limitation("invalid verify arguments")
	}
	if e.Scope.Role != trace.Manager && (in.Workstream != "" || in.Unit != "") {
		return limitation("only the manager names a workstream or unit; omit them to verify your own target")
	}
	hash := strings.TrimPrefix(trace.ToolCallID(e.Scope, ToolName, raw), "tool-")
	own := config.WorkstreamID(e.Scope.Workstream)
	docs, err := latestDocuments(e.Repo, own)
	if err != nil {
		return limitation(err.Error())
	}
	var op Operation
	var doc trace.Document
	next := 1
	for ; ; next++ {
		d, found := docs[callID(hash, next)]
		if !found {
			break
		}
		doc = d
	}
	if doc.ID != "" {
		if err := json.Unmarshal([]byte(doc.Content), &op); err != nil {
			return limitation(err.Error())
		}
		if op.Finished {
			doc, op = trace.Document{}, Operation{}
		}
	}
	var target Target
	resolved := false
	if doc.ID == "" {
		if err := callargs.Validate("lever call", in.Call); err != nil {
			return limitation(err.Error())
		}
		if callargs.SetsRunID(in.Call) {
			return limitation("lever call cannot set " + callargs.RunIDFlag + ": the service gives every run its own")
		}
		timeout := DefaultTimeout
		if in.Timeout != "" {
			timeout, err = time.ParseDuration(in.Timeout)
			if err != nil || timeout < MinTimeout || timeout > MaxTimeout {
				return limitation("verify timeout must be from 1m to 30m")
			}
		}
		if err := e.Repo.ActiveTurn(e.Scope); err != nil {
			return nil, err
		}
		if _, err := e.Repo.BeginTool(ctx, e.Scope, tool, raw, e.Now); err != nil {
			return nil, err
		}
		if e.Scope.Role == trace.Manager && in.Workstream == "" {
			members, err := e.Repo.WorkgroupMembers(own)
			if err != nil {
				return limitation(err.Error())
			}
			if len(members) > 1 {
				return limitation("a shared manager must qualify workstream for verify")
			}
		}
		var refusal string
		target, refusal, err = e.Resolve(ctx, in)
		if err != nil {
			return limitation(err.Error())
		}
		if refusal != "" {
			return limitation(refusal)
		}
		if target.Config == nil || !target.Config.Project.Verification.Enabled() {
			return limitation("the project has no verification setting")
		}
		resolved = true
		id := callID(hash, next)
		op = Operation{ID: id, Scope: e.Scope, Call: bytes.Clone(raw), Workstream: target.Stream, Unit: target.Unit, Commit: target.Commit, Args: in.Call, Timeout: timeout.String(), RunID: NewRunID()}
		doc = trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: id, Revision: 1, Project: e.Repo.Project(), Workstream: own, At: e.Now(), Actor: serviceActor, Cause: e.Scope.Turn}, Path: "tools/" + id + ".json"}
		if err := e.write(ctx, &doc, op); err != nil {
			return limitation(err.Error())
		}
	}
	memberRepo, stream, refusal, err := e.Repo.ManagerMember(own, op.Workstream)
	if err != nil {
		return limitation(err.Error())
	}
	if refusal != "" {
		return limitation(refusal)
	}
	if op.Outcome == nil {
		if kept, ok, err := Retained(memberRepo, stream, op.RunID); err != nil {
			return limitation(err.Error())
		} else if ok {
			op.Outcome = &kept
		} else {
			if !resolved {
				target, refusal, err = e.Resolve(ctx, in)
				if err != nil {
					return limitation(err.Error())
				}
				if refusal != "" {
					return limitation(refusal)
				}
			}
			outcome := e.execute(ctx, target, op)
			if ctx.Err() != nil {
				result, _ := json.Marshal(BuildResult(Operation{RunID: op.RunID, Workstream: op.Workstream, Commit: op.Commit, Outcome: &outcome}))
				return result, nil
			}
			op.Outcome = &outcome
		}
		doc.Revision++
		if err := e.write(ctx, &doc, op); err != nil {
			return limitation(err.Error())
		}
	}
	if _, err := Place(memberRepo, stream, op.RunID, e.View, true); err != nil {
		return limitation(err.Error())
	}
	output, err := json.Marshal(BuildResult(op))
	if err != nil {
		return nil, err
	}
	op.Finished = true
	doc.Revision++
	if err := e.write(ctx, &doc, op); err != nil {
		return limitation(err.Error())
	}
	if err := FinishTool(ctx, e.Repo, e.Scope, raw, output, e.Now); err != nil {
		return limitation(err.Error())
	}
	return output, nil
}

func callID(hash string, n int) string {
	if n <= 1 {
		return "verify-" + hash
	}
	return fmt.Sprintf("verify-%s-%d", hash, n)
}

func (e Env) write(ctx context.Context, doc *trace.Document, op Operation) error {
	return writeOperation(ctx, e.Repo, doc, op, e.Now)
}

func (e Env) execute(ctx context.Context, t Target, op Operation) Outcome {
	duration, err := time.ParseDuration(op.Timeout)
	if err != nil || duration <= 0 {
		return limited("verify timeout is invalid")
	}
	if e.Slots == nil {
		return limited("verify service is unavailable")
	}
	release, err := e.Slots(ctx, t.Repo.Project(), t.Stream)
	if err != nil {
		return limited("verify cancelled while waiting: " + err.Error())
	}
	defer release()
	return RunOperation(ctx, t, op)
}

func limited(message string) Outcome { return Outcome{Limitation: Diagnostic(message)} }

// RunOperation exports the commit of op, runs the lever once under op's run ID
// within op's timeout, and retains the report and the files it references. It
// holds no checks slot: the caller does. Every way it can fail is a limitation
// in the returned Outcome.
func RunOperation(ctx context.Context, t Target, op Operation) Outcome {
	return bounded(ctx, op, func(ctx context.Context, timedOut func(error) Outcome) Outcome {
		dir, err := os.MkdirTemp("", "osmia-verify-source-")
		if err != nil {
			return limited(err.Error())
		}
		defer os.RemoveAll(dir)
		if err := t.Export(ctx, op.Commit, dir, nil); err != nil {
			return timedOut(fmt.Errorf("verify source export: %w", err))
		}
		return runIn(ctx, timedOut, t, dir, op)
	})
}

// RunExported runs the lever once in dir, an export of op's commit the caller
// made, under op's run ID and timeout, and retains the report and the files it
// references. It holds no checks slot.
func RunExported(ctx context.Context, t Target, dir string, op Operation) Outcome {
	return bounded(ctx, op, func(ctx context.Context, timedOut func(error) Outcome) Outcome {
		return runIn(ctx, timedOut, t, dir, op)
	})
}

func bounded(ctx context.Context, op Operation, run func(context.Context, func(error) Outcome) Outcome) Outcome {
	duration, err := time.ParseDuration(op.Timeout)
	if err != nil || duration <= 0 {
		return limited("verify timeout is invalid")
	}
	outer := ctx
	ctx, cancel := context.WithTimeout(ctx, duration)
	defer cancel()
	return run(ctx, func(err error) Outcome {
		if outer.Err() == nil && ctx.Err() != nil {
			return limited("verify timed out after " + duration.String())
		}
		return limited(err.Error())
	})
}

func runIn(ctx context.Context, timedOut func(error) Outcome, t Target, dir string, op Operation) Outcome {
	run, err := Dagger(t.Config).RunLever(ctx, dir, checks.LeverCall{Base: t.Config.Project.Verification.Call, Args: op.Args, RunID: op.RunID})
	if err != nil {
		return timedOut(err)
	}
	defer run.Close()
	outcome, err := Retain(t.Repo, t.Stream, op.RunID, run, Outcome{Result: run.Result, Diagnostics: run.Diagnostics})
	if err != nil {
		return limited("the run's evidence could not be retained: " + err.Error())
	}
	return outcome
}

// Dagger returns the Dagger runner a lever run of cfg's project uses, with
// the cloud token registered for redaction.
func Dagger(cfg *config.Config) checks.DaggerChecks {
	resolved := cfg.Dagger.Resolve()
	token := os.Getenv(resolved.CloudComputeTokenEnv)
	redact.Register(token)
	selection := string(resolved.Mode)
	if resolved.Address != "" {
		selection = resolved.Address
	}
	return checks.DaggerChecks{Engine: selection, CloudToken: token}
}

// Operations returns the verify calls recorded in stream's trace, each at its
// latest revision, in the order they were first recorded.
func Operations(repo *trace.Repository, stream config.WorkstreamID) ([]Operation, map[string]trace.Document, error) {
	docs, err := trace.Read[trace.Document](repo, stream)
	if err != nil {
		return nil, nil, err
	}
	latest := map[string]trace.Document{}
	var order []string
	for _, d := range docs {
		if _, seen := latest[d.ID]; !seen {
			order = append(order, d.ID)
		}
		if d.Revision >= latest[d.ID].Revision {
			latest[d.ID] = d
		}
	}
	var ops []Operation
	for _, id := range order {
		if !strings.HasPrefix(id, "verify-") {
			continue
		}
		var op Operation
		if err := json.Unmarshal([]byte(latest[id].Content), &op); err != nil {
			return nil, nil, err
		}
		ops = append(ops, op)
	}
	return ops, latest, nil
}

// Restore puts back into view the retained files of the verify calls the
// holder of scope made, and lists their results in .osmia-verify/results.json.
// It restores a call of the same turn, and a call an earlier turn left
// unfinished, completing its record. It never runs a lever: a call whose run
// left no outcome is skipped.
func Restore(ctx context.Context, repo *trace.Repository, scope coreadapter.Scope, view isolation.View, now func() time.Time) error {
	own := config.WorkstreamID(scope.Workstream)
	ops, latest, err := Operations(repo, own)
	if err != nil {
		return err
	}
	var recovered []Result
	for _, op := range ops {
		if op.Scope.Role != scope.Role || op.Scope.Thread != scope.Thread || op.Scope.Unit != scope.Unit {
			continue
		}
		if op.Scope.Turn != scope.Turn && op.Finished {
			continue
		}
		member, stream, refusal, err := repo.ManagerMember(own, op.Workstream)
		if err != nil {
			return err
		}
		if refusal != "" {
			continue
		}
		doc := latest[op.ID]
		if op.Outcome == nil {
			kept, ok, err := Retained(member, stream, op.RunID)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			op.Outcome = &kept
			doc.Revision++
			if err := writeOperation(ctx, repo, &doc, op, now); err != nil {
				return err
			}
		}
		if _, err := Place(member, stream, op.RunID, view, true); err != nil {
			return err
		}
		result := BuildResult(op)
		if !op.Finished {
			output, _ := json.Marshal(result)
			op.Finished = true
			doc.Revision++
			if err := writeOperation(ctx, repo, &doc, op, now); err != nil {
				return err
			}
			if err := FinishTool(ctx, repo, op.Scope, op.Call, output, now); err != nil {
				return err
			}
		}
		recovered = append(recovered, result)
	}
	if len(recovered) == 0 {
		return nil
	}
	data, _ := json.Marshal(recovered)
	return view.Put(EvidenceDir+"/results.json", data)
}

func writeOperation(ctx context.Context, repo *trace.Repository, doc *trace.Document, op Operation, now func() time.Time) error {
	data, err := json.Marshal(op)
	if err != nil {
		return err
	}
	doc.At, doc.Content = now(), string(data)
	return repo.RecordDocuments(ctx, []trace.Document{*doc})
}

func latestDocuments(repo *trace.Repository, stream config.WorkstreamID) (map[string]trace.Document, error) {
	docs, err := trace.Read[trace.Document](repo, stream)
	latest := map[string]trace.Document{}
	for _, doc := range docs {
		if doc.Revision >= latest[doc.ID].Revision {
			latest[doc.ID] = doc
		}
	}
	return latest, err
}

func latestDocument(repo *trace.Repository, stream config.WorkstreamID, id string) (trace.Document, error) {
	docs, err := latestDocuments(repo, stream)
	return docs[id], err
}

// FinishTool completes the trace record of a verify call with its output. It
// does nothing when the record is already complete.
func FinishTool(ctx context.Context, repo *trace.Repository, scope coreadapter.Scope, raw, output json.RawMessage, now func() time.Time) error {
	doc, err := latestDocument(repo, config.WorkstreamID(scope.Workstream), trace.ToolCallID(scope, ToolName, raw))
	if err != nil {
		return err
	}
	if doc.Revision == 2 {
		return nil
	}
	if doc.ID == "" {
		return errors.New("verify has no started tool record")
	}
	var call trace.ToolCall
	if err := json.Unmarshal([]byte(doc.Content), &call); err != nil {
		return err
	}
	call.State, call.Failed = "completed", false
	call.Output = &trace.ToolPayload{Content: string(output), Bytes: len(output), SHA256: fmt.Sprintf("%x", sha256.Sum256(output))}
	data, _ := json.Marshal(call)
	doc.Revision, doc.At, doc.Content = 2, now(), string(data)
	return repo.RecordDocuments(ctx, []trace.Document{doc})
}

// Diagnostic bounds a message to preview's text limit and removes known
// secrets from it.
func Diagnostic(message string) string {
	message = redact.Secrets(message, redact.KnownSecrets()...)
	if len(message) > verification.MaxDiagnosticBytes {
		message = message[len(message)-verification.MaxDiagnosticBytes:]
		for len(message) > 0 && !utf8.RuneStart(message[0]) {
			message = message[1:]
		}
	}
	return message
}
