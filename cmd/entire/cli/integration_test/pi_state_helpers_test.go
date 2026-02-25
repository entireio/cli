//go:build integration

package integration

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

func sessionStateStoreForRepo(repoDir string) *session.StateStore {
	stateDir := filepath.Join(repoDir, ".git", session.SessionStateDirName)
	return session.NewStateStoreWithDir(stateDir)
}

func saveSessionStateToRepo(t *testing.T, repoDir string, state *strategy.SessionState) {
	t.Helper()

	store := sessionStateStoreForRepo(repoDir)
	if err := store.Save(context.Background(), state); err != nil {
		t.Fatalf("failed to save session state: %v", err)
	}
}

func loadSessionStateFromRepo(t *testing.T, repoDir, sessionID string) *strategy.SessionState {
	t.Helper()

	store := sessionStateStoreForRepo(repoDir)
	state, err := store.Load(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("failed to load session state: %v", err)
	}
	return state
}
