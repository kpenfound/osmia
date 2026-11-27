package intake

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/trace"
)

var (
	shedRoundTurn      = regexp.MustCompile(`^shed-([0-9]+)-`)
	amendmentRoundTurn = regexp.MustCompile(`^amend-(.+)-round-([0-9]+)-`)
)

// SettleCommitteeAnswers cancels the answers queued on a committee member's
// thread that the round which asked can no longer take: an answer whose plan
// debate round was ratified, concluded or left the shed, or whose amendment
// round is no longer being debated, before it ran. A thread runs its turns in
// order, so such an answer would hold every later turn of the member, in a
// plan debate, an amendment debate or a final review, behind it. An answer a
// live round still waits for, one already claimed or attempted, and one whose
// asking turn the trace cannot place are kept.
func SettleCommitteeAnswers(ctx context.Context, repository *trace.Repository, stream config.WorkstreamID, member string, at time.Time) error {
	t, err := repository.Thread(stream, member)
	if err != nil {
		return err
	}
	asked, err := repository.Questions(stream)
	if err != nil {
		return err
	}
	for _, q := range t.Turns {
		if !strings.HasPrefix(q.Request.TurnID, questions.TurnID("")) || !q.CompletedAt.IsZero() || q.Claim != nil || len(q.Attempts) > 0 || q.Response != nil {
			continue
		}
		origin := answerOrigin(t, asked, q.Request.TurnID)
		if origin == "" {
			continue
		}
		over, why, err := committeeRoundOver(repository, stream, origin)
		if err != nil {
			return err
		}
		if !over {
			continue
		}
		reason := fmt.Sprintf("the answer arrived after %s; the round that asked in turn %s no longer runs, so the answer is not delivered to %s", why, origin, member)
		if _, err := repository.CancelQueuedTurn(ctx, stream, member, q.Request.TurnID, at, ShedActor, reason); err != nil {
			return err
		}
	}
	return nil
}

func answerOrigin(t trace.Thread, asked []trace.QuestionState, turn string) string {
	for range len(t.Turns) + 1 {
		if !strings.HasPrefix(turn, questions.TurnID("")) {
			return turn
		}
		id := strings.TrimPrefix(turn, questions.TurnID(""))
		next := ""
		for _, q := range asked {
			if q.Asked.ID == id && q.Asked.Thread == t.Identity.ThreadID {
				next = q.Asked.Turn
			}
		}
		if next == "" {
			return ""
		}
		turn = next
	}
	return ""
}

func committeeRoundOver(repository *trace.Repository, stream config.WorkstreamID, turn string) (bool, string, error) {
	if m := amendmentRoundTurn.FindStringSubmatch(turn); m != nil {
		id := m[1]
		n, _ := strconv.Atoi(m[2])
		state, err := repository.Workflow(stream, AmendmentSubject(id))
		if err != nil {
			return false, "", err
		}
		round, err := CurrentAmendmentRound(repository, stream, id)
		if err != nil {
			return false, "", err
		}
		if state.Value == "debating" && round == n {
			return false, "", nil
		}
		return true, fmt.Sprintf("round %d of amendment %s ended (the amendment is %s)", n, id, state.Value), nil
	}
	if m := shedRoundTurn.FindStringSubmatch(turn); m != nil {
		n, _ := strconv.Atoi(m[1])
		feature, err := repository.Workflow(stream, trace.FeatureSubject)
		if err != nil {
			return false, "", err
		}
		if feature.Value != InShedState {
			return true, fmt.Sprintf("the plan debate ended (the workstream is %s)", FeatureState(feature.Value)), nil
		}
		shed, err := repository.Workflow(stream, ShedSubject)
		if err != nil {
			return false, "", err
		}
		if kind, round, ok := ShedState(shed.Value); ok && round == n && (kind == "round" || kind == "waiting") {
			return false, "", nil
		}
		return true, fmt.Sprintf("plan debate round %d ended (the shed is %s)", n, shed.Value), nil
	}
	return false, "", nil
}
