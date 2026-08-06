package checkpoint

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing/object"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/redact"
)

// newImportedTestStore builds a GitStore over a fresh temp repo with one
// commit (so HEAD exists) and returns it with a redacted one-line transcript,
// shared setup for the imported-checkpoint tests below.
func newImportedTestStore(t *testing.T) (*GitStore, redact.RedactedBytes) {
	t.Helper()
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

	red, err := redact.JSONLBytes([]byte(`{"type":"user","uuid":"u1","message":{"role":"user","content":"hi"}}` + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	return NewGitStore(repo, DefaultV1Refs()), red
}

func TestWrite_ImportedSurfacesOnList(t *testing.T) {
	t.Parallel()
	store, red := newImportedTestStore(t)
	err := store.Write(context.Background(), Session{
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

func TestImportedCheckpoint_CommitSHAPersisted(t *testing.T) {
	t.Parallel()
	store, red := newImportedTestStore(t)

	const commitSHA = "b01b59663fd4860fd15a9939499be44a14dbf168"
	ctx := context.Background()
	cid := id.MustCheckpointID("aabbccddeeff")
	err := store.Write(ctx, Session{
		CheckpointID:     cid,
		SessionID:        "s1",
		Strategy:         "import",
		Kind:             "imported",
		Agent:            agent.AgentTypeClaudeCode,
		Transcript:       red,
		Prompts:          []string{"hi"},
		CheckpointsCount: 1,
		CommitSHA:        commitSHA,
	})
	if err != nil {
		t.Fatalf("write imported checkpoint: %v", err)
	}

	md, err := store.ReadSessionMetadata(ctx, cid, 0)
	if err != nil {
		t.Fatalf("read session metadata: %v", err)
	}
	if md.CommitSHA != commitSHA {
		t.Fatalf("expected session metadata commit_sha %q, got %q", commitSHA, md.CommitSHA)
	}

	summary, err := store.Read(ctx, cid)
	if err != nil {
		t.Fatalf("read checkpoint summary: %v", err)
	}
	if summary.CommitSHA != commitSHA {
		t.Fatalf("expected checkpoint summary commit_sha %q, got %q", commitSHA, summary.CommitSHA)
	}

	// omitempty guard: a checkpoint written without CommitSHA must not surface
	// "commit_sha" in the marshaled metadata at all.
	cid2 := id.MustCheckpointID("112233445566")
	err = store.Write(ctx, Session{
		CheckpointID:     cid2,
		SessionID:        "s1",
		Strategy:         "import",
		Kind:             "imported",
		Agent:            agent.AgentTypeClaudeCode,
		Transcript:       red,
		Prompts:          []string{"hi"},
		CheckpointsCount: 1,
	})
	if err != nil {
		t.Fatalf("write second imported checkpoint: %v", err)
	}

	md2, err := store.ReadSessionMetadata(ctx, cid2, 0)
	if err != nil {
		t.Fatalf("read second session metadata: %v", err)
	}
	if md2.CommitSHA != "" {
		t.Fatalf("expected empty commit_sha, got %q", md2.CommitSHA)
	}
	rawMD, err := json.Marshal(md2)
	if err != nil {
		t.Fatalf("marshal metadata: %v", err)
	}
	if bytes.Contains(rawMD, []byte(`"commit_sha"`)) {
		t.Fatalf("expected marshaled metadata to omit commit_sha when unset, got %s", rawMD)
	}
}

// TestImportedCheckpoint_CommitSHASurvivesSummaryRewrite proves the root
// summary's preserve-on-rewrite: a later write to the SAME checkpoint that
// carries no CommitSHA (e.g. a review session attached to it) must not clear
// the stamped anchor from the root CheckpointSummary. Session-level Metadata
// is deliberately NOT preserved the same way — each session's metadata.json
// records what its own write carried.
func TestImportedCheckpoint_CommitSHASurvivesSummaryRewrite(t *testing.T) {
	t.Parallel()
	store, red := newImportedTestStore(t)

	const commitSHA = "b01b59663fd4860fd15a9939499be44a14dbf168"
	ctx := context.Background()
	cid := id.MustCheckpointID("ddeeff001122")
	err := store.Write(ctx, Session{
		CheckpointID:     cid,
		SessionID:        "s1",
		Strategy:         "import",
		Kind:             "imported",
		Agent:            agent.AgentTypeClaudeCode,
		Transcript:       red,
		Prompts:          []string{"hi"},
		CheckpointsCount: 1,
		CommitSHA:        commitSHA,
	})
	if err != nil {
		t.Fatalf("write imported checkpoint: %v", err)
	}

	// Second write to the same checkpoint, different session, no CommitSHA.
	err = store.Write(ctx, Session{
		CheckpointID:     cid,
		SessionID:        "s2",
		Strategy:         "manual-commit",
		Agent:            agent.AgentTypeClaudeCode,
		Transcript:       red,
		Prompts:          []string{"review it"},
		CheckpointsCount: 1,
	})
	if err != nil {
		t.Fatalf("second write to same checkpoint: %v", err)
	}

	summary, err := store.Read(ctx, cid)
	if err != nil {
		t.Fatalf("read checkpoint summary: %v", err)
	}
	if summary.CommitSHA != commitSHA {
		t.Fatalf("root summary commit_sha must survive a CommitSHA-less rewrite: expected %q, got %q", commitSHA, summary.CommitSHA)
	}
	if len(summary.Sessions) != 2 {
		t.Fatalf("expected both sessions in the rewritten summary, got %d", len(summary.Sessions))
	}
}
