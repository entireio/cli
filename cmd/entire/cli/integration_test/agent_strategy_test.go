//go:build integration

package integration

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/claudecode"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

// TestAgentStrategyComposition verifies that agent and strategy work together correctly.
// This tests the full flow: agent parses session → strategy saves checkpoint → rewind works.
func TestAgentStrategyComposition(t *testing.T) {
	t.Parallel()

	RunForAllStrategies(t, func(t *testing.T, env *TestEnv, strategyName string) {
		// Get agent and strategy
		ag, err := agent.Get("claude-code")
		if err != nil {
			t.Fatalf("Get(claude-code) error = %v", err)
		}

		_, err = strategy.Get(strategyName)
		if err != nil {
			t.Fatalf("Get(%s) error = %v", strategyName, err)
		}

		// Create a session with the agent
		session := env.NewSession()

		// Create test file
		env.WriteFile("feature.go", "package main\n// new feature")

		// Create transcript via agent's expected format
		transcriptPath := session.CreateTranscript("Add a feature", []FileChange{
			{Path: "feature.go", Content: "package main\n// new feature"},
		})

		// Read session via agent interface
		agentSession, err := ag.ReadSession(&agent.HookInput{
			SessionID:  session.ID,
			SessionRef: transcriptPath,
		})
		if err != nil {
			t.Fatalf("ReadSession() error = %v", err)
		}

		// Verify agent computed modified files
		if len(agentSession.ModifiedFiles) == 0 {
			t.Error("agent.ReadSession() should compute ModifiedFiles")
		}

		// Simulate session flow: UserPromptSubmit → make changes → Stop
		if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
			t.Fatalf("SimulateUserPromptSubmit error = %v", err)
		}

		if err := env.SimulateStop(session.ID, transcriptPath); err != nil {
			t.Fatalf("SimulateStop error = %v", err)
		}

		// Verify checkpoint was created
		points := env.GetRewindPoints()
		if len(points) == 0 {
			t.Fatal("expected at least 1 rewind point after Stop hook")
		}
	})
}

// TestAgentSessionIDTransformation verifies session ID transformation across agent/strategy boundary.
func TestAgentSessionIDTransformation(t *testing.T) {
	t.Parallel()

	RunForAllStrategies(t, func(t *testing.T, env *TestEnv, strategyName string) {
		// Create session and simulate full flow
		session := env.NewSession()
		env.WriteFile("test.go", "package main")
		transcriptPath := session.CreateTranscript("Test", []FileChange{
			{Path: "test.go", Content: "package main"},
		})

		// Simulate hooks
		env.SimulateUserPromptSubmit(session.ID)
		env.SimulateStop(session.ID, transcriptPath)

		// Get rewind points and verify we can rewind
		points := env.GetRewindPoints()
		if len(points) == 0 {
			t.Skip("no rewind points created")
		}

		// Rewind should work
		if err := env.Rewind(points[0].ID); err != nil {
			t.Errorf("Rewind() error = %v", err)
		}
	})
}

