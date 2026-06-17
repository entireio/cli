package review

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

func TestReviewSettingsMigration_MovesProjectReviewToClonePreferences(t *testing.T) {
	tmp := t.TempDir()
	testutil.InitRepo(t, tmp)
	t.Chdir(tmp)
	session.ClearGitCommonDirCache()

	entireDir := filepath.Join(tmp, ".entire")
	if err := os.MkdirAll(entireDir, 0o750); err != nil {
		t.Fatalf("mkdir .entire: %v", err)
	}
	projectSettings := []byte(`{
		"enabled": true,
		"log_level": "debug",
		"review": {"claude-code": {"skills": ["/review"], "prompt": "project"}},
		"review_fix_agent": "claude-code"
	}`)
	projectPath := filepath.Join(entireDir, "settings.json")
	if err := os.WriteFile(projectPath, projectSettings, 0o600); err != nil {
		t.Fatalf("write project settings: %v", err)
	}

	prompted := false
	promptQuestion := ""
	var out bytes.Buffer
	if err := maybePromptReviewSettingsMigration(context.Background(), &out, &out, true, func(_ context.Context, question string, _ bool) (bool, error) {
		prompted = true
		promptQuestion = question
		return true, nil
	}); err != nil {
		t.Fatalf("migration: %v", err)
	}
	if !prompted {
		t.Fatal("expected migration prompt")
	}
	for _, want := range []string{"project settings", "clone-local preferences", "typically committed"} {
		if !strings.Contains(promptQuestion, want) {
			t.Fatalf("migration prompt = %q, want it to mention %q", promptQuestion, want)
		}
	}

	prefs, err := settings.LoadClonePreferences(context.Background())
	if err != nil {
		t.Fatalf("load preferences: %v", err)
	}
	if got := prefs.Review["claude-code"].Prompt; got != "project" {
		t.Fatalf("migrated prompt = %q, want project", got)
	}
	if prefs.ReviewFixAgent != "claude-code" {
		t.Fatalf("ReviewFixAgent = %q, want claude-code", prefs.ReviewFixAgent)
	}

	raw := map[string]json.RawMessage{}
	data, err := os.ReadFile(projectPath)
	if err != nil {
		t.Fatalf("read project settings: %v", err)
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal project settings: %v", err)
	}
	if _, ok := raw["review"]; ok {
		t.Fatalf("project review key was not removed: %s", data)
	}
	if _, ok := raw["review_fix_agent"]; ok {
		t.Fatalf("project review_fix_agent key was not removed: %s", data)
	}
	if _, ok := raw["log_level"]; !ok {
		t.Fatalf("unrelated project settings were not preserved: %s", data)
	}
}

// TestReviewSettingsMigration_MergesNonOverlappingPrefs verifies that when the
// project file has review keys for an agent NOT present in clone-local prefs,
// the migration merges them in. Previously the migration silently dropped any
// project config when prefs already had any review entry — that was data loss.
func TestReviewSettingsMigration_MergesNonOverlappingPrefs(t *testing.T) {
	tmp := t.TempDir()
	testutil.InitRepo(t, tmp)
	t.Chdir(tmp)
	session.ClearGitCommonDirCache()

	entireDir := filepath.Join(tmp, ".entire")
	if err := os.MkdirAll(entireDir, 0o750); err != nil {
		t.Fatalf("mkdir .entire: %v", err)
	}
	projectPath := filepath.Join(entireDir, "settings.json")
	projectSettings := []byte(`{
		"enabled": true,
		"review": {"project-agent": {"prompt": "project"}}
	}`)
	if err := os.WriteFile(projectPath, projectSettings, 0o600); err != nil {
		t.Fatalf("write project settings: %v", err)
	}
	if err := settings.SaveClonePreferences(context.Background(), &settings.ClonePreferences{
		Review: map[string]settings.ReviewConfig{
			"local-agent": {Prompt: "local"},
		},
	}); err != nil {
		t.Fatalf("seed preferences: %v", err)
	}

	var out bytes.Buffer
	if err := maybePromptReviewSettingsMigration(context.Background(), &out, &out, true, func(context.Context, string, bool) (bool, error) {
		return true, nil
	}); err != nil {
		t.Fatalf("migration: %v", err)
	}

	prefs, err := settings.LoadClonePreferences(context.Background())
	if err != nil {
		t.Fatalf("load preferences: %v", err)
	}
	if got := prefs.Review["local-agent"].Prompt; got != "local" {
		t.Fatalf("local prompt = %q, want preserved as %q", got, "local")
	}
	if got := prefs.Review["project-agent"].Prompt; got != "project" {
		t.Fatalf("project prompt = %q, want merged in as %q", got, "project")
	}

	data, err := os.ReadFile(projectPath)
	if err != nil {
		t.Fatalf("read project settings: %v", err)
	}
	raw := map[string]json.RawMessage{}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal project settings: %v", err)
	}
	if _, ok := raw["review"]; ok {
		t.Fatalf("project review key was not removed: %s", data)
	}
}

