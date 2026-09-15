package tokenstore

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/entireio/cli/internal/entireclient/userdirs"
)

// The marker lives next to contexts.json in the per-user config dir. Every
// test here isolates that dir: the marker is process-visible state and the
// testdirs fallback dir is shared by every test in the process. It also
// blanks ENTIRE_TOKEN_STORE_PATH, which a developer shell may export and
// which stops the marker from being written (see rememberBackend).
func isolateConfigDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(userdirs.EnvConfigDir, dir)
	t.Setenv(PathEnvVar, "")
	return dir
}

// captureNotices redirects the package's stderr notices to a buffer for the
// test and installs a fresh warn-once so the test sees its own warning.
func captureNotices(t *testing.T) *bytes.Buffer {
	t.Helper()
	var out bytes.Buffer
	prevW, prevOnce := fallbackNoticeW, markerWarnOnce
	fallbackNoticeW, markerWarnOnce = &out, new(sync.Once)
	t.Cleanup(func() { fallbackNoticeW, markerWarnOnce = prevW, prevOnce })
	return &out
}

func TestPersistedBackend_WarnsOnceAboutAnUnusableMarker(t *testing.T) {
	dir := isolateConfigDir(t)
	out := captureNotices(t)
	if err := os.WriteFile(filepath.Join(dir, preferenceFileName), []byte("{ nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if got := persistedBackend(); got != "" {
			t.Fatalf("persistedBackend() = %q for corrupt JSON, want empty", got)
		}
	}
	if n := strings.Count(out.String(), "Warning: ignoring unusable token store preference"); n != 1 {
		t.Fatalf("warned %d times, want once:\n%s", n, out.String())
	}
	if !strings.Contains(out.String(), preferenceFileName) {
		t.Fatalf("warning should name the marker file:\n%s", out.String())
	}
}

func TestPersistedBackend_AbsentMarkerIsSilent(t *testing.T) {
	isolateConfigDir(t)
	out := captureNotices(t)
	if got := persistedBackend(); got != "" {
		t.Fatalf("persistedBackend() = %q, want empty", got)
	}
	if out.Len() != 0 {
		t.Fatalf("an absent marker must not warn:\n%s", out.String())
	}
}

func TestPersistedBackend_UnsetWhenNoMarker(t *testing.T) {
	isolateConfigDir(t)
	if got := persistedBackend(); got != "" {
		t.Fatalf("persistedBackend() = %q, want empty with no marker", got)
	}
}

func TestRememberBackend_FileWritesMarkerAndKeyringClearsIt(t *testing.T) {
	dir := isolateConfigDir(t)

	if err := rememberBackend(backendFile); err != nil {
		t.Fatalf("rememberBackend(file): %v", err)
	}
	if got := persistedBackend(); got != backendFile {
		t.Fatalf("persistedBackend() = %q after remembering file, want %q", got, backendFile)
	}
	info, err := os.Stat(filepath.Join(dir, preferenceFileName))
	if err != nil {
		t.Fatalf("marker not written: %v", err)
	}
	// Windows reports synthetic modes, so the permission check is unix-only,
	// matching TestFileStore_FilePermissions.
	if perm := info.Mode().Perm(); runtime.GOOS != goosWindows && perm&0o077 != 0 {
		t.Fatalf("marker mode = %04o, want owner-only", perm)
	}
	if data, err := os.ReadFile(filepath.Join(dir, preferenceFileName)); err != nil || string(data) != `{"backend":"file"}` {
		t.Fatalf("marker bytes = (%q, %v), want the documented on-disk form", data, err)
	}

	if err := rememberBackend(backendKeyring); err != nil {
		t.Fatalf("rememberBackend(keyring): %v", err)
	}
	if got := persistedBackend(); got != "" {
		t.Fatalf("persistedBackend() = %q after remembering keyring, want empty", got)
	}
	if _, err := os.Stat(filepath.Join(dir, preferenceFileName)); !os.IsNotExist(err) {
		t.Fatalf("marker should be removed, stat err = %v", err)
	}
}

