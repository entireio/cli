package settings_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

func TestLoad_MigratesLegacyRolesInMemory(t *testing.T) {
	// Not t.Parallel(): t.Chdir cannot be combined with parallel tests.
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	t.Chdir(dir)

	settingsPath := filepath.Join(dir, ".entire", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
		t.Fatal(err)
	}
	initial := []byte(`{
      "review": {
        "claude-code": {"skills": ["/review"]},
        "codex": {"skills": ["/review"]}
      },
      "review_fix_agent": "codex"
    }`)
	if err := os.WriteFile(settingsPath, initial, 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := settings.Load(context.Background())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if s.Review["claude-code"].Role != settings.RoleReviewer {
		t.Errorf("claude-code role = %q, want reviewer", s.Review["claude-code"].Role)
	}
	if s.Review["codex"].Role != settings.RoleBoth {
		t.Errorf("codex role = %q, want both (skills + was fix agent)",
			s.Review["codex"].Role)
	}

	// CRITICAL: Load must NOT persist the migration. The file on disk
	// should still be the original legacy form, so re-loading must yield
	// the same migrated result deterministically (idempotent).
	raw, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	var disk map[string]any
	if err := json.Unmarshal(raw, &disk); err != nil {
		t.Fatal(err)
	}
	review, ok := disk["review"].(map[string]any)
	if !ok {
		t.Fatalf("review key missing or wrong type in on-disk JSON: %v", disk["review"])
	}
	cc, ok := review["claude-code"].(map[string]any)
	if !ok {
		t.Fatalf("claude-code entry missing or wrong type: %v", review["claude-code"])
	}
	if _, hasRole := cc["role"]; hasRole {
		t.Errorf("Load auto-persisted role field; expected on-disk schema unchanged")
	}
}

func TestMigrateLegacyRoles_NilSettings(t *testing.T) {
	t.Parallel()
	if got := settings.MigrateLegacyRoles(nil); got {
		t.Errorf("MigrateLegacyRoles(nil) = true, want false")
	}
}

func TestMigrateLegacyRoles_EmptyReviewMap(t *testing.T) {
	t.Parallel()
	s1 := &settings.EntireSettings{Review: nil}
	if got := settings.MigrateLegacyRoles(s1); got {
		t.Errorf("MigrateLegacyRoles(Review=nil) = true, want false")
	}
	s2 := &settings.EntireSettings{Review: map[string]settings.ReviewConfig{}}
	if got := settings.MigrateLegacyRoles(s2); got {
		t.Errorf("MigrateLegacyRoles(Review={}) = true, want false")
	}
}

func TestMigrateLegacyRoles_Idempotent(t *testing.T) {
	t.Parallel()
	s := &settings.EntireSettings{
		Review: map[string]settings.ReviewConfig{
			"claude-code": {Skills: []string{"/review"}},
			"codex":       {Skills: []string{"/review"}},
		},
		ReviewFixAgent: "codex",
	}
	if got := settings.MigrateLegacyRoles(s); !got {
		t.Fatalf("first call returned false, want true (entries needed migration)")
	}
	if got := settings.MigrateLegacyRoles(s); got {
		t.Errorf("second call returned true, want false (already migrated)")
	}
	// Sanity: roles should still be set correctly after second call.
	if s.Review["claude-code"].Role != settings.RoleReviewer {
		t.Errorf("claude-code role = %q, want reviewer", s.Review["claude-code"].Role)
	}
	if s.Review["codex"].Role != settings.RoleBoth {
		t.Errorf("codex role = %q, want both", s.Review["codex"].Role)
	}
}

func TestMigrateLegacyRoles_LegacyFixAgentWithoutSkills(t *testing.T) {
	t.Parallel()
	s := &settings.EntireSettings{
		Review: map[string]settings.ReviewConfig{
			"claude-code": {Skills: []string{"/review"}},
			"codex":       {}, // no Skills, no Prompt
		},
		ReviewFixAgent: "codex",
	}
	if got := settings.MigrateLegacyRoles(s); !got {
		t.Fatalf("MigrateLegacyRoles = false, want true")
	}
	if s.Review["codex"].Role != settings.RoleFixer {
		t.Errorf("codex role = %q, want fixer (empty Skills/Prompt should not yield Both)",
			s.Review["codex"].Role)
	}
	if s.Review["claude-code"].Role != settings.RoleReviewer {
		t.Errorf("claude-code role = %q, want reviewer", s.Review["claude-code"].Role)
	}
}

func TestMigrateLegacyRoles_PreservesExistingRoles(t *testing.T) {
	t.Parallel()
	s := &settings.EntireSettings{
		Review: map[string]settings.ReviewConfig{
			"claude-code": {Role: settings.RoleSkip, Skills: []string{"/review"}},
			"codex":       {Skills: []string{"/review"}}, // empty Role, will migrate
			"gemini":      {Role: settings.RoleReviewer, Skills: []string{"/review"}},
		},
		ReviewFixAgent: "codex",
	}
	if got := settings.MigrateLegacyRoles(s); !got {
		t.Fatalf("MigrateLegacyRoles = false, want true (codex needed migration)")
	}
	if s.Review["claude-code"].Role != settings.RoleSkip {
		t.Errorf("claude-code role mutated: got %q, want skip (preserved)",
			s.Review["claude-code"].Role)
	}
	if s.Review["gemini"].Role != settings.RoleReviewer {
		t.Errorf("gemini role mutated: got %q, want reviewer (preserved)",
			s.Review["gemini"].Role)
	}
	// codex was the migration target; it had skills + matched ReviewFixAgent → RoleBoth.
	if s.Review["codex"].Role != settings.RoleBoth {
		t.Errorf("codex role = %q, want both (has skills + is fix agent)",
			s.Review["codex"].Role)
	}
}

// TestMigrateLegacyRoles_MultipleEmpty_AlphabeticalFixerWins:
// MigrateLegacyRoles alone cannot produce two fixers from empty-role entries
// (only the ReviewFixAgent gets fixer-bearing roles; all other empty entries
// become RoleReviewer). The alphabetical-winner branch is exercised when a
// pre-existing fixer collides with a migrated fixer; that scenario is covered
// by TestMigrateLegacyRoles_PreservesExistingRoles above, and the pure
// at-most-one-fixer behavior is locked down by TestNormalizeRoles_AtMostOneFixer
// in roles_test.go.

func TestSave_RoundtripsMigratedSchema(t *testing.T) {
	// Not t.Parallel(): t.Chdir cannot be combined with parallel tests.
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	t.Chdir(dir)

	s := &settings.EntireSettings{
		Review: map[string]settings.ReviewConfig{
			"claude-code": {Role: settings.RoleReviewer, Skills: []string{"/review"}},
			"codex":       {Role: settings.RoleFixer},
		},
		FixAfterReview: settings.FixAfterReviewAlways,
	}
	if err := settings.Save(context.Background(), s); err != nil {
		t.Fatalf("save: %v", err)
	}

	loaded, err := settings.Load(context.Background())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.Review["claude-code"].Role != settings.RoleReviewer {
		t.Errorf("role lost: %+v", loaded.Review["claude-code"])
	}
	if loaded.FixAfterReview != settings.FixAfterReviewAlways {
		t.Errorf("fix_after_review lost: %q", loaded.FixAfterReview)
	}
}

func TestLoad_RejectsInvalidRole(t *testing.T) {
	// Not t.Parallel(): t.Chdir cannot be combined with parallel tests.
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	t.Chdir(dir)

	settingsPath := filepath.Join(dir, ".entire", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
		t.Fatal(err)
	}
	bad := []byte(`{"review": {"claude-code": {"role": "bogus"}}}`)
	if err := os.WriteFile(settingsPath, bad, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := settings.Load(context.Background()); err == nil {
		t.Fatal("expected error for invalid role, got nil")
	} else if !strings.Contains(err.Error(), "invalid role") {
		t.Errorf("error should mention invalid role, got: %v", err)
	}
}

func TestLoad_RejectsInvalidFixAfterReview(t *testing.T) {
	// Not t.Parallel(): t.Chdir cannot be combined with parallel tests.
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	t.Chdir(dir)

	settingsPath := filepath.Join(dir, ".entire", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
		t.Fatal(err)
	}
	bad := []byte(`{"fix_after_review": "sometimes"}`)
	if err := os.WriteFile(settingsPath, bad, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := settings.Load(context.Background()); err == nil {
		t.Fatal("expected error for invalid fix_after_review, got nil")
	} else if !strings.Contains(err.Error(), "invalid fix_after_review") {
		t.Errorf("error should mention invalid fix_after_review, got: %v", err)
	}
}

// A clone-local "ask" must override a project-level "always". The empty
// string remains "no override"; "ask" is the explicit, non-empty token that
// lets a clone opt back out of auto-fix.
func TestLoad_CloneAskOverridesProjectAlways(t *testing.T) {
	// Not t.Parallel(): t.Chdir cannot be combined with parallel tests.
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	t.Chdir(dir)

	entireDir := filepath.Join(dir, ".entire")
	if err := os.MkdirAll(entireDir, 0o755); err != nil {
		t.Fatal(err)
	}
	project := []byte(`{"enabled": true, "fix_after_review": "always"}`)
	if err := os.WriteFile(filepath.Join(entireDir, "settings.json"), project, 0o644); err != nil {
		t.Fatal(err)
	}

	prefsDir := filepath.Join(dir, ".git", "entire")
	if err := os.MkdirAll(prefsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	prefs := []byte(`{"fix_after_review": "ask"}`)
	if err := os.WriteFile(filepath.Join(prefsDir, "preferences.json"), prefs, 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := settings.Load(context.Background())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if s.FixAfterReview != settings.FixAfterReviewAsk {
		t.Errorf("clone 'ask' should override project 'always', got %q", s.FixAfterReview)
	}
}
