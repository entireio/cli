package gitrepo

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/testutil/gitenv"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/format/config"
)

func TestChunkPathsBoundsCommandsWithoutDroppingPaths(t *testing.T) {
	t.Parallel()

	paths := []string{
		strings.Repeat("a", 40),
		strings.Repeat("b", 40),
		strings.Repeat("c", 120), // A single path larger than the budget stays alone.
		strings.Repeat("d", 40),
	}
	const budget = 100
	chunks := chunkPaths(paths, budget)

	var got []string
	for _, chunk := range chunks {
		var size int
		for _, path := range chunk {
			got = append(got, path)
			size += len(path) + 1
		}
		if len(chunk) > 1 && size > budget {
			t.Fatalf("multi-path chunk has size %d, budget %d", size, budget)
		}
	}
	if !slices.Equal(got, paths) {
		t.Fatalf("chunked paths = %q, want %q", got, paths)
	}
}

func TestHashWorktreeFilesAppliesFiltersAcrossBatches(t *testing.T) {
	t.Parallel()
	dir := initWorktreeHashRepo(t)
	paths := []string{"star*.txt", "[abc].txt"}
	for _, path := range paths {
		if err := os.WriteFile(filepath.Join(dir, path), []byte("content\r\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// One path per command exercises aggregation across multiple batches.
	hashes, err := hashWorktreeFiles(t.Context(), dir, paths, len(paths[0])+1)
	if err != nil {
		t.Fatal(err)
	}
	want := blobHash(t, "content\n")
	for _, path := range paths {
		if got := hashes[path]; got != want {
			t.Errorf("hash for %q = %s, want clean-filtered %s", path, got, want)
		}
	}
}

func TestHashWorktreeFilesDoesNotRewriteStatStaleIndex(t *testing.T) {
	t.Parallel()
	dir := initWorktreeHashRepo(t)
	gitenv.Run(t, dir, "config", "user.name", "Test User")
	gitenv.Run(t, dir, "config", "user.email", "test@example.com")
	path := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(path, []byte("content\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitenv.Run(t, dir, "add", "--", "file.txt")
	gitenv.Run(t, dir, "commit", "-qm", "initial")

	indexPath := filepath.Join(dir, ".git", "index")
	before, err := os.Stat(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	staleTime := time.Now().Add(time.Second)
	if err := os.Chtimes(path, staleTime, staleTime); err != nil {
		t.Fatal(err)
	}

	if _, err := HashWorktreeFiles(t.Context(), dir, []string{"file.txt"}); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("HashWorktreeFiles rewrote the stat-stale index")
	}
}

func TestHashWorktreeFilesDoesNotRetrySpawnFailurePerPath(t *testing.T) {
	t.Setenv("PATH", "")

	_, err := hashWorktreeFiles(t.Context(), t.TempDir(), []string{"one", "two", "three"}, gitHashObjectPathBudget)
	if err == nil {
		t.Fatal("expected git spawn failure")
	}
	if got := strings.Count(err.Error(), "git hash-object"); got != 1 {
		t.Fatalf("git hash-object errors = %d, want one batch failure: %v", got, err)
	}
}

func TestHashWorktreeFilesKeepsSuccessfulHashesWhenOnePathFails(t *testing.T) {
	t.Parallel()
	dir := initWorktreeHashRepo(t)
	paths := []string{"first.txt", "missing.txt", "last.txt"}
	for _, path := range []string{paths[0], paths[2]} {
		if err := os.WriteFile(filepath.Join(dir, path), []byte("content\r\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	hashes, err := hashWorktreeFiles(t.Context(), dir, paths, gitHashObjectPathBudget)
	if err == nil || !strings.Contains(err.Error(), "missing.txt") {
		t.Fatalf("error = %v, want missing path diagnosis", err)
	}
	want := blobHash(t, "content\n")
	for _, path := range []string{paths[0], paths[2]} {
		if got := hashes[path]; got != want {
			t.Errorf("hash for %q = %s, want %s", path, got, want)
		}
	}
}

func initWorktreeHashRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitenv.Run(t, dir, "init", "-q")
	gitenv.Run(t, dir, "config", "core.autocrlf", "true")
	return dir
}

func blobHash(t *testing.T, content string) plumbing.Hash {
	t.Helper()
	h := plumbing.NewHasher(config.SHA1, plumbing.BlobObject, int64(len(content)))
	if _, err := h.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	return h.Sum()
}
