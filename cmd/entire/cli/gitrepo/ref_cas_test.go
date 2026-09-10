package gitrepo

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/testutil/gitenv"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/require"
)

type refCASBackend struct {
	name string
	init func(*testing.T) (repoDir, initial, replacement string)
}

func refCASBackends() []refCASBackend {
	return []refCASBackend{
		{name: "files", init: initFilesRefCASRepo},
		{
			name: "reftable",
			init: func(t *testing.T) (string, string, string) {
				t.Helper()
				repoDir, initial := initReftableRepo(t, "initial.txt", "initial\n")
				replacement := reftableCommit(t, repoDir, "next.txt", "next\n")
				return repoDir, initial, replacement
			},
		},
	}
}

func initFilesRefCASRepo(t *testing.T) (string, string, string) {
	t.Helper()
	repoDir := t.TempDir()
	gitenv.Run(t, repoDir, "init", "-b", "main")
	gitenv.Run(t, repoDir, "config", "user.name", "Test User")
	gitenv.Run(t, repoDir, "config", "user.email", "test@example.com")
	gitenv.Run(t, repoDir, "config", "commit.gpgsign", "false")
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, "initial.txt"), []byte("initial\n"), 0o644))
	gitenv.Run(t, repoDir, "add", "initial.txt")
	gitenv.Run(t, repoDir, "commit", "--no-gpg-sign", "-m", "initial")
	initial := strings.TrimSpace(gitenv.Run(t, repoDir, "rev-parse", "HEAD"))
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, "next.txt"), []byte("next\n"), 0o644))
	gitenv.Run(t, repoDir, "add", "next.txt")
	gitenv.Run(t, repoDir, "commit", "--no-gpg-sign", "-m", "next")
	replacement := strings.TrimSpace(gitenv.Run(t, repoDir, "rev-parse", "HEAD"))
	return repoDir, initial, replacement
}

func TestCompareAndSwapRef_RejectsSymbolicRef(t *testing.T) {
	t.Parallel()
	for _, tt := range refCASBackends() {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			repoDir, initial, replacement := tt.init(t)
			refName := plumbing.ReferenceName("refs/entire/symbolic-cas")
			targetName := plumbing.NewBranchReferenceName("main")
			gitenv.Run(t, repoDir, "update-ref", targetName.String(), initial)
			gitenv.Run(t, repoDir, "symbolic-ref", refName.String(), targetName.String())

			err := CompareAndSwapRef(
				context.Background(),
				repoDir,
				refName,
				plumbing.NewHash(replacement),
				plumbing.NewHash(initial),
			)

			require.ErrorIs(t, err, ErrRefSymbolic)
			require.Equal(t, targetName.String(), strings.TrimSpace(gitenv.Run(t, repoDir, "symbolic-ref", refName.String())))
			require.Equal(t, initial, strings.TrimSpace(gitenv.Run(t, repoDir, "rev-parse", targetName.String())))

			gitenv.Run(t, repoDir, "symbolic-ref", "-d", refName.String())
			gitenv.Run(t, repoDir, "update-ref", refName.String(), initial)
			err = CompareAndSwapRef(
				context.Background(),
				repoDir,
				refName,
				plumbing.NewHash(replacement),
				plumbing.NewHash(initial),
			)
			require.NoError(t, err)
			require.Equal(t, replacement, strings.TrimSpace(gitenv.Run(t, repoDir, "rev-parse", refName.String())))
		})
	}
}

func TestPreparedRefCASPreventsConcurrentSymbolicConversion(t *testing.T) {
	t.Parallel()
	for _, tt := range refCASBackends() {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			repoDir, initial, replacement := tt.init(t)
			refName := plumbing.ReferenceName("refs/entire/prepared-cas")
			targetName := plumbing.NewBranchReferenceName("main")
			gitenv.Run(t, repoDir, "update-ref", refName.String(), initial)

			tx, err := prepareRefCAS(
				context.Background(),
				repoDir,
				refName,
				plumbing.NewHash(replacement),
				plumbing.NewHash(initial),
			)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, tx.abort()) })

			cmd := exec.Command("git", "symbolic-ref", refName.String(), targetName.String()) //nolint:noctx // must run while the prepared transaction is open
			cmd.Dir = repoDir
			cmd.Env = gitenv.Isolated()
			output, err := cmd.CombinedOutput()
			require.Error(t, err, "a writer must not change the ref type after CAS preparation")
			require.Contains(t, strings.ToLower(string(output)), "lock")
			require.NoError(t, tx.abort())

			_, symbolic, err := symbolicRefTarget(context.Background(), repoDir, refName)
			require.NoError(t, err)
			require.False(t, symbolic)
			require.Equal(t, initial, strings.TrimSpace(gitenv.Run(t, repoDir, "rev-parse", refName.String())))
		})
	}
}

