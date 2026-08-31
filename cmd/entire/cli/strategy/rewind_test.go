package strategy

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	_ "github.com/entireio/cli/cmd/entire/cli/agent/claudecode" // Register agent for ResolveAgentForRewind tests
	_ "github.com/entireio/cli/cmd/entire/cli/agent/geminicli"  // Register agent for ResolveAgentForRewind tests
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	cpkg "github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/redact"
	"github.com/stretchr/testify/require"

	"github.com/go-git/go-git/v6"
)

// TestRestoreLogsOnly_KeepsExistingLocalLog verifies the default (non-force)
// behavior: a session log already present on disk is kept untouched and still
// reported so the caller prints its resume command. --force overwrites it.
func TestRestoreLogsOnly_KeepsExistingLocalLog(t *testing.T) {
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	t.Chdir(dir)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)
	t.Cleanup(func() { repo.Close() })

	agentName := types.AgentName("keep-existing-agent")
	agentType := types.AgentType("Keep Existing Agent")
	sessionDir := filepath.Join(dir, "keep-existing-sessions")
	require.NoError(t, os.MkdirAll(sessionDir, 0o750))
	agent.Register(agentName, func() agent.Agent {
		return &restoreLogsOnlyAgent{name: agentName, agentType: agentType, sessionDir: sessionDir}
	})

	ctx := context.Background()
	cpID := id.MustCheckpointID("abc111abc111")
	sessionID := "keep-existing-session"

	checkpointTranscript := []byte(`{"type":"user","timestamp":"2025-01-02T10:00:00Z","message":{"content":[{"type":"text","text":"from checkpoint"}]}}` + "\n")
	writeCommittedRewindCheckpoint(t, repo, cpID, sessionID, agentType, checkpointTranscript, time.Date(2025, 1, 2, 10, 0, 0, 0, time.UTC))

	// Pre-existing local log with a (different) timestamped entry.
	localPath := filepath.Join(sessionDir, sessionID+".jsonl")
	existingLocal := []byte(`{"type":"user","timestamp":"2025-06-01T10:00:00Z","message":{"content":[{"type":"text","text":"live local"}]}}` + "\n")
	require.NoError(t, os.WriteFile(localPath, existingLocal, 0o600))

	point := RewindPoint{IsLogsOnly: true, CheckpointID: cpID}

	// Non-force: keep the existing local log, but still report the session.
	var stdout, stderr bytes.Buffer
	restored, err := NewManualCommitStrategy().RestoreLogsOnly(ctx, &stdout, &stderr, point, false)
	require.NoError(t, err, "stderr: %s", stderr.String())
	require.Len(t, restored, 1, "stdout: %s", stdout.String())
	require.Contains(t, stdout.String(), "Keeping existing")

	got, err := os.ReadFile(localPath)
	require.NoError(t, err)
	require.Equal(t, string(existingLocal), string(got), "non-force restore must not overwrite an existing local log")

	// Force: overwrite from the checkpoint.
	restored, err = NewManualCommitStrategy().RestoreLogsOnly(ctx, io.Discard, io.Discard, point, true)
	require.NoError(t, err)
	require.Len(t, restored, 1)

	got, err = os.ReadFile(localPath)
	require.NoError(t, err)
	require.Equal(t, string(checkpointTranscript), string(got), "force restore must overwrite from the checkpoint")
}

func TestRestoredPromptPreviewFallsBackInOrder(t *testing.T) {
	t.Parallel()

	ag := &restoreLogsOnlyAgent{
		extractedPrompts: []string{
			"# AGENTS.md instructions for /repo\n\n<INSTRUCTIONS>\nskip me\n</INSTRUCTIONS>",
			"<environment_context>\n  <cwd>/repo</cwd>\n</environment_context>",
			"prompt from transcript",
		},
	}

	if got := restoredPromptPreview(ag, "prompt sidecar", []byte("transcript"), "review prompt"); got != "prompt sidecar" {
		t.Fatalf("sidecar prompt = %q, want prompt sidecar", got)
	}
	if got := restoredPromptPreview(ag, "", []byte("transcript"), "review prompt"); got != "review prompt" {
		t.Fatalf("review prompt = %q, want review prompt", got)
	}
	if got := restoredPromptPreview(ag, "", []byte("transcript"), ""); got != "prompt from transcript" {
		t.Fatalf("transcript prompt = %q, want prompt from transcript", got)
	}
}

