package claudecode

import (
	"encoding/json"
	"os"
	"testing"
)

func TestUserHooksRequireSelectedInstallation(t *testing.T) {
	path := claudeUserSettings(t)
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	_, err := (&ClaudeCodeAgent{}).InstallUserHooks(t.Context())
	if err == nil {
		t.Fatal("user hook installation must require a selected installation")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("unselected installation changed user hooks: %v", err)
	}
}

func TestUserHookInventoryRequiresExactMatchersAndTypes(t *testing.T) {
	for _, field := range []string{"matcher", "type"} {
		t.Run(field, func(t *testing.T) {
			path := claudeUserSettings(t)
			ag := &ClaudeCodeAgent{}
			if _, err := ag.InstallUserHooks(t.Context()); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var config ClaudeSettings
			if err := json.Unmarshal(data, &config); err != nil {
				t.Fatal(err)
			}
			if field == "matcher" {
				config.Hooks.SessionStart[0].Matcher = "resume"
			} else {
				config.Hooks.SessionStart[0].Hooks[0].Type = "prompt"
			}
			data, err = json.Marshal(config)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			current, err := ag.AreUserHooksInstalled(t.Context())
			if err != nil || current {
				t.Fatalf("modified inventory current = %v, %v", current, err)
			}
		})
	}
}
