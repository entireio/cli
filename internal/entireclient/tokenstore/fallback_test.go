package tokenstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/entireio/cli/internal/entireclient/userdirs"
)

// The D-Bus error a Secret-Service-less Linux session produces, verbatim,
// so the notice tests read like the customer report that motivated them.
var errNoSecretService = errors.New("The name org.freedesktop.secrets was not provided by any .service files") //nolint:staticcheck // verbatim D-Bus error text; ST1005's test exemption covers function bodies only

// newTestFallback builds a fallbackStore over a scripted primary and the
// default-path file store production would fall back to, in an isolated
// config dir, capturing the notice and the adopted store. Building the store
// with defaultFileStore pins that its path is the one the notice names.
func newTestFallback(t *testing.T, primary store) (*fallbackStore, *fileStore, *bytes.Buffer, *store) {
	t.Helper()
	isolateConfigDir(t) // also blanks PathEnvVar, so defaultFileStore lands in the isolated dir
	file := defaultFileStore()
	notice := captureNotices(t)
	var adopted store
	f := newFallbackStore(primary)
	f.newFile = func() store { return file }
	f.adopt = func(s store) { adopted = s }
	return f, file, notice, &adopted
}

func TestFallbackEligible(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		err  error
		want bool
	}{
		"nil":                  {nil, false},
		"not found":            {ErrNotFound, false},
		"wrapped not found":    {fmt.Errorf("read: %w", ErrNotFound), false},
		"ctrl-c":               {fmt.Errorf("get interrupted: %w", context.Canceled), false},
		"timeout":              {fmt.Errorf("get timed out: %w", context.DeadlineExceeded), true},
		"no secret service":    {errNoSecretService, true},
		"no session bus":       {errors.New("dbus: DBUS_SESSION_BUS_ADDRESS not set"), true},
		"locked collection":    {errors.New("failed to unlock correct collection"), true},
		"unsupported platform": {keyring.ErrUnsupportedPlatform, true},
	}
	for name, tc := range cases {
		if got := fallbackEligible(tc.err); got != tc.want {
			t.Errorf("%s: fallbackEligible(%v) = %v, want %v", name, tc.err, got, tc.want)
		}
	}
}

func TestIsSecretServicePlatform(t *testing.T) {
	t.Parallel()
	for goos, want := range map[string]bool{
		"linux": true, "freebsd": true, "openbsd": true, "netbsd": true, "dragonfly": true,
		"darwin": false, "windows": false, "plan9": false,
	} {
		if got := isSecretServicePlatform(goos); got != want {
			t.Errorf("isSecretServicePlatform(%s) = %v, want %v", goos, got, want)
		}
	}
}

func TestFallbackStore_GetAdoptsFileWhenKeyringUnavailable(t *testing.T) {
	primary := newScriptedStore()
	primary.getErr = errNoSecretService
	f, file, notice, adopted := newTestFallback(t, primary)
	if err := file.Set("svc", "alice", "tok"); err != nil {
		t.Fatal(err)
	}

	got, err := f.Get("svc", "alice")
	if err != nil || got != "tok" {
		t.Fatalf("Get = (%q, %v), want (tok, nil)", got, err)
	}
	if *adopted != store(file) {
		t.Fatalf("adopted = %v, want the file store", *adopted)
	}
	if persistedBackend() != backendFile {
		t.Fatal("marker not written after adopting the file store")
	}
	out := notice.String()
	for _, want := range []string{"OS keyring", errNoSecretService.Error(), file.path, BackendEnvVar + "=" + backendKeyring} {
		if !strings.Contains(out, want) {
			t.Errorf("notice missing %q:\n%s", want, out)
		}
	}
}

func TestFallbackStore_GetMissInBothStoresReturnsTheKeyringError(t *testing.T) {
	primary := newScriptedStore()
	primary.getErr = errNoSecretService
	f, _, notice, adopted := newTestFallback(t, primary)

	_, err := f.Get("svc", "alice")
	if !errors.Is(err, errNoSecretService) {
		t.Fatalf("err = %v, want the keyring error (a saved context with no readable credential is a store problem)", err)
	}
	if errors.Is(err, ErrNotFound) {
		t.Fatal("err must not read as ErrNotFound: callers would print 'not logged in'")
	}
	if *adopted != nil || persistedBackend() != "" || notice.Len() != 0 {
		t.Fatal("nothing must be adopted, remembered, or announced when the file store has nothing")
	}
}

