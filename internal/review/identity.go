package review

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/kpenfound/osmia/internal/coreadapter"
)

// UnitReviewIdentity is the durable identity of a prepared review. A returned
// review must match Candidate and DiffSHA256 before its verdict can be used.
type UnitReviewIdentity struct {
	Discoveries string                `json:"discoveries,omitempty"`
	Subject     string                `json:"subject"`
	Candidate   coreadapter.Candidate `json:"candidate"`
	DiffSHA256  string                `json:"diff_sha256"`
	Report      string                `json:"report"`
	Seal        int                   `json:"seal"`
}

func (i UnitReviewIdentity) Matches(result coreadapter.ReviewResult) bool {
	return result.Subject == i.Subject && result.Candidate == i.Candidate && result.DiffSHA256 == i.DiffSHA256
}

// IdentityInPrompt reads the candidate identity a review prompt pins:
// the indented JSON after its introduction, up to the first blank line.
func IdentityInPrompt(prompt string) (UnitReviewIdentity, error) {
	const start = "The candidate identity is:\n"
	_, body, ok := strings.Cut(prompt, start)
	if !ok {
		return UnitReviewIdentity{}, errors.New("review turn lacks candidate identity")
	}
	body, _, ok = strings.Cut(body, "\n\n")
	if !ok {
		return UnitReviewIdentity{}, errors.New("review turn lacks the end of its candidate identity")
	}
	var identity UnitReviewIdentity
	err := json.Unmarshal([]byte(body), &identity)
	return identity, err
}
