package strategy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Entire owns entire-lefthook.yml outright and adds exactly one extends entry
// to the local config, so it never merges hook sections into a file the user
// owns. One table over the local-config matrix.
func TestEnsureLefthookExtends(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// existing local config, "" meaning absent
		localName string
		local     string
		wantErr   error
		// the local config must still contain these after the write
		wantKept []string
	}{
		{
			name:      "creates the local config when absent",
			localName: "",
		},
		{
			name:      "adds one entry to an existing yaml local config",
			localName: "lefthook-local.yml",
			local:     "pre-commit:\n  commands:\n    mine:\n      run: echo hi\n",
			wantKept:  []string{"mine", "echo hi"},
		},
		{
			name:      "appends to an existing extends sequence",
			localName: "lefthook-local.yml",
			local:     "extends:\n  - user-extra.yml\n",
			wantKept:  []string{"user-extra.yml"},
		},
		{
			name:      "preserves user comments",
			localName: "lefthook-local.yml",
			local:     "# keep me\npre-commit:\n  commands:\n    mine:\n      run: true\n",
			wantKept:  []string{"# keep me"},
		},
		{
			// lefthook-local.yml shadows lefthook-local.toml, so creating a
			// .yml beside a .toml would silently disable the user's config.
			name:      "refuses a non-yaml local config rather than shadowing it",
			localName: "lefthook-local.toml",
			local:     "[pre-commit.commands.mine]\nrun = \"echo hi\"\n",
			wantErr:   errLefthookLocalConfigUnwritable,
		},
		{
			name:      "detects a dotted local config name",
			localName: ".lefthook-local.yml",
			local:     "pre-commit:\n  commands:\n    mine:\n      run: true\n",
			wantKept:  []string{"mine"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if tc.localName != "" {
				require.NoError(t, os.WriteFile(filepath.Join(dir, tc.localName), []byte(tc.local), 0o644))
			}
			root, err := os.OpenRoot(dir)
			require.NoError(t, err)
			defer root.Close()

			name, err := ensureLefthookExtends(root)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				// nothing may be written when we refuse
				entries, readErr := os.ReadDir(dir)
				require.NoError(t, readErr)
				require.Len(t, entries, 1, "refusal must not create a config")
				return
			}
			require.NoError(t, err)

			want := tc.localName
			if want == "" {
				want = lefthookLocalConfigName
			}
			require.Equal(t, want, name, "must write into the config lefthook actually reads")

			data, readErr := os.ReadFile(filepath.Join(dir, name))
			require.NoError(t, readErr)
			got := string(data)
			require.Contains(t, got, entireLefthookConfigName, "the extends entry must be present")
			require.Equal(t, 1, strings.Count(got, entireLefthookConfigName), "exactly one entry")
			for _, keep := range tc.wantKept {
				require.Contains(t, got, keep, "user content must survive")
			}

			// Idempotent: a second call changes nothing.
			_, err = ensureLefthookExtends(root)
			require.NoError(t, err)
			again, readErr := os.ReadFile(filepath.Join(dir, name))
			require.NoError(t, readErr)
			require.Equal(t, got, string(again), "repeat must be a no-op")
		})
	}
}

// Entire no longer reads the main config, so its format and contents are
// irrelevant — including the extends and remotes keys the old code refused.
func TestEnsureLefthookExtends_IgnoresMainConfig(t *testing.T) {
	t.Parallel()
	for _, main := range []struct{ name, body string }{
		{"lefthook.yml", "pre-commit:\n  commands:\n    a:\n      run: true\n"},
		{"lefthook.toml", "[pre-commit.commands.a]\nrun = \"true\"\n"},
		{"lefthook.json", `{"pre-commit":{"commands":{"a":{"run":"true"}}}}`},
		{"lefthook.yml with extends", "extends:\n  - other.yml\n"},
		{"lefthook.yml with remotes", "remotes:\n  - git_url: https://example.test/x.git\n"},
	} {
		t.Run(main.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			fname := strings.Fields(main.name)[0]
			require.NoError(t, os.WriteFile(filepath.Join(dir, fname), []byte(main.body), 0o644))
			root, err := os.OpenRoot(dir)
			require.NoError(t, err)
			defer root.Close()

			_, err = ensureLefthookExtends(root)
			require.NoError(t, err, "the main config must never be read")
		})
	}
}