func TestFirstDisplayPrompt(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		prompts []string
		want    string
	}{
		{
			name:    "first prompt is genuine",
			prompts: []string{"fix the login bug", "second prompt"},
			want:    "fix the login bug",
		},
		{
			name: "skips codex environment context prefix",
			prompts: []string{
				"<environment_context>\n  <cwd>/repo</cwd>\n  <shell>zsh</shell>\n</environment_context>",
				"investigate the flaky test",
			},
			want: "investigate the flaky test",
		},
		{
			name: "skips AGENTS.md instruction prefix",
			prompts: []string{
				"# AGENTS.md instructions for /repo\n\n<INSTRUCTIONS>\nread the docs\n</INSTRUCTIONS>",
				"add error handling",
			},
			want: "add error handling",
		},
		{
			name:    "skips empty and separator-only entries",
			prompts: []string{"   ", "---", "apply the fixes"},
			want:    "apply the fixes",
		},
		{
			name:    "only injected prompts yields empty",
			prompts: []string{"<environment_context>\n  <cwd>/repo</cwd>\n</environment_context>"},
			want:    "",
		},
		{name: "empty list yields empty", prompts: nil, want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := FirstDisplayPrompt(tt.prompts); got != tt.want {
				t.Errorf("FirstDisplayPrompt(%v) = %q, want %q", tt.prompts, got, tt.want)
			}
		})
	}
}

func TestResolveAgentForRewind(t *testing.T) {
	t.Parallel()

	t.Run("empty type returns error", func(t *testing.T) {
		t.Parallel()
		_, err := ResolveAgentForRewind("")
		if err == nil {
			t.Error("expected error for empty agent type")
		}
	})

	t.Run("Claude Code type resolves correctly", func(t *testing.T) {
		t.Parallel()
		ag, err := ResolveAgentForRewind("Claude Code")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ag.Name() != agent.AgentNameClaudeCode {
			t.Errorf("Name() = %q, want %q", ag.Name(), agent.AgentNameClaudeCode)
		}
	})

	t.Run("Gemini CLI type resolves correctly", func(t *testing.T) {
		t.Parallel()
		ag, err := ResolveAgentForRewind("Gemini CLI")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ag.Name() != agent.AgentNameGemini {
			t.Errorf("Name() = %q, want %q", ag.Name(), agent.AgentNameGemini)
		}
	})

	t.Run("unknown type returns error", func(t *testing.T) {
		t.Parallel()
		_, err := ResolveAgentForRewind("Nonexistent Agent")
		if err == nil {
			t.Error("expected error for unknown agent type")
		}
	})

	t.Run("dynamically registered agent resolves by type", func(t *testing.T) {
		t.Parallel()

		// Simulate what external.DiscoverAndRegister does: register an agent at runtime.
		testName := types.AgentName("test-external-rewind-agent")
		testType := types.AgentType("Entire Test External Rewind Agent")
		agent.Register(testName, func() agent.Agent {
			return &fakeExternalAgent{name: testName, agentType: testType}
		})

		ag, err := ResolveAgentForRewind(testType)
		if err != nil {
			t.Fatalf("expected dynamically registered agent to resolve, got error: %v", err)
		}
		if ag.Type() != testType {
			t.Errorf("Type() = %q, want %q", ag.Type(), testType)
		}
		if ag.Name() != testName {
			t.Errorf("Name() = %q, want %q", ag.Name(), testName)
		}
	})
}

func writeCommittedRewindCheckpoint(
	t *testing.T,
	repo *git.Repository,
	checkpointID id.CheckpointID,
	sessionID string,
	agentType types.AgentType,
	transcript []byte,
	createdAt time.Time,
) {
	t.Helper()

	err := cpkg.NewGitStore(repo, cpkg.DefaultV1Refs()).Write(context.Background(), cpkg.Session{
		CheckpointID: checkpointID,
		SessionID:    sessionID,
		CreatedAt:    createdAt,
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted(transcript),
		Prompts:      []string{"restore prompt"},
		Agent:        agentType,
		AuthorName:   "Test",
		AuthorEmail:  "test@example.com",
	})
	require.NoError(t, err)
}

type restoreLogsOnlyAgent struct {
	name             types.AgentName
	agentType        types.AgentType
	sessionDir       string
	extractedPrompts []string
}

