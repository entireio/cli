package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"charm.land/huh/v2"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/gitdir"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/redact"
)

func TestValidateCheckpointDeleteFlags(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		flags   checkpointDeleteFlags
		wantErr string
	}{
		{name: "plain", flags: checkpointDeleteFlags{}},
		{name: "dry-run json", flags: checkpointDeleteFlags{dryRun: true, json: true}},
		{name: "force json", flags: checkpointDeleteFlags{force: true, json: true}},
		{name: "json alone cannot prompt", flags: checkpointDeleteFlags{json: true}, wantErr: "--json without --dry-run requires --force"},
		{name: "local-only with remote", flags: checkpointDeleteFlags{localOnly: true, remotes: []string{"origin"}}, wantErr: "--local-only cannot be combined with --remote"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := validateCheckpointDeleteFlags(tt.flags)
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tt.wantErr)
		})
	}
}

func TestSelectCheckpointDeleteTargets(t *testing.T) {
	t.Parallel()
	oid := plumbing.NewHash("1111111111111111111111111111111111111111")
	plan := &strategy.CheckpointDeletePlan{Targets: []strategy.CheckpointDeleteTarget{
		{Remotes: []string{"origin"}, URL: "/a", Reachable: true, RefOID: oid},
		{Remotes: []string{strategy.CheckpointRemoteTargetName}, URL: "/b", Reachable: true, RefOID: oid},
		{Remotes: []string{"upstream"}, URL: "/c", Reachable: true}, // holds nothing
		{Remotes: []string{"backup"}, URL: "/d"},                    // unreachable
	}}
	urls := func(ts []strategy.CheckpointDeleteTarget) []string {
		var out []string
		for _, t := range ts {
			out = append(out, t.URL)
		}
		return out
	}

	all, err := selectCheckpointDeleteTargets(plan, checkpointDeleteFlags{})
	require.NoError(t, err)
	assert.Equal(t, []string{"/a", "/b"}, urls(all), "default: every reachable holder")

	none, err := selectCheckpointDeleteTargets(plan, checkpointDeleteFlags{localOnly: true})
	require.NoError(t, err)
	assert.Empty(t, none)

	named, err := selectCheckpointDeleteTargets(plan, checkpointDeleteFlags{remotes: []string{strategy.CheckpointRemoteTargetName}})
	require.NoError(t, err)
	assert.Equal(t, []string{"/b"}, urls(named))

	notHolding, err := selectCheckpointDeleteTargets(plan, checkpointDeleteFlags{remotes: []string{"upstream"}})
	require.NoError(t, err)
	assert.Empty(t, notHolding, "a named remote without a copy has nothing to delete")

	_, err = selectCheckpointDeleteTargets(plan, checkpointDeleteFlags{remotes: []string{"typo"}})
	require.ErrorContains(t, err, `--remote "typo" is not a checkpoint remote`)
}

func TestCheckpointDeleteGuidance_TellsAgentsNotToRunIt(t *testing.T) {
	t.Parallel()
	facts, ok := agentHelpClassified("checkpoint delete")
	require.True(t, ok)
	assert.Equal(t, agentHelpAudienceUserOwned, facts.audience)
	assert.False(t, facts.listed)
	assert.Contains(t, agentHelpGuidance["checkpoint delete"], "Never run this unless the user")
	assert.Contains(t, agentHelpGuidance["checkpoint delete"], "do not pass --force on your own")
}

// deleteCmdFixture is a refs-backend repo with one checkpoint pushed to a bare
// origin. CWD is the repo, so the test cannot run in parallel.
type deleteCmdFixture struct {
	dir     string
	bareDir string
	cid     id.CheckpointID
	ref     plumbing.ReferenceName
}

