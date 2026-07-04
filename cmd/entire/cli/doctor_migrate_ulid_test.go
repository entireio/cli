package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"

	cpkg "github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
	"github.com/entireio/cli/redact"
)

// Not parallel: uses t.Chdir so the command resolves the repo from CWD.
func TestDoctorMigrateToULID_EndToEnd(t *testing.T) {
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.IsolateGitConfigEnv(t)
	t.Chdir(dir)
	ctx := context.Background()

	repo, err := git.PlainOpen(dir)
	if err != nil {
		t.Fatal(err)
	}

	// Seed a hex checkpoint on the v1 branch and a user commit that references it.
	hexID := id.MustCheckpointID("a1b2c3d4e5f6")
	store := cpkg.NewGitStore(repo, cpkg.DefaultV1Refs())
	if err := store.Write(ctx, cpkg.Session{
		CheckpointID: hexID,
		SessionID:    "s1",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte(`{"type":"user","message":"hi"}` + "\n")),
		AuthorName:   "Test",
		AuthorEmail:  "test@example.com",
	}); err != nil {
		t.Fatalf("seed checkpoint: %v", err)
	}
	testutil.WriteFile(t, dir, "work.txt", "work")
	testutil.GitAdd(t, dir, "work.txt")
	testutil.GitCommit(t, dir, "do work\n\nEntire-Checkpoint: "+hexID.String())

	// Run the migration.
	cmd := newDoctorMigrateToULIDCmd()
	cmd.SetContext(ctx)
	cmd.SetArgs([]string{"--yes"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("migrate-to-ulid failed: %v\n%s", err, out.String())
	}

	// HEAD's trailer is now a ULID (and no longer the hex).
	repo2, err := git.PlainOpen(dir)
	if err != nil {
		t.Fatal(err)
	}
	head, err := repo2.Head()
	if err != nil {
		t.Fatal(err)
	}
	headCommit, err := repo2.CommitObject(head.Hash())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(headCommit.Message, hexID.String()) {
		t.Errorf("HEAD message still contains the hex id:\n%s", headCommit.Message)
	}
	cps := trailers.ParseAllCheckpoints(headCommit.Message)
	if len(cps) != 1 {
		t.Fatalf("expected one checkpoint trailer, got %v", cps)
	}
	ulid := cps[0]
	if ulid.Kind() != id.KindULID {
		t.Fatalf("HEAD trailer should be a ULID, got %q (kind %v)", ulid, ulid.Kind())
	}

	// The ULID ref exists and the migrated checkpoint reads back re-stamped.
	refName, err := cpkg.RefName(ulid)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo2.Reference(refName, true); err != nil {
		t.Fatalf("ULID ref %s should exist: %v", refName, err)
	}

	// The repo is now configured on the git-refs backend (so pre-push flushes the
	// ULID ref queue and reads resolve as refs).
	cfg, err := settings.LoadCheckpointsConfig(ctx)
	if err != nil {
		t.Fatalf("load checkpoints config: %v", err)
	}
	if cfg == nil || cfg.Primary.Type != cpkg.BackendTypeGitRefs {
		t.Fatalf("checkpoints.primary should be git-refs after migration, got %+v", cfg)
	}

	// The entire/checkpoints/v1 branch is gone (refs-native).
	v1 := plumbing.NewBranchReferenceName(paths.MetadataBranchName)
	if _, err := repo2.Reference(v1, true); err == nil {
		t.Error("entire/checkpoints/v1 branch should have been deleted")
	}

	// The checkpoint resolves via the normal read path (kind routing → refs).
	stores, err := cpkg.Open(ctx, repo2, cpkg.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	summary, err := stores.Persistent.Read(ctx, ulid)
	if err != nil {
		t.Fatalf("read migrated checkpoint: %v", err)
	}
	if summary == nil {
		t.Fatal("migrated ULID checkpoint should resolve via Open().Persistent")
	}
	if summary.CheckpointID != ulid {
		t.Errorf("embedded checkpoint_id = %q, want %q", summary.CheckpointID, ulid)
	}
}
