package checkselect

import (
	"fmt"
	"math"

	"github.com/kpenfound/agentcore/jev/systemone"
)

func usableNouls(r systemone.Response, count int, question func(int) string) bool {
	if len(r.Answers) != count {
		return false
	}
	for i := range count {
		a, ok := r.Answers[question(i)]
		if !ok || a.Kind != systemone.KindNoul || math.IsNaN(a.Noul) || a.Noul < 0 || a.Noul > 1 {
			return false
		}
	}
	return true
}

// RankingAcceptance returns an empty string for a usable ranking with a
// threshold selection or fallback picks, or the reason the caller must use
// its ranking-failure path. Selection and fallback-pick behavior are shared
// with Selected and FallbackPicks.
func RankingAcceptance(r systemone.Response, candidates []string, threshold float64, picks int) string {
	if !usableNouls(r, len(candidates), Question) {
		return "unusable ranking answer"
	}
	if len(Selected(r, candidates, threshold)) == 0 && len(FallbackPicks(r, candidates, picks)) == 0 {
		return fmt.Sprintf("no check reached probability %.2f", threshold)
	}
	return ""
}