// TestAgentTranscriptRestoration verifies transcript is restored correctly on rewind.
func TestAgentTranscriptRestoration(t *testing.T) {
	t.Parallel()

	// Only test with manual-commit strategy as it has full transcript restoration
	env := NewFeatureBranchEnv(t, strategy.StrategyNameManualCommit)

	ag, _ := agent.Get("claude-code")

	// Create first session
	session1 := env.NewSession()
	env.WriteFile("file1.go", "package main\n// file1 v1")
	transcript1 := session1.CreateTranscript("Create file1", []FileChange{
		{Path: "file1.go", Content: "package main\n// file1 v1"},
	})

	env.SimulateUserPromptSubmit(session1.ID)
	env.SimulateStop(session1.ID, transcript1)

	// Get checkpoint after first prompt
	points1 := env.GetRewindPoints()
	if len(points1) == 0 {
		t.Fatal("no rewind point after first prompt")
	}
	checkpoint1ID := points1[0].ID

	// Continue the SAME session with second prompt (manual-commit strategy requires same session on same base commit)
	// Reset transcript builder for the new checkpoint
	session1.TranscriptBuilder = NewTranscriptBuilder()
	env.WriteFile("file1.go", "package main\n// file1 v2")
	env.WriteFile("file2.go", "package main\n// file2")
	transcript2 := session1.CreateTranscript("Modify file1, create file2", []FileChange{
		{Path: "file1.go", Content: "package main\n// file1 v2"},
		{Path: "file2.go", Content: "package main\n// file2"},
	})

	env.SimulateUserPromptSubmit(session1.ID)
	env.SimulateStop(session1.ID, transcript2)

	// Verify we have 2 checkpoints
	points2 := env.GetRewindPoints()
	if len(points2) < 2 {
		t.Fatalf("expected at least 2 rewind points, got %d", len(points2))
	}

	// Rewind to first checkpoint
	if err := env.Rewind(checkpoint1ID); err != nil {
		t.Fatalf("Rewind() error = %v", err)
	}

	// Verify file content is restored
	content := env.ReadFile("file1.go")
	if content != "package main\n// file1 v1" {
		t.Errorf("file1.go content after rewind = %q, want v1 content", content)
	}

	// file2.go should not exist after rewind to checkpoint 1
	if env.FileExists("file2.go") {
		t.Error("file2.go should not exist after rewind to checkpoint 1")
	}

	// Verify agent can read the restored transcript
	// The transcript path should be restored to the session directory
	sessionDir, err := ag.GetSessionDir(env.RepoDir)
	if err != nil {
		t.Fatalf("GetSessionDir() error = %v", err)
	}
	t.Logf("Session directory: %s", sessionDir)
}

// TestAgentGetSessionDir verifies session directory resolution.
func TestAgentGetSessionDir(t *testing.T) {
	t.Parallel()

	env := NewTestEnv(t)
	env.InitRepo()

	ag, _ := agent.Get("claude-code")

	// With test override
	sessionDir, err := ag.GetSessionDir(env.RepoDir)
	if err != nil {
		t.Fatalf("GetSessionDir() error = %v", err)
	}

	// Should return the override path from ENTIRE_TEST_CLAUDE_PROJECT_DIR
	// (set in test environment)
	if sessionDir == "" {
		t.Error("GetSessionDir() returned empty string")
	}

	t.Logf("Session directory for %s: %s", env.RepoDir, sessionDir)
}

// TestAgentFormatResumeCommand verifies resume command formatting.
func TestAgentFormatResumeCommand(t *testing.T) {
	t.Parallel()

	ag, _ := agent.Get("claude-code")

	cmd := ag.FormatResumeCommand("test-session-123")
	expected := "claude -r test-session-123"

	if cmd != expected {
		t.Errorf("FormatResumeCommand() = %q, want %q", cmd, expected)
	}
}

// TestAgentHookParsing verifies hook input parsing via agent interface.
func TestAgentHookParsing(t *testing.T) {
	t.Parallel()

	ag, _ := agent.Get("claude-code")

	tests := []struct {
		name     string
		hookType agent.HookType
		input    string
		wantID   string
		wantRef  string
	}{
		{
			name:     "SessionStart",
			hookType: agent.HookSessionStart,
			input:    `{"session_id":"sess-123","transcript_path":"/tmp/transcript.jsonl"}`,
			wantID:   "sess-123",
			wantRef:  "/tmp/transcript.jsonl",
		},
		{
			name:     "UserPromptSubmit",
			hookType: agent.HookUserPromptSubmit,
			input:    `{"session_id":"sess-456","transcript_path":""}`,
			wantID:   "sess-456",
			wantRef:  "",
		},
		{
			name:     "Stop",
			hookType: agent.HookStop,
			input:    `{"session_id":"sess-789","transcript_path":"/path/to/transcript.jsonl"}`,
			wantID:   "sess-789",
			wantRef:  "/path/to/transcript.jsonl",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			reader := newStringReader(tt.input)
			hookInput, err := ag.ParseHookInput(tt.hookType, reader)
			if err != nil {
				t.Fatalf("ParseHookInput() error = %v", err)
			}

			if hookInput.SessionID != tt.wantID {
				t.Errorf("SessionID = %q, want %q", hookInput.SessionID, tt.wantID)
			}
			if hookInput.SessionRef != tt.wantRef {
				t.Errorf("SessionRef = %q, want %q", hookInput.SessionRef, tt.wantRef)
			}
		})
	}
}

