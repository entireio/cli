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

// TestAbsoluteGitHookPath_ProjectScopeIsDeprecatedNotDropped pins the staged
// rollout.
//
// The setting rewrites every generated git hook to name one machine's binary
// path, so a committed value imposes that on everyone who clones. It is still
// honored from there for now: the only way to enable the feature used to write it
// exclusively to the project file, so dropping it immediately would unpin the
// hooks of everyone who ever used it — silently, in the GUI git client that
// cannot find `entire` on PATH. So the committed value keeps working AND reports
// a deprecation, and the deprecation goes quiet once the local file sets the key.
func TestAbsoluteGitHookPath_ProjectScopeIsDeprecatedNotDropped(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name           string
		project        string
		local          string
		want           bool
		wantDeprecated bool
	}{
		{
			// Still honored — nobody's hooks change — but warned about.
			name:           "committed project file is honored and deprecated",
			project:        `{"enabled": true, "absolute_git_hook_path": true}`,
			want:           true,
			wantDeprecated: true,
		},
		{
			name:    "local override is honored with no warning",
			project: `{"enabled": true}`,
			local:   `{"absolute_git_hook_path": true}`,
			want:    true,
		},
		{
			// The local file is the authority, so it can turn it back off. No
			// warning: the user has already stated their choice locally.
			name:    "local false wins over a committed true",
			project: `{"enabled": true, "absolute_git_hook_path": true}`,
			local:   `{"absolute_git_hook_path": false}`,
			want:    false,
		},
		{
			// Post-migration state: the committed value is redundant, so warning
			// about it would be noise the user cannot act on further.
			name:    "no warning once the local file sets the key",
			project: `{"enabled": true, "absolute_git_hook_path": true}`,
			local:   `{"absolute_git_hook_path": true}`,
			want:    true,
		},
		{
			// A committed false pins nothing, so there is nothing to warn about.
			name:    "an explicit false in the project file is not deprecated",
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
			require.Equal(t, tc.wantDeprecated, s.AbsoluteGitHookPathDeprecation() != "",
				"deprecation recorded (notice %q)", s.AbsoluteGitHookPathDeprecation())
		})
	}
}

// TestAbsoluteGitHookPath_DeprecationDoesNotSerialize guards against the notice
// leaking into a written settings file, and against the loader rewriting a file
// it only read.
func TestAbsoluteGitHookPath_DeprecationDoesNotSerialize(t *testing.T) {
	t.Parallel()

	const body = `{"enabled": true, "absolute_git_hook_path": true}`
	projectPath, localPath := newHookPathRepo(t)
	require.NoError(t, os.WriteFile(projectPath, []byte(body), 0o600))

	s, err := loadMergedSettings(t.Context(), projectPath, "", localPath)
	require.NoError(t, err)
	require.NotEmpty(t, s.AbsoluteGitHookPathDeprecation(), "expected a deprecation to be recorded")

	after, err := os.ReadFile(projectPath)
	require.NoError(t, err)
	require.Equal(t, body, string(after), "loading must not rewrite the settings file")
}
