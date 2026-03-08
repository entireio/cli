package benchutil

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"

	gogit "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing/object"
)

// --- WriteTemporary benchmarks ---
// WriteTemporary is the hot path that fires on every agent turn (SaveStep).
// It builds a git tree from changed files and commits to the shadow branch.

func BenchmarkWriteTemporary(b *testing.B) {
	b.Run("FirstCheckpoint_SmallRepo", benchWriteTemporaryFirstCheckpoint(10, 100))
	b.Run("FirstCheckpoint_LargeRepo", benchWriteTemporaryFirstCheckpoint(50, 500))
	b.Run("Incremental_FewFiles", benchWriteTemporaryIncremental(3, 0, 0))
	b.Run("Incremental_ManyFiles", benchWriteTemporaryIncremental(30, 10, 5))
	b.Run("Incremental_LargeFiles", benchWriteTemporaryIncrementalLargeFiles(2, 10000))
	b.Run("Dedup_NoChanges", benchWriteTemporaryDedup())
	b.Run("ManyPriorCheckpoints", benchWriteTemporaryWithHistory(50))
}

// benchWriteTemporaryFirstCheckpoint benchmarks the first checkpoint of a session.
// The first checkpoint captures all changed files via `git status`, which is heavier
// than incremental checkpoints.
func benchWriteTemporaryFirstCheckpoint(fileCount, fileSizeLines int) func(*testing.B) {
	return func(b *testing.B) {
		repo := NewBenchRepo(b, RepoOpts{
			FileCount:     fileCount,
			FileSizeLines: fileSizeLines,
		})
		sessionID := repo.CreateSessionState(b, SessionOpts{})

		// Modify a few files to create a dirty working directory
		for i := range 3 {
			name := fmt.Sprintf("src/file_%03d.go", i)
			repo.WriteFile(b, name, GenerateGoFile(9000+i, fileSizeLines))
		}

		metadataDir := paths.SessionMetadataDirFromSessionID(sessionID)
		metadataDirAbs := filepath.Join(repo.Dir, metadataDir)
		if err := os.MkdirAll(metadataDirAbs, 0o750); err != nil {
			b.Fatalf("mkdir metadata: %v", err)
		}

		// WriteTemporary uses paths.WorktreeRoot() which requires cwd to be in the repo.
		b.Chdir(repo.Dir)

		ctx := context.Background()
		b.ResetTimer()
		for i := range b.N {
			// Re-create the shadow branch state for each iteration so we always
			// measure the first-checkpoint path (which runs collectChangedFiles).
			// We use a unique session ID per iteration to get a fresh shadow branch.
			sid := fmt.Sprintf("bench-first-%d", i)
			_, writeErr := repo.Store.WriteTemporary(ctx, checkpoint.WriteTemporaryOptions{
				SessionID:         sid,
				BaseCommit:        repo.HeadHash,
				WorktreeID:        repo.WorktreeID,
				ModifiedFiles:     []string{"src/file_000.go", "src/file_001.go", "src/file_002.go"},
				MetadataDir:       metadataDir,
				MetadataDirAbs:    metadataDirAbs,
				CommitMessage:     "benchmark checkpoint",
				AuthorName:        "Bench",
				AuthorEmail:       "bench@test.com",
				IsFirstCheckpoint: true,
			})
			if writeErr != nil {
				b.Fatalf("WriteTemporary: %v", writeErr)
			}
		}
	}
}

