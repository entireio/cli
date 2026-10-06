package checkpoint

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
	gogit "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing/object"
)

// setupTestRepo creates a temporary git repo with some initial files.
func setupTestRepo(t *testing.T) *gogit.Repository {
	t.Helper()

	dir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}

	testutil.InitRepo(t, dir)
	repo, err := gogit.PlainOpen(dir)
	if err != nil {
		t.Fatalf("git open: %v", err)
	}

	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}

	// Create initial files
	for _, name := range []string{"file1.txt", "file2.txt", "file3.txt", "src/main.go"} {
		abs := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(abs), 0o750); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(abs, []byte("initial content of "+name), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		if _, err := wt.Add(name); err != nil {
			t.Fatalf("add: %v", err)
		}
	}

	// Create .gitignore
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(".entire/\n"), 0o600); err != nil {
		t.Fatalf("write .gitignore: %v", err)
	}
	if _, err := wt.Add(".gitignore"); err != nil {
		t.Fatalf("add .gitignore: %v", err)
	}

	_, err = wt.Commit("Initial commit", &gogit.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com"},
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}

	return repo
}
