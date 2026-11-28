package followup

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

// TriageDocumentID and TriagePath name the document that records the manager of
// staff's triage of a final review's blocking findings. Each revision holds
// every decision recorded so far.
const (
	TriageDocumentID = "final-triage"
	TriagePath       = "final/triage.json"
)

// The outcomes of triaging a blocking finding: it needs code in a plan
// revision, or it is left to the owner, who can accept it at delivery.
const (
	TriageCode  = "code"
	TriageOwner = "owner"
)

// Triage is the decision on one blocking finding of one final review.
type Triage struct {
	Review  int    `json:"review"`
	Finding string `json:"finding"`
	// Text is the finding as the final review reported it.
	Text    string `json:"text"`
	Outcome string `json:"outcome"`
	// Note is the reasoning: what the repair is for a finding that needs code,
	// and why it is mistaken or not worth building for one left to the owner.
	Note string `json:"note"`
	Turn string `json:"turn,omitempty"`
	// DecidedBy is empty for the manager's own decision, or
	// trace.DecidedByJev with the Jev judgment record it comes from.
	DecidedBy string `json:"decided_by,omitempty"`
	Judgment  string `json:"judgment,omitempty"`
	// Refused is why the service could not carry out Jev's pick, when it
	// could not: the finding is then left to the owner with Jev's judgment.
	Refused string `json:"refused,omitempty"`
	// Amendment is the plan revision that took a finding needing code.
	Amendment string `json:"amendment,omitempty"`
}

// ByJev reports whether Jev decided the finding.
func (t Triage) ByJev() bool { return t.DecidedBy == trace.DecidedByJev }

// ReadTriage returns every recorded triage decision in the order recorded, and
// the revision of the document that holds them.
func ReadTriage(repo *trace.Repository, stream config.WorkstreamID) ([]Triage, int, error) {
	docs, err := trace.Read[trace.Document](repo, stream)
	if err != nil {
		return nil, 0, err
	}
	var latest trace.Document
	for _, d := range docs {
		if d.ID == TriageDocumentID && d.Revision > latest.Revision {
			latest = d
		}
	}
	if latest.Revision == 0 {
		return nil, 0, nil
	}
	var out []Triage
	if err := json.Unmarshal([]byte(latest.Content), &out); err != nil {
		return nil, 0, fmt.Errorf("%s revision %d: %w", TriagePath, latest.Revision, err)
	}
	return out, latest.Revision, nil
}

// TriageOf returns the decision on a review's finding, if one is recorded.
func TriageOf(repo *trace.Repository, stream config.WorkstreamID, review int, finding string) (Triage, bool, error) {
	all, _, err := ReadTriage(repo, stream)
	if err != nil {
		return Triage{}, false, err
	}
	i := slices.IndexFunc(all, func(t Triage) bool { return t.Review == review && t.Finding == finding })
	if i < 0 {
		return Triage{}, false, nil
	}
	return all[i], true, nil
}

// RecordTriage records the decision on a finding as a new revision of the
// triage document. It is refused for a finding already decided. A decision Jev
// made is written by the Jev actor, any other by the manager.
func RecordTriage(ctx context.Context, repo *trace.Repository, stream config.WorkstreamID, t Triage, at time.Time) error {
	if t.Outcome != TriageCode && t.Outcome != TriageOwner {
		return fmt.Errorf("a triage outcome is %s or %s", TriageCode, TriageOwner)
	}
	if !trace.ValidDecider(t.DecidedBy, t.Judgment) || t.DecidedBy == trace.DecidedByOwner {
		return fmt.Errorf("triage decider %q with judgment %q is not valid", t.DecidedBy, t.Judgment)
	}
	all, revision, err := ReadTriage(repo, stream)
	if err != nil {
		return err
	}
	if slices.ContainsFunc(all, func(x Triage) bool { return x.Review == t.Review && x.Finding == t.Finding }) {
		return fmt.Errorf("finding %s of final review %d is already triaged", t.Finding, t.Review)
	}
	actor := trace.Actor{Kind: "agent", ID: trace.Manager}
	if t.ByJev() {
		actor = trace.JevActor
	}
	return write(ctx, repo, stream, append(slices.Clone(all), t), revision+1, actor, fmt.Sprintf("final-%d-%s", t.Review, t.Finding), at)
}

func write(ctx context.Context, repo *trace.Repository, stream config.WorkstreamID, all []Triage, revision int, actor trace.Actor, cause string, at time.Time) error {
	content, err := json.MarshalIndent(all, "", "  ")
	if err != nil {
		return err
	}
	h := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: TriageDocumentID, Revision: revision, Project: repo.Project(), Workstream: stream, At: at, Actor: actor, Cause: cause}
	return repo.RecordDocuments(ctx, []trace.Document{{Header: h, Path: TriagePath, Content: string(content) + "\n"}})
}

// PendingCode returns the findings triaged as needing code that no plan
// revision has taken yet, in the order they were decided.
func PendingCode(repo *trace.Repository, stream config.WorkstreamID) ([]Triage, error) {
	all, _, err := ReadTriage(repo, stream)
	if err != nil {
		return nil, err
	}
	var out []Triage
	for _, t := range all {
		if t.Outcome == TriageCode && t.Amendment == "" && t.Refused == "" {
			out = append(out, t)
		}
	}
	return out, nil
}

// RevisionText is what a plan revision states of the findings it takes.
func RevisionText(pending []Triage) string {
	var b strings.Builder
	b.WriteString("Blocking findings of final review triaged as needing code, which this revision takes:")
	for _, t := range pending {
		fmt.Fprintf(&b, "\n- final review %d, %s: %s (%s)", t.Review, t.Finding, t.Text, strings.TrimSpace(t.Note))
	}
	return b.String()
}

// Carry records the plan revision that took the pending findings.
func Carry(ctx context.Context, repo *trace.Repository, stream config.WorkstreamID, taken []Triage, amendment string, at time.Time) error {
	all, revision, err := ReadTriage(repo, stream)
	if err != nil || len(taken) == 0 {
		return err
	}
	next := slices.Clone(all)
	for i := range next {
		if slices.ContainsFunc(taken, func(t Triage) bool { return t.Review == next[i].Review && t.Finding == next[i].Finding }) {
			next[i].Amendment = amendment
		}
	}
	return write(ctx, repo, stream, next, revision+1, trace.Actor{Kind: "agent", ID: trace.Manager}, "amendment-"+amendment, at)
}
