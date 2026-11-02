//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/execx"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/stretchr/testify/require"
)

// checkpointPushJSON decodes `entire checkpoint push --json`.
type checkpointPushJSON struct {
	Results []checkpointPushResultJSON `json:"results"`
}

type checkpointPushResultJSON struct {
	CheckpointID    string `json:"checkpoint_id"`
	Ref             string `json:"ref"`
	Commit          string `json:"commit"`
	RequestedCommit string `json:"requested_commit"`
	Status          string `json:"status"`
	Error           string `json:"error"`
}

func (r checkpointPushJSON) status(t *testing.T, checkpointID string) string {
	t.Helper()
	for _, result := range r.Results {
		if result.CheckpointID == checkpointID {
			return result.Status
		}
	}
	t.Fatalf("checkpoint %s not in checkpoint push results: %+v", checkpointID, r.Results)
	return ""
}

// policyListing is the part of `checkpoint list --outgoing --json` the policy adds.
type policyListing struct {
	Policy      string `json:"checkpoint_push_policy"`
	PolicyError string `json:"checkpoint_push_policy_error"`
	Checkpoints []struct {
		CheckpointID string   `json:"checkpoint_id"`
		Ref          string   `json:"ref"`
		Commit       string   `json:"commit"`
		SessionIDs   []string `json:"session_ids"`
	} `json:"checkpoints"`
}

func listPolicyOutgoing(t *testing.T, env *TestEnv) policyListing {
	t.Helper()
	var listing policyListing
	raw := cliStdout(t, env, "checkpoint", "list", "--outgoing", "--json")
	require.NoError(t, json.Unmarshal([]byte(raw), &listing), "outgoing --json must be valid JSON, got: %s", raw)
	return listing
}

// outgoingCommit is the pin an `<id>@<commit>` caller copies from --outgoing.
func outgoingCommit(t *testing.T, env *TestEnv, checkpointID string) string {
	t.Helper()
	for _, row := range listPolicyOutgoing(t, env).Checkpoints {
		if row.CheckpointID == checkpointID {
			require.NotEmpty(t, row.Commit, "--outgoing must give the commit to pin")
			return row.Commit
		}
	}
	t.Fatalf("checkpoint %s is not outgoing", checkpointID)
	return ""
}

func gitCommonDirFile(t *testing.T, env *TestEnv, name string) string {
	t.Helper()
	commonDir := strings.TrimSpace(testutil.RunGit(t, env.RepoDir, "rev-parse", "--path-format=absolute", "--git-common-dir"))
	return filepath.Join(commonDir, filepath.FromSlash(name))
}

func writeGitCommonDirFile(t *testing.T, env *TestEnv, name, content string) {
	t.Helper()
	path := gitCommonDirFile(t, env, name)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
}

func setCheckpointPushPolicy(t *testing.T, env *TestEnv, policy string) {
	t.Helper()
	writeGitCommonDirFile(t, env, settings.CheckpointPushPolicyFile, policy+"\n")
}

// gitPushWithHooksIn pushes HEAD from dir through the real pre-push hook and
// returns git's combined output, which carries the hook's stderr.
func gitPushWithHooksIn(t *testing.T, env *TestEnv, dir string) string {
	t.Helper()
	env.InstallRealPrePushHook()
	cmd := execx.NonInteractive(t.Context(), "git", "push", "origin", "HEAD")
	cmd.Dir = dir
	cmd.Env = env.cliEnv()
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "the code push must succeed whatever the policy: %s", output)
	return string(output)
}

// runCheckpointPush runs `entire checkpoint push`, as a terminal when tty is
// true, and returns stdout, stderr and the exit error.
func runCheckpointPush(t *testing.T, env *TestEnv, tty bool, args ...string) (string, string, error) {
	t.Helper()
	ttyValue := "0"
	if tty {
		ttyValue = "1"
	}
	cmd := execx.NonInteractive(t.Context(), getTestBinary(), append([]string{"checkpoint", "push"}, args...)...)
	cmd.Dir = env.RepoDir
	cmd.Env = append(env.cliEnv(), "ENTIRE_TEST_TTY="+ttyValue)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

func runCheckpointPushJSON(t *testing.T, env *TestEnv, tty bool, args ...string) (checkpointPushJSON, error) {
	t.Helper()
	stdout, stderr, err := runCheckpointPush(t, env, tty, append(args, "--json")...)
	var parsed checkpointPushJSON
	require.NoError(t, json.Unmarshal([]byte(stdout), &parsed), "checkpoint push --json must be valid JSON\nstdout: %s\nstderr: %s", stdout, stderr)
	return parsed, err
}

func remoteBranchMatchesHead(t *testing.T, env *TestEnv, bareDir string) {
	t.Helper()
	branch := strings.TrimSpace(testutil.RunGit(t, env.RepoDir, "rev-parse", "--abbrev-ref", "HEAD"))
	head := strings.TrimSpace(testutil.RunGit(t, env.RepoDir, "rev-parse", "HEAD"))
	remote := strings.TrimSpace(testutil.RunGit(t, bareDir, "rev-parse", "refs/heads/"+branch))
	require.Equal(t, head, remote, "the user's code must reach the remote")
}

func TestCheckpointPushPolicy_ManualGitPushSendsNoCheckpoints(t *testing.T) {
	t.Parallel()
	env := newOutgoingEnv(t)
	bareDir := env.SetupBareRemote()
	setCheckpointPushPolicy(t, env, "manual")

	auth := createCheckpointedCommit(t, env, "Add auth module", "auth.go", "package auth", "Add auth module")
	billing := createCheckpointedCommit(t, env, "Add billing module", "billing.go", "package billing", "Add billing module")

	output := gitPushWithHooksIn(t, env, env.RepoDir)

	remoteBranchMatchesHead(t, env, bareDir)
	require.Empty(t, remoteCheckpointIDs(t, bareDir), "under manual, git push sends no checkpoints")
	require.Contains(t, output, "2 checkpoints waiting")
	require.Contains(t, output, "entire checkpoint push --all", "the suggested command must upload when run as printed")

	listing := listPolicyOutgoing(t, env)
	require.Equal(t, "manual", listing.Policy)
	require.Len(t, listing.Checkpoints, 2)
	outgoing, _ := listOutgoing(t, env)
	require.Equal(t, sortedIDs(auth, billing), outgoing.ids(), "waiting checkpoints stay queued")
}

func TestCheckpointPushPolicy_AutomaticIsTheDefault(t *testing.T) {
	t.Parallel()
	env := newOutgoingEnv(t)
	bareDir := env.SetupBareRemote()

	work := createCheckpointedCommit(t, env, "Some work", "work.go", "package work", "Some work")
	require.Equal(t, "automatic", listPolicyOutgoing(t, env).Policy)

	output := gitPushWithHooksIn(t, env, env.RepoDir)
	require.Equal(t, []string{work}, remoteCheckpointIDs(t, bareDir))
	require.NotContains(t, output, "waiting")
}

// The policy is per clone: the push queue is shared by every worktree, so a
// push from a linked worktree must see the policy set from the main one.
func TestCheckpointPushPolicy_AppliesToEveryWorktree(t *testing.T) {
	t.Parallel()
	env := newOutgoingEnv(t)
	bareDir := env.SetupBareRemote()
	work := createCheckpointedCommit(t, env, "Some work", "work.go", "package work", "Some work")

	linked := filepath.Join(t.TempDir(), "linked")
	testutil.RunGit(t, env.RepoDir, "worktree", "add", "-b", "linked-branch", linked)
	// The fixture leaves settings.json uncommitted; a real repository commits
	// it, so every worktree has Entire enabled.
	settingsJSON, err := os.ReadFile(filepath.Join(env.RepoDir, ".entire", "settings.json"))
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Join(linked, ".entire"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(linked, ".entire", "settings.json"), settingsJSON, 0o600))

	setCheckpointPushPolicy(t, env, "manual")
	gitPushWithHooksIn(t, env, linked)
	require.Empty(t, remoteCheckpointIDs(t, bareDir), "manual set from the main worktree must hold for a linked one")

	// Control: the same push uploads once the policy allows it, so the
	// assertion above is not passing because the linked worktree skips Entire.
	setCheckpointPushPolicy(t, env, "automatic")
	gitPushWithHooksIn(t, env, linked)
	require.Equal(t, []string{work}, remoteCheckpointIDs(t, bareDir))
}

