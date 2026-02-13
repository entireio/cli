//go:build integration

package integration

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/strategy"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

func TestImportSession_ImportToHEAD(t *testing.T) {
	t.Parallel()
	env := NewTestEnv(t)
	defer env.Cleanup()

	env.InitRepo()
	env.WriteFile("README.md", "# Test")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")
	env.InitEntire(strategy.StrategyNameManualCommit)

	// Create a valid Claude Code JSONL transcript
	transcriptContent := `{"type":"user","uuid":"u1","message":{"content":"Add a hello function"}}
{"type":"assistant","uuid":"a1","message":{"content":[{"type":"tool_use","id":"t1","name":"Write","input":{"file_path":"hello.go","contents":"package main\n\nfunc Hello() string { return \"hi\" }"}}]}}
{"type":"user","uuid":"u2","message":{"content":[{"type":"tool_result","tool_use_id":"t1"}]}}
`
	env.WriteFile("session.jsonl", transcriptContent)
	// WriteFile creates repo-relative - we need abs path for CLI
	absPath := filepath.Join(env.RepoDir, "session.jsonl")

	output := env.RunCLI("import-session", absPath)

	if !strings.Contains(output, "Imported 1 session") {
		t.Errorf("expected 'Imported 1 session' in output, got:\n%s", output)
	}
	if !strings.Contains(output, "To link this checkpoint to the commit") {
		t.Errorf("expected trailer instructions in output, got:\n%s", output)
	}
	if !strings.Contains(output, "Entire-Checkpoint") {
		t.Errorf("expected Entire-Checkpoint in output, got:\n%s", output)
	}

	// Verify checkpoint exists on entire/checkpoints/v1
	repo, err := git.PlainOpen(env.RepoDir)
	if err != nil {
		t.Fatalf("failed to open repo: %v", err)
	}
	_, err = repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	if err != nil {
		t.Fatalf("metadata branch not found: %v", err)
	}

	store := checkpoint.NewGitStore(repo)
	committed, err := store.ListCommitted(env.T.Context())
	if err != nil {
		t.Fatalf("ListCommitted failed: %v", err)
	}
	if len(committed) != 1 {
		t.Fatalf("expected 1 committed checkpoint, got %d", len(committed))
	}

	// Verify we can read the transcript
	content, err := store.ReadSessionContent(env.T.Context(), committed[0].CheckpointID, 0)
	if err != nil {
		t.Fatalf("ReadSessionContent failed: %v", err)
	}
	if !strings.Contains(string(content.Transcript), "Add a hello function") {
		t.Errorf("transcript should contain prompt, got: %s", string(content.Transcript)[:min(100, len(content.Transcript))])
	}
	if len(content.Metadata.FilesTouched) == 0 {
		t.Error("expected files_touched to include hello.go")
	}
}

func TestImportSession_MultipleSessions(t *testing.T) {
	t.Parallel()
	env := NewTestEnv(t)
	defer env.Cleanup()

	env.InitRepo()
	env.WriteFile("README.md", "# Test")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")
	env.InitEntire(strategy.StrategyNameManualCommit)

	transcript1 := `{"type":"user","uuid":"u1","message":{"content":"First task"}}
{"type":"assistant","uuid":"a1","message":{"content":[]}}
`
	transcript2 := `{"type":"user","uuid":"u2","message":{"content":"Second task"}}
{"type":"assistant","uuid":"a2","message":{"content":[]}}
`
	env.WriteFile("s1.jsonl", transcript1)
	env.WriteFile("s2.jsonl", transcript2)
	s1Path := filepath.Join(env.RepoDir, "s1.jsonl")
	s2Path := filepath.Join(env.RepoDir, "s2.jsonl")

	output := env.RunCLI("import-session", s1Path, s2Path)

	if !strings.Contains(output, "Imported 2 session(s)") {
		t.Errorf("expected 'Imported 2 session(s)', got:\n%s", output)
	}

	// Should have one checkpoint with two sessions
	repo, err := git.PlainOpen(env.RepoDir)
	if err != nil {
		t.Fatalf("failed to open repo: %v", err)
	}
	store := checkpoint.NewGitStore(repo)
	committed, err := store.ListCommitted(env.T.Context())
	if err != nil {
		t.Fatalf("ListCommitted failed: %v", err)
	}
	if len(committed) != 1 {
		t.Fatalf("expected 1 checkpoint (with 2 sessions), got %d checkpoints", len(committed))
	}

	// Read both sessions
	for i := 0; i < 2; i++ {
		content, err := store.ReadSessionContent(env.T.Context(), committed[0].CheckpointID, i)
		if err != nil {
			t.Fatalf("ReadSessionContent(%d) failed: %v", i, err)
		}
		if len(content.Transcript) == 0 {
			t.Errorf("session %d has empty transcript", i)
		}
	}
}

func TestImportSession_RequiresEnabled(t *testing.T) {
	t.Parallel()
	env := NewTestEnv(t)
	defer env.Cleanup()

	env.InitRepo()
	env.WriteFile("README.md", "# Test")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")

	// Create .entire with enabled: false (don't use InitEntire which sets enabled: true)
	env.WriteFile(".entire/settings.json", `{"strategy": "manual-commit", "enabled": false}`)

	transcriptPath := filepath.Join(env.RepoDir, "session.jsonl")
	env.WriteFile("session.jsonl", `{"type":"user","uuid":"u1","message":{"content":"test"}}`)

	_, err := env.RunCLIWithError("import-session", transcriptPath)
	if err == nil {
		t.Error("expected import-session to fail when Entire is disabled")
	}
}
