package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/binding"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordBindingForTest writes one machine-level session record and ages it, so
// the sweep has something to prune. It writes through the real record API and
// then rewrites the timestamp on disk, because UpdatedAt is what retention
// reads and the writer always stamps now.
func recordBindingForTest(ctx context.Context, t *testing.T, configDir, sessionID string, updatedAt time.Time) string {
	t.Helper()
	require.NoError(t, binding.RecordBinding(ctx, sessionID, binding.SessionMeta{
		AgentType:      "claude-code",
		TranscriptPath: filepath.Join(configDir, "transcript.jsonl"),
	}, binding.Evidence{
		Repo:    binding.RepoIdentity{WorktreeRoot: "/repos/x", CommonDir: "/repos/x/.git"},
		Enabled: true,
	}))

	path := filepath.Join(configDir, "sessions", sessionID+".json")
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var rec map[string]any
	require.NoError(t, json.Unmarshal(data, &rec))
	rec["updated_at"] = updatedAt.Format(time.RFC3339Nano)
	data, err = json.Marshal(rec)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))
	return path
}

// TestRunSessionSweep_PrunesStaleSessionRecords pins that the sweep is where
// binding's record retention actually happens. The records live under the user
// config directory, so no repo-level clean reaches them, and the sweep is the
// only detached pass that runs without a user waiting on it.
func TestRunSessionSweep_PrunesStaleSessionRecords(t *testing.T) {
	// Cannot use t.Parallel(): t.Chdir and t.Setenv are process-global.
	dir := setupGitRepoForPhaseTest(t)
	t.Chdir(dir)
	configDir := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", configDir)
	ctx := context.Background()

	stale := recordBindingForTest(ctx, t, configDir, "stale-session",
		time.Now().Add(-binding.RecordRetention-time.Hour))
	fresh := recordBindingForTest(ctx, t, configDir, "fresh-session", time.Now())

	require.NoError(t, runSessionSweep(ctx))

	_, err := os.Stat(stale)
	assert.True(t, os.IsNotExist(err), "the sweep must prune a record past the retention window (stat err = %v)", err)
	_, err = os.Stat(fresh)
	assert.NoError(t, err, "the sweep must leave a record inside the window alone")
}

// TestMaybeSpawnSessionSweep_RetentionNominatesWithoutZombies is the other half
// of the wiring. The sweep is spawned only when something nominates it, and the
// machines whose record store grows are exactly the ones with no zombie
// sessions to nominate one: a session that never touches a repo writes a record
// and no session state at all.
func TestMaybeSpawnSessionSweep_RetentionNominatesWithoutZombies(t *testing.T) {
	// Cannot use t.Parallel(): t.Chdir, t.Setenv and the seam swap are global.
	dir := setupGitRepoForPhaseTest(t)
	t.Chdir(dir)
	configDir := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", configDir)
	ctx := context.Background()

	var spawns atomic.Int32
	prevSpawn := sweepSpawn
	sweepSpawn = func(string) { spawns.Add(1) }
	t.Cleanup(func() { sweepSpawn = prevSpawn })

	// No zombies and no record store: nothing nominates a sweep.
	maybeSpawnSessionSweep(ctx)
	require.Equal(t, int32(0), spawns.Load(),
		"an empty machine must not fork a child to discover it has nothing to do")

	recordBindingForTest(ctx, t, configDir, "s1", time.Now())
	maybeSpawnSessionSweep(ctx)
	assert.Equal(t, int32(1), spawns.Load(),
		"an unpruned record store must nominate a sweep even with no zombie sessions")
}