// A policy file this binary cannot read must not read as automatic, and the
// warning names the file to fix.
func TestCheckpointPushPolicy_UnreadablePolicyUploadsNothing(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		plant     func(t *testing.T, env *TestEnv)
		namesFile bool
	}{
		{name: "empty", namesFile: true, plant: func(t *testing.T, env *TestEnv) {
			t.Helper()
			writeGitCommonDirFile(t, env, settings.CheckpointPushPolicyFile, "")
		}},
		{name: "unknown value", namesFile: true, plant: func(t *testing.T, env *TestEnv) {
			t.Helper()
			setCheckpointPushPolicy(t, env, "approved")
		}},
		{name: "symlink", plant: func(t *testing.T, env *TestEnv) {
			t.Helper()
			path := gitCommonDirFile(t, env, settings.CheckpointPushPolicyFile)
			require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
			planted := filepath.Join(filepath.Dir(path), "planted")
			require.NoError(t, os.WriteFile(planted, []byte("automatic\n"), 0o600))
			if err := os.Symlink("planted", path); err != nil {
				t.Skipf("cannot create symlink: %v", err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			env := newOutgoingEnv(t)
			bareDir := env.SetupBareRemote()
			createCheckpointedCommit(t, env, "Some work", "work.go", "package work", "Some work")
			tt.plant(t, env)

			output := gitPushWithHooksIn(t, env, env.RepoDir)

			remoteBranchMatchesHead(t, env, bareDir)
			require.Empty(t, remoteCheckpointIDs(t, bareDir), "an unreadable policy must not read as automatic")
			if tt.namesFile {
				require.Contains(t, output, "checkpoint-push-policy", "the warning names the file to fix")
			}
		})
	}
}

// A committed settings.json this binary cannot load (here, a key from a newer
// version) must not loosen manual. The push path's settings fall back to
// defaults on a load error (resolvePushSettings), so the policy is read from
// its own file alone.
func TestCheckpointPushPolicy_BrokenProjectSettingsKeepManual(t *testing.T) {
	t.Parallel()
	env := newOutgoingEnv(t)
	bareDir := env.SetupBareRemote()
	createCheckpointedCommit(t, env, "Some work", "work.go", "package work", "Some work")
	setCheckpointPushPolicy(t, env, "manual")
	env.PatchSettings(map[string]any{"setting_from_a_newer_version": true})

	gitPushWithHooksIn(t, env, env.RepoDir)

	remoteBranchMatchesHead(t, env, bareDir)
	require.Empty(t, remoteCheckpointIDs(t, bareDir))
}

// Released binaries rewrite clone preferences without the keys they do not
// model, for example when the trails cache refreshes. That must not turn a
// manual clone back into automatic.
func TestCheckpointPushPolicy_ManualSurvivesPreferencesRewrite(t *testing.T) {
	t.Parallel()
	env := newOutgoingEnv(t)
	bareDir := env.SetupBareRemote()
	env.RunCLI("configure", "--checkpoint-push-policy", "manual")
	writeGitCommonDirFile(t, env, settings.ClonePreferencesFile, `{"trails_enabled":true}`)
	createCheckpointedCommit(t, env, "Some work", "work.go", "package work", "Some work")

	gitPushWithHooksIn(t, env, env.RepoDir)

	remoteBranchMatchesHead(t, env, bareDir)
	require.Empty(t, remoteCheckpointIDs(t, bareDir), "an older binary's preferences rewrite must not release checkpoints")
}

func TestCheckpointPush_SendsOnlyTheNamedCheckpoint(t *testing.T) {
	t.Parallel()
	env := newOutgoingEnv(t)
	bareDir := env.SetupBareRemote()
	setCheckpointPushPolicy(t, env, "manual")
	auth := createCheckpointedCommit(t, env, "Add auth module", "auth.go", "package auth", "Add auth module")
	billing := createCheckpointedCommit(t, env, "Add billing module", "billing.go", "package billing", "Add billing module")
	gitPushWithHooksIn(t, env, env.RepoDir)

	result, err := runCheckpointPushJSON(t, env, false, auth+"@"+outgoingCommit(t, env, auth))
	require.NoError(t, err)
	require.Equal(t, "uploaded", result.status(t, auth))

	require.Equal(t, []string{auth}, remoteCheckpointIDs(t, bareDir))
	outgoing, _ := listOutgoing(t, env)
	require.Equal(t, []string{billing}, outgoing.ids(), "the checkpoint not named stays waiting")
}

func TestCheckpointPush_PinnedCommitThatMovedIsNotSent(t *testing.T) {
	t.Parallel()
	env := newOutgoingEnv(t)
	bareDir := env.SetupBareRemote()
	setCheckpointPushPolicy(t, env, "manual")

	session := env.NewSession()
	transcriptPath := session.CreateTranscript("Add cache layer", []FileChange{{Path: "cache.go", Content: "package cache"}})
	require.NoError(t, env.SimulateUserPromptSubmitWithPromptAndTranscriptPath(session.ID, "Add cache layer", transcriptPath))
	env.WriteFile("cache.go", "package cache")
	env.GitAdd("cache.go")
	env.GitCommitWithHooks("Add cache layer", "cache.go")
	cache := env.LatestCheckpointID()
	seen := outgoingCommit(t, env, cache)

	// Ending the turn rewrites the checkpoint after the user looked at it.
	session.TranscriptBuilder.AddAssistantMessage("Also tuned the eviction policy.")
	require.NoError(t, session.TranscriptBuilder.WriteToFile(transcriptPath))
	require.NoError(t, env.SimulateStop(session.ID, transcriptPath))
	require.NotEqual(t, seen, outgoingCommit(t, env, cache), "the stop hook must move the checkpoint for this test to mean anything")

	result, err := runCheckpointPushJSON(t, env, false, cache+"@"+seen)
	require.Error(t, err, "a checkpoint that did not upload is a failed push")
	require.Equal(t, "changed", result.status(t, cache))
	require.Empty(t, remoteCheckpointIDs(t, bareDir), "a version nobody looked at must not upload")
	outgoing, _ := listOutgoing(t, env)
	require.Equal(t, []string{cache}, outgoing.ids())
}

func TestCheckpointPush_RewriteAfterPushWaitsAgain(t *testing.T) {
	t.Parallel()
	env := newOutgoingEnv(t)
	bareDir := env.SetupBareRemote()
	setCheckpointPushPolicy(t, env, "manual")

	session := env.NewSession()
	transcriptPath := session.CreateTranscript("Add cache layer", []FileChange{{Path: "cache.go", Content: "package cache"}})
	require.NoError(t, env.SimulateUserPromptSubmitWithPromptAndTranscriptPath(session.ID, "Add cache layer", transcriptPath))
	env.WriteFile("cache.go", "package cache")
	env.GitAdd("cache.go")
	env.GitCommitWithHooks("Add cache layer", "cache.go")
	cache := env.LatestCheckpointID()

	_, err := runCheckpointPushJSON(t, env, false, cache+"@"+outgoingCommit(t, env, cache))
	require.NoError(t, err)
	pushed := env.RemoteCheckpointState(bareDir)

	session.TranscriptBuilder.AddAssistantMessage("Also tuned the eviction policy.")
	require.NoError(t, session.TranscriptBuilder.WriteToFile(transcriptPath))
	require.NoError(t, env.SimulateStop(session.ID, transcriptPath))

	outgoing, _ := listOutgoing(t, env)
	require.Equal(t, []string{cache}, outgoing.ids(), "the rewrite waits for the next checkpoint push")
	gitPushWithHooksIn(t, env, env.RepoDir)
	require.Equal(t, pushed, env.RemoteCheckpointState(bareDir), "git push still sends nothing under manual")

	_, err = runCheckpointPushJSON(t, env, false, cache+"@"+outgoingCommit(t, env, cache))
	require.NoError(t, err)
	require.NotEqual(t, pushed, env.RemoteCheckpointState(bareDir), "checkpoint push sends the rewrite")
	outgoing, _ = listOutgoing(t, env)
	require.Empty(t, outgoing.Checkpoints)
}

// Broad uploads need a person at a terminal; without one, a caller names the
// exact version it means to send.
func TestCheckpointPush_WithoutTerminalNeedsExactVersions(t *testing.T) {
	t.Parallel()
	env := newOutgoingEnv(t)
	bareDir := env.SetupBareRemote()
	setCheckpointPushPolicy(t, env, "manual")
	work := createCheckpointedCommit(t, env, "Some work", "work.go", "package work", "Some work")

	for _, args := range [][]string{{"--all"}, {work}} {
		_, stderr, err := runCheckpointPush(t, env, false, args...)
		require.Error(t, err, "checkpoint push %v without a terminal must be refused", args)
		require.Contains(t, stderr, "@", "the refusal shows the <id>@<commit> form: %s", stderr)
	}
	require.Empty(t, remoteCheckpointIDs(t, bareDir))

	_, err := runCheckpointPushJSON(t, env, false, work+"@"+outgoingCommit(t, env, work))
	require.NoError(t, err)
	require.Equal(t, []string{work}, remoteCheckpointIDs(t, bareDir))
}

func TestCheckpointPush_AllFromTerminal(t *testing.T) {
	t.Parallel()
	env := newOutgoingEnv(t)
	bareDir := env.SetupBareRemote()
	setCheckpointPushPolicy(t, env, "manual")
	auth := createCheckpointedCommit(t, env, "Add auth module", "auth.go", "package auth", "Add auth module")
	billing := createCheckpointedCommit(t, env, "Add billing module", "billing.go", "package billing", "Add billing module")

	result, err := runCheckpointPushJSON(t, env, true, "--all")
	require.NoError(t, err)
	require.Equal(t, "uploaded", result.status(t, auth))
	require.Equal(t, "uploaded", result.status(t, billing))
	require.Equal(t, sortedIDs(auth, billing), remoteCheckpointIDs(t, bareDir))
}

func TestCheckpointPush_UnknownCheckpointIsReported(t *testing.T) {
	t.Parallel()
	env := newOutgoingEnv(t)
	env.SetupBareRemote()
	setCheckpointPushPolicy(t, env, "manual")
	createCheckpointedCommit(t, env, "Some work", "work.go", "package work", "Some work")

	missing := "a1b2c3d4e5f6"
	result, err := runCheckpointPushJSON(t, env, false, missing+"@1111111111111111111111111111111111111111")
	require.Error(t, err, "an ID with no checkpoint here is an error, so a typo fails")
	require.Equal(t, "not_found", result.status(t, missing))
}

// Naming a checkpoint that was already uploaded asks for nothing that has not
// happened, so a rerun succeeds.
func TestCheckpointPush_AlreadyUploadedSucceeds(t *testing.T) {
	t.Parallel()
	env := newOutgoingEnv(t)
	env.SetupBareRemote()
	setCheckpointPushPolicy(t, env, "manual")
	work := createCheckpointedCommit(t, env, "Some work", "work.go", "package work", "Some work")
	pinned := work + "@" + outgoingCommit(t, env, work)
	_, err := runCheckpointPushJSON(t, env, false, pinned)
	require.NoError(t, err)

	result, err := runCheckpointPushJSON(t, env, false, pinned)
	require.NoError(t, err, "a rerun after the upload is not a failure")
	require.Equal(t, "not_waiting", result.status(t, work))
}

// A bare push only lists, which is a text view. With --json it is refused,
// as JSON, and points at the listing that has a JSON form.
func TestCheckpointPush_BareJSONIsRefusedAsJSON(t *testing.T) {
	t.Parallel()
	env := newOutgoingEnv(t)
	env.SetupBareRemote()
	setCheckpointPushPolicy(t, env, "manual")
	createCheckpointedCommit(t, env, "Some work", "work.go", "package work", "Some work")

	stdout, _, err := runCheckpointPush(t, env, false, "--json")

	require.Error(t, err)
	var parsed struct {
		Error string `json:"error"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &parsed), "stdout must be JSON: %q", stdout)
	require.Contains(t, parsed.Error, "entire checkpoint list --outgoing --json")
}

// Callers that pass --json read stdout as JSON, failures included.
func TestCheckpointPush_JSONRefusalIsJSON(t *testing.T) {
	t.Parallel()
	env := newOutgoingEnv(t)
	env.SetupBareRemote()
	setCheckpointPushPolicy(t, env, "manual")
	createCheckpointedCommit(t, env, "Some work", "work.go", "package work", "Some work")

	stdout, _, err := runCheckpointPush(t, env, false, "--all", "--json")

	require.Error(t, err)
	var parsed struct {
		Error string `json:"error"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &parsed), "stdout must be JSON: %q", stdout)
	require.NotEmpty(t, parsed.Error)
}

