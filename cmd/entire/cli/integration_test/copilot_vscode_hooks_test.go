//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent/copilotcli"
)

func TestCopilotVSCodeHooks_UserPromptSubmitted(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	runner := NewHookRunner(env.RepoDir, env.ClaudeProjectDir, t)

	sessionID := "b0ff98c0-8e01-4b73-bf92-9649b139931b"
	transcriptPath := filepath.Join(env.RepoDir, ".entire", "tmp", "copilot-events.jsonl")

	output := runner.runAgentHookWithOutput(
		"copilot-cli",
		"user-prompt-submitted",
		mustMarshalJSON(t, map[string]any{
			"timestamp":       "2026-04-08T15:04:05.000Z",
			"cwd":             env.RepoDir,
			"sessionId":       sessionID,
			"prompt":          "Create feature.txt",
			"hookEventName":   "UserPromptSubmit",
			"transcript_path": transcriptPath,
		}),
	)

	if output.Err != nil {
		t.Fatalf("user-prompt-submitted failed: %v\nstderr: %s\nstdout: %s", output.Err, output.Stderr, output.Stdout)
	}
	assertNoParseFailure(t, output)

	state, err := env.GetSessionState(sessionID)
	if err != nil {
		t.Fatalf("failed to read session state: %v", err)
	}
	if state == nil {
		t.Fatal("expected session state to be initialized")
	}
	if state.SessionID != sessionID {
		t.Fatalf("state.SessionID = %q, want %q", state.SessionID, sessionID)
	}
	if state.TranscriptPath != transcriptPath {
		t.Fatalf("state.TranscriptPath = %q, want %q", state.TranscriptPath, transcriptPath)
	}
	if state.LastPrompt != "Create feature.txt" {
		t.Fatalf("state.LastPrompt = %q, want %q", state.LastPrompt, "Create feature.txt")
	}
}

func TestCopilotVSCodeHooks_AgentStopCreatesCheckpoint(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	runner := NewHookRunner(env.RepoDir, env.ClaudeProjectDir, t)

	sessionID := "b0ff98c0-8e01-4b73-bf92-9649b139931c"
	transcriptPath := filepath.Join(env.RepoDir, ".entire", "tmp", sessionID, "events.jsonl")

	startOutput := runner.runAgentHookWithOutput(
		"copilot-cli",
		"user-prompt-submitted",
		mustMarshalJSON(t, map[string]any{
			"timestamp":       "2026-04-08T15:04:05.000Z",
			"cwd":             env.RepoDir,
			"sessionId":       sessionID,
			"prompt":          "Create feature.txt",
			"hookEventName":   "UserPromptSubmit",
			"transcript_path": transcriptPath,
		}),
	)
	if startOutput.Err != nil {
		t.Fatalf("user-prompt-submitted failed: %v\nstderr: %s\nstdout: %s", startOutput.Err, startOutput.Stderr, startOutput.Stdout)
	}
	assertNoParseFailure(t, startOutput)

	env.WriteFile("feature.txt", "created by copilot")
	writeCopilotTranscript(t, transcriptPath, "feature.txt", "Create feature.txt")

	stopOutput := runner.runAgentHookWithOutput(
		"copilot-cli",
		"agent-stop",
		mustMarshalJSON(t, map[string]any{
			"timestamp":       "2026-04-08T15:05:05.000Z",
			"cwd":             env.RepoDir,
			"sessionId":       sessionID,
			"stopReason":      "end_turn",
			"hookEventName":   "Stop",
			"transcript_path": transcriptPath,
		}),
	)
	if stopOutput.Err != nil {
		t.Fatalf("agent-stop failed: %v\nstderr: %s\nstdout: %s", stopOutput.Err, stopOutput.Stderr, stopOutput.Stdout)
	}
	assertNoParseFailure(t, stopOutput)

	state, err := env.GetSessionState(sessionID)
	if err != nil {
		t.Fatalf("failed to read session state: %v", err)
	}
	if state == nil {
		t.Fatal("expected session state after agent-stop")
	}
	if state.StepCount == 0 {
		t.Fatal("expected agent-stop to create at least one checkpoint")
	}
	if len(state.FilesTouched) == 0 {
		t.Fatal("expected FilesTouched to include transcript-derived changes")
	}
	if !slices.Contains(state.FilesTouched, "feature.txt") {
		t.Fatalf("expected FilesTouched to contain feature.txt, got %v", state.FilesTouched)
	}
}

