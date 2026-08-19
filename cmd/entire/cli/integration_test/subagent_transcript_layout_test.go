//go:build integration

package integration

import (
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// TestSubagentCheckpoints_StoresSubagentTranscript asserts the task checkpoint
// captures the subagent's own transcript, in both layouts agents have used for it —
// see ResolveAgentTranscriptPath for which one wins and why the fallback exists.
//
// Claude Code writes an agent-<id>.meta.json sidecar next to the nested transcript;
// nothing in the CLI reads it, so these tests do not create one.
func TestSubagentCheckpoints_StoresSubagentTranscript(t *testing.T) {
	t.Parallel()

	const editedFile = "docs/red.md"

	tests := []struct {
		name          string
		taskToolUseID string
		subagentID    string
		write         func(s *Session, agentID string, changes []FileChange) string
	}{
		{
			name:          "current nested layout",
			taskToolUseID: "toolu_01LayoutABC123",
			subagentID:    "a0123456789abcdef",
			write:         (*Session).CreateSubagentTranscript,
		},
		{
			name:          "legacy sibling layout",
			taskToolUseID: "toolu_01LegacyABC123",
			subagentID:    "afedcba9876543210",
			write:         (*Session).CreateLegacySubagentTranscript,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			// Each subtest asserts a different on-disk layout, so they cannot share a repo.
			env := NewFeatureBranchEnv(t)
			session := env.NewSession()
			session.CreateTranscript("delegate "+editedFile+" to a subagent", nil)
			tt.write(session, tt.subagentID, []FileChange{{Path: editedFile, Content: "content"}})

			if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
				t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
			}
			if err := env.SimulatePreTask(session.ID, session.TranscriptPath, tt.taskToolUseID); err != nil {
				t.Fatalf("SimulatePreTask failed: %v", err)
			}

			// The subagent edits a file; only its own transcript records the Write.
			env.WriteFile(editedFile, "Red is a warm colour.\n")

			if err := env.SimulatePostTask(PostTaskInput{
				SessionID:      session.ID,
				TranscriptPath: session.TranscriptPath,
				ToolUseID:      tt.taskToolUseID,
				AgentID:        tt.subagentID,
			}); err != nil {
				t.Fatalf("SimulatePostTask failed: %v", err)
			}

			wantPath := paths.EntireMetadataDir + "/" + session.ID +
				"/tasks/" + tt.taskToolUseID + "/" + paths.AgentTranscriptFileName(tt.subagentID)
			content, ok := env.ReadFileFromBranch(env.GetShadowBranchName(), wantPath)
			if !ok {
				t.Fatalf("subagent transcript not stored in shadow branch at %s", wantPath)
			}
			if !strings.Contains(content, editedFile) {
				t.Errorf("stored subagent transcript does not reference the subagent's edit: %q", content)
			}
		})
	}
}
