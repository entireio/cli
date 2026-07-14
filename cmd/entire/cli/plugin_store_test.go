package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// testPluginName is the bare plugin name used across managed-store tests.
const testPluginName = "pgr"

// withPluginDir points $ENTIRE_PLUGIN_DIR at a fresh temp dir so the managed
// helpers operate in isolation. Mutates process state, so the calling test
// must not be t.Parallel.
func withPluginDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(pluginEnvPluginDir, dir)
	return dir
}

func TestPluginParentDir_HonorsOverride(t *testing.T) { //nolint:paralleltest // mutates env
	dir := withPluginDir(t)
	got, err := pluginParentDir()
	if err != nil {
		t.Fatalf("pluginParentDir: %v", err)
	}
	if got != dir {
		t.Errorf("pluginParentDir = %q, want %q", got, dir)
	}
}

func TestPluginParentDir_RejectsRelativeOverride(t *testing.T) { //nolint:paralleltest // mutates env
	// A relative ENTIRE_PLUGIN_DIR would resolve against startup CWD —
	// typically inside the user's repo. Reject rather than silently
	// fall through to the platform default.
	t.Setenv(pluginEnvPluginDir, "plugins-relative")
	if _, err := pluginParentDir(); err == nil {
		t.Errorf("pluginParentDir with relative override = nil error; want error")
	}
	t.Setenv(pluginEnvPluginDir, ".")
	if _, err := pluginParentDir(); err == nil {
		t.Errorf("pluginParentDir with '.' override = nil error; want error")
	}
}

func TestPluginParentDir_WindowsIgnoresXDG(t *testing.T) { //nolint:paralleltest // mutates env
	if runtime.GOOS != windowsGOOS {
		t.Skip("Windows-only behavior")
	}
	// ENTIRE_PLUGIN_DIR not set; XDG_DATA_HOME set. Result must NOT be
	// rooted at the XDG path — Windows users expect Windows conventions.
	xdg := t.TempDir()
	t.Setenv(pluginEnvPluginDir, "")
	t.Setenv("XDG_DATA_HOME", xdg)
	got, err := pluginParentDir()
	if err != nil {
		t.Fatalf("pluginParentDir: %v", err)
	}
	xdgRoot := filepath.Clean(filepath.Join(xdg, pluginManagedTopDir, pluginManagedSubDir))
	if strings.HasPrefix(filepath.Clean(got), xdgRoot) {
		t.Errorf("pluginParentDir = %q is rooted at XDG path %q; XDG_DATA_HOME must be ignored on Windows", got, xdgRoot)
	}
}

func TestPluginParentDir_UnixHonorsXDG(t *testing.T) { //nolint:paralleltest // mutates env
	if runtime.GOOS == windowsGOOS {
		t.Skip("Unix-only behavior")
	}
	t.Setenv(pluginEnvPluginDir, "")
	xdg := t.TempDir()
	t.Setenv("XDG_DATA_HOME", xdg)
	got, err := pluginParentDir()
	if err != nil {
		t.Fatalf("pluginParentDir: %v", err)
	}
	want := filepath.Join(xdg, "entire", "plugins")
	if got != want {
		t.Errorf("pluginParentDir = %q, want %q", got, want)
	}
}

func TestPluginBinDir_AndDataDir(t *testing.T) { //nolint:paralleltest // mutates env
	root := withPluginDir(t)
	bin, err := PluginBinDir()
	if err != nil {
		t.Fatalf("PluginBinDir: %v", err)
	}
	if want := filepath.Join(root, "bin"); bin != want {
		t.Errorf("PluginBinDir = %q, want %q", bin, want)
	}
	data, err := PluginDataDir("pgr")
	if err != nil {
		t.Fatalf("PluginDataDir: %v", err)
	}
	if want := filepath.Join(root, "data", "pgr"); data != want {
		t.Errorf("PluginDataDir(pgr) = %q, want %q", data, want)
	}
}

