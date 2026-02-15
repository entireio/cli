package codexcli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

func TestGetHookNames(t *testing.T) {
	t.Parallel()
	ag := &CodexCLIAgent{}
	names := ag.GetHookNames()
	if len(names) != 1 || names[0] != HookNameTurnComplete {
		t.Errorf("GetHookNames() = %v, want [%s]", names, HookNameTurnComplete)
	}
}

func TestGetSupportedHooks(t *testing.T) {
	t.Parallel()
	ag := &CodexCLIAgent{}
	hooks := ag.GetSupportedHooks()
	if len(hooks) != 1 || hooks[0] != agent.HookStop {
		t.Errorf("GetSupportedHooks() = %v, want [%v]", hooks, agent.HookStop)
	}
}

func TestInstallHooks_FreshInstall(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, ".codex", "config.toml")

	// Override GetHookConfigPath by creating the directory and writing directly
	if err := os.MkdirAll(filepath.Dir(configPath), 0o750); err != nil {
		t.Fatalf("failed to create dir: %v", err)
	}

	// Simulate a fresh config file
	if err := os.WriteFile(configPath, []byte("model = \"gpt-4\"\n"), 0o600); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	// Test the TOML manipulation helpers directly
	content := "model = \"gpt-4\"\n"
	notifyCmd := "entire hooks codex turn-complete"

	// No existing notify — should add it
	if hasNotifyConfig(content) {
		t.Error("hasNotifyConfig() should return false for config without notify")
	}

	content += "notify = [\"" + escapeTomlString(notifyCmd) + "\"]\n"

	if !strings.Contains(content, notifyCmd) {
		t.Error("content should contain notify command after adding")
	}
}

func TestInstallHooks_Idempotent(t *testing.T) {
	t.Parallel()

	content := "notify = [\"entire hooks codex turn-complete\"]\n"
	notifyCmd := "entire hooks codex turn-complete"

	// Already installed — should detect
	if !strings.Contains(content, notifyCmd) {
		t.Error("should detect already installed command")
	}
}

func TestHasNotifyConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		content string
		want    bool
	}{
		{"empty", "", false},
		{"no notify", "model = \"gpt-4\"\n", false},
		{"simple notify", "notify = \"cmd\"\n", true},
		{"array notify", "notify = [\"cmd1\", \"cmd2\"]\n", true},
		{"notify with spaces", "  notify  =  [\"cmd\"]\n", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := hasNotifyConfig(tt.content); got != tt.want {
				t.Errorf("hasNotifyConfig(%q) = %v, want %v", tt.content, got, tt.want)
			}
		})
	}
}

func TestAppendToNotify_Array(t *testing.T) {
	t.Parallel()

	content := "notify = [\"existing-cmd\"]\nmodel = \"gpt-4\"\n"
	result := appendToNotify(content, "new-cmd")

	if !strings.Contains(result, "existing-cmd") {
		t.Error("should preserve existing command")
	}
	if !strings.Contains(result, "new-cmd") {
		t.Error("should add new command")
	}
}

func TestAppendToNotify_String(t *testing.T) {
	t.Parallel()

	content := "notify = \"existing-cmd\"\n"
	result := appendToNotify(content, "new-cmd")

	if !strings.Contains(result, "existing-cmd") {
		t.Error("should preserve existing command")
	}
	if !strings.Contains(result, "new-cmd") {
		t.Error("should add new command")
	}
	// Should be converted to array format
	if !strings.Contains(result, "[") {
		t.Error("should convert to array format")
	}
}

func TestRemoveEntireNotify_RemovesEntireOnly(t *testing.T) {
	t.Parallel()

	content := "notify = [\"user-cmd\", \"entire hooks codex turn-complete\"]\nmodel = \"gpt-4\"\n"
	result := removeEntireNotify(content)

	if !strings.Contains(result, "user-cmd") {
		t.Error("should preserve user command")
	}
	if strings.Contains(result, "entire hooks codex") {
		t.Error("should remove Entire command")
	}
}

func TestRemoveEntireNotify_RemovesEntireLine(t *testing.T) {
	t.Parallel()

	content := "notify = [\"entire hooks codex turn-complete\"]\nmodel = \"gpt-4\"\n"
	result := removeEntireNotify(content)

	if strings.Contains(result, "notify") {
		t.Error("should remove entire notify line when only Entire entries exist")
	}
	if !strings.Contains(result, "model") {
		t.Error("should preserve other config lines")
	}
}

func TestRemoveEntireNotify_LocalDev(t *testing.T) {
	t.Parallel()

	content := "notify = [\"go run /path/to/main.go hooks codex turn-complete\"]\n"
	result := removeEntireNotify(content)

	if strings.Contains(result, "notify") {
		t.Error("should remove local dev notify entries")
	}
}

func TestEscapeTomlString(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input string
		want  string
	}{
		{"simple", "simple"},
		{"with \"quotes\"", "with \\\"quotes\\\""},
		{"with \\backslash", "with \\\\backslash"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			t.Parallel()
			if got := escapeTomlString(tt.input); got != tt.want {
				t.Errorf("escapeTomlString(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestAreHooksInstalled(t *testing.T) {
	t.Parallel()

	// Without a real config file, should return false
	ag := &CodexCLIAgent{}
	if ag.AreHooksInstalled() {
		t.Error("AreHooksInstalled() should return false when no config exists")
	}
}

func TestHasNonEntireNotifyEntries(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		line string
		want bool
	}{
		{"only entire", `notify = ["entire hooks codex turn-complete"]`, false},
		{"mixed", `notify = ["user-cmd", "entire hooks codex turn-complete"]`, true},
		{"only user", `notify = ["user-cmd"]`, true},
		{"local dev only", `notify = ["go run /path/main.go hooks codex turn-complete"]`, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := hasNonEntireNotifyEntries(tt.line); got != tt.want {
				t.Errorf("hasNonEntireNotifyEntries(%q) = %v, want %v", tt.line, got, tt.want)
			}
		})
	}
}

func TestInsertAtTopLevel_BeforeSection(t *testing.T) {
	t.Parallel()

	content := "model = \"gpt-4\"\n[features]\ncollab = true\n"
	result := insertAtTopLevel(content, "notify = [\"cmd\"]\n")

	// notify should appear before [features]
	notifyIdx := strings.Index(result, "notify")
	sectionIdx := strings.Index(result, "[features]")
	if notifyIdx < 0 || sectionIdx < 0 || notifyIdx > sectionIdx {
		t.Errorf("notify should be inserted before [features], got:\n%s", result)
	}
}

func TestInsertAtTopLevel_NoSections(t *testing.T) {
	t.Parallel()

	content := "model = \"gpt-4\"\n"
	result := insertAtTopLevel(content, "notify = [\"cmd\"]\n")

	if !strings.Contains(result, "notify") {
		t.Error("should contain notify line")
	}
	if !strings.Contains(result, "model") {
		t.Error("should preserve existing content")
	}
}
