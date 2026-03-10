package strategy

import (
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/config"
)

func TestRemoteExists_True(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)

	repo, err := git.PlainOpen(tmpDir)
	if err != nil {
		t.Fatalf("failed to open repo: %v", err)
	}

	// Add a remote
	_, err = repo.CreateRemote(&config.RemoteConfig{
		Name: "private",
		URLs: []string{"https://example.com/repo.git"},
	})
	if err != nil {
		t.Fatalf("failed to create remote: %v", err)
	}

	if !remoteExists(repo, "private") {
		t.Error("remoteExists() = false, want true for configured remote")
	}
}

func TestRemoteExists_False(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)

	repo, err := git.PlainOpen(tmpDir)
	if err != nil {
		t.Fatalf("failed to open repo: %v", err)
	}

	if remoteExists(repo, "nonexistent") {
		t.Error("remoteExists() = true, want false for unconfigured remote")
	}
}
