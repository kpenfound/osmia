package trace

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
)

// The states of a charter proposal's workflow subject. A proposal waits for
// the owner in CharterProposed; a ratified one is CharterRatified until its
// rule is recorded in the charter, then CharterChartered.
const (
	CharterProposed  = "proposed"
	CharterRatified  = "ratified"
	CharterDeclined  = "declined"
	CharterChartered = "chartered"
)

// The owner's decisions on a charter proposal.
const (
	CharterRatify  = "ratify"
	CharterDecline = "decline"
)

// CharterSubject is the workflow subject of the charter proposal made from
// the owner's ruling on a question.
func CharterSubject(question string) string { return "charter_" + question }

// CharterProposalPath is the workstream document that records the charter
// proposal made from the owner's ruling on a question, and
// CharterProposalID its record ID.
func CharterProposalPath(question string) string { return "questions/" + question + "/charter.json" }
func CharterProposalID(question string) string   { return "charter_" + question }

// CharterProposal is the content of every revision of
// questions/<n>/charter.json. Revision 1 is the chief of staff's proposal:
// the rule text, the number the rule would take in the charter as it was
// then, and the source ruling, its record path, revision and the owner's
// words. The owner's decision adds Decision and Note. A ratified proposal's
// last revision adds the number the rule took and Charter, the charter.md
// revision that records it.
type CharterProposal struct {
	Question       string `json:"question"`
	Ruling         string `json:"ruling"`
	RulingRevision int    `json:"ruling_revision"`
	OwnerResponse  string `json:"owner_response"`
	Rule           string `json:"rule"`
	Number         int    `json:"number"`
	Decision       string `json:"decision,omitempty"`
	Note           string `json:"note,omitempty"`
	Charter        int    `json:"charter,omitempty"`
}

// CharterProposalState is one charter proposal: its first and latest
// document revisions, the latest content and its workflow state.
type CharterProposalState struct {
	Workstream config.WorkstreamID
	Proposed   Document
	Latest     Document
	Proposal   CharterProposal
	State      WorkflowState
}

// CharterProposals returns every charter proposal of the project, oldest
// first, from one trace snapshot.
func (r *Repository) CharterProposals() ([]CharterProposalState, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	records, streams, err := r.scan()
	if err != nil {
		return nil, err
	}
	var out []CharterProposalState
	for _, stream := range streams {
		_, v, err := r.loadWorkflow(stream)
		if err != nil {
			return nil, fmt.Errorf("workstream %s: %w", stream, err)
		}
		found, err := charterProposals(records, v, stream)
		if err != nil {
			return nil, err
		}
		out = append(out, found...)
	}
	slices.SortStableFunc(out, func(x, y CharterProposalState) int {
		return cmp.Or(x.Proposed.At.Compare(y.Proposed.At), strings.Compare(string(x.Workstream), string(y.Workstream)), cmp.Compare(len(x.Proposal.Question), len(y.Proposal.Question)), strings.Compare(x.Proposal.Question, y.Proposal.Question))
	})
	return out, nil
}

// charterProposals returns the charter proposals of one workstream.
func charterProposals(records []Record, v *workflowView, stream config.WorkstreamID) ([]CharterProposalState, error) {
	byPath := map[string]*CharterProposalState{}
	for _, rec := range records {
		d, ok := rec.(Document)
		if !ok || d.Workstream != stream || !charterProposalPath(strings.Split(d.Path, "/")) {
			continue
		}
		s := byPath[d.Path]
		if s == nil {
			s = &CharterProposalState{Workstream: stream, Proposed: d}
			byPath[d.Path] = s
		}
		s.Latest = d
	}
	var out []CharterProposalState
	for _, path := range slices.Sorted(maps.Keys(byPath)) {
		s := byPath[path]
		if err := json.Unmarshal([]byte(s.Latest.Content), &s.Proposal); err != nil {
			return nil, fmt.Errorf("%s revision %d: %w", s.Latest.Path, s.Latest.Revision, err)
		}
		s.State = v.states[CharterSubject(s.Proposal.Question)]
		out = append(out, *s)
	}
	return out, nil
}

// charterProposalPath reports whether parts name a charter proposal of a
// workstream, questions/<n>/charter.json, where the question is a key.
func charterProposalPath(parts []string) bool {
	return len(parts) == 3 && parts[0] == "questions" && key(parts[1]) && parts[2] == "charter.json"
}

