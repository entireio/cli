package tokenstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeKeyring stands in for the OS keyring. err is returned by every
// operation when set; calls counts what actually reached it, which is how the
// process-local unusable latch is observed.
type fakeKeyring struct {
	err    error
	values map[string]string
	calls  int
}

func (f *fakeKeyring) key(service, user string) string { return service + "\x00" + user }

func (f *fakeKeyring) Get(service, user string) (string, error) {
	f.calls++
	if f.err != nil {
		return "", f.err
	}
	if v, ok := f.values[f.key(service, user)]; ok {
		return v, nil
	}
	return "", ErrNotFound
}

func (f *fakeKeyring) Set(service, user, password string) error {
	f.calls++
	if f.err != nil {
		return f.err
	}
	if f.values == nil {
		f.values = make(map[string]string)
	}
	f.values[f.key(service, user)] = password
	return nil
}

func (f *fakeKeyring) Delete(service, user string) error {
	f.calls++
	if f.err != nil {
		return f.err
	}
	if _, ok := f.values[f.key(service, user)]; !ok {
		return ErrNotFound
	}
	delete(f.values, f.key(service, user))
	return nil
}

// newFallbackStoreForTest wires a fallbackStore over a fake keyring and a
// per-test file, and captures the one-time fallback warning. It resets the
// process-wide fallback marker, so these tests cannot run in parallel with
// anything reading it (FellBackToFileStore, BackendDescription).
func newFallbackStoreForTest(t *testing.T, keyringErr error) (*fallbackStore, *fakeKeyring, *bytes.Buffer) {
	t.Helper()

	kr := &fakeKeyring{err: keyringErr}
	s := &fallbackStore{
		keyring: kr,
		file:    &fileStore{path: filepath.Join(t.TempDir(), "tokens.json")},
	}

	var warn bytes.Buffer
	prevWarn := fallbackWarnW
	fallbackWarnW = &warn
	resetFileFallbackForTesting()
	t.Cleanup(func() {
		fallbackWarnW = prevWarn
		resetFileFallbackForTesting()
	})

	return s, kr, &warn
}

// errKeyringless is the shape of the real failure on a machine with no Secret
// Service: the provider's helper binary is simply absent.
var errKeyringless = errors.New(`exec: "dbus-launch": executable file not found in $PATH`)

// A login is completed before its tokens are written, so a keyring-less
// machine must not lose it: the write lands in the 0600 file store, is
// readable afterwards, and the user is told once.
func TestFallbackStore_SetFallsBackToFileAndStaysReadable(t *testing.T) {
	s, kr, warn := newFallbackStoreForTest(t, errKeyringless)

	if err := s.Set("entire-core:https://auth.test", "alice", "token-1"); err != nil {
		t.Fatalf("Set() error = %v, want nil", err)
	}

	got, err := s.Get("entire-core:https://auth.test", "alice")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got != "token-1" {
		t.Errorf("Get() = %q, want %q", got, "token-1")
	}

	path, ok := FellBackToFileStore()
	if !ok || path != s.file.path {
		t.Errorf("FellBackToFileStore() = (%q, %v), want (%q, true)", path, ok, s.file.path)
	}
	for _, want := range []string{keyringProviderName(), s.file.path, "dbus-launch", BackendEnvVar + "=file", BackendEnvVar + "=keyring"} {
		if !strings.Contains(warn.String(), want) {
			t.Errorf("warning missing %q:\n%s", want, warn.String())
		}
	}
	// The latch: one failed call is enough to stop paying for the keyring,
	// which can cost keyringTimeout per call when a provider hangs.
	if kr.calls != 1 {
		t.Errorf("keyring calls = %d, want 1 (unusable latch not held)", kr.calls)
	}
}

