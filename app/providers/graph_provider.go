package providers

import (
	"context"

	"github.com/entireio/cli/app/models"
)

// EntireGraphProvider defines the interface for querying Entire Graph structural evidence.
type EntireGraphProvider interface {
	GetGraphFindings(ctx context.Context, repoID string) ([]models.GraphFinding, error)
}

// DevGraphProvider provides development test data implementing EntireGraphProvider.
// NOTE: This provider returns development fixtures for testing core application foundation.
type DevGraphProvider struct{}

func NewDevGraphProvider() EntireGraphProvider {
	return &DevGraphProvider{}
}

func (p *DevGraphProvider) GetGraphFindings(ctx context.Context, repoID string) ([]models.GraphFinding, error) {
	return []models.GraphFinding{
		{
			ID:                 "graph-001",
			QueryChange:        "AST structure check on app/api/handlers.go",
			AffectedFiles:      []string{"app/api/handlers.go", "app/api/server.go"},
			AffectedFunctions:  []string{"RegisterRoutes", "HealthHandler", "RepositoryHandler"},
			Callers:            []string{"main.go"},
			RoutesTypes:        []string{"GET /api/health", "GET /api/repositories"},
			RiskInformation:    "Low risk — pure additive REST interface handlers",
			VerificationStatus: "VERIFIED",
			SourceEvidence:     "Entire Graph AST indexing & symbol relationship tree",
		},
	}, nil
}
