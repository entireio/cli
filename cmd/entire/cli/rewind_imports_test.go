package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing/object"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/redact"
)

func TestRefuseIfImportedCheckpoint(t *testing.T) {
	// Not parallel: uses t.Chdir for CWD-based repo resolution.
	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	repo, err := git.PlainOpen(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	testutil.WriteFile(t, repoDir, "f.txt", "x")
	if _, err := wt.Add("f.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Commit("init", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com"},
	}); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repoDir)

	cid := id.MustCheckpointID("aabbccddeeff")
	store := checkpoint.NewGitStore(repo, checkpoint.DefaultV1Refs())
	red, err := redact.JSONLBytes([]byte(`{"type":"user","uuid":"u1","message":{"role":"user","content":"hi"}}` + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Write(context.Background(), checkpoint.Session(checkpoint.WriteOptions{
		CheckpointID: cid, SessionID: "s", Strategy: "import", Kind: "imported",
		Transcript: red, Prompts: []string{"hi"}, CheckpointsCount: 1,
	})); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	err = refuseIfImportedCheckpoint(context.Background(), &out, cid.String())
	if err == nil {
		t.Fatal("expected refusal error for imported checkpoint")
	}
	if !strings.Contains(out.String(), "read-only and not rewindable") {
		t.Fatalf("missing clear refusal message, got: %q", out.String())
	}

	// A non-imported ID must not be refused.
	out.Reset()
	if err := refuseIfImportedCheckpoint(context.Background(), &out, "ffffffffffff"); err != nil {
		t.Fatalf("non-imported id should not be refused: %v", err)
	}
}
