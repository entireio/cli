package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// fakeSyncEngine records every engine call and answers from canned data. Its
// Hydrate writes the inventoried refs into the local repo at HEAD, so the
// verify step has something real to list.
type fakeSyncEngine struct {
	t             *testing.T
	inventories   map[string]strategy.CheckpointInventory
	inventoryErrs map[string]error
	publishPushed int
	publishOff    bool
	publishErr    error
	verifyMissing []plumbing.ReferenceName
	removeErr     error
	state         strategy.EntireSyncState
	stateOK       bool

	calls        []string
	removed      map[string][]plumbing.ReferenceName
	removedV1    map[string]bool
	marks        []strategy.EntireSyncMigration
	convertCalls int
}

func newFakeSyncEngine(t *testing.T) *fakeSyncEngine {
	t.Helper()
	return &fakeSyncEngine{
		t:             t,
		inventories:   map[string]strategy.CheckpointInventory{},
		inventoryErrs: map[string]error{},
		removed:       map[string][]plumbing.ReferenceName{},
		removedV1:     map[string]bool{},
	}
}

func (f *fakeSyncEngine) record(call string) { f.calls = append(f.calls, call) }

func (f *fakeSyncEngine) Inventory(_ context.Context, remote string) (strategy.CheckpointInventory, error) {
	f.record("inventory:" + remote)
	if err := f.inventoryErrs[remote]; err != nil {
		return strategy.CheckpointInventory{}, err
	}
	inv := f.inventories[remote]
	inv.Remote = remote
	return inv, nil
}

func (f *fakeSyncEngine) Hydrate(_ context.Context, repo *git.Repository, from string, inv strategy.CheckpointInventory, _ strategy.ProgressFunc) (strategy.HydrateResult, error) {
	f.record("hydrate:" + from)
	for ref, hash := range inv.Refs {
		require.NoError(f.t, repo.Storer.SetReference(plumbing.NewHashReference(ref, hash)))
	}
	return strategy.HydrateResult{RefsFetched: len(inv.Refs)}, nil
}

func (f *fakeSyncEngine) Convert(_ context.Context, _ *git.Repository, dryRun bool) (checkpoint.MigrateResult, error) {
	if !dryRun {
		f.convertCalls++
		f.record("convert")
	}
	return checkpoint.MigrateResult{}, nil
}

func (f *fakeSyncEngine) Requeue(_ context.Context, _ *git.Repository) (int, error) {
	f.record("requeue")
	return 0, nil
}

func (f *fakeSyncEngine) Publish(_ context.Context, _ *git.Repository, to string) (int, bool, error) {
	f.record("publish:" + to)
	return f.publishPushed, f.publishOff, f.publishErr
}

func (f *fakeSyncEngine) Verify(_ context.Context, _ *git.Repository, to string, expected []plumbing.ReferenceName) ([]plumbing.ReferenceName, []plumbing.ReferenceName, error) {
	f.record("verify:" + to)
	var verified []plumbing.ReferenceName
	for _, ref := range expected {
		if !containsRef(f.verifyMissing, ref) {
			verified = append(verified, ref)
		}
	}
	return verified, f.verifyMissing, nil
}

func (f *fakeSyncEngine) Remove(_ context.Context, from string, refs []plumbing.ReferenceName, deleteV1 bool, _ strategy.ProgressFunc) (strategy.RemoveResult, error) {
	f.record("remove:" + from)
	if f.removeErr != nil {
		return strategy.RemoveResult{}, f.removeErr
	}
	f.removed[from] = refs
	f.removedV1[from] = deleteV1
	return strategy.RemoveResult{RefsDeleted: len(refs), V1Deleted: deleteV1}, nil
}

func (f *fakeSyncEngine) LoadState(context.Context) (strategy.EntireSyncState, bool) {
	return f.state, f.stateOK
}

func (f *fakeSyncEngine) MarkMigration(_ context.Context, status strategy.EntireSyncMigration, _ string) error {
	f.record("mark:" + string(status))
	f.marks = append(f.marks, status)
	return nil
}

