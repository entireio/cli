package flock

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/osroot"
)

func TestAcquireIn_CreatesTheLockFileAndReleases(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	root, err := os.OpenRoot(dir) //nolint:noinlineerr // test fixture: the root's base is the temp dir itself
	if err != nil {
		t.Fatalf("OpenRoot() error = %v", err)
	}
	t.Cleanup(func() { _ = root.Close() })

	release, err := AcquireIn(root, "state.lock")
	if err != nil {
		t.Fatalf("AcquireIn() error = %v", err)
	}
	release()

	if _, err := os.Lstat(filepath.Join(dir, "state.lock")); err != nil {
		t.Fatalf("lock file should exist after acquire: %v", err)
	}

	// Taking it again reaches the plain-open fallback, since the file now
	// exists. That is the step a planted link would be followed through.
	release, err = AcquireIn(root, "state.lock")
	if err != nil {
		t.Fatalf("second AcquireIn() error = %v", err)
	}
	release()
}

// A lock file is opened O_RDWR and its contents are immaterial, so following a
// link here would not leak anything by itself. It is refused because os.Root
// stops only the links that escape it, and every other opener in this tree
// stopped following the ones that do not.
func TestAcquireIn_RefusesASymlinkedLockPath(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "windows" {
		t.Skip("symlink creation is not generally available on Windows")
	}

	for _, tc := range []struct{ name, plant string }{
		{"a link at the lock file itself", "state.lock"},
		{"a link at the lock file's parent", "locks"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			victimDir := filepath.Join(dir, "victim")
			if err := os.MkdirAll(victimDir, 0o750); err != nil {
				t.Fatal(err)
			}
			victim := filepath.Join(victimDir, "state.lock")
			if err := os.WriteFile(victim, []byte("not a lock"), 0o600); err != nil {
				t.Fatal(err)
			}

			name, target := "state.lock", "victim/state.lock"
			if tc.plant == "locks" {
				name, target = "locks/state.lock", "victim"
			}
			// A relative link that stays inside the root. os.Root permits exactly
			// this shape, which is why refusing it has to happen here.
			if err := os.Symlink(target, filepath.Join(dir, tc.plant)); err != nil {
				t.Skipf("symlink not supported: %v", err)
			}

			root, err := os.OpenRoot(dir) //nolint:noinlineerr // test fixture: the root's base is the temp dir itself
			if err != nil {
				t.Fatalf("OpenRoot() error = %v", err)
			}
			t.Cleanup(func() { _ = root.Close() })

			release, err := AcquireIn(root, name)
			if err == nil {
				release()
				t.Fatal("AcquireIn() through a symlink should be refused")
			}
			if !errors.Is(err, osroot.ErrSymlinkedPath) {
				t.Fatalf("AcquireIn() error = %v, want %v", err, osroot.ErrSymlinkedPath)
			}

			data, readErr := os.ReadFile(victim)
			if readErr != nil {
				t.Fatalf("the link target must be left alone: %v", readErr)
			}
			if string(data) != "not a lock" {
				t.Fatalf("the link target was written through, contents = %q", data)
			}
		})
	}
}
