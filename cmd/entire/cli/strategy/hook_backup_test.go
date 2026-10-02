package strategy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var rotationClock = time.Date(2026, 10, 1, 17, 4, 5, 0, time.UTC)

const rotatedSuffix = ".20261001T170405Z"

// backupFixture is a hooks dir opened as a root, with helpers to seed files.
type backupFixture struct {
	t    *testing.T
	dir  string
	root *os.Root
}

func newBackupFixture(t *testing.T) *backupFixture {
	t.Helper()
	dir := t.TempDir()
	root, err := hooksRootForInstall(dir)
	if err != nil {
		t.Fatalf("open hooks root: %v", err)
	}
	return &backupFixture{t: t, dir: dir, root: root}
}

func (f *backupFixture) write(name, content string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.dir, name), []byte(content), 0o755); err != nil {
		f.t.Fatal(err)
	}
}

func (f *backupFixture) symlink(name, target string) {
	f.t.Helper()
	if err := os.Symlink(target, filepath.Join(f.dir, name)); err != nil {
		f.t.Skipf("symlinks unavailable: %v", err)
	}
}

func (f *backupFixture) read(name string) string {
	f.t.Helper()
	data, err := os.ReadFile(filepath.Join(f.dir, name))
	if err != nil {
		f.t.Fatalf("read %s: %v", name, err)
	}
	return string(data)
}

func (f *backupFixture) exists(name string) bool {
	_, err := os.Lstat(filepath.Join(f.dir, name))
	return err == nil
}

func (f *backupFixture) prepare(name string) hookBackupResult {
	f.t.Helper()
	res, err := prepareHookBackup(f.root, name, rotationClock)
	if err != nil {
		f.t.Fatalf("prepareHookBackup(%s): %v", name, err)
	}
	return res
}

const (
	userHookV1 = "#!/bin/sh\necho v1\n"
	userHookV2 = "#!/bin/sh\necho v2\n"
	ourHook    = "#!/bin/sh\n# " + entireHookMarker + "\nentire hooks git pre-push \"$1\"\n"
)

// A hook that changed since Entire backed it up (a checkout restored it, the
// user edited it) is the one the user wants now: it becomes the backup, and the
// older backup is kept rather than the new hook being overwritten.
func TestPrepareHookBackup_ChangedHookRotatesOlderBackup(t *testing.T) {
	t.Parallel()
	f := newBackupFixture(t)
	f.write("pre-push", userHookV2)
	f.write("pre-push"+backupSuffix, userHookV1)

	res := f.prepare("pre-push")
	if res.Action != backupRotated || !res.Chain {
		t.Fatalf("result = %+v, want backupRotated with chain", res)
	}
	if res.OlderCopy != "pre-push"+backupSuffix+rotatedSuffix {
		t.Errorf("OlderCopy = %q", res.OlderCopy)
	}
	if got := f.read("pre-push" + backupSuffix); got != userHookV2 {
		t.Errorf("backup = %q, want the current hook", got)
	}
	if got := f.read(res.OlderCopy); got != userHookV1 {
		t.Errorf("older copy = %q, want the previous backup", got)
	}
}

func TestPrepareHookBackup_SameContentReplacesWithoutRotation(t *testing.T) {
	t.Parallel()
	f := newBackupFixture(t)
	f.write("pre-push", userHookV1)
	f.write("pre-push"+backupSuffix, userHookV1)

	res := f.prepare("pre-push")
	if res.Action != backupReplacedSame || !res.Chain {
		t.Fatalf("result = %+v, want backupReplacedSame with chain", res)
	}
	if f.exists("pre-push" + backupSuffix + rotatedSuffix) {
		t.Error("identical hook should not create an older copy")
	}
}