func containsRef(refs []plumbing.ReferenceName, ref plumbing.ReferenceName) bool {
	for _, r := range refs {
		if r == ref {
			return true
		}
	}
	return false
}

func (f *fakeSyncEngine) called(prefix string) bool {
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

// syncTestRepo is a repo with one commit, origin (GitHub) and an Entire remote,
// where origin holds two checkpoint refs and the v1 branch. It swaps in the
// fake engine and restores every seam on cleanup. Tests using it change the
// process cwd and cannot run in parallel.
func syncTestRepo(t *testing.T, settingsJSON string) (*fakeSyncEngine, plumbing.Hash) {
	t.Helper()
	testutil.IsolateGitConfigEnv(t)
	dir := setupTestRepo(t)
	testutil.WriteFile(t, dir, "README.md", "hello")
	testutil.GitAdd(t, dir, "README.md")
	testutil.GitCommit(t, dir, "initial")
	head := plumbing.NewHash(testutil.GetHeadHash(t, dir))
	writeSettings(t, settingsJSON)
	testutil.AddRemote(t, dir, "origin", "https://github.com/acme/app.git")
	testutil.AddRemote(t, dir, testEntireRemote, "entire://cluster.test/gh/acme/app")

	fake := newFakeSyncEngine(t)
	fake.inventories["origin"] = strategy.CheckpointInventory{
		Refs: map[plumbing.ReferenceName]plumbing.Hash{
			"refs/entire/checkpoints/ab/0123456789ab": head,
			"refs/entire/checkpoints/cd/0123456789cd": head,
		},
		V1Branch: head,
	}
	fake.inventories[testEntireRemote] = strategy.CheckpointInventory{}
	fake.publishPushed = 2

	prevEngine, prevPrompt, prevConfirm := newSyncEngine, syncCanPrompt, syncConfirmFn
	newSyncEngine = func() syncEngine { return fake }
	syncCanPrompt = func() bool { return false }
	t.Cleanup(func() {
		newSyncEngine, syncCanPrompt, syncConfirmFn = prevEngine, prevPrompt, prevConfirm
		strategy.InvalidateGitRemoteCache(context.Background())
	})
	return fake, head
}

func runSync(t *testing.T, opts checkpointMigrateOptions) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := runCheckpointMigrate(context.Background(), &out, io.Discard, opts)
	return out.String(), err
}

func readProjectSettingsBackend(t *testing.T) string {
	t.Helper()
	cfg, err := settings.LoadCheckpointsConfig(context.Background())
	require.NoError(t, err)
	if cfg == nil {
		return ""
	}
	return cfg.Primary.Type
}

func TestRunCheckpointMigrate_NonInteractiveReportsAndChangesNothing(t *testing.T) {
	fake, _ := syncTestRepo(t, testSettingsEnabled)

	out, err := runSync(t, checkpointMigrateOptions{})
	require.NoError(t, err)

	assert.Contains(t, out, "Destination")
	assert.Contains(t, out, "entire (your Entire remote)")
	assert.Contains(t, out, "origin (github.com/acme/app) holds 2 checkpoint refs and the entire/checkpoints/v1 branch")
	assert.Contains(t, out, "entire checkpoint migrate --yes")
	assert.Contains(t, out, "entire checkpoint migrate --yes --remove")
	assert.True(t, fake.called("inventory:"), "the plan needs the inventory")
	for _, forbidden := range []string{"hydrate:", "publish:", "verify:", "remove:", "mark:", "convert"} {
		assert.False(t, fake.called(forbidden), "a bare non-interactive run must not %s", forbidden)
	}
	assert.Empty(t, readProjectSettingsBackend(t), "settings must not be written")
}

