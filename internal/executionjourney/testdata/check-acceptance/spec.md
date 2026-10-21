# Acceptance never requires a full check run

## Background

The factory already runs the project's checks itself, deterministically: candidate checks after every unit report, and final checks before final review. Smart checks pick which checks a change needs, so a candidate usually runs only some of the project's checks. Spec criteria and unit acceptance sometimes still say that every `dagger check` must pass, or that a full check run is required. That wording repeats work the factory already does. It also conflicts with smart checks: a reviewer holding a partial selection can never show that "all checks pass", so the unit cannot be resolved.

The same conflict appears whenever a reviewer is asked to confirm that particular tests ran. The reviewer cannot control which tests smart checks select, and cannot run checks itself. Test passing is therefore the check gate's verdict, not something the reviewer proves.

## Intended behaviour

- When the architect drafts a spec and plan, drafts an amendment, or drafts a workgroup delta, its instructions say three things:
  - Acceptance states the observable behaviours that show the work holds. It may name tests a unit adds as the evidence for a behaviour.
  - Acceptance never requires the whole project check suite (for example "all `dagger checks` pass", "all checks pass" or "a full check run is green"), and does not need to list existing tests the work must keep passing. Regressions are what the factory's check gate catches.
  - Running checks is the factory's job, and the factory decides which checks to run. A passing service-owned check gate is the evidence that the tests the work needed ran and passed.
- The committee that debates plans gets the same rule. Acceptance that demands a full or all-checks run, or that asks a reviewer to show which tests ran, counts as an engineering defect that the plan should correct.
- Before a new draft, an amendment revision or a workgroup delta is accepted, the service checks it. Any spec criterion or unit acceptance item that obliges the work to make the whole check suite pass or run makes it invalid. The problem message names that criterion or unit item and says what to write instead (the observable behaviour, or the tests the unit adds). `draft_write` and `deliver_delta` return this problem as soon as the file is delivered, so the architect can fix it in the same turn.
- The guard targets obligations, not mentions. Text that describes, quotes, tests or forbids whole-suite wording (for example "acceptance never requires all checks to pass" or "the validator rejects acceptance saying \"all checks pass\"") is not an obligation and stays valid. So does a universal scoped to a named package or test set, such as "all internal/plan tests pass".
- Unit reviewers and the final reviewer treat test passing as decided by the service-owned check gate. When the gate passed for the candidate, every acceptance item or criterion that requires tests to pass is satisfied as far as passing goes: whole-suite wording in existing plans, scoped wording, and named tests alike. A reviewer never raises a finding because only a smart-check selection ran, because it cannot see that a named test was selected, or because it cannot run the tests itself. Reviewers still judge from the work itself whether tests the unit was asked to add exist and exercise the stated behaviour. This lets in-flight work that already has such wording finish.

## Must not

- Must not change how smart checks select checks, how candidate or final checks run, or what counts as a passing check gate.
- Must not make reviewers run checks, choose a check selection, or ask for evidence of which tests ran.
- Must not make specs, plans, amendments or workgroup slices that are already recorded invalid. Loading or migrating stored trace records must accept them as before.
- Must not reject an amendment or workgroup delta only because it carries over unchanged spec text or unchanged unit acceptance that already had such wording, whether in a slice the delta changes or one it leaves untouched. Only text that the revision adds or changes is checked.
- Must not reject acceptance that names specific tests, packages or behaviours, or that mentions, quotes or prohibits whole-suite wording without obliging a whole-suite run.
- Must not change the owner's charter or its parallelism rule (charter#4). That rule is about people running the full suite by hand, not about acceptance wording.

## Acceptance criteria

1. The architect's spec-and-plan drafting instructions, amendment-drafting instructions and workgroup delta instructions all say that acceptance states observable behaviours (optionally naming tests the unit adds), never a pass or run of the whole project check suite, need not list existing tests to keep passing, and that the factory runs and selects checks itself.
2. The architect and committee role policy says that acceptance which requires the whole check suite to pass or run, or which asks a reviewer to show which tests ran, is a defect to correct, because the service-owned check gate already runs checks with smart-check selection.
3. A new spec-and-plan draft is invalid if any spec criterion or unit acceptance item obliges the whole check suite to pass or run. The draft_write verdict lists a problem naming that criterion or unit item.
4. An amendment revision or workgroup delta is invalid if a spec criterion or unit acceptance item that it adds or changes obliges the whole check suite to pass or run. Carried-over unchanged text that already had such wording, in changed or untouched slices, does not make it invalid.
5. Spec criteria and unit acceptance still pass validation when they name specific tests, packages or behaviours (including scoped universals such as "all internal/plan tests pass"), or when they describe, quote, test or prohibit whole-suite wording without obliging a whole-suite run. This feature's own spec and plan pass the guard.
6. Specs, plans, amendments and workgroup slices that are already recorded still load and migrate without new validation errors, even when they contain wording that requires the whole check suite.
7. Unit reviewer and final reviewer instructions say that test passing is decided by the service-owned check gate: a passing gate is the evidence that the tests an acceptance item or criterion requires (whole-suite, scoped or named) ran and passed. A reviewer does not raise a finding because only a smart-check selection ran, because it cannot see that a named test was selected, or because it cannot run tests itself.
