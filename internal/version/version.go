// Package version holds build-time version information injected via ldflags.
package version

var (
	// Version is the semantic version of the build.
	Version = "dev"
	// BuildDate is the RFC 3339 timestamp of the build.
	BuildDate = "unknown"
	// BuildRef is the VCS reference (commit SHA) of the build.
	BuildRef = "unknown"
)
