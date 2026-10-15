package brainnotify

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
)

func captureSpawns(t *testing.T) (*string, *[]string, *int) {
	t.Helper()
	original := spawnDetached
	t.Cleanup(func() { spawnDetached = original })
	var dir string
	var args []string
	spawned := 0
	spawnDetached = func(d string, a ...string) {
		dir = d
		args = slices.Clone(a)
		spawned++
	}
	return &dir, &args, &spawned
}

func TestNotifyCheckpointSpawnsContentFreeBrainHint(t *testing.T) {
	t.Setenv(memoryWorkerOriginEnv, "")
	gotDir, gotArgs, _ := captureSpawns(t)

	NotifyCheckpoint("/repo", "session-123", "feature/memory")

	require.Equal(t, "/repo", *gotDir)
	require.Equal(t, []string{
		"brain", "memory", "notify",
		"--event", "checkpoint",
		"--session", "session-123",
		"--branch", "feature/memory",
	}, *gotArgs)
	require.NotContains(t, *gotArgs, "--repo-key", "Brain remains the repository identity authority")
}

func TestNotifyCheckpointOmitsUnknownBranch(t *testing.T) {
	t.Setenv(memoryWorkerOriginEnv, "")
	_, gotArgs, _ := captureSpawns(t)

	NotifyCheckpoint("/repo", "session-123", "")

	require.Equal(t, []string{
		"brain", "memory", "notify",
		"--event", "checkpoint",
		"--session", "session-123",
	}, *gotArgs)
}

func TestNotifyCheckpointSkipsInvalidOrRecursiveHints(t *testing.T) {
	_, _, spawned := captureSpawns(t)

	t.Run("empty repository root", func(t *testing.T) {
		t.Setenv(memoryWorkerOriginEnv, "")
		NotifyCheckpoint("", "session-123", "main")
	})
	t.Run("empty session", func(t *testing.T) {
		t.Setenv(memoryWorkerOriginEnv, "")
		NotifyCheckpoint("/repo", "", "main")
	})
	t.Run("memory worker origin", func(t *testing.T) {
		t.Setenv(memoryWorkerOriginEnv, "1")
		NotifyCheckpoint("/repo", "session-123", "main")
	})

	require.Zero(t, *spawned)
}

func TestNotifyCheckpointTrimsRoutingValues(t *testing.T) {
	t.Setenv(memoryWorkerOriginEnv, "")
	gotDir, gotArgs, _ := captureSpawns(t)

	NotifyCheckpoint(" /repo ", " session-123 ", " main ")

	require.Equal(t, "/repo", *gotDir)
	require.Equal(t, []string{
		"brain", "memory", "notify",
		"--event", "checkpoint",
		"--session", "session-123",
		"--branch", "main",
	}, *gotArgs)
}
