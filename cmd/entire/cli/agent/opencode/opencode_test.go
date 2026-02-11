package opencode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

func TestNewOpenCodeAgent(t *testing.T) {
	t.Parallel()

	ag := NewOpenCodeAgent()
	if ag == nil {
		t.Fatal("NewOpenCodeAgent() returned nil")
	}

	oc, ok := ag.(*OpenCodeAgent)
	if !ok {
		t.Fatal("NewOpenCodeAgent() didn't return *OpenCodeAgent")
	}
	if oc == nil {
		t.Fatal("NewOpenCodeAgent() returned nil agent")
	}
}

func TestName(t *testing.T) {
	t.Parallel()

	ag := &OpenCodeAgent{}
	if name := ag.Name(); name != agent.AgentNameOpenCode {
		t.Errorf("Name() = %q, want %q", name, agent.AgentNameOpenCode)
	}
}

func TestType(t *testing.T) {
	t.Parallel()

	ag := &OpenCodeAgent{}
	if agType := ag.Type(); agType != agent.AgentTypeOpenCode {
		t.Errorf("Type() = %q, want %q", agType, agent.AgentTypeOpenCode)
	}
}

func TestDescription(t *testing.T) {
	t.Parallel()

	ag := &OpenCodeAgent{}
	desc := ag.Description()
	if desc == "" {
		t.Error("Description() returned empty string")
	}
}

func TestSupportsHooks(t *testing.T) {
	t.Parallel()

	ag := &OpenCodeAgent{}
	if !ag.SupportsHooks() {
		t.Error("SupportsHooks() = false, want true")
	}
}

func TestParseHookInput_SessionIdle(t *testing.T) {
	t.Parallel()

	ag := &OpenCodeAgent{}
	input := `{"type":"session-idle","sessionID":"ses_abc123"}`

	result, err := ag.ParseHookInput(agent.HookStop, strings.NewReader(input))
	if err != nil {
		t.Fatalf("ParseHookInput() error = %v", err)
	}

	if result.SessionID != "ses_abc123" {
		t.Errorf("SessionID = %q, want %q", result.SessionID, "ses_abc123")
	}
	if result.RawData["type"] != "session-idle" {
		t.Errorf("RawData[type] = %q, want %q", result.RawData["type"], "session-idle")
	}
}

func TestParseHookInput_SessionCreated(t *testing.T) {
	t.Parallel()

	ag := &OpenCodeAgent{}
	input := `{"type":"session-created","sessionID":"ses_xyz789"}`

	result, err := ag.ParseHookInput(agent.HookSessionStart, strings.NewReader(input))
	if err != nil {
		t.Fatalf("ParseHookInput() error = %v", err)
	}

	if result.SessionID != "ses_xyz789" {
		t.Errorf("SessionID = %q, want %q", result.SessionID, "ses_xyz789")
	}
}

func TestParseHookInput_EmptyInput(t *testing.T) {
	t.Parallel()

	ag := &OpenCodeAgent{}
	_, err := ag.ParseHookInput(agent.HookStop, strings.NewReader(""))
	if err == nil {
		t.Error("ParseHookInput() should return error for empty input")
	}
}

