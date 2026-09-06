package providers

import (
	"context"
	"time"

	"github.com/entireio/cli/app/models"
)

// EntireCheckpointProvider defines the interface for interacting with Entire Checkpoints.
type EntireCheckpointProvider interface {
	GetCheckpoints(ctx context.Context, repoID string) ([]models.Checkpoint, error)
	GetCheckpointByID(ctx context.Context, repoID, cpID string) (*models.Checkpoint, error)
}

// DevCheckpointProvider provides development test data implementing EntireCheckpointProvider.
// NOTE: This provider returns development fixtures for testing core application foundation.
type DevCheckpointProvider struct{}

func NewDevCheckpointProvider() EntireCheckpointProvider {
	return &DevCheckpointProvider{}
}

func (p *DevCheckpointProvider) GetCheckpoints(ctx context.Context, repoID string) ([]models.Checkpoint, error) {
	now := time.Now()
	return []models.Checkpoint{
		{
			CheckpointID:  "cp-001-baseline",
			CommitRef:     "3dbdf8b83c39",
			Timestamp:     now.Add(-2 * time.Hour),
			IntentContext: "Initial architectural understanding and core foundation setup",
			FilesChanged:  []string{"app/config/config.go", "app/models/repository.go", "app/api/server.go"},
			AssociatedRequirements: []string{"REQ-001"},
			VerificationInfo: "Foundation interfaces and REST endpoints verified via unit tests",
		},
		{
			CheckpointID:  "cp-002-foundation",
			CommitRef:     "78f4dc59700e",
			Timestamp:     now.Add(-30 * time.Minute),
			IntentContext: "Establish domain models and provider abstraction interfaces",
			FilesChanged:  []string{"app/providers/checkpoint_provider.go", "app/providers/graph_provider.go"},
			AssociatedRequirements: []string{"REQ-001", "REQ-002"},
			VerificationInfo: "All provider interface methods satisfied by dev mock types",
		},
	}, nil
}

func (p *DevCheckpointProvider) GetCheckpointByID(ctx context.Context, repoID, cpID string) (*models.Checkpoint, error) {
	cps, err := p.GetCheckpoints(ctx, repoID)
	if err != nil {
		return nil, err
	}
	for _, cp := range cps {
		if cp.CheckpointID == cpID {
			return &cp, nil
		}
	}
	return &cps[0], nil
}
