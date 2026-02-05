package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"entire.io/cli/cmd/entire/cli/checkpoint"
	"entire.io/cli/cmd/entire/cli/checkpoint/id"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// BenchmarkGetBranchCheckpointsFull benchmarks the full getBranchCheckpoints function.
// Run with: go test -bench=BenchmarkGetBranchCheckpointsFull -benchtime=3s -run=^$ ./cmd/entire/cli/
func BenchmarkGetBranchCheckpointsFull(b *testing.B) {
	checkpointCounts := []int{5, 10, 20, 50}

	for _, count := range checkpointCounts {
		b.Run(fmt.Sprintf("checkpoints=%d", count), func(b *testing.B) {
			// Setup: create a test repo with N checkpoints
			repo, cleanup := setupBenchmarkRepo(b, count)
			defer cleanup()

			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				_, err := getBranchCheckpoints(repo, branchCheckpointsLimit)
				if err != nil {
					b.Fatalf("getBranchCheckpoints failed: %v", err)
				}
			}
		})
	}
}

// BenchmarkListCommitted benchmarks listing committed checkpoints.
func BenchmarkListCommitted(b *testing.B) {
	checkpointCounts := []int{5, 10, 20, 50, 100}

	for _, count := range checkpointCounts {
		b.Run(fmt.Sprintf("checkpoints=%d", count), func(b *testing.B) {
			repo, cleanup := setupBenchmarkRepoWithCommittedCheckpoints(b, count)
			defer cleanup()

			store := checkpoint.NewGitStore(repo)

			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				_, err := store.ListCommitted(context.Background())
				if err != nil {
					b.Fatalf("ListCommitted failed: %v", err)
				}
			}
		})
	}
}

// BenchmarkReadCommitted benchmarks reading a single committed checkpoint.
func BenchmarkReadCommitted(b *testing.B) {
	repo, cleanup := setupBenchmarkRepoWithCommittedCheckpoints(b, 20)
	defer cleanup()

	store := checkpoint.NewGitStore(repo)

	// Get a checkpoint ID to read
	checkpoints, err := store.ListCommitted(context.Background())
	if err != nil || len(checkpoints) == 0 {
		b.Fatalf("Failed to get checkpoints: %v", err)
	}
	checkpointID := checkpoints[0].CheckpointID

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		_, err := store.ReadCommitted(context.Background(), checkpointID)
		if err != nil {
			b.Fatalf("ReadCommitted failed: %v", err)
		}
	}
}

// BenchmarkReadCommittedAllCheckpoints simulates the getBranchCheckpoints behavior
// of reading every checkpoint to extract the first prompt. This is the main bottleneck.
func BenchmarkReadCommittedAllCheckpoints(b *testing.B) {
	checkpointCounts := []int{10, 20, 50, 100}

	for _, count := range checkpointCounts {
		b.Run(fmt.Sprintf("checkpoints=%d", count), func(b *testing.B) {
			repo, cleanup := setupBenchmarkRepoWithCommittedCheckpoints(b, count)
			defer cleanup()

			store := checkpoint.NewGitStore(repo)

			// Get all checkpoint IDs
			checkpoints, err := store.ListCommitted(context.Background())
			if err != nil {
				b.Fatalf("Failed to list checkpoints: %v", err)
			}

			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				// This simulates the explain.go behavior: read each checkpoint to get prompt
				for _, cp := range checkpoints {
					_, err := store.ReadCommitted(context.Background(), cp.CheckpointID)
					if err != nil {
						b.Fatalf("ReadCommitted failed: %v", err)
					}
				}
			}
		})
	}
}


// BenchmarkGetReachableTemporaryCheckpoints benchmarks the shadow branch reachability checks.
// This is the O(N*M) bottleneck where N = shadow branches and M = commit history depth.
// The test creates repos with 50 commits - multiply times by 10x for 500-commit repos.
func BenchmarkGetReachableTemporaryCheckpoints(b *testing.B) {
	shadowBranchCounts := []int{5, 10, 20}

	for _, count := range shadowBranchCounts {
		b.Run(fmt.Sprintf("shadowBranches=%d", count), func(b *testing.B) {
			repo, cleanup := setupBenchmarkRepoWithShadowBranches(b, count)
			defer cleanup()

			store := checkpoint.NewGitStore(repo)
			head, _ := repo.Head()

			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				// Simulate being on a feature branch (isOnDefault=false)
				// This triggers the O(N*M) reachability checks
				_ = getReachableTemporaryCheckpoints(repo, store, head.Hash(), false, branchCheckpointsLimit)
			}
		})
	}
}