func TestDetectPresence(t *testing.T) {
	t.Run("no .opencode directory", func(t *testing.T) {
		tempDir := t.TempDir()
		t.Chdir(tempDir)

		ag := &OpenCodeAgent{}
		present, err := ag.DetectPresence()
		if err != nil {
			t.Fatalf("DetectPresence() error = %v", err)
		}
		if present {
			t.Error("DetectPresence() = true, want false")
		}
	})

	t.Run("with .opencode directory", func(t *testing.T) {
		tempDir := t.TempDir()
		t.Chdir(tempDir)

		if err := os.Mkdir(".opencode", 0o755); err != nil {
			t.Fatalf("failed to create .opencode: %v", err)
		}

		ag := &OpenCodeAgent{}
		present, err := ag.DetectPresence()
		if err != nil {
			t.Fatalf("DetectPresence() error = %v", err)
		}
		if !present {
			t.Error("DetectPresence() = false, want true")
		}
	})

	t.Run("with opencode.json", func(t *testing.T) {
		tempDir := t.TempDir()
		t.Chdir(tempDir)

		if err := os.WriteFile("opencode.json", []byte(`{}`), 0o600); err != nil {
			t.Fatalf("failed to create opencode.json: %v", err)
		}

		ag := &OpenCodeAgent{}
		present, err := ag.DetectPresence()
		if err != nil {
			t.Fatalf("DetectPresence() error = %v", err)
		}
		if !present {
			t.Error("DetectPresence() = false, want true")
		}
	})

	t.Run("with opencode.jsonc", func(t *testing.T) {
		tempDir := t.TempDir()
		t.Chdir(tempDir)

		if err := os.WriteFile("opencode.jsonc", []byte(`{}`), 0o600); err != nil {
			t.Fatalf("failed to create opencode.jsonc: %v", err)
		}

		ag := &OpenCodeAgent{}
		present, err := ag.DetectPresence()
		if err != nil {
			t.Fatalf("DetectPresence() error = %v", err)
		}
		if !present {
			t.Error("DetectPresence() = false, want true")
		}
	})
}

func TestResolveSessionFile(t *testing.T) {
	t.Parallel()

	ag := &OpenCodeAgent{}
	result := ag.ResolveSessionFile("/data/opencode/storage/session", "ses_abc123")
	expected := "/data/opencode/storage/session/ses_abc123.json"
	if result != expected {
		t.Errorf("ResolveSessionFile() = %q, want %q", result, expected)
	}
}

func TestProtectedDirs(t *testing.T) {
	t.Parallel()

	ag := &OpenCodeAgent{}
	dirs := ag.ProtectedDirs()
	if len(dirs) != 1 || dirs[0] != ".opencode" {
		t.Errorf("ProtectedDirs() = %v, want [.opencode]", dirs)
	}
}

func TestFormatResumeCommand(t *testing.T) {
	t.Parallel()

	ag := &OpenCodeAgent{}
	cmd := ag.FormatResumeCommand("ses_abc123")
	expected := "opencode run --session ses_abc123"
	if cmd != expected {
		t.Errorf("FormatResumeCommand() = %q, want %q", cmd, expected)
	}
}

func TestTransformSessionID(t *testing.T) {
	t.Parallel()

	ag := &OpenCodeAgent{}
	id := ag.TransformSessionID("ses_abc123")
	if id != "ses_abc123" {
		t.Errorf("TransformSessionID() = %q, want %q", id, "ses_abc123")
	}
}

func TestGetSessionDir(t *testing.T) {
	// Not parallel: subtests use t.Setenv which modifies process-global state

	t.Run("with test override", func(t *testing.T) {
		t.Setenv("ENTIRE_TEST_OPENCODE_PROJECT_DIR", "/tmp/test-opencode")
		ag := &OpenCodeAgent{}
		dir, err := ag.GetSessionDir("/some/repo")
		if err != nil {
			t.Fatalf("GetSessionDir() error = %v", err)
		}
		if dir != "/tmp/test-opencode" {
			t.Errorf("GetSessionDir() = %q, want %q", dir, "/tmp/test-opencode")
		}
	})
}

func TestGetHookNames(t *testing.T) {
	t.Parallel()

	ag := &OpenCodeAgent{}
	names := ag.GetHookNames()
	if len(names) != 2 {
		t.Fatalf("GetHookNames() returned %d names, want 2", len(names))
	}
	if names[0] != HookNameSessionCreated {
		t.Errorf("names[0] = %q, want %q", names[0], HookNameSessionCreated)
	}
	if names[1] != HookNameSessionIdle {
		t.Errorf("names[1] = %q, want %q", names[1], HookNameSessionIdle)
	}
}

