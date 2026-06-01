package tokenstore

// UseFileBackendForTesting points the package-level token store at a
// per-test JSON file and returns a cleanup function that restores the
// previous backend (re-resolving on next use). It serializes against
// concurrent Get/Set/Delete calls via the same mutex they use, so it is
// safe to call from any test even if other goroutines are mid-call.
//
// Each call replaces the active backend; tests that invoke this from
// parallel subtests will trample each other and should arrange their own
// per-subtest paths if they actually need isolation.
func UseFileBackendForTesting(path string) func() {
	backendMu.Lock()
	prevBackend := backend
	prevResolved := resolved
	backend = &fileStore{path: path}
	resolved = true
	backendMu.Unlock()

	return func() {
		backendMu.Lock()
		backend = prevBackend
		resolved = prevResolved
		backendMu.Unlock()
	}
}
