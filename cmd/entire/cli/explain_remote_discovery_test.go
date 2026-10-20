package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
	"github.com/entireio/cli/redact"
	"github.com/go-git/go-git/v6"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGetBranchCheckpoints_HydratesRemoteDiscoveredStub is the trail-871 /
// PR-1771 headline integration: device B has the trailer commit (pulled) but no
// local checkpoint ref; with checkpoint_remote configured against a local bare
// remote that advertises the ref, getBranchCheckpoints must return a PendingCheckpoint
// with the real SessionID (not an empty stub that --session would drop).
//
// Offline: provider "local" is unknown to providerHost, so FetchURL falls back
// to origin after file:// derivation fails — origin is the bare file:// URL.
// Not parallel: uses t.Chdir.
func TestGetBranchCheckpoints_HydratesRemoteDiscoveredStub(t *testing.T) {
	settingsBody := `{"enabled":true,"checkpoints":{"primary":{"type":"git-refs"}},"strategy_options":{"checkpoint_remote":{"provider":"local","repo":"org/checkpoints"}}}`
	deviceA, bareURL, branch, stores := setupRemoteDiscoveryDeviceA(t, settingsBody)

	cid := id.CheckpointID("01KVBJCWYA4YW6J5M9GP655HZN")
	const sessionID = "session-from-device-a"
	require.NoError(t, stores.Persistent.Write(context.Background(), checkpoint.Session{
		CheckpointID: cid,
		SessionID:    sessionID,
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte("transcript from A")),
		Prompts:      []string{"do the thing on device A"},
		FilesTouched: []string{"a.go"},
		AuthorName:   "Test",
		AuthorEmail:  "test@example.com",
	}))

	refName, err := checkpoint.RefName(cid)
	require.NoError(t, err)
	gitRun(t, deviceA, "push", "-q", "origin", refName.String()+":"+refName.String())

	testutil.WriteFile(t, deviceA, "feature.txt", "from A")
	testutil.GitAdd(t, deviceA, "feature.txt")
	msgPath := filepath.Join(deviceA, ".git", "COMMIT_EDITMSG_TEST")
	require.NoError(t, os.WriteFile(msgPath, []byte(trailers.FormatCheckpoint("feat from device A", cid)), 0o644))
	gitRun(t, deviceA, "commit", "-F", msgPath)
	gitRun(t, deviceA, "push", "-q", "origin", "HEAD:"+branch)

	deviceB := cloneRemoteDiscoveryDeviceB(t, bareURL, branch, settingsBody)

	// Clone must not have brought the checkpoint ref — that is the second-device gap.
	verify := exec.CommandContext(context.Background(), "git", "show-ref", "--verify", "--quiet", refName.String())
	verify.Dir = deviceB
	verify.Env = testutil.GitIsolatedEnv()
	require.Error(t, verify.Run(), "device B must lack the checkpoint ref locally before discovery")

	logMsg := gitOutput(t, deviceB, "log", "-1", "--format=%B")
	require.Contains(t, logMsg, cid.String(), "device B HEAD must carry the Entire-Checkpoint trailer")

	t.Chdir(deviceB)
	repoB, err := git.PlainOpen(deviceB)
	require.NoError(t, err)

	points, _, err := getBranchCheckpoints(context.Background(), repoB, 10)
	require.NoError(t, err)
	require.NotEmpty(t, points, "discovered+hydrated checkpoint must appear on device B")

	var found bool
	for _, p := range points {
		if p.CheckpointID == cid {
			found = true
			assert.Equal(t, sessionID, p.SessionID, "hydration must fill SessionID for --session filters")
			assert.Equal(t, 1, p.SessionCount)
			assert.Contains(t, p.SessionIDs, sessionID)
			assert.False(t, p.Date.IsZero())
			break
		}
	}
	require.True(t, found, "PendingCheckpoint for remote-discovered checkpoint %s missing; got %+v", cid, points)
}

