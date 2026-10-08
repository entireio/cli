package agent

import "maps"

// SnapshotRegistryForTesting captures the registry and returns a func that
// restores it. External-agent discovery registers plugins into the
// process-global registry, so a test that triggers it otherwise leaks entries
// into every later test that walks List() — which then execs a binary that the
// leaking test's TempDir cleanup has already deleted. Test-only.
func SnapshotRegistryForTesting() func() {
	registryMu.Lock()
	defer registryMu.Unlock()

	snapshot := maps.Clone(registry)
	return func() {
		registryMu.Lock()
		defer registryMu.Unlock()
		registry = snapshot
	}
}