func TestRunCheckpointMigrate_YesMigratesWithoutRemoving(t *testing.T) {
	fake, _ := syncTestRepo(t, testSettingsEnabled)

	out, err := runSync(t, checkpointMigrateOptions{Yes: true})
	require.NoError(t, err)

	assert.True(t, fake.called("hydrate:origin"))
	assert.True(t, fake.called("publish:entire"))
	assert.True(t, fake.called("verify:entire"))
	assert.False(t, fake.called("remove:"), "--yes never implies --remove")
	assert.Equal(t, []strategy.EntireSyncMigration{strategy.EntireSyncMigrationDone}, fake.marks)

	assert.Contains(t, out, "Your checkpoints now live on Entire.")
	assert.Contains(t, out, "Older copies stay on origin")
	assert.Contains(t, out, "entire checkpoint migrate --remove")
	assert.Contains(t, out, "Commit .entire/settings.json")
	assert.Equal(t, checkpoint.BackendTypeGitRefs, readProjectSettingsBackend(t), "a git-branch repo is flipped to git-refs")
}

func TestRunCheckpointMigrate_YesRemoveDeletesOnlyVerified(t *testing.T) {
	fake, _ := syncTestRepo(t, testSettingsEnabled)

	out, err := runSync(t, checkpointMigrateOptions{Yes: true, Remove: true})
	require.NoError(t, err)

	require.Len(t, fake.removed["origin"], 2, "both verified refs are deleted from origin")
	assert.True(t, fake.removedV1["origin"], "the v1 branch goes too once every checkpoint is a ref")
	assert.Contains(t, out, "Removed 2 checkpoint refs and entire/checkpoints/v1 from origin")
	assert.NotContains(t, out, "Older copies stay")
	assert.Contains(t, out, "Your checkpoints now live on Entire.")
}

func TestRunCheckpointMigrate_VerifyMissingStopsBeforeRemove(t *testing.T) {
	fake, _ := syncTestRepo(t, testSettingsEnabled)
	fake.verifyMissing = []plumbing.ReferenceName{"refs/entire/checkpoints/ab/0123456789ab"}

	out, err := runSync(t, checkpointMigrateOptions{Yes: true, Remove: true})
	require.Error(t, err)
	var silent *SilentError
	require.ErrorAs(t, err, &silent, "the message was already printed; exit silently")

	assert.False(t, fake.called("remove:"), "nothing is removed while a ref is missing on the destination")
	assert.Empty(t, fake.marks, "the ledger stays pending so status keeps nudging")
	assert.Contains(t, out, "1 of 2 checkpoint refs did not arrive on entire. Nothing was removed from origin.")
}

func TestRunCheckpointMigrate_RemoveFailureKeepsCheckpointsSafe(t *testing.T) {
	fake, _ := syncTestRepo(t, testSettingsEnabled)
	fake.removeErr = errors.New("remote hung up")

	out, err := runSync(t, checkpointMigrateOptions{Yes: true, Remove: true})
	require.Error(t, err)

	assert.Contains(t, out, "Could not remove old copies from origin")
	assert.Contains(t, out, "Your checkpoints are safe on entire")
	assert.Contains(t, out, "entire checkpoint migrate --remove")
	assert.Contains(t, out, "Your checkpoints now live on Entire.", "the migration itself succeeded and is celebrated")
	assert.Equal(t, []strategy.EntireSyncMigration{strategy.EntireSyncMigrationDone}, fake.marks)
}

func TestRunCheckpointMigrate_DryRunTouchesNothing(t *testing.T) {
	fake, _ := syncTestRepo(t, testSettingsEnabled)

	out, err := runSync(t, checkpointMigrateOptions{DryRun: true})
	require.NoError(t, err)

	assert.Contains(t, out, "Would fetch 2 checkpoint refs from origin")
	assert.Contains(t, out, "Would push")
	assert.Contains(t, out, "only with --remove")
	for _, c := range fake.calls {
		assert.True(t, strings.HasPrefix(c, "inventory:"), "dry run may only list remotes, got %s", c)
	}
	assert.Empty(t, readProjectSettingsBackend(t))
}

func TestRunCheckpointMigrate_PushSessionsDisabledStops(t *testing.T) {
	fake, _ := syncTestRepo(t, `{"enabled": true, "strategy_options": {"push_sessions": false}}`)
	fake.publishOff = true

	out, err := runSync(t, checkpointMigrateOptions{Yes: true, Remove: true})
	require.Error(t, err)

	assert.Contains(t, out, "push_sessions is false")
	assert.False(t, fake.called("remove:"))
	assert.Empty(t, fake.marks)
}

