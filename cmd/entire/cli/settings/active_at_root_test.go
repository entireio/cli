package settings

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestIsActiveAtRoot is the property a settings-file presence check cannot
// provide.
//
// Presence only Lstats for a file, so a repo the user explicitly disabled still
// answers true — the file is still there, it just says enabled:false.
// binding_adopt.go used presence as what its own comment called "an absolute
// veto" on replicating a session into a foreign repo, which meant the veto
// never fired: Entire would write session state and checkpoints into a
// repository the user had turned it off in.
//
// These are the repo-level cases, which are the whole answer on this branch.
// The user-global tier's cases belong to the classifier-backed implementation
// that replaces this one; they are written against IsActiveAtRoot too, so the
// two tables union rather than compete.
func TestIsActiveAtRoot(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		write    map[string]string // relative path -> contents
		expected bool
	}{
		{
			name:     "never set up",
			expected: false,
		},
		{
			name:     "enabled in the project file",
			write:    map[string]string{EntireSettingsFile: `{"enabled":true}`},
			expected: true,
		},
		{
			// The case the whole helper exists for.
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
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			for rel, body := range tc.write {
				path := filepath.Join(root, rel)
				if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			if got := IsActiveAtRoot(context.Background(), root); got != tc.expected {
				t.Errorf("IsActiveAtRoot() = %v, want %v", got, tc.expected)
			}
		})
	}
}
