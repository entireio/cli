package brainnotify

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNotifySpawnsContentFreeBrainHint(t *testing.T) {
	t.Setenv(memoryWorkerOriginEnv, "")

	originalSpawn := spawnDetached
	t.Cleanup(func() { spawnDetached = originalSpawn })

	var gotDir string
	var gotArgs []string
	spawnDetached = func(dir string, args ...string) {
		gotDir = dir
		gotArgs = slices.Clone(args)
	}

	Notify("/repo", EventCheckpoint, "session-123", "feature/memory")

	require.Equal(t, "/repo", gotDir)
	require.Equal(t, []string{
		"brain", "memory", "notify",
		"--event", "checkpoint",
		"--session", "session-123",
		"--branch", "feature/memory",
	}, gotArgs)
	require.NotContains(t, gotArgs, "--repo-key", "Brain remains the repository identity authority")
}

func TestNotifyOmitsUnknownBranch(t *testing.T) {
	t.Setenv(memoryWorkerOriginEnv, "")

	originalSpawn := spawnDetached
	t.Cleanup(func() { spawnDetached = originalSpawn })

	var gotArgs []string
	spawnDetached = func(_ string, args ...string) {
		gotArgs = slices.Clone(args)
	}

	Notify("/repo", EventSessionStart, "session-123", "")

	require.Equal(t, []string{
		"brain", "memory", "notify",
		"--event", "session_start",
		"--session", "session-123",
	}, gotArgs)
}

func TestNotifySkipsInvalidOrRecursiveHints(t *testing.T) {
	originalSpawn := spawnDetached
	t.Cleanup(func() { spawnDetached = originalSpawn })

	spawned := 0
	spawnDetached = func(_ string, _ ...string) { spawned++ }

	t.Run("empty repository root", func(t *testing.T) {
		t.Setenv(memoryWorkerOriginEnv, "")
		Notify("", EventSessionStart, "session-123", "main")
	})
	t.Run("empty session", func(t *testing.T) {
		t.Setenv(memoryWorkerOriginEnv, "")
		Notify("/repo", EventSessionStart, "", "main")
	})
	t.Run("unknown event", func(t *testing.T) {
		t.Setenv(memoryWorkerOriginEnv, "")
		Notify("/repo", Event("turn_end"), "session-123", "main")
	})

	require.Zero(t, spawned)
}

func TestNotifySkipsMemoryWorkerOrigin(t *testing.T) {
	t.Setenv(memoryWorkerOriginEnv, "1")

	originalSpawn := spawnDetached
	t.Cleanup(func() { spawnDetached = originalSpawn })

	spawned := 0
	spawnDetached = func(_ string, _ ...string) { spawned++ }

	Notify("/repo", EventSessionEnd, "session-123", "main")

	require.Zero(t, spawned, "a Brain worker must not recursively launch another memory worker")
}
