// Package scripts tests scripts/build-sandbox.sh and the osmia sandbox build
// and check commands, which carry the same recipe, against a fake sbx and a
// fake dagger. The commands' tests live here because the script and the fake
// are shared with them and a test container holds only what a test embeds.
package scripts
