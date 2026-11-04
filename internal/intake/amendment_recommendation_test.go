package intake

import (
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/shed"
)

func TestAmendmentRecommendationUsesAvailableDecisions(t *testing.T) {
	blocked := []shed.Entry{{Dissent: shed.Dissent{Objection: shed.Objection{ID: "veto", Kind: shed.Charter}}, Blocking: true}}
	for _, another := range []bool{true, false} {
		got := AmendmentRecommendation(blocked, nil, another)
		if !strings.HasPrefix(got, "do not approve yet:") || !strings.Contains(got, "overrule or reject") || strings.Contains(got, "ratify") || strings.Contains(got, "sustain") || strings.Contains(got, "redraft") || strings.Contains(got, "another debate round") != another {
			t.Errorf("recommendation %q, another round %t", got, another)
		}
	}
	if got := AmendmentRecommendation(nil, []shed.Record{{Failure: "interrupted"}}, false); got != "do not approve yet: a committee member failed to complete its review" {
		t.Errorf("failure recommendation %q", got)
	}
	if got := shed.Recommend(nil); got != "ratify: no objection stands" {
		t.Errorf("ratification recommendation %q", got)
	}
}
