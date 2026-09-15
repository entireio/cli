package tokenstore

import (
	"errors"
	"path/filepath"
	"testing"
)

// scriptedStore is a primary store whose behaviour tests dictate. Values are
// keyed by service+"\x00"+user.
type scriptedStore struct {
	getErr, setErr, delErr error
	values                 map[string]string
	sets, gets, dels       int
}

func newScriptedStore() *scriptedStore { return &scriptedStore{values: map[string]string{}} }

func (s *scriptedStore) key(service, user string) string { return service + "\x00" + user }

func (s *scriptedStore) Get(service, user string) (string, error) {
	s.gets++
	if s.getErr != nil {
		return "", s.getErr
	}
	v, ok := s.values[s.key(service, user)]
	if !ok {
		return "", ErrNotFound
	}
	return v, nil
}

func (s *scriptedStore) Set(service, user, password string) error {
	s.sets++
	if s.setErr != nil {
		return s.setErr
	}
	s.values[s.key(service, user)] = password
	return nil
}

func (s *scriptedStore) Delete(service, user string) error {
	s.dels++
	if s.delErr != nil {
		return s.delErr
	}
	if _, ok := s.values[s.key(service, user)]; !ok {
		return ErrNotFound
	}
	delete(s.values, s.key(service, user))
	return nil
}

// Tests that need the process backend re-resolved use the existing
// resetBackendForTesting helper in file_test.go; do not add a second one.

func TestResolveBackend_ExplicitFileEnvIsARecordingFileStore(t *testing.T) {
	isolateConfigDir(t)
	got := resolveBackend(backendInputs{envValue: backendFile, goos: "darwin"})
	rec, ok := got.(recordingStore)
	if !ok || rec.name != backendFile {
		t.Fatalf("got %T (%+v), want recordingStore{name: file}", got, got)
	}
	if _, ok := rec.inner.(*fileStore); !ok {
		t.Fatalf("inner = %T, want *fileStore", rec.inner)
	}
}

func TestResolveBackend_ExplicitKeyringEnvIsARecordingKeyringStore(t *testing.T) {
	isolateConfigDir(t)
	for _, v := range []string{backendKeyring, "anything-else"} {
		got := resolveBackend(backendInputs{envValue: v, remembered: backendFile, testDir: t.TempDir(), goos: "linux"})
		rec, ok := got.(recordingStore)
		if !ok || rec.name != backendKeyring {
			t.Fatalf("env=%q: got %T (%+v), want recordingStore{name: keyring}", v, got, got)
		}
		if _, ok := rec.inner.(keyringStore); !ok {
			t.Fatalf("env=%q: inner = %T, want keyringStore (explicit selection never falls back)", v, rec.inner)
		}
	}
}

func TestResolveBackend_RememberedFileBeatsTestDirAndPlatform(t *testing.T) {
	isolateConfigDir(t)
	got := resolveBackend(backendInputs{remembered: backendFile, testDir: t.TempDir(), goos: "linux"})
	fs, ok := got.(*fileStore)
	if !ok {
		t.Fatalf("got %T, want *fileStore (remembered choice, not recording: nothing new to learn)", got)
	}
	if fs.path != FileBackendPath() {
		t.Fatalf("path = %q, want the default %q", fs.path, FileBackendPath())
	}
}

func TestResolveBackend_TestDirBeatsKeyring(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	got := resolveBackend(backendInputs{testDir: dir, goos: "linux"})
	fs, ok := got.(*fileStore)
	if !ok || fs.path != filepath.Join(dir, "tokens.json") {
		t.Fatalf("got %T (%+v), want the testdirs file store", got, got)
	}
}

// Not parallel, and config-dir isolated: Task 3 gives the fallback store a
// file half at the default path, and a parallel test that reaches it would
// read the path environment while sequential tests are setting it.
func TestResolveBackend_DefaultIsFallbackOnSecretServicePlatformsOnly(t *testing.T) {
	isolateConfigDir(t)
	for goos, wantFallback := range map[string]bool{
		"linux": true, "freebsd": true, "openbsd": true, "netbsd": true, "dragonfly": true,
		"darwin": false, "windows": false,
	} {
		got := resolveBackend(backendInputs{goos: goos})
		switch got.(type) {
		case *fallbackStore:
			if !wantFallback {
				t.Fatalf("goos=%s: got the fallback store, want a bare keyringStore", goos)
			}
		case keyringStore:
			if wantFallback {
				t.Fatalf("goos=%s: got a bare keyringStore, want the fallback store", goos)
			}
		default:
			t.Fatalf("goos=%s: got %T, want *fallbackStore or keyringStore", goos, got)
		}
	}
}