// benchWriteTemporaryIncremental benchmarks subsequent checkpoints (not the first).
// These skip collectChangedFiles and only process the provided file lists.
func benchWriteTemporaryIncremental(modified, newFiles, deleted int) func(*testing.B) {
	return func(b *testing.B) {
		repo := NewBenchRepo(b, RepoOpts{
			FileCount:     max(modified+newFiles, 10),
			FileSizeLines: 100,
		})
		sessionID := repo.CreateSessionState(b, SessionOpts{})

		// Seed one checkpoint so subsequent ones are not IsFirstCheckpoint
		repo.SeedShadowBranch(b, sessionID, 1, 3)

		// Prepare file lists
		modifiedFiles := make([]string, 0, modified)
		for i := range modified {
			name := fmt.Sprintf("src/file_%03d.go", i)
			repo.WriteFile(b, name, GenerateGoFile(8000+i, 100))
			modifiedFiles = append(modifiedFiles, name)
		}
		newFileList := make([]string, 0, newFiles)
		for i := range newFiles {
			name := fmt.Sprintf("src/new_%03d.go", i)
			repo.WriteFile(b, name, GenerateGoFile(7000+i, 100))
			newFileList = append(newFileList, name)
		}
		deletedFiles := make([]string, 0, deleted)
		for i := range deleted {
			deletedFiles = append(deletedFiles, fmt.Sprintf("src/file_%03d.go", modified+newFiles+i))
		}

		metadataDir := paths.SessionMetadataDirFromSessionID(sessionID)
		metadataDirAbs := filepath.Join(repo.Dir, metadataDir)
		if err := os.MkdirAll(metadataDirAbs, 0o750); err != nil {
			b.Fatalf("mkdir metadata: %v", err)
		}

		b.Chdir(repo.Dir)

		ctx := context.Background()
		b.ResetTimer()
		for range b.N {
			_, writeErr := repo.Store.WriteTemporary(ctx, checkpoint.WriteTemporaryOptions{
				SessionID:         sessionID,
				BaseCommit:        repo.HeadHash,
				WorktreeID:        repo.WorktreeID,
				ModifiedFiles:     modifiedFiles,
				NewFiles:          newFileList,
				DeletedFiles:      deletedFiles,
				MetadataDir:       metadataDir,
				MetadataDirAbs:    metadataDirAbs,
				CommitMessage:     "benchmark checkpoint",
				AuthorName:        "Bench",
				AuthorEmail:       "bench@test.com",
				IsFirstCheckpoint: false,
			})
			if writeErr != nil {
				b.Fatalf("WriteTemporary: %v", writeErr)
			}
		}
	}
}

// benchWriteTemporaryIncrementalLargeFiles benchmarks checkpoints with large files.
func benchWriteTemporaryIncrementalLargeFiles(fileCount, linesPerFile int) func(*testing.B) {
	return func(b *testing.B) {
		repo := NewBenchRepo(b, RepoOpts{
			FileCount:     fileCount,
			FileSizeLines: linesPerFile,
		})
		sessionID := repo.CreateSessionState(b, SessionOpts{})
		repo.SeedShadowBranch(b, sessionID, 1, fileCount)

		modifiedFiles := make([]string, 0, fileCount)
		for i := range fileCount {
			name := fmt.Sprintf("src/file_%03d.go", i)
			repo.WriteFile(b, name, GenerateGoFile(6000+i, linesPerFile))
			modifiedFiles = append(modifiedFiles, name)
		}

		metadataDir := paths.SessionMetadataDirFromSessionID(sessionID)
		metadataDirAbs := filepath.Join(repo.Dir, metadataDir)
		if err := os.MkdirAll(metadataDirAbs, 0o750); err != nil {
			b.Fatalf("mkdir metadata: %v", err)
		}

		b.Chdir(repo.Dir)

		ctx := context.Background()
		b.ResetTimer()
		for range b.N {
			_, writeErr := repo.Store.WriteTemporary(ctx, checkpoint.WriteTemporaryOptions{
				SessionID:         sessionID,
				BaseCommit:        repo.HeadHash,
				WorktreeID:        repo.WorktreeID,
				ModifiedFiles:     modifiedFiles,
				MetadataDir:       metadataDir,
				MetadataDirAbs:    metadataDirAbs,
				CommitMessage:     "benchmark checkpoint",
				AuthorName:        "Bench",
				AuthorEmail:       "bench@test.com",
				IsFirstCheckpoint: false,
			})
			if writeErr != nil {
				b.Fatalf("WriteTemporary: %v", writeErr)
			}
		}
	}
}

