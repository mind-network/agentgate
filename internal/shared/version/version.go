// Package version provides build-time version information.
package version

import "fmt"

// Populated at build time with -ldflags.
var (
	Semver = "0.1.0"
	GitSHA = "unknown"
)

// String returns a human-readable version string.
func String() string {
	return fmt.Sprintf("agentgate v%s (%s)", Semver, GitSHA[:min(7, len(GitSHA))])
}