func TestFallbackStore_SetWritesFileAndRemembersOnce(t *testing.T) {
	primary := newScriptedStore()
	primary.setErr = errNoSecretService
	f, file, notice, adopted := newTestFallback(t, primary)

	for i := range 2 {
		if err := f.Set("svc", "alice", fmt.Sprintf("tok%d", i)); err != nil {
			t.Fatalf("Set %d: %v", i, err)
		}
	}
	if got, err := file.Get("svc", "alice"); err != nil || got != "tok1" {
		t.Fatalf("file has (%q, %v), want tok1", got, err)
	}
	if *adopted != store(file) || persistedBackend() != backendFile {
		t.Fatal("file store must be adopted and remembered")
	}
	// Count the sentence only the notice prints: keyringProviderName() itself
	// returns "OS keyring" on an unrecognised GOOS, which would double a count
	// on that string.
	if n := strings.Count(notice.String(), "Storing Entire tokens"); n != 1 {
		t.Fatalf("notice printed %d times, want once:\n%s", n, notice.String())
	}
}

func TestFallbackStore_SetReportsBothFailures(t *testing.T) {
	primary := newScriptedStore()
	primary.setErr = errNoSecretService
	f, file, _, adopted := newTestFallback(t, primary)
	file.pathErr = errors.New("relative config dir refused")

	err := f.Set("svc", "alice", "tok")
	if !errors.Is(err, errNoSecretService) || !strings.Contains(err.Error(), "relative config dir refused") {
		t.Fatalf("err = %v, want both the keyring and the file failure", err)
	}
	if !errors.Is(err, ErrFileStoreFailed) {
		t.Fatalf("err = %v, want ErrFileStoreFailed so the login hint points at the path override rather than the file store", err)
	}
	if *adopted != nil || persistedBackend() != "" {
		t.Fatal("nothing adopted or remembered when the file store also failed")
	}
}

func TestFallbackStore_NotFoundAndInterruptPassThrough(t *testing.T) {
	for name, primaryErr := range map[string]error{
		"not found": ErrNotFound,
		"ctrl-c":    fmt.Errorf("get interrupted: %w", context.Canceled),
	} {
		t.Run(name, func(t *testing.T) {
			primary := newScriptedStore()
			primary.getErr = primaryErr
			f, _, notice, adopted := newTestFallback(t, primary)
			fileCalls := 0
			inner := f.newFile
			f.newFile = func() store { fileCalls++; return inner() }

			_, err := f.Get("svc", "alice")
			if !errors.Is(err, primaryErr) {
				t.Fatalf("err = %v, want %v unchanged", err, primaryErr)
			}
			if fileCalls != 0 || *adopted != nil || notice.Len() != 0 {
				t.Fatal("file store must not be consulted")
			}
		})
	}
}

func TestFallbackStore_SuccessfulKeyringCallTouchesNothing(t *testing.T) {
	primary := newScriptedStore()
	f, _, notice, adopted := newTestFallback(t, primary)
	fileCalls := 0
	inner := f.newFile
	f.newFile = func() store { fileCalls++; return inner() }
	if err := f.Set("svc", "alice", "tok"); err != nil {
		t.Fatal(err)
	}
	if got, err := f.Get("svc", "alice"); err != nil || got != "tok" {
		t.Fatalf("Get = (%q, %v)", got, err)
	}
	if fileCalls != 0 || *adopted != nil || persistedBackend() != "" || notice.Len() != 0 {
		t.Fatal("a working keyring must leave no trace of the fallback, not even a constructed file store")
	}
}