// The zero hash is how the strategy spells "not pinned", so accepting it as a
// pin would let a caller without a terminal send a version it never saw.
func TestCheckpointPush_ZeroCommitIsNotAPin(t *testing.T) {
	t.Parallel()
	env := newOutgoingEnv(t)
	bareDir := env.SetupBareRemote()
	setCheckpointPushPolicy(t, env, "manual")
	work := createCheckpointedCommit(t, env, "Some work", "work.go", "package work", "Some work")

	_, _, err := runCheckpointPush(t, env, false, work+"@0000000000000000000000000000000000000000")
	require.Error(t, err)
	require.Empty(t, remoteCheckpointIDs(t, bareDir))
	outgoing, _ := listOutgoing(t, env)
	require.Equal(t, []string{work}, outgoing.ids())
}

func TestCheckpointPush_ConflictingPinsAreRefused(t *testing.T) {
	t.Parallel()
	env := newOutgoingEnv(t)
	bareDir := env.SetupBareRemote()
	setCheckpointPushPolicy(t, env, "manual")
	work := createCheckpointedCommit(t, env, "Some work", "work.go", "package work", "Some work")
	seen := outgoingCommit(t, env, work)
	other := strings.TrimSpace(testutil.RunGit(t, env.RepoDir, "rev-parse", "HEAD"))
	require.NotEqual(t, seen, other)

	_, _, err := runCheckpointPush(t, env, false, work+"@"+other, work+"@"+seen)
	require.Error(t, err, "two different versions of one checkpoint cannot both be the one to send")
	require.Empty(t, remoteCheckpointIDs(t, bareDir))

	result, err := runCheckpointPushJSON(t, env, false, work+"@"+seen, work+"@"+seen)
	require.NoError(t, err, "naming the same version twice is not a conflict")
	require.Equal(t, "uploaded", result.status(t, work))
}

