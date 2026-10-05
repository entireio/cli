//go:build integration

package integration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAttach_GrowingWorkspaceSession captures later messages from the same
// workspace-level session without changing earlier commit snapshots.
func TestAttach_GrowingWorkspaceSession(t *testing.T) {
	t.Parallel()
	ForEachBackend(t, func(t *testing.T, backend string) {
		env := NewFeatureBranchEnv(t)
		env.CheckpointStore = backend

		// The agent's project is the workspace above the repository. Force the
		// repo-local lookup to miss, exercising the cross-project fallback.
		fakeHome := t.TempDir()
		workspace := filepath.Dir(env.RepoDir)
		projectKey := strings.ReplaceAll(workspace, string(filepath.Separator), "-")
		projectDir := filepath.Join(fakeHome, ".claude", "projects", projectKey)
		if err := os.MkdirAll(projectDir, 0o700); err != nil {
			t.Fatal(err)
		}
		env.ExtraEnv = append(env.ExtraEnv, "HOME="+fakeHome)
		const sessionID = "attach-growing-workspace-session"
		transcriptPath := filepath.Join(projectDir, sessionID+".jsonl")
		tb := NewTranscriptBuilder()
		tb.AddUserMessage("FIRST_TURN: implement the first change")
		tb.AddAssistantMessage("The first change is complete.")
		if err := tb.WriteToFile(transcriptPath); err != nil {
			t.Fatal(err)
		}
		env.WriteFile("work.txt", "first change\n")
		env.GitAdd("work.txt")
		env.GitCommit("first change")
		t.Log(env.RunCLI("session", "attach", sessionID, "-a", agentClaudeCode, "-f"))
		firstID := env.GetCheckpointIDFromCommitMessage(env.GetHeadHash())
		if firstID == "" {
			t.Fatal("first attach did not link a checkpoint")
		}
		firstTranscript := env.RunCLI("checkpoint", "explain", firstID, "--transcript")
		if !strings.Contains(firstTranscript, "FIRST_TURN") {
			t.Fatal("first checkpoint is missing the initial conversation")
		}

		tb.AddUserMessage("SECOND_TURN: implement the next change")
		tb.AddAssistantMessage("The second change is complete.")
		if err := tb.WriteToFile(transcriptPath); err != nil {
			t.Fatal(err)
		}
		env.WriteFile("work.txt", "first change\nsecond change\n")
		env.GitAdd("work.txt")
		env.GitCommit("second change")
		t.Log(env.RunCLI("session", "attach", sessionID, "-a", agentClaudeCode, "-f"))
		secondID := env.GetCheckpointIDFromCommitMessage(env.GetHeadHash())
		if secondID == "" {
			t.Fatal("second attach did not link a checkpoint")
		}
		secondTranscript := env.RunCLI("checkpoint", "explain", secondID, "--transcript")
		t.Logf("first checkpoint=%s; second checkpoint=%s; stored transcript unchanged=%t",
			firstID, secondID, firstTranscript == secondTranscript)
		if !strings.Contains(secondTranscript, "FIRST_TURN") || !strings.Contains(secondTranscript, "SECOND_TURN") {
			t.Error("second commit's checkpoint must contain both turns of the same session")
		}
		if firstID == secondID {
			t.Error("later messages on a new commit should receive a new checkpoint")
		}
		if got := env.RunCLI("checkpoint", "explain", firstID, "--transcript"); got != firstTranscript {
			t.Error("second attach changed the first commit's conversation snapshot")
		}
		// An unchanged repeat must not amend HEAD or rewrite checkpoint storage.
		head := env.GetHeadHash()
		stored := env.RemoteCheckpointState(env.RepoDir)
		env.RunCLI("session", "attach", sessionID, "-a", agentClaudeCode, "-f")
		if env.GetHeadHash() != head || env.RemoteCheckpointState(env.RepoDir) != stored {
			t.Error("repeat attach mutated HEAD or checkpoint storage")
		}
		// Stored membership remains authoritative even if local state and the
		// live transcript have been cleaned up since the successful attach.
		for _, path := range []string{
			filepath.Join(env.RepoDir, ".git", "entire-sessions", sessionID+".json"),
			transcriptPath,
		} {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		}
		env.RunCLI("session", "attach", sessionID, "-a", agentClaudeCode, "-f")
		if env.GetHeadHash() != head || env.RemoteCheckpointState(env.RepoDir) != stored {
			t.Error("repeat attach without local state mutated HEAD or checkpoint storage")
		}
	})
}
