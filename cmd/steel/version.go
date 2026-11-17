package main

import "runtime/debug"

// version is stamped into release builds with
// -ldflags "-X main.version=v1.2.3".
var version string

// versionString reports which steel this is: the release version, or else
// the module version Go records at build time. For `go install ...@v1.2.3`
// that's the tag; for a build from a git checkout it's a pseudo-version
// naming the commit, with "+dirty" for uncommitted changes.
func versionString() string {
	if version != "" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		return info.Main.Version
	}
	return "unknown"
}