func TestInstallHooks(t *testing.T) {
	t.Run("installs plugin file", func(t *testing.T) {
		tempDir := t.TempDir()
		t.Chdir(tempDir)

		ag := &OpenCodeAgent{}
		count, err := ag.InstallHooks(false, false)
		if err != nil {
			t.Fatalf("InstallHooks() error = %v", err)
		}
		if count != 2 {
			t.Errorf("InstallHooks() count = %d, want 2", count)
		}

		// Verify file exists and contains marker
		pluginPath := filepath.Join(".opencode", "plugins", EntirePluginFileName)
		data, err := os.ReadFile(pluginPath)
		if err != nil {
			t.Fatalf("failed to read plugin file: %v", err)
		}
		content := string(data)
		if !strings.Contains(content, entirePluginMarker) {
			t.Error("plugin file missing marker")
		}
		if !strings.Contains(content, "session.created") {
			t.Error("plugin file missing session.created handler")
		}
		if !strings.Contains(content, "session.status") {
			t.Error("plugin file missing session.status handler")
		}
		if !strings.Contains(content, "entire hooks opencode") {
			t.Error("plugin file missing entire command reference")
		}
	})

	t.Run("idempotent install", func(t *testing.T) {
		tempDir := t.TempDir()
		t.Chdir(tempDir)

		ag := &OpenCodeAgent{}
		// First install
		_, err := ag.InstallHooks(false, false)
		if err != nil {
			t.Fatalf("first InstallHooks() error = %v", err)
		}

		// Second install should be no-op
		count, err := ag.InstallHooks(false, false)
		if err != nil {
			t.Fatalf("second InstallHooks() error = %v", err)
		}
		if count != 0 {
			t.Errorf("second InstallHooks() count = %d, want 0", count)
		}
	})

	t.Run("local dev mode", func(t *testing.T) {
		tempDir := t.TempDir()
		t.Chdir(tempDir)

		ag := &OpenCodeAgent{}
		count, err := ag.InstallHooks(true, false)
		if err != nil {
			t.Fatalf("InstallHooks() error = %v", err)
		}
		if count != 2 {
			t.Errorf("InstallHooks() count = %d, want 2", count)
		}

		pluginPath := filepath.Join(".opencode", "plugins", EntirePluginFileName)
		data, err := os.ReadFile(pluginPath)
		if err != nil {
			t.Fatalf("failed to read plugin file: %v", err)
		}
		content := string(data)
		if !strings.Contains(content, "go run") {
			t.Error("plugin file missing local dev 'go run' command")
		}
	})

	t.Run("force reinstall", func(t *testing.T) {
		tempDir := t.TempDir()
		t.Chdir(tempDir)

		ag := &OpenCodeAgent{}
		// Install production mode
		_, err := ag.InstallHooks(false, false)
		if err != nil {
			t.Fatalf("first InstallHooks() error = %v", err)
		}

		// Force reinstall in local dev mode
		count, err := ag.InstallHooks(true, true)
		if err != nil {
			t.Fatalf("force InstallHooks() error = %v", err)
		}
		if count != 2 {
			t.Errorf("force InstallHooks() count = %d, want 2", count)
		}

		pluginPath := filepath.Join(".opencode", "plugins", EntirePluginFileName)
		data, err := os.ReadFile(pluginPath)
		if err != nil {
			t.Fatalf("failed to read plugin file: %v", err)
		}
		if !strings.Contains(string(data), "go run") {
			t.Error("plugin file should be in local dev mode after force reinstall")
		}
	})
}