// The warning is a once-per-process notice, not per credential — a login
// writes both a refresh and an access token.
func TestFallbackStore_WarnsOncePerProcess(t *testing.T) {
	s, _, warn := newFallbackStoreForTest(t, errKeyringless)

	for _, service := range []string{"entire-core:https://auth.test:refresh", "entire-core:https://auth.test"} {
		if err := s.Set(service, "alice", "token"); err != nil {
			t.Fatalf("Set(%q) error = %v", service, err)
		}
	}

	if got := strings.Count(warn.String(), "Credentials saved to"); got != 1 {
		t.Errorf("fallback warning printed %d times, want 1:\n%s", got, warn.String())
	}
}

// A healthy keyring is always preferred, and nothing is written to disk.
func TestFallbackStore_PrefersHealthyKeyring(t *testing.T) {
	s, kr, warn := newFallbackStoreForTest(t, nil)

	if err := s.Set("svc", "alice", "token-1"); err != nil {
		t.Fatalf("Set() error = %v", err)
	}
	if got := kr.values[kr.key("svc", "alice")]; got != "token-1" {
		t.Errorf("keyring holds %q, want %q", got, "token-1")
	}
	if value, err := s.file.Get("svc", "alice"); !errors.Is(err, ErrNotFound) {
		t.Errorf("file store holds %q (err %v); a working keyring must not spill tokens to disk", value, err)
	}
	if _, ok := FellBackToFileStore(); ok {
		t.Error("FellBackToFileStore() = true with a healthy keyring")
	}
	if warn.Len() != 0 {
		t.Errorf("unexpected warning:\n%s", warn.String())
	}
}

// A keyring miss is not a keyring failure: the entry may still be in the file
// store from an earlier fallback, and the keyring stays usable.
func TestFallbackStore_KeyringMissConsultsFile(t *testing.T) {
	s, kr, _ := newFallbackStoreForTest(t, nil)

	if err := s.file.Set("svc", "alice", "from-file"); err != nil {
		t.Fatalf("seed file store: %v", err)
	}

	got, err := s.Get("svc", "alice")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got != "from-file" {
		t.Errorf("Get() = %q, want %q", got, "from-file")
	}
	if !s.keyringUsable() {
		t.Error("a keyring miss marked the keyring unusable")
	}
	if kr.calls != 1 {
		t.Errorf("keyring calls = %d, want 1", kr.calls)
	}
}

// A broken keyring with nothing in the file must surface the keyring error, so
// a keyring problem never reads as "not logged in".
func TestFallbackStore_GetSurfacesKeyringFailureOverNotFound(t *testing.T) {
	s, _, _ := newFallbackStoreForTest(t, errKeyringless)

	_, err := s.Get("svc", "alice")
	if !errors.Is(err, errKeyringless) {
		t.Fatalf("Get() error = %v, want the keyring failure", err)
	}
	if errors.Is(err, ErrNotFound) {
		t.Error("a broken keyring reported ErrNotFound")
	}
}

// Ctrl-C during a keyring call is the user aborting the command, not a broken
// keyring: it must propagate and must never be answered by writing a bearer
// token to disk.
func TestFallbackStore_InterruptDoesNotWriteToDisk(t *testing.T) {
	s, _, warn := newFallbackStoreForTest(t, fmt.Errorf("set interrupted: %w", context.Canceled))

	err := s.Set("svc", "alice", "token-1")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Set() error = %v, want context.Canceled", err)
	}
	if value, fileErr := s.file.Get("svc", "alice"); !errors.Is(fileErr, ErrNotFound) {
		t.Errorf("file store holds %q (err %v) after an interrupt", value, fileErr)
	}
	if _, ok := FellBackToFileStore(); ok {
		t.Error("FellBackToFileStore() = true after an interrupt")
	}
	if warn.Len() != 0 {
		t.Errorf("unexpected warning:\n%s", warn.String())
	}
	if !s.keyringUsable() {
		t.Error("an interrupt marked the keyring unusable")
	}
}

