package settings

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestAbsoluteGitHookPath_ScopeGate pins where the setting is honored.
//
// It rewrites every generated git hook to name one machine's binary path, so a
// committed value would impose that on everyone who clones — pinning their hooks
// to whichever binary they happened to run, which is more brittle than resolving
// through PATH.
func TestAbsoluteGitHookPath_ScopeGate(t *testing.T) {
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
			// A project value of false is a no-op, so there is nothing to report.
			name:    "an explicit false in the project file is not a rejection",
			project: `{"enabled": true, "absolute_git_hook_path": false}`,
			want:    false,
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
			name:    "absent everywhere",
			project: `{"enabled": true}`,
			want:    false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Chdir(dir)
			if err := os.MkdirAll(filepath.Join(dir, ".entire"), 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, EntireSettingsFile), []byte(tc.project), 0o600); err != nil {
				t.Fatal(err)
			}
			if tc.local != "" {
				if err := os.WriteFile(filepath.Join(dir, EntireSettingsLocalFile), []byte(tc.local), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			s, err := Load(context.Background())
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if s.AbsoluteGitHookPath != tc.want {
				t.Errorf("AbsoluteGitHookPath = %v, want %v", s.AbsoluteGitHookPath, tc.want)
			}
			gotRejection := s.AbsoluteGitHookPathRejection() != ""
			if gotRejection != tc.wantRejection {
				t.Errorf("rejection recorded = %v, want %v (reason %q)", gotRejection, tc.wantRejection, s.AbsoluteGitHookPathRejection())
			}
		})
	}
}

// TestAbsoluteGitHookPath_RejectionDoesNotSerialize guards against writing the
// dropped value back to disk as though the user had unset it.
func TestAbsoluteGitHookPath_RejectionDoesNotSerialize(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.MkdirAll(filepath.Join(dir, ".entire"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, EntireSettingsFile),
		[]byte(`{"enabled": true, "absolute_git_hook_path": true}`), 0o600); err != nil {
		t.Fatal(err)
	}

	s, err := Load(context.Background())
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if s.AbsoluteGitHookPathRejection() == "" {
		t.Fatal("expected a rejection to have been recorded")
	}
	data, err := os.ReadFile(filepath.Join(dir, EntireSettingsFile))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"enabled": true, "absolute_git_hook_path": true}` {
		t.Errorf("Load must not rewrite the settings file, got:\n%s", data)
	}
}