// setupBenchmarkRepoWithShadowBranches creates a repo with shadow branches (temporary checkpoints).
func setupBenchmarkRepoWithShadowBranches(b *testing.B, numShadowBranches int) (*git.Repository, func()) {
	b.Helper()

	tmpDir, err := os.MkdirTemp("", "entire-benchmark-shadow-*")
	if err != nil {
		b.Fatalf("Failed to create temp dir: %v", err)
	}

	cleanup := func() {
		os.RemoveAll(tmpDir)
	}

	repo, err := git.PlainInit(tmpDir, false)
	if err != nil {
		cleanup()
		b.Fatalf("Failed to init repo: %v", err)
	}

	worktree, err := repo.Worktree()
	if err != nil {
		cleanup()
		b.Fatalf("Failed to get worktree: %v", err)
	}

	// Create initial commit
	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("initial"), 0644); err != nil {
		cleanup()
		b.Fatalf("Failed to write test file: %v", err)
	}

	if _, err := worktree.Add("test.txt"); err != nil {
		cleanup()
		b.Fatalf("Failed to add file: %v", err)
	}

	_, err = worktree.Commit("Initial commit", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test",
			Email: "test@example.com",
			When:  time.Now(),
		},
	})
	if err != nil {
		cleanup()
		b.Fatalf("Failed to commit: %v", err)
	}

	// Create more commits to simulate deeper history (more expensive reachability checks)
	for i := 0; i < 50; i++ {
		if err := os.WriteFile(testFile, []byte(fmt.Sprintf("content %d", i)), 0644); err != nil {
			cleanup()
			b.Fatalf("Failed to write test file: %v", err)
		}
		if _, err := worktree.Add("test.txt"); err != nil {
			cleanup()
			b.Fatalf("Failed to add file: %v", err)
		}
		_, err = worktree.Commit(fmt.Sprintf("Commit %d", i), &git.CommitOptions{
			Author: &object.Signature{
				Name:  "Test",
				Email: "test@example.com",
				When:  time.Now().Add(time.Duration(i) * time.Minute),
			},
		})
		if err != nil {
			cleanup()
			b.Fatalf("Failed to commit: %v", err)
		}
	}

	// Create metadata directory (required by WriteTemporary)
	metadataDir := filepath.Join(tmpDir, ".entire")
	if err := os.MkdirAll(metadataDir, 0755); err != nil {
		cleanup()
		b.Fatalf("Failed to create metadata dir: %v", err)
	}
	// Create a placeholder file in metadata dir
	if err := os.WriteFile(filepath.Join(metadataDir, "session.txt"), []byte("session"), 0644); err != nil {
		cleanup()
		b.Fatalf("Failed to write metadata file: %v", err)
	}

	// Create shadow branches using WriteTemporary
	store := checkpoint.NewGitStore(repo)
	for i := 0; i < numShadowBranches; i++ {
		sessionID := fmt.Sprintf("session-%d", i)

		// Create a unique base commit for each shadow branch by making another commit
		if err := os.WriteFile(testFile, []byte(fmt.Sprintf("shadow base %d", i)), 0644); err != nil {
			cleanup()
			b.Fatalf("Failed to write test file: %v", err)
		}
		if _, err := worktree.Add("test.txt"); err != nil {
			cleanup()
			b.Fatalf("Failed to add file: %v", err)
		}
		commitHash, err := worktree.Commit(fmt.Sprintf("Shadow base %d", i), &git.CommitOptions{
			Author: &object.Signature{
				Name:  "Test",
				Email: "test@example.com",
				When:  time.Now().Add(time.Duration(100+i) * time.Minute),
			},
		})
		if err != nil {
			cleanup()
			b.Fatalf("Failed to commit: %v", err)
		}
		baseCommit := commitHash.String()

		_, err = store.WriteTemporary(context.Background(), checkpoint.WriteTemporaryOptions{
			SessionID:      sessionID,
			BaseCommit:     baseCommit,
			CommitMessage:  fmt.Sprintf("Checkpoint for session %d", i),
			AuthorName:     "Test",
			AuthorEmail:    "test@example.com",
			ModifiedFiles:  []string{"test.txt"},
			MetadataDir:    ".entire",
			MetadataDirAbs: metadataDir,
		})
		if err != nil {
			cleanup()
			b.Fatalf("Failed to write temporary checkpoint: %v", err)
		}
	}

	return repo, cleanup
}

