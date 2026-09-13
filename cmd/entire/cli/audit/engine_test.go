package audit

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing/object"
)

func TestAuditEngineRun(t *testing.T) {
	tempDir := t.TempDir()

	repo, err := git.PlainInit(tempDir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	testFilePath := filepath.Join(tempDir, "main.go")
	content := `package main

// TODO: Implement user authentication
// FIXME: Fix potential memory leak in listener
func main() {
    println("Hello World")
}
`
	if err := os.WriteFile(testFilePath, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	_, err = worktree.Add("main.go")
	if err != nil {
		t.Fatalf("failed to add file: %v", err)
	}

	commitHash, err := worktree.Commit("Initial commit with Entire-Checkpoint: test-session", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test Developer",
			Email: "test@example.com",
			When:  time.Now(),
		},
	})
	if err != nil {
		t.Fatalf("failed to commit: %v", err)
	}

	engine := NewEngine(repo, tempDir)
	res, err := engine.Run(context.Background(), AuditOptions{
		MaxDepth:     10,
		IncludeGraph: true,
	})

	if err != nil {
		t.Fatalf("Audit Engine Run failed: %v", err)
	}

	if res.HeadCommit != commitHash.String() {
		t.Errorf("expected HeadCommit %s, got %s", commitHash.String(), res.HeadCommit)
	}

	if len(res.Intents) == 0 {
		t.Errorf("expected at least 1 intent item, got 0")
	}

	if len(res.Risks) < 2 {
		t.Errorf("expected at least 2 risk items (TODO and FIXME), got %d", len(res.Risks))
	}

	if res.ReadinessScore <= 0 || res.ReadinessScore > 100 {
		t.Errorf("invalid readiness score: %d", res.ReadinessScore)
	}

	markdownReport := RenderMarkdownReport(res)
	if markdownReport == "" {
		t.Errorf("markdown report should not be empty")
	}
}