func TestRecordingStore_SetRemembersTheExplicitBackend(t *testing.T) {
	isolateConfigDir(t)
	inner := newScriptedStore()

	if err := (recordingStore{inner: inner, name: backendFile}).Set("svc", "alice", "tok"); err != nil {
		t.Fatal(err)
	}
	if got := persistedBackend(); got != backendFile {
		t.Fatalf("after explicit file write: persisted = %q, want file", got)
	}

	if err := (recordingStore{inner: inner, name: backendKeyring}).Set("svc", "alice", "tok2"); err != nil {
		t.Fatal(err)
	}
	if got := persistedBackend(); got != "" {
		t.Fatalf("after explicit keyring write: persisted = %q, want empty", got)
	}
}

func TestRecordingStore_ReadsAndDeletesRememberNothing(t *testing.T) {
	isolateConfigDir(t)
	inner := newScriptedStore()
	rec := recordingStore{inner: inner, name: backendFile}
	if _, err := rec.Get("svc", "alice"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get = %v, want ErrNotFound passed through", err)
	}
	if err := rec.Delete("svc", "alice"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete = %v, want ErrNotFound passed through", err)
	}
	if got := persistedBackend(); got != "" {
		t.Fatalf("persisted = %q after a read and a delete, want empty: only writes remember", got)
	}
	if inner.gets != 1 || inner.dels != 1 {
		t.Fatalf("gets=%d dels=%d, want 1 and 1", inner.gets, inner.dels)
	}
}

func TestRecordingStore_FailedSetRemembersNothing(t *testing.T) {
	isolateConfigDir(t)
	inner := newScriptedStore()
	inner.setErr = errors.New("boom")
	err := (recordingStore{inner: inner, name: backendFile}).Set("svc", "alice", "tok")
	if err == nil || err.Error() != "boom" {
		t.Fatalf("err = %v, want the inner error unchanged", err)
	}
	if got := persistedBackend(); got != "" {
		t.Fatalf("persisted = %q after a failed write, want empty", got)
	}
}

func TestFileBackendSelected_HonorsEnvThenMarker(t *testing.T) {
	isolateConfigDir(t)
	t.Setenv(BackendEnvVar, "")
	if FileBackendSelected() {
		t.Fatal("nothing selected: want false")
	}
	if err := rememberBackend(backendFile); err != nil {
		t.Fatal(err)
	}
	if !FileBackendSelected() {
		t.Fatal("marker says file: want true")
	}
	t.Setenv(BackendEnvVar, backendKeyring)
	if FileBackendSelected() {
		t.Fatal("explicit keyring env must beat the marker")
	}
	t.Setenv(BackendEnvVar, backendFile)
	if !FileBackendSelected() {
		t.Fatal("explicit file env: want true")
	}
}

// A developer's shell may export ENTIRE_TOKEN_STORE=keyring; under `go test`
// that must not route a test's writes to the real OS keyring. The pure
// resolver honours the explicit value (pinned by
// TestResolveBackend_ExplicitKeyringEnvIsARecordingKeyringStore); the
// gathering step drops it when a test process is detected.
func TestResolveBackendLocked_IgnoresExplicitKeyringUnderTest(t *testing.T) {
	isolateConfigDir(t)
	t.Setenv(BackendEnvVar, backendKeyring)
	resetBackendForTesting(t)
	fs, ok := currentBackend().(*fileStore)
	if !ok {
		t.Fatalf("currentBackend() = %T under go test with an explicit keyring env, want the testdirs *fileStore", currentBackend())
	}
	if fs.path == FileBackendPath() {
		t.Fatalf("path = %q is the default file store; want the testdirs store, since no marker was written", fs.path)
	}
}

// Under go test the testdirs store is itself a *fileStore, so the type alone
// proves nothing; the PATH is what tells a marker-resolved store (the default
// path in the isolated config dir) from the testdirs one. isolateConfigDir
// blanks PathEnvVar, which is what lets rememberBackend write the marker.
func TestCurrentBackend_ReadsMarkerWhenEnvUnset(t *testing.T) {
	isolateConfigDir(t)
	t.Setenv(BackendEnvVar, "")
	resetBackendForTesting(t)
	if err := rememberBackend(backendFile); err != nil {
		t.Fatal(err)
	}
	fs, ok := currentBackend().(*fileStore)
	if !ok {
		t.Fatalf("currentBackend() = %T, want *fileStore from the marker", currentBackend())
	}
	if fs.path != FileBackendPath() {
		t.Fatalf("path = %q, want the default %q (the marker, not the testdirs store)", fs.path, FileBackendPath())
	}
}