// ProposeCharter records the chief of staff's proposal to make the owner's
// ruling on question id a standing charter rule: revision 1 of the
// question's charter.json, with the rule text, the number it would take and
// the ruling it comes from, and the proposal's move to CharterProposed, in
// one commit. next returns the number from charter.md as it is on disk, which
// is read, not recorded. A turn of another role fails. A question that does not exist,
// has no ruling of the owner or already has a proposal, and rule text that
// is empty, spans lines or holds an HTML comment marker, are refused with
// *QuestionRefused.
func (r *Repository) ProposeCharter(ctx context.Context, agent string, scope coreadapter.Scope, id, rule string, next func(charter string) int, at time.Time) (CharterProposal, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	turn, log, v, records, err := r.chiefTurn(ctx, agent, scope, at)
	if err != nil {
		return CharterProposal{}, err
	}
	stream := config.WorkstreamID(scope.Workstream)
	existing := questions(records, v, stream)
	i := slices.IndexFunc(existing, func(q QuestionState) bool { return q.Asked.ID == id })
	subject := CharterSubject(id)
	rule = strings.TrimSpace(rule)
	switch {
	case i < 0:
		return CharterProposal{}, refused("there is no question %s in this workstream", id)
	case existing[i].Ruling == nil || existing[i].Ruling.Decision != DecisionRuling:
		return CharterProposal{}, refused("question %s has no ruling from the owner; only a ruling the owner gave can become a charter rule", id)
	case v.states[subject].Value != "":
		return CharterProposal{}, refused("the owner's ruling on question %s already has a charter proposal, which is %s", id, v.states[subject].Value)
	case rule == "":
		return CharterProposal{}, refused("rule is required: the standing rule as the charter should state it")
	case strings.ContainsAny(rule, "\r\n"):
		return CharterProposal{}, refused("rule must be one line: a charter rule is one numbered item")
	case strings.Contains(rule, "<!--") || strings.Contains(rule, "-->"):
		return CharterProposal{}, refused("rule must not contain an HTML comment marker")
	}
	current, err := r.readFile("charter.md")
	if err != nil {
		return CharterProposal{}, err
	}
	number := next(string(current))
	if number < 1 {
		return CharterProposal{}, fmt.Errorf("a charter rule number starts at 1")
	}
	ruling := existing[i].Ruling
	// The owner's words are in the ruling's first revision; the relay keeps
	// them.
	proposal := CharterProposal{Question: id, Ruling: recordPath(*ruling), RulingRevision: ruling.Revision, OwnerResponse: ruling.OwnerResponse, Rule: rule, Number: number}
	content, err := json.MarshalIndent(proposal, "", "  ")
	if err != nil {
		return CharterProposal{}, err
	}
	actor := Actor{Kind: "agent", ID: agent}
	doc := Document{Header: Header{Schema: "osmia.trace.document", Version: Version, ID: CharterProposalID(id), Revision: 1, Project: r.project, Workstream: stream,
		At: at, Actor: actor, Cause: turn.Request.ID, Depth: turn.Request.Depth + 1}, Path: CharterProposalPath(id), Content: string(content) + "\n"}
	h := Header{Schema: "osmia.trace.transition", Version: Version, ID: subject + "_" + CharterProposed, Revision: 1, Project: r.project, Workstream: stream,
		At: at, Actor: actor, Cause: turn.Request.ID, Depth: turn.Request.Depth + 1}
	tx := Transaction{ExpectedVersion: v.states[subject].Version, Transition: Transition{Header: h, Subject: subject, From: "", To: CharterProposed,
		Reason: fmt.Sprintf("The chief of staff proposed the owner's ruling on question %s as charter rule %d", id, number)}}
	files, removed, err := r.documentFiles(ctx, []Document{doc})
	if err != nil {
		return CharterProposal{}, err
	}
	staged, _, err := r.stage(stream, log, v, nil, tx)
	if err != nil {
		return CharterProposal{}, err
	}
	maps.Copy(files, staged)
	if err := r.publishTree(ctx, files, removed); err != nil {
		return CharterProposal{}, err
	}
	_ = r.wake.Notify(context.Background())
	return proposal, nil
}