func TestPreparedRefCASCancellationReleasesGitLock(t *testing.T) {
	t.Parallel()
	for _, tt := range refCASBackends() {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			repoDir, initial, replacement := tt.init(t)
			refName := plumbing.ReferenceName("refs/entire/canceled-cas")
			gitenv.Run(t, repoDir, "update-ref", refName.String(), initial)

			ctx, cancel := context.WithCancel(context.Background())
			tx, err := prepareRefCAS(
				ctx,
				repoDir,
				refName,
				plumbing.NewHash(replacement),
				plumbing.NewHash(initial),
			)
			require.NoError(t, err)
			require.Equal(t, refCASWaitDelay, tx.cmd.WaitDelay)

			cancel()
			require.ErrorIs(t, tx.wait(), context.Canceled)
			require.Equal(t, initial, strings.TrimSpace(gitenv.Run(t, repoDir, "rev-parse", refName.String())))

			gitenv.Run(t, repoDir, "update-ref", refName.String(), replacement, initial)
			require.Equal(t, replacement, strings.TrimSpace(gitenv.Run(t, repoDir, "rev-parse", refName.String())))
		})
	}
}

func TestRefCASCommitAcknowledgementSurvivesCancellation(t *testing.T) {
	t.Parallel()
	for _, backend := range refCASBackends() {
		t.Run(backend.name, func(t *testing.T) {
			t.Parallel()
			repoDir, initial, replacement := backend.init(t)
			refName := plumbing.ReferenceName("refs/entire/committed-cas")
			gitenv.Run(t, repoDir, "update-ref", refName.String(), initial)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			tx, err := prepareRefCAS(ctx, repoDir, refName, plumbing.NewHash(replacement), plumbing.NewHash(initial))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, tx.abort()) })

			require.NoError(t, tx.exchange("commit", "commit: ok"))
			cancel()
			require.Eventually(t, tx.canceled.Load, time.Second, time.Millisecond)
			require.NoError(t, tx.wait(), "a confirmed ref update must not become a cancellation failure")
			require.Equal(t, replacement, strings.TrimSpace(gitenv.Run(t, repoDir, "rev-parse", refName.String())))

			gitenv.Run(t, repoDir, "update-ref", refName.String(), initial, replacement)
			require.Equal(t, initial, strings.TrimSpace(gitenv.Run(t, repoDir, "rev-parse", refName.String())))
		})
	}
}

func TestCompareAndSwapRef_DeletedRefRemainsCASConflict(t *testing.T) {
	t.Parallel()
	for _, backend := range refCASBackends() {
		t.Run(backend.name, func(t *testing.T) {
			t.Parallel()
			repoDir, initial, replacement := backend.init(t)
			refName := plumbing.ReferenceName("refs/entire/deleted-cas")
			gitenv.Run(t, repoDir, "update-ref", refName.String(), initial)
			gitenv.Run(t, repoDir, "update-ref", "-d", refName.String())

			err := CompareAndSwapRef(t.Context(), repoDir, refName, plumbing.NewHash(replacement), plumbing.NewHash(initial))
			require.ErrorIs(t, err, ErrRefCASConflict)
			require.NoError(t, CompareAndSwapRef(t.Context(), repoDir, refName, plumbing.NewHash(replacement), plumbing.ZeroHash))
			require.Equal(t, replacement, strings.TrimSpace(gitenv.Run(t, repoDir, "rev-parse", refName.String())))
		})
	}
}

func TestCompareAndSwapRef_DirectoryAtRefPath(t *testing.T) {
	t.Parallel()
	for _, layout := range []string{"worktree", "linked worktree", "bare"} {
		t.Run(layout, func(t *testing.T) {
			t.Parallel()
			repoDir, initial, replacement := initFilesRefCASRepo(t)
			commonDir := filepath.Join(repoDir, ".git")
			switch layout {
			case "linked worktree":
				linkedDir := t.TempDir()
				gitenv.Run(t, repoDir, "worktree", "add", "-b", "linked", linkedDir)
				repoDir = linkedDir
			case "bare":
				bareDir := t.TempDir()
				gitenv.Run(t, repoDir, "clone", "--bare", repoDir, bareDir)
				repoDir, commonDir = bareDir, bareDir
			}
			refName := plumbing.ReferenceName("refs/entire/directory-cas")
			refPath := filepath.Join(commonDir, filepath.FromSlash(refName.String()))
			require.NoError(t, os.MkdirAll(refPath, 0o755))

			err := CompareAndSwapRef(t.Context(), repoDir, refName, plumbing.NewHash(replacement), plumbing.NewHash(initial))
			require.ErrorContains(t, err, "is a directory")
			require.NotErrorIs(t, err, ErrRefCASConflict)
			require.NotErrorIs(t, err, ErrRefLocked)

			require.NoError(t, os.Remove(refPath))
			require.NoError(t, CompareAndSwapRef(t.Context(), repoDir, refName, plumbing.NewHash(replacement), plumbing.ZeroHash))
			require.Equal(t, replacement, strings.TrimSpace(gitenv.Run(t, repoDir, "rev-parse", refName.String())))
		})
	}
}

