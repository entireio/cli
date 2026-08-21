//go:build integration

package integration

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
)

// TestEnable_GitRefsPrimaryDoesNotPullCheckpointBranch is the end-to-end
// acceptance test for the shape the CLI repo itself ships: committed settings
// that pair the git-refs primary with a dedicated checkpoint_remote whose v1
// branch carries the repo's legacy checkpoint history.
//
// Before the fix, `entire enable` on a fresh clone of such a repo eagerly cloned
// that branch — every blob of every transcript ever committed — under a 30s
// deadline. On a real checkpoint remote the transfer cannot finish in time, and
// because the git-refs path deliberately creates no orphan when the fetch fails,
// the next run found identical state and started over. It never converged, and
// the failure was logged at Debug so the command just looked slow.
//
// The two halves are a pair, and testing either alone would miss the point:
// enable must not pull the branch, AND a checkpoint that exists only on that
// branch must still be readable. The branch is not vestigial under git-refs —
// legacy hex-ID checkpoints live there and reads route to it by ID format — so
// "don't fetch it" is only correct because reads recover it on demand.
func TestEnable_GitRefsPrimaryDoesNotPullCheckpointBranch(t *testing.T) {
	t.Parallel()

	// Producer: a git-branch repo that condenses a checkpoint onto v1 and pushes
	// it to a bare repo, which then plays the consumer's checkpoint_remote. This
	// models history written before the repo moved to the git-refs backend.
	producer := NewFeatureBranchEnv(t)
	bareDir := producer.SetupBareRemote()
	const prompt = "Add rate limiting to the API gateway"
	checkpointID := createAndPushCheckpoint(t, producer, "limiter.go", prompt)

	// Consumer: a fresh-clone-shaped repo carrying the committed settings.
	consumerDir := setupGitRefsCheckpointRemoteRepo(t, bareDir)
	requireNoMetadataBranch(t, consumerDir, "precondition: the fresh repo has no v1 branch")

	// --- enable must not pull the branch ---
	runEntireInDir(t, consumerDir, "enable", "--agent", agentClaudeCode, "--telemetry=false")

	requireNoMetadataBranch(t, consumerDir,
		"enable must not fetch v1 under the git-refs primary: the branch is never written or pushed by that backend, so there is no divergence to prevent and the fetch only costs the full transcript history")

	// The settings that drive this must have survived enable, or the assertion
	// above would pass for the wrong reason (a repo that silently fell back to
	// the git-branch primary has no v1 branch yet either).
	settingsBytes, err := os.ReadFile(filepath.Join(consumerDir, ".entire", paths.SettingsFileName))
	require.NoError(t, err)
	settings := string(settingsBytes)
	assert.Contains(t, settings, `"git-refs"`, "enable must preserve the committed git-refs primary")
	assert.Contains(t, settings, checkpointRemoteRepoSlug, "enable must preserve the committed checkpoint_remote")

	// --- and a checkpoint that exists only on that branch must still read ---
	output := runExplainInDir(t, consumerDir, checkpointID)
	require.Contains(t, output, prompt,
		"explain must recover the v1 branch from the checkpoint remote on demand; without that tier a legacy hex-ID checkpoint is unreadable, since the branch is absent locally and origin never carried it")

	// Prove the read did the recovery rather than finding data that was already
	// there: the branch is local only now, after explain.
	require.True(t, testutil.BranchExists(t, consumerDir, paths.MetadataBranchName),
		"explain should have fetched v1 from the checkpoint remote")
}

// checkpointRemoteRepoSlug is the owner/repo the consumer's checkpoint_remote
// names. The derived URL is redirected at the local bare via insteadOf, so
// nothing in this test dials the network.
const checkpointRemoteRepoSlug = "org/checkpoints"

// setupGitRefsCheckpointRemoteRepo creates a repo shaped like a fresh clone of a
// project whose committed settings select the git-refs primary and point
// checkpoints at a dedicated checkpoint_remote — the CLI repo's own shape.
//
// origin is a separate empty bare holding no checkpoint data, which is the point:
// the v1 branch exists only on the checkpoint remote, so origin's remote-tracking
// ref can never satisfy a read. checkpoint_remote must be a real provider/repo
// pair (settings.GetCheckpointRemote rejects anything else, and a rejected config
// reads as "not configured"), so the derived URL is redirected at the bare with
// git's insteadOf rather than by naming a path directly.
func setupGitRefsCheckpointRemoteRepo(t *testing.T, checkpointBareDir string) string {
	t.Helper()
	dir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	testutil.InitRepo(t, dir)

	require.NoError(t, os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(".entire/\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("# consumer\n"), 0o644))

	entireDir := filepath.Join(dir, ".entire")
	require.NoError(t, os.MkdirAll(entireDir, 0o755))
	settings := map[string]any{
		"enabled":   true,
		"local_dev": true,
		"strategy":  "manual-commit",
		"strategy_options": map[string]any{
			"filtered_fetches": true,
			"checkpoint_remote": map[string]any{
				"provider": "github",
				"repo":     checkpointRemoteRepoSlug,
			},
		},
		"checkpoints": map[string]any{
			"primary": map[string]any{"type": "git-refs"},
		},
	}
	data, err := jsonutil.MarshalIndentWithNewline(settings, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(entireDir, paths.SettingsFileName), data, 0o644))

	gitOutput(t, dir, "add", ".gitignore", "README.md")
	gitOutput(t, dir, "commit", "-m", "initial commit")

	// origin: a real but empty bare, so URL derivation has a remote to read and
	// nothing ever dials the network. It must be a file:// URL, not a bare path —
	// remote.FetchURL parses origin to derive the checkpoint URL, and a path with
	// no protocol makes it give up and fall back to origin itself.
	originBare := t.TempDir()
	gitOutput(t, originBare, "init", "--bare")
	gitOutput(t, dir, "remote", "add", "origin", "file://"+originBare)

	// Redirect both URL shapes the checkpoint_remote can derive to (HTTPS from
	// the provider's canonical host, or SSH) at the local bare.
	fileURL := "file://" + checkpointBareDir
	for _, derived := range []string{
		"https://github.com/" + checkpointRemoteRepoSlug + ".git",
		"git@github.com:" + checkpointRemoteRepoSlug + ".git",
	} {
		gitOutput(t, dir, "config", "--add", "url."+fileURL+".insteadOf", derived)
	}

	return dir
}

// requireNoMetadataBranch asserts the v1 branch is absent both locally and as an
// origin remote-tracking ref.
func requireNoMetadataBranch(t *testing.T, dir, msg string) {
	t.Helper()
	require.False(t, testutil.BranchExists(t, dir, paths.MetadataBranchName), msg)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)
	defer repo.Close()
	_, err = repo.Storer.Reference(plumbing.NewRemoteReferenceName("origin", paths.MetadataBranchName))
	require.ErrorIs(t, err, plumbing.ErrReferenceNotFound, msg)
}

// runEntireInDir runs the entire binary in dir under git-isolated env and
// returns its combined output, failing the test if the command errors. Thin
// wrapper over runEntire, which owns the execx.NonInteractive spawn (project
// rule: the child gets no controlling terminal, so it never blocks on a prompt).
func runEntireInDir(t *testing.T, dir string, args ...string) string {
	t.Helper()
	stdout, stderr, err := runEntire(t, testutil.GitIsolatedEnv(), dir, args...)
	if err != nil {
		t.Fatalf("entire %v failed: %v\n%s%s", args, err, stdout, stderr)
	}
	return stdout + stderr
}
