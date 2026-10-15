package brainnotify

import (
	"errors"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
)

func stubPluginOnPath(t *testing.T) {
	t.Helper()
	original := lookPath
	t.Cleanup(func() { lookPath = original })
	lookPath = func(string) (string, error) { return "/managed/entire-brain", nil }
}

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
	stubPluginOnPath(t)
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
	stubPluginOnPath(t)
	_, gotArgs, _ := captureSpawns(t)

	NotifyCheckpoint("/repo", "session-123", "")

	require.Equal(t, []string{
		"brain", "memory", "notify",
		"--event", "checkpoint",
		"--session", "session-123",
	}, *gotArgs)
}

func TestNotifyCheckpointSkipsInvalidOrRecursiveHints(t *testing.T) {
	stubPluginOnPath(t)
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

func TestNotifyCheckpointSkipsWhenPluginMissing(t *testing.T) {
	t.Setenv(memoryWorkerOriginEnv, "")
	originalLook := lookPath
	t.Cleanup(func() { lookPath = originalLook })
	lookPath = func(string) (string, error) { return "", errors.New("not installed") }
	_, _, spawned := captureSpawns(t)

	NotifyCheckpoint("/repo", "session-123", "main")

	require.Zero(t, *spawned, "no plugin on PATH must cost a LookPath, not a fork")
}
