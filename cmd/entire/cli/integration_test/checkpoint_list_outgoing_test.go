//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/execx"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/require"
)

// outgoingListing decodes `entire checkpoint list --outgoing --json`.
type outgoingListing struct {
	SyncRemote        string        `json:"checkpoint_sync_remote"`
	SyncRemoteSource  string        `json:"checkpoint_sync_remote_source"`
	SyncError         string        `json:"checkpoint_sync_error"`
	PushDisabled      bool          `json:"checkpoint_push_disabled"`
	StaleQueueEntries int           `json:"stale_queue_entries"`
	Checkpoints       []outgoingRow `json:"checkpoints"`
}

type outgoingRow struct {
	CheckpointID string   `json:"checkpoint_id"`
	Ref          string   `json:"ref"`
	CreatedAt    string   `json:"created_at"`
	Agent        string   `json:"agent"`
	SessionCount int      `json:"session_count"`
	Prompt       string   `json:"prompt"`
	Summary      string   `json:"summary"`
	FilesTouched []string `json:"files_touched"`
	Error        string   `json:"error"`
}

func (l outgoingListing) ids() []string {
	ids := make([]string, 0, len(l.Checkpoints))
	for _, row := range l.Checkpoints {
		if row.CheckpointID != "" {
			ids = append(ids, row.CheckpointID)
		}
	}
	slices.Sort(ids)
	return ids
}

func (l outgoingListing) row(t *testing.T, checkpointID string) outgoingRow {
	t.Helper()
	for _, row := range l.Checkpoints {
		if row.CheckpointID == checkpointID {
			return row
		}
	}
	t.Fatalf("checkpoint %s not in outgoing listing: %+v", checkpointID, l.Checkpoints)
	return outgoingRow{}
}

// cliStdout runs the CLI and returns stdout alone, so a warning on stderr can
// never corrupt the JSON being decoded.
func cliStdout(t *testing.T, env *TestEnv, args ...string) string {
	t.Helper()
	cmd := execx.NonInteractive(t.Context(), getTestBinary(), args...)
	cmd.Dir = env.RepoDir
	cmd.Env = env.cliEnv()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("entire %v failed: %v\nstdout: %s\nstderr: %s", args, err, stdout.String(), stderr.String())
	}
	return stdout.String()
}

// listOutgoing also checks that `entire status` counts what the listing lists.
func listOutgoing(t *testing.T, env *TestEnv) (outgoingListing, string) {
	t.Helper()
	raw := cliStdout(t, env, "checkpoint", "list", "--outgoing", "--json")
	var listing outgoingListing
	require.NoError(t, json.Unmarshal([]byte(raw), &listing), "outgoing --json must be valid JSON, got: %s", raw)
	require.Equal(t, len(listing.Checkpoints), statusSyncJSONOutput(t, env).UnpushedCheckpoints,
		"entire status must count what checkpoint list --outgoing lists")
	return listing, raw
}

func listOutgoingText(t *testing.T, env *TestEnv) string {
	t.Helper()
	return cliStdout(t, env, "checkpoint", "list", "--outgoing", "--no-pager")
}

// newOutgoingEnv is a feature-branch repo on the git-refs backend, the only
// one --outgoing supports.
func newOutgoingEnv(t *testing.T) *TestEnv {
	t.Helper()
	env := NewFeatureBranchEnv(t)
	env.CheckpointStore = StoreGitRefs
	return env
}

// remoteCheckpointIDs lists the checkpoint refs the bare remote holds, sorted.
func remoteCheckpointIDs(t *testing.T, bareDir string) []string {
	t.Helper()
	var ids []string
	out := testutil.RunGit(t, bareDir, "for-each-ref", "--format=%(refname)", checkpointRefPrefix)
	for _, ref := range strings.Fields(out) {
		if cid, ok := checkpoint.ParseRef(plumbing.ReferenceName(ref)); ok {
			ids = append(ids, cid.String())
		}
	}
	slices.Sort(ids)
	return ids
}

func sortedIDs(ids ...string) []string {
	sorted := slices.Clone(ids)
	slices.Sort(sorted)
	return sorted
}

