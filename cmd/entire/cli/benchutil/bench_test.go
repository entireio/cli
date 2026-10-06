package benchutil

import (
	"context"
	"fmt"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/redact"

	gogit "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing/object"
)

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
			redactedTranscript := redact.AlreadyRedacted(transcript)
			err = repo.Store.Write(ctx, checkpoint.Session{
				CheckpointID:     cpID,
				SessionID:        fmt.Sprintf("bench-session-%d", i),
				Strategy:         "manual-commit",
				Transcript:       redactedTranscript,
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
			_, buildErr := checkpoint.BuildTreeFromEntries(context.Background(), freshRepo, entries)
			if buildErr != nil {
				b.Fatalf("BuildTreeFromEntries: %v", buildErr)
			}
		}
	}
}