func TestFallbackStore_DeleteFallsBackAndTreatsFileMissAsNotFound(t *testing.T) {
	primary := newScriptedStore()
	primary.delErr = errNoSecretService
	f, file, notice, adopted := newTestFallback(t, primary)

	if err := f.Delete("svc", "alice"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete with nothing anywhere = %v, want ErrNotFound so logout can finish", err)
	}
	if *adopted != nil {
		t.Fatal("a miss adopts nothing")
	}
	// The miss reads as ErrNotFound for logout's sake, but the keyring copy is
	// unconfirmed — it may sit in a keyring that is merely locked or slow — so
	// the user is told, once, not on every slot logout clears.
	if out := notice.String(); !strings.Contains(out, "Could not confirm") || !strings.Contains(out, errNoSecretService.Error()) {
		t.Fatalf("a miss in both stores must warn that the keyring copy is unconfirmed:\n%s", out)
	}
	if err := f.Delete("svc", "bob"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second miss = %v, want ErrNotFound", err)
	}
	if n := strings.Count(notice.String(), "Could not confirm"); n != 1 {
		t.Fatalf("unconfirmed-removal warning printed %d times, want once per process:\n%s", n, notice.String())
	}

	if err := file.Set("svc", "alice", "tok"); err != nil {
		t.Fatal(err)
	}
	if err := f.Delete("svc", "alice"); err != nil {
		t.Fatalf("Delete of a file-held credential: %v", err)
	}
	if _, err := file.Get("svc", "alice"); !errors.Is(err, ErrNotFound) {
		t.Fatal("credential should be gone from the file store")
	}
	if *adopted != store(file) {
		t.Fatal("a successful file delete adopts the file store")
	}
}

// With an explicit ENTIRE_TOKEN_STORE_PATH the fallback still adopts the file
// store for this process, but the marker is not written (it cannot carry the
// path) and the notice must say so instead of claiming the choice is
// remembered. That holds when the keyring timed out too: the timeout branch
// has its own reason for not remembering, and it must not swallow the advice
// that the path variable has to stay set — a later command without it looks
// at the default path, which does not hold the token.
func TestFallbackStore_ExplicitPathIsAdoptedButNotRemembered(t *testing.T) {
	for name, tc := range map[string]struct {
		keyringErr error
		timedOut   bool
	}{
		"keyring unavailable": {errNoSecretService, false},
		"keyring timed out":   {fmt.Errorf("set timed out: %w", context.DeadlineExceeded), true},
	} {
		t.Run(name, func(t *testing.T) {
			primary := newScriptedStore()
			primary.setErr = tc.keyringErr
			f, _, notice, adopted := newTestFallback(t, primary)
			t.Setenv(PathEnvVar, filepath.Join(t.TempDir(), "tokens.json"))
			file := defaultFileStore()
			f.newFile = func() store { return file }

			if err := f.Set("svc", "alice", "tok"); err != nil {
				t.Fatal(err)
			}
			if *adopted != store(file) {
				t.Fatal("the explicit-path file store must still be adopted for this process")
			}
			if got := persistedBackend(); got != "" {
				t.Fatalf("persisted = %q, want empty while %s is set", got, PathEnvVar)
			}
			out := notice.String()
			if strings.Contains(out, "This choice is remembered") || !strings.Contains(out, PathEnvVar+" is set") {
				t.Fatalf("notice must say the choice is not remembered and why:\n%s", out)
			}
			if strings.Contains(out, "timed out") != tc.timedOut {
				t.Fatalf("notice mentions a timeout = %v, want %v:\n%s", !tc.timedOut, tc.timedOut, out)
			}
		})
	}
}

