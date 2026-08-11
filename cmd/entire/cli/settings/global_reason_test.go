package settings

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// No t.Parallel in this file: every subtest uses t.Chdir and t.Setenv.

// TestIsActiveForRepoWithReason pins the reason variant of the hook gate. The
// SessionStart wrong-folder notice keys off these reasons, so the mapping
// matters: repo-level disable must read as the explicit veto (silence), and
// the two global-tier inactive shapes must stay distinguishable.
func TestIsActiveForRepoWithReason(t *testing.T) {
	newRepo := func(t *testing.T) string {
		t.Helper()
		dir := t.TempDir()
		testutil.InitRepo(t, dir)
		return dir
	}
	writeRepoSettings := func(t *testing.T, dir, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(dir, ".entire"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".entire", "settings.json"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("repo enabled is active with no reason", func(t *testing.T) {
		dir := newRepo(t)
		t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
		t.Cleanup(ClearGlobalModeCache)
		writeRepoSettings(t, dir, `{"enabled":true}`)
		t.Chdir(dir)
		active, reason := IsActiveForRepoWithReason(t.Context())
		if !active || reason != InactiveReasonNone {
			t.Fatalf("got active=%v reason=%v, want active with InactiveReasonNone", active, reason)
		}
	})

	t.Run("repo disabled is the explicit veto", func(t *testing.T) {
		dir := newRepo(t)
		cfg := t.TempDir()
		t.Setenv("ENTIRE_CONFIG_DIR", cfg)
		t.Cleanup(ClearGlobalModeCache)
		// Even with the global tier on: repo-level setup makes its answer final.
		writeUserSettings(t, cfg, `{"global":{"enabled":true}}`)
		writeRepoSettings(t, dir, `{"enabled":false}`)
		t.Chdir(dir)
		active, reason := IsActiveForRepoWithReason(t.Context())
		if active || reason != InactiveReasonRepoDisabled {
			t.Fatalf("got active=%v reason=%v, want InactiveReasonRepoDisabled", active, reason)
		}
	})

	t.Run("no setup and global unconfigured is global-off", func(t *testing.T) {
		dir := newRepo(t)
		t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
		t.Cleanup(ClearGlobalModeCache)
		t.Chdir(dir)
		active, reason := IsActiveForRepoWithReason(t.Context())
		if active || reason != InactiveReasonGlobalOff {
			t.Fatalf("got active=%v reason=%v, want InactiveReasonGlobalOff", active, reason)
		}
	})

	t.Run("no setup and global disabled is global-off", func(t *testing.T) {
		dir := newRepo(t)
		cfg := t.TempDir()
		t.Setenv("ENTIRE_CONFIG_DIR", cfg)
		t.Cleanup(ClearGlobalModeCache)
		writeUserSettings(t, cfg, `{"global":{"enabled":false}}`)
		t.Chdir(dir)
		active, reason := IsActiveForRepoWithReason(t.Context())
		if active || reason != InactiveReasonGlobalOff {
			t.Fatalf("got active=%v reason=%v, want InactiveReasonGlobalOff", active, reason)
		}
	})

	t.Run("global on is active", func(t *testing.T) {
		dir := newRepo(t)
		cfg := t.TempDir()
		t.Setenv("ENTIRE_CONFIG_DIR", cfg)
		t.Cleanup(ClearGlobalModeCache)
		writeUserSettings(t, cfg, `{"global":{"enabled":true}}`)
		t.Chdir(dir)
		active, reason := IsActiveForRepoWithReason(t.Context())
		if !active || reason != InactiveReasonNone {
			t.Fatalf("got active=%v reason=%v, want active with InactiveReasonNone", active, reason)
		}
	})

	t.Run("excluded worktree reads as excluded", func(t *testing.T) {
		dir := newRepo(t)
		// t.Chdir resolves symlinks on macOS (/var → /private/var), so build
		// the exclude pattern from the resolved path or it never matches.
		resolved, err := filepath.EvalSymlinks(dir)
		if err != nil {
			t.Fatal(err)
		}
		cfg := t.TempDir()
		t.Setenv("ENTIRE_CONFIG_DIR", cfg)
		t.Cleanup(ClearGlobalModeCache)
		writeUserSettings(t, cfg,
			`{"global":{"enabled":true,"exclude_paths":["`+filepath.ToSlash(resolved)+`"]}}`)
		t.Chdir(dir)
		active, reason := IsActiveForRepoWithReason(t.Context())
		if active || reason != InactiveReasonGlobalExcluded {
			t.Fatalf("got active=%v reason=%v, want InactiveReasonGlobalExcluded", active, reason)
		}
	})
}