func TestCheckpointPush_AllWithNothingWaitingStillReportsJSON(t *testing.T) {
	t.Parallel()
	env := newOutgoingEnv(t)
	env.SetupBareRemote()
	setCheckpointPushPolicy(t, env, "manual")

	result, err := runCheckpointPushJSON(t, env, true, "--all")
	require.NoError(t, err)
	require.NotNil(t, result.Results, "results must be an empty array, not missing")
	require.Empty(t, result.Results)
}

// A queued ref that cannot be read is still waiting; reporting it as
// not_waiting would tell the caller there is nothing left to send.
func TestCheckpointPush_UnreadableRefIsAFailure(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root reads files regardless of their mode")
	}
	env := newOutgoingEnv(t)
	bareDir := env.SetupBareRemote()
	setCheckpointPushPolicy(t, env, "manual")
	work := createCheckpointedCommit(t, env, "Some work", "work.go", "package work", "Some work")
	seen := outgoingCommit(t, env, work)

	var ref string
	for _, row := range listPolicyOutgoing(t, env).Checkpoints {
		if row.CheckpointID == work {
			ref = row.Ref
		}
	}
	commonDir := strings.TrimSpace(testutil.RunGit(t, env.RepoDir, "rev-parse", "--path-format=absolute", "--git-common-dir"))
	refFile := filepath.Join(commonDir, filepath.FromSlash(ref))
	require.FileExists(t, refFile, "the checkpoint ref must be loose for this test to mean anything")
	require.NoError(t, os.Chmod(refFile, 0o000))
	t.Cleanup(func() { _ = os.Chmod(refFile, 0o600) }) //nolint:errcheck // best-effort restore before TempDir cleanup

	result, err := runCheckpointPushJSON(t, env, false, work+"@"+seen)
	require.Error(t, err)
	require.Equal(t, "failed", result.status(t, work))
	require.Contains(t, result.Results[0].Error, "read checkpoint ref", "the failure must come from the ref read, not an earlier step")
	require.Empty(t, remoteCheckpointIDs(t, bareDir))
}

func TestCheckpointPush_RefusedWhenPushingIsDisabled(t *testing.T) {
	t.Parallel()
	env := newOutgoingEnv(t)
	bareDir := env.SetupBareRemote()
	env.PatchSettings(map[string]any{
		"strategy_options": map[string]any{"push_sessions": false},
	})
	work := createCheckpointedCommit(t, env, "Some work", "work.go", "package work", "Some work")

	_, stderr, err := runCheckpointPush(t, env, false, work+"@"+outgoingCommit(t, env, work))
	require.Error(t, err)
	require.Contains(t, stderr, "push_sessions")
	require.Empty(t, remoteCheckpointIDs(t, bareDir))
}