func TestCheckpointListOutgoing_EmptyBeforeAnyCheckpoint(t *testing.T) {
	t.Parallel()
	env := newOutgoingEnv(t)
	env.SetupBareRemote()

	listing, raw := listOutgoing(t, env)
	require.Equal(t, "origin", listing.SyncRemote)
	require.Contains(t, raw, `"checkpoints": []`, "an empty listing must be an empty array, not null")

	require.Contains(t, listOutgoingText(t, env), "No checkpoints are queued for origin.")
}

// TestCheckpointListOutgoing_MatchesWhatPushSends is the command's contract:
// the checkpoints it lists are exactly the ones a real `git push` then lands
// on the remote, and nothing is listed once they have.
func TestCheckpointListOutgoing_MatchesWhatPushSends(t *testing.T) {
	t.Parallel()
	env := newOutgoingEnv(t)
	bareDir := env.SetupBareRemote()

	auth := createCheckpointedCommit(t, env, "Add auth module", "auth.go", "package auth", "Add auth module")
	billing := createCheckpointedCommit(t, env, "Add billing module", "billing.go", "package billing", "Add billing module")

	listing, _ := listOutgoing(t, env)
	require.Equal(t, sortedIDs(auth, billing), listing.ids())
	row := listing.row(t, auth)
	require.Equal(t, string(agent.AgentTypeClaudeCode), row.Agent)
	require.Equal(t, "Add auth module", row.Prompt)
	require.Contains(t, row.FilesTouched, "auth.go")
	require.NotEmpty(t, row.CreatedAt)
	require.Empty(t, row.Error)

	text := listOutgoingText(t, env)
	for _, want := range []string{auth, billing, "Add auth module", "auth.go", "with your next 'git push origin'", "entire checkpoint explain"} {
		require.Contains(t, text, want)
	}

	env.GitPushWithHooks("origin", "HEAD")

	require.Equal(t, listing.ids(), remoteCheckpointIDs(t, bareDir), "the push must send exactly the listed checkpoints")
	after, _ := listOutgoing(t, env)
	require.Empty(t, after.Checkpoints, "nothing is outgoing once the push landed")
}

// TestCheckpointListOutgoing_RewriteAfterPushGoesOutAgain commits in the
// middle of a turn and pushes, then ends the turn: the stop hook rewrites that
// checkpoint, the queue takes it back, and the next push sends it again.
func TestCheckpointListOutgoing_RewriteAfterPushGoesOutAgain(t *testing.T) {
	t.Parallel()
	env := newOutgoingEnv(t)
	bareDir := env.SetupBareRemote()

	session := env.NewSession()
	transcriptPath := session.CreateTranscript("Add cache layer", []FileChange{{Path: "cache.go", Content: "package cache"}})
	require.NoError(t, env.SimulateUserPromptSubmitWithPromptAndTranscriptPath(session.ID, "Add cache layer", transcriptPath))
	env.WriteFile("cache.go", "package cache")
	env.GitAdd("cache.go")
	env.GitCommitWithHooks("Add cache layer", "cache.go")
	cache := env.LatestCheckpointID()
	require.NotEmpty(t, cache, "the mid-turn commit should condense a checkpoint")

	env.GitPushWithHooks("origin", "HEAD")
	pushed := env.RemoteCheckpointState(bareDir)
	listing, _ := listOutgoing(t, env)
	require.Empty(t, listing.Checkpoints)

	session.TranscriptBuilder.AddAssistantMessage("Also tuned the eviction policy.")
	require.NoError(t, session.TranscriptBuilder.WriteToFile(transcriptPath))
	require.NoError(t, env.SimulateStop(session.ID, transcriptPath))

	listing, _ = listOutgoing(t, env)
	require.Equal(t, []string{cache}, listing.ids(), "the rewritten checkpoint goes out again")

	env.GitPushWithHooks("origin", "HEAD")
	require.NotEqual(t, pushed, env.RemoteCheckpointState(bareDir), "the push must send the rewrite")
	listing, _ = listOutgoing(t, env)
	require.Empty(t, listing.Checkpoints)
}

