package strategy

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/benchutil"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
)

// BenchmarkPrepareCommitMsg measures the full PrepareCommitMsg hook execution time
// at various repo sizes and session counts.
//
// Setup: N files in a git repo, M active sessions with shadow branch checkpoints,
// modified files staged for commit, and a temporary commit message file.
// PrepareCommitMsg reads session states, checks for new content (getStagedFiles,
// transcript comparison, content overlap), extracts the last prompt, and writes
// the checkpoint trailer to the message file.
func BenchmarkPrepareCommitMsg(b *testing.B) {
	b.Run("SmallRepo_1Session", benchPrepareCommitMsg(10, 1))
	b.Run("SmallRepo_3Sessions", benchPrepareCommitMsg(10, 3))
	b.Run("MediumRepo_1Session", benchPrepareCommitMsg(100, 1))
	b.Run("MediumRepo_3Sessions", benchPrepareCommitMsg(100, 3))
	b.Run("LargeRepo_1Session", benchPrepareCommitMsg(500, 1))
	b.Run("LargeRepo_3Sessions", benchPrepareCommitMsg(500, 3))
}

func benchPrepareCommitMsg(fileCount, sessionCount int) func(*testing.B) {
	return func(b *testing.B) {
		// Setup once before the loop — repo creation is expensive and
		// PrepareCommitMsg only mutates COMMIT_EDITMSG on the idle-session + no-TTY path.
		dir, commitMsgFile := benchSetupPrepareCommitMsgRepo(b, fileCount, sessionCount)
		b.Chdir(dir)

		b.ResetTimer()
		for range b.N {
			// Reset only what PrepareCommitMsg mutates
			if err := os.WriteFile(commitMsgFile, []byte("implement feature\n"), 0o644); err != nil {
				b.Fatalf("rewrite commit msg: %v", err)
			}
			paths.ClearWorktreeRootCache()

			s := &ManualCommitStrategy{}
			if err := s.PrepareCommitMsg(context.Background(), commitMsgFile, ""); err != nil {
				b.Fatalf("PrepareCommitMsg: %v", err)
			}
		}
	}
}

// BenchmarkGetStagedFiles measures the isolated cost of getStagedFiles at different
// repo sizes. This is the primary bottleneck: go-git's worktree.Status() scans the
// entire working tree.
func BenchmarkGetStagedFiles(b *testing.B) {
	for _, fileCount := range []int{10, 100, 500} {
		b.Run(fmt.Sprintf("Files_%d", fileCount), func(b *testing.B) {
			// Setup once before the loop — repo creation + staging is expensive.
			br := benchutil.NewBenchRepo(b, benchutil.RepoOpts{FileCount: fileCount})
			b.Chdir(br.Dir)

			// Stage some modifications
			for i := range min(5, fileCount) {
				name := fmt.Sprintf("src/file_%03d.go", i)
				content := benchutil.GenerateGoFile(9000+i, 100)
				br.WriteFile(b, name, content)
				wt, err := br.Repo.Worktree()
				if err != nil {
					b.Fatalf("worktree: %v", err)
				}
				if _, err := wt.Add(name); err != nil {
					b.Fatalf("add: %v", err)
				}
			}

			b.ResetTimer()
			for range b.N {
				paths.ClearWorktreeRootCache()

				if _, err := getStagedFiles(context.Background()); err != nil {
					b.Fatalf("getStagedFiles: %v", err)
				}
			}
		})
	}
}

// benchSetupPrepareCommitMsgRepo creates a git repo with N files, M sessions
// with shadow branch checkpoints, and staged modifications ready for PrepareCommitMsg.
// Returns the repo directory path and the path to the temporary commit message file.
func benchSetupPrepareCommitMsgRepo(b *testing.B, fileCount, sessionCount int) (string, string) {
	b.Helper()

	br := benchutil.NewBenchRepo(b, benchutil.RepoOpts{FileCount: fileCount})

	// Modify and stage files so getStagedFiles returns non-empty
	modifiedFiles := make([]string, 0, min(5, fileCount))
	for i := range min(5, fileCount) {
		modifiedFiles = append(modifiedFiles, fmt.Sprintf("src/file_%03d.go", i))
	}

	// Create sessions with shadow branch checkpoints
	for i := range sessionCount {
		sessionID := fmt.Sprintf("bench-pcm-session-%d", i)

		// Write transcript to metadata dir on disk (for live transcript fallback)
		transcript := benchutil.GenerateTranscript(benchutil.TranscriptOpts{
			MessageCount:    20,
			AvgMessageBytes: 300,
			IncludeToolUse:  true,
			FilesTouched:    modifiedFiles,
		})
		transcriptPath := br.WriteTranscriptFile(b, sessionID, transcript)

		// Seed shadow branch with checkpoint
		br.SeedShadowBranch(b, sessionID, 1, min(5, fileCount))

		// Create session state
		br.CreateSessionState(b, benchutil.SessionOpts{
			SessionID:      sessionID,
			Phase:          session.PhaseIdle,
			StepCount:      1,
			FilesTouched:   modifiedFiles,
			TranscriptPath: transcriptPath,
		})
	}

	// Now stage modifications (after shadow branch seeding, since SeedShadowBranch
	// writes files that overlap with what we stage)
	b.Chdir(br.Dir)
	paths.ClearWorktreeRootCache()

	wt, err := br.Repo.Worktree()
	if err != nil {
		b.Fatalf("worktree: %v", err)
	}
	for _, name := range modifiedFiles {
		content := benchutil.GenerateGoFile(8000, 100)
		br.WriteFile(b, name, content)
		if _, err := wt.Add(name); err != nil {
			b.Fatalf("add %s: %v", name, err)
		}
	}

	// Write temporary commit message file
	commitMsgFile := filepath.Join(br.Dir, ".git", "COMMIT_EDITMSG")

	if err := os.WriteFile(commitMsgFile, []byte("implement feature\n"), 0o644); err != nil {
		b.Fatalf("write commit msg: %v", err)
	}

	// Set ENTIRE_TEST_TTY=0 so hasTTY() returns false (simulates agent subprocess).
	// This avoids interactive TTY prompts during benchmarks.
	b.Setenv("ENTIRE_TEST_TTY", "0")

	return br.Dir, commitMsgFile
}
