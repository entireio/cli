//go:build integration

package integration

import (
	"testing"
)

// TestSubagentCheckpoints_StoresSubagentTranscript asserts the completed task
// record points at the subagent's own transcript, in both layouts agents have
// used for it — see ResolveAgentTranscriptPath for which one wins and why the
// fallback exists. The materializer follows the recorded path at condensation.
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
			subagentTranscriptPath := tt.write(session, tt.subagentID, []FileChange{{Path: editedFile, Content: "content"}})

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

			state, err := env.GetSessionState(session.ID)
			if err != nil {
				t.Fatalf("GetSessionState failed: %v", err)
			}
			rec := state.FindTaskRecord(tt.taskToolUseID)
			if rec == nil || rec.CompletedAt.IsZero() {
				t.Fatalf("expected a completed task record for %s, got %+v", tt.taskToolUseID, rec)
			}
			if rec.DeclaredTranscriptPath != subagentTranscriptPath {
				t.Errorf("the record must point at the layout-resolved subagent transcript: got %q, want %q",
					rec.DeclaredTranscriptPath, subagentTranscriptPath)
			}
			if !containsFile(rec.Files, editedFile) {
				t.Errorf("the record must carry the subagent's edit, got %v", rec.Files)
			}
		})
	}
}
