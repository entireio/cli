package checkpoint

import (
	"context"
	"testing"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing/object"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/redact"
)

func TestWrite_ImportedSurfacesOnList(t *testing.T) {
	t.Parallel()
	tempDir := t.TempDir()
	testutil.InitRepo(t, tempDir)
	repo, err := git.PlainOpen(tempDir)
	if err != nil {
		t.Fatalf("open repo: %v", err)
	}
	// Initial commit so HEAD exists.
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	testutil.WriteFile(t, tempDir, "f.txt", "x")
	if _, err := wt.Add("f.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Commit("init", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com"},
	}); err != nil {
		t.Fatal(err)
	}

	store := NewGitStore(repo, DefaultV1Refs())
	red, err := redact.JSONLBytes([]byte(`{"type":"user","uuid":"u1","message":{"role":"user","content":"hi"}}` + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	err = store.Write(context.Background(), Session{
		CheckpointID:     id.MustCheckpointID("aabbccddeeff"),
		SessionID:        "s1",
		Strategy:         "import",
		Kind:             "imported",
		Agent:            agent.AgentTypeClaudeCode,
		Transcript:       red,
		Prompts:          []string{"hi"},
		CheckpointsCount: 1,
	})
	if err != nil {
		t.Fatalf("write imported checkpoint: %v", err)
	}

	infos, err := store.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 || !infos[0].Imported {
		t.Fatalf("expected 1 imported checkpoint, got %+v", infos)
	}
}
