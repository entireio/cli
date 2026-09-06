package worktreedir

import (
	"path/filepath"
	"testing"
)

func TestName(t *testing.T) {
	t.Parallel()

	root := filepath.Join(string(filepath.Separator), "repo")

	t.Run("relative name is kept", func(t *testing.T) {
		t.Parallel()
		got, err := Name(root, "api/src/types.ts")
		if err != nil {
			t.Fatalf("Name: %v", err)
		}
		if got != "api/src/types.ts" {
			t.Errorf("got %q", got)
		}
	})

	t.Run("absolute path inside the worktree is relativized", func(t *testing.T) {
		t.Parallel()
		got, err := Name(root, filepath.Join(root, "api", "file.ts"))
		if err != nil {
			t.Fatalf("Name: %v", err)
		}
		if got != "api/file.ts" {
			t.Errorf("got %q", got)
		}
	})

	for _, p := range []string{
		"",
		".",
		"../escape",
		"api/../../escape",
	} {
		t.Run("rejects "+p, func(t *testing.T) {
			t.Parallel()
			if got, err := Name(root, p); err == nil {
				t.Errorf("Name(%q) = %q, want error", p, got)
			}
		})
	}

	// "C:foo" is drive-relative on Windows: separator-free, and IsAbs reports
	// false, so pairing IsAbs with VolumeName is what keeps it off the name
	// branch — filepath.Join would otherwise drop the base directory. On Unix
	// the same string is an ordinary relative filename and must be accepted,
	// which is why this asserts each platform's answer rather than skipping.
	t.Run("drive-relative path", func(t *testing.T) {
		t.Parallel()
		got, err := Name(root, "C:foo")
		if filepath.VolumeName("C:foo") != "" {
			if err == nil {
				t.Errorf("Name(\"C:foo\") = %q, want error on a volume-aware platform", got)
			}
			return
		}
		if err != nil || got != "C:foo" {
			t.Errorf("Name(\"C:foo\") = %q, %v; want it kept as a plain filename", got, err)
		}
	})

	t.Run("rejects an absolute path outside the worktree", func(t *testing.T) {
		t.Parallel()
		outside := filepath.Join(string(filepath.Separator), "elsewhere", "file.ts")
		if got, err := Name(root, outside); err == nil {
			t.Errorf("Name(%q) = %q, want error", outside, got)
		}
	})
}
