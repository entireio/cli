package agent

import (
	"maps"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
)

// SnapshotForTesting captures the registry's current contents and returns a
// function that restores it to exactly that state.
//
// Tests that trigger agent registration as a side effect — external-agent
// discovery via runAgentList, for example — must defer or t.Cleanup the
// returned restore. Otherwise mock agents backed by t.TempDir binaries leak
// into the process-global registry and corrupt later tests in the package:
// GetAgentsWithHooksInstalled would exec their now-deleted binaries, and only
// TempDir cleanup ordering keeps that from mis-reporting installed agents.
//
// Restore installs a fresh copy of the snapshot each time, so it is repeatable:
// a test that calls it directly and also registers it with t.Cleanup still ends
// with the captured state. Handing back the snapshot map itself would alias it
// into the registry, and the next Register would silently edit the snapshot.
//
// Capture and restore both hold registryMu, so readers never observe a
// half-updated registry. Concurrent *writers* are not accounted for: restore
// replaces the whole map, so any Register that lands between capture and
// restore is discarded. Callers are serial tests, where that cannot happen.
func SnapshotForTesting() func() {
	registryMu.Lock()
	defer registryMu.Unlock()
	snapshot := make(map[types.AgentName]Factory, len(registry))
	maps.Copy(snapshot, registry)
	return func() {
		registryMu.Lock()
		defer registryMu.Unlock()
		registry = maps.Clone(snapshot)
	}
}