func TestCheckpointPushPolicy_ReadSurfaces(t *testing.T) {
	t.Parallel()
	env := newOutgoingEnv(t)
	env.SetupBareRemote()
	setCheckpointPushPolicy(t, env, "manual")
	work := createCheckpointedCommit(t, env, "Some work", "work.go", "package work", "Some work")

	listing := listPolicyOutgoing(t, env)
	require.Equal(t, "manual", listing.Policy)
	require.Len(t, listing.Checkpoints, 1)
	row := listing.Checkpoints[0]
	require.Equal(t, work, row.CheckpointID)
	require.Equal(t, strings.TrimSpace(testutil.RunGit(t, env.RepoDir, "rev-parse", row.Ref)), row.Commit)
	require.NotEmpty(t, row.SessionIDs)

	require.Contains(t, listOutgoingText(t, env), "entire checkpoint push --all")

	var status struct {
		Policy string `json:"checkpoint_push_policy"`
	}
	require.NoError(t, json.Unmarshal([]byte(cliStdout(t, env, "status", "--json")), &status))
	require.Equal(t, "manual", status.Policy)
	require.Contains(t, cliStdout(t, env, "status"), "entire checkpoint push --all")
}

// A bare `entire checkpoint push` only lists. Listing is not a failure, and
// it must say where to find the commit an <id>@<commit> argument needs.
func TestCheckpointPush_BareListsWaitingAndSucceeds(t *testing.T) {
	t.Parallel()
	env := newOutgoingEnv(t)
	bareDir := env.SetupBareRemote()
	setCheckpointPushPolicy(t, env, "manual")
	work := createCheckpointedCommit(t, env, "Some work", "work.go", "package work", "Some work")

	for _, tty := range []bool{true, false} {
		_, stderr, err := runCheckpointPush(t, env, tty)
		require.NoError(t, err, "listing is not a failure: %s", stderr)
		require.Contains(t, stderr, work)
		require.Contains(t, stderr, "entire checkpoint list --outgoing --json")
		if tty {
			require.Contains(t, stderr, "--all")
		} else {
			require.NotContains(t, stderr, "--all", "--all needs a terminal, so it is not offered without one")
		}
	}
	require.Empty(t, remoteCheckpointIDs(t, bareDir), "a bare push uploads nothing")
}

func TestCheckpointPushPolicy_ReadSurfacesReportUnreadablePolicy(t *testing.T) {
	t.Parallel()
	env := newOutgoingEnv(t)
	env.SetupBareRemote()
	setCheckpointPushPolicy(t, env, "approved")
	createCheckpointedCommit(t, env, "Some work", "work.go", "package work", "Some work")

	listing := listPolicyOutgoing(t, env)
	require.Empty(t, listing.Policy, "an unreadable policy is not reported as either value")
	require.Contains(t, listing.PolicyError, "approved")
}

// gitPushArgs runs `git push args...` from the repository through the real
// pre-push hook and returns git's combined output and exit error.
func gitPushArgs(t *testing.T, env *TestEnv, args ...string) (string, error) {
	t.Helper()
	env.InstallRealPrePushHook()
	cmd := execx.NonInteractive(t.Context(), "git", append([]string{"push"}, args...)...)
	cmd.Dir = env.RepoDir
	cmd.Env = env.cliEnv()
	output, err := cmd.CombinedOutput()
	return string(output), err
}

func onlyOutgoingRef(t *testing.T, env *TestEnv) string {
	t.Helper()
	listing := listPolicyOutgoing(t, env)
	require.Len(t, listing.Checkpoints, 1)
	return listing.Checkpoints[0].Ref
}

func remoteHasHead(t *testing.T, env *TestEnv, bareDir string) bool {
	t.Helper()
	head := strings.TrimSpace(testutil.RunGit(t, env.RepoDir, "rev-parse", "HEAD"))
	return strings.Contains(testutil.RunGit(t, bareDir, "for-each-ref", "--format=%(objectname)", "refs/heads"), head)
}

// Under manual, checkpoints leave only through `entire checkpoint push`. A
// user's own push that carries checkpoint content is refused as a whole, since
// pre-push cannot drop single refs from it; --no-verify is the stated way out.
func TestCheckpointPushPolicy_ManualRefusesExplicitCheckpointPush(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args func(t *testing.T, env *TestEnv) []string
	}{
		{name: "refspec", args: func(t *testing.T, env *TestEnv) []string {
			t.Helper()
			ref := onlyOutgoingRef(t, env)
			return []string{"origin", "HEAD", ref + ":" + ref}
		}},
		{name: "mirror", args: func(t *testing.T, _ *TestEnv) []string {
			t.Helper()
			return []string{"--mirror", "origin"}
		}},
		{name: "by commit", args: func(t *testing.T, env *TestEnv) []string {
			t.Helper()
			tip := strings.TrimSpace(testutil.RunGit(t, env.RepoDir, "rev-parse", onlyOutgoingRef(t, env)))
			return []string{"origin", "HEAD", tip + ":refs/heads/held-copy"}
		}},
		{name: "by expression", args: func(t *testing.T, env *TestEnv) []string {
			t.Helper()
			return []string{"origin", "HEAD", onlyOutgoingRef(t, env) + "~0:refs/heads/held-copy"}
		}},
		{name: "branch at the checkpoint's commit", args: func(t *testing.T, env *TestEnv) []string {
			t.Helper()
			testutil.RunGit(t, env.RepoDir, "branch", "held-copy", onlyOutgoingRef(t, env))
			return []string{"origin", "HEAD", "held-copy"}
		}},
		{name: "tag at the checkpoint's commit", args: func(t *testing.T, env *TestEnv) []string {
			t.Helper()
			testutil.RunGit(t, env.RepoDir, "tag", "held-tag", onlyOutgoingRef(t, env))
			return []string{"origin", "HEAD", "refs/tags/held-tag"}
		}},
		{name: "annotated tag at the checkpoint's commit", args: func(t *testing.T, env *TestEnv) []string {
			t.Helper()
			testutil.RunGit(t, env.RepoDir, "tag", "-a", "held-atag", "-m", "copy", onlyOutgoingRef(t, env))
			return []string{"origin", "HEAD", "refs/tags/held-atag"}
		}},
		{name: "v1 branch", args: func(t *testing.T, env *TestEnv) []string {
			t.Helper()
			// -f: the fixture may already have a v1 branch.
			testutil.RunGit(t, env.RepoDir, "branch", "-f", "entire/checkpoints/v1", "HEAD")
			return []string{"origin", "HEAD", "entire/checkpoints/v1"}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			env := newOutgoingEnv(t)
			bareDir := env.SetupBareRemote()
			setCheckpointPushPolicy(t, env, "manual")
			createCheckpointedCommit(t, env, "Some work", "work.go", "package work", "Some work")
			args := tt.args(t, env)

			output, err := gitPushArgs(t, env, args...)

			require.Error(t, err, "the push must be refused: %s", output)
			require.Contains(t, output, "entire checkpoint push")
			require.Contains(t, output, "--no-verify")
			require.Empty(t, remoteCheckpointIDs(t, bareDir))
			require.False(t, remoteHasHead(t, env, bareDir), "git aborts the whole push, code included")

			output, err = gitPushArgs(t, env, append([]string{"--no-verify"}, args...)...)
			require.NoError(t, err, "--no-verify bypasses the refusal: %s", output)
			require.True(t, remoteHasHead(t, env, bareDir))
		})
	}
}

