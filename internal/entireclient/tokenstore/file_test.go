package tokenstore

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func newTestStore(t *testing.T) *fileStore {
	t.Helper()
	return &fileStore{path: filepath.Join(t.TempDir(), "tokens.json")}
}

func TestFileStore_GetMissingFile(t *testing.T) {
	s := newTestStore(t)
	_, err := s.Get("svc", "user")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestFileStore_SetAndGet(t *testing.T) {
	s := newTestStore(t)

	if err := s.Set("svc", "alice", "secret"); err != nil {
		t.Fatal(err)
	}

	got, err := s.Get("svc", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if got != "secret" {
		t.Fatalf("got %q, want %q", got, "secret")
	}
}

func TestFileStore_GetWrongService(t *testing.T) {
	s := newTestStore(t)

	if err := s.Set("svc", "alice", "secret"); err != nil {
		t.Fatal(err)
	}

	_, err := s.Get("other", "alice")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestFileStore_GetWrongUser(t *testing.T) {
	s := newTestStore(t)

	if err := s.Set("svc", "alice", "secret"); err != nil {
		t.Fatal(err)
	}

	_, err := s.Get("svc", "bob")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestFileStore_Overwrite(t *testing.T) {
	s := newTestStore(t)

	if err := s.Set("svc", "alice", "old"); err != nil {
		t.Fatal(err)
	}
	if err := s.Set("svc", "alice", "new"); err != nil {
		t.Fatal(err)
	}

	got, err := s.Get("svc", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if got != "new" {
		t.Fatalf("got %q, want %q", got, "new")
	}
}

func TestFileStore_MultipleServices(t *testing.T) {
	s := newTestStore(t)

	if err := s.Set("svc1", "alice", "pw1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Set("svc2", "bob", "pw2"); err != nil {
		t.Fatal(err)
	}

	got, err := s.Get("svc1", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if got != "pw1" {
		t.Fatalf("got %q, want %q", got, "pw1")
	}

	got, err = s.Get("svc2", "bob")
	if err != nil {
		t.Fatal(err)
	}
	if got != "pw2" {
		t.Fatalf("got %q, want %q", got, "pw2")
	}
}

func TestFileStore_Delete(t *testing.T) {
	s := newTestStore(t)

	if err := s.Set("svc", "alice", "secret"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete("svc", "alice"); err != nil {
		t.Fatal(err)
	}

	_, err := s.Get("svc", "alice")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestFileStore_DeleteCleansEmptyService(t *testing.T) {
	s := newTestStore(t)

	if err := s.Set("svc", "alice", "secret"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete("svc", "alice"); err != nil {
		t.Fatal(err)
	}

	// Re-read the file to confirm the service key is gone entirely.
	store, err := s.load()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := store["svc"]; ok {
		t.Fatal("expected service key to be removed when last user is deleted")
	}
}

func TestFileStore_DeletePreservesOtherUsers(t *testing.T) {
	s := newTestStore(t)

	if err := s.Set("svc", "alice", "pw1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Set("svc", "bob", "pw2"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete("svc", "alice"); err != nil {
		t.Fatal(err)
	}

	got, err := s.Get("svc", "bob")
	if err != nil {
		t.Fatal(err)
	}
	if got != "pw2" {
		t.Fatalf("got %q, want %q", got, "pw2")
	}
}

func TestFileStore_DeleteNotFound(t *testing.T) {
	s := newTestStore(t)

	err := s.Delete("svc", "alice")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestFileStore_DeleteNotFoundUser(t *testing.T) {
	s := newTestStore(t)

	if err := s.Set("svc", "alice", "secret"); err != nil {
		t.Fatal(err)
	}

	err := s.Delete("svc", "bob")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestFileStore_LoadCorruptFile(t *testing.T) {
	s := newTestStore(t)

	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.path, []byte("not json"), 0600); err != nil {
		t.Fatal(err)
	}

	_, err := s.Get("svc", "user")
	if err == nil {
		t.Fatal("expected error for corrupt file")
	}
}

func TestFileStore_CreatesDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "dir")
	s := &fileStore{path: filepath.Join(dir, "tokens.json")}

	if err := s.Set("svc", "alice", "secret"); err != nil {
		t.Fatal(err)
	}

	got, err := s.Get("svc", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if got != "secret" {
		t.Fatalf("got %q, want %q", got, "secret")
	}
}

func TestFileStore_FilePermissions(t *testing.T) {
	s := newTestStore(t)

	if err := s.Set("svc", "alice", "secret"); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(s.path)
	if err != nil {
		t.Fatal(err)
	}
	perm := info.Mode().Perm()
	if perm != 0600 {
		t.Fatalf("file permissions = %o, want 0600", perm)
	}
}

// looseStoreFile creates a store file and then chmods it explicitly —
// os.WriteFile's mode is masked by the process umask (a hardened umask like
// 077 would silently produce 0600), while chmod is not.
func looseStoreFile(t *testing.T, s *fileStore, perm os.FileMode) {
	t.Helper()
	if err := os.WriteFile(s.path, []byte(`{"svc":{"alice":"tokval"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(s.path, perm); err != nil {
		t.Fatal(err)
	}
}

// captureLoosePermsWarnings redirects the loose-permissions warning writer
// to a buffer for the duration of the test. Tests using it must not be
// parallel (package-global writer).
func captureLoosePermsWarnings(t *testing.T) *strings.Builder {
	t.Helper()
	var buf strings.Builder
	prev := loosePermsWarnW
	loosePermsWarnW = &buf
	t.Cleanup(func() { loosePermsWarnW = prev })
	return &buf
}

// The store file holds bearer tokens: a group/other-accessible file draws a
// warning naming the file and the chmod remediation, but operations still
// work. Deliberately not a refusal — externally provisioned files (CI secret
// mounts, read-only volumes) can carry modes the user cannot change, and a
// hard failure would also block the login rewrite that restores 0600.
func TestFileStore_WarnsOnLoosePermissionsButWorks(t *testing.T) {
	if runtime.GOOS == goosWindows {
		t.Skip("unix permission semantics")
	}
	for name, perm := range map[string]os.FileMode{
		"group-readable": 0o640,
		"world-readable": 0o604,
		"group-writable": 0o620,
	} {
		t.Run(name, func(t *testing.T) {
			s := newTestStore(t)
			looseStoreFile(t, s, perm)
			warnings := captureLoosePermsWarnings(t)

			got, err := s.Get("svc", "alice")
			if err != nil {
				t.Fatalf("Get on a %s file must still work, got error: %v", name, err)
			}
			if got != "tokval" {
				t.Fatalf("Get = %q, want %q", got, "tokval")
			}
			warned := warnings.String()
			if !strings.Contains(warned, "chmod 0600") || !strings.Contains(warned, s.path) {
				t.Fatalf("warning should name the file and the chmod remediation, got: %q", warned)
			}
		})
	}
}

// Set must also warn on (and still repair) a loose file: login's rewrite is
// exactly how a loose store gets restored to 0600.
func TestFileStore_SetOnLooseFileWarnsAndRestores0600(t *testing.T) {
	if runtime.GOOS == goosWindows {
		t.Skip("unix permission semantics")
	}
	s := newTestStore(t)
	looseStoreFile(t, s, 0o644)
	warnings := captureLoosePermsWarnings(t)

	if err := s.Set("svc", "alice", "fresh"); err != nil {
		t.Fatalf("Set on a loose file must still work, got: %v", err)
	}
	if !strings.Contains(warnings.String(), "chmod 0600") {
		t.Fatalf("Set should emit the loose-permissions warning, got: %q", warnings.String())
	}
	info, err := os.Stat(s.path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("save must restore 0600 on rewrite, got %04o", perm)
	}
}

// The warning is emitted once per store instance, not once per operation.
func TestFileStore_LoosePermissionWarningIsDeduped(t *testing.T) {
	if runtime.GOOS == goosWindows {
		t.Skip("unix permission semantics")
	}
	s := newTestStore(t)
	looseStoreFile(t, s, 0o640)
	warnings := captureLoosePermsWarnings(t)

	for range 3 {
		if _, err := s.Get("svc", "alice"); err != nil {
			t.Fatal(err)
		}
	}
	if n := strings.Count(warnings.String(), "chmod 0600"); n != 1 {
		t.Fatalf("warning should be emitted once, got %d occurrences:\n%s", n, warnings.String())
	}
}

// A correctly-permissioned file draws no warning.
func TestFileStore_Reads0600FileWithoutWarning(t *testing.T) {
	s := newTestStore(t)
	if err := os.WriteFile(s.path, []byte(`{"svc":{"alice":"tokval"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(s.path, 0o600); err != nil {
		t.Fatal(err)
	}
	warnings := captureLoosePermsWarnings(t)
	got, err := s.Get("svc", "alice")
	if err != nil {
		t.Fatalf("Get on a 0600 file should succeed, got: %v", err)
	}
	if got != "tokval" {
		t.Fatalf("Get = %q, want %q", got, "tokval")
	}
	if warnings.Len() != 0 {
		t.Fatalf("no warning expected for a 0600 file, got: %q", warnings.String())
	}
}

// BackendDescription pins: user-facing provenance wording must track the env
// the way resolveBackendLocked does. Not parallel: t.Setenv.
func TestBackendDescription_Keyring(t *testing.T) {
	t.Setenv(BackendEnvVar, "")
	got := BackendDescription()
	if got != keyringProviderName() {
		t.Fatalf("BackendDescription() = %q, want the per-OS keyring name %q", got, keyringProviderName())
	}
	if strings.HasPrefix(got, "file ") {
		t.Fatalf("BackendDescription() = %q, must not claim the file backend when env is unset", got)
	}
}

func TestBackendDescription_FileWithExplicitPath(t *testing.T) {
	t.Setenv(BackendEnvVar, "file")
	t.Setenv(PathEnvVar, "/ci/secrets/tokens.json")
	if got := BackendDescription(); got != "file /ci/secrets/tokens.json" {
		t.Fatalf("BackendDescription() = %q, want %q", got, "file /ci/secrets/tokens.json")
	}
}

// The default file location is tokens.json in the per-user config dir — this
// is production routing (resolveBackendLocked uses the same helper), so a
// typo'd default would relocate real users' token files.
func TestFileBackendPath_DefaultsToConfigDirTokensJSON(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv(PathEnvVar, "")
	t.Setenv("ENTIRE_CONFIG_DIR", cfgDir)
	want := filepath.Join(cfgDir, "tokens.json")
	if got := FileBackendPath(); got != want {
		t.Fatalf("FileBackendPath() = %q, want %q", got, want)
	}
}

// The warning's production destination is stderr. Pinned because every other
// warning test swaps the writer via captureLoosePermsWarnings — without this,
// changing the default to io.Discard would silently delete the feature in
// production while the whole suite stays green (verified by mutation).
// Not parallel: reads the package-global writer that other tests swap.
func TestLoosePermsWarnWriter_DefaultsToStderr(t *testing.T) {
	if loosePermsWarnW != os.Stderr {
		t.Fatalf("loosePermsWarnW default = %T, want os.Stderr", loosePermsWarnW)
	}
}
