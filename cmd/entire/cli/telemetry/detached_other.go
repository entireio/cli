//go:build !unix && !windows

package telemetry

// SpawnDetached is a no-op on platforms without detached-process support.
// Detached background work (analytics, pricing refresh) is best-effort, so
// callers ignore the returned (nil) error and simply skip the work here. The
// leading dir parameter mirrors the unix/windows signatures and is ignored.
func SpawnDetached(_, _ string, _ ...string) error {
	return nil
}
