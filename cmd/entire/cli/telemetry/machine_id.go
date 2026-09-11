package telemetry

import (
	"sync"

	"github.com/denisbrodbeck/machineid"
)

// machineIDAppKey scopes the derived ID to this application, so the value sent
// to PostHog is not the raw platform UUID.
const machineIDAppKey = "entire-cli"

// machineid.ProtectedID is not a cheap lookup. On macOS it shells out to
// `ioreg -rd1 -c IOPlatformExpertDevice` (measured p50 11.8ms); Linux and
// Windows read a file or the registry. Every payload builder needs the same
// value, and payload builders run in loops — TrackSkillInvocationsDetached
// builds one per skill event — so an uncached call made the per-event cost
// 11.6ms and put 218ms of blocking `ioreg` on the hook path for a 20-event
// batch. The machine ID cannot change while the process runs, so resolve it
// once. paths.WorktreeRoot memoizes `git rev-parse` for the same reason.
//
// The error is cached too: a failed lookup means telemetry is skipped (callers
// treat it as "no payload"), and retrying it once per event would reintroduce
// exactly the per-event subprocess this exists to remove.

// machineIDResolver performs the underlying platform lookup. Swapped by tests.
//
//nolint:gochecknoglobals // test seam, set and restored by in-package tests.
var machineIDResolver = func() (string, error) { return machineid.ProtectedID(machineIDAppKey) }

// cachedMachineID memoizes machineIDResolver for the process lifetime.
//
//nolint:gochecknoglobals // process-lifetime memoization, reset only by tests.
var cachedMachineID = sync.OnceValues(func() (string, error) { return machineIDResolver() })

// telemetryMachineID returns the app-scoped machine ID used as the PostHog
// distinct ID, resolving it at most once per process.
func telemetryMachineID() (string, error) {
	return cachedMachineID()
}

// resetMachineIDCacheForTest clears the memoized value so a test can install a
// different resolver. Not safe for concurrent use; callers must not run in
// parallel with anything that builds a payload.
func resetMachineIDCacheForTest() {
	cachedMachineID = sync.OnceValues(func() (string, error) { return machineIDResolver() })
}