func TestCompareAndSwapRef_IgnoresInheritedRepoOverrides(t *testing.T) {
	// Not parallel: t.Setenv is incompatible with t.Parallel.
	targetDir, initial, replacement := initFilesRefCASRepo(t)
	decoyDir, _, _ := initFilesRefCASRepo(t)
	refName := plumbing.ReferenceName("refs/entire/environment-cas")
	gitenv.Run(t, targetDir, "update-ref", refName.String(), initial)
	t.Setenv("GIT_DIR", filepath.Join(decoyDir, ".git"))
	t.Setenv("GIT_WORK_TREE", decoyDir)

	err := CompareAndSwapRef(
		context.Background(),
		targetDir,
		refName,
		plumbing.NewHash(replacement),
		plumbing.NewHash(initial),
	)

	require.NoError(t, err)
	verify := exec.Command("git", "rev-parse", "--verify", refName.String()) //nolint:noctx // short local test command
	verify.Dir = targetDir
	verify.Env = EnvWithoutRepoOverrides()
	out, err := verify.Output()
	require.NoError(t, err)
	require.Equal(t, replacement, strings.TrimSpace(string(out)))
}

func TestCompareAndSwapRef_RejectsRefProtocolInjection(t *testing.T) {
	t.Parallel()
	repoDir, initial, replacement := initFilesRefCASRepo(t)
	targetName := plumbing.NewBranchReferenceName("main")
	gitenv.Run(t, repoDir, "update-ref", targetName.String(), initial)
	maliciousName := plumbing.ReferenceName("refs/entire/invalid\nupdate " + targetName.String())

	err := CompareAndSwapRef(
		context.Background(),
		repoDir,
		maliciousName,
		plumbing.NewHash(replacement),
		plumbing.NewHash(initial),
	)

	require.ErrorContains(t, err, "validate ref for compare-and-swap")
	require.Equal(t, initial, strings.TrimSpace(gitenv.Run(t, repoDir, "rev-parse", targetName.String())))
}

func TestCompareAndSwapRef_ReftableLockContention(t *testing.T) {
	t.Parallel()
	repoDir, initial := initReftableRepo(t, "initial.txt", "initial\n")
	newHash := reftableCommit(t, repoDir, "next.txt", "next\n")
	refName := plumbing.ReferenceName("refs/entire/reftable-lock")
	gitenv.Run(t, repoDir, "update-ref", refName.String(), initial)
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, ".git", "reftable", "tables.list.lock"), nil, 0o600))

	err := CompareAndSwapRef(
		context.Background(),
		repoDir,
		refName,
		plumbing.NewHash(newHash),
		plumbing.NewHash(initial),
	)

	require.ErrorIs(t, err, ErrRefLocked)
	require.Equal(t, initial, strings.TrimSpace(gitenv.Run(t, repoDir, "rev-parse", refName.String())))
}

func TestRefCASErrorClassification(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		stderr       string
		wantConflict bool
		wantLocked   bool
	}{
		{
			name:         "expected value mismatch",
			stderr:       "cannot lock ref 'refs/heads/main': is at aaaa but expected bbbb",
			wantConflict: true,
		},
		{
			name:         "expected ref was deleted",
			stderr:       "cannot lock ref 'refs/heads/main': unable to resolve reference 'refs/heads/main'",
			wantConflict: true,
		},
		{
			name:   "expected ref is corrupt",
			stderr: "cannot lock ref 'refs/heads/main': unable to resolve reference 'refs/heads/main': reference broken",
		},
		{
			name:   "expected ref is unreadable",
			stderr: "cannot lock ref 'refs/heads/main': unable to resolve reference 'refs/heads/main': Permission denied",
		},
		{
			name:         "create if absent conflict",
			stderr:       "cannot lock ref 'refs/heads/main': reference already exists",
			wantConflict: true,
		},
		{
			name:       "lock held by another process",
			stderr:     "unable to create '/repo/.git/refs/heads/main.lock': File exists.",
			wantLocked: true,
		},
		{
			name:       "reftable lock held by another process",
			stderr:     "fatal: update_ref failed for ref 'refs/heads/main': cannot lock references",
			wantLocked: true,
		},
		{
			name:   "permission failure",
			stderr: "cannot lock ref 'refs/heads/main': unable to create '/repo/.git/refs/heads/main.lock': Permission denied",
		},
		{
			name:   "namespace conflict",
			stderr: "cannot lock ref 'refs/heads/main/child': 'refs/heads/main' exists; cannot create 'refs/heads/main/child'",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			stderr := []byte(tt.stderr)
			require.Equal(t, tt.wantConflict, isRefCASConflict(plumbing.NewBranchReferenceName("main"), stderr))
			require.Equal(t, tt.wantLocked, isRefLockContention(stderr))
		})
	}
}
