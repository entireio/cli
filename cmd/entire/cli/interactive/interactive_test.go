package interactive

import "testing"

func TestOverrideForTest_ForcedOn(t *testing.T) {
	restore := OverrideForTest(func() bool { return true })
	defer restore()
	if !CanPromptInteractively() {
		t.Error("CanPromptInteractively() = false; want true when override returns true")
	}
	if !CanPromptFromHook() {
		t.Error("CanPromptFromHook() = false; want true when override returns true")
	}
}

func TestOverrideForTest_ForcedOff(t *testing.T) {
	restore := OverrideForTest(func() bool { return false })
	defer restore()
	if CanPromptInteractively() {
		t.Error("CanPromptInteractively() = true; want false when override returns false")
	}
	if CanPromptFromHook() {
		t.Error("CanPromptFromHook() = true; want false when override returns false")
	}
}

func TestOverrideForTest_RestoresOriginal(t *testing.T) {
	origCLI := CanPromptInteractively()
	origHook := CanPromptFromHook()

	restore := OverrideForTest(func() bool { return !origCLI })
	restore()

	if CanPromptInteractively() != origCLI {
		t.Error("restore did not return CLI detection to original")
	}
	if CanPromptFromHook() != origHook {
		t.Error("restore did not return hook detection to original")
	}
}

func TestCanPromptFromHook_EnvVarForceOn(t *testing.T) {
	t.Setenv("ENTIRE_TEST_TTY", "1")
	if !CanPromptFromHook() {
		t.Error("CanPromptFromHook() = false; want true when ENTIRE_TEST_TTY=1")
	}
}

func TestCanPromptFromHook_EnvVarForceOff(t *testing.T) {
	t.Setenv("ENTIRE_TEST_TTY", "0")
	if CanPromptFromHook() {
		t.Error("CanPromptFromHook() = true; want false when ENTIRE_TEST_TTY=0")
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
