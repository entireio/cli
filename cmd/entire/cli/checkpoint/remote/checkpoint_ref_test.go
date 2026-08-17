package remote

import (
	"context"
	"os/exec"
	"testing"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// checkpointRefFixture creates a work repo whose origin is a local bare repo
// holding (or not holding) a checkpoint ref, and chdirs into the work repo so
// fetch-target resolution finds origin.
func checkpointRefFixture(t *testing.T, withRef bool) (workDir string, ref plumbing.ReferenceName) {
	t.Helper()
	bareDir := t.TempDir()
	out, err := exec.CommandContext(t.Context(), "git", "init", "--bare", bareDir).CombinedOutput()
	require.NoError(t, err, "git init --bare: %s", out)

	workDir = t.TempDir()
	testutil.InitRepo(t, workDir)
	testutil.WriteFile(t, workDir, "f.txt", "content")
	testutil.GitAdd(t, workDir, "f.txt")
	testutil.GitCommit(t, workDir, "init")
	out, err = exec.CommandContext(t.Context(), "git", "-C", workDir, "remote", "add", "origin", bareDir).CombinedOutput()
	require.NoError(t, err, "git remote add: %s", out)

	ref = plumbing.ReferenceName("refs/entire/checkpoints/Z9/01KVBJCWYA4YW6J5M9GP655HZ9")
	if withRef {
		out, err = exec.CommandContext(t.Context(), "git", "-C", workDir, "push", "--quiet", "origin", "HEAD:"+ref.String()).CombinedOutput()
		require.NoError(t, err, "git push checkpoint ref: %s", out)
	}

	t.Chdir(workDir)
	return workDir, ref
}

// TestFetchCheckpointRef_RemoteMissingRefIsAbsence: a remote that does not
// have the requested checkpoint ref is ABSENCE, not a transport failure — the
// error must wrap plumbing.ErrReferenceNotFound so store probes (reads and
// backfill writes) classify it as "checkpoint not found" and, under kind
// routing, may legitimately fall through to another backend. Before this
// distinction, git fetch of a missing refspec failed like a network error,
// which made wiring a fetcher into write paths unsafe.
func TestFetchCheckpointRef_RemoteMissingRefIsAbsence(t *testing.T) {
	t.Run("origin is the checkpoint host", func(t *testing.T) {
		_, ref := checkpointRefFixture(t, false)

		err := FetchCheckpointRef(context.Background(), ref)
		require.Error(t, err)
		require.ErrorIs(t, err, plumbing.ErrReferenceNotFound,
			"a ref the checkpoint host does not have must classify as absence")
	})

	t.Run("origin is not the elected checkpoint host", func(t *testing.T) {
		workDir, ref := checkpointRefFixture(t, false)
		bareUpstream := t.TempDir()
		out, err := exec.CommandContext(t.Context(), "git", "init", "--bare", bareUpstream).CombinedOutput()
		require.NoError(t, err, "git init --bare: %s", out)
		out, err = exec.CommandContext(t.Context(), "git", "-C", workDir, "remote", "add", "upstream", bareUpstream).CombinedOutput()
		require.NoError(t, err, "git remote add upstream: %s", out)
		testutil.WriteCheckpointPushRemoteSetting(t, workDir, "upstream")

		err = HookCheckpointRefFetcher()(context.Background(), ref)
		require.Error(t, err)
		require.NotErrorIs(t, err, plumbing.ErrReferenceNotFound,
			"a miss on non-elected origin must not certify global absence")
	})
}

// TestFetchCheckpointRef_PresentRefFetches: the ref exists on the remote but
// not locally; the fetch must create the local ref of the same name.
func TestFetchCheckpointRef_PresentRefFetches(t *testing.T) {
	workDir, ref := checkpointRefFixture(t, true)

	require.NoError(t, FetchCheckpointRef(context.Background(), ref))

	out, err := exec.CommandContext(t.Context(), "git", "-C", workDir, "show-ref", "--verify", ref.String()).CombinedOutput()
	require.NoError(t, err, "fetched ref must exist locally: %s", out)
}

// TestFetchCheckpointRef_FallbackTargetNeverClassifiesAbsence: when a
// checkpoint_remote is configured but cannot be resolved (unknown provider +
// an origin whose protocol can't be mapped), the probe runs against an origin
// FALLBACK that never hosts the configured checkpoint refs. Emptiness there
// must be a failure, not absence — absence would silently drop backfills for
// checkpoints that exist on the real checkpoint remote.
func TestFetchCheckpointRef_FallbackTargetNeverClassifiesAbsence(t *testing.T) {
	workDir, ref := checkpointRefFixture(t, false)
	testutil.WriteFile(t, workDir, ".entire/settings.json",
		`{"enabled": true, "strategy_options": {"checkpoint_remote": {"provider": "bogusforge", "repo": "acme/checkpoints"}}}`)

	err := FetchCheckpointRef(context.Background(), ref)
	require.Error(t, err)
	require.NotErrorIs(t, err, plumbing.ErrReferenceNotFound,
		"emptiness on a non-authoritative fallback target must not classify as absence")
}

// TestFetchCheckpointRef_UnreachableRemoteIsFailure: a transport-level
// failure (unreachable remote) must NOT classify as absence.
func TestFetchCheckpointRef_UnreachableRemoteIsFailure(t *testing.T) {
	workDir, ref := checkpointRefFixture(t, false)
	out, err := exec.CommandContext(t.Context(), "git", "-C", workDir, "remote", "set-url", "origin", workDir+"/nonexistent-remote").CombinedOutput()
	require.NoError(t, err, "%s", out)

	err = FetchCheckpointRef(context.Background(), ref)
	require.Error(t, err)
	require.NotErrorIs(t, err, plumbing.ErrReferenceNotFound,
		"a transport failure must stay distinguishable from absence")
	require.Contains(t, err.Error(), workDir+"/nonexistent-remote")
	require.NotContains(t, err.Error(), ":///", "local paths must not be mangled as URLs")
}

func TestFetchCheckpointRefFrom_FailureSemantics(t *testing.T) {
	t.Run("later transport failure prevents false absence", func(t *testing.T) {
		workDir, ref := checkpointRefFixture(t, false)

		bareUpstream := t.TempDir()
		out, err := exec.CommandContext(t.Context(), "git", "init", "--bare", bareUpstream).CombinedOutput()
		require.NoError(t, err, "git init --bare: %s", out)
		out, err = exec.CommandContext(t.Context(), "git", "-C", workDir, "remote", "add", "upstream", bareUpstream).CombinedOutput()
		require.NoError(t, err, "git remote add upstream: %s", out)
		out, err = exec.CommandContext(t.Context(), "git", "-C", workDir, "remote", "set-url", "origin", workDir+"/nonexistent-remote").CombinedOutput()
		require.NoError(t, err, "git remote set-url origin: %s", out)

		err = FetchCheckpointRefFrom(context.Background(), ref, []string{"upstream", "origin"}, nil)
		require.Error(t, err)
		require.NotErrorIs(t, err, plumbing.ErrReferenceNotFound,
			"absence is not proven when any read candidate fails")
	})

	for _, tc := range []struct {
		name              string
		addBrokenUpstream bool
	}{
		{name: "elected transport failure cannot seed from legacy origin", addBrokenUpstream: true},
		{name: "elected resolution failure cannot substitute legacy origin"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workDir, ref := checkpointRefFixture(t, true)
			if tc.addBrokenUpstream {
				out, err := exec.CommandContext(t.Context(), "git", "-C", workDir, "remote", "add", "upstream", workDir+"/nonexistent-remote").CombinedOutput()
				require.NoError(t, err, "git remote add upstream: %s", out)
			}

			err := FetchCheckpointRefFrom(context.Background(), ref, []string{"upstream", "origin"}, nil)
			require.Error(t, err)
			require.NotErrorIs(t, err, plumbing.ErrReferenceNotFound)
			if !tc.addBrokenUpstream {
				require.Contains(t, err.Error(), "upstream")
				require.NotContains(t, err.Error(), "://upstream", "remote names must not be mangled as URLs")
			}
			out, err := exec.CommandContext(t.Context(), "git", "-C", workDir, "show-ref", "--verify", ref.String()).CombinedOutput()
			require.Error(t, err, "legacy origin must not seed the canonical ref after an elected remote failure: %s", out)
		})
	}
}

