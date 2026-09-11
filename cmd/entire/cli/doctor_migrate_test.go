package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	git "github.com/go-git/go-git/v6"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/redact"
)

// Not parallel: uses t.Chdir()
func TestResolveMigratePushRemote_ExplicitValueReturnedVerbatim(t *testing.T) {
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "f.txt", "init")
	testutil.GitAdd(t, tmpDir, "f.txt")
	testutil.GitCommit(t, tmpDir, "init")
	t.Chdir(tmpDir)

	got, err := resolveMigratePushRemote(context.Background(), "explicit-remote")
	require.NoError(t, err)
	assert.Equal(t, "explicit-remote", got)
}

// Not parallel: uses t.Chdir()
func TestResolveMigratePushRemote_EmptyUsesConfiguredSetting(t *testing.T) {
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "f.txt", "init")
	testutil.GitAdd(t, tmpDir, "f.txt")
	testutil.GitCommit(t, tmpDir, "init")
	testutil.AddRemote(t, tmpDir, "origin", "https://example.com/origin.git")
	testutil.AddRemote(t, tmpDir, "private", "https://example.com/private.git")
	testutil.WriteCheckpointPushRemoteSetting(t, tmpDir, "private")
	t.Chdir(tmpDir)

	got, err := resolveMigratePushRemote(context.Background(), "")
	require.NoError(t, err)
	assert.Equal(t, "private", got)
}

// Not parallel: uses t.Chdir()
func TestResolveMigratePushRemote_EmptyDefaultsToOrigin(t *testing.T) {
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "f.txt", "init")
	testutil.GitAdd(t, tmpDir, "f.txt")
	testutil.GitCommit(t, tmpDir, "init")
	testutil.AddRemote(t, tmpDir, "origin", "https://example.com/origin.git")
	testutil.AddRemote(t, tmpDir, "publish", "https://example.com/publish.git")
	t.Chdir(tmpDir)

	got, err := resolveMigratePushRemote(context.Background(), "")
	require.NoError(t, err)
	assert.Equal(t, "origin", got)
}

// Not parallel: uses t.Chdir()
func TestResolveMigratePushRemote_MisconfiguredSettingFailsClosed(t *testing.T) {
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "f.txt", "init")
	testutil.GitAdd(t, tmpDir, "f.txt")
	testutil.GitCommit(t, tmpDir, "init")
	testutil.AddRemote(t, tmpDir, "origin", "https://example.com/origin.git")
	testutil.WriteCheckpointPushRemoteSetting(t, tmpDir, "gone")
	t.Chdir(tmpDir)

	got, err := resolveMigratePushRemote(context.Background(), "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "checkpoint_push_remote")
	assert.Empty(t, got)
}

// Not parallel: uses t.Chdir()
func TestResolveMigratePushRemote_EmptyNoRemotesErrors(t *testing.T) {
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "f.txt", "init")
	testutil.GitAdd(t, tmpDir, "f.txt")
	testutil.GitCommit(t, tmpDir, "init")
	t.Chdir(tmpDir)

	got, err := resolveMigratePushRemote(context.Background(), "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--remote")
	assert.Empty(t, got)
}

func TestDoctorMigrateCheckpoints_RefusesWhenRefsPrimary(t *testing.T) {
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "f.txt", "init")
	testutil.GitAdd(t, tmpDir, "f.txt")
	testutil.GitCommit(t, tmpDir, "init")
	require.NoError(t, os.MkdirAll(filepath.Join(tmpDir, ".entire"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, ".entire", "settings.json"),
		[]byte(`{"enabled": true, "checkpoints": {"primary": {"type": "git-refs"}}}`), 0o644))
	t.Chdir(tmpDir)
	paths.ClearWorktreeRootCache()

	cmd := newDoctorMigrateCheckpointsCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetContext(context.Background())
	require.NoError(t, cmd.Execute())
	assert.Contains(t, out.String(), "already the primary",
		"must refuse to migrate when git-refs is already the primary store")
}

