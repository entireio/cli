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
	for _, want := range []string{"project settings", "local preferences"} {
		if !strings.Contains(promptQuestion, want) {
			t.Fatalf("migration prompt = %q, want it to mention %q", promptQuestion, want)
		}
	}
	if strings.Contains(promptQuestion, "can be committed") {
		t.Fatalf("migration prompt = %q, should not mention commit risk", promptQuestion)
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

func TestReviewSettingsMigration_DoesNotOverwriteExistingClonePreferences(t *testing.T) {
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
		"review": {"project-agent": {"prompt": "project"}},
		"review_fix_agent": "project-agent"
	}`)
	if err := os.WriteFile(projectPath, projectSettings, 0o600); err != nil {
		t.Fatalf("write project settings: %v", err)
	}
	if err := settings.SaveClonePreferences(context.Background(), &settings.ClonePreferences{
		Review: map[string]settings.ReviewConfig{
			"local-agent": {Prompt: "local"},
		},
		ReviewFixAgent: "local-agent",
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
		t.Fatalf("local prompt = %q, want local", got)
	}
	if _, ok := prefs.Review["project-agent"]; ok {
		t.Fatalf("project review overwrote existing preferences: %+v", prefs.Review)
	}
	if prefs.ReviewFixAgent != "local-agent" {
		t.Fatalf("ReviewFixAgent = %q, want local-agent", prefs.ReviewFixAgent)
	}

	data, err := os.ReadFile(projectPath)
	if err != nil {
		t.Fatalf("read project settings: %v", err)
	}
	if bytes.Contains(data, []byte("review")) {
		t.Fatalf("project review keys were not removed: %s", data)
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
