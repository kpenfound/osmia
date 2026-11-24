package delivery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/followup"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/jev"
	"github.com/kpenfound/osmia/internal/trace"
)

// TriageToolName is the chief-only service tool that records the triage of a
// blocking finding of a final review.
const TriageToolName = "triage_finding"

// FindingTriageKind is the decide kind of a blocking finding's triage.
const FindingTriageKind = string(jev.KindTriage)

// TriageSubject is a blocking finding of the workstream's current reviewed
// final review, prepared for its triage.
type TriageSubject struct {
	Report   FinalReport
	Revision int
	Finding  BlockingFinding
	// Key is the finding's occurrence key.
	Key string
	// Stands says the finding is blocking in the current review and not yet
	// triaged. A finding that stands but is not the chief of staff's to
	// triage has a reason, and is left to the owner.
	Stands bool
}

// Label names the finding for the chief of staff.
func (s TriageSubject) Label() string {
	return fmt.Sprintf("finding %s of final review %d", s.Finding.ID, s.Report.Review)
}

// TriageSubject resolves the finding of the final review the chief of staff
// names, or says why the chief of staff may not triage it with Jev today: the
// review is not the workstream's current reviewed one, awaiting the owner's
// delivery approval, the finding is not blocking in it, it is already triaged,
// or final reviews found blocking problems often enough that further
// revisions are the owner's decision, or a follow-up unit repairs the finding.
func (s *Controller) TriageSubject(ctx context.Context, repository *trace.Repository, stream config.WorkstreamID, review int, finding string) (TriageSubject, string, error) {
	target, reason, err := s.briefTarget(ctx, repository, stream, nil)
	if err != nil || reason != "" {
		return TriageSubject{}, strings.Replace(reason, "no reviewed final review awaits delivery approval", "no final review awaits triage", 1), err
	}
	sub := TriageSubject{Report: target.report, Revision: target.revision}
	if review != target.report.Review {
		return TriageSubject{}, fmt.Sprintf("final review %d is not the current one: the current one is review %d; only its findings are triaged", review, target.report.Review), nil
	}
	blocking := BlockingFindings(target.report)
	finding = strings.TrimSpace(finding)
	found := false
	for _, b := range blocking {
		if b.ID == finding {
			sub.Finding, found = b, true
		}
	}
	if !found {
		ids := make([]string, len(blocking))
		for i, b := range blocking {
			ids[i] = b.ID
		}
		return TriageSubject{}, fmt.Sprintf("final review %d has no blocking finding %q; its blocking findings are: %s", review, finding, strings.Join(ids, ", ")), nil
	}
	sub.Key = jev.FindingKey(review, finding)
	if t, done, err := followup.TriageOf(repository, stream, review, finding); err != nil {
		return TriageSubject{}, "", err
	} else if done {
		return TriageSubject{}, fmt.Sprintf("%s is already triaged: %s", sub.Label(), t.Outcome), nil
	}
	sub.Stands = true
	if !target.report.Maintenance {
		rounds, err := FinalRoundsWithBlocking(repository, stream)
		if err != nil {
			return TriageSubject{}, "", err
		}
		if rounds >= maxFinalRounds {
			return sub, fmt.Sprintf("final reviews have found blocking problems %d times; further revisions of the assembled plan are the owner's decision, who can accept the findings at delivery or approve the work that resolves them", rounds), nil
		}
	} else if s.WorkgroupEdgePending != nil {
		pending, err := s.WorkgroupEdgePending(repository, stream)
		if err != nil {
			return TriageSubject{}, "", err
		}
		if pending {
			return sub, "the post-merge dependency update is repaired by a follow-up unit the service adds; there is nothing to triage", nil
		}
	}
	return sub, "", nil
}