// TestReviewSettingsMigration_RefusesConflictingPrefs verifies that when both
// the project file and clone-local prefs have review config for the SAME agent
// with DIFFERENT values, the migration aborts with a clear error rather than
// silently dropping one side. The user must reconcile manually.
func TestReviewSettingsMigration_RefusesConflictingPrefs(t *testing.T) {
	tmp := t.TempDir()
	testutil.InitRepo(t, tmp)
	t.Chdir(tmp)
	session.ClearGitCommonDirCache()

	entireDir := filepath.Join(tmp, ".entire")
	if err := os.MkdirAll(entireDir, 0o750); err != nil {
		t.Fatalf("mkdir .entire: %v", err)
	}
	projectPath := filepath.Join(entireDir, "settings.json")
	projectSettings := []byte(`{
		"enabled": true,
		"review": {"claude-code": {"prompt": "project"}}
	}`)
	if err := os.WriteFile(projectPath, projectSettings, 0o600); err != nil {
		t.Fatalf("write project settings: %v", err)
	}
	if err := settings.SaveClonePreferences(context.Background(), &settings.ClonePreferences{
		Review: map[string]settings.ReviewConfig{
			"claude-code": {Prompt: "local"},
		},
	}); err != nil {
		t.Fatalf("seed preferences: %v", err)
	}

	var out bytes.Buffer
	err := maybePromptReviewSettingsMigration(context.Background(), &out, &out, true, func(context.Context, string, bool) (bool, error) {
		return true, nil
	})
	if err == nil {
		t.Fatal("expected migration to refuse conflicting prefs")
	}
	if !strings.Contains(err.Error(), "claude-code") {
		t.Errorf("error = %q, want it to name the conflicting agent (claude-code)", err.Error())
	}
	if !strings.Contains(err.Error(), "reconcile manually") {
		t.Errorf("error = %q, want it to guide manual reconciliation", err.Error())
	}

	// Project file must NOT have been rewritten on the conflict path.
	data, err := os.ReadFile(projectPath)
	if err != nil {
		t.Fatalf("read project settings: %v", err)
	}
	if !bytes.Contains(data, []byte("claude-code")) {
		t.Fatalf("project file was modified despite conflict abort: %s", data)
	}

	// Clone prefs must be unchanged.
	prefs, err := settings.LoadClonePreferences(context.Background())
	if err != nil {
		t.Fatalf("load preferences: %v", err)
	}
	if got := prefs.Review["claude-code"].Prompt; got != "local" {
		t.Errorf("local prompt = %q, want unchanged as %q", got, "local")
	}
}

