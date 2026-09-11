package tokenstore

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
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
//
// Pinned by descriptor rather than pointer equality with os.Stderr: under
// `go test -json` (Go 1.26) the testing package replaces the os.Stderr
// variable after package init to attribute output to tests, so the default
// captured at init no longer compares equal even though it is the process's
// real stderr. io.Discard or a buffer still fails this (not an *os.File).
func TestLoosePermsWarnWriter_DefaultsToStderr(t *testing.T) {
	f, ok := loosePermsWarnW.(*os.File)
	if !ok || f.Fd() != uintptr(syscall.Stderr) {
		t.Fatalf("loosePermsWarnW default = %T, want the process stderr", loosePermsWarnW)
	}
}

// A path the user pointed us at with ENTIRE_TOKEN_STORE_PATH names a
// directory Entire did not choose — a CI secret mount, a shared secrets
// directory, or $HOME itself. Tightening it is a side effect nobody asked
// for, and on a mount the caller doesn't own the chmod fails and takes every
// Get/Set/Delete with it.
func TestFileStore_LeavesUnownedDirectoryModeAlone(t *testing.T) {
	if runtime.GOOS == goosWindows {
		t.Skip("unix permission bits are synthetic on Windows")
	}
	t.Parallel()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	s := &fileStore{path: filepath.Join(dir, "tokens.json")}

	if err := s.Set("svc", "alice", "secret"); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o755 {
		t.Errorf("directory mode = %04o, want 0755 left as the user set it", got)
	}
}

// The default location is Entire's own config directory, shared with
// contexts.json, and a mode-0755 one left behind by an early version check is
// exactly what EnsurePrivateDir exists to repair.
func TestFileStore_TightensOwnedDirectory(t *testing.T) {
	if runtime.GOOS == goosWindows {
		t.Skip("unix permission bits are synthetic on Windows")
	}
	t.Parallel()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	s := &fileStore{path: filepath.Join(dir, "tokens.json"), ownsDir: true}

	if err := s.Set("svc", "alice", "secret"); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got&0o077 != 0 {
		t.Errorf("directory mode = %04o, still accessible by group/other", got)
	}
}

func TestResolveBackend_OwnsDirOnlyForTheDefaultPath(t *testing.T) {
	t.Setenv(BackendEnvVar, "file")

	t.Setenv(PathEnvVar, "")
	def, ok := resolveBackendLocked().(*fileStore)
	if !ok {
		t.Fatalf("default path: got %T, want *fileStore", resolveBackendLocked())
	}
	if !def.ownsDir {
		t.Error("default path: ownsDir = false, want true")
	}

	t.Setenv(PathEnvVar, filepath.Join(t.TempDir(), "tokens.json"))
	custom, ok := resolveBackendLocked().(*fileStore)
	if !ok {
		t.Fatalf("custom path: got %T, want *fileStore", resolveBackendLocked())
	}
	if custom.ownsDir {
		t.Error("custom path: ownsDir = true, want false")
	}
}

// The token store is the fourth consumer of userdirs.Config(), and the one with
// no root to catch a rejected override: fileStore.dir anchors on the dirname of
// its own path (permitted, since ENTIRE_TOKEN_STORE_PATH names a file the caller
// chose) and reaches it through filepath.Abs, which launders a relative
// ENTIRE_CONFIG_DIR into a plausible-looking path. Left unchecked, bearer tokens
// landed at ./<value>/tokens.json — for a CLI run from a repository, inside the
// repository.
func TestFileBackendPath_RejectsRelativeConfigDirWithoutTouchingDisk(t *testing.T) {
	t.Setenv(PathEnvVar, "")
	t.Setenv("ENTIRE_CONFIG_DIR", "relative-config")
	t.Chdir(t.TempDir())

	path, err := fileBackendPathChecked()
	if err == nil {
		t.Fatalf("fileBackendPathChecked() = %q, nil; want a rejected override", path)
	}
	if !strings.Contains(err.Error(), "ENTIRE_CONFIG_DIR") {
		t.Errorf("error = %q, want it to name the variable the user has to change", err)
	}

	// The carried error must stop every operation before it creates anything.
	s := &fileStore{path: path, pathErr: err, ownsDir: true}
	if _, getErr := s.Get("svc", "user"); getErr == nil {
		t.Error("Get() succeeded against a rejected config dir")
	}
	if setErr := s.Set("svc", "user", "secret"); setErr == nil {
		t.Error("Set() succeeded against a rejected config dir")
	}
	if _, statErr := os.Stat("relative-config"); statErr == nil {
		t.Error("the store created the directory it was refusing")
	}
}

