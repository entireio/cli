//go:build e2e

package tests

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/e2e/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDeletedFilesCommitDeletion tests that deleting a file during a session
// doesn't break checkpointing. The deletion is bundled with a new file in a
// single commit (via git add .), not a separate git rm commit.
func TestDeletedFilesCommitDeletion(t *testing.T) {
	testutil.ForEachAgent(t, 3*time.Minute, func(t *testing.T, s *testutil.RepoState, ctx context.Context) {
		require.NoError(t, os.WriteFile(
			filepath.Join(s.Dir, "to_delete.go"),
			[]byte("package main\n\nfunc ToDelete() {}\n"), 0o644))
		s.Git(t, "add", "to_delete.go")
		s.Git(t, "commit", "--no-verify", "-m", "Add to_delete.go")

		_, err := s.RunPrompt(t, ctx,
			"Do two things: (1) Delete the file to_delete.go using rm. "+
				"(2) Create a new file replacement.go with content 'package main; func Replacement() {}'. "+
				"Do both tasks. Do not commit. "+
				"Do not ask for confirmation, just make the changes.")
		if err != nil {
			t.Fatalf("agent failed: %v", err)
		}

		assert.NoFileExists(t, filepath.Join(s.Dir, "to_delete.go"))
		testutil.AssertFileExists(t, s.Dir, "replacement.go")

		s.Git(t, "add", ".")
		s.Git(t, "commit", "-m", "Add replacement")
		testutil.WaitForCheckpoint(t, s, 30*time.Second)
		cpID1 := testutil.AssertHasCheckpointTrailer(t, s.Dir, "HEAD")
		testutil.AssertCheckpointExists(t, s.Dir, cpID1)

		// git add . already staged the deletion of to_delete.go above,
		// so no separate git rm + commit is needed.
		testutil.WaitForNoShadowBranches(t, s.Dir, 10*time.Second)
	})
}