func newDeleteCmdFixture(t *testing.T) deleteCmdFixture {
	t.Helper()
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "README.md", "# test")
	testutil.GitAdd(t, dir, "README.md")
	testutil.GitCommit(t, dir, "init")
	bareDir := t.TempDir()
	testutil.RunGit(t, bareDir, "init", "--bare")
	testutil.RunGit(t, dir, "remote", "add", "origin", bareDir)
	testutil.RunGit(t, dir, "push", "--no-verify", "origin", "HEAD")
	t.Chdir(dir)
	t.Setenv("ENTIRE_CHECKPOINTS_PRIMARY", "git-refs")
	paths.ClearWorktreeRootCache()
	gitdir.ClearCache()
	writeSettings(t, `{"enabled": true}`)

	cid := id.MustCheckpointID("01K6ZQ2M8E3V7R5T9Y4X6W2A1B")
	repo, err := gitrepo.OpenPath(dir)
	require.NoError(t, err)
	defer repo.Close()
	stores, err := checkpoint.Open(context.Background(), repo, checkpoint.OpenOptions{})
	require.NoError(t, err)
	require.NoError(t, stores.Persistent.Write(context.Background(), checkpoint.Session{
		CheckpointID: cid,
		SessionID:    "sess-1",
		Strategy:     "manual-commit",
		Agent:        "Claude Code",
		Transcript:   redact.AlreadyRedacted([]byte("transcript\n")),
		Prompts:      []string{"do it"},
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
		TokenUsage:   &types.TokenUsage{InputTokens: 3, OutputTokens: 4},
	}))
	ref, err := checkpoint.RefName(cid)
	require.NoError(t, err)
	testutil.RunGit(t, dir, "push", "--no-verify", "origin", ref.String()+":"+ref.String())
	return deleteCmdFixture{dir: dir, bareDir: bareDir, cid: cid, ref: ref}
}

func (f deleteCmdFixture) localHasRef(t *testing.T) bool {
	t.Helper()
	return strings.Contains(testutil.RunGit(t, f.dir, "for-each-ref", "--format=%(refname)"), f.ref.String())
}

func (f deleteCmdFixture) remoteHasRef(t *testing.T) bool {
	t.Helper()
	return strings.Contains(testutil.RunGit(t, f.bareDir, "for-each-ref", "--format=%(refname)"), f.ref.String())
}

func runCheckpointDeleteCmd(t *testing.T, canPrompt bool, args ...string) (string, error) {
	t.Helper()
	cmd := newCheckpointDeleteCmdWithPrompt(func() bool { return canPrompt })
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetContext(context.Background())
	cmd.SetArgs(args)
	err := cmd.Execute()
	return stdout.String(), err
}

func TestCheckpointDelete_DryRunJSONChangesNothing(t *testing.T) {
	f := newDeleteCmdFixture(t)

	stdout, err := runCheckpointDeleteCmd(t, false, f.cid.String(), "--dry-run", "--json")
	require.NoError(t, err)

	var doc checkpointDeleteJSON
	require.NoError(t, json.Unmarshal([]byte(stdout), &doc), stdout)
	assert.True(t, doc.DryRun)
	assert.Equal(t, f.cid.String(), doc.CheckpointID)
	assert.Equal(t, f.ref.String(), doc.Local.Ref)
	require.Len(t, doc.Remotes, 1)
	assert.Equal(t, "origin", doc.Remotes[0].Name)
	assert.Equal(t, "holds", doc.Remotes[0].Status)
	assert.True(t, doc.Remotes[0].Selected)
	assert.Nil(t, doc.Remotes[0].Result, "a dry run has no results")
	require.Len(t, doc.Sessions, 1)
	assert.Equal(t, checkpointDeleteSessionJSON{SessionID: "sess-1", Agent: "Claude Code", InputTokens: 3, OutputTokens: 4}, doc.Sessions[0])
	assert.False(t, doc.V1HistoryRetained)

	assert.True(t, f.localHasRef(t), "a dry run must not delete locally")
	assert.True(t, f.remoteHasRef(t), "a dry run must not delete on the remote")
}

func TestCheckpointDelete_RefusesWithoutConfirmation(t *testing.T) {
	f := newDeleteCmdFixture(t)

	_, err := runCheckpointDeleteCmd(t, false, f.cid.String())
	require.EqualError(t, err, "refusing to delete checkpoint "+f.cid.String()+" without confirmation; pass --force")
	assert.True(t, f.localHasRef(t))
	assert.True(t, f.remoteHasRef(t))
}

func TestCheckpointDelete_JSONWithoutForceRefused(t *testing.T) {
	f := newDeleteCmdFixture(t)

	_, err := runCheckpointDeleteCmd(t, false, f.cid.String(), "--json")
	require.ErrorContains(t, err, "--json without --dry-run requires --force")
	assert.True(t, f.localHasRef(t))
}

