//go:build integration

package integration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/proclive"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/stretchr/testify/require"
)

func TestStatusMultipleSessionsPreservedAcrossWorktrees(t *testing.T) {
	t.Parallel()
	env := NewRepoWithCommit(t)
	env.RunCLI("enable", "--agent", agentClaudeCode, "--telemetry=false")
	linked := filepath.Join(t.TempDir(), "linked")
	testutil.RunGit(t, env.RepoDir, "worktree", "add", "-b", "status-linked", linked)

	store := session.NewStateStoreWithDir(filepath.Join(env.RepoDir, ".git", session.SessionStateDirName))
	lastActive := time.Now().UTC().Add(-time.Minute)
	for _, state := range []*session.State{
		{SessionID: "same-agent-main", WorktreeID: "main", WorktreePath: env.RepoDir, Branch: "main", StartedAt: lastActive.Add(-time.Hour), LastInteractionTime: &lastActive, Phase: session.PhaseActive, AgentType: types.AgentType(agentClaudeCode)},
		{SessionID: "same-agent-linked", WorktreeID: "linked", WorktreePath: linked, Branch: "status-linked", StartedAt: lastActive.Add(-2 * time.Hour), LastInteractionTime: &lastActive, Phase: session.PhaseActive, AgentType: types.AgentType(agentClaudeCode)},
	} {
		require.NoError(t, store.Save(t.Context(), state))
	}

	var got struct {
		ActiveSessions []struct {
			SessionID    string `json:"session_id"`
			WorktreePath string `json:"worktree_path"`
		} `json:"active_sessions"`
	}
	require.NoError(t, json.Unmarshal([]byte(env.RunCLI("status", "--json")), &got))
	require.Len(t, got.ActiveSessions, 2)
	require.Equal(t, []string{"same-agent-linked", "same-agent-main"}, []string{got.ActiveSessions[0].SessionID, got.ActiveSessions[1].SessionID})
	require.ElementsMatch(t, []string{linked, env.RepoDir}, []string{got.ActiveSessions[0].WorktreePath, got.ActiveSessions[1].WorktreePath})
}

func TestStatusReadOnlyPreservesDeadOwnerStateAndTranscript(t *testing.T) {
	t.Parallel()
	env := NewRepoWithCommit(t)
	env.RunCLI("enable", "--agent", agentClaudeCode, "--telemetry=false")
	transcriptPath := filepath.Join(env.RepoDir, "status-read-only.jsonl")
	require.NoError(t, os.WriteFile(transcriptPath, []byte("unchanged\n"), 0o600))
	store := session.NewStateStoreWithDir(filepath.Join(env.RepoDir, ".git", session.SessionStateDirName))
	lastActive := time.Now().UTC().Add(-time.Minute)
	state := &session.State{
		SessionID: "dead-owner-status", WorktreeID: "main", WorktreePath: env.RepoDir, Branch: "main",
		StartedAt: lastActive.Add(-time.Hour), LastInteractionTime: &lastActive,
		Phase: session.PhaseActive, AgentType: types.AgentType(agentClaudeCode),
		Owner: &proclive.Identity{PID: os.Getpid(), Start: "not-this-process"}, TranscriptPath: transcriptPath,
	}
	require.NoError(t, store.Save(t.Context(), state))
	statePath := filepath.Join(env.RepoDir, ".git", session.SessionStateDirName, state.SessionID+".json")
	beforeState, err := os.ReadFile(statePath)
	require.NoError(t, err)
	beforeStateInfo, err := os.Stat(statePath)
	require.NoError(t, err)
	beforeTranscript, err := os.ReadFile(transcriptPath)
	require.NoError(t, err)
	beforeTranscriptInfo, err := os.Stat(transcriptPath)
	require.NoError(t, err)

	text := env.RunCLI("status")
	jsonStatus := env.RunCLI("status", "--json")
	require.Contains(t, text, "dead-owner-status")
	require.Contains(t, text, "exited")
	require.Contains(t, jsonStatus, `"status":"exited"`)
	afterState, err := os.ReadFile(statePath)
	require.NoError(t, err)
	afterStateInfo, err := os.Stat(statePath)
	require.NoError(t, err)
	afterTranscript, err := os.ReadFile(transcriptPath)
	require.NoError(t, err)
	afterTranscriptInfo, err := os.Stat(transcriptPath)
	require.NoError(t, err)
	require.Equal(t, beforeState, afterState)
	require.Equal(t, beforeStateInfo.ModTime(), afterStateInfo.ModTime())
	require.Equal(t, beforeTranscript, afterTranscript)
	require.Equal(t, beforeTranscriptInfo.ModTime(), afterTranscriptInfo.ModTime())
}
