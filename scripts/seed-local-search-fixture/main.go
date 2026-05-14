// Build a tiny git repo with two committed checkpoints so the entire
// binary can be exercised against real data without spinning up an agent.
// Used by manual end-to-end verification of `entire checkpoint search --local`.
// Run: go run ./scripts/seed-local-search-fixture <repo-dir>
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/redact"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing/object"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: seed-local-search-fixture <repo-dir>")
		os.Exit(2)
	}
	dir := os.Args[1]
	if err := run(dir); err != nil {
		fmt.Fprintf(os.Stderr, "seed failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("seeded fixture repo at %s\n", dir)
}

func run(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		return fmt.Errorf("git init: %w", err)
	}
	// Disable gpg signing for the initial commit, otherwise PlainInit-created
	// repos pick up the user's global config and the commit will fail in CI.
	cmd := exec.Command("git", "-C", dir, "config", "commit.gpgsign", "false")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("disable gpgsign: %w: %s", err, out)
	}

	readme := filepath.Join(dir, "README.md")
	if err := os.WriteFile(readme, []byte("# fixture\n"), 0o644); err != nil {
		return fmt.Errorf("write README: %w", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		return fmt.Errorf("worktree: %w", err)
	}
	if _, err := wt.Add("README.md"); err != nil {
		return fmt.Errorf("git add: %w", err)
	}
	if _, err := wt.Commit("init", &git.CommitOptions{
		Author: &object.Signature{Name: "Fixture", Email: "fixture@example.com"},
	}); err != nil {
		return fmt.Errorf("git commit: %w", err)
	}

	store := checkpoint.NewGitStore(repo)
	fixtures := []struct {
		id           string
		branch       string
		filesTouched []string
		prompt       string
		transcript   string
	}{
		{
			id:           "a1b2c3d4e5f6",
			branch:       "main",
			filesTouched: []string{"src/auth.go"},
			prompt:       "Fix the login flow",
			transcript:   "assistant: I rewrote the Lefthook config to call entire instead.\n",
		},
		{
			id:           "b2c3d4e5f6a7",
			branch:       "feature",
			filesTouched: []string{"docs/intro.md"},
			prompt:       "Add intro docs",
			transcript:   "assistant: drafted an intro section about onboarding.\n",
		},
	}
	for _, f := range fixtures {
		if err := store.WriteCommitted(context.Background(), checkpoint.WriteCommittedOptions{
			CheckpointID: id.MustCheckpointID(f.id),
			SessionID:    "session-" + f.id,
			Strategy:     "manual-commit",
			Branch:       f.branch,
			FilesTouched: f.filesTouched,
			Prompts:      []string{f.prompt},
			Transcript:   redact.AlreadyRedacted([]byte(f.transcript)),
			AuthorName:   "Fixture",
			AuthorEmail:  "fixture@example.com",
		}); err != nil {
			return fmt.Errorf("write checkpoint %s: %w", f.id, err)
		}
	}
	return nil
}
