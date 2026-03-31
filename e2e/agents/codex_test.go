package agents

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestSeedCodexHome_WritesAPIKeyAuth(t *testing.T) {
	home := t.TempDir()
	projectDir := filepath.Join(t.TempDir(), "repo")
	t.Setenv("OPENAI_API_KEY", "sk-test-key")
	t.Setenv("E2E_CODEX_MODEL", "gpt-5.4")

	if err := os.MkdirAll(projectDir, 0o750); err != nil {
		t.Fatalf("mkdir project dir: %v", err)
	}

	if err := seedCodexHome(home, projectDir); err != nil {
		t.Fatalf("seedCodexHome: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(home, "auth.json"))
	if err != nil {
		t.Fatalf("read auth.json: %v", err)
	}

	var auth struct {
		AuthMode     string `json:"auth_mode"`
		OpenAIAPIKey string `json:"OPENAI_API_KEY"`
	}
	if err := json.Unmarshal(data, &auth); err != nil {
		t.Fatalf("unmarshal auth.json: %v", err)
	}

	if auth.AuthMode != "apikey" {
		t.Fatalf("auth_mode = %q, want %q", auth.AuthMode, "apikey")
	}
	if auth.OpenAIAPIKey != "sk-test-key" {
		t.Fatalf("OPENAI_API_KEY = %q, want %q", auth.OpenAIAPIKey, "sk-test-key")
	}
}