func TestCopilotVSCodeHooks_GeneratedHookCommands(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	env.InitEntire()
	writeNonLocalDevSettings(t, env)
	runner := NewHookRunner(env.RepoDir, env.ClaudeProjectDir, t)

	output, err := env.RunCLIWithError("enable", "--agent", "copilot-cli")
	if err != nil {
		t.Fatalf("enable copilot-cli failed: %v\noutput: %s", err, output)
	}

	hooksFile := readCopilotHooksFile(t, env)
	userPromptCommand := resolveHookCommand(t, findHookCommand(t, hooksFile.Hooks.UserPromptSubmitted, "user-prompt-submitted"))
	agentStopCommand := resolveHookCommand(t, findHookCommand(t, hooksFile.Hooks.AgentStop, "agent-stop"))

	sessionID := "b0ff98c0-8e01-4b73-bf92-9649b139931d"
	transcriptPath := filepath.Join(env.RepoDir, ".entire", "tmp", sessionID, "events.jsonl")

	startOutput := runner.runShellHookCommandWithOutput(
		userPromptCommand,
		mustMarshalJSON(t, map[string]any{
			"timestamp":       "2026-04-08T15:04:05.000Z",
			"cwd":             env.RepoDir,
			"sessionId":       sessionID,
			"prompt":          "Create generated-hook.txt",
			"hookEventName":   "UserPromptSubmit",
			"transcript_path": transcriptPath,
		}),
	)
	if startOutput.Err != nil {
		t.Fatalf("generated user-prompt-submitted command failed: %v\nstderr: %s\nstdout: %s",
			startOutput.Err, startOutput.Stderr, startOutput.Stdout)
	}
	assertNoParseFailure(t, startOutput)

	env.WriteFile("generated-hook.txt", "created via generated hook command")
	writeCopilotTranscript(t, transcriptPath, "generated-hook.txt", "Create generated-hook.txt")

	stopOutput := runner.runShellHookCommandWithOutput(
		agentStopCommand,
		mustMarshalJSON(t, map[string]any{
			"timestamp":       "2026-04-08T15:05:05.000Z",
			"cwd":             env.RepoDir,
			"sessionId":       sessionID,
			"stopReason":      "end_turn",
			"hookEventName":   "Stop",
			"transcript_path": transcriptPath,
		}),
	)
	if stopOutput.Err != nil {
		t.Fatalf("generated agent-stop command failed: %v\nstderr: %s\nstdout: %s",
			stopOutput.Err, stopOutput.Stderr, stopOutput.Stdout)
	}
	assertNoParseFailure(t, stopOutput)

	state, stateErr := env.GetSessionState(sessionID)
	if stateErr != nil {
		t.Fatalf("failed to read session state: %v", stateErr)
	}
	if state == nil {
		t.Fatal("expected session state after generated hook commands")
	}
	if state.StepCount == 0 {
		t.Fatal("expected generated hook commands to create a checkpoint")
	}
	if !slices.Contains(state.FilesTouched, "generated-hook.txt") {
		t.Fatalf("expected FilesTouched to contain generated-hook.txt, got %v", state.FilesTouched)
	}
}

func mustMarshalJSON(t *testing.T, v any) []byte {
	t.Helper()

	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("failed to marshal JSON: %v", err)
	}
	return data
}

func assertNoParseFailure(t *testing.T, output HookOutput) {
	t.Helper()

	stderr := string(output.Stderr)
	if strings.Contains(stderr, "failed to parse hook input") {
		t.Fatalf("unexpected parse failure in stderr: %s", stderr)
	}
	if strings.Contains(stderr, "cannot unmarshal string into Go struct field") {
		t.Fatalf("unexpected schema mismatch in stderr: %s", stderr)
	}
	if strings.Contains(stderr, "Warning from") {
		t.Fatalf("unexpected warning in stderr: %s", stderr)
	}
}

