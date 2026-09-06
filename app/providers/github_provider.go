package providers

import (
	"context"

	"github.com/entireio/cli/app/models"
)

// GitHubProvider defines the interface for interacting with GitHub repositories.
type GitHubProvider interface {
	GetRepositoryInfo(ctx context.Context, owner, repo string) (*models.Repository, error)
}

// DevGitHubProvider provides development test data implementing GitHubProvider.
// NOTE: This provider returns development fixtures for testing core application foundation.
type DevGitHubProvider struct{}

func NewDevGitHubProvider() GitHubProvider {
	return &DevGitHubProvider{}
}

func (p *DevGitHubProvider) GetRepositoryInfo(ctx context.Context, owner, repo string) (*models.Repository, error) {
	return &models.Repository{
		ID:            "repo-btw-cli",
		Name:          repo,
		Owner:         owner,
		URL:           "https://github.com/" + owner + "/" + repo,
		LocalPath:     "d:\\scaler buildathon\\cli_BTW",
		DefaultBranch: "main",
		Description:   "Bengaluru Tech Week Buildathon 2026 — Entire Checkpoint Intelligence Application",
	}, nil
}