func TestAreHooksInstalled(t *testing.T) {
	t.Run("not installed", func(t *testing.T) {
		tempDir := t.TempDir()
		t.Chdir(tempDir)

		ag := &OpenCodeAgent{}
		if ag.AreHooksInstalled() {
			t.Error("AreHooksInstalled() = true, want false")
		}
	})

	t.Run("installed", func(t *testing.T) {
		tempDir := t.TempDir()
		t.Chdir(tempDir)

		ag := &OpenCodeAgent{}
		_, err := ag.InstallHooks(false, false)
		if err != nil {
			t.Fatalf("InstallHooks() error = %v", err)
		}

		if !ag.AreHooksInstalled() {
			t.Error("AreHooksInstalled() = false, want true")
		}
	})

	t.Run("non-entire plugin file", func(t *testing.T) {
		tempDir := t.TempDir()
		t.Chdir(tempDir)

		// Create a plugin file without the Entire marker
		pluginPath := filepath.Join(".opencode", "plugins", EntirePluginFileName)
		if err := os.MkdirAll(filepath.Dir(pluginPath), 0o750); err != nil {
			t.Fatalf("failed to create dirs: %v", err)
		}
		if err := os.WriteFile(pluginPath, []byte("export default {}"), 0o600); err != nil {
			t.Fatalf("failed to write plugin: %v", err)
		}

		ag := &OpenCodeAgent{}
		if ag.AreHooksInstalled() {
			t.Error("AreHooksInstalled() = true for non-Entire plugin, want false")
		}
	})
}

func TestUninstallHooks(t *testing.T) {
	t.Run("uninstalls plugin", func(t *testing.T) {
		tempDir := t.TempDir()
		t.Chdir(tempDir)

		ag := &OpenCodeAgent{}
		_, err := ag.InstallHooks(false, false)
		if err != nil {
			t.Fatalf("InstallHooks() error = %v", err)
		}

		if err := ag.UninstallHooks(); err != nil {
			t.Fatalf("UninstallHooks() error = %v", err)
		}

		if ag.AreHooksInstalled() {
			t.Error("AreHooksInstalled() = true after uninstall")
		}
	})

	t.Run("no plugin file", func(t *testing.T) {
		tempDir := t.TempDir()
		t.Chdir(tempDir)

		ag := &OpenCodeAgent{}
		if err := ag.UninstallHooks(); err != nil {
			t.Errorf("UninstallHooks() error = %v, want nil", err)
		}
	})

	t.Run("does not remove non-entire plugin", func(t *testing.T) {
		tempDir := t.TempDir()
		t.Chdir(tempDir)

		// Create a user's own plugin with the same filename
		pluginPath := filepath.Join(".opencode", "plugins", EntirePluginFileName)
		if err := os.MkdirAll(filepath.Dir(pluginPath), 0o750); err != nil {
			t.Fatalf("failed to create dirs: %v", err)
		}
		if err := os.WriteFile(pluginPath, []byte("export default {}"), 0o600); err != nil {
			t.Fatalf("failed to write plugin: %v", err)
		}

		ag := &OpenCodeAgent{}
		if err := ag.UninstallHooks(); err != nil {
			t.Fatalf("UninstallHooks() error = %v", err)
		}

		// File should still exist
		if _, err := os.Stat(pluginPath); os.IsNotExist(err) {
			t.Error("UninstallHooks() removed non-Entire plugin file")
		}
	})
}

func TestGetSupportedHooks(t *testing.T) {
	t.Parallel()

	ag := &OpenCodeAgent{}
	hooks := ag.GetSupportedHooks()
	if len(hooks) != 2 {
		t.Fatalf("GetSupportedHooks() returned %d hooks, want 2", len(hooks))
	}
}

func TestGetHookConfigPath(t *testing.T) {
	t.Parallel()

	ag := &OpenCodeAgent{}
	path := ag.GetHookConfigPath()
	if !strings.Contains(path, "entire.ts") {
		t.Errorf("GetHookConfigPath() = %q, want path containing 'entire.ts'", path)
	}
}

func TestWriteSession_CreatesParentDir(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	sessionPath := filepath.Join(tempDir, "nested", "dir", "session.json")

	ag := &OpenCodeAgent{}
	err := ag.WriteSession(&agent.AgentSession{
		SessionID:  "test-session",
		AgentName:  agent.AgentNameOpenCode,
		SessionRef: sessionPath,
		NativeData: []byte(`{"test": true}`),
	})
	if err != nil {
		t.Fatalf("WriteSession() error = %v", err)
	}

	// Verify file was written
	if _, err := os.Stat(sessionPath); os.IsNotExist(err) {
		t.Error("WriteSession() did not create file")
	}
}
