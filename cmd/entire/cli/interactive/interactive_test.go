package interactive

import "testing"

func TestCanPromptInteractively_ForcedOn(t *testing.T) {
	restore := OverrideForTest(func() bool { return true })
	defer restore()
	if !CanPromptInteractively() {
		t.Error("CanPromptInteractively() = false; want true when override returns true")
	}
}

func TestCanPromptInteractively_ForcedOff(t *testing.T) {
	restore := OverrideForTest(func() bool { return false })
	defer restore()
	if CanPromptInteractively() {
		t.Error("CanPromptInteractively() = true; want false when override returns false")
	}
}

func TestOverrideForTest_RestoresOriginal(t *testing.T) {
	orig := CanPromptInteractively()

	restore := OverrideForTest(func() bool { return !orig })
	if CanPromptInteractively() == orig {
		t.Error("override did not take effect")
	}
	restore()

	if CanPromptInteractively() != orig {
		t.Error("restore did not return to original detection")
	}
}

func TestIsAgentSubprocess_GeminiCli(t *testing.T) {
	t.Setenv("GEMINI_CLI", "1")
	if !isAgentSubprocess() {
		t.Error("isAgentSubprocess() = false; want true when GEMINI_CLI is set")
	}
}

func TestIsAgentSubprocess_CopilotCli(t *testing.T) {
	t.Setenv("COPILOT_CLI", "1")
	if !isAgentSubprocess() {
		t.Error("isAgentSubprocess() = false; want true when COPILOT_CLI is set")
	}
}

func TestIsAgentSubprocess_PiCodingAgent(t *testing.T) {
	t.Setenv("PI_CODING_AGENT", "1")
	if !isAgentSubprocess() {
		t.Error("isAgentSubprocess() = false; want true when PI_CODING_AGENT is set")
	}
}

func TestIsAgentSubprocess_GitTerminalPromptZero(t *testing.T) {
	t.Setenv("GIT_TERMINAL_PROMPT", "0")
	if !isAgentSubprocess() {
		t.Error("isAgentSubprocess() = false; want true when GIT_TERMINAL_PROMPT=0")
	}
}

func TestIsAgentSubprocess_GitTerminalPromptOne(t *testing.T) {
	t.Setenv("GIT_TERMINAL_PROMPT", "1")
	// Clear other agent env vars so only GIT_TERMINAL_PROMPT drives detection.
	t.Setenv("GEMINI_CLI", "")
	t.Setenv("COPILOT_CLI", "")
	t.Setenv("PI_CODING_AGENT", "")
	if isAgentSubprocess() {
		t.Error("isAgentSubprocess() = true; want false when GIT_TERMINAL_PROMPT=1")
	}
}
