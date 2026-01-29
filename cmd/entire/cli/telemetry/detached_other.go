//go:build !unix

package telemetry

// spawnDetachedAnalytics is a no-op on non-Unix platforms.
// Windows support for detached processes would require different syscall flags
// (CREATE_NEW_PROCESS_GROUP, DETACHED_PROCESS), but telemetry is best-effort
// so we simply skip it on unsupported platforms.
func spawnDetachedAnalytics(string) {
	// No-op: detached subprocess spawning not implemented for this platform
}