// TestGetBranchCheckpoints_ListsRemoteOnlyImportedCheckpoints covers a fresh
// clone of a git-refs repo produced by `entire repo migrate`: no
// checkpoint_remote (checkpoints live on origin), imported checkpoints with
// legacy hex IDs, and no commit trailers. The imported pass must hydrate the
// remote-discovered stubs to learn they are imported, and order them by their
// stored CreatedAt, since legacy IDs carry no timestamp. An unlinked native
// ULID checkpoint on origin is never imported and must not be fetched.
// Not parallel: uses t.Chdir.
func TestGetBranchCheckpoints_ListsRemoteOnlyImportedCheckpoints(t *testing.T) {
	settingsBody := `{"enabled":true,"checkpoints":{"primary":{"type":"git-refs"}}}`
	deviceA, bareURL, branch, stores := setupRemoteDiscoveryDeviceA(t, settingsBody)

	// The newer checkpoint has the ID that sorts last by ref name, so the
	// expected order only holds if it comes from CreatedAt.
	older := id.MustCheckpointID("aaaaaaaaaaaa")
	newer := id.MustCheckpointID("ffffffffffff")
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	for i, cid := range []id.CheckpointID{older, newer} {
		require.NoError(t, stores.Persistent.Write(context.Background(), checkpoint.Session{
			CheckpointID: cid,
			SessionID:    "imported-" + cid.String(),
			CreatedAt:    base.Add(time.Duration(i) * time.Hour),
			Strategy:     "import",
			Kind:         "imported",
			Transcript:   redact.AlreadyRedacted([]byte("imported transcript")),
			Prompts:      []string{"prompt " + cid.String()},
			AuthorName:   "Test",
			AuthorEmail:  "test@example.com",
		}))
		refName, refErr := checkpoint.RefName(cid)
		require.NoError(t, refErr)
		gitRun(t, deviceA, "push", "-q", "origin", refName.String()+":"+refName.String())
	}
	native := id.CheckpointID("01KVBJCWYA4YW6J5M9GP655HZN")
	require.NoError(t, stores.Persistent.Write(context.Background(), checkpoint.Session{
		CheckpointID: native,
		SessionID:    "native-session",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte("native transcript")),
		Prompts:      []string{"native work on another branch"},
		AuthorName:   "Test",
		AuthorEmail:  "test@example.com",
	}))
	nativeRef, err := checkpoint.RefName(native)
	require.NoError(t, err)
	gitRun(t, deviceA, "push", "-q", "origin", nativeRef.String()+":"+nativeRef.String())

	deviceB := cloneRemoteDiscoveryDeviceB(t, bareURL, branch, settingsBody)
	require.Empty(t, gitOutput(t, deviceB, "for-each-ref", checkpoint.CheckpointRefPrefix),
		"clone must not bring checkpoint refs; discovery has to find them on origin")

	t.Chdir(deviceB)
	repoB, err := git.PlainOpen(deviceB)
	require.NoError(t, err)

	points, _, err := getBranchCheckpoints(context.Background(), repoB, 10)
	require.NoError(t, err)
	require.Len(t, points, 2, "both remote-only imported checkpoints must be listed; got %+v", points)

	assert.Equal(t, newer, points[0].CheckpointID, "newest imported checkpoint first")
	assert.Equal(t, older, points[1].CheckpointID)
	for i, p := range points {
		assert.True(t, p.Imported)
		assert.Equal(t, "imported-"+p.CheckpointID.String(), p.SessionID)
		assert.Equal(t, "prompt "+p.CheckpointID.String(), p.SessionPrompt)
		assert.Equal(t, base.Add(time.Duration(1-i)*time.Hour), p.Date.UTC())
	}
	assert.NotContains(t, gitOutput(t, deviceB, "for-each-ref", checkpoint.CheckpointRefPrefix), nativeRef.String(),
		"a ULID stub cannot be imported, so the imported pass must not fetch it")
}