// When neither backend can take the write, the error says so — and marks
// itself so login stops suggesting the file store it just tried.
func TestFallbackStore_BothBackendsFailing(t *testing.T) {
	s, _, _ := newFallbackStoreForTest(t, errKeyringless)
	// A path whose parent is a file, so MkdirAll (and the write) must fail.
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0600); err != nil {
		t.Fatalf("seed blocker: %v", err)
	}
	s.file = &fileStore{path: filepath.Join(blocker, "tokens.json")}

	err := s.Set("svc", "alice", "token-1")
	if !errors.Is(err, ErrFileFallbackFailed) {
		t.Fatalf("Set() error = %v, want ErrFileFallbackFailed", err)
	}
	if !strings.Contains(err.Error(), "dbus-launch") {
		t.Errorf("error drops the keyring cause: %v", err)
	}
}

// Logout has to clear every copy: a fallback write may have left the
// credential in either backend.
func TestFallbackStore_DeleteClearsBothBackends(t *testing.T) {
	s, kr, _ := newFallbackStoreForTest(t, nil)

	if err := kr.Set("svc", "alice", "from-keyring"); err != nil {
		t.Fatalf("seed keyring: %v", err)
	}
	if err := s.file.Set("svc", "alice", "from-file"); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	if err := s.Delete("svc", "alice"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if _, err := kr.Get("svc", "alice"); !errors.Is(err, ErrNotFound) {
		t.Errorf("keyring copy survived delete: %v", err)
	}
	if _, err := s.file.Get("svc", "alice"); !errors.Is(err, ErrNotFound) {
		t.Errorf("file copy survived delete: %v", err)
	}
}

// Nothing anywhere still reports ErrNotFound, which callers (logout) match on.
func TestFallbackStore_DeleteMissingReportsNotFound(t *testing.T) {
	s, _, _ := newFallbackStoreForTest(t, nil)

	if err := s.Delete("svc", "alice"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Delete() error = %v, want ErrNotFound", err)
	}
}

// A broken keyring must not stop the delete from clearing the file copy.
func TestFallbackStore_DeleteSucceedsWithBrokenKeyring(t *testing.T) {
	s, _, _ := newFallbackStoreForTest(t, errKeyringless)

	if err := s.file.Set("svc", "alice", "from-file"); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	if err := s.Delete("svc", "alice"); err != nil {
		t.Fatalf("Delete() error = %v, want nil", err)
	}
	if _, err := s.file.Get("svc", "alice"); !errors.Is(err, ErrNotFound) {
		t.Errorf("file copy survived delete: %v", err)
	}
}

// Provenance follows the tokens: after a fallback, auth status must name the
// file, not the keyring that refused the write.
func TestBackendDescription_ReflectsFileFallback(t *testing.T) {
	s, _, _ := newFallbackStoreForTest(t, errKeyringless)
	t.Setenv(BackendEnvVar, "")

	if got := BackendDescription(); got != keyringProviderName() {
		t.Errorf("BackendDescription() = %q before any fallback, want %q", got, keyringProviderName())
	}
	if err := s.Set("svc", "alice", "token-1"); err != nil {
		t.Fatalf("Set() error = %v", err)
	}
	got := BackendDescription()
	if !strings.Contains(got, s.file.path) || !strings.Contains(got, keyringProviderName()) {
		t.Errorf("BackendDescription() = %q, want it to name %q and %q", got, s.file.path, keyringProviderName())
	}
}

func TestBackendSelectionEnvVar(t *testing.T) {
	cases := []struct {
		value       string
		wantFile    bool
		wantKeyring bool
	}{
		{value: "", wantFile: false, wantKeyring: false},
		{value: "file", wantFile: true, wantKeyring: false},
		{value: "keyring", wantFile: false, wantKeyring: true},
		{value: "nonsense", wantFile: false, wantKeyring: false},
	}
	for _, tc := range cases {
		t.Run("value="+tc.value, func(t *testing.T) {
			t.Setenv(BackendEnvVar, tc.value)
			if got := FileBackendSelected(); got != tc.wantFile {
				t.Errorf("FileBackendSelected() = %v, want %v", got, tc.wantFile)
			}
			if got := KeyringOnlySelected(); got != tc.wantKeyring {
				t.Errorf("KeyringOnlySelected() = %v, want %v", got, tc.wantKeyring)
			}
		})
	}
}
