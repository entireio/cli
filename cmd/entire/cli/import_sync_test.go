package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	git "github.com/go-git/go-git/v6"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// testSyncRemote is the remote the stubbed election resolves to.
const testSyncRemote = "origin"

// stubImportSync redirects the login and push seams for one test and restores
// them afterwards. It returns a pointer to the recorded push calls.
func stubImportSync(t *testing.T, loggedIn bool, push func(context.Context, *git.Repository, string) (int, bool, error)) *[]string {
	t.Helper()
	origLogin, origRemote, origPush := importLoggedIn, importResolvePushRemote, importPushQueuedRefs
	t.Cleanup(func() {
		importLoggedIn, importResolvePushRemote, importPushQueuedRefs = origLogin, origRemote, origPush
	})

	var calls []string
	importLoggedIn = func() bool { return loggedIn }
	importResolvePushRemote = func(_ context.Context, explicit string) (string, error) {
		if explicit != "" {
			return explicit, nil
		}
		return testSyncRemote, nil
	}
	importPushQueuedRefs = func(ctx context.Context, repo *git.Repository, remote string) (int, bool, error) {
		calls = append(calls, remote)
		return push(ctx, repo, remote)
	}
	return &calls
}

// TestSyncImportedCheckpoints covers each way the post-import sync can end, and
// in particular that it never runs when logged out (the logged-out notice owns
// that case) and never pushes on the git-branch backend, which has no queue.
// Mutates package-level seams and the checkpoints env override, so no t.Parallel.
func TestSyncImportedCheckpoints(t *testing.T) {
	cases := []struct {
		name      string
		loggedIn  bool
		backend   string
		pushed    int
		disabled  bool
		pushErr   error
		wantPush  bool
		wantOut   string
		wantEmpty bool
	}{
		{
			name:      "logged out never pushes",
			loggedIn:  false,
			backend:   "git-refs",
			wantPush:  false,
			wantEmpty: true,
		},
		{
			name:      "git-branch backend never pushes",
			loggedIn:  true,
			backend:   "git-branch",
			wantPush:  false,
			wantEmpty: true,
		},
		{
			name:     "successful push reports the count",
			loggedIn: true,
			backend:  "git-refs",
			pushed:   3,
			wantPush: true,
			wantOut:  "Synced 3 checkpoint(s) to origin.",
		},
		{
			name:     "push disabled in settings is reported",
			loggedIn: true,
			backend:  "git-refs",
			disabled: true,
			wantPush: true,
			wantOut:  "Checkpoint pushing is disabled in settings",
		},
		{
			name:      "empty queue says nothing",
			loggedIn:  true,
			backend:   "git-refs",
			pushed:    0,
			wantPush:  true,
			wantEmpty: true,
		},
		{
			name:     "push failure is reported but not fatal",
			loggedIn: true,
			backend:  "git-refs",
			pushErr:  errors.New("remote hung up"),
			wantPush: true,
			wantOut:  "could not sync imported history to origin",
		},
		{
			name:      "cancellation stays silent",
			loggedIn:  true,
			backend:   "git-refs",
			pushErr:   context.Canceled,
			wantPush:  true,
			wantEmpty: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(settings.EnvCheckpointsPrimary, tc.backend)
			calls := stubImportSync(t, tc.loggedIn, func(context.Context, *git.Repository, string) (int, bool, error) {
				return tc.pushed, tc.disabled, tc.pushErr
			})

			var buf bytes.Buffer
			syncImportedCheckpoints(context.Background(), &buf, nil, "")

			if got := len(*calls) > 0; got != tc.wantPush {
				t.Fatalf("push attempted=%v, want %v (calls=%v)", got, tc.wantPush, *calls)
			}
			out := buf.String()
			switch {
			case tc.wantEmpty:
				require.Empty(t, out, "expected no output")
			default:
				require.Contains(t, out, tc.wantOut)
			}
		})
	}
}