func TestCheckpointDelete_ActiveSessionRefusedWithoutForce(t *testing.T) {
	f := newDeleteCmdFixture(t)
	now := time.Now()
	require.NoError(t, strategy.SaveSessionState(context.Background(), &strategy.SessionState{
		SessionID: "sess-1", StartedAt: now, LastInteractionTime: &now, Phase: session.PhaseActive, LastCheckpointID: f.cid,
	}))

	_, err := runCheckpointDeleteCmd(t, true, f.cid.String())
	require.ErrorContains(t, err, "session sess-1 is still active")
	assert.True(t, f.localHasRef(t))

	stdout, err := runCheckpointDeleteCmd(t, false, f.cid.String(), "--force", "--json")
	require.NoError(t, err)
	assert.False(t, f.localHasRef(t), "--force overrides the active-session refusal")
	var doc checkpointDeleteJSON
	require.NoError(t, json.Unmarshal([]byte(stdout), &doc), stdout)
	require.Len(t, doc.SessionStates, 1)
	assert.True(t, doc.SessionStates[0].Active)
}

func TestCheckpointDelete_ForceJSONDeletesEverywhere(t *testing.T) {
	f := newDeleteCmdFixture(t)

	stdout, err := runCheckpointDeleteCmd(t, false, f.cid.String(), "-f", "--json")
	require.NoError(t, err)
	var doc checkpointDeleteJSON
	require.NoError(t, json.Unmarshal([]byte(stdout), &doc), stdout)
	assert.False(t, doc.DryRun)
	assert.Equal(t, "deleted", doc.Local.RefResult)
	require.Len(t, doc.Remotes, 1)
	require.NotNil(t, doc.Remotes[0].Result)
	assert.Equal(t, "deleted", doc.Remotes[0].Result.Ref)
	assert.Empty(t, doc.Remotes[0].Result.Retry)

	assert.False(t, f.localHasRef(t))
	assert.False(t, f.remoteHasRef(t))
}

func TestCheckpointDelete_RejectsPrefixAndInvalidIDs(t *testing.T) {
	f := newDeleteCmdFixture(t)

	_, err := runCheckpointDeleteCmd(t, false, f.cid.String()[:10], "--dry-run")
	require.ErrorContains(t, err, "prefixes are not accepted")
}

func TestPrintCheckpointDeleteResult_UnreachableRemoteIsNotClaimedDeleted(t *testing.T) {
	t.Parallel()
	cid := id.MustCheckpointID("a1b2c3d4e5f6")
	plan := &strategy.CheckpointDeletePlan{CheckpointID: cid, Targets: []strategy.CheckpointDeleteTarget{
		{Remotes: []string{"origin"}, URL: "/a", Reachable: true},
		{Remotes: []string{"backup"}, URL: "/b"},
	}}
	result := &strategy.CheckpointDeleteResult{LocalRef: strategy.DeleteOutcomeDeleted, LocalV1: strategy.DeleteOutcomeAbsent}

	var buf bytes.Buffer
	printCheckpointDeleteResult(&buf, plan, nil, result, checkpointDeleteFlags{})
	assert.Contains(t, buf.String(), "Could not check backup")
	assert.Contains(t, buf.String(), "entire checkpoint delete a1b2c3d4e5f6 --remote backup")
	assert.NotContains(t, buf.String(), "Deleted checkpoint a1b2c3d4e5f6.\n")
}

// --local-only never consults remotes, so the summary must not claim the
// checkpoint is gone everywhere: it names the remotes it left unchecked.
func TestPrintCheckpointDeleteResult_LocalOnlyNamesUncheckedRemotes(t *testing.T) {
	t.Parallel()
	cid := id.MustCheckpointID("a1b2c3d4e5f6")
	plan := &strategy.CheckpointDeletePlan{CheckpointID: cid, Targets: []strategy.CheckpointDeleteTarget{
		{Remotes: []string{"origin"}, URL: "/a", NotChecked: true},
	}}
	result := &strategy.CheckpointDeleteResult{LocalRef: strategy.DeleteOutcomeDeleted, LocalV1: strategy.DeleteOutcomeAbsent}

	var buf bytes.Buffer
	printCheckpointDeleteResult(&buf, plan, nil, result, checkpointDeleteFlags{localOnly: true})
	assert.NotContains(t, buf.String(), "Deleted checkpoint a1b2c3d4e5f6.\n")
	assert.Contains(t, buf.String(), "Deleted the local copy of checkpoint a1b2c3d4e5f6.")
	assert.Contains(t, buf.String(), "origin was not checked (--local-only)")
}