// benchWriteTemporaryDedup benchmarks the dedup fast-path where the tree hash
// matches the previous checkpoint, so the write is skipped.
func benchWriteTemporaryDedup() func(*testing.B) {
	return func(b *testing.B) {
		repo := NewBenchRepo(b, RepoOpts{FileCount: 10})
		sessionID := repo.CreateSessionState(b, SessionOpts{})
		repo.SeedShadowBranch(b, sessionID, 1, 3)

		// Don't modify any files — tree will match the previous checkpoint
		metadataDir := paths.SessionMetadataDirFromSessionID(sessionID)
		metadataDirAbs := filepath.Join(repo.Dir, metadataDir)

		b.Chdir(repo.Dir)

		ctx := context.Background()
		b.ResetTimer()
		for range b.N {
			result, writeErr := repo.Store.WriteTemporary(ctx, checkpoint.WriteTemporaryOptions{
				SessionID:         sessionID,
				BaseCommit:        repo.HeadHash,
				WorktreeID:        repo.WorktreeID,
				ModifiedFiles:     []string{"src/file_000.go", "src/file_001.go", "src/file_002.go"},
				MetadataDir:       metadataDir,
				MetadataDirAbs:    metadataDirAbs,
				CommitMessage:     "benchmark checkpoint",
				AuthorName:        "Bench",
				AuthorEmail:       "bench@test.com",
				IsFirstCheckpoint: false,
			})
			if writeErr != nil {
				b.Fatalf("WriteTemporary: %v", writeErr)
			}
			if !result.Skipped {
				b.Fatal("expected dedup skip")
			}
		}
	}
}

// benchWriteTemporaryWithHistory benchmarks WriteTemporary when the shadow branch
// already has many prior checkpoint commits.
func benchWriteTemporaryWithHistory(priorCheckpoints int) func(*testing.B) {
	return func(b *testing.B) {
		repo := NewBenchRepo(b, RepoOpts{FileCount: 10})
		sessionID := repo.CreateSessionState(b, SessionOpts{})
		repo.SeedShadowBranch(b, sessionID, priorCheckpoints, 3)

		// Modify files for the new checkpoint
		for i := range 3 {
			name := fmt.Sprintf("src/file_%03d.go", i)
			repo.WriteFile(b, name, GenerateGoFile(5000+i, 100))
		}

		metadataDir := paths.SessionMetadataDirFromSessionID(sessionID)
		metadataDirAbs := filepath.Join(repo.Dir, metadataDir)
		if err := os.MkdirAll(metadataDirAbs, 0o750); err != nil {
			b.Fatalf("mkdir metadata: %v", err)
		}

		b.Chdir(repo.Dir)

		ctx := context.Background()
		b.ResetTimer()
		for range b.N {
			_, writeErr := repo.Store.WriteTemporary(ctx, checkpoint.WriteTemporaryOptions{
				SessionID:         sessionID,
				BaseCommit:        repo.HeadHash,
				WorktreeID:        repo.WorktreeID,
				ModifiedFiles:     []string{"src/file_000.go", "src/file_001.go", "src/file_002.go"},
				MetadataDir:       metadataDir,
				MetadataDirAbs:    metadataDirAbs,
				CommitMessage:     "benchmark checkpoint",
				AuthorName:        "Bench",
				AuthorEmail:       "bench@test.com",
				IsFirstCheckpoint: false,
			})
			if writeErr != nil {
				b.Fatalf("WriteTemporary: %v", writeErr)
			}
		}
	}
}

// --- WriteCommitted benchmarks ---
// WriteCommitted fires during PostCommit condensation when the user does `git commit`.
// It writes session metadata to the entire/checkpoints/v1 branch.

func BenchmarkWriteCommitted(b *testing.B) {
	b.Run("SmallTranscript", benchWriteCommitted(20, 500, 3, 0))
	b.Run("MediumTranscript", benchWriteCommitted(200, 500, 15, 0))
	b.Run("LargeTranscript", benchWriteCommitted(2000, 500, 50, 0))
	b.Run("HugeTranscript", benchWriteCommitted(10000, 1000, 100, 0))
	b.Run("EmptyMetadataBranch", benchWriteCommitted(200, 500, 15, 0))
	b.Run("FewPriorCheckpoints", benchWriteCommitted(200, 500, 15, 10))
	b.Run("ManyPriorCheckpoints", benchWriteCommitted(200, 500, 15, 200))
}

