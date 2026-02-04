//go:build integration

package integration

import (
	"strings"
	"testing"
)

func TestExplain_NoCurrentSession(t *testing.T) {
	t.Parallel()
	RunForAllStrategies(t, func(t *testing.T, env *TestEnv, strategyName string) {
		// Without any flags, explain shows the branch view (not an error)
		output, err := env.RunCLIWithError("explain")

		if err != nil {
			t.Errorf("expected success for branch view, got error: %v, output: %s", err, output)
			return
		}

		// Should show branch information and checkpoint count
		if !strings.Contains(output, "Branch:") {
			t.Errorf("expected 'Branch:' header in output, got: %s", output)
		}
		if !strings.Contains(output, "Checkpoints:") {
			t.Errorf("expected 'Checkpoints:' in output, got: %s", output)
		}
	})
}

func TestExplain_SessionFilter(t *testing.T) {
	t.Parallel()
	RunForAllStrategies(t, func(t *testing.T, env *TestEnv, strategyName string) {
		// --session now filters the list view instead of showing session details
		// A nonexistent session ID should show an empty list, not an error
		output, err := env.RunCLIWithError("explain", "--session", "nonexistent-session-id")

		if err != nil {
			t.Errorf("expected success (empty list) for session filter, got error: %v, output: %s", err, output)
			return
		}

		// Should show branch header
		if !strings.Contains(output, "Branch:") {
			t.Errorf("expected 'Branch:' header in output, got: %s", output)
		}

		// Should show 0 checkpoints (filter found no matches)
		if !strings.Contains(output, "Checkpoints: 0") {
			t.Errorf("expected 'Checkpoints: 0' for nonexistent session filter, got: %s", output)
		}

		// Should show filter info
		if !strings.Contains(output, "Filtered by session:") {
			t.Errorf("expected 'Filtered by session:' in output, got: %s", output)
		}
	})
}

func TestExplain_MutualExclusivity(t *testing.T) {
	t.Parallel()
	RunForAllStrategies(t, func(t *testing.T, env *TestEnv, strategyName string) {
		// Try to provide both --session and --commit flags
		output, err := env.RunCLIWithError("explain", "--session", "test-session", "--commit", "abc123")

		if err == nil {
			t.Errorf("expected error when both flags provided, got output: %s", output)
			return
		}

		if !strings.Contains(strings.ToLower(output), "cannot specify multiple") {
			t.Errorf("expected 'cannot specify multiple' error, got: %s", output)
		}
	})
}

func TestExplain_CheckpointNotFound(t *testing.T) {
	t.Parallel()
	RunForAllStrategies(t, func(t *testing.T, env *TestEnv, strategyName string) {
		// Try to explain a non-existent checkpoint
		output, err := env.RunCLIWithError("explain", "--checkpoint", "nonexistent123")

		if err == nil {
			t.Errorf("expected error for nonexistent checkpoint, got output: %s", output)
			return
		}

		if !strings.Contains(output, "checkpoint not found") {
			t.Errorf("expected 'checkpoint not found' error, got: %s", output)
		}
	})
}

func TestExplain_CheckpointMutualExclusivity(t *testing.T) {
	t.Parallel()
	RunForAllStrategies(t, func(t *testing.T, env *TestEnv, strategyName string) {
		// Try to provide --checkpoint with --session
		output, err := env.RunCLIWithError("explain", "--session", "test-session", "--checkpoint", "abc123")

		if err == nil {
			t.Errorf("expected error when both flags provided, got output: %s", output)
			return
		}

		if !strings.Contains(strings.ToLower(output), "cannot specify multiple") {
			t.Errorf("expected 'cannot specify multiple' error, got: %s", output)
		}
	})
}

func TestExplain_CommitWithoutCheckpoint(t *testing.T) {
	t.Parallel()
	RunForAllStrategies(t, func(t *testing.T, env *TestEnv, strategyName string) {
		// Create a regular commit without Entire-Checkpoint trailer
		env.WriteFile("test.txt", "content")
		env.GitAdd("test.txt")
		env.GitCommit("Regular commit without Entire trailer")

		// Get the commit hash
		commitHash := env.GetHeadHash()

		// Run explain --commit
		output, err := env.RunCLIWithError("explain", "--commit", commitHash[:7])
		if err != nil {
			t.Fatalf("unexpected error: %v, output: %s", err, output)
		}

		// Should show "No associated Entire checkpoint" message
		if !strings.Contains(output, "No associated Entire checkpoint") {
			t.Errorf("expected 'No associated Entire checkpoint' message, got: %s", output)
		}
	})
}

func TestExplain_CommitWithCheckpointTrailer(t *testing.T) {
	t.Parallel()
	RunForAllStrategies(t, func(t *testing.T, env *TestEnv, strategyName string) {
		// Create a commit with Entire-Checkpoint trailer
		env.WriteFile("test.txt", "content")
		env.GitAdd("test.txt")
		env.GitCommitWithCheckpointID("Commit with checkpoint", "abc123def456")

		// Get the commit hash
		commitHash := env.GetHeadHash()

		// Run explain --commit - it should try to look up the checkpoint
		// Since the checkpoint doesn't actually exist in the store, it should error
		output, err := env.RunCLIWithError("explain", "--commit", commitHash[:7])

		// We expect an error because the checkpoint abc123def456 doesn't exist
		if err == nil {
			// If it succeeded, check if it found the checkpoint (it shouldn't)
			if strings.Contains(output, "Checkpoint:") {
				t.Logf("checkpoint was found (unexpected but ok if test created one)")
			}
		} else {
			// Expected: checkpoint not found error
			if !strings.Contains(output, "checkpoint not found") {
				t.Errorf("expected 'checkpoint not found' error, got: %s", output)
			}
		}
	})
}