func TestRunCheckpointMigrate_JSONPlan(t *testing.T) {
	syncTestRepo(t, testSettingsEnabled)

	out, err := runSync(t, checkpointMigrateOptions{JSON: true})
	require.NoError(t, err)

	var got checkpointMigrateJSON
	require.NoError(t, json.Unmarshal([]byte(out), &got), "output: %s", out)
	assert.Equal(t, "migrate", got.State)
	assert.Equal(t, testEntireRemote, got.Destination)
	assert.Equal(t, testEntireRemote, got.DestinationSource)
	assert.Equal(t, []string{"origin"}, got.Sources)
	assert.Contains(t, got.Steps, stepHydrate)
	assert.Contains(t, got.Steps, stepConvertBackend)
	assert.Nil(t, got.Result, "a plan-only run carries no result")
	assert.Equal(t, "entire checkpoint migrate --yes", got.NextCommand)
}

func TestRunCheckpointMigrate_InteractiveDeclineRecordsDeclined(t *testing.T) {
	fake, _ := syncTestRepo(t, testSettingsEnabled)
	syncCanPrompt = func() bool { return true }
	var prompts []string
	syncConfirmFn = func(_ context.Context, _ io.Writer, title string) (bool, error) {
		prompts = append(prompts, title)
		// Agree to the copy, decline the deletion.
		return !strings.HasPrefix(title, "Delete"), nil
	}

	out, err := runSync(t, checkpointMigrateOptions{})
	require.NoError(t, err)

	require.Len(t, prompts, 2, "one combined question, then one removal question: %v", prompts)
	assert.Contains(t, prompts[0], "copy 2 checkpoints from origin to entire")
	assert.Contains(t, prompts[0], "verify")
	assert.Equal(t, "Delete 2 checkpoint refs and the entire/checkpoints/v1 branch from origin (github.com/acme/app)? Your code is untouched.", prompts[1])
	assert.False(t, fake.called("remove:"))
	assert.Equal(t, []strategy.EntireSyncMigration{strategy.EntireSyncMigrationDone, strategy.EntireSyncMigrationDeclined}, fake.marks)
	assert.Contains(t, out, "Older copies stay on origin")
}

func TestRunCheckpointMigrate_InteractiveDeclineOfCopyChangesNothing(t *testing.T) {
	fake, _ := syncTestRepo(t, testSettingsEnabled)
	syncCanPrompt = func() bool { return true }
	syncConfirmFn = func(context.Context, io.Writer, string) (bool, error) { return false, nil }

	out, err := runSync(t, checkpointMigrateOptions{})
	require.NoError(t, err)

	assert.Contains(t, out, syncNothingChanged)
	assert.False(t, fake.called("hydrate:"))
	assert.Empty(t, readProjectSettingsBackend(t))
}

