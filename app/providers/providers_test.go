package providers

import (
	"context"
	"testing"

	"github.com/entireio/cli/app/models"
)

func TestDevCheckpointProvider(t *testing.T) {
	provider := NewDevCheckpointProvider()
	ctx := context.Background()

	cps, err := provider.GetCheckpoints(ctx, "test-repo")
	if err != nil {
		t.Fatalf("unexpected error getting checkpoints: %v", err)
	}

	if len(cps) == 0 {
		t.Errorf("expected checkpoints, got empty list")
	}

	single, err := provider.GetCheckpointByID(ctx, "test-repo", cps[0].CheckpointID)
	if err != nil {
		t.Fatalf("unexpected error getting checkpoint by ID: %v", err)
	}

	if single.CheckpointID != cps[0].CheckpointID {
		t.Errorf("expected CheckpointID %s, got %s", cps[0].CheckpointID, single.CheckpointID)
	}
}

func TestDevGraphProvider(t *testing.T) {
	provider := NewDevGraphProvider()
	ctx := context.Background()

	findings, err := provider.GetGraphFindings(ctx, "test-repo")
	if err != nil {
		t.Fatalf("unexpected error getting graph findings: %v", err)
	}

	if len(findings) == 0 {
		t.Errorf("expected graph findings, got empty list")
	}
}

func TestDevGitHubProvider(t *testing.T) {
	provider := NewDevGitHubProvider()
	ctx := context.Background()

	repo, err := provider.GetRepositoryInfo(ctx, "KAUSHALK123", "cli_BTW")
	if err != nil {
		t.Fatalf("unexpected error getting repo info: %v", err)
	}

	if repo.Owner != "KAUSHALK123" || repo.Name != "cli_BTW" {
		t.Errorf("unexpected repo data: %+v", repo)
	}
}

func TestDevAnalyzer(t *testing.T) {
	analyzer := NewDevAnalyzer()
	ctx := context.Background()

	reqs, err := analyzer.AnalyzeRequirements(ctx, "test-repo")
	if err != nil {
		t.Fatalf("unexpected error analyzing requirements: %v", err)
	}

	if len(reqs) == 0 {
		t.Errorf("expected requirements, got empty list")
	}
	if reqs[0].Status != models.StatusCompleted {
		t.Errorf("expected completed status, got %s", reqs[0].Status)
	}

	handoff, err := analyzer.GetHandoff(ctx, "test-repo")
	if err != nil {
		t.Fatalf("unexpected error getting handoff: %v", err)
	}
	if handoff.ID == "" {
		t.Errorf("expected non-empty handoff ID")
	}
}
