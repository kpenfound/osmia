package intake

import (
	"context"
	"fmt"
	"strings"

	"github.com/kpenfound/osmia/internal/climb"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/kinds"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
)

// StreamKind returns the registered kind of a workstream. A workstream whose
// stored kind this build does not register is an error: it is never given
// another kind's documents or loop.
func StreamKind(repository *trace.Repository, stream config.WorkstreamID) (kinds.Kind, error) {
	kind, stored, ok, err := WorkstreamKind(repository, stream)
	if err != nil {
		return kinds.Kind{}, err
	}
	if !ok {
		return kinds.Kind{}, fmt.Errorf("workstream %s has kind %q, which this build does not register", stream, stored)
	}
	return kind, nil
}

// DraftFile is one file the architect delivers: the trace document ID it is
// recorded under and its path.
type DraftFile struct {
	ID   string
	Path string
}

// DraftFiles lists the files the kind's architect delivers, the spec slot
// first. A kind with a plan delivers spec.md and plan.json; a kind without
// one delivers its single document.
func DraftFiles(k kinds.Kind) []DraftFile {
	files := []DraftFile{{ID: k.Documents.Drafted[0], Path: k.Documents.SpecPath}}
	if k.Documents.Planned() {
		files = append(files, DraftFile{ID: plan.PlanDocument, Path: plan.PlanPath})
	}
	return files
}

// DraftPaths lists the paths of DraftFiles.
func DraftPaths(k kinds.Kind) []string {
	files := DraftFiles(k)
	paths := make([]string, len(files))
	for i, f := range files {
		paths[i] = f.Path
	}
	return paths
}

// DraftNoun names what the kind's architect delivers, for messages.
func DraftNoun(k kinds.Kind) string {
	if k.Documents.Planned() {
		return "spec and plan"
	}
	return "climb spec"
}

// ClimbDefaults returns the defaults a climb spec takes from the
// `[kinds.hill-climb]` settings of cfg.
func ClimbDefaults(cfg *config.Config) climb.Defaults { return climb.DefaultsOf(cfg.HillClimb()) }

// ClimbProblems returns every problem with a drafted climb spec. A problem
// the architect can correct is listed; nothing is read from the trace.
func ClimbProblems(content string, cfg *config.Config) []string {
	_, problems := climb.Check([]byte(content), ClimbDefaults(cfg))
	return problems
}

// ClimbSystemPrompt is the architect's standing instruction on a Hill Climb
// workstream.
func ClimbSystemPrompt(p config.Project) string {
	return fmt.Sprintf("You are the architect of the %s project (%s). You draft this workstream's climb spec from what the owner handed in, the project's charter and its entity map. A climb spec replaces the spec and the plan: the workstream improves one measured number, the service measures it with an evaluator function the owner approves, and Go alone decides whether an attempt improved it. You never judge a score and you never choose the workstream's kind. Use inspect_code to read the repository, read_remote_file to retrieve referenced GitHub files, and factory_context to retrieve workstream records. You hold no version control tool: you deliver climb-spec.json with %s, and the service records and validates it. When your view does not settle something you must know, call %s: your work waits for the answer, which arrives as your next turn, and what you delivered so far is kept.", p.Name, p.Upstream, DraftTool, questions.AskTool)
}

// ClimbSpecShape is the climb spec's schema as the architect's prompt gives it.
const ClimbSpecShape = `{
  "version": 1,
  "goal": "what the climb improves and why, in prose",
  "evaluator": "name-of-the-function-in-the-dev-module",
  "metric": {"name": "wall_ms", "direction": "lower", "min_improvement": 5},
  "repeats": 1,
  "guardrails": {
    "checks": ["names of the project's checks that must still pass"],
    "limits": [{"metric": "peak_rss_mb", "direction": "lower", "max": 200}]
  },
  "protected": {"fixtures": ["paths of the files the evaluator reads"], "add": ["any further path to freeze"]},
  "stop": {"max_attempts": 20, "max_cost_usd": 40, "max_wall_clock": "6h", "plateau": 5, "target": 500}
}`

