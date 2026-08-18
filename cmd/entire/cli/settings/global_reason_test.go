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
	cases := []struct {
		name         string
		repoSettings string // "" = no repo-level file
		userSettings string // "" = no user file
		excludeSelf  bool
		wantActive   bool
		wantReason   InactiveReason
	}{
		{"repo enabled is active with no reason", `{"enabled":true}`, "", false, true, InactiveReasonNone},
		// Even with the global tier on: repo-level setup makes its answer final.
		{"repo disabled is the explicit veto", `{"enabled":false}`, `{"global":{"enabled":true}}`, false, false, InactiveReasonRepoDisabled},
		{"no setup and global unconfigured is global-off", "", "", false, false, InactiveReasonGlobalOff},
		{"no setup and global disabled is global-off", "", `{"global":{"enabled":false}}`, false, false, InactiveReasonGlobalOff},
		{"global on is active", "", `{"global":{"enabled":true}}`, false, true, InactiveReasonNone},
		{"excluded worktree reads as excluded", "", "", true, false, InactiveReasonGlobalExcluded},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			testutil.InitRepo(t, dir)
			cfg := t.TempDir()
			t.Setenv("ENTIRE_CONFIG_DIR", cfg)
			t.Cleanup(ClearGlobalModeCache)
			if c.repoSettings != "" {
				if err := os.MkdirAll(filepath.Join(dir, ".entire"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, ".entire", "settings.json"), []byte(c.repoSettings), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			userSettings := c.userSettings
			if c.excludeSelf {
				// t.Chdir resolves symlinks on macOS (/var → /private/var), so
				// build the exclude pattern from the resolved path.
				resolved, err := filepath.EvalSymlinks(dir)
				if err != nil {
					t.Fatal(err)
				}
				userSettings = `{"global":{"enabled":true,"exclude_paths":["` + filepath.ToSlash(resolved) + `"]}}`
			}
			if userSettings != "" {
				writeUserSettings(t, cfg, userSettings)
			}
			t.Chdir(dir)
			active, reason := IsActiveForRepoWithReason(t.Context())
			if active != c.wantActive || reason != c.wantReason {
				t.Fatalf("got active=%v reason=%v, want active=%v reason=%v", active, reason, c.wantActive, c.wantReason)
			}
		})
	}
}