// TestReviewSettingsMigration_NoMoveCleansUpKeys verifies the cleanup-only
// path: project has only `null` values for review keys, so nothing actually
// moves, but the project keys are still stripped and the success message
// reflects that distinction.
func TestReviewSettingsMigration_NoMoveCleansUpKeys(t *testing.T) {
	tmp := t.TempDir()
	testutil.InitRepo(t, tmp)
	t.Chdir(tmp)
	session.ClearGitCommonDirCache()

	entireDir := filepath.Join(tmp, ".entire")
	if err := os.MkdirAll(entireDir, 0o750); err != nil {
		t.Fatalf("mkdir .entire: %v", err)
	}
	projectPath := filepath.Join(entireDir, "settings.json")
	if err := os.WriteFile(projectPath, []byte(`{
		"enabled": true,
		"review": null,
		"review_fix_agent": null
	}`), 0o600); err != nil {
		t.Fatalf("write project settings: %v", err)
	}

	var out bytes.Buffer
	if err := maybePromptReviewSettingsMigration(context.Background(), &out, &out, true, func(context.Context, string, bool) (bool, error) {
		return true, nil
	}); err != nil {
		t.Fatalf("migration: %v", err)
	}
	if !strings.Contains(out.String(), "Removed unused review keys") {
		t.Errorf("output = %q, want the cleanup-only message", out.String())
	}

	data, err := os.ReadFile(projectPath)
	if err != nil {
		t.Fatalf("read project settings: %v", err)
	}
	raw := map[string]json.RawMessage{}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal project settings: %v", err)
	}
	if _, ok := raw["review"]; ok {
		t.Fatalf("project review key was not removed: %s", data)
	}
}

// TestReviewSettingsMigration_DeclinePersistsDismissal verifies that declining
// the prompt records ReviewMigrationDismissed in clone-local prefs, and that a
// subsequent invocation does NOT re-prompt. Without this, teams who
// intentionally commit review prefs would be re-prompted on every command.
func TestReviewSettingsMigration_DeclinePersistsDismissal(t *testing.T) {
	tmp := t.TempDir()
	testutil.InitRepo(t, tmp)
	t.Chdir(tmp)
	session.ClearGitCommonDirCache()

	entireDir := filepath.Join(tmp, ".entire")
	if err := os.MkdirAll(entireDir, 0o750); err != nil {
		t.Fatalf("mkdir .entire: %v", err)
	}
	projectPath := filepath.Join(entireDir, "settings.json")
	projectSettings := []byte(`{
		"enabled": true,
		"review": {"claude-code": {"prompt": "project"}}
	}`)
	if err := os.WriteFile(projectPath, projectSettings, 0o600); err != nil {
		t.Fatalf("write project settings: %v", err)
	}

	// First invocation: user declines.
	var out bytes.Buffer
	promptCount := 0
	declineThenFail := func(context.Context, string, bool) (bool, error) {
		promptCount++
		return false, nil
	}
	if err := maybePromptReviewSettingsMigration(context.Background(), &out, &out, true, declineThenFail); err != nil {
		t.Fatalf("first invocation: %v", err)
	}
	if promptCount != 1 {
		t.Errorf("first invocation prompted %d times, want 1", promptCount)
	}

	// Dismissal must be persisted.
	prefs, err := settings.LoadClonePreferences(context.Background())
	if err != nil {
		t.Fatalf("load preferences: %v", err)
	}
	if prefs == nil || !prefs.ReviewMigrationDismissed {
		t.Fatalf("ReviewMigrationDismissed = false, want true after decline (prefs = %+v)", prefs)
	}

	// Project file must be untouched on decline.
	data, err := os.ReadFile(projectPath)
	if err != nil {
		t.Fatalf("read project settings: %v", err)
	}
	if !bytes.Contains(data, []byte("claude-code")) {
		t.Errorf("project file was modified on decline: %s", data)
	}

	// Second invocation: must NOT re-prompt.
	failIfPrompted := func(context.Context, string, bool) (bool, error) {
		t.Fatal("prompt should not be called when dismissal is persisted")
		return false, nil
	}
	if err := maybePromptReviewSettingsMigration(context.Background(), &out, &out, true, failIfPrompted); err != nil {
		t.Fatalf("second invocation: %v", err)
	}
}