// push_sessions=false keeps Entire's own upload out of the way: under
// automatic it would send the ref first, and git would then reject the user's
// copy as already existing.
func TestCheckpointPushPolicy_AutomaticAllowsExplicitCheckpointPush(t *testing.T) {
	t.Parallel()
	env := newOutgoingEnv(t)
	bareDir := env.SetupBareRemote()
	work := createCheckpointedCommit(t, env, "Some work", "work.go", "package work", "Some work")
	env.PatchSettings(map[string]any{
		"strategy_options": map[string]any{"push_sessions": false},
	})
	ref := onlyOutgoingRef(t, env)

	output, err := gitPushArgs(t, env, "origin", "HEAD", ref+":"+ref)

	require.NoError(t, err, "under automatic an explicit checkpoint push is the user's call: %s", output)
	require.Equal(t, []string{work}, remoteCheckpointIDs(t, bareDir))
}

// Under automatic, pushing a checkpoint's commit under another name is the
// user's call, and an ordinary push is never held up by the commit check.
func TestCheckpointPushPolicy_AutomaticAllowsPushByCommit(t *testing.T) {
	t.Parallel()
	env := newOutgoingEnv(t)
	bareDir := env.SetupBareRemote()
	createCheckpointedCommit(t, env, "Some work", "work.go", "package work", "Some work")
	env.PatchSettings(map[string]any{
		"strategy_options": map[string]any{"push_sessions": false},
	})
	tip := strings.TrimSpace(testutil.RunGit(t, env.RepoDir, "rev-parse", onlyOutgoingRef(t, env)))

	output, err := gitPushArgs(t, env, "origin", "HEAD", tip+":refs/heads/copy")

	require.NoError(t, err, "under automatic a push by commit is the user's call: %s", output)
	require.Contains(t, testutil.RunGit(t, bareDir, "for-each-ref", "refs/heads/copy"), tip)
}

func TestCheckpointPushPolicy_UnreadablePolicyRefusesExplicitCheckpointPush(t *testing.T) {
	t.Parallel()
	env := newOutgoingEnv(t)
	bareDir := env.SetupBareRemote()
	createCheckpointedCommit(t, env, "Some work", "work.go", "package work", "Some work")
	setCheckpointPushPolicy(t, env, "approved")

	output, err := gitPushArgs(t, env, "--mirror", "origin")

	require.Error(t, err, "an unreadable policy must not read as automatic: %s", output)
	require.Contains(t, output, "checkpoint-push-policy", "the refusal names the file to fix")
	require.Empty(t, remoteCheckpointIDs(t, bareDir))
}

