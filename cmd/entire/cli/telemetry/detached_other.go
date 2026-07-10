//go:build !unix && !windows

package telemetry

// SpawnDetached is a no-op on platforms without detached-process support.
// Detached background work (analytics, pricing refresh) is best-effort, so
// callers ignore the returned (nil) error and simply skip the work here.
func SpawnDetached(string, ...string) error {
	return nil
}