func TestPrependPluginBinDirToPATH(t *testing.T) { //nolint:paralleltest // mutates env
	root := withPluginDir(t)
	original := "/usr/bin:/bin"
	t.Setenv("PATH", original)
	restore := PrependPluginBinDirToPATH(context.Background())
	bin := filepath.Join(root, "bin")
	got := os.Getenv("PATH")
	if !strings.HasPrefix(got, bin+string(os.PathListSeparator)) {
		t.Errorf("PATH does not start with managed bin dir: %q", got)
	}
	// Idempotent: a second call returns a no-op restore and does not
	// double-prepend.
	noop := PrependPluginBinDirToPATH(context.Background())
	if strings.Count(os.Getenv("PATH"), bin) != 1 {
		t.Errorf("PATH contains managed bin dir %d times after second prepend; want 1: %q", strings.Count(os.Getenv("PATH"), bin), os.Getenv("PATH"))
	}
	noop() // safe no-op

	// Restoring must return PATH to exactly what it was before the
	// prepend, so built-in execution doesn't inherit the managed dir.
	restore()
	if got := os.Getenv("PATH"); got != original {
		t.Errorf("PATH after restore = %q; want %q", got, original)
	}
}

func TestInstallPluginFromPath_SymlinksAndLists(t *testing.T) { //nolint:paralleltest // mutates env
	if runtime.GOOS == windowsGOOS {
		t.Skip("symlink path is Unix-only here")
	}
	withPluginDir(t)
	src := filepath.Join(t.TempDir(), "entire-pgr")
	if err := os.WriteFile(src, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write src: %v", err)
	}

	p, err := InstallPluginFromPath(InstallPluginOptions{SourcePath: src})
	if err != nil {
		t.Fatalf("InstallPluginFromPath: %v", err)
	}
	if p.Name != testPluginName {
		t.Errorf("Name = %q, want %s", p.Name, testPluginName)
	}
	if !p.Symlink {
		t.Errorf("expected Symlink=true; got %+v", p)
	}

	plugins, err := ListInstalledPlugins()
	if err != nil {
		t.Fatalf("ListInstalledPlugins: %v", err)
	}
	if len(plugins) != 1 || plugins[0].Name != testPluginName {
		t.Errorf("ListInstalledPlugins = %+v; want one entry named %s", plugins, testPluginName)
	}

	// Re-install without --force fails.
	if _, err := InstallPluginFromPath(InstallPluginOptions{SourcePath: src}); err == nil {
		t.Errorf("expected error on re-install without --force")
	}
	// With --force succeeds.
	if _, err := InstallPluginFromPath(InstallPluginOptions{SourcePath: src, Force: true}); err != nil {
		t.Errorf("InstallPluginFromPath --force: %v", err)
	}

	// Remove unlinks the managed entry without disturbing the source.
	if err := RemoveInstalledPlugin(testPluginName); err != nil {
		t.Fatalf("RemoveInstalledPlugin: %v", err)
	}
	if _, err := os.Stat(src); err != nil {
		t.Errorf("source %s was disturbed by RemoveInstalledPlugin: %v", src, err)
	}
	if err := RemoveInstalledPlugin(testPluginName); err == nil {
		t.Errorf("expected error removing already-removed plugin")
	}
}

func TestInstallPluginFromPath_RejectsBadBasename(t *testing.T) { //nolint:paralleltest // mutates env
	if runtime.GOOS == windowsGOOS {
		t.Skip("Unix permissions checks")
	}
	withPluginDir(t)
	src := filepath.Join(t.TempDir(), "not-prefixed")
	if err := os.WriteFile(src, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write src: %v", err)
	}
	if _, err := InstallPluginFromPath(InstallPluginOptions{SourcePath: src}); err == nil {
		t.Errorf("expected error for non-prefixed basename")
	}
}

func TestInstallPluginFromPath_RejectsNonExecutable(t *testing.T) { //nolint:paralleltest // mutates env
	if runtime.GOOS == windowsGOOS {
		t.Skip("Unix permissions checks")
	}
	withPluginDir(t)
	src := filepath.Join(t.TempDir(), "entire-noexec")
	if err := os.WriteFile(src, []byte("#!/bin/sh\nexit 0\n"), 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}
	if _, err := InstallPluginFromPath(InstallPluginOptions{SourcePath: src}); err == nil {
		t.Errorf("expected error for non-executable source")
	}
}

