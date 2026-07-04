//go:build e2e

package tests

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/e2e/entire"
	"github.com/entireio/cli/e2e/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestResumeFromWorktreeSession is G2: an agent session runs inside a linked
// `git worktree`, the user commits and pushes the worktree's branch with a plain
// `git push` (the real pre-push hook syncs checkpoints — no explicit
// PushCheckpointRefs), and a fresh clone elsewhere can `entire resume` the
// session. This exercises the worktree shadow-branch namespace and the shared
// (common-dir) push queue end to end.
//
// vogon-only: deterministic and free, matching the doctor canary. The prompt
// phrasing is copied verbatim from the resume-from-clone test so vogon's
// regex-based prompt parser needs no changes.
func TestResumeFromWorktreeSession(t *testing.T) {
	testutil.ForEachNamedAgent(t, 3*time.Minute, []string{"vogon"}, func(t *testing.T, s *testutil.RepoState, ctx context.Context) {
		bareDir := testutil.SetupBareRemote(t, s)

		// Commit the `entire enable` files on the default branch and push, so the
		// bare remote has a base the clone can start from.
		s.Git(t, "add", ".")
		s.Git(t, "commit", "-m", "Enable entire")
		s.Git(t, "push")

		// Create a linked worktree on a feature branch. It shares the main repo's
		// git common dir (objects, refs, hooks, and the checkpoint push queue).
		worktreeDir := filepath.Join(t.TempDir(), "worktree")
		if resolved, symErr := filepath.EvalSymlinks(filepath.Dir(worktreeDir)); symErr == nil {
			worktreeDir = filepath.Join(resolved, "worktree")
		}
		s.Git(t, "worktree", "add", worktreeDir, "-b", "feature")

		// .entire/ is gitignored, so the worktree's working tree has none — enable
		// Entire in the worktree to give it its own settings (hooks live in the
		// shared common dir and are already installed).
		entire.Enable(t, worktreeDir, s.Agent.EntireAgent())
		testutil.PatchSettings(t, worktreeDir, map[string]any{"log_level": "debug", "commit_linking": "always"})

		// Run the agent IN the worktree (its cwd is the worktree, so the session
		// transcript and shadow branch are scoped to it).
		out, err := s.Agent.RunPrompt(ctx, worktreeDir,
			"create a file at docs/hello.md with a paragraph about greetings. Do not ask for confirmation, just make the change.")
		s.ConsoleLog.WriteString("> [worktree] " + out.Command + "\nstdout:\n" + out.Stdout + "\nstderr:\n" + out.Stderr + "\n")
		require.NoError(t, err, "agent failed in worktree")

		testutil.Git(t, worktreeDir, "add", ".")
		testutil.Git(t, worktreeDir, "commit", "-m", "Add hello doc")

		// Checkpoint refs live in the shared common dir, visible from s.Dir.
		testutil.WaitForCheckpoint(t, s, 30*time.Second)
		checkpointID := testutil.AssertHasCheckpointTrailer(t, worktreeDir, "HEAD")
		require.NotEmpty(t, checkpointID, "worktree commit should carry a checkpoint trailer")
		sessionMeta := testutil.WaitForSessionMetadata(t, s.Dir, checkpointID, 0, 30*time.Second)

		// Plain push of the worktree's branch through the real hook drains the
		// shared push queue / syncs the v1 branch — no explicit PushCheckpointRefs.
		testutil.Git(t, worktreeDir, "push", "-u", "origin", "feature")
		testutil.AssertCheckpointsOnRemote(t, s, bareDir)

		// Clone elsewhere (a teammate) and resume the worktree's session.
		cloneDir := t.TempDir()
		if resolved, symErr := filepath.EvalSymlinks(cloneDir); symErr == nil {
			cloneDir = resolved
		}
		require.NoError(t, os.RemoveAll(cloneDir))
		testutil.Git(t, "", "clone", bareDir, cloneDir)
		testutil.Git(t, cloneDir, "config", "user.name", "E2E Clone")
		testutil.Git(t, cloneDir, "config", "user.email", "e2e-clone@test.local")

		require.False(t, testutil.CheckpointsPresent(cloneDir),
			"checkpoint metadata should not exist locally in the clone before resume")

		// Materialize the feature branch locally, then return to the default branch
		// so resume can switch to it.
		mainClone := testutil.GitOutput(t, cloneDir, "branch", "--show-current")
		testutil.Git(t, cloneDir, "checkout", "feature")
		testutil.Git(t, cloneDir, "checkout", mainClone)

		entire.Enable(t, cloneDir, s.Agent.EntireAgent())
		testutil.CommitIfDirty(t, cloneDir, "Enable entire in clone")

		resumeOut, err := entire.Resume(cloneDir, "feature")
		require.NoError(t, err, "entire resume failed in clone: %s", resumeOut)

		current := testutil.GitOutput(t, cloneDir, "branch", "--show-current")
		assert.Equal(t, "feature", current, "should be on the feature branch after resume")
		assert.Contains(t, resumeOut, "To continue", "resume output should show resume instructions")
		assert.True(t, testutil.CheckpointsPresent(cloneDir),
			"checkpoint metadata should exist locally after resuming the worktree session")

		if restoredTranscript, ok := testutil.RestoredSessionTranscriptPath(t, cloneDir, sessionMeta); ok {
			_, statErr := os.Stat(restoredTranscript)
			assert.NoError(t, statErr, "restored session transcript should exist at %s after resume", restoredTranscript)
		}
	})
}

// TestDoctorUnreachableRemote is G3: `entire doctor` on a repo whose origin
// points at an unreachable target reports the situation and exits without
// hanging or crashing. It pins the current behavior: doctor degrades to
// local-only checks rather than blocking on the dead remote.
func TestDoctorUnreachableRemote(t *testing.T) {
	testutil.ForEachNamedAgent(t, 3*time.Minute, []string{"vogon"}, func(t *testing.T, s *testutil.RepoState, ctx context.Context) {
		// A committed checkpoint so doctor has real metadata/session state to scan.
		s.Git(t, "add", ".")
		s.Git(t, "commit", "-m", "Enable entire")

		_, err := s.RunPrompt(t, ctx,
			"create a file at docs/doctor.md with a short paragraph about checkpoint health. Do not ask for confirmation or approval, just make the change.")
		require.NoError(t, err, "agent failed")

		s.Git(t, "add", ".")
		s.Git(t, "commit", "-m", "Add doctor coverage doc")
		testutil.WaitForCheckpoint(t, s, 30*time.Second)

		// Point origin at a local path that does not exist — unreachable but fully
		// hermetic (no network) and fails fast.
		deadRemote := filepath.Join(t.TempDir(), "nonexistent-bare.git")
		s.Git(t, "remote", "add", "origin", deadRemote)

		// Bound the run so a hang surfaces as a deadline rather than blocking.
		dctx, cancel := context.WithTimeout(ctx, 90*time.Second)
		defer cancel()

		out, doctorErr := entire.DoctorCtx(dctx, s.Dir)
		t.Logf("doctor (unreachable remote) exit=%v\n%s", doctorErr, out)

		require.False(t, errors.Is(dctx.Err(), context.DeadlineExceeded),
			"doctor must not hang on an unreachable remote")
		require.NotContains(t, out, "panic:", "doctor must not crash on an unreachable remote")

		// Pin: doctor still completes its local scan and reports session health
		// despite the dead remote (it degrades rather than aborting the scan).
		assert.Contains(t, out, "stuck sessions",
			"doctor should still report the session-health scan even with an unreachable remote")
	})
}
