package settings

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// newHookPathRepo returns the absolute project and local settings paths under a
// fresh temp root.
//
// Goes through loadMergedSettings directly rather than Load, matching
// opf_command_trust_test.go: Load resolves clone preferences from the cwd, so a
// Load-based test must t.Chdir (blocking t.Parallel) and pays a `git rev-parse`
// per case. loadMergedSettings takes explicit paths and covers both code sites
// under test.
func newHookPathRepo(t *testing.T) (projectPath, localPath string) {
	t.Helper()
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".entire"), 0o755))
	return filepath.Join(root, EntireSettingsFile), filepath.Join(root, EntireSettingsLocalFile)
}

// TestAbsoluteGitHookPath_ScopeGate pins where the setting is honored.
//
// It rewrites every generated git hook to name one machine's binary path, so a
// committed value would impose that on everyone who clones — pinning their hooks
// to whichever binary they happened to run, which is more brittle than resolving
// through PATH.
func TestAbsoluteGitHookPath_ScopeGate(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name          string
		project       string
		local         string
		want          bool
		wantRejection bool
	}{
		{
			name:          "committed project file is ignored",
			project:       `{"enabled": true, "absolute_git_hook_path": true}`,
			want:          false,
			wantRejection: true,
		},
		{
			name:    "local override is honored",
			project: `{"enabled": true}`,
			local:   `{"absolute_git_hook_path": true}`,
			want:    true,
		},
		{
			// The local file is the authority, so it can also turn it back off.
			name:          "local override wins over the project file",
			project:       `{"enabled": true, "absolute_git_hook_path": true}`,
			local:         `{"absolute_git_hook_path": false}`,
			want:          false,
			wantRejection: true,
		},
		{
			// The project value was redundant, not overridden: reporting it as
			// ignored beside a hook that is in fact pinned reads as a
			// contradiction.
			name:          "no rejection reported when the local file enables it anyway",
			project:       `{"enabled": true, "absolute_git_hook_path": true}`,
			local:         `{"absolute_git_hook_path": true}`,
			want:          true,
			wantRejection: false,
		},
		{
			// A project value of false is a no-op, so there is nothing to report.
			name:    "an explicit false in the project file is not a rejection",
			project: `{"enabled": true, "absolute_git_hook_path": false}`,
			want:    false,
		},
		{
			name:    "absent everywhere",
			project: `{"enabled": true}`,
			want:    false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			projectPath, localPath := newHookPathRepo(t)
			require.NoError(t, os.WriteFile(projectPath, []byte(tc.project), 0o600))
			if tc.local != "" {
				require.NoError(t, os.WriteFile(localPath, []byte(tc.local), 0o600))
			}

			s, err := loadMergedSettings(t.Context(), projectPath, "", localPath)
			require.NoError(t, err)

			require.Equal(t, tc.want, s.AbsoluteGitHookPath, "effective absolute_git_hook_path")
			require.Equal(t, tc.wantRejection, s.AbsoluteGitHookPathRejection() != "",
				"rejection recorded (reason %q)", s.AbsoluteGitHookPathRejection())
		})
	}
}

// TestAbsoluteGitHookPath_RejectionDoesNotSerialize guards against writing the
// dropped value back to disk as though the user had unset it.
func TestAbsoluteGitHookPath_RejectionDoesNotSerialize(t *testing.T) {
	t.Parallel()

	const body = `{"enabled": true, "absolute_git_hook_path": true}`
	projectPath, localPath := newHookPathRepo(t)
	require.NoError(t, os.WriteFile(projectPath, []byte(body), 0o600))

	s, err := loadMergedSettings(t.Context(), projectPath, "", localPath)
	require.NoError(t, err)
	require.NotEmpty(t, s.AbsoluteGitHookPathRejection(), "expected a rejection to be recorded")

	after, err := os.ReadFile(projectPath)
	require.NoError(t, err)
	require.Equal(t, body, string(after), "loading must not rewrite the settings file")
}
