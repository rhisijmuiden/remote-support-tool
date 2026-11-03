// Package version holds build information set at link time.
package version

// Set with -ldflags "-X github.com/rhisijmuiden/remote-support-tool/server/internal/version.Version=v0.1.0 ..."
var (
	Version   = "v0.0.0-dev"
	Commit    = "unknown"
	BuildDate = "unknown"
	// Channel is the release channel this binary was built for: "production" or "test".
	Channel = "test"
)