// setupBenchmarkRepo creates a test repository with N checkpoints for benchmarking.
// Returns the repo and a cleanup function.
func setupBenchmarkRepo(b *testing.B, numCheckpoints int) (*git.Repository, func()) {
	b.Helper()

	// Create temp directory
	tmpDir, err := os.MkdirTemp("", "entire-benchmark-*")
	if err != nil {
		b.Fatalf("Failed to create temp dir: %v", err)
	}

	cleanup := func() {
		os.RemoveAll(tmpDir)
	}

	// Initialize git repo
	repo, err := git.PlainInit(tmpDir, false)
	if err != nil {
		cleanup()
		b.Fatalf("Failed to init repo: %v", err)
	}

	// Create initial commit
	worktree, err := repo.Worktree()
	if err != nil {
		cleanup()
		b.Fatalf("Failed to get worktree: %v", err)
	}

	// Create a file and commit
	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("initial content"), 0644); err != nil {
		cleanup()
		b.Fatalf("Failed to write test file: %v", err)
	}

	if _, err := worktree.Add("test.txt"); err != nil {
		cleanup()
		b.Fatalf("Failed to add file: %v", err)
	}

	_, err = worktree.Commit("Initial commit", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test",
			Email: "test@example.com",
			When:  time.Now(),
		},
	})
	if err != nil {
		cleanup()
		b.Fatalf("Failed to commit: %v", err)
	}

	// Create checkpoints
	store := checkpoint.NewGitStore(repo)
	for i := 0; i < numCheckpoints; i++ {
		cpID, err := id.Generate()
		if err != nil {
			cleanup()
			b.Fatalf("Failed to generate checkpoint ID: %v", err)
		}

		// Write committed checkpoint
		err = store.WriteCommitted(context.Background(), checkpoint.WriteCommittedOptions{
			CheckpointID:     cpID,
			SessionID:        fmt.Sprintf("session-%d", i),
			Strategy:         "manual-commit",
			Branch:           "main",
			CheckpointsCount: 1,
			FilesTouched:     []string{fmt.Sprintf("file%d.txt", i)},
			Agent:            "Claude Code",
			Transcript:       []byte(fmt.Sprintf(`{"type":"user","content":"prompt %d"}`, i)),
			Prompts:          []string{fmt.Sprintf("prompt %d", i)},
			AuthorName:       "Test",
			AuthorEmail:      "test@example.com",
		})
		if err != nil {
			cleanup()
			b.Fatalf("Failed to write checkpoint: %v", err)
		}

		// Create a commit on main with the checkpoint trailer
		if err := os.WriteFile(testFile, []byte(fmt.Sprintf("content %d", i)), 0644); err != nil {
			cleanup()
			b.Fatalf("Failed to write test file: %v", err)
		}

		if _, err := worktree.Add("test.txt"); err != nil {
			cleanup()
			b.Fatalf("Failed to add file: %v", err)
		}

		_, err = worktree.Commit(fmt.Sprintf("Commit %d\n\nEntire-Checkpoint: %s", i, cpID), &git.CommitOptions{
			Author: &object.Signature{
				Name:  "Test",
				Email: "test@example.com",
				When:  time.Now().Add(time.Duration(i) * time.Minute),
			},
		})
		if err != nil {
			cleanup()
			b.Fatalf("Failed to commit: %v", err)
		}
	}

	return repo, cleanup
}

// setupBenchmarkRepoWithCommittedCheckpoints creates a repo with only committed checkpoints
// (no git commits with trailers, just the entire/sessions branch).
func setupBenchmarkRepoWithCommittedCheckpoints(b *testing.B, numCheckpoints int) (*git.Repository, func()) {
	b.Helper()

	tmpDir, err := os.MkdirTemp("", "entire-benchmark-*")
	if err != nil {
		b.Fatalf("Failed to create temp dir: %v", err)
	}

	cleanup := func() {
		os.RemoveAll(tmpDir)
	}

	repo, err := git.PlainInit(tmpDir, false)
	if err != nil {
		cleanup()
		b.Fatalf("Failed to init repo: %v", err)
	}

	// Create initial commit (required for branches to work)
	worktree, err := repo.Worktree()
	if err != nil {
		cleanup()
		b.Fatalf("Failed to get worktree: %v", err)
	}

	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("initial"), 0644); err != nil {
		cleanup()
		b.Fatalf("Failed to write test file: %v", err)
	}

	if _, err := worktree.Add("test.txt"); err != nil {
		cleanup()
		b.Fatalf("Failed to add file: %v", err)
	}

	_, err = worktree.Commit("Initial commit", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test",
			Email: "test@example.com",
			When:  time.Now(),
		},
	})
	if err != nil {
		cleanup()
		b.Fatalf("Failed to commit: %v", err)
	}

	// Create checkpoints on entire/sessions branch
	store := checkpoint.NewGitStore(repo)
	for i := 0; i < numCheckpoints; i++ {
		cpID, err := id.Generate()
		if err != nil {
			cleanup()
			b.Fatalf("Failed to generate checkpoint ID: %v", err)
		}

		// Create varied transcript sizes to simulate real data
		transcriptSize := 1000 + (i * 500) // 1KB to ~25KB
		transcript := make([]byte, transcriptSize)
		for j := range transcript {
			transcript[j] = byte('a' + (j % 26))
		}

		err = store.WriteCommitted(context.Background(), checkpoint.WriteCommittedOptions{
			CheckpointID:     cpID,
			SessionID:        fmt.Sprintf("session-%d", i),
			Strategy:         "manual-commit",
			Branch:           "main",
			CheckpointsCount: 1,
			FilesTouched:     []string{fmt.Sprintf("file%d.txt", i), fmt.Sprintf("other%d.go", i)},
			Agent:            "Claude Code",
			Transcript:       transcript,
			Prompts:          []string{fmt.Sprintf("This is a longer prompt for checkpoint %d that simulates real user input", i)},
			AuthorName:       "Test",
			AuthorEmail:      "test@example.com",
		})
		if err != nil {
			cleanup()
			b.Fatalf("Failed to write checkpoint: %v", err)
		}
	}

	return repo, cleanup
}