// benchWriteCommitted benchmarks writing to the entire/checkpoints/v1 branch.
func benchWriteCommitted(messageCount, avgMsgBytes, filesTouched, priorCheckpoints int) func(*testing.B) {
	return func(b *testing.B) {
		repo := NewBenchRepo(b, RepoOpts{
			FileCount: max(filesTouched, 10),
		})

		// Seed prior checkpoints if requested
		if priorCheckpoints > 0 {
			repo.SeedMetadataBranch(b, priorCheckpoints)
		}

		// Pre-generate transcript data (not part of the benchmark)
		files := make([]string, 0, filesTouched)
		for i := range filesTouched {
			files = append(files, fmt.Sprintf("src/file_%03d.go", i))
		}
		transcript := GenerateTranscript(TranscriptOpts{
			MessageCount:    messageCount,
			AvgMessageBytes: avgMsgBytes,
			IncludeToolUse:  true,
			FilesTouched:    files,
		})
		prompts := []string{"Implement the feature", "Fix the bug in handler"}

		b.ResetTimer()
		b.ReportMetric(float64(len(transcript)), "transcript_bytes")

		ctx := context.Background()
		for i := range b.N {
			cpID, err := id.Generate()
			if err != nil {
				b.Fatalf("generate ID: %v", err)
			}
			err = repo.Store.WriteCommitted(ctx, checkpoint.WriteCommittedOptions{
				CheckpointID:     cpID,
				SessionID:        fmt.Sprintf("bench-session-%d", i),
				Strategy:         "manual-commit",
				Transcript:       transcript,
				Prompts:          prompts,
				FilesTouched:     files,
				CheckpointsCount: 5,
				AuthorName:       "Bench",
				AuthorEmail:      "bench@test.com",
			})
			if err != nil {
				b.Fatalf("WriteCommitted: %v", err)
			}
		}
	}
}

// --- FlattenTree + BuildTreeFromEntries benchmarks ---
// These isolate the git plumbing cost that's shared by both hot paths.

func BenchmarkFlattenTree(b *testing.B) {
	b.Run("10files", benchFlattenTree(10, 100))
	b.Run("50files", benchFlattenTree(50, 100))
	b.Run("200files", benchFlattenTree(200, 50))
}

func benchFlattenTree(fileCount, fileSizeLines int) func(*testing.B) {
	return func(b *testing.B) {
		repo := NewBenchRepo(b, RepoOpts{
			FileCount:     fileCount,
			FileSizeLines: fileSizeLines,
		})

		// Get HEAD tree
		head, err := repo.Repo.Head()
		if err != nil {
			b.Fatalf("head: %v", err)
		}
		commit, err := repo.Repo.CommitObject(head.Hash())
		if err != nil {
			b.Fatalf("commit: %v", err)
		}
		tree, err := commit.Tree()
		if err != nil {
			b.Fatalf("tree: %v", err)
		}

		b.ResetTimer()
		for range b.N {
			entries := make(map[string]object.TreeEntry, fileCount)
			if err := checkpoint.FlattenTree(repo.Repo, tree, "", entries); err != nil {
				b.Fatalf("FlattenTree: %v", err)
			}
		}
	}
}

func BenchmarkBuildTreeFromEntries(b *testing.B) {
	b.Run("10entries", benchBuildTree(10))
	b.Run("50entries", benchBuildTree(50))
	b.Run("200entries", benchBuildTree(200))
}

func benchBuildTree(entryCount int) func(*testing.B) {
	return func(b *testing.B) {
		repo := NewBenchRepo(b, RepoOpts{
			FileCount: entryCount,
		})

		// Flatten the HEAD tree to get realistic entries
		head, err := repo.Repo.Head()
		if err != nil {
			b.Fatalf("head: %v", err)
		}
		commit, err := repo.Repo.CommitObject(head.Hash())
		if err != nil {
			b.Fatalf("commit: %v", err)
		}
		tree, err := commit.Tree()
		if err != nil {
			b.Fatalf("tree: %v", err)
		}
		entries := make(map[string]object.TreeEntry, entryCount)
		if err := checkpoint.FlattenTree(repo.Repo, tree, "", entries); err != nil {
			b.Fatalf("FlattenTree: %v", err)
		}

		// Open a fresh repo handle for building (to avoid storer cache effects)
		freshRepo, err := gogit.PlainOpen(repo.Dir)
		if err != nil {
			b.Fatalf("open: %v", err)
		}

		b.ResetTimer()
		for range b.N {
			_, buildErr := checkpoint.BuildTreeFromEntries(freshRepo, entries)
			if buildErr != nil {
				b.Fatalf("BuildTreeFromEntries: %v", buildErr)
			}
		}
	}
}