// ClimbPrompt is the architect's task for one draft of a climb spec.
func ClimbPrompt(handedPath string, n int, previous string, drafted bool, note string, answers []string) string {
	var b strings.Builder
	draft := ""
	if note != "" {
		fmt.Fprintf(&b, "Your earlier drafts were refused and drafting stopped; you were asked for another run of drafts with this note:\n%s\n\n", note)
	}
	switch {
	case n > 1 && drafted:
		fmt.Fprintf(&b, "Draft %d was not accepted:\n%s\n\nYour previous %s is in draft/. Deliver a corrected file.\n\n", n-1, previous, kinds.ClimbSpecPath)
		draft = "- draft/" + kinds.ClimbSpecPath + ": your previous draft.\n"
	case n > 1:
		fmt.Fprintf(&b, "Draft %d was not accepted:\n%s\n\nNo file of that draft was recorded. Deliver the file.\n\n", n-1, previous)
	}
	fmt.Fprintf(&b, `Draft the climb spec for this workstream.

Your view holds:
- %s: what the owner handed in, unchanged. It says what to improve.
- charter.md: the owner's optional guidance for contributing to this project; it may be empty. Cite a numbered rule in it as charter#<n>.
- context.md: the project context bundle: the charter's rules, the entity map and the recorded decisions.
%s
Deliver one file with %s: %s, a JSON object of this shape:

%s

- goal: required prose.
- evaluator: required. The name of the function, in the project's dev module (the module dagger.toml marks main = true), that measures one run. Each climb may create its own; name the function the climb needs. It is not a check.
- metric: required. name is the primary metric as the evaluator reports it, direction is "higher" or "lower" for which way is better, and min_improvement is the absolute improvement, in the metric's own units and above zero, that an attempt must reach over the best to be kept. Choose it above the run-to-run noise: a climb whose noise reaches it is held as too noisy to climb.
- repeats: optional, default 1. Each attempt is measured this many times and Go compares the median; every run must succeed and be valid.
- guardrails: checks the project already has that must still pass, and limits on secondary metrics of the evaluator's output. A limit names the metric, the direction the evaluator reports it with, and a min and/or a max. Never limit the primary metric.
- protected: the files the climb freezes once the owner approves the evaluator. The dev module, dagger.toml and dagger.lock are always frozen. fixtures lists the files and directories the evaluator reads; add lists any further path. The measurement machinery belongs here: the evaluator, its scoring and timing helpers, the correctness oracle, the fixtures and the workload. The code being improved does not.
- stop: max_attempts is required and a hard limit. max_cost_usd and max_wall_clock are optional hard limits; plateau is the number of attempts without improvement after which the owner is asked whether to go on; target is a score at which the climb is done.

Think about the metric before you write it: how it could be gamed by an attempt that is wrong but scores well, how noisy it is, and which paths the evaluator depends on. The committee reads only that, in one round, before the owner ratifies.

The draft is accepted only when every required field is present and every value is valid.

Call %s when something you must know to draft is not in your view: the draft waits for the answer, which arrives as your next turn.
`, handedPath, draft, DraftTool, kinds.ClimbSpecPath, ClimbSpecShape, questions.AskTool)
	b.WriteString(Answered("draft", answers))
	return b.String()
}

// ClimbSketchReason says what a valid climb spec draft holds.
func ClimbSketchReason(n int, doc trace.Document, spec climb.Spec) string {
	return fmt.Sprintf("the architect's draft %d passed validation: %s revision %d with evaluator %s, primary metric %s (%s is better) and a hard limit of %d attempts", n, doc.Path, doc.Revision, spec.Evaluator, spec.Metric.Name, spec.Metric.Direction, spec.Stop.MaxAttempts)
}

func (d *Drafter) climbProblems(_ context.Context, content string) []string {
	return ClimbProblems(content, d.S.About(d.Repository))
}

// FixedRounds returns the number of committee rounds the workstream's kind
// fixes, or zero when its rounds follow shed.max_rounds and the owner's
// requests. A kind with fixed rounds gets exactly that many: the owner cannot
// ask for more or for a redraft that is debated.
func (d *Debate) FixedRounds(stream config.WorkstreamID) (int, error) {
	kind, err := StreamKind(d.Repository, stream)
	return kind.Rounds, err
}

// ClimbCommitteeSystemPrompt is a committee member's standing instruction in
// the one round of a climb spec.
func ClimbCommitteeSystemPrompt(p config.Project) string {
	return fmt.Sprintf("You are a member of the committee that debates one workstream's climb spec for the %s project (%s) before the owner ratifies it. You look only at the metric: whether it can be gamed, whether it is too noisy to climb on, and whether a path the measurement depends on is missing from the protected paths. You read; you hold no tool that writes or runs. You contribute only through %s and %s, and every objection cites what it rests on. You never judge a score: Go decides every attempt by arithmetic. When your view does not settle something you must know to judge the revision, call %s: the round waits for the answer, which arrives as your next turn, and your contributions so far are kept.", p.Name, p.Upstream, shed.ObjectTool, shed.ConcedeTool, questions.AskTool)
}