func TestCheckpointListOutgoing_PushDisabled(t *testing.T) {
	t.Parallel()
	env := newOutgoingEnv(t)
	bareDir := env.SetupBareRemote()
	env.PatchSettings(map[string]any{
		"strategy_options": map[string]any{"push_sessions": false},
	})

	work := createCheckpointedCommit(t, env, "Some work", "work.go", "package work", "Some work")

	listing, _ := listOutgoing(t, env)
	require.True(t, listing.PushDisabled)
	require.Equal(t, []string{work}, listing.ids(), "disabled pushing still lists the queue")
	text := listOutgoingText(t, env)
	require.Contains(t, text, "push_sessions=false")
	require.Contains(t, text, "sends none of these")
	require.Contains(t, text, "1 checkpoint is queued for origin", "the queue says what is waiting, not what origin holds")

	env.GitPushWithHooks("origin", "HEAD")
	require.Empty(t, remoteCheckpointIDs(t, bareDir), "with pushing disabled, git push sends nothing")
	listing, _ = listOutgoing(t, env)
	require.Equal(t, []string{work}, listing.ids())
}

// TestCheckpointListOutgoing_FollowsElectedRemote elects "publish" over origin:
// a push to origin carries no checkpoints, so they stay listed until a push to
// publish sends them.
func TestCheckpointListOutgoing_FollowsElectedRemote(t *testing.T) {
	t.Parallel()
	env := newOutgoingEnv(t)
	bareOrigin := env.SetupBareRemote()
	barePublish := env.SetupNamedBareRemote("publish")
	env.PatchSettings(map[string]any{
		"strategy_options": map[string]any{"checkpoint_push_remote": "publish"},
	})

	router := createCheckpointedCommit(t, env, "Add router module", "router.go", "package router", "Add router module")

	listing, _ := listOutgoing(t, env)
	require.Equal(t, "publish", listing.SyncRemote)
	require.Equal(t, "config", listing.SyncRemoteSource)
	require.Equal(t, []string{router}, listing.ids())
	require.Contains(t, listOutgoingText(t, env), "with your next 'git push publish'")

	env.GitPushWithHooks("origin", "HEAD")
	require.Empty(t, remoteCheckpointIDs(t, bareOrigin))
	listing, _ = listOutgoing(t, env)
	require.Equal(t, []string{router}, listing.ids(), "a push to a non-elected remote sends nothing")

	env.GitPushWithHooks("publish", "HEAD")
	require.Equal(t, []string{router}, remoteCheckpointIDs(t, barePublish))
	listing, _ = listOutgoing(t, env)
	require.Empty(t, listing.Checkpoints)
}

// addGitHubOrigin adds an origin that could only be reached over the network,
// and records it as the expected git config.
func addGitHubOrigin(t *testing.T, env *TestEnv) {
	t.Helper()
	testutil.RunGit(t, env.RepoDir, "remote", "add", "origin", "git@github.com:acme/widgets.git")
	env.setGitConfigBaseline()
}

// trapNetwork points every git transport at a script that records being run,
// and proves the trap fires before the test relies on it staying silent.
func trapNetwork(t *testing.T, env *TestEnv) string {
	t.Helper()
	dir := t.TempDir()
	marker := filepath.Join(dir, "dialed")
	script := filepath.Join(dir, "ssh")
	require.NoError(t, os.WriteFile(script, []byte(fmt.Sprintf("#!/bin/sh\ntouch %q\nexit 1\n", marker)), 0o755))
	env.ExtraEnv = append(env.ExtraEnv,
		"GIT_SSH_COMMAND="+script,
		// Send HTTPS forms of GitHub URLs through the same trap.
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=url.git@github.com:.insteadOf",
		"GIT_CONFIG_VALUE_0=https://github.com/",
	)

	probe := exec.CommandContext(t.Context(), "git", "ls-remote", "origin")
	probe.Dir = env.RepoDir
	probe.Env = env.cliEnv()
	_ = probe.Run() //nolint:errcheck // the trap makes it fail; only the marker matters
	require.FileExists(t, marker, "the network trap must fire for a real remote call")
	require.NoError(t, os.Remove(marker))
	return marker
}

func TestCheckpointListOutgoing_NoNetwork(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("the network trap is a POSIX shell script")
	}

	env := newOutgoingEnv(t)
	addGitHubOrigin(t, env)
	work := createCheckpointedCommit(t, env, "Some work", "work.go", "package work", "Some work")
	marker := trapNetwork(t, env)

	listing, _ := listOutgoing(t, env)
	require.Equal(t, []string{work}, listing.ids())
	listOutgoingText(t, env)
	require.NoFileExists(t, marker, "checkpoint list --outgoing must not contact a remote")
}

