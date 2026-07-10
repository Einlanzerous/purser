// Package version carries the build version, injected at link time via
// -ldflags "-X github.com/Einlanzerous/purser/internal/version.Version=…".
package version

// Version is the running build's version. "dev" for local/unstamped builds;
// the Makefile and Docker build stamp it from git / release-please.
var Version = "dev"