// ClimbCommitteePrompt is a member's task in the round of a climb spec.
func ClimbCommitteePrompt(in RoundInput, standing []shed.Dissent, answers []string) string {
	var b strings.Builder
	round := fmt.Sprintf("Round %d of the shed, the only round the climb spec gets.", in.Round)
	if in.Amendment != "" {
		round = fmt.Sprintf("Round %d of the shed for amendment %s to the ratified climb spec.", in.Round, in.Amendment)
	}
	fmt.Fprintf(&b, `%s Every member reads the same revision: %s.

Your view holds:
- %s: the climb spec under debate. Cite one of its parts as climb#goal, climb#evaluator, climb#metric, climb#guardrails, climb#protected or climb#stop.
- handed/: what the owner handed in, unchanged. It says what to improve.
- charter.md: the owner's optional guidance for contributing to this project; it may be empty. Cite a numbered rule in it as charter#<n>.
- context.md: the project context bundle: the charter's rules, the entity map and the recorded decisions. Cite an entity as %s<entity>.
- repo/: the tracked files of the owner's clone, read-only.

Look at the metric and nothing else, and call %s once for each thing you find:
- metric, gaming: an attempt could raise the score without improving what the goal names, by caching, special-casing the workload, weakening the correctness oracle, or reading what the evaluator reads. Say how.
- metric, noise: the metric varies run to run by about as much as the minimum improvement, so the climb could not tell a gain from luck, or repeats is too low for it.
- metric, protected paths: something the evaluator, its scoring and timing helpers, its oracle, its fixtures or its workload depend on is not in the protected paths, so an attempt could change what is measured. The dev module, dagger.toml and dagger.lock are always protected.
- charter: a part that violates a charter rule. This is a veto on that part; cite the rule.

Metric objections are advice to the owner, who ratifies after this round. An objection that is refused comes back with the reason; correct it and call again. Ending your turn without objecting or conceding says you have no new dissent on this revision. Call %s when something you must know to judge the revision is not in your view: the round waits for the answer, which arrives as your next turn.
`, round, in.Pin(), kinds.ClimbSpecPath, shed.EntityCitation, shed.ObjectTool, questions.AskTool)
	if len(standing) > 0 {
		fmt.Fprintf(&b, "\nYour objections that still stand. Call %s for each one that the revision or the debate has settled:\n", shed.ConcedeTool)
		for _, s := range standing {
			fmt.Fprintf(&b, "- %s (%s, %s, made against %s): %s\n", s.ID, s.Kind, s.Part, s.Revision, s.Argument)
		}
	}
	b.WriteString(Answered("round", answers))
	return b.String()
}

// ClimbReplyPrompt is the architect's reply to the one round of a climb spec.
func ClimbReplyPrompt(in RoundInput, latest shed.Pin, open []shed.Entry, problems []string) string {
	var b strings.Builder
	returned, redraft := InvalidRedraft(problems)
	b.WriteString(returned)
	fmt.Fprintf(&b, "The one round of the shed is heard. The committee debated %s. %d objections stand:\n", in.Pin(), len(open))
	b.WriteString(DissentList(open))
	fmt.Fprintf(&b, `
What each kind asks of you:
- metric: advice to the owner on gaming, noise or a missing protected path. It never blocks; answer it, and change the climb spec when it is right.
- charter: a veto on the part it names. It blocks until its member concedes it or the owner disposes of it.
- owner: the owner's own objection. It blocks until the owner disposes of it; answer it as you would a member's.

Your view holds:
- %s: the latest recorded revision, %s.
- handed/, charter.md and context.md: what you drafted from.
- shed/round-<n>/: what every member contributed to the round, and the owner's own files of it.
%s
This is your one reply to this round, and the committee does not debate again. Answer each objection with %s, giving its ID and your answer. When the objections call for a change, also deliver the changed %s with %s: it is validated by the rules your draft was, and the owner ratifies the revision the packet names. An invalid one comes back to you. Deliver nothing to leave the revision as it is.`, kinds.ClimbSpecPath, latest, redraft, shed.ReplyTool, kinds.ClimbSpecPath, DraftTool)
	return b.String()
}

// SealedClimbSpec returns the climb spec of the workstream's latest seal, as
// the building loop reads it, with the seal it is sealed by. A workstream of a
// kind that has no climb spec, and one that is not sealed, are errors.
func SealedClimbSpec(repository *trace.Repository, cfg *config.Config, stream config.WorkstreamID) (climb.Spec, seal.Seal, error) {
	kind, err := StreamKind(repository, stream)
	if err != nil {
		return climb.Spec{}, seal.Seal{}, err
	}
	if kind.Name != kinds.HillClimb {
		return climb.Spec{}, seal.Seal{}, fmt.Errorf("a %s workstream has no climb spec", kind.Name)
	}
	sealed, _, found, err := seal.Latest(repository, stream)
	if err != nil {
		return climb.Spec{}, seal.Seal{}, err
	}
	if !found {
		return climb.Spec{}, seal.Seal{}, fmt.Errorf("workstream %s is not sealed", stream)
	}
	spec, _, err := climb.SealedSpec(repository, stream, sealed.Revision.Spec, ClimbDefaults(cfg))
	return spec, sealed, err
}