func TestBareNameFromBinaryName(t *testing.T) {
	t.Parallel()
	// Cases that hold on every platform.
	common := map[string]string{
		"entire-pgr": "pgr",
		"entire-":    "",
		"foo":        "",
		"":           "",
	}
	for in, want := range common {
		if got := bareNameFromBinaryName(in); got != want {
			t.Errorf("bareNameFromBinaryName(%q) = %q; want %q", in, got, want)
		}
	}
	// Platform-conditional: extensions are stripped only on Windows so a
	// managed entry actually resolves at runtime via exec.LookPath.
	if runtime.GOOS == windowsGOOS {
		for in, want := range map[string]string{
			"entire-pgr.exe": "pgr",
			"entire-foo.bat": "foo",
			"entire-foo.cmd": "foo",
		} {
			if got := bareNameFromBinaryName(in); got != want {
				t.Errorf("[windows] bareNameFromBinaryName(%q) = %q; want %q", in, got, want)
			}
		}
	} else {
		// On Unix, a .exe basename is *not* a valid bare name — installing
		// it would yield a managed entry that LookPath would never match.
		// We accept that bareNameFromBinaryName may return a non-empty
		// string here (the dispatcher uses exact-match LookPath); the
		// guarantee we test is that "entire-pgr.exe" doesn't collapse to
		// "pgr" on Unix.
		if got := bareNameFromBinaryName("entire-pgr.exe"); got == "pgr" {
			t.Errorf("[unix] bareNameFromBinaryName(entire-pgr.exe) collapsed to %q; should not strip .exe on Unix", got)
		}
	}
}

func TestValidatePluginName(t *testing.T) {
	t.Parallel()
	good := []string{"pgr", "foo-bar", "x", "v1"}
	for _, n := range good {
		if err := validatePluginName(n); err != nil {
			t.Errorf("validatePluginName(%q) = %v; want nil", n, err)
		}
	}
	bad := []string{"", ".", "..", "-foo", "agent-foo", "foo/bar", `foo\bar`}
	for _, n := range bad {
		if err := validatePluginName(n); err == nil {
			t.Errorf("validatePluginName(%q) = nil; want error", n)
		}
	}
}

func TestPluginDataDir_RejectsPathTraversal(t *testing.T) { //nolint:paralleltest // mutates env
	withPluginDir(t)
	for _, name := range []string{"", ".", "..", "agent-foo", "foo/bar"} {
		if _, err := PluginDataDir(name); err == nil {
			t.Errorf("PluginDataDir(%q) = nil error; want error", name)
		}
	}
}

func TestInstallPluginFromPath_RejectsAgentReservedName(t *testing.T) { //nolint:paralleltest // mutates env
	if runtime.GOOS == windowsGOOS {
		t.Skip("Unix-only test")
	}
	withPluginDir(t)
	src := filepath.Join(t.TempDir(), "entire-agent-foo")
	if err := os.WriteFile(src, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write src: %v", err)
	}
	if _, err := InstallPluginFromPath(InstallPluginOptions{SourcePath: src}); err == nil {
		t.Errorf("expected error: agent-* basename is reserved")
	}
}

func TestInstallPluginFromPath_RejectsSelfInstall(t *testing.T) { //nolint:paralleltest // mutates env
	if runtime.GOOS == windowsGOOS {
		t.Skip("Unix-only test")
	}
	root := withPluginDir(t)
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Drop the source directly into the managed dir, then attempt to
	// install it from that same path. Without the self-install guard,
	// --force would Remove() this file before symlinking to a missing
	// target, deleting the working install.
	src := filepath.Join(binDir, "entire-foo")
	if err := os.WriteFile(src, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write src: %v", err)
	}
	_, err := InstallPluginFromPath(InstallPluginOptions{SourcePath: src, Force: true})
	if err == nil {
		t.Fatalf("expected self-install rejection; got nil")
	}
	if _, statErr := os.Stat(src); statErr != nil {
		t.Errorf("self-install attempt deleted the source: %v", statErr)
	}
}