// TestFetchCheckpointRef_NoRemoteAtAllIsAbsence: a fully local repository —
// no origin remote and no checkpoint_remote configured — has no remote that
// could host checkpoint refs, so the ref's local absence is the final
// verdict, not a transport failure. Regression: the origin-name fallback
// probe used to run `git ls-remote origin` in a remoteless repo and surface
// exit 128, which broke backfill routing (and `explain --generate`) in fully
// local repos.
func TestFetchCheckpointRef_NoRemoteAtAllIsAbsence(t *testing.T) {
	workDir := t.TempDir()
	testutil.InitRepo(t, workDir)
	testutil.WriteFile(t, workDir, "f.txt", "content")
	testutil.GitAdd(t, workDir, "f.txt")
	testutil.GitCommit(t, workDir, "init")
	t.Chdir(workDir)

	ref := plumbing.ReferenceName("refs/entire/checkpoints/Z9/01KVBJCWYA4YW6J5M9GP655HZ9")
	err := FetchCheckpointRef(context.Background(), ref)
	require.Error(t, err)
	require.ErrorIs(t, err, plumbing.ErrReferenceNotFound,
		"a repo with no remotes must classify a locally absent ref as absence")
}

// TestFetchCheckpointRef_UnreadableSettingsNeverClassifiesAbsence: when the
// checkpoint_remote configuration CANNOT BE READ (corrupt settings), whether a
// checkpoint remote exists is undeterminable. The no-remotes absence shortcut
// must not fire on a load error — the run falls through to the ls-remote
// probe, which surfaces the missing origin as a transport error, never as
// absence.
func TestFetchCheckpointRef_UnreadableSettingsNeverClassifiesAbsence(t *testing.T) {
	workDir := t.TempDir()
	testutil.InitRepo(t, workDir)
	testutil.WriteFile(t, workDir, "f.txt", "content")
	testutil.GitAdd(t, workDir, "f.txt")
	testutil.GitCommit(t, workDir, "init")
	testutil.WriteFile(t, workDir, ".entire/settings.json", "{not valid json")
	t.Chdir(workDir)

	ref := plumbing.ReferenceName("refs/entire/checkpoints/Z9/01KVBJCWYA4YW6J5M9GP655HZ9")
	err := FetchCheckpointRef(context.Background(), ref)
	require.Error(t, err)
	require.NotErrorIs(t, err, plumbing.ErrReferenceNotFound,
		"an unreadable checkpoint_remote configuration must not classify as absence")
}

