package settings

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// TestIsActiveAtRoot covers the at-root predicate, which had no test and no
// caller: it exists for a process deciding what it may do to a repository it is
// not running in (session binding, whose evidence and adoption paths are its
// first callers).
//
// Both halves need their qualifiers, and each half is the one a naive check
// gets wrong. A settings file that says enabled:false is a veto, so presence
// cannot stand in for the repo half — `entire disable` leaves the file there.
// And a globally tracked repo has no settings file at all, so presence misses
// the tier half completely.
func TestIsActiveAtRoot(t *testing.T) {
	// No t.Parallel: each case sets ENTIRE_CONFIG_DIR for the user-global tier.
	for _, tc := range []struct {
		name         string
		userSettings string            // user-global settings.json body, "" for none
		write        map[string]string // repo-relative path -> contents
		notARepo     bool              // leave the root bare instead of git-initializing it
		expected     bool
	}{
		{
			name:     "never set up, tier off",
			expected: false,
		},
		{
			name:     "enabled in the project file",
			write:    map[string]string{EntireSettingsFile: `{"enabled":true}`},
			expected: true,
		},
		{
			// The case the disable veto exists for.
			name:     "explicitly disabled in the project file",
			write:    map[string]string{EntireSettingsFile: `{"enabled":false}`},
			expected: false,
		},
		{
			name:     "enabled only in the local file",
			write:    map[string]string{EntireSettingsLocalFile: `{"enabled":true}`},
			expected: true,
		},
		{
			// The local layer is the developer's own, so its disable wins over
			// a project file that enables.
			name: "local disable overrides a project enable",
			write: map[string]string{
				EntireSettingsFile:      `{"enabled":true}`,
				EntireSettingsLocalFile: `{"enabled":false}`,
			},
			expected: false,
		},
		{
			// Fail closed: an unreadable repo is not a repo we may write to.
			name:     "malformed settings are not active",
			write:    map[string]string{EntireSettingsFile: `{"enabled":true`},
			expected: false,
		},
		{
			// The half no settings file can express.
			name:         "no repo setup, carried by the tier",
			userSettings: `{"global":{"enabled":true}}`,
			expected:     true,
		},
		{
			// The tier's own qualifier.
			name:         "excluded from the tier",
			userSettings: `{"global":{"enabled":true,"exclude_paths":["__ROOT__"]}}`,
			expected:     false,
		},
		{
			// A repo-level disable is a veto over the tier too.
			name:         "explicitly disabled beats the tier",
			userSettings: `{"global":{"enabled":true}}`,
			write:        map[string]string{EntireSettingsFile: `{"enabled":false}`},
			expected:     false,
		},
		{
			// Nothing to activate outside a repository, whatever the tier says.
			name:         "not a repository",
			userSettings: `{"global":{"enabled":true}}`,
			notARepo:     true,
			expected:     false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if !tc.notARepo {
				testutil.InitRepo(t, root)
			}
			for rel, body := range tc.write {
				path := filepath.Join(root, rel)
				if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			configDir := t.TempDir()
			t.Setenv("ENTIRE_CONFIG_DIR", configDir)
			if tc.userSettings != "" {
				body := strings.ReplaceAll(tc.userSettings, "__ROOT__", root)
				if err := os.WriteFile(filepath.Join(configDir, UserSettingsFileName),
					[]byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			if got := IsActiveAtRoot(context.Background(), root); got != tc.expected {
				t.Errorf("IsActiveAtRoot() = %v, want %v", got, tc.expected)
			}
		})
	}
}