// An explicit ENTIRE_TOKEN_STORE_PATH is deliberately not held to the rule: it
// names a file the user chose, the same reasoning that exempts it from the
// root-base rule in CLAUDE.md.
func TestFileBackendPath_ExplicitPathIsNotHeldToTheAbsoluteRule(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", "relative-config")
	t.Setenv(PathEnvVar, "relative-tokens.json")

	path, err := fileBackendPathChecked()
	if err != nil {
		t.Fatalf("fileBackendPathChecked() error = %v, want the caller's own path honored", err)
	}
	if path != "relative-tokens.json" {
		t.Errorf("path = %q, want the value as given", path)
	}
}

// probeSecret is the value the relative-config-dir tests round-trip.
const probeSecret = "secret"

// resetBackendForTesting forces the next Get/Set/Delete to re-resolve the
// backend from the environment, which is what a fresh process does. The
// package-level backend is memoized, so without this a test inherits whatever
// an earlier one resolved.
func resetBackendForTesting(t *testing.T) {
	t.Helper()
	reset := func() {
		backendMu.Lock()
		defer backendMu.Unlock()
		backend = nil
		resolved = false
	}
	reset()
	t.Cleanup(reset)
}

// The check has to bite through the PUBLIC surface, not just the internal
// resolver: Get/Set/Delete are what a command calls, and each of them creates
// the directory, then a .lock, then the token file.
//
// The token store does open an os.Root — but on filepath.Dir of its own
// absolutized path, one of the two bases CLAUDE.md permits to be derived, since
// ENTIRE_TOKEN_STORE_PATH names a file the caller chose. That filepath.Abs is
// exactly what stops the root from ever refusing a relative config dir: it
// launders ./relative-config into a plausible absolute path first. Hence the
// explicit check rather than relying on the root.
func TestFileStore_PublicOpsRejectRelativeConfigDirWithoutSideEffects(t *testing.T) {
	t.Setenv("ENTIRE_TOKEN_STORE", "file")
	t.Setenv(PathEnvVar, "")
	t.Setenv("ENTIRE_CONFIG_DIR", "relative-config")
	cwd := t.TempDir()
	t.Chdir(cwd)
	resetBackendForTesting(t)

	if err := Set("svc", "user", probeSecret); err == nil {
		t.Error("Set() succeeded with a relative ENTIRE_CONFIG_DIR")
	}
	if _, err := Get("svc", "user"); err == nil {
		t.Error("Get() succeeded with a relative ENTIRE_CONFIG_DIR")
	}
	if err := Delete("svc", "user"); err == nil {
		t.Error("Delete() succeeded with a relative ENTIRE_CONFIG_DIR")
	}

	entries, readErr := os.ReadDir(cwd)
	if readErr != nil {
		t.Fatal(readErr)
	}
	for _, e := range entries {
		t.Errorf("left %q behind in the working directory; bearer tokens must not land under the cwd", e.Name())
	}
}

// An explicit ENTIRE_TOKEN_STORE_PATH is unchanged by the above: the user named
// that file, which is the same reasoning that exempts it from the root-base
// rule. A relative ENTIRE_CONFIG_DIR alongside it is simply irrelevant.
func TestFileStore_ExplicitPathKeepsWorkingWithARelativeConfigDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ENTIRE_TOKEN_STORE", "file")
	t.Setenv("ENTIRE_CONFIG_DIR", "relative-config")
	t.Setenv(PathEnvVar, filepath.Join(dir, "tokens.json"))
	resetBackendForTesting(t)

	if err := Set("svc", "user", probeSecret); err != nil {
		t.Fatalf("Set() with an explicit path = %v", err)
	}
	got, err := Get("svc", "user")
	if err != nil || got != probeSecret {
		t.Fatalf("Get() = %q, %v; want the explicitly named file honored", got, err)
	}
}