func writeCopilotTranscript(t *testing.T, transcriptPath, modifiedFile, prompt string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(transcriptPath), 0o755); err != nil {
		t.Fatalf("failed to create transcript directory: %v", err)
	}

	lines := []string{
		`{"type":"session.start","data":{"sessionId":"sess"},"id":"1","timestamp":"2026-04-08T15:04:05Z","parentId":""}`,
		fmt.Sprintf(`{"type":"user.message","data":{"content":%q},"id":"2","timestamp":"2026-04-08T15:04:06Z","parentId":"1"}`, prompt),
		`{"type":"session.model_change","data":{"newModel":"claude-sonnet-4.6"},"id":"3","timestamp":"2026-04-08T15:04:07Z","parentId":"2"}`,
		`{"type":"assistant.message","data":{"content":"Working on it."},"id":"4","timestamp":"2026-04-08T15:04:08Z","parentId":"3"}`,
		fmt.Sprintf(`{"type":"tool.execution_complete","data":{"toolCallId":"tc1","model":"claude-sonnet-4.6","toolTelemetry":{"properties":{"filePaths":"[%q]"},"metrics":{"linesAdded":1,"linesRemoved":0}}},"id":"5","timestamp":"2026-04-08T15:04:09Z","parentId":"4"}`, modifiedFile),
		`{"type":"assistant.message","data":{"content":"Done."},"id":"6","timestamp":"2026-04-08T15:04:10Z","parentId":"5"}`,
	}

	content := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(transcriptPath, []byte(content), 0o644); err != nil {
		t.Fatalf("failed to write transcript: %v", err)
	}
}

func readCopilotHooksFile(t *testing.T, env *TestEnv) copilotcli.CopilotHooksFile {
	t.Helper()

	data, err := os.ReadFile(filepath.Join(env.RepoDir, ".github", "hooks", copilotcli.HooksFileName))
	if err != nil {
		t.Fatalf("failed to read copilot hooks file: %v", err)
	}

	var hooksFile copilotcli.CopilotHooksFile
	if err := json.Unmarshal(data, &hooksFile); err != nil {
		t.Fatalf("failed to parse copilot hooks file: %v", err)
	}
	return hooksFile
}

func findHookCommand(t *testing.T, entries []copilotcli.CopilotHookEntry, hookName string) string {
	t.Helper()

	for _, entry := range entries {
		if strings.Contains(entry.Bash, hookName) {
			return entry.Bash
		}
	}

	t.Fatalf("failed to find hook command for %s", hookName)
	return ""
}

func resolveHookCommand(t *testing.T, command string) string {
	t.Helper()

	return resolveHookCommandWithBinary(command, getTestBinary())
}

func TestResolveHookCommand_RewritesWrappedProductionCommand(t *testing.T) {
	t.Parallel()

	command := `sh -c 'if ! command -v entire >/dev/null 2>&1; then exit 0; fi; exec entire hooks copilot-cli user-prompt-submitted'`
	got := resolveHookCommandWithBinary(command, "/tmp/entire-test-binary")

	if strings.Contains(got, "command -v entire") {
		t.Fatalf("resolveHookCommand() should not depend on PATH lookup for entire, got %q", got)
	}
	if strings.Contains(got, "exec entire hooks") {
		t.Fatalf("resolveHookCommand() should rewrite wrapped exec target, got %q", got)
	}
	if !strings.Contains(got, `exec "/tmp/entire-test-binary" hooks copilot-cli user-prompt-submitted`) {
		t.Fatalf("resolveHookCommand() did not rewrite wrapped command correctly, got %q", got)
	}
}

func resolveHookCommandWithBinary(command, binaryPath string) string {
	testBinary := fmt.Sprintf("%q", binaryPath)

	if strings.HasPrefix(command, "entire ") {
		return testBinary + strings.TrimPrefix(command, "entire")
	}

	if strings.Contains(command, "command -v entire") && strings.Contains(command, "exec entire ") {
		resolved := strings.Replace(command,
			"command -v entire >/dev/null 2>&1",
			"test -x "+testBinary,
			1,
		)
		resolved = strings.Replace(resolved, "exec entire ", "exec "+testBinary+" ", 1)
		return resolved
	}

	return command
}

func writeNonLocalDevSettings(t *testing.T, env *TestEnv) {
	t.Helper()

	settingsPath := filepath.Join(env.RepoDir, ".entire", "settings.json")
	if err := os.WriteFile(settingsPath, []byte("{\n  \"enabled\": true\n}\n"), 0o644); err != nil {
		t.Fatalf("failed to write non-local-dev settings: %v", err)
	}
}
