// Package remotehelper holds the shared name of the git remote-helper
// binary; the protocol implementation lives in the subpackages (githelper,
// transport, …) and the binary itself in cmd/git-remote-entire.
package remotehelper

// BinaryName is the git remote-helper executable name. Git resolves
// `entire://` URLs by exec'ing a binary called this on PATH; cmd/git-remote-entire
// is built and shipped under this name and uses it for its usage text and
// git agent string.
const BinaryName = "git-remote-entire"