var _ agent.Agent = (*restoreLogsOnlyAgent)(nil)

func (a *restoreLogsOnlyAgent) Name() types.AgentName                          { return a.name }
func (a *restoreLogsOnlyAgent) Type() types.AgentType                          { return a.agentType }
func (a *restoreLogsOnlyAgent) Description() string                            { return "restore logs test agent" }
func (a *restoreLogsOnlyAgent) IsPreview() bool                                { return false }
func (a *restoreLogsOnlyAgent) DetectPresence(_ context.Context) (bool, error) { return true, nil }
func (a *restoreLogsOnlyAgent) ProtectedDirs() []string                        { return nil }
func (a *restoreLogsOnlyAgent) ReadTranscript(string) ([]byte, error)          { return nil, nil }
func (a *restoreLogsOnlyAgent) ChunkTranscript(_ context.Context, content []byte, _ int) ([][]byte, error) {
	return [][]byte{content}, nil
}
func (a *restoreLogsOnlyAgent) ReassembleTranscript(chunks [][]byte) ([]byte, error) {
	var out []byte
	for _, chunk := range chunks {
		out = append(out, chunk...)
	}
	return out, nil
}
func (a *restoreLogsOnlyAgent) GetSessionID(*agent.HookInput) string { return "" }
func (a *restoreLogsOnlyAgent) GetSessionDir(string) (string, error) { return a.sessionDir, nil }
func (a *restoreLogsOnlyAgent) ResolveSessionFile(sessionDir, sessionID string) string {
	return filepath.Join(sessionDir, sessionID+".jsonl")
}
func (a *restoreLogsOnlyAgent) ReadSession(*agent.HookInput) (*agent.AgentSession, error) {
	return nil, nil //nolint:nilnil // Not used by this test agent.
}
func (a *restoreLogsOnlyAgent) WriteSession(_ context.Context, session *agent.AgentSession) error {
	if err := os.MkdirAll(filepath.Dir(session.SessionRef), 0o750); err != nil {
		return err
	}
	return os.WriteFile(session.SessionRef, session.NativeData, 0o600)
}
func (a *restoreLogsOnlyAgent) FormatResumeCommand(sessionID string) string {
	return "restore-logs " + sessionID
}

//nolint:unparam // error is always nil in this test helper; satisfies PromptExtractor.
func (a *restoreLogsOnlyAgent) ExtractPrompts(string, int) ([]string, error) {
	return a.extractedPrompts, nil
}

// fakeExternalAgent is a minimal Agent implementation for testing dynamic registration.
// It simulates an external agent that was discovered and registered at runtime.
type fakeExternalAgent struct {
	name      types.AgentName
	agentType types.AgentType
}

func (f *fakeExternalAgent) Name() types.AgentName                          { return f.name }
func (f *fakeExternalAgent) Type() types.AgentType                          { return f.agentType }
func (f *fakeExternalAgent) Description() string                            { return "Fake external agent" }
func (f *fakeExternalAgent) IsPreview() bool                                { return false }
func (f *fakeExternalAgent) DetectPresence(_ context.Context) (bool, error) { return false, nil }
func (f *fakeExternalAgent) ProtectedDirs() []string                        { return nil }
func (f *fakeExternalAgent) ReadTranscript(_ string) ([]byte, error)        { return nil, nil }
func (f *fakeExternalAgent) ChunkTranscript(_ context.Context, _ []byte, _ int) ([][]byte, error) {
	return nil, nil
}
func (f *fakeExternalAgent) ReassembleTranscript(_ [][]byte) ([]byte, error) { return nil, nil }
func (f *fakeExternalAgent) GetSessionID(_ *agent.HookInput) string          { return "" }
func (f *fakeExternalAgent) GetSessionDir(_ string) (string, error)          { return "", nil }
func (f *fakeExternalAgent) ResolveSessionFile(_, _ string) string           { return "" }
func (f *fakeExternalAgent) ReadSession(_ *agent.HookInput) (*agent.AgentSession, error) {
	return nil, nil //nolint:nilnil // test stub
}
func (f *fakeExternalAgent) WriteSession(_ context.Context, _ *agent.AgentSession) error { return nil }
func (f *fakeExternalAgent) FormatResumeCommand(_ string) string                         { return "" }