func TestRunCheckpointMigrate_PinnedElsewhereSwitchWritesLocalSetting(t *testing.T) {
	fake, _ := syncTestRepo(t, `{"enabled": true, "strategy_options": {"checkpoint_push_remote": "origin"}}`)
	syncCanPrompt = func() bool { return true }
	var prompts []string
	syncConfirmFn = func(_ context.Context, _ io.Writer, title string) (bool, error) {
		prompts = append(prompts, title)
		return true, nil
	}

	out, err := runSync(t, checkpointMigrateOptions{})
	require.NoError(t, err)

	require.NotEmpty(t, prompts)
	assert.Equal(t, "Switch checkpoint sync to entire?", prompts[0])
	assert.Contains(t, out, "Recorded strategy_options.checkpoint_push_remote in .entire/settings.local.json")
	assert.True(t, fake.called("publish:entire"))

	raw, err := os.ReadFile(filepath.Join(".entire", "settings.local.json"))
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"checkpoint_push_remote": "entire"`)
	elected, err := strategy.ResolveCheckpointSyncRemote(context.Background())
	require.NoError(t, err)
	assert.Equal(t, strategy.CheckpointSyncRemote{Name: testEntireRemote, Source: strategy.SyncRemoteSourceConfig}, elected)
}

func TestRunCheckpointMigrate_ToAlreadyElectedWritesNothing(t *testing.T) {
	syncTestRepo(t, testSettingsEnabled)

	_, err := runSync(t, checkpointMigrateOptions{To: testEntireRemote, DryRun: true})
	require.NoError(t, err)
	_, statErr := os.Stat(filepath.Join(".entire", "settings.local.json"))
	assert.True(t, os.IsNotExist(statErr), "--to naming the elected remote records nothing")
}

func TestRunCheckpointMigrate_UnknownToIsAnError(t *testing.T) {
	syncTestRepo(t, testSettingsEnabled)

	_, err := runSync(t, checkpointMigrateOptions{To: "nowhere"})
	require.Error(t, err)
}

func TestRunCheckpointMigrate_InventoryErrorLeavesRemoteUntouched(t *testing.T) {
	fake, _ := syncTestRepo(t, testSettingsEnabled)
	testutil.AddRemote(t, ".", "fork", "https://github.com/me/app.git")
	strategy.InvalidateGitRemoteCache(context.Background())
	fake.inventoryErrs["fork"] = errors.New("auth failed")

	out, err := runSync(t, checkpointMigrateOptions{Yes: true, Remove: true})
	require.NoError(t, err)

	assert.Contains(t, out, "Could not check fork")
	assert.Contains(t, out, "It is left untouched")
	assert.False(t, fake.called("remove:fork"))
	assert.True(t, fake.called("remove:origin"))
}

func TestSetCheckpointPushRemoteLocally_PreservesOtherKeys(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	dir := setupTestRepo(t)
	writeSettings(t, testSettingsEnabled)
	testutil.AddRemote(t, dir, "origin", "https://github.com/acme/app.git")
	testutil.AddRemote(t, dir, "fork", "https://github.com/me/app.git")
	require.NoError(t, os.WriteFile(filepath.Join(".entire", "settings.local.json"),
		[]byte(`{"log_level": "DEBUG", "strategy_options": {"push_sessions": true}}`), 0o600))
	t.Cleanup(func() { strategy.InvalidateGitRemoteCache(context.Background()) })

	grant := setCheckpointPushRemoteLocally(context.Background(), "fork")
	require.True(t, grant.Effective, "reason: %s", grant.Reason)

	raw, err := os.ReadFile(filepath.Join(".entire", "settings.local.json"))
	require.NoError(t, err)
	var doc map[string]any
	require.NoError(t, json.Unmarshal(raw, &doc))
	assert.Equal(t, "DEBUG", doc["log_level"])
	so, ok := doc["strategy_options"].(map[string]any)
	require.True(t, ok, "strategy_options must stay an object: %s", raw)
	assert.Equal(t, true, so["push_sessions"])
	assert.Equal(t, "fork", so["checkpoint_push_remote"])
}

func TestRunCheckpointMigrate_AlreadyHomeReportsValue(t *testing.T) {
	fake, head := syncTestRepo(t, `{"enabled": true, "checkpoints": {"primary": {"type": "git-refs"}}}`)
	// Everything already lives on the Entire remote: local refs match its listing.
	ref := plumbing.ReferenceName("refs/entire/checkpoints/ab/0123456789ab")
	testutil.RunGit(t, ".", "update-ref", ref.String(), head.String())
	fake.inventories["origin"] = strategy.CheckpointInventory{}
	fake.inventories[testEntireRemote] = strategy.CheckpointInventory{Refs: map[plumbing.ReferenceName]plumbing.Hash{ref: head}}

	out, err := runSync(t, checkpointMigrateOptions{})
	require.NoError(t, err)

	assert.Contains(t, out, "Checkpoints live on entire (your Entire remote)")
	assert.Equal(t, []strategy.EntireSyncMigration{strategy.EntireSyncMigrationDone}, fake.marks, "a repo that is already home records the ledger so status stops nudging")
	assert.False(t, fake.called("publish:"))
}
