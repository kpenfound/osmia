package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
)

const projectSpecDraftInputDocument = "project-spec-draft-input"

// projectSpecDraftInput pins the documents shown to one architect turn. It is
// recorded atomically with the drafting transition and reused after a restart.
type projectSpecDraftInput struct {
	Turn    string                           `json:"turn"`
	Members map[config.WorkstreamID]shed.Pin `json:"members"`
	Guarded map[config.WorkstreamID]shed.Pin `json:"guarded,omitempty"`
}

func changedContractCriterion(slice trace.Slice, content string) int {
	before, after := plan.ParseSpec(slice.Spec), plan.ParseSpec(content)
	for _, n := range slice.ContractCriteria {
		old, had := before.Criterion(n)
		next, has := after.Criterion(n)
		if !had || !has || old.Text != next.Text {
			return n
		}
	}
	return 0
}

func memberSliceRefreshAllowed(slice trace.Slice, spec, _ string) bool {
	return changedContractCriterion(slice, spec) == 0
}

func readProjectSpecDraftInput(wt *trace.Repository, host config.WorkstreamID) (projectSpecDraftInput, int, error) {
	docs, err := trace.Read[trace.Document](wt, host)
	if err != nil {
		return projectSpecDraftInput{}, 0, err
	}
	var latest trace.Document
	for _, d := range docs {
		if d.ID == projectSpecDraftInputDocument && d.Revision > latest.Revision {
			latest = d
		}
	}
	var input projectSpecDraftInput
	if latest.Revision != 0 {
		err = json.Unmarshal([]byte(latest.Content), &input)
	}
	return input, latest.Revision, err
}

func pinnedMemberSpecAndPlan(repo *trace.Repository, member config.WorkstreamID, pin shed.Pin) (string, string, error) {
	spec, err := trace.Get[trace.Document](repo, member, plan.SpecDocument, pin.Spec)
	if err != nil {
		return "", "", err
	}
	graph, err := trace.Get[trace.Document](repo, member, plan.PlanDocument, pin.Plan)
	return spec.Content, graph.Content, err
}

func (s *Service) contractGuardRefusal(wt *trace.Repository, g trace.Workgroup, spec trace.ProjectSpec) (string, error) {
	for _, member := range g.Members[1:] {
		_, _, repo, api := s.conversationTrace(string(member))
		if api != nil {
			return "", fmt.Errorf("workstream %s: %s", member, api.Message)
		}
		content, _, err := memberSpecAndPlan(repo, member)
		if err != nil {
			return "", err
		}
		slice, ok := trace.SliceFor(spec, member)
		if !ok {
			continue
		}
		if n := changedContractCriterion(slice, content); n != 0 {
			message := fmt.Sprintf("workgroup %s, member %s, criterion %d: the project spec's contracts are being redrafted; read the packet again", g.ID, member, n)
			state, err := wt.Workflow(g.Members[0], projectSpecDraftSubject)
			if err != nil {
				return "", err
			}
			if state.Value == "failed" {
				transitions, err := trace.Read[trace.Transition](wt, g.Members[0])
				if err != nil {
					return "", err
				}
				for i := len(transitions) - 1; i >= 0; i-- {
					tr := transitions[i]
					if tr.Subject == projectSpecDraftSubject && tr.To == "failed" {
						message += "; redraft failed: " + tr.Reason
						break
					}
				}
			}
			return message, nil
		}
	}
	return "", nil
}

func (s *Service) snapshotProjectSpecDraft(members []trace.CrossRepoMember, spec trace.ProjectSpec) (projectSpecDraftInput, error) {
	input := projectSpecDraftInput{Members: map[config.WorkstreamID]shed.Pin{}, Guarded: map[config.WorkstreamID]shed.Pin{}}
	for _, m := range members {
		pin, err := latestPin(m.Project, m.Workstream)
		if err != nil {
			return input, err
		}
		input.Members[m.Workstream] = pin
		content, _, err := pinnedMemberSpecAndPlan(m.Project, m.Workstream, pin)
		if err != nil {
			return input, err
		}
		if slice, ok := trace.SliceFor(spec, m.Workstream); ok && changedContractCriterion(slice, content) != 0 {
			input.Guarded[m.Workstream] = pin
		}
	}
	return input, nil
}

func projectSpecDraftRecorded(wt *trace.Repository, turn string) (bool, error) {
	docs, err := trace.Read[trace.Document](wt, "")
	if err != nil {
		return false, err
	}
	for _, d := range docs {
		if d.ID == trace.ProjectSpecsDocument && d.Cause == turn {
			return true, nil
		}
	}
	return false, nil
}

// supersedeUnpinnedProjectSpecDraft settles an unpinned turn without recording
// its output, so its replacement can run with an explicit member snapshot.
func (s *Service) supersedeUnpinnedProjectSpecDraft(ctx context.Context, wt *trace.Repository, host config.WorkstreamID, turn string) error {
	t, err := wt.Thread(host, architectAgent)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, q := range t.Turns {
		if q.Request.TurnID != turn || !q.CompletedAt.IsZero() {
			continue
		}
		switch {
		case q.Response != nil:
			op, err := thread.TurnOperation(wt.Project(), "project-spec-draft", thread.TurnInput{Workstream: host, Agent: architectAgent, Turn: turn})
			if err != nil {
				return err
			}
			dispatcher := thread.Dispatcher{Runner: s.threadRunner(s.current(), wt, nil, s.now)}
			_, err = dispatcher.Apply(ctx, op)
			return err
		case q.Claim != nil:
			return wt.AbandonTurn(ctx, host, architectAgent, turn, s.now())
		default:
			_, err := wt.CancelQueuedTurn(ctx, host, architectAgent, turn, s.now(), projectSpecDraftActor, "the project spec draft has no recorded member revisions; a pinned turn supersedes it")
			return err
		}
	}
	return nil
}

func (s *Service) finishProjectSpecDraft(ctx context.Context, wt *trace.Repository, host config.WorkstreamID, turn string) error {
	state, err := wt.Workflow(host, projectSpecDraftSubject)
	if err != nil {
		return err
	}
	_, err = wt.Transact(ctx, trace.Transaction{ExpectedVersion: state.Version,
		Transition: trace.Transition{Header: trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: turn + "-drafted", Revision: 1, Project: wt.Project(), Workstream: host, At: s.now(), Actor: projectSpecDraftActor, Cause: turn}, Subject: projectSpecDraftSubject, From: state.Value, To: "drafted", Reason: "the project spec is recorded, covering every member"}})
	return err
}