func TestCheckpointListOutgoing_DedicatedCheckpointRemote(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("the network trap is a POSIX shell script")
	}

	env := newOutgoingEnv(t)
	addGitHubOrigin(t, env)
	local, err := jsonutil.MarshalIndentWithNewline(map[string]any{
		"strategy_options": map[string]any{
			"checkpoint_remote": map[string]any{"provider": "github", "repo": "acme/widgets-checkpoints"},
		},
	}, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(env.RepoDir, ".entire", "settings.local.json"), local, 0o644))

	work := createCheckpointedCommit(t, env, "Some work", "work.go", "package work", "Some work")
	marker := trapNetwork(t, env)

	listing, _ := listOutgoing(t, env)
	require.Equal(t, "dedicated", listing.SyncRemoteSource)
	require.Equal(t, "acme/widgets-checkpoints", listing.SyncRemote)
	require.Equal(t, []string{work}, listing.ids())
	require.Contains(t, listOutgoingText(t, env), "the dedicated checkpoint remote (acme/widgets-checkpoints)")
	require.NoFileExists(t, marker, "checkpoint list --outgoing must not contact a remote")
}

// TestCheckpointListOutgoing_GitRefsQueueEntries: a queued ref deleted locally
// is pruned by the push, so it is counted, not listed; a ref that exists but
// does not name a checkpoint is still pushed, so it is listed.
func TestCheckpointListOutgoing_GitRefsQueueEntries(t *testing.T) {
	t.Parallel()
	env := newOutgoingEnv(t)
	bareDir := env.SetupBareRemote()

	work := createCheckpointedCommit(t, env, "Some work", "work.go", "package work", "Some work")
	tip := strings.TrimSpace(testutil.RunGit(t, env.RepoDir, "rev-parse", checkpointRefName(work)))
	const stale = "refs/entire/checkpoints/ST/01JZZZZZZZZZZZZZZZZZZZZZST"
	const foreign = "refs/entire/checkpoints/zz/01JAAAAAAAAAAAAAAAAAAAAAAB"
	testutil.RunGit(t, env.RepoDir, "update-ref", foreign, tip)

	queuePath := filepath.Join(env.RepoDir, ".git", "entire-checkpoint-push-queue.jsonl")
	queue, err := os.OpenFile(queuePath, os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	_, err = fmt.Fprintf(queue, "{\"ref\":%q}\n{\"ref\":%q}\n", stale, foreign)
	require.NoError(t, err)
	require.NoError(t, queue.Close())

	listing, _ := listOutgoing(t, env)
	require.Equal(t, 1, listing.StaleQueueEntries)
	require.Equal(t, []string{work}, listing.ids())
	var foreignRow *outgoingRow
	for i := range listing.Checkpoints {
		if listing.Checkpoints[i].Ref == foreign {
			foreignRow = &listing.Checkpoints[i]
		}
	}
	require.NotNil(t, foreignRow, "a pushable ref that names no checkpoint must still be listed")
	require.Empty(t, foreignRow.CheckpointID)
	require.NotEmpty(t, foreignRow.Error)
	require.NotContains(t, listOutgoingText(t, env), "01JZZZZZZZZZZZZZZZZZZZZZST", "a stale entry is not outgoing")

	env.GitPushWithHooks("origin", "HEAD")
	require.True(t, refExists(t, bareDir, foreign), "the push sends the listed foreign ref")
	listing, _ = listOutgoing(t, env)
	require.Empty(t, listing.Checkpoints)
	require.Zero(t, listing.StaleQueueEntries, "the push prunes stale entries")
}

// TestCheckpointListOutgoing_RefusesGitBranch: a git-branch push sends
// entire/checkpoints/v1 as one branch, so there is no per-checkpoint answer to
// give; the command says so instead of estimating one.
func TestCheckpointListOutgoing_RefusesGitBranch(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	env.CheckpointStore = StoreGitBranch
	env.SetupBareRemote()
	createCheckpointedCommit(t, env, "Some work", "work.go", "package work", "Some work")

	for _, args := range [][]string{
		{"checkpoint", "list", "--outgoing"},
		{"checkpoint", "list", "--outgoing", "--json"},
	} {
		out, err := env.RunCLIWithError(args...)
		require.Error(t, err, "entire %v must fail on the git-branch backend", args)
		require.Contains(t, out, "git-refs", "entire %v must name the backend it needs", args)
	}
}