func TestReviewSettingsMigration_SkipsWhenProjectHasNoReviewKeys(t *testing.T) {
	tmp := t.TempDir()
	testutil.InitRepo(t, tmp)
	t.Chdir(tmp)
	session.ClearGitCommonDirCache()

	entireDir := filepath.Join(tmp, ".entire")
	if err := os.MkdirAll(entireDir, 0o750); err != nil {
		t.Fatalf("mkdir .entire: %v", err)
	}
	projectPath := filepath.Join(entireDir, "settings.json")
	if err := os.WriteFile(projectPath, []byte(`{"enabled":true,"log_level":"debug"}`), 0o600); err != nil {
		t.Fatalf("write project settings: %v", err)
	}

	var out bytes.Buffer
	if err := maybePromptReviewSettingsMigration(context.Background(), &out, &out, true, func(context.Context, string, bool) (bool, error) {
		t.Fatal("prompt should not be called")
		return false, nil
	}); err != nil {
		t.Fatalf("migration: %v", err)
	}

	preferencesPath, err := settings.ClonePreferencesPath(context.Background())
	if err != nil {
		t.Fatalf("preferences path: %v", err)
	}
	if _, err := os.Stat(preferencesPath); !os.IsNotExist(err) {
		t.Fatalf("preferences file exists after no-op migration: %v", err)
	}
}

// TestReviewSettingsMigration_BailsOnLocalSettingsReviewKeys pins the
// precondition: when .entire/settings.local.json has review keys, those
// override clone-local preferences via mergeJSON's wholesale-replace path,
// so the migration must surface the conflict up front rather than silently
// produce a migrated-but-masked state. Bailing also intentionally does NOT
// set ReviewMigrationDismissed — this is a fixable precondition, not a
// rejected migration, and the user should be re-prompted after cleaning
// settings.local.json.
func TestReviewSettingsMigration_BailsOnLocalSettingsReviewKeys(t *testing.T) {
	tmp := t.TempDir()
	testutil.InitRepo(t, tmp)
	t.Chdir(tmp)
	session.ClearGitCommonDirCache()

	entireDir := filepath.Join(tmp, ".entire")
	if err := os.MkdirAll(entireDir, 0o750); err != nil {
		t.Fatalf("mkdir .entire: %v", err)
	}
	projectPath := filepath.Join(entireDir, "settings.json")
	projectSettings := []byte(`{
		"enabled": true,
		"review": {"claude-code": {"prompt": "project"}}
	}`)
	if err := os.WriteFile(projectPath, projectSettings, 0o600); err != nil {
		t.Fatalf("write project settings: %v", err)
	}
	localPath := filepath.Join(entireDir, "settings.local.json")
	localSettings := []byte(`{"review": {"local-agent": {"prompt": "local"}}}`)
	if err := os.WriteFile(localPath, localSettings, 0o600); err != nil {
		t.Fatalf("write local settings: %v", err)
	}

	var out, errOut bytes.Buffer
	if err := maybePromptReviewSettingsMigration(context.Background(), &out, &errOut, true, func(context.Context, string, bool) (bool, error) {
		t.Fatal("prompt should not be called when settings.local.json has review keys")
		return false, nil
	}); err != nil {
		t.Fatalf("migration: %v", err)
	}

	stderr := errOut.String()
	for _, want := range []string{"settings.local.json", "review", "Remove"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr = %q, want it to mention %q", stderr, want)
		}
	}

	// Project file must NOT have been rewritten — the bail path leaves
	// everything in place so the user can clean settings.local.json and
	// re-run.
	got, err := os.ReadFile(projectPath)
	if err != nil {
		t.Fatalf("read project settings: %v", err)
	}
	if !bytes.Contains(got, []byte(`"claude-code"`)) {
		t.Fatalf("project file was modified despite bail; got: %s", got)
	}

	// Dismissal must NOT be persisted — the user didn't choose to dismiss,
	// they hit a fixable precondition. Next run should re-prompt.
	prefs, err := settings.LoadClonePreferences(context.Background())
	if err != nil {
		t.Fatalf("load preferences: %v", err)
	}
	if prefs != nil && prefs.ReviewMigrationDismissed {
		t.Fatalf("ReviewMigrationDismissed = true after bail; should not persist a fixable precondition as dismissal")
	}
}
