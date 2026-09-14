package geminicli

import (
	"encoding/json"
	"os"
	"testing"
)

func TestUserHooksRepairDisabledInventory(t *testing.T) {
	path := geminiUserSettings(t)
	ag := &GeminiCLIAgent{}
	if _, err := ag.InstallUserHooks(t.Context()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var config GeminiSettings
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	config.HooksConfig.Enabled = false
	data, err = json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ag.InstallUserHooks(t.Context()); err != nil {
		t.Fatal(err)
	}
	current, err := ag.AreUserHooksInstalled(t.Context())
	if err != nil || !current {
		t.Fatalf("repaired inventory current = %v, %v", current, err)
	}
}