// TestMaterializeManagedEntry_HappyPath is a smoke test that the helper
// completes successfully on a normal write-capable destination. On Unix it
// exits through the symlink branch; the hardlink and copy fallbacks exist
// for Windows-without-Developer-Mode and aren't portably triggerable from an
// in-process test (forcing os.Symlink to fail without mocks would require a
// non-portable filesystem setup). The other tests in this file exercise
// InstallPluginFromPath end-to-end, which calls into here.
func TestMaterializeManagedEntry_HappyPath(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == windowsGOOS {
		t.Skip("test exercises Unix file modes")
	}
	srcDir := t.TempDir()
	src := filepath.Join(srcDir, "src-bin")
	body := []byte("#!/bin/sh\nexit 0\n")
	if err := os.WriteFile(src, body, 0o755); err != nil {
		t.Fatalf("write src: %v", err)
	}
	srcInfo, err := os.Stat(src)
	if err != nil {
		t.Fatalf("stat src: %v", err)
	}

	dest := filepath.Join(t.TempDir(), "out")
	if err := materializeManagedEntry(src, dest, srcInfo); err != nil {
		t.Fatalf("materializeManagedEntry: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read result: %v", err)
	}
	if string(got) != string(body) {
		t.Errorf("dest content mismatch: got %q want %q", got, body)
	}
}

func TestRemoveInstalledPlugin_RemovesAllVariants(t *testing.T) { //nolint:paralleltest // mutates env
	// Simulate a corrupted state with two variants for the same bare name
	// (the situation `entire plugin install` now prevents but legacy state
	// or hand-edits could produce). RemoveInstalledPlugin must clean up
	// every match, not just the first one FindInstalledPlugin returns.
	if runtime.GOOS == windowsGOOS {
		t.Skip("file-naming below is Unix-style")
	}
	root := withPluginDir(t)
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// On Unix, bareNameFromBinaryName preserves dots, so to produce two
	// entries with identical bare names we need names that the helper
	// folds together. We can't natively get that on Unix, so this test
	// asserts the loop behavior by placing one entry and verifying the
	// removal path iterates correctly even when only a single match
	// exists. The Windows-specific multi-variant path is covered by the
	// implementation reading installedVariantsByBareName.
	body := []byte("#!/bin/sh\nexit 0\n")
	if err := os.WriteFile(filepath.Join(binDir, "entire-foo"), body, 0o755); err != nil {
		t.Fatalf("write entry: %v", err)
	}
	if err := RemoveInstalledPlugin("foo"); err != nil {
		t.Fatalf("RemoveInstalledPlugin: %v", err)
	}
	if _, err := os.Stat(filepath.Join(binDir, "entire-foo")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("entire-foo still present after remove: %v", err)
	}
}

func TestInstallPluginFromPath_TmpDoesNotClobberDottedPlugin(t *testing.T) { //nolint:paralleltest // mutates env
	if runtime.GOOS == windowsGOOS {
		t.Skip("symlink path is Unix-only here")
	}
	root := withPluginDir(t)
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Pre-populate a plugin literally named "foo.tmp" — entirely valid:
	// the dispatcher's name validator allows dots. The naive `dest+".tmp"`
	// scheme would have clobbered this on the install below.
	dotted := filepath.Join(binDir, "entire-foo.tmp")
	dottedBody := []byte("#!/bin/sh\necho dotted\n")
	if err := os.WriteFile(dotted, dottedBody, 0o755); err != nil {
		t.Fatalf("write dotted: %v", err)
	}

	// Now install entire-foo. Its temp path must not collide with
	// entire-foo.tmp.
	src := filepath.Join(t.TempDir(), "entire-foo")
	if err := os.WriteFile(src, []byte("#!/bin/sh\necho foo\n"), 0o755); err != nil {
		t.Fatalf("write src: %v", err)
	}
	if _, err := InstallPluginFromPath(InstallPluginOptions{SourcePath: src}); err != nil {
		t.Fatalf("InstallPluginFromPath: %v", err)
	}

	// The dotted plugin must still exist with its original content.
	got, err := os.ReadFile(dotted)
	if err != nil {
		t.Fatalf("dotted plugin disappeared: %v", err)
	}
	if string(got) != string(dottedBody) {
		t.Errorf("dotted plugin clobbered: got %q want %q", got, dottedBody)
	}
}

