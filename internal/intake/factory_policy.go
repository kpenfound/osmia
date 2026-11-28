package intake

import (
	"context"
	"fmt"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

const FactoryPolicySubject = "factory-policy"

const FactoryPolicyVersion = "adaptive"

// AcceptanceDraftInstructions applies to every architect submission path that
// drafts acceptance.
const AcceptanceDraftInstructions = "\n\n## Acceptance and the check gate\nAcceptance states observable behaviours and may name tests the unit adds as evidence. Do not require a whole-suite check pass or run in an acceptance item: the factory runs and selects checks itself, using smart-check selection, so no unit's own work can produce that evidence and the item could not be met. Do not list existing tests to keep passing either: the factory's service-owned check gate catches regressions. A passing service-owned check gate is the evidence that required tests ran and passed.\n"

// RecordFactoryPolicy commits adoption and its recovery notice together. Repeating
// reconciliation never loses the notice or creates another one after acknowledgment.
func RecordFactoryPolicy(ctx context.Context, r *trace.Repository, stream config.WorkstreamID, at time.Time, revisit bool) error {
	st, err := r.Workflow(stream, FactoryPolicySubject)
	if err != nil || st.Value == FactoryPolicyVersion {
		return err
	}
	if st.Value != "" {
		return fmt.Errorf("unsupported factory policy %q", st.Value)
	}
	id := FactoryPolicySubject + "-" + FactoryPolicyVersion
	tx := trace.Transaction{ExpectedVersion: st.Version, Transition: trace.Transition{
		Header:  trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: r.Project(), Workstream: stream, At: at, Actor: ServiceActor, Cause: FactoryPolicySubject},
		Subject: FactoryPolicySubject, To: FactoryPolicyVersion, Reason: "Current factory authority applies to subsequent turns; sealed intent, assignments and owner decisions are preserved.",
	}}
	if revisit {
		tx.Events = []trace.Event{trace.Notice(id, trace.ManagerKey, "Reassess this in-flight workstream under current factory authority. Read factory_context and current activity for unresolved questions, contested units and pending amendments, including blockers whose earlier notices were already acknowledged. Investigate and resolve engineering decisions within approved intent; commission a plan revision where assignments or acceptance methods need adaptation. Keep the work moving: move stuck units and workstreams, reopen work and commission revisions where needed. Do not clear explicit owner holds or substitute for an owner decision. Existing escalations remain recorded. Ratification and publication still require the owner.")}
	}
	_, err = r.Transact(ctx, tx)
	return err
}
