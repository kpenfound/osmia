package cli

import (
	"fmt"
	"io"

	"github.com/kpenfound/osmia/internal/service"
)

func traceRef(w io.Writer, label string, ref *service.TraceRef) {
	if ref == nil {
		return
	}
	fmt.Fprintf(w, "%s: %s %s revision %d path=%s at=%s actor=%s/%s cause=%s\n", label, ref.Kind, ref.ID, ref.Revision, ref.Path, ref.At.Format("2006-01-02T15:04:05Z07:00"), ref.Actor.Kind, ref.Actor.ID, ref.Cause)
}

func traceGaps(w io.Writer, gaps []service.TraceGap) {
	for _, gap := range gaps {
		fmt.Fprintf(w, "  Gap: %s [%s] %s\n", gap.Link, gap.State, gap.Reason)
	}
}

func traceUnit(w io.Writer, u service.UnitTrace) {
	fmt.Fprintf(w, "Unit %s: %s state=%s source=%s complete=%t\n", u.Unit, u.Title, u.State, u.Source, u.Complete)
	traceRef(w, "  Definition", u.Definition)
	for _, a := range u.Addresses {
		fmt.Fprintf(w, "  Criterion %s: %s; proof=%s %s\n", a.Criterion, a.Text, a.Proof.Kind, a.Proof.Name)
	}
	for _, r := range u.Reports {
		traceRef(w, "  Report", &r.Ref)
		fmt.Fprintf(w, "    turn=%s outcome=%s base=%s candidate=%s\n", r.Turn, r.Outcome, r.Base, r.Candidate)
	}
	for _, r := range u.Reviews {
		traceRef(w, "  Review", &r.Ref)
		fmt.Fprintf(w, "    turn=%s decision=%s report=%s candidate=%s\n", r.Turn, r.Decision, r.Report, r.Candidate.Revision)
	}
	for _, r := range u.Rulings {
		traceRef(w, "  Ruling", &r.Ref)
		fmt.Fprintf(w, "    %s: %s\n", r.Kind, r.Decision)
	}
	for _, r := range u.Rebases {
		traceRef(w, "  Rebase", &r.Ref)
	}
	for _, r := range u.Landings {
		traceRef(w, "  Landing", &r.Ref)
		fmt.Fprintf(w, "    commit=%s candidate=%s approval=%s\n", r.Commit, r.Candidate, r.Approval)
	}
	for _, turn := range u.Turns {
		fmt.Fprintf(w, "  Turn %s: %s status=%s cost=$%.4f\n", turn.Turn, turn.Role, turn.Status, turn.CostUSD)
		traceRef(w, "    Request", &turn.Request)
		traceRef(w, "    Response", turn.Response)
	}
	fmt.Fprintf(w, "  Cost: $%.4f, %d unknown\n", u.CostUSD, u.UnknownCosts)
	for _, e := range u.History {
		traceRef(w, "  Event", &e.Ref)
		fmt.Fprintf(w, "    %s\n", e.Summary)
	}
	traceGaps(w, u.Gaps)
}

func traceDelivery(w io.Writer, d *service.TraceDelivery) {
	if d == nil {
		return
	}
	traceRef(w, "Final report", d.Report)
	traceRef(w, "Delivery approval", d.Approval)
	traceRef(w, "Publication", d.Publication)
	fmt.Fprintf(w, "Delivery: %s reviewed=%s published=%s pull_request=%d %s\n", d.Status, d.Reviewed, d.Published, d.PullRequest, d.URL)
}

func showTrace(w io.Writer, result any) {
	switch v := result.(type) {
	case service.TraceSummary:
		fmt.Fprintf(w, "Workstream %s: state=%s complete=%t\n", v.Workstream, v.Feature, v.Complete)
		if v.Seal != nil {
			traceRef(w, "Seal", &v.Seal.Ref)
		}
		traceRef(w, "Spec", v.Spec)
		traceRef(w, "Plan", v.Plan)
		for _, c := range v.Criteria {
			fmt.Fprintf(w, "Criterion %s: %s\n", c.Criterion, c.Text)
		}
		for _, u := range v.Units {
			traceUnit(w, u)
		}
		traceDelivery(w, v.Delivery)
		traceGaps(w, v.Gaps)
	case service.UnitTrace:
		fmt.Fprintf(w, "Workstream %s: state=%s\n", v.Workstream, v.Feature)
		traceUnit(w, v)
	case service.CriterionTrace:
		fmt.Fprintf(w, "Criterion %s of %s: %s state=%s complete=%t\n", v.Criterion, v.Workstream, v.Text, v.Feature, v.Complete)
		if v.Seal != nil {
			traceRef(w, "Seal", &v.Seal.Ref)
		}
		traceRef(w, "Spec", v.Spec)
		traceRef(w, "Plan", v.Plan)
		for _, r := range v.Rulings {
			traceRef(w, "Ruling", &r.Ref)
			fmt.Fprintf(w, "  %s: %s\n", r.Kind, r.Decision)
		}
		for _, u := range v.Units {
			traceUnit(w, u)
		}
		if v.Final != nil {
			fmt.Fprintf(w, "Final account: %s evidence=%s gap=%s\n", v.Final.Criterion, v.Final.Evidence, v.Final.Gap)
		}
		traceDelivery(w, v.Delivery)
		traceGaps(w, v.Gaps)
	case service.CommitTrace:
		fmt.Fprintf(w, "Commit %s of %s: state=%s operation=%s complete=%t\n", v.Commit, v.Workstream, v.Feature, v.Operation, v.Complete)
		for _, r := range v.Records {
			traceRef(w, "Record "+r.Role+" unit="+r.Unit, &r.Ref)
		}
		for _, l := range v.Landings {
			fmt.Fprintf(w, "  Unit %s landing=%s candidate=%s\n", l.Unit, l.Landing.Commit, l.Landing.Candidate)
			traceRef(w, "    Landing", &l.Landing.Ref)
			if l.Review != nil {
				traceRef(w, "    Review", &l.Review.Ref)
			}
			if l.Report != nil {
				traceRef(w, "    Report", &l.Report.Ref)
			}
			traceRef(w, "    Spec", l.Spec)
			traceRef(w, "    Plan", l.Plan)
			for _, c := range l.Criteria {
				fmt.Fprintf(w, "    Criterion %s: %s\n", c.Criterion, c.Text)
			}
			for _, r := range l.Rulings {
				traceRef(w, "    Ruling", &r.Ref)
			}
		}
		traceDelivery(w, v.Delivery)
		traceGaps(w, v.Gaps)
	}
}
