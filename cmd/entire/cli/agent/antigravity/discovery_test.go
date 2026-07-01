package antigravity

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func writeAgySkill(t *testing.T, scopeDir, name, description string) {
	t.Helper()
	dir := filepath.Join(scopeDir, name)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body := "---\nname: " + name + "\ndescription: " + description + "\n---\nbody\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func TestDiscoverReviewSkills_ScansGlobalAndSharedScopes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Chdir(t.TempDir()) // non-repo cwd → project scope skipped deterministically

	writeAgySkill(t, filepath.Join(home, ".gemini", "antigravity-cli", "skills"), "code-review", "Review PRs.")
	writeAgySkill(t, filepath.Join(home, ".gemini", "skills"), "security-audit", "Audit deps.")
	writeAgySkill(t, filepath.Join(home, ".gemini", "skills"), "formatter", "Format code.")

	got, err := (&AntigravityAgent{}).DiscoverReviewSkills(context.Background())
	if err != nil {
		t.Fatalf("DiscoverReviewSkills: %v", err)
	}
	names := map[string]bool{}
	for _, s := range got {
		names[s.Name] = true
	}
	if !names["/code-review"] {
		t.Errorf("missing global-scope /code-review: %+v", got)
	}
	if !names["/security-audit"] {
		t.Errorf("missing shared-scope /security-audit: %+v", got)
	}
	if names["/formatter"] {
		t.Errorf("non-review /formatter should be excluded: %+v", got)
	}
}

func TestDiscoverReviewSkills_DedupesAcrossScopes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Chdir(t.TempDir())

	writeAgySkill(t, filepath.Join(home, ".gemini", "antigravity-cli", "skills"), "code-review", "Global.")
	writeAgySkill(t, filepath.Join(home, ".gemini", "skills"), "code-review", "Shared.")

	got, err := (&AntigravityAgent{}).DiscoverReviewSkills(context.Background())
	if err != nil {
		t.Fatalf("DiscoverReviewSkills: %v", err)
	}
	count := 0
	for _, s := range got {
		if s.Name == "/code-review" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("want 1 /code-review after dedupe, got %d: %+v", count, got)
	}
}

func TestDiscoverReviewSkills_NoSkillsReturnsNil(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Chdir(t.TempDir())

	got, err := (&AntigravityAgent{}).DiscoverReviewSkills(context.Background())
	if err != nil || got != nil {
		t.Fatalf("want (nil, nil) on empty install, got (%+v, %v)", got, err)
	}
}
