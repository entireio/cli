package tokenstore

import "fmt"

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

// UseFailingBackendForTesting wraps a file backend so Set returns an error
// for any (service, user) pair where failSet reports true; all other
// operations behave normally. It lets tests exercise partial-write failure
// paths (e.g. the refresh-then-access ordering in contextTokenStore) without
// exposing the unexported store interface. Returns a cleanup function.
func UseFailingBackendForTesting(path string, failSet func(service, user string) bool) func() {
	return installFaultStore(faultStore{inner: &fileStore{path: path}, failSet: failSet})
}

// UseFailingGetBackendForTesting is the read-side analogue: Get returns an
// error for any (service, user) pair where failGet reports true. Used to test
// that callers surface a real store failure rather than swallowing it.
func UseFailingGetBackendForTesting(path string, failGet func(service, user string) bool) func() {
	return installFaultStore(faultStore{inner: &fileStore{path: path}, failGet: failGet})
}

func installFaultStore(fs faultStore) func() {
	backendMu.Lock()
	prevBackend := backend
	prevResolved := resolved
	backend = fs
	resolved = true
	backendMu.Unlock()

	return func() {
		backendMu.Lock()
		backend = prevBackend
		resolved = prevResolved
		backendMu.Unlock()
	}
}

type faultStore struct {
	inner   store
	failSet func(service, user string) bool
	failGet func(service, user string) bool
}

func (f faultStore) Get(service, user string) (string, error) {
	if f.failGet != nil && f.failGet(service, user) {
		return "", fmt.Errorf("injected Get failure for %s/%s", service, user)
	}
	//nolint:wrapcheck // thin test wrapper; callers handle errors
	return f.inner.Get(service, user)
}

func (f faultStore) Set(service, user, password string) error {
	if f.failSet != nil && f.failSet(service, user) {
		return fmt.Errorf("injected Set failure for %s/%s", service, user)
	}
	//nolint:wrapcheck // thin test wrapper; callers handle errors
	return f.inner.Set(service, user, password)
}

func (f faultStore) Delete(service, user string) error {
	//nolint:wrapcheck // thin test wrapper; callers handle errors
	return f.inner.Delete(service, user)
}