// The OPF gate inside the push reads process-global config that only
// EnsureRedactionConfigured sets, so the push path must configure it before
// gating — otherwise the gate reads "OPF off" and flushes unscanned content.
//
// Asserted on the push path itself rather than through cobra: the guarantee
// must not depend on whether a parent command configures redaction, since
// PreRunE is not inherited and doctor's lives on its own.
func TestPushMigratedRefs_ConfiguresRedactionBeforeGating(t *testing.T) {
	repo, bareDir := setupMigratePushRepo(t,
		`{"enabled": true, "redaction": {"openai_privacy_filter": {"enabled": true, "categories": {"private_person": true}}}}`)

	strategy.ResetRedactionConfiguredForTest()
	t.Cleanup(strategy.ResetRedactionConfiguredForTest)
	redact.ResetOPFConfigForTest()
	t.Cleanup(redact.ResetOPFConfigForTest)
	require.False(t, redact.OPFEnabled(), "precondition: OPF unconfigured before the push")

	var out bytes.Buffer
	require.NoError(t, pushMigratedRefs(context.Background(), &out, repo, bareDir))

	assert.True(t, redact.OPFEnabled(),
		"the push must configure redaction, or its OPF gate is a no-op and ships unscanned content")
}

// Read-only outcomes must not consult redaction settings at all: a repo whose
// scanner config is invalid still gets an honest --dry-run report. Regression —
// an unconditional PreRunE ran ahead of these early returns and failed here.
func TestDoctorMigrateCheckpoints_ReadOnlyPathsIgnoreScannerConfig(t *testing.T) {
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "f.txt", "init")
	testutil.GitAdd(t, tmpDir, "f.txt")
	testutil.GitCommit(t, tmpDir, "init")
	require.NoError(t, os.MkdirAll(filepath.Join(tmpDir, ".entire"), 0o755))
	// Both scanners off — settings.Load fails with ErrScannerConfig.
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, ".entire", "settings.json"),
		[]byte(`{"enabled": true, "redaction": {"betterleaks": {"enabled": false}, "goredact": {"enabled": false}}}`), 0o644))
	t.Chdir(tmpDir)
	paths.ClearWorktreeRootCache()

	cmd := newDoctorMigrateCheckpointsCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--dry-run"})
	cmd.SetContext(context.Background())

	require.NoError(t, cmd.Execute(), "--dry-run reads nothing redaction-related and must not fail on it")
	assert.Contains(t, out.String(), "nothing to migrate")
}

// setupMigratePushRepo builds a repo with the given .entire/settings.json and a
// bare remote, and returns the open repo plus the remote path.
//
// It queues no checkpoint refs, deliberately. The caller asserts that the push
// path configures redaction before it gates, which happens before the flush and
// so does not depend on the queue holding anything. Queuing one would tie the
// test to whether the fixture's content reaches the OPF runtime at all:
// redact.BatchBytesWithPrivacyFilter only shells out for leaves containing a
// space, so this fixture ("init") sends none and passes without `opf` installed
// — until someone puts a space in it, at which point the test needs a real OPF
// binary. Real queued refs scanned by a fake runtime are covered by
// TestPushQueuedCheckpointRefs_OPFAppliedBeforePush.
func setupMigratePushRepo(t *testing.T, settingsJSON string) (*git.Repository, string) {
	t.Helper()
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "f.txt", "init")
	testutil.GitAdd(t, tmpDir, "f.txt")
	testutil.GitCommit(t, tmpDir, "init")
	require.NoError(t, os.MkdirAll(filepath.Join(tmpDir, ".entire"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(tmpDir, ".entire", "settings.json"), []byte(settingsJSON), 0o644))
	t.Chdir(tmpDir)
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)

	bareDir := t.TempDir()
	testutil.RunGit(t, bareDir, "init", "--bare")

	repo, err := git.PlainOpen(tmpDir)
	require.NoError(t, err)
	t.Cleanup(func() { repo.Close() })
	return repo, bareDir
}
