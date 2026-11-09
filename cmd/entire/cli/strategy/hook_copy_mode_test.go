package strategy

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// failHookLinks makes linkOrCopy take its copy path, as on a filesystem
// without hard links (some FUSE and network mounts, exFAT).
func failHookLinks(t *testing.T) {
	t.Helper()
	orig := linkHookFile
	linkHookFile = func(*os.Root, string, string) error { return errors.ErrUnsupported }
	t.Cleanup(func() { linkHookFile = orig })
}

// writeMode is write with perm instead of 0o755, set exactly whatever the umask.
func (f *hooksFixture) writeMode(name, content string, perm fs.FileMode) {
	f.t.Helper()
	f.write(name, content)
	if err := os.Chmod(filepath.Join(f.dir, name), perm); err != nil {
		f.t.Fatal(err)
	}
}

func (f *hooksFixture) assertMode(name string, want fs.FileMode) {
	f.t.Helper()
	info, err := os.Lstat(filepath.Join(f.dir, name))
	if err != nil {
		f.t.Fatalf("lstat %s: %v", name, err)
	}
	if got := info.Mode().Perm(); got != want {
		f.t.Errorf("%s mode = %v, want %v", name, got, want)
	}
}

// Where hard links are unavailable the backup is a byte copy, and it keeps
// the mode a link would share: a hook the user switched off with chmod -x
// stays off, and a private hook stays private. The older copy a rotation keeps
// is a copy of the backup, and keeps its mode as well.
func TestInstallHooks_CopiedBackupKeepsHookMode(t *testing.T) { //nolint:paralleltest // replaces linkHookFile
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits")
	}
	failHookLinks(t)
	spec := specFor(t, "pre-push")
	for _, perm := range []fs.FileMode{0o644, 0o600, 0o700, 0o755} {
		t.Run(perm.String(), func(t *testing.T) {
			f := newHooksFixture(t)
			f.writeMode("pre-push", userHookV1, perm)

			f.install(spec)

			f.assertMode("pre-push"+backupSuffix, perm)
		})
	}

	t.Run("rotated older copy", func(t *testing.T) {
		f := newHooksFixture(t)
		f.writeMode("pre-push", userHookV1, 0o600)
		f.install(spec)
		f.writeMode("pre-push", userHookV2, 0o644) // the user's hook replaces Entire's

		f.install(spec)

		older := f.olderCopies()
		if len(older) != 1 {
			t.Fatalf("older copies = %v, want one", older)
		}
		assertHooks(t, f, map[string]string{older[0]: userHookV1, "pre-push" + backupSuffix: userHookV2})
		f.assertMode(older[0], 0o600)
		f.assertMode("pre-push"+backupSuffix, 0o644)
	})
}

// The behavior the mode protects: git skips a hook that is not executable, and
// Entire's chain call skips the backup for the same reason, so backing up a
// switched-off hook must not make the next push run it.
func TestInstallHooks_CopiedBackupOfSwitchedOffHookDoesNotRun(t *testing.T) { //nolint:paralleltest // replaces linkHookFile
	if runtime.GOOS == "windows" {
		t.Skip("runs POSIX shell hooks")
	}
	failHookLinks(t)
	f := newHooksFixture(t)
	bin := t.TempDir()
	log := filepath.Join(t.TempDir(), "log")
	writeExec(t, filepath.Join(bin, "entire"), "#!/bin/sh\necho entire >> "+log+"\n")
	f.writeMode("pre-push", "#!/bin/sh\necho user >> "+log+"\n", 0o644)

	f.installExecutable(specFor(t, "pre-push"))

	ctx, cancel := context.WithTimeout(t.Context(), hookRunTimeout)
	defer cancel()
	if out, err := runHook(ctx, bin, filepath.Join(f.dir, "pre-push"), "origin", "url"); err != nil {
		t.Fatalf("hook failed: %v\n%s", err, out)
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(strings.Fields(string(data)), ","); got != "entire" {
		t.Errorf("ran %s, want entire only", got)
	}
}

// Taking a moved copy back from a hook manager always copies, hard links or
// not, so the user's hook that lands where the manager's wrapper runs it keeps
// its mode too: pre-commit runs <hook>.legacy only when it is executable.
func TestInstallHooks_ReclaimedUserHookKeepsMode(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits")
	}
	spec := specFor(t, "commit-msg")
	chained := generateChainedContent(spec.content, spec.name)
	for name, tc := range map[string]struct {
		wrapper, moved string
	}{
		"pre-commit":   {preCommitWrapper("commit-msg"), "commit-msg" + legacySuffix},
		"tool wrapper": {toolWrapper("commit-msg.tool-old", "/dev/null"), "commit-msg.tool-old"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newHooksFixture(t)
			f.write("commit-msg", tc.wrapper)
			f.write(tc.moved, chained)
			f.writeMode("commit-msg"+backupSuffix, userHookV1, 0o644)

			f.install(spec)

			assertHooks(t, f, map[string]string{tc.moved: userHookV1, tc.moved + backupSuffix: userHookV1})
			f.assertMode(tc.moved, 0o644)
			f.assertMode(tc.moved+backupSuffix, 0o644)
		})
	}
}