// A holder the user left out (--remote, or deselected) is named, not hidden
// behind a "Deleted checkpoint" claim.
func TestPrintCheckpointDeleteResult_NamesUnselectedHolders(t *testing.T) {
	t.Parallel()
	cid := id.MustCheckpointID("a1b2c3d4e5f6")
	oid := plumbing.NewHash("1111111111111111111111111111111111111111")
	origin := strategy.CheckpointDeleteTarget{Remotes: []string{"origin"}, URL: "/a", Reachable: true, RefOID: oid}
	upstream := strategy.CheckpointDeleteTarget{Remotes: []string{"upstream"}, URL: "/b", Reachable: true, RefOID: oid}
	plan := &strategy.CheckpointDeletePlan{CheckpointID: cid, Targets: []strategy.CheckpointDeleteTarget{origin, upstream}}
	result := &strategy.CheckpointDeleteResult{LocalRef: strategy.DeleteOutcomeDeleted, LocalV1: strategy.DeleteOutcomeAbsent,
		Targets: []strategy.CheckpointDeleteTargetResult{{Target: upstream, Ref: strategy.DeleteOutcomeDeleted, V1: strategy.DeleteOutcomeAbsent}}}

	var buf bytes.Buffer
	printCheckpointDeleteResult(&buf, plan, []strategy.CheckpointDeleteTarget{upstream}, result, checkpointDeleteFlags{remotes: []string{"upstream"}})
	assert.NotContains(t, buf.String(), "Deleted checkpoint a1b2c3d4e5f6.\n")
	assert.Contains(t, buf.String(), "Still on origin (not selected)")
}

// addUpstreamHolder adds a second bare remote, makes it the sync remote, and
// pushes the checkpoint there too, so two remotes hold it.
func (f deleteCmdFixture) addUpstreamHolder(t *testing.T) string {
	t.Helper()
	upstream := t.TempDir()
	testutil.RunGit(t, upstream, "init", "--bare")
	testutil.RunGit(t, f.dir, "remote", "add", "upstream", upstream)
	testutil.RunGit(t, f.dir, "push", "--no-verify", "upstream", f.ref.String()+":"+f.ref.String())
	writeSettings(t, `{"enabled": true, "strategy_options": {"checkpoint_push_remote": "upstream"}}`)
	return upstream
}

func runCheckpointDeleteCmdWithSelector(t *testing.T, selectTargets checkpointDeleteTargetSelector, args ...string) (string, error) {
	t.Helper()
	cmd := newCheckpointDeleteCmdWithPrompts(checkpointDeletePrompts{canPrompt: func() bool { return true }, selectTargets: selectTargets})
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetContext(context.Background())
	cmd.SetArgs(args)
	err := cmd.Execute()
	return stdout.String(), err
}

// Deselecting every remote of a checkpoint with no local copy leaves nothing
// to delete: the command cancels without recording anything.
func TestCheckpointDelete_DeselectAllWithoutLocalCopyCancels(t *testing.T) {
	f := newDeleteCmdFixture(t)
	upstream := f.addUpstreamHolder(t)
	testutil.RunGit(t, f.dir, "update-ref", "-d", f.ref.String())

	deselectAll := func(context.Context, []strategy.CheckpointDeleteTarget) ([]strategy.CheckpointDeleteTarget, error) {
		return nil, nil
	}
	stdout, err := runCheckpointDeleteCmdWithSelector(t, deselectAll, f.cid.String())
	require.NoError(t, err)
	assert.Contains(t, stdout, "No remote selected")
	assert.True(t, f.remoteHasRef(t))
	assert.Contains(t, testutil.RunGit(t, upstream, "for-each-ref", "--format=%(refname)"), f.ref.String())
	deleted, err := checkpoint.LoadDeletedCheckpoints(context.Background())
	require.NoError(t, err)
	assert.False(t, deleted.Contains(f.cid), "a cancelled delete records nothing")
}

// Esc in the remote multi-select is a clean cancel, like the confirmation.
func TestCheckpointDelete_AbortedRemoteSelectionCancels(t *testing.T) {
	f := newDeleteCmdFixture(t)
	f.addUpstreamHolder(t)

	abort := func(context.Context, []strategy.CheckpointDeleteTarget) ([]strategy.CheckpointDeleteTarget, error) {
		return nil, fmt.Errorf("remote selection: %w", huh.ErrUserAborted)
	}
	stdout, err := runCheckpointDeleteCmdWithSelector(t, abort, f.cid.String())
	require.NoError(t, err)
	assert.Contains(t, stdout, "Deletion cancelled.")
	assert.True(t, f.localHasRef(t))
	assert.True(t, f.remoteHasRef(t))
}