// TestSyncImportedCheckpoints_ExplicitRemoteWins verifies --remote overrides the
// elected checkpoint sync remote.
func TestSyncImportedCheckpoints_ExplicitRemoteWins(t *testing.T) {
	t.Setenv(settings.EnvCheckpointsPrimary, "git-refs")
	calls := stubImportSync(t, true, func(context.Context, *git.Repository, string) (int, bool, error) {
		return 1, false, nil
	})

	var buf bytes.Buffer
	syncImportedCheckpoints(context.Background(), &buf, nil, "upstream")

	require.Equal(t, []string{"upstream"}, *calls)
	require.Contains(t, buf.String(), "to upstream.")
}

// TestSyncImportedCheckpoints_RemoteResolutionFailureIsReported covers the repo
// with no git remote: the import already succeeded locally, so the command must
// explain why nothing synced rather than fail.
func TestSyncImportedCheckpoints_RemoteResolutionFailureIsReported(t *testing.T) {
	t.Setenv(settings.EnvCheckpointsPrimary, "git-refs")
	calls := stubImportSync(t, true, func(context.Context, *git.Repository, string) (int, bool, error) {
		return 0, false, nil
	})
	importResolvePushRemote = func(context.Context, string) (string, error) {
		return "", errors.New("no git remotes configured")
	}

	var buf bytes.Buffer
	syncImportedCheckpoints(context.Background(), &buf, nil, "")

	require.Empty(t, *calls, "must not attempt a push without a remote")
	require.Contains(t, buf.String(), "no git remotes configured")
}

// TestImportSyncsAlreadyImportedTurns is the regression test for issue #1773:
// history imported while logged out stays queued, so a later logged-in run that
// imports nothing new ("0 turn(s) ... (N already imported)") must STILL push.
// Gating the sync on TurnsImported > 0 would reintroduce the bug.
func TestImportSyncsAlreadyImportedTurns(t *testing.T) {
	// Not parallel: t.Chdir for CWD-based repo resolution plus t.Setenv.
	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	testutil.WriteFile(t, repoDir, "f.txt", "x")
	testutil.GitAdd(t, repoDir, "f.txt")
	testutil.GitCommit(t, repoDir, "init")
	t.Chdir(repoDir)
	t.Setenv(settings.EnvCheckpointsPrimary, "git-refs")

	claudeDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(claudeDir, "s.jsonl"),
		[]byte(`{"type":"user","uuid":"u1","message":{"role":"user","content":"hi"}}`+"\n"), 0o644))

	calls := stubImportSync(t, true, func(context.Context, *git.Repository, string) (int, bool, error) {
		return 1, false, nil
	})

	runImport := func() string {
		cmd := newImportCmd()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs([]string{"claude-code", "--path", claudeDir})
		require.NoError(t, cmd.Execute(), "import failed: %s", out.String())
		return out.String()
	}

	first := runImport()
	require.Contains(t, first, "Imported 1 turn(s)")
	require.Len(t, *calls, 1, "first import should sync")

	second := runImport()
	require.Contains(t, second, "(1 already imported)", "second run should be idempotent locally")
	require.Contains(t, second, "Imported 0 turn(s)")
	require.Len(t, *calls, 2, "second import must STILL sync even though it imported nothing new (#1773)")
}

// TestImportDryRunNeverSyncs guards the dry-run contract: it writes nothing
// locally, so there is nothing to push.
func TestImportDryRunNeverSyncs(t *testing.T) {
	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	testutil.WriteFile(t, repoDir, "f.txt", "x")
	testutil.GitAdd(t, repoDir, "f.txt")
	testutil.GitCommit(t, repoDir, "init")
	t.Chdir(repoDir)
	t.Setenv(settings.EnvCheckpointsPrimary, "git-refs")

	claudeDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(claudeDir, "s.jsonl"),
		[]byte(`{"type":"user","uuid":"u1","message":{"role":"user","content":"hi"}}`+"\n"), 0o644))

	calls := stubImportSync(t, true, func(context.Context, *git.Repository, string) (int, bool, error) {
		return 1, false, nil
	})

	cmd := newImportCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"claude-code", "--path", claudeDir, "--dry-run"})
	require.NoError(t, cmd.Execute())

	require.Contains(t, out.String(), "Would import", "expected dry-run wording: %q", out.String())
	require.Empty(t, *calls, "dry run must not push")
}