// With the mode kept, a change of mode alone is a new version of the hook. A
// hook manager can put the backed-up script, switched back on, in Entire's
// place; reusing the switched-off backup would keep the chain call skipping
// it. The backup is refreshed and the old one kept as an older copy, whether
// the backup is a copy or a hard link of the file the first install found.
func TestInstallHooks_ModeOnlyChangeRefreshesBackup(t *testing.T) { //nolint:paralleltest // replaces linkHookFile
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits")
	}
	spec := specFor(t, "pre-push")
	for name, noLinks := range map[string]bool{"copy": true, "link": false} {
		t.Run(name, func(t *testing.T) {
			if noLinks {
				failHookLinks(t)
			}
			f := newHooksFixture(t)
			f.writeMode("pre-push", userHookV1, 0o644)
			f.install(spec)
			f.writeMode("pre-push", userHookV1, 0o755) // the same script, switched on, replaces Entire's

			f.install(spec)

			f.assertMode("pre-push"+backupSuffix, 0o755)
			older := f.olderCopies()
			if len(older) != 1 {
				t.Fatalf("older copies = %v, want one", older)
			}
			assertHooks(t, f, map[string]string{older[0]: userHookV1, "pre-push" + backupSuffix: userHookV1})
			f.assertMode(older[0], 0o644)
		})
	}
}

// A keep copy picks up a change of mode alone in the file a manager's wrapper
// runs, so a user who switches their hook off there gets it back switched off
// when the manager next moves Entire's hook onto that file and Entire takes it
// back from the keep copy.
func TestInstallHooks_KeepCopyFollowsModeOnlyChange(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits")
	}
	spec := specFor(t, "commit-msg")
	chained := generateChainedContent(spec.content, spec.name)
	for name, tc := range map[string]struct {
		wrapper, moved string
	}{
		"pre-commit":   {preCommitWrapper("commit-msg"), "commit-msg" + legacySuffix},
		"tool wrapper": {toolWrapper("commit-msg.tool-old", "/dev/null"), "commit-msg.tool-old"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newHooksFixture(t)
			f.write("commit-msg", tc.wrapper)
			f.write(tc.moved, chained)
			f.write("commit-msg"+backupSuffix, userHookV1)
			f.install(spec)
			if err := os.Chmod(filepath.Join(f.dir, tc.moved), 0o644); err != nil { // the user switches it off
				t.Fatal(err)
			}

			f.install(spec)
			f.assertMode(tc.moved+backupSuffix, 0o644)

			f.write("commit-msg", tc.wrapper) // the manager reinstalls, moving Entire's hook onto it
			f.write(tc.moved, chained)
			f.install(spec)
			assertHooks(t, f, map[string]string{tc.moved: userHookV1})
			f.assertMode(tc.moved, 0o644)
		})
	}
}
