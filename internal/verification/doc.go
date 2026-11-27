// Package verification holds the service-side rules of the verification
// lever: the project setting, the version-1 report a lever returns, the
// attribution of claims to flows, and the feature map that names the flows a
// change touches.
//
// A lever is a Dagger function in a product's repository. The service runs
// `dagger call <base arguments> --run-id <run ID> <agent arguments>` and
// exports the Directory it returns. The run ID is chosen by the service for
// every run; the lever uses it so that the product's service and driver start
// anew for that run. The Directory holds report.json and the evidence files
// the report names. The lever's list function, called as
// `dagger call <base arguments> list`, returns its named flows as a JSON array
// of strings.
//
// The package never interprets the arguments a lever receives and never
// derives a flow from them: only the flow field of a claim says which flow a
// run covered. An inconclusive run, claim or flow is never counted as a pass.
package verification