// TestGetBranchCheckpoints_FeatureBranchSkipsMainLinkedStubs: on a feature
// branch the commit walk stops at main, so main's checkpoints are not collected.
// Their trailers still mark them linked, including trailers on a side branch
// merged into main (second parent), so the imported pass must not fetch each of
// them just to learn they are not imported. Uses a legacy hex ID, since only
// those are imported-pass candidates.
// Not parallel: uses t.Chdir.
func TestGetBranchCheckpoints_FeatureBranchSkipsMainLinkedStubs(t *testing.T) {
	settingsBody := `{"enabled":true,"checkpoints":{"primary":{"type":"git-refs"}}}`
	deviceA, bareURL, branch, stores := setupRemoteDiscoveryDeviceA(t, settingsBody)

	cid := id.MustCheckpointID("bbbbbbbbbbbb")
	require.NoError(t, stores.Persistent.Write(context.Background(), checkpoint.Session{
		CheckpointID: cid,
		SessionID:    "main-session",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte("transcript")),
		Prompts:      []string{"main work"},
		AuthorName:   "Test",
		AuthorEmail:  "test@example.com",
	}))
	refName, err := checkpoint.RefName(cid)
	require.NoError(t, err)
	gitRun(t, deviceA, "push", "-q", "origin", refName.String()+":"+refName.String())

	gitRun(t, deviceA, "switch", "-q", "-c", "side")
	testutil.WriteFile(t, deviceA, "side.txt", "on side")
	testutil.GitAdd(t, deviceA, "side.txt")
	msgPath := filepath.Join(deviceA, ".git", "COMMIT_EDITMSG_TEST")
	require.NoError(t, os.WriteFile(msgPath, []byte(trailers.FormatCheckpoint("side work", cid)), 0o644))
	gitRun(t, deviceA, "commit", "-F", msgPath)
	gitRun(t, deviceA, "switch", "-q", branch)
	gitRun(t, deviceA, "merge", "-q", "--no-ff", "-m", "merge side", "side")
	gitRun(t, deviceA, "push", "-q", "origin", "HEAD:"+branch)

	deviceB := cloneRemoteDiscoveryDeviceB(t, bareURL, branch, settingsBody)
	gitRun(t, deviceB, "switch", "-q", "-c", "feature")

	t.Chdir(deviceB)
	repoB, err := git.PlainOpen(deviceB)
	require.NoError(t, err)

	points, _, err := getBranchCheckpoints(context.Background(), repoB, 10)
	require.NoError(t, err)
	assert.Empty(t, points, "main's checkpoint is not unique to the feature branch")
	assert.Empty(t, gitOutput(t, deviceB, "for-each-ref", checkpoint.CheckpointRefPrefix),
		"main-linked stub must not be fetched by the imported pass")
}

// setupRemoteDiscoveryDeviceA creates a bare file:// origin and a device A repo
// that pushed one commit to it, writes settingsBody, chdirs into device A, and
// opens its checkpoint stores.
func setupRemoteDiscoveryDeviceA(t *testing.T, settingsBody string) (deviceA, bareURL, branch string, stores *checkpoint.Stores) {
	t.Helper()
	bareDir := t.TempDir()
	gitRun(t, bareDir, "init", "--bare", "-q", bareDir)
	bareURL = "file://" + filepath.ToSlash(bareDir)

	deviceA = t.TempDir()
	testutil.InitRepo(t, deviceA)
	testutil.WriteFile(t, deviceA, "f.txt", "init")
	testutil.GitAdd(t, deviceA, "f.txt")
	testutil.GitCommit(t, deviceA, "init")
	branch = gitDefaultBranch(t, deviceA)
	gitRun(t, deviceA, "remote", "add", "origin", bareURL)
	gitRun(t, deviceA, "push", "-q", "-u", "origin", "HEAD:"+branch)
	gitRun(t, bareDir, "symbolic-ref", "HEAD", "refs/heads/"+branch)

	testutil.WriteFile(t, deviceA, ".entire/settings.json", settingsBody)
	t.Chdir(deviceA)

	repoA, err := git.PlainOpen(deviceA)
	require.NoError(t, err)
	stores, err = checkpoint.Open(context.Background(), repoA, checkpoint.OpenOptions{})
	require.NoError(t, err)
	return deviceA, bareURL, branch, stores
}

// cloneRemoteDiscoveryDeviceB clones bareURL into a fresh device B repo (heads
// only, as a normal clone) and writes settingsBody.
func cloneRemoteDiscoveryDeviceB(t *testing.T, bareURL, branch, settingsBody string) string {
	t.Helper()
	deviceB := filepath.Join(t.TempDir(), "device-b")
	gitRun(t, t.TempDir(), "clone", "-q", "--branch", branch, bareURL, deviceB)
	testutil.WriteFile(t, deviceB, ".entire/settings.json", settingsBody)
	return deviceB
}
