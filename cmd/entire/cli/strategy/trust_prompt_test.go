package strategy

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// newGloballyEnrolledPromptRepo creates a repo with NO repo-level setup,
// chdirs into it, and enrolls the user-global tier with the given settings
// JSON. Returns the config dir so tests can inspect what the prompt wrote.
// Not parallel-safe: t.Chdir + t.Setenv.
func newGloballyEnrolledPromptRepo(t *testing.T, userSettingsJSON string) (dir, cfgDir string) {
	t.Helper()
	testutil.IsolateGitConfigEnv(t)
	dir = t.TempDir()
	testutil.InitRepo(t, dir)
	t.Chdir(dir)
	cfgDir = enrollRepoGlobally(t, userSettingsJSON)
	return dir, cfgDir
}

// A non-interactive pre-push must hold WITHOUT opening a prompt: pre-push
// stdin carries git ref lines, and agent/CI pushes have no user to answer.
// A prompt firing here would hang or eat the ref list.
func TestResolveTrustDecision_NonInteractiveHoldsWithoutPrompting(t *testing.T) {
	t.Parallel()
	ask := func() (trustChoice, error) {
		t.Fatal("prompt must not open in a non-interactive pre-push")
		return trustChoiceNotNow, nil
	}
	d, err := resolveTrustDecision(context.Background(), false, ask, io.Discard)
	require.NoError(t, err)
	require.Equal(t, TrustHeld, d)
}

// "Yes" must persist trust through settings.TrustCurrentRepo — the same
// writer bare `entire trust` uses — so the gate re-evaluates open on this
// very push. A prompt that only returns Granted without writing would sync
// once and re-ask forever.
func TestResolveTrustDecision_YesPersistsRepoTrust(t *testing.T) {
	dir, cfgDir := newGloballyEnrolledPromptRepo(t, `{"global":{"enabled":true}}`)
	testutil.AddRemote(t, dir, "origin", "https://github.com/acme/widgets.git")
	ctx := context.Background()
	require.False(t, settings.CheckpointEgressAllowed(ctx), "fixture must start held")

	var warnings bytes.Buffer
	d, err := resolveTrustDecision(ctx, true,
		func() (trustChoice, error) { return trustChoiceYes, nil }, &warnings)
	require.NoError(t, err)
	require.Equal(t, TrustGranted, d)
	require.Empty(t, warnings.String())

	require.True(t, settings.CheckpointEgressAllowed(ctx), "granted trust must open the gate")
	raw, err := os.ReadFile(filepath.Join(cfgDir, "settings.json"))
	require.NoError(t, err)
	require.Contains(t, string(raw), "github.com/acme/widgets",
		"trust must be written under the repo's origin key")
}

// "Always" must set trust_all machine-wide, not just this repo's key.
func TestResolveTrustDecision_AlwaysSetsTrustAll(t *testing.T) {
	newGloballyEnrolledPromptRepo(t, `{"global":{"enabled":true}}`)
	ctx := context.Background()

	d, err := resolveTrustDecision(ctx, true,
		func() (trustChoice, error) { return trustChoiceAlways, nil }, io.Discard)
	require.NoError(t, err)
	require.Equal(t, TrustGranted, d)
	require.Equal(t, settings.TrustSourceAll, settings.CurrentTrustSource(ctx),
		"Always must grant via trust_all, not a per-repo key")
}

// "Not now" holds without writing anything — the question is re-asked on the
// next push, so a stray write here would silently skip the re-ask.
func TestResolveTrustDecision_NotNowHoldsAndWritesNothing(t *testing.T) {
	newGloballyEnrolledPromptRepo(t, `{"global":{"enabled":true}}`)
	ctx := context.Background()

	d, err := resolveTrustDecision(ctx, true,
		func() (trustChoice, error) { return trustChoiceNotNow, nil }, io.Discard)
	require.NoError(t, err)
	require.Equal(t, TrustHeld, d)
	require.False(t, settings.CheckpointEgressAllowed(ctx), "declining must leave the gate held")
}

// A trust-write failure (here: the global tier is unconfigured, the same shape
// as a newer-CLI strict-decode abort) must warn and hold — NEVER return an
// error, which prePush could turn into a failed user push.
func TestResolveTrustDecision_TrustWriteFailureWarnsAndHolds(t *testing.T) {
	newGloballyEnrolledPromptRepo(t, `{}`)
	ctx := context.Background()

	var warnings bytes.Buffer
	d, err := resolveTrustDecision(ctx, true,
		func() (trustChoice, error) { return trustChoiceYes, nil }, &warnings)
	require.NoError(t, err, "a failed trust write must never surface as an error")
	require.Equal(t, TrustHeld, d)
	require.Contains(t, warnings.String(), "Warning", "the swallowed write failure must be visible on stderr")
}
