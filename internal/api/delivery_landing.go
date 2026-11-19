package api

import "time"

// DeliveryLanding records a workstream's landing: the signed commit of its
// current push that the service fast-forwarded the upstream base branch to.
// Branch is the feature branch, which stays where it is. Remote names the
// clone's remote for the upstream repository Upstream, BaseBranch the branch
// that moved and BaseBefore the commit it was at when the landing began.
// Confirmed is the commit a fresh fetch of the base branch found after the push,
// which holds Commit. DecidedBy is who decided to land, the owner or Jev, with
// the judgment Jev's decision rests on.
type DeliveryLanding struct {
	Push       int       `json:"push"`
	Review     int       `json:"review"`
	Branch     string    `json:"branch"`
	Commit     string    `json:"commit"`
	Reviewed   string    `json:"reviewed"`
	Remote     string    `json:"remote"`
	Upstream   string    `json:"upstream"`
	BaseBranch string    `json:"base_branch"`
	BaseBefore string    `json:"base_before"`
	Confirmed  string    `json:"confirmed"`
	At         time.Time `json:"at"`
	DecidedBy  string    `json:"decided_by"`
	Judgment   string    `json:"judgment,omitempty"`
}
