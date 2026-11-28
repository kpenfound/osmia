// Package verifytool is the service side of the verify tool: it runs a
// project's verification lever for a reviewer, the final reviewer or the
// manager, records the run in the trace, retains its report and evidence
// outside the target repository and places the retained files into agent
// views.
//
// The run identity, the retained evidence and the view placement are shared by
// every lever run the service makes, so other callers reuse NewRunID, Retain
// and Place. The contract of the lever itself is in docs/testing.md.
package verifytool