// Clearing when there is no config dir at all exercises the fs.ErrNotExist
// branch: nothing to remove, and the directory must not be created just to
// report that.
func TestRememberBackend_KeyringWithNoConfigDirIsANoOp(t *testing.T) {
	dir := isolateConfigDir(t)
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := rememberBackend(backendKeyring); err != nil {
		t.Fatalf("rememberBackend(keyring) with no config dir: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("clearing must not create the config dir, stat err = %v", err)
	}
}

// The write path creates a missing config dir, and creates it private: the
// marker steers where bearer tokens go, so it lives behind the same 0700 as
// contexts.json. The clear path (tested above) must NOT create it.
func TestRememberBackend_FileCreatesAPrivateConfigDir(t *testing.T) {
	dir := isolateConfigDir(t)
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := rememberBackend(backendFile); err != nil {
		t.Fatalf("rememberBackend(file) with no config dir: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("config dir should have been created: %v", err)
	}
	if perm := info.Mode().Perm(); runtime.GOOS != goosWindows && perm&0o077 != 0 {
		t.Fatalf("config dir mode = %04o, want owner-only", perm)
	}
	if got := persistedBackend(); got != backendFile {
		t.Fatalf("persistedBackend() = %q, want %q", got, backendFile)
	}
}

// An explicit ENTIRE_TOKEN_STORE_PATH is environment configuration: a marker
// saying only "file" would send a later env-less process to the DEFAULT path,
// which does not hold the token. So nothing is remembered while it is set.
func TestRememberBackend_FileIsNotRememberedWhenPathIsOverridden(t *testing.T) {
	dir := isolateConfigDir(t)
	t.Setenv(PathEnvVar, filepath.Join(t.TempDir(), "tokens.json"))
	if err := rememberBackend(backendFile); err != nil {
		t.Fatalf("rememberBackend(file) with a path override: %v", err)
	}
	if got := persistedBackend(); got != "" {
		t.Fatalf("persistedBackend() = %q, want empty while %s is set", got, PathEnvVar)
	}
	if _, err := os.Stat(filepath.Join(dir, preferenceFileName)); !os.IsNotExist(err) {
		t.Fatalf("marker must not be written, stat err = %v", err)
	}
}

func TestRememberBackend_FileIsIdempotent(t *testing.T) {
	isolateConfigDir(t)
	for i := range 2 {
		if err := rememberBackend(backendFile); err != nil {
			t.Fatalf("call %d: %v", i+1, err)
		}
	}
	if got := persistedBackend(); got != backendFile {
		t.Fatalf("persistedBackend() = %q, want %q", got, backendFile)
	}
}

func TestPersistedBackend_CorruptOrUnknownMarkerIsUnset(t *testing.T) {
	dir := isolateConfigDir(t)
	captureNotices(t)
	for name, body := range map[string]string{
		"not json":        "{ nope",
		"unknown backend": `{"backend":"vault"}`,
		"empty backend":   `{"backend":""}`,
		"keyring is the default, never a marker value": `{"backend":"keyring"}`,
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(filepath.Join(dir, preferenceFileName), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if got := persistedBackend(); got != "" {
				t.Fatalf("persistedBackend() = %q, want empty for %s", got, name)
			}
		})
	}
}

func TestRememberBackend_RejectsUnknownName(t *testing.T) {
	isolateConfigDir(t)
	if err := rememberBackend("vault"); err == nil {
		t.Fatal("rememberBackend(vault) should error")
	}
}

// A planted symlink at the marker's name is refused on read and replaced on
// write; nothing follows it to its target. The target holds valid JSON that
// would read as "file" if followed, with a trailing newline that a
// write-through would erase, so both halves are observable. It sits INSIDE
// the config dir on purpose: a link escaping the root is refused by os.Root
// on its own, so the read half would pass even with a following read. An
// in-root target is what ReadFileNoFollow exists to refuse, and it is the
// realistic case — an attacker planting the link is writing into that
// directory anyway.
func TestMarker_SymlinkIsNeitherFollowedNorWrittenThrough(t *testing.T) {
	if runtime.GOOS == goosWindows {
		t.Skip("creating symlinks needs a privilege the Windows runner may lack")
	}
	dir := isolateConfigDir(t)
	captureNotices(t)
	planted := []byte("{\"backend\":\"file\"}\n")
	target := filepath.Join(dir, "inside.json")
	if err := os.WriteFile(target, planted, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, preferenceFileName)
	if err := os.Symlink("inside.json", link); err != nil {
		t.Fatal(err)
	}

	if got := persistedBackend(); got != "" {
		t.Fatalf("persistedBackend() = %q through a symlink, want empty (link refused)", got)
	}

	if err := rememberBackend(backendFile); err != nil {
		t.Fatalf("rememberBackend(file) over a symlink: %v", err)
	}
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Fatal("the write should have replaced the symlink with a regular file")
	}
	if data, err := os.ReadFile(target); err != nil || string(data) != string(planted) {
		t.Fatalf("symlink target must be untouched, got (%q, %v)", data, err)
	}
	if got := persistedBackend(); got != backendFile {
		t.Fatalf("persistedBackend() = %q after replacing the link, want %q", got, backendFile)
	}
}