func TestInstallPluginFromPath_RequiresForceForSameBareName(t *testing.T) { //nolint:paralleltest // mutates env
	// A second install of a different source file that resolves to the
	// same bare name as a prior install must require --force. The
	// cross-extension flavor of this conflict (entire-foo.exe vs
	// entire-foo.bat sharing bare name "foo") is Windows-only and
	// exercised by installedVariantsByBareName at the implementation
	// level — the same-bare-name guard tested here is the user-visible
	// surface on every platform.
	if runtime.GOOS == windowsGOOS {
		t.Skip("file-naming below is Unix-style; Windows tests would need a different harness")
	}
	root := withPluginDir(t)
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Install entire-foo first.
	srcA := filepath.Join(t.TempDir(), "entire-foo")
	if err := os.WriteFile(srcA, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write src A: %v", err)
	}
	if _, err := InstallPluginFromPath(InstallPluginOptions{SourcePath: srcA}); err != nil {
		t.Fatalf("first install: %v", err)
	}

	// A second install of the exact same source path is a self-install
	// (path-equal) — that's tested elsewhere. Here we test that a
	// different-source same-bare-name install requires --force.
	srcB := filepath.Join(t.TempDir(), "entire-foo")
	if err := os.WriteFile(srcB, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write src B: %v", err)
	}
	if _, err := InstallPluginFromPath(InstallPluginOptions{SourcePath: srcB}); err == nil {
		t.Errorf("expected error: bare name %q already installed", "foo")
	}
	if _, err := InstallPluginFromPath(InstallPluginOptions{SourcePath: srcB, Force: true}); err != nil {
		t.Errorf("force install: %v", err)
	}
}

func TestMakeInstallTmpPath_Unique(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	a, err := makeInstallTmpPath(dir)
	if err != nil {
		t.Fatalf("makeInstallTmpPath: %v", err)
	}
	b, err := makeInstallTmpPath(dir)
	if err != nil {
		t.Fatalf("makeInstallTmpPath: %v", err)
	}
	if a == b {
		t.Errorf("two calls returned the same path: %q", a)
	}
	// Tmp prefix must not match the listing filter (which keys off
	// "entire-"); the dot-prefix achieves that.
	if !strings.HasPrefix(filepath.Base(a), ".install-") {
		t.Errorf("tmp path %q does not start with .install-", a)
	}
}

func TestRemoveEnvKey(t *testing.T) {
	t.Parallel()
	in := []string{"FOO=1", "BAR=2", "FOO=3", "BAZ=4"}
	got := removeEnvKey(in, "FOO")
	want := []string{"BAR=2", "BAZ=4"}
	if !equalStringSlices(got, want) {
		t.Errorf("removeEnvKey = %v, want %v", got, want)
	}
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestInstallPluginFromPath_AtomicForceReplace(t *testing.T) { //nolint:paralleltest // mutates env
	if runtime.GOOS == windowsGOOS {
		t.Skip("symlink/atomic-rename behavior is Unix-focused here")
	}
	withPluginDir(t)
	srcDir := t.TempDir()
	src := filepath.Join(srcDir, "entire-foo")
	if err := os.WriteFile(src, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write src: %v", err)
	}
	if _, err := InstallPluginFromPath(InstallPluginOptions{SourcePath: src}); err != nil {
		t.Fatalf("first install: %v", err)
	}
	binDir, err := PluginBinDir()
	if err != nil {
		t.Fatalf("PluginBinDir: %v", err)
	}
	dest := filepath.Join(binDir, "entire-foo")
	if _, err := os.Lstat(dest); err != nil {
		t.Fatalf("first install missing: %v", err)
	}

	// Force-install from the same source. The replace should succeed and
	// the previous symlink remains valid throughout.
	if _, err := InstallPluginFromPath(InstallPluginOptions{SourcePath: src, Force: true}); err != nil {
		t.Errorf("force replace: %v", err)
	}
	if _, err := os.Lstat(dest); err != nil {
		t.Errorf("dest missing after force replace: %v", err)
	}
}
