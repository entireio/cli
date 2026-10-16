//go:build !darwin && !linux && !windows

package strategy

// processLookup finds nothing on platforms without an implementation: the hook
// records the push without a PID and the worker falls back to a fixed delay.
func processLookup() func(pid int) (ppid int, name string, ok bool) {
	return func(int) (int, string, bool) { return 0, "", false }
}