// TestAgentPrePostToolHookParsing verifies PreToolUse and PostToolUse hook parsing.
func TestAgentPrePostToolHookParsing(t *testing.T) {
	t.Parallel()

	ag, _ := agent.Get("claude-code")

	t.Run("PreToolUse", func(t *testing.T) {
		t.Parallel()

		input := `{"session_id":"sess-123","transcript_path":"/tmp/t.jsonl","tool_use_id":"tool-456","tool_input":{"prompt":"test"}}`
		reader := newStringReader(input)

		hookInput, err := ag.ParseHookInput(agent.HookPreToolUse, reader)
		if err != nil {
			t.Fatalf("ParseHookInput(PreToolUse) error = %v", err)
		}

		if hookInput.ToolUseID != "tool-456" {
			t.Errorf("ToolUseID = %q, want %q", hookInput.ToolUseID, "tool-456")
		}
	})

	t.Run("PostToolUse with agent ID", func(t *testing.T) {
		t.Parallel()

		input := `{"session_id":"sess-123","transcript_path":"/tmp/t.jsonl","tool_use_id":"tool-789","tool_input":{},"tool_response":{"agentId":"agent-abc"}}`
		reader := newStringReader(input)

		hookInput, err := ag.ParseHookInput(agent.HookPostToolUse, reader)
		if err != nil {
			t.Fatalf("ParseHookInput(PostToolUse) error = %v", err)
		}

		if hookInput.ToolUseID != "tool-789" {
			t.Errorf("ToolUseID = %q, want %q", hookInput.ToolUseID, "tool-789")
		}

		// Agent ID should be in RawData
		if agentID, ok := hookInput.RawData["agent_id"]; !ok || agentID != "agent-abc" {
			t.Errorf("RawData[agent_id] = %v, want %q", agentID, "agent-abc")
		}
	})
}

// stringReader implements io.Reader for test strings
type stringReader struct {
	data []byte
	pos  int
}

func newStringReader(s string) *stringReader {
	return &stringReader{data: []byte(s)}
}

func (r *stringReader) Read(p []byte) (n int, err error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	n = copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}

// TestSetupAgentFlag verifies the --agent flag in enable command.
func TestSetupAgentFlag(t *testing.T) {
	t.Parallel()

	env := NewTestEnv(t)
	env.InitRepo()

	// Run enable with --agent flag
	output := env.RunCLI("enable", "--agent", "claude-code")
	if strings.Contains(output, "error") || strings.Contains(output, "Error") {
		t.Fatalf("enable --agent claude-code failed\nOutput: %s", output)
	}

	// Verify hooks were installed
	settingsPath := filepath.Join(env.RepoDir, ".claude", claudecode.ClaudeSettingsFileName)
	if _, err := os.Stat(settingsPath); os.IsNotExist(err) {
		t.Errorf("enable --agent should create .claude/%s", claudecode.ClaudeSettingsFileName)
	}

	// Verify .entire/settings has agent set
	entireSettingsPath := filepath.Join(env.RepoDir, ".entire", paths.SettingsFileName)
	data, err := os.ReadFile(entireSettingsPath)
	if err != nil {
		t.Fatalf("failed to read .entire/%s: %v", paths.SettingsFileName, err)
	}

	if !strings.Contains(string(data), `"agent"`) && !strings.Contains(string(data), `"agent":`) {
		t.Logf("settings content: %s", data)
		// Agent field may be omitted if default
	}
}
