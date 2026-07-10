package telemetry

import (
	"os"
	"path/filepath"
	"testing"
)

// TestResolveDetachedDir covers the working-directory resolver behind
// SpawnDetached: an empty or missing dir falls back to the platform default,
// while an existing directory is preserved so the pricing refresh can root the
// worker in the project directory.
func TestResolveDetachedDir(t *testing.T) {
	t.Parallel()

	existing := t.TempDir()
	file := filepath.Join(existing, "afile")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	missing := filepath.Join(existing, "does-not-exist")

	cases := []struct {
		name     string
		dir      string
		fallback string
		want     string
	}{
		{"empty uses unix fallback (analytics default)", "", "/", "/"},
		{"empty uses windows fallback", "", os.TempDir(), os.TempDir()},
		{"existing dir preserved", existing, "/", existing},
		{"missing dir falls back", missing, "/", "/"},
		{"file path falls back", file, "/", "/"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := resolveDetachedDir(tc.dir, tc.fallback); got != tc.want {
				t.Errorf("resolveDetachedDir(%q, %q) = %q, want %q", tc.dir, tc.fallback, got, tc.want)
			}
		})
	}
}