// Settings Entire cannot load turn its git hooks off, but a push that sends
// checkpoint refs itself must still be refused: neither the backend nor the
// policy is known, so it may be publishing what manual holds back.
func TestCheckpointPushPolicy_UnloadableSettingsRefuseExplicitCheckpointPush(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		preferences string
	}{
		{name: "malformed", preferences: `{not json`},
		{name: "wrong type", preferences: `{"trails_enabled":5}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			env := newOutgoingEnv(t)
			bareDir := env.SetupBareRemote()
			setCheckpointPushPolicy(t, env, "manual")
			createCheckpointedCommit(t, env, "Some work", "work.go", "package work", "Some work")
			writeGitCommonDirFile(t, env, settings.ClonePreferencesFile, tt.preferences)

			output, err := gitPushArgs(t, env, "--mirror", "origin")

			require.Error(t, err, "unloadable settings must not let checkpoint refs through: %s", output)
			require.Contains(t, output, "--no-verify")
			require.Empty(t, remoteCheckpointIDs(t, bareDir))

			output, err = gitPushArgs(t, env, "origin", "HEAD")
			require.NoError(t, err, "a push without checkpoint refs is not refused: %s", output)
		})
	}
}

// Disabling Entire turns its hooks off, and `"enabled": false` can come from
// the committed settings file. Neither may let a user's push send what this
// clone's manual policy holds back.
func TestCheckpointPushPolicy_DisabledRepositoryStillRefusesExplicitCheckpointPush(t *testing.T) {
	t.Parallel()
	env := newOutgoingEnv(t)
	bareDir := env.SetupBareRemote()
	setCheckpointPushPolicy(t, env, "manual")
	createCheckpointedCommit(t, env, "Some work", "work.go", "package work", "Some work")
	env.PatchSettings(map[string]any{"enabled": false})

	output, err := gitPushArgs(t, env, "--mirror", "origin")

	require.Error(t, err, "a disabled repository must not let held checkpoints through: %s", output)
	require.Contains(t, output, "--no-verify")
	require.Empty(t, remoteCheckpointIDs(t, bareDir))

	output, err = gitPushArgs(t, env, "origin", "HEAD")
	require.NoError(t, err, "a push without checkpoint refs is not refused: %s", output)
}

// gitPushArgsWithEnv is gitPushArgs with extra environment for the push.
func gitPushArgsWithEnv(t *testing.T, env *TestEnv, extraEnv []string, args ...string) (string, error) {
	t.Helper()
	env.InstallRealPrePushHook()
	cmd := execx.NonInteractive(t.Context(), "git", append([]string{"push"}, args...)...)
	cmd.Dir = env.RepoDir
	cmd.Env = append(env.cliEnv(), extraEnv...)
	output, err := cmd.CombinedOutput()
	return string(output), err
}

// The policy, the queue and the checkpoint refs belong to the clone, so what
// the checked-out branch says about Entire must not decide whether a push may
// send held checkpoints.
func TestCheckpointPushPolicy_RefusalDoesNotDependOnTheCheckedOutBranch(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		extraEnv []string
		switchTo func(t *testing.T, env *TestEnv)
	}{
		{name: "branch without .entire", switchTo: func(t *testing.T, env *TestEnv) {
			t.Helper()
			testutil.RunGit(t, env.RepoDir, "checkout", "-q", "--orphan", "before-entire")
			testutil.RunGit(t, env.RepoDir, "rm", "-rfq", ".")
			testutil.RunGit(t, env.RepoDir, "clean", "-fdxq")
			require.NoError(t, os.WriteFile(filepath.Join(env.RepoDir, "README"), []byte("old\n"), 0o600))
			testutil.RunGit(t, env.RepoDir, "add", "README")
			testutil.RunGit(t, env.RepoDir, "commit", "-qm", "before entire")
		}},
		{name: "backend reads as git-branch", extraEnv: []string{"ENTIRE_CHECKPOINTS_PRIMARY=git-branch"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			env := newOutgoingEnv(t)
			bareDir := env.SetupBareRemote()
			setCheckpointPushPolicy(t, env, "manual")
			createCheckpointedCommit(t, env, "Some work", "work.go", "package work", "Some work")
			if tt.switchTo != nil {
				tt.switchTo(t, env)
			}

			for _, args := range [][]string{{"--mirror", "origin"}, {"origin", "refs/entire/*:refs/entire/*"}} {
				output, err := gitPushArgsWithEnv(t, env, tt.extraEnv, args...)
				require.Error(t, err, "git push %v must be refused: %s", args, output)
				require.Contains(t, output, "--no-verify")
			}
			require.Empty(t, remoteCheckpointIDs(t, bareDir))
		})
	}
}

// gitPushWithUnsavedRefList pushes through a hook in the form the hook line
// falls back to when it cannot save git's ref list (no usable TMPDIR).
func gitPushWithUnsavedRefList(t *testing.T, env *TestEnv, args ...string) (string, error) {
	t.Helper()
	script := fmt.Sprintf("#!/bin/sh\n%s=0 exec %q hooks git pre-push \"$1\" < /dev/null\n", strategy.PrePushStdinRefsEnv, getTestBinary())
	require.NoError(t, os.WriteFile(filepath.Join(env.RepoDir, ".git", "hooks", "pre-push"), []byte(script), 0o755))
	cmd := execx.NonInteractive(t.Context(), "git", append([]string{"push"}, args...)...)
	cmd.Dir = env.RepoDir
	cmd.Env = env.cliEnv()
	output, err := cmd.CombinedOutput()
	return string(output), err
}

// When the hook cannot save git's ref list, the push cannot be checked. Under
// manual with checkpoints waiting that must not let a mirror push through, and
// the message must name the real cause, not an old hook.
func TestCheckpointPushPolicy_UnsavedRefListRefusesUnderManual(t *testing.T) {
	t.Parallel()
	env := newOutgoingEnv(t)
	bareDir := env.SetupBareRemote()
	setCheckpointPushPolicy(t, env, "manual")
	createCheckpointedCommit(t, env, "Some work", "work.go", "package work", "Some work")
	env.InstallRealPrePushHook()

	output, err := gitPushWithUnsavedRefList(t, env, "--mirror", "origin")

	require.Error(t, err, "an unchecked push must not send held checkpoints: %s", output)
	require.Contains(t, output, "TMPDIR")
	require.NotContains(t, output, "does not pass the refs", "this is not an outdated hook")
	require.Empty(t, remoteCheckpointIDs(t, bareDir))
}

func TestCheckpointPushPolicy_UnsavedRefListPassesUnderAutomatic(t *testing.T) {
	t.Parallel()
	env := newOutgoingEnv(t)
	env.SetupBareRemote()
	createCheckpointedCommit(t, env, "Some work", "work.go", "package work", "Some work")
	env.InstallRealPrePushHook()

	output, err := gitPushWithUnsavedRefList(t, env, "origin", "HEAD")

	require.NoError(t, err, "nothing is held under automatic: %s", output)
}

// A checkpoints block that settings.Load accepts but the backend loader
// rejects leaves the backend unknown. Checkpoint refs are refused from the
// clone's policy alone, so an unknown backend does not let them through.
func TestCheckpointPushPolicy_UnreadableBackendRefusesExplicitCheckpointPush(t *testing.T) {
	t.Parallel()
	env := newOutgoingEnv(t)
	bareDir := env.SetupBareRemote()
	setCheckpointPushPolicy(t, env, "manual")
	createCheckpointedCommit(t, env, "Some work", "work.go", "package work", "Some work")
	env.CheckpointStore = ""
	env.PatchSettings(map[string]any{
		"checkpoints": map[string]any{"primary": map[string]any{"type": ""}},
	})

	output, err := gitPushArgs(t, env, "--mirror", "origin")

	require.Error(t, err, "an unreadable backend must not let checkpoint refs through: %s", output)
	require.Empty(t, remoteCheckpointIDs(t, bareDir))
}

// Deleting a remote checkpoint ref sends no checkpoint content.
func TestCheckpointPushPolicy_ManualAllowsDeletingARemoteCheckpoint(t *testing.T) {
	t.Parallel()
	env := newOutgoingEnv(t)
	bareDir := env.SetupBareRemote()
	setCheckpointPushPolicy(t, env, "manual")
	work := createCheckpointedCommit(t, env, "Some work", "work.go", "package work", "Some work")
	ref := onlyOutgoingRef(t, env)
	_, err := runCheckpointPushJSON(t, env, false, work+"@"+outgoingCommit(t, env, work))
	require.NoError(t, err)
	require.Equal(t, []string{work}, remoteCheckpointIDs(t, bareDir))

	output, err := gitPushArgs(t, env, "origin", ":"+ref)

	require.NoError(t, err, "deleting a remote checkpoint ref is allowed under manual: %s", output)
	require.Empty(t, remoteCheckpointIDs(t, bareDir))
}

// A pre-push line written before the hook passed git's ref list cannot see what
// the user's push sends. Under manual it says so once, and only while
// checkpoints are waiting, since that is when an explicit push could leak them.
func TestCheckpointPushPolicy_OldHookWarnsOnlyWhileCheckpointsWait(t *testing.T) {
	t.Parallel()
	env := newOutgoingEnv(t)
	env.SetupBareRemote()
	setCheckpointPushPolicy(t, env, "manual")
	work := createCheckpointedCommit(t, env, "Some work", "work.go", "package work", "Some work")
	push := installOldPrePushHook(t, env)

	output := push()
	require.Equal(t, 1, strings.Count(output, oldHookWarning), "warn once while checkpoints wait:\n%s", output)

	_, err := runCheckpointPushJSON(t, env, false, work+"@"+outgoingCommit(t, env, work))
	require.NoError(t, err)
	env.WriteFile("more.go", "package more")
	env.GitAdd("more.go")
	env.GitCommit("More")
	require.NotContains(t, push(), oldHookWarning, "nothing waiting, nothing to warn about")
}

// The warning is about what the user's own push can send, so the gates on
// what Entire sends must not hide it.
func TestCheckpointPushPolicy_OldHookWarnsWhenEntireSendsNothing(t *testing.T) {
	t.Parallel()
	env := newOutgoingEnv(t)
	env.SetupBareRemote()
	setCheckpointPushPolicy(t, env, "manual")
	createCheckpointedCommit(t, env, "Some work", "work.go", "package work", "Some work")
	env.PatchSettings(map[string]any{
		"strategy_options": map[string]any{"push_sessions": false},
	})
	push := installOldPrePushHook(t, env)

	output := push()
	require.Equal(t, 1, strings.Count(output, oldHookWarning), "push_sessions=false must not hide the warning:\n%s", output)
}

// With Entire's hooks off for the branch, the clone's policy still holds
// checkpoints back, so an old hook line must still say the push is unchecked.
func TestCheckpointPushPolicy_OldHookWarnsWhenEntireIsDisabled(t *testing.T) {
	t.Parallel()
	env := newOutgoingEnv(t)
	env.SetupBareRemote()
	setCheckpointPushPolicy(t, env, "manual")
	createCheckpointedCommit(t, env, "Some work", "work.go", "package work", "Some work")
	env.PatchSettings(map[string]any{"enabled": false})
	push := installOldPrePushHook(t, env)

	output := push()
	require.Equal(t, 1, strings.Count(output, oldHookWarning), "a disabled repository must not hide the warning:\n%s", output)
}

const oldHookWarning = "does not pass the refs being pushed"

// installOldPrePushHook writes a pre-push line from before the hook passed
// git's ref list, and returns a push of HEAD through it.
func installOldPrePushHook(t *testing.T, env *TestEnv) func() string {
	t.Helper()
	hookPath := filepath.Join(env.RepoDir, ".git", "hooks", "pre-push")
	require.NoError(t, os.MkdirAll(filepath.Dir(hookPath), 0o755))
	oldHook := "#!/bin/sh\nexec \"" + getTestBinary() + "\" hooks git pre-push \"$1\"\n"
	require.NoError(t, os.WriteFile(hookPath, []byte(oldHook), 0o755))
	return func() string {
		t.Helper()
		cmd := execx.NonInteractive(t.Context(), "git", "push", "origin", "HEAD")
		cmd.Dir = env.RepoDir
		cmd.Env = env.cliEnv()
		output, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", output)
		return string(output)
	}
}

func TestConfigure_SetsCheckpointPushPolicy(t *testing.T) {
	t.Parallel()
	env := newOutgoingEnv(t)
	env.SetupBareRemote()

	env.RunCLI("configure", "--checkpoint-push-policy", "manual")
	require.Equal(t, "manual", listPolicyOutgoing(t, env).Policy)

	env.RunCLI("configure", "--checkpoint-push-policy", "automatic")
	require.Equal(t, "automatic", listPolicyOutgoing(t, env).Policy)

	_, err := env.RunCLIWithError("configure", "--checkpoint-push-policy", "approved")
	require.Error(t, err)
	require.Equal(t, "automatic", listPolicyOutgoing(t, env).Policy, "a rejected value changes nothing")
}

// git-branch has no per-checkpoint refs for `entire checkpoint push` to send,
// so manual there would hold nothing back and leave no way to upload.
func TestConfigure_ManualPolicyNeedsGitRefs(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	env.SetupBareRemote()

	_, err := env.RunCLIWithError("configure", "--checkpoint-push-policy", "manual")
	require.Error(t, err)
	require.NoFileExists(t, gitCommonDirFile(t, env, settings.CheckpointPushPolicyFile), "a refused policy saves nothing")

	env.RunCLI("configure", "--checkpoint-push-policy", "automatic")

	env.RunCLI("configure", "--checkpoint-backend", "git-refs", "--checkpoint-push-policy", "manual")
	require.Equal(t, "manual", listPolicyOutgoing(t, env).Policy)
}

// The policy is saved only once the other flags have applied: a failed
// backend switch must not leave manual on a git-branch clone.
func TestConfigure_PolicyNotSavedWhenBackendSwitchFails(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root writes into read-only directories")
	}
	env := NewFeatureBranchEnv(t)
	env.SetupBareRemote()
	entireDir := filepath.Join(env.RepoDir, ".entire")
	require.NoError(t, os.Chmod(entireDir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(entireDir, 0o750) }) //nolint:errcheck // best-effort restore before TempDir cleanup

	_, err := env.RunCLIWithError("configure", "--checkpoint-backend", "git-refs", "--checkpoint-push-policy", "manual")
	require.Error(t, err, "the backend save must fail for this test to mean anything")
	require.NoFileExists(t, gitCommonDirFile(t, env, settings.CheckpointPushPolicyFile), "manual must not be saved when the backend switch failed")
}

// The policy applies to git-refs only, so on git-branch status must not report
// it, and a policy file it cannot read is not a warning there.
func TestCheckpointPushPolicy_StatusIgnoresPolicyOnGitBranch(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	env.SetupBareRemote()
	setCheckpointPushPolicy(t, env, "approved")
	createCheckpointedCommit(t, env, "Some work", "work.go", "package work", "Some work")

	var status struct {
		Policy      string `json:"checkpoint_push_policy"`
		PolicyError string `json:"checkpoint_push_policy_error"`
	}
	require.NoError(t, json.Unmarshal([]byte(cliStdout(t, env, "status", "--json")), &status))
	require.Empty(t, status.Policy)
	require.Empty(t, status.PolicyError)
	require.NotContains(t, cliStdout(t, env, "status"), "push policy")
}

// A checkpoints block that cannot be read is a config error to fix, not a
// sign the repository uses git-branch.
func TestCheckpointPush_UnreadableBackendNamesTheConfigError(t *testing.T) {
	t.Parallel()
	env := newOutgoingEnv(t)
	env.SetupBareRemote()
	env.CheckpointStore = ""
	env.PatchSettings(map[string]any{
		"checkpoints": map[string]any{"primary": map[string]any{"type": ""}},
	})

	_, stderr, err := runCheckpointPush(t, env, true, "--all")

	require.Error(t, err)
	require.NotContains(t, stderr, "git-branch")
	require.Contains(t, stderr, "checkpoints.primary.type")
}
