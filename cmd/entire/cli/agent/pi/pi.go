package pi

import (
	"context"
	"path/filepath"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/session"
)

type PiAgent struct{}

func NewPiAgent() *PiAgent {
	return &PiAgent{}
}

func (a *PiAgent) Name() string {
	return "Pi"
}

func (a *PiAgent) Detect(ctx context.Context, repoRoot string) (bool, error) {
	// Pi usually has a ~/.pi directory or project local .pi
	// Simplified detection for skeleton
	return true, nil
}

func (a *PiAgent) InstallHooks(ctx context.Context, repoRoot string) error {
	return nil
}

func (a *PiAgent) UninstallHooks(ctx context.Context, repoRoot string) error {
	return nil
}

func (a *PiAgent) ReadSession(ctx context.Context, sessionID string) (*agent.SessionMetadata, error) {
	return &agent.SessionMetadata{
		SessionID: sessionID,
	}, nil
}