// TestFetchCheckpointRef_MalformedCheckpointRemoteNeverClassifiesAbsence: a
// checkpoint_remote entry that is present but malformed (here: missing the
// required repo field) means the user configured a checkpoint remote and
// botched it. Combined with a missing origin, that must stay a failure —
// classifying it as absence would misroute backfills for checkpoints that
// live on the remote the user intended.
func TestFetchCheckpointRef_MalformedCheckpointRemoteNeverClassifiesAbsence(t *testing.T) {
	workDir := t.TempDir()
	testutil.InitRepo(t, workDir)
	testutil.WriteFile(t, workDir, "f.txt", "content")
	testutil.GitAdd(t, workDir, "f.txt")
	testutil.GitCommit(t, workDir, "init")
	testutil.WriteFile(t, workDir, ".entire/settings.json",
		`{"enabled": true, "strategy_options": {"checkpoint_remote": {"provider": "github"}}}`)
	t.Chdir(workDir)

	ref := plumbing.ReferenceName("refs/entire/checkpoints/Z9/01KVBJCWYA4YW6J5M9GP655HZ9")
	err := FetchCheckpointRef(context.Background(), ref)
	require.Error(t, err)
	require.NotErrorIs(t, err, plumbing.ErrReferenceNotFound,
		"a present-but-malformed checkpoint_remote must not classify as absence")
}

// TestFetchCheckpointRef_NonOriginRemoteNeverClassifiesAbsence: a repo whose
// only remote is not named origin (git clone -o upstream is a common shape)
// is NOT remoteless — checkpoint refs are pushed to whatever remote the
// pre-push hook fires for, so they can legitimately live on a non-origin
// remote. Classifying this repo as absence would misroute backfills; it must
// stay a failure.
func TestFetchCheckpointRef_NonOriginRemoteNeverClassifiesAbsence(t *testing.T) {
	bareDir := t.TempDir()
	out, err := exec.CommandContext(t.Context(), "git", "init", "--bare", bareDir).CombinedOutput()
	require.NoError(t, err, "git init --bare: %s", out)

	workDir := t.TempDir()
	testutil.InitRepo(t, workDir)
	testutil.WriteFile(t, workDir, "f.txt", "content")
	testutil.GitAdd(t, workDir, "f.txt")
	testutil.GitCommit(t, workDir, "init")
	out, err = exec.CommandContext(t.Context(), "git", "-C", workDir, "remote", "add", "upstream", bareDir).CombinedOutput()
	require.NoError(t, err, "git remote add upstream: %s", out)
	t.Chdir(workDir)

	ref := plumbing.ReferenceName("refs/entire/checkpoints/Z9/01KVBJCWYA4YW6J5M9GP655HZ9")
	err = FetchCheckpointRef(context.Background(), ref)
	require.Error(t, err)
	require.NotErrorIs(t, err, plumbing.ErrReferenceNotFound,
		"a repo with a non-origin remote must not classify as absence")
}

// TestFetchCheckpointRef_CanceledContextNeverClassifiesAbsence: a dead caller
// context makes every git subprocess fail, which must surface as a transport
// failure — never as absence. Regression: the no-remotes guard once inferred
// "no origin" from a GetRemoteURL failure, which a canceled context also
// produces, converting Ctrl-C in a healthy repo into a false "checkpoint does
// not exist" verdict that write routing acts on.
func TestFetchCheckpointRef_CanceledContextNeverClassifiesAbsence(t *testing.T) {
	_, ref := checkpointRefFixture(t, true)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := FetchCheckpointRef(ctx, ref)
	require.Error(t, err)
	require.NotErrorIs(t, err, plumbing.ErrReferenceNotFound,
		"a canceled context must stay a failure, never absence")
}