// --remote leaves the other holders alone, and the summary says so.
func TestCheckpointDelete_RemoteFlagReportsUnselectedHolder(t *testing.T) {
	f := newDeleteCmdFixture(t)
	upstream := f.addUpstreamHolder(t)

	stdout, err := runCheckpointDeleteCmd(t, false, f.cid.String(), "--remote", "upstream", "--force")
	require.NoError(t, err)
	assert.NotContains(t, testutil.RunGit(t, upstream, "for-each-ref", "--format=%(refname)"), f.ref.String())
	assert.True(t, f.remoteHasRef(t), "origin was not selected")
	assert.Contains(t, stdout, "Still on origin (not selected)")
	assert.NotContains(t, stdout, "Deleted checkpoint "+f.cid.String()+".\n")
}

// --local-only plans without contacting any remote: JSON marks them unchecked.
func TestCheckpointDelete_LocalOnlyDryRunDoesNotProbe(t *testing.T) {
	f := newDeleteCmdFixture(t)
	testutil.RunGit(t, f.dir, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "missing"))

	stdout, err := runCheckpointDeleteCmd(t, false, f.cid.String(), "--local-only", "--dry-run", "--json")
	require.NoError(t, err)
	var doc checkpointDeleteJSON
	require.NoError(t, json.Unmarshal([]byte(stdout), &doc), stdout)
	require.Len(t, doc.Remotes, 1)
	assert.Equal(t, "not_checked", doc.Remotes[0].Status)
	assert.Empty(t, doc.Warnings)

	text, err := runCheckpointDeleteCmd(t, false, f.cid.String(), "--local-only", "--dry-run")
	require.NoError(t, err)
	assert.Contains(t, text, "not checked (--local-only)")
}

// A miss in --json mode is still JSON, naming the remotes that could not be
// reached; the text error names them too.
func TestCheckpointDelete_NotFoundNamesUnreachableRemotes(t *testing.T) {
	f := newDeleteCmdFixture(t)
	testutil.RunGit(t, f.dir, "update-ref", "-d", f.ref.String())
	testutil.RunGit(t, f.dir, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "missing"))

	stdout, err := runCheckpointDeleteCmd(t, false, f.cid.String(), "--dry-run", "--json")
	require.Error(t, err)
	var doc checkpointDeleteJSON
	require.NoError(t, json.Unmarshal([]byte(stdout), &doc), stdout)
	assert.True(t, doc.NotFound)
	require.Len(t, doc.Remotes, 1)
	assert.Equal(t, "unreachable", doc.Remotes[0].Status)

	_, err = runCheckpointDeleteCmd(t, false, f.cid.String(), "--dry-run")
	require.ErrorContains(t, err, "could not reach origin")
}

func TestBuildCheckpointDeleteJSON_TrackingV1AndHistoryFlag(t *testing.T) {
	t.Parallel()
	cid := id.MustCheckpointID("a1b2c3d4e5f6")
	origin := strategy.CheckpointDeleteTarget{Remotes: []string{"origin"}, URL: "/a", Reachable: true, V1: strategy.V1CopyPresent}
	plan := &strategy.CheckpointDeletePlan{CheckpointID: cid, TrackingV1: []string{"origin"}, Targets: []strategy.CheckpointDeleteTarget{origin}}

	selected := buildCheckpointDeleteJSON(plan, []strategy.CheckpointDeleteTarget{origin}, nil, true, false)
	assert.Equal(t, []string{"origin"}, selected.Local.TrackingV1)
	assert.True(t, selected.V1HistoryRetained, "deleting the remote v1 copy leaves it in that branch's history")

	notSelected := buildCheckpointDeleteJSON(plan, nil, nil, true, true)
	assert.False(t, notSelected.V1HistoryRetained, "--local-only with only a remote v1 copy touches no v1 branch")

	var buf bytes.Buffer
	printCheckpointDeletePlan(&buf, plan, nil, true)
	assert.Contains(t, buf.String(), "refs/remotes/origin/"+paths.MetadataBranchName)
	assert.Contains(t, buf.String(), "until origin's copy is deleted")
}