// A keyring that timed out is fallback-eligible, but callKeyringWithTimeout
// abandons the goroutine rather than cancelling it, so the keyring may still
// complete the write later. Remembering the file store then would orphan that
// keyring copy for good; the adoption is for this process only, and the
// notice says why.
func TestFallbackStore_TimeoutAdoptsButDoesNotRemember(t *testing.T) {
	primary := newScriptedStore()
	primary.setErr = fmt.Errorf("set timed out: %w", context.DeadlineExceeded)
	f, file, notice, adopted := newTestFallback(t, primary)

	if err := f.Set("svc", "alice", "tok"); err != nil {
		t.Fatal(err)
	}
	if *adopted != store(file) {
		t.Fatal("the file store must still be adopted for this process")
	}
	if got := persistedBackend(); got != "" {
		t.Fatalf("persisted = %q after a keyring timeout, want empty: the keyring may still answer", got)
	}
	out := notice.String()
	for _, want := range []string{"timed out", "not remembered", keyringTimeoutEnvVar, BackendEnvVar + "=" + backendFile} {
		if !strings.Contains(out, want) {
			t.Errorf("notice missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "This choice is remembered") {
		t.Fatalf("notice must not promise a memory a timeout does not keep:\n%s", out)
	}
	if strings.Contains(out, PathEnvVar+" is set") {
		t.Fatalf("notice must not mention %s when it is not set:\n%s", PathEnvVar, out)
	}
}

// failingPrimary is a goroutine-safe primary whose every call fails with err,
// for tests that fall back from several goroutines at once (scriptedStore's
// counters are not safe to share).
type failingPrimary struct{ err error }

func (p failingPrimary) Get(string, string) (string, error) { return "", p.err }
func (p failingPrimary) Set(string, string, string) error   { return p.err }
func (p failingPrimary) Delete(string, string) error        { return p.err }

// The struct comment on fallbackStore justifies memoizing the file store by a
// concurrency argument; this pins it. newFile hands out a fresh instance per
// call, so only fileOnce can make every adoption see the same one.
func TestFallbackStore_ConcurrentFallbacksShareOneFileStore(t *testing.T) {
	f, _, _, _ := newTestFallback(t, failingPrimary{err: errNoSecretService})
	f.newFile = func() store { return defaultFileStore() }
	var mu sync.Mutex
	var adopted []store
	f.adopt = func(s store) {
		mu.Lock()
		defer mu.Unlock()
		adopted = append(adopted, s)
	}

	var wg sync.WaitGroup
	for i := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := f.Set("svc", fmt.Sprintf("user%d", i), "tok"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()

	if len(adopted) != 4 {
		t.Fatalf("adopt called %d times, want 4 (once per successful fallback)", len(adopted))
	}
	for _, s := range adopted[1:] {
		if s != adopted[0] {
			t.Fatal("concurrent fallbacks adopted different file store instances; memoization is broken")
		}
	}
}

func TestSetBackend_ReplacesTheProcessBackend(t *testing.T) {
	resetBackendForTesting(t)
	s := newScriptedStore()
	setBackend(s)
	if currentBackend() != store(s) {
		t.Fatal("currentBackend() should be the adopted store without re-resolving")
	}
}

// With ENTIRE_TOKEN_STORE_PATH set the fallback adopts the file store but
// writes no marker, so provenance cannot learn the switch from disk: without
// the adoption flag, `auth status` named the keyring for a token it had just
// read from the file. The test-only restore must put the flag back too, or one
// test's adoption would colour every later provenance assertion.
func TestSetBackend_AdoptionIsVisibleToProvenanceWhenPathIsSet(t *testing.T) {
	isolateConfigDir(t)
	t.Setenv(BackendEnvVar, "")
	t.Setenv(PathEnvVar, filepath.Join(t.TempDir(), "tokens.json"))
	resetBackendForTesting(t)
	if FileBackendSelected() {
		t.Fatal("nothing selected before the adoption")
	}

	restore := UseFileBackendForTesting(filepath.Join(t.TempDir(), "override.json"))
	setBackend(defaultFileStore())
	if !FileBackendSelected() {
		t.Fatal("an in-process adoption must select the file store for provenance")
	}
	if got := BackendDescription(); !strings.HasPrefix(got, "file ") {
		t.Fatalf("BackendDescription() = %q after adoption, want the file store", got)
	}
	if got := persistedBackend(); got != "" {
		t.Fatalf("persisted = %q, want empty: the flag, not a marker, carries the PATH case", got)
	}

	restore()
	if FileBackendSelected() {
		t.Fatal("restoring the test backend must clear the adoption flag")
	}
}

// Pinned by descriptor rather than pointer equality with os.Stderr, for the
// reason TestLoosePermsWarnWriter_DefaultsToStderr gives: under `go test -json`
// the testing package swaps the os.Stderr variable after package init.
func TestFallbackNoticeWriter_DefaultsToStderr(t *testing.T) {
	f, ok := fallbackNoticeW.(*os.File)
	if !ok || f.Fd() != uintptr(syscall.Stderr) {
		t.Fatalf("fallbackNoticeW default = %T, want the process stderr", fallbackNoticeW)
	}
}

// The struct tests inject adopt and never touch package state; this one drives
// the package-level Set and Get through a fallback store installed the way
// resolveBackend installs it (adoptedFile still false), so it pins the wiring
// those tests bypass: newFallbackStore's adopt is setBackend, and setBackend
// replaces the process backend, so the very next package call reads the file
// store directly instead of asking the keyring again.
func TestPackageSetAndGetGoThroughTheFallback(t *testing.T) {
	isolateConfigDir(t)
	t.Setenv(BackendEnvVar, "")
	notice := captureNotices(t)
	resetBackendForTesting(t)
	primary := newScriptedStore()
	primary.setErr, primary.getErr = errNoSecretService, errNoSecretService
	backendMu.Lock()
	backend, resolved = newFallbackStore(primary), true
	backendMu.Unlock()
	if FileBackendSelected() {
		t.Fatal("precondition: nothing selects the file store before the fallback fires")
	}

	if err := Set("svc", "alice", "tok"); err != nil {
		t.Fatalf("Set through the fallback: %v", err)
	}
	got, err := Get("svc", "alice")
	if err != nil || got != "tok" {
		t.Fatalf("Get after adoption = (%q, %v), want (tok, nil)", got, err)
	}
	if primary.gets != 0 {
		t.Fatalf("keyring asked %d times on Get, want 0: adoption must replace the process backend", primary.gets)
	}
	if got := persistedBackend(); got != backendFile {
		t.Fatalf("persisted = %q, want %q", got, backendFile)
	}
	if !FileBackendSelected() {
		t.Fatal("FileBackendSelected() must follow the adoption")
	}
	if n := strings.Count(notice.String(), "Note: OS keyring"); n != 1 {
		t.Fatalf("notice printed %d times, want once:\n%s", n, notice.String())
	}
}

// A tokens.json that will not parse is a file store that FAILED, not one that
// has nothing: on Get and on Delete alike the fallback must report both
// failures — ErrFileStoreFailed for login's hint, the keyring error for auth
// status — and adopt nothing. Reading it as a miss would print "not logged in"
// about a credential sitting in a file the CLI cannot parse, or, on Delete,
// tell logout the slot is gone when nothing was checked.
func TestFallbackStore_CorruptFileStoreReportsBothFailures(t *testing.T) {
	primary := newScriptedStore()
	primary.getErr, primary.delErr = errNoSecretService, errNoSecretService
	f, file, notice, adopted := newTestFallback(t, primary)
	if err := os.MkdirAll(filepath.Dir(file.path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file.path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, getErr := f.Get("svc", "alice")
	for op, err := range map[string]error{"Get": getErr, "Delete": f.Delete("svc", "alice")} {
		if !errors.Is(err, ErrFileStoreFailed) || !errors.Is(err, errNoSecretService) {
			t.Fatalf("%s = %v, want both ErrFileStoreFailed and the keyring error reachable", op, err)
		}
		if errors.Is(err, ErrNotFound) {
			t.Fatalf("%s = %v must not read as ErrNotFound: nothing was checked", op, err)
		}
	}
	if *adopted != nil {
		t.Fatal("a file store that failed must not be adopted")
	}
	if notice.Len() != 0 {
		t.Fatalf("nothing to announce when both stores failed:\n%s", notice.String())
	}
}

// rememberWarn's comment says the marker write is retried on every adoption
// and only the warning is deduped; this pins both halves. The config dir is
// made unwritable by pointing ENTIRE_CONFIG_DIR at a regular FILE, which fails
// userdirs.ConfigRoot with something other than ErrNotExist (a missing
// directory would simply be created), while the file store is injected at a
// working path so the adoption itself succeeds and switchTo runs. The failed
// open is not memoized — osroot.Shared caches successful opens only — so
// replacing the file with a directory is all the retry needs.
func TestFallbackStore_MarkerWriteIsRetriedOnEveryAdoption(t *testing.T) {
	configDir := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(configDir, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(userdirs.EnvConfigDir, configDir)
	t.Setenv(PathEnvVar, "")
	notice := captureNotices(t)
	primary := newScriptedStore()
	primary.setErr = errNoSecretService
	tokens := &fileStore{path: filepath.Join(t.TempDir(), "tokens.json")}
	f := newFallbackStore(primary)
	f.newFile = func() store { return tokens }
	f.adopt = func(store) {}

	const couldNotRemember = "could not remember the token store choice"
	if err := f.Set("svc", "alice", "tok"); err != nil {
		t.Fatalf("first Set: %v", err)
	}
	if !strings.Contains(notice.String(), couldNotRemember) {
		t.Fatalf("a failed marker write must be warned about:\n%s", notice.String())
	}

	if err := os.Remove(configDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := f.Set("svc", "alice", "tok"); err != nil {
		t.Fatalf("second Set: %v", err)
	}
	if got := persistedBackend(); got != backendFile {
		t.Fatalf("persisted = %q after the second adoption, want %q: the marker write must be retried", got, backendFile)
	}
	if n := strings.Count(notice.String(), couldNotRemember); n != 1 {
		t.Fatalf("could-not-remember warned %d times, want once:\n%s", n, notice.String())
	}
}