// TriageToolOf returns the triage_finding tool of one claimed chief-of-staff turn,
// over the controller load returns when the tool is called. It
// records the chief of staff's triage of one blocking finding of the current
// final review: the finding needs code, so the plan revision the chief of staff
// files takes it, or it is left to the owner, who sees it at delivery approval.
// The decision is recorded only after decide was called for the same finding,
// and a confident Jev pick allows only the picked outcome. A call the service
// refuses is an ordinary result, {"recorded":false,"reason":...}, that records
// nothing.
func TriageToolOf(load func() *Controller, repository *trace.Repository, scope coreadapter.Scope) coreadapter.Tool {
	tool := coreadapter.Tool{Name: TriageToolName, Effect: coreadapter.ToolMemory,
		Description: "Record how a blocking finding of the workstream's current final review is triaged, after you called decide for it. review: the final review's number; finding: the finding's ID as the final review notice gives it; decision: code (it needs code: the one plan revision you file with route_amendment takes it) or owner (it is mistaken or not worth building, so the owner reads it at delivery and can accept it); " +
			"note: for code, what the repair is; for owner, why the finding is mistaken or not worth building, which the owner reads. When Jev's decision on the finding is binding, only the outcome it picked is recorded, with your note. workstream: a member workstream ID of your workgroup to act on; omit for this conversation's own workstream.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"review":{"type":"integer","minimum":1},"finding":{"type":"string"},"decision":{"type":"string","enum":["code","owner"]},"note":{"type":"string"},"workstream":{"type":"string"}},"required":["review","finding","decision","note"],"additionalProperties":false}`)}
	tool.Handle = func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
		var input struct {
			Review     int                 `json:"review"`
			Finding    string              `json:"finding"`
			Decision   string              `json:"decision"`
			Note       string              `json:"note"`
			Workstream config.WorkstreamID `json:"workstream"`
		}
		d := json.NewDecoder(bytes.NewReader(raw))
		d.DisallowUnknownFields()
		if err := d.Decode(&input); err != nil {
			return nil, fmt.Errorf("tool input: %w", err)
		}
		if err := d.Decode(new(any)); err != io.EOF {
			return nil, errors.New("tool input must be one object")
		}
		s := load()
		if s == nil {
			return nil, errors.New("finding triage is unavailable")
		}
		if err := repository.ActiveTurn(scope); err != nil {
			return nil, err
		}
		if scope.Role != trace.ChiefOfStaff {
			return intake.PriorityRefusal(TriageToolName + " is only available to the chief of staff")
		}
		target, stream, refusal, err := repository.ChiefMember(config.WorkstreamID(scope.Workstream), input.Workstream)
		if err != nil {
			return nil, err
		}
		if refusal != "" {
			return intake.PriorityRefusal(refusal)
		}
		if input.Decision != followup.TriageCode && input.Decision != followup.TriageOwner {
			return intake.PriorityRefusal("a decision is code or owner")
		}
		note := strings.TrimSpace(input.Note)
		if note == "" {
			return intake.PriorityRefusal("a note is required")
		}
		sub, why, err := s.TriageSubject(ctx, target, stream, input.Review, input.Finding)
		if err != nil {
			return nil, err
		}
		if !sub.Stands {
			return intake.PriorityRefusal(why)
		}
		var bound *jev.Result
		if res, err := jev.Recorded(target, stream, jev.KindTriage, sub.Key); err != nil {
			return nil, err
		} else if why == "" || res.Status == jev.Binding {
			var requireWhy string
			if bound, requireWhy, err = jev.Require(target, stream, jev.KindTriage, sub.Key, input.Decision, sub.Label()); err != nil {
				return nil, err
			} else if requireWhy != "" {
				return intake.PriorityRefusal(requireWhy)
			}
		}
		t := followup.Triage{Review: input.Review, Finding: sub.Finding.ID, Text: sub.Finding.Text, Outcome: input.Decision, Note: note, Turn: scope.Turn}
		if bound != nil {
			t.DecidedBy, t.Judgment = trace.DecidedByJev, bound.Judgment
		}
		detail := map[string]string{
			followup.TriageCode:  "the finding is taken by the plan revision you file next with route_amendment",
			followup.TriageOwner: "the finding is left to the owner, who reads it with your note at delivery approval",
		}[input.Decision]
		if why != "" && input.Decision == followup.TriageCode {
			if bound == nil {
				return intake.PriorityRefusal(why)
			}
			t.Outcome, t.Refused = followup.TriageOwner, why
			t.Note = fmt.Sprintf("Jev decided code with confidence %.2f (judgment %s), but the service refused it: %s. The decision is yours. The chief of staff's note: %s", bound.Confidence, bound.Judgment, why, note)
			detail = "the service could not carry out Jev's decision (" + why + "); the finding is left to the owner with Jev's judgment"
		}
		if err := followup.RecordTriage(ctx, target, stream, t, s.Now()); err != nil {
			if errors.Is(err, trace.ErrConflict) || strings.Contains(err.Error(), "already triaged") {
				return intake.PriorityRefusal(err.Error())
			}
			return nil, err
		}
		return json.Marshal(struct {
			Recorded bool   `json:"recorded"`
			Review   int    `json:"review"`
			Finding  string `json:"finding"`
			Outcome  string `json:"outcome"`
			Detail   string `json:"detail"`
		}{true, t.Review, t.Finding, t.Outcome, detail})
	}
	return tool
}

// TriageNotes states what the chief of staff decided of the final review's
// blocking findings, for the owner's delivery approval: each finding left to
// the owner with the reason, and each finding a plan revision takes. A finding
// Jev decided says so.
func TriageNotes(repository *trace.Repository, stream config.WorkstreamID, review int) (string, error) {
	all, _, err := followup.ReadTriage(repository, stream)
	if err != nil {
		return "", err
	}
	var owner, code []string
	for _, t := range all {
		if t.Review != review {
			continue
		}
		by := ""
		if t.ByJev() {
			by = fmt.Sprintf(" (decided by Jev, judgment %s)", t.Judgment)
		}
		switch t.Outcome {
		case followup.TriageOwner:
			owner = append(owner, fmt.Sprintf("%s%s: %s", t.Text, by, t.Note))
		case followup.TriageCode:
			code = append(code, fmt.Sprintf("%s%s", t.Text, by))
		}
	}
	var b strings.Builder
	if len(owner) > 0 {
		b.WriteString(" Left to you: " + strings.Join(owner, " | ") + ".")
	}
	if len(code) > 0 {
		b.WriteString(" Triaged as needing code in a plan revision: " + strings.Join(code, " | ") + ".")
	}
	return b.String(), nil
}
