//go:build e2e

package tests

import (
	"context"
	"testing"
	"time"

	"github.com/entireio/cli/e2e/testutil"
)

// TestCheckpointMetadataDeepValidation runs deep validation on checkpoint metadata
// including transcript JSONL, content hash, and prompt content.
func TestCheckpointMetadataDeepValidation(t *testing.T) {
	testutil.ForEachAgent(t, 2*time.Minute, func(t *testing.T, s *testutil.RepoState, ctx context.Context) {
		prompt := "Create a file called validated.go with content 'package main; func Validated() {}'. " +
			"Create only this file. Do not commit. " +
			"Do not ask for confirmation, just make the change."

		_, err := s.RunPrompt(t, ctx, prompt)
		if err != nil {
			t.Fatalf("agent failed: %v", err)
		}

		testutil.AssertFileExists(t, s.Dir, "validated.go")

		s.Git(t, "add", ".")
		s.Git(t, "commit", "-m", "Add validated.go")
		testutil.WaitForCheckpoint(t, s, 30*time.Second)

		cpID := testutil.AssertHasCheckpointTrailer(t, s.Dir, "HEAD")
		testutil.ValidateCheckpointDeep(t, s.Dir, testutil.DeepCheckpointValidation{
			CheckpointID: cpID,
			Strategy:     "manual-commit",
			FilesTouched: []string{"validated.go"},
		})
		testutil.WaitForNoShadowBranches(t, s.Dir, 10*time.Second)
	})
}
