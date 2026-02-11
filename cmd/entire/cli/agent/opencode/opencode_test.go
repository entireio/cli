package opencode

import (
	"os"
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

func TestSupportsHooks_ReturnsFalse(t *testing.T) {
	t.Parallel()

	ag := &OpenCodeAgent{}
	if ag.SupportsHooks() {
		t.Error("SupportsHooks() = true, want false")
	}
}

func TestParseHookInput_ReturnsError(t *testing.T) {
	t.Parallel()

	ag := &OpenCodeAgent{}
	_, err := ag.ParseHookInput(agent.HookStop, strings.NewReader("{}"))
	if err == nil {
		t.Error("ParseHookInput() should return error for OpenCode")
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