// TestGetBranchCheckpointsPerformanceProfile is a test that can be used with go tool pprof.
// Run with: go test -run=TestGetBranchCheckpointsPerformanceProfile -cpuprofile=cpu.prof ./cmd/entire/cli/
func TestGetBranchCheckpointsPerformanceProfile(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping performance profile test in short mode")
	}

	// Create a repo with 20 checkpoints
	repo, cleanup := setupBenchmarkRepoForTest(t, 20)
	defer cleanup()

	// Run getBranchCheckpoints multiple times to get meaningful profile data
	for i := 0; i < 100; i++ {
		_, err := getBranchCheckpoints(repo, branchCheckpointsLimit)
		if err != nil {
			t.Fatalf("getBranchCheckpoints failed: %v", err)
		}
	}
}

// setupBenchmarkRepoForTest is the test version of setupBenchmarkRepo.
func setupBenchmarkRepoForTest(t *testing.T, numCheckpoints int) (*git.Repository, func()) {
	t.Helper()

	tmpDir, err := os.MkdirTemp("", "entire-benchmark-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}

	cleanup := func() {
		os.RemoveAll(tmpDir)
	}

	repo, err := git.PlainInit(tmpDir, false)
	if err != nil {
		cleanup()
		t.Fatalf("Failed to init repo: %v", err)
	}

	worktree, err := repo.Worktree()
	if err != nil {
		cleanup()
		t.Fatalf("Failed to get worktree: %v", err)
	}

	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("initial content"), 0644); err != nil {
		cleanup()
		t.Fatalf("Failed to write test file: %v", err)
	}

	if _, err := worktree.Add("test.txt"); err != nil {
		cleanup()
		t.Fatalf("Failed to add file: %v", err)
	}

	_, err = worktree.Commit("Initial commit", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test",
			Email: "test@example.com",
			When:  time.Now(),
		},
	})
	if err != nil {
		cleanup()
		t.Fatalf("Failed to commit: %v", err)
	}

	store := checkpoint.NewGitStore(repo)
	for i := 0; i < numCheckpoints; i++ {
		cpID, err := id.Generate()
		if err != nil {
			cleanup()
			t.Fatalf("Failed to generate checkpoint ID: %v", err)
		}

		err = store.WriteCommitted(context.Background(), checkpoint.WriteCommittedOptions{
			CheckpointID:     cpID,
			SessionID:        fmt.Sprintf("session-%d", i),
			Strategy:         "manual-commit",
			Branch:           "main",
			CheckpointsCount: 1,
			FilesTouched:     []string{fmt.Sprintf("file%d.txt", i)},
			Agent:            "Claude Code",
			Transcript:       []byte(fmt.Sprintf(`{"type":"user","content":"prompt %d"}`, i)),
			Prompts:          []string{fmt.Sprintf("prompt %d", i)},
			AuthorName:       "Test",
			AuthorEmail:      "test@example.com",
		})
		if err != nil {
			cleanup()
			t.Fatalf("Failed to write checkpoint: %v", err)
		}

		if err := os.WriteFile(testFile, []byte(fmt.Sprintf("content %d", i)), 0644); err != nil {
			cleanup()
			t.Fatalf("Failed to write test file: %v", err)
		}

		if _, err := worktree.Add("test.txt"); err != nil {
			cleanup()
			t.Fatalf("Failed to add file: %v", err)
		}

		_, err = worktree.Commit(fmt.Sprintf("Commit %d\n\nEntire-Checkpoint: %s", i, cpID), &git.CommitOptions{
			Author: &object.Signature{
				Name:  "Test",
				Email: "test@example.com",
				When:  time.Now().Add(time.Duration(i) * time.Minute),
			},
		})
		if err != nil {
			cleanup()
			t.Fatalf("Failed to commit: %v", err)
		}
	}

	return repo, cleanup
}

