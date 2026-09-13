package providers_test

import (
	"context"
	"testing"

	"github.com/entireio/cli/app/models"
	"github.com/entireio/cli/app/providers"
)

func TestGetRecentCommits(t *testing.T) {
	commitProvider := providers.NewDevCommitProvider(nil)
	ctx := context.Background()

	commits, err := commitProvider.GetRecentCommits(ctx, "repo-cli-btw", 10)
	if err != nil {
		t.Fatalf("Failed to retrieve commits: %v", err)
	}

	if len(commits) == 0 {
		t.Fatalf("Expected commits, got empty list")
	}

	first := commits[0]
	if first.SHA == "" || first.Message == "" {
		t.Errorf("Expected commit metadata to be populated, got SHA=%s, Message=%s", first.SHA, first.Message)
	}
}

func TestGetCommitDevelopmentContext_Available(t *testing.T) {
	commitProvider := providers.NewDevCommitProvider(nil)
	ctx := context.Background()

	// 3dbdf8b83c39 has a matching checkpoint in DevCheckpointProvider
	devCtx, err := commitProvider.GetCommitDevelopmentContext(ctx, "repo-cli-btw", "3dbdf8b83c39")
	if err != nil {
		t.Fatalf("Failed to get commit context: %v", err)
	}

	if devCtx.CheckpointStatus != models.CheckpointAvailable {
		t.Errorf("Expected status %s, got %s", models.CheckpointAvailable, devCtx.CheckpointStatus)
	}

	if !devCtx.HasCheckpoint || devCtx.Checkpoint == nil {
		t.Errorf("Expected Checkpoint to be present")
	}

	if devCtx.Source != "git_and_checkpoint" {
		t.Errorf("Expected source 'git_and_checkpoint', got %s", devCtx.Source)
	}
}

func TestGetCommitDevelopmentContext_MissingCheckpoint(t *testing.T) {
	commitProvider := providers.NewDevCommitProvider(nil)
	ctx := context.Background()

	// a1b2c3d4e5f6 does not have a checkpoint
	devCtx, err := commitProvider.GetCommitDevelopmentContext(ctx, "repo-cli-btw", "a1b2c3d4e5f6")
	if err != nil {
		t.Fatalf("Failed to get commit context: %v", err)
	}

	if devCtx.CheckpointStatus != models.CheckpointUnavailable {
		t.Errorf("Expected status %s, got %s", models.CheckpointUnavailable, devCtx.CheckpointStatus)
	}

	if devCtx.HasCheckpoint {
		t.Errorf("Expected HasCheckpoint to be false")
	}

	if devCtx.MissingContextReason == "" {
		t.Errorf("Expected MissingContextReason to explain missing checkpoint")
	}

	if devCtx.Source != "git_only" {
		t.Errorf("Expected source 'git_only', got %s", devCtx.Source)
	}
}
