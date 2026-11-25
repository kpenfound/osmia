// Package climb is the deterministic core of the Hill Climb workstream kind:
// the evaluator contract, the measurement runner, the keep/discard
// arithmetic, the protected-path gate and the attempt records.
//
// Agents propose changes and the service measures them. Every function here
// is plain Go arithmetic over measured values, so no agent decides whether a
// score improved.
package climb