func TestPrepareHookBackup_SymlinkedHookIsMovedNotFollowed(t *testing.T) {
	t.Parallel()
	f := newBackupFixture(t)
	target := filepath.Join(t.TempDir(), "shared-hook")
	if err := os.WriteFile(target, []byte(userHookV2), 0o755); err != nil {
		t.Fatal(err)
	}
	f.symlink("pre-push", target)
	f.write("pre-push"+backupSuffix, userHookV1)

	res := f.prepare("pre-push")
	if res.Action != backupRotated {
		t.Fatalf("result = %+v, want backupRotated", res)
	}
	link, err := os.Readlink(filepath.Join(f.dir, "pre-push"+backupSuffix))
	if err != nil || link != target {
		t.Errorf("backup should be the moved link to %s, got %q (%v)", target, link, err)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != userHookV2 {
		t.Error("link target must not be modified")
	}
}

func TestPrepareHookBackup_SymlinkedBackupCountsAsDifferent(t *testing.T) {
	t.Parallel()
	f := newBackupFixture(t)
	target := filepath.Join(t.TempDir(), "old-hook")
	if err := os.WriteFile(target, []byte(userHookV1), 0o755); err != nil {
		t.Fatal(err)
	}
	f.write("pre-push", userHookV1)
	f.symlink("pre-push"+backupSuffix, target)

	if res := f.prepare("pre-push"); res.Action != backupRotated {
		t.Fatalf("result = %+v, want backupRotated", res)
	}
}

func TestPrepareHookBackup_RotationNameCollisionGetsSuffix(t *testing.T) {
	t.Parallel()
	f := newBackupFixture(t)
	f.write("pre-push", userHookV2)
	f.write("pre-push"+backupSuffix, userHookV1)
	f.write("pre-push"+backupSuffix+rotatedSuffix, "#!/bin/sh\necho older\n")

	res := f.prepare("pre-push")
	if want := "pre-push" + backupSuffix + rotatedSuffix + "-2"; res.OlderCopy != want {
		t.Fatalf("OlderCopy = %q, want %q", res.OlderCopy, want)
	}
	if got := f.read("pre-push" + backupSuffix + rotatedSuffix); got != "#!/bin/sh\necho older\n" {
		t.Error("existing rotated copy was overwritten")
	}
}

// pre-commit keeps the hook it replaces as <hook>.legacy and runs it first.
// When that is Entire's hook, rotating would make it chain into pre-commit's own
// wrapper, and pre-commit then fails every commit ("installed in migration
// mode"). Keep the pre-existing replace behaviour there.
func TestPrepareHookBackup_EntireLegacyCopySkipsRotation(t *testing.T) {
	t.Parallel()
	f := newBackupFixture(t)
	f.write("commit-msg", "#!/usr/bin/env bash\n# File generated by pre-commit\n")
	f.write("commit-msg"+backupSuffix, userHookV1)
	f.write("commit-msg.legacy", ourHook)

	res := f.prepare("commit-msg")
	if res.Action != backupReplacedLegacy {
		t.Fatalf("result = %+v, want backupReplacedLegacy", res)
	}
	if got := f.read("commit-msg" + backupSuffix); got != userHookV1 {
		t.Errorf("backup = %q, want it untouched", got)
	}
	if f.exists("commit-msg" + backupSuffix + rotatedSuffix) {
		t.Error("no older copy should be created in pre-commit migration state")
	}
}

// A backup that carries Entire's marker would make the chain call Entire's hook
// from inside itself, forever. It can appear when two processes install at
// once; never chain to it.
func TestPrepareHookBackup_MarkerCarryingBackupIsNotChained(t *testing.T) {
	t.Parallel()
	f := newBackupFixture(t)
	f.write("pre-push", ourHook)
	f.write("pre-push"+backupSuffix, ourHook)

	res := f.prepare("pre-push")
	if res.Chain {
		t.Fatalf("result = %+v, want no chain to a marker-carrying backup", res)
	}
	if !res.SelfBackup {
		t.Error("SelfBackup should be reported so the install can warn")
	}
	if got := f.read("pre-push" + backupSuffix); got != ourHook {
		t.Error("marker-carrying backup should be left in place")
	}
}

func TestRotatedHookCopies(t *testing.T) {
	t.Parallel()
	f := newBackupFixture(t)
	f.write("pre-push"+backupSuffix+rotatedSuffix, userHookV1)
	f.write("commit-msg"+backupSuffix+rotatedSuffix+"-2", userHookV1)
	f.write("pre-commit"+backupSuffix+rotatedSuffix, userHookV1) // not a managed hook
	f.write("pre-push"+backupSuffix, userHookV1)                 // the live backup

	got, err := rotatedHookCopies(f.root)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"commit-msg" + backupSuffix + rotatedSuffix + "-2", "pre-push" + backupSuffix + rotatedSuffix}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("rotatedHookCopies = %v, want %v", got, want)
	}
}
