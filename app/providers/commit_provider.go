package providers

import (
	"context"
	"fmt"
	"time"

	"github.com/entireio/cli/app/models"
)

// CommitProvider defines the interface for retrieving commit history and mapping Entire Checkpoints.
type CommitProvider interface {
	GetRecentCommits(ctx context.Context, repoID string, limit int) ([]models.Commit, error)
	GetCommitBySHA(ctx context.Context, repoID, sha string) (*models.Commit, error)
	GetCommitDevelopmentContext(ctx context.Context, repoID, sha string) (*models.CommitDevelopmentContext, error)
}

// DevCommitProvider provides dev/test data implementing CommitProvider.
type DevCommitProvider struct {
	checkpointProvider EntireCheckpointProvider
}

func NewDevCommitProvider(cpProvider EntireCheckpointProvider) CommitProvider {
	if cpProvider == nil {
		cpProvider = NewDevCheckpointProvider()
	}
	return &DevCommitProvider{checkpointProvider: cpProvider}
}

func (p *DevCommitProvider) GetRecentCommits(ctx context.Context, repoID string, limit int) ([]models.Commit, error) {
	now := time.Now()
	commits := []models.Commit{
		{
			SHA:          "3dbdf8b83c391234567890abcdef1234567890ab",
			ShortSHA:     "3dbdf8b83c39",
			Message:      "feat(app): initial architectural understanding and foundation setup",
			AuthorName:   "KAUSHAL K",
			AuthorEmail:  "kaushal@example.com",
			Timestamp:    now.Add(-2 * time.Hour),
			FilesChanged: []string{"app/config/config.go", "app/models/repository.go", "app/api/server.go"},
			Additions:    120,
			Deletions:    5,
		},
		{
			SHA:          "78f4dc59700e9876543210fedcba09876543210f",
			ShortSHA:     "78f4dc59700e",
			Message:      "feat(providers): establish domain models and provider abstraction interfaces",
			AuthorName:   "KAUSHAL K",
			AuthorEmail:  "kaushal@example.com",
			Timestamp:    now.Add(-30 * time.Minute),
			FilesChanged: []string{"app/providers/checkpoint_provider.go", "app/providers/graph_provider.go"},
			Additions:    85,
			Deletions:    2,
		},
		{
			SHA:          "a1b2c3d4e5f678901234567890abcdef12345678",
			ShortSHA:     "a1b2c3d4e5f6",
			Message:      "chore: update dependencies and documentation baseline",
			AuthorName:   "Developer",
			AuthorEmail:  "dev@example.com",
			Timestamp:    now.Add(-10 * time.Minute),
			FilesChanged: []string{"BUILDATHON.md", "README.md"},
			Additions:    45,
			Deletions:    12,
		},
	}

	if limit > 0 && limit < len(commits) {
		return commits[:limit], nil
	}
	return commits, nil
}

func (p *DevCommitProvider) GetCommitBySHA(ctx context.Context, repoID, sha string) (*models.Commit, error) {
	commits, err := p.GetRecentCommits(ctx, repoID, 0)
	if err != nil {
		return nil, err
	}
	for _, c := range commits {
		if c.SHA == sha || c.ShortSHA == sha {
			return &c, nil
		}
	}
	return nil, fmt.Errorf("commit not found: %s", sha)
}

func (p *DevCommitProvider) GetCommitDevelopmentContext(ctx context.Context, repoID, sha string) (*models.CommitDevelopmentContext, error) {
	commit, err := p.GetCommitBySHA(ctx, repoID, sha)
	if err != nil {
		return nil, err
	}

	cps, err := p.checkpointProvider.GetCheckpoints(ctx, repoID)
	if err != nil {
		// Fallback to Git-only context if Checkpoint provider fails
		return &models.CommitDevelopmentContext{
			Commit:               *commit,
			CheckpointStatus:     models.CheckpointUnavailable,
			HasCheckpoint:        false,
			MissingContextReason: "Entire Checkpoint context unavailable for this commit session",
			Source:               "git_only",
		}, nil
	}

	for _, cp := range cps {
		if cp.CommitRef == commit.ShortSHA || cp.CommitRef == commit.SHA {
			return &models.CommitDevelopmentContext{
				Commit:           *commit,
				CheckpointStatus: models.CheckpointAvailable,
				Checkpoint:       &cp,
				HasCheckpoint:    true,
				Source:           "git_and_checkpoint",
			}, nil
		}
	}

	// Commit exists in Git history but has no associated Entire Checkpoint
	return &models.CommitDevelopmentContext{
		Commit:               *commit,
		CheckpointStatus:     models.CheckpointUnavailable,
		HasCheckpoint:        false,
		MissingContextReason: "No Entire Checkpoint was captured for this commit session (Git-only history)",
		Source:               "git_only",
	}, nil
}
