# M4 amendments and standing rulings

`TestM4AmendmentDemonstration` in `internal/service/amendment_demo_test.go`
starts with a sealed, building workstream. A fake mason calls `amend` with
`spec#1`, a proposed change and a reason. Its unit waits while another unit
continues. The fake architect drafts a revised criterion, the committee runs
one amendment round, and the chief of staff presents the packet. The service
restarts before the owner decides; the same packet and round remain available.
`TestAmendmentRoundResumesCompletedMember` stops after one committee member
objects, then resumes the round. It checks that the member is not run again,
the objection reaches the packet, and the reply and packet are written once.

The owner reads the packet through `GET /v1/amendment/<workstream>/<n>` or
`osmia amendment <workstream> <n>`, then chooses `approve`, `reject`, `round`
or `overrule` through the API or CLI. Approval versions the spec, advances
the seal and returns units addressing the changed criterion to implementation.
Other units continue under the new seal. Rejection keeps the prior spec, plan
and seal in force. Either decision delivers the owner's words to the requester
and resumes its waiting unit. A later round requires an explicit owner
decision; automatic debate runs once per presentation.

`TestM4CharterDemonstration` starts with the chief of staff's `propose_charter`
call on an owner ruling. `osmia charter` lists the open proposal. The owner
ratifies or declines it with `osmia charter <workstream> <question> ratify`
or `decline` (or the corresponding API request). Ratification appends a
numbered rule to `charter.md` with its ruling source. Every in-flight
workstream receives the rule and ruling as a charter notice in its next
bundle, including workstreams other than the one that raised the question.
Declining leaves the charter and bundles unchanged.

Run the demonstrations with `dagger check`. The tests use local repositories,
fake agents and the local service client; they do not contact model providers
or remote delivery services.
