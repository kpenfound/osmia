// Package buildinfo holds the release version and commit stamped into the
// osmia binary at link time:
//
//	go build -ldflags "-X github.com/kpenfound/osmia/internal/buildinfo.Version=v0.1.0 -X github.com/kpenfound/osmia/internal/buildinfo.Commit=abc123" ./cmd/osmia
//
// An unstamped build reports version dev and an empty commit.
package buildinfo

var (
	// Version is the release version, or dev for an unstamped build.
	Version = "dev"
	// Commit is the source commit the release was built from, or empty.
	Commit = ""
)
