package providers

import (
	"context"

	"github.com/entireio/cli/app/models"
)

// RepositoryAnalyzer defines the interface for local repository structure analysis.
type RepositoryAnalyzer interface {
	AnalyzeRepository(ctx context.Context, localPath string) (*models.Repository, error)
}

// RequirementAnalyzer defines the interface for evaluating requirements completion against checkpoints.
type RequirementAnalyzer interface {
	AnalyzeRequirements(ctx context.Context, repoID string) ([]models.Requirement, error)
	GetHandoff(ctx context.Context, repoID string) (*models.Handoff, error)
}

// DevAnalyzer provides development test implementations for RepositoryAnalyzer and RequirementAnalyzer.
// NOTE: This provider returns development fixtures for testing core application foundation.
type DevAnalyzer struct{}

func NewDevAnalyzer() *DevAnalyzer {
	return &DevAnalyzer{}
}

func (a *DevAnalyzer) AnalyzeRepository(ctx context.Context, localPath string) (*models.Repository, error) {
	return &models.Repository{
		ID:            "repo-cli-btw",
		Name:          "cli_BTW",
		Owner:         "KAUSHALK123",
		URL:           "https://github.com/KAUSHALK123/cli_BTW",
		LocalPath:     localPath,
		DefaultBranch: "main",
		Description:   "Entire Checkpoint Intelligence & Release Readiness Platform",
	}, nil
}

func (a *DevAnalyzer) AnalyzeRequirements(ctx context.Context, repoID string) ([]models.Requirement, error) {
	return []models.Requirement{
		{
			ID:                   "REQ-001",
			Title:                "Core Application Foundation",
			Description:          "Establish domain models, REST API, provider abstractions, and clean application architecture",
			Status:               models.StatusCompleted,
			Source:               "GitHub Issue #1",
			RelatedCheckpoints:   []string{"cp-001-baseline", "cp-002-foundation"},
			RelatedFiles:         []string{"app/config/config.go", "app/models/repository.go", "app/api/server.go"},
			VerificationEvidence: "All Go unit tests passing; REST API endpoints serving valid JSON contracts",
		},
		{
			ID:                   "REQ-002",
			Title:                "Entire Checkpoint Intelligence Integration",
			Description:          "Extract intent context, tool execution history, and risk matrices from checkpoints",
			Status:               models.StatusNeedsVerification,
			Source:               "Track 1 Specification",
			RelatedCheckpoints:   []string{"cp-002-foundation"},
			RelatedFiles:         []string{"app/providers/checkpoint_provider.go"},
			VerificationEvidence: "Provider interface defined and dev mock integrated",
		},
	}, nil
}

func (a *DevAnalyzer) GetHandoff(ctx context.Context, repoID string) (*models.Handoff, error) {
	return &models.Handoff{
		ID:                    "handoff-001",
		OriginalIntent:        "Establish technical foundation for Checkpoint Intelligence application (Issue #1)",
		CompletedWork:         []string{"Domain models defined", "Provider interfaces established", "REST API handlers implemented", "Frontend dashboard created"},
		RemainingWork:         []string{"Live Entire Checkpoint transcript parser integration", "Live Graph query integration", "GitHub API client binding"},
		ImportantFiles:        []string{"app/models/", "app/providers/", "app/api/server.go", "BUILDATHON.md"},
		Risks:                 []string{"Ensure live Entire CLI commands handle non-zero exit codes safely"},
		LastCheckpoint:        "cp-002-foundation",
		RecommendedNextAction: "Proceed to Issue #2: Frontend Dashboard or Checkpoint Intelligence integration",
	}, nil
}
