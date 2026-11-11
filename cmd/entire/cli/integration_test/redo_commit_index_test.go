//go:build integration

package integration

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/execx"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
)

// A redo compares the dropped commit with the commit git is preparing. `git
// commit -a` and path-limited commits prepare it in a temporary index, so the
// ordinary index can hold different content than what is being committed.
func TestRedo_ComparesTheCommitIndex(t *testing.T) {
	t.Parallel()
	// Long enough for git's rename detection to pair a deleted b.txt with a copy.
	body := strings.Repeat("unchanged line\n", 30)
	commitStaged := []string{"commit", "--no-gpg-sign", "-m", "redo"}
	commitAll := []string{"commit", "--no-gpg-sign", "-am", "redo"}
	commitPath := []string{"commit", "--no-gpg-sign", "-m", "redo", "--", "a.txt"}
	stageAll := func(_ *TestEnv, git func(...string) string) { git("add", "-A") }
	// The ordinary index then holds exactly the dropped commit's content, so
	// only the commit index shows the rewrite.
	stageAllThenRewriteA := func(env *TestEnv, git func(...string) string) {
		git("add", "-A")
		env.WriteFile("a.txt", "human rewrite\n")
	}
	for _, tc := range []struct {
		name     string
		prepare  func(*TestEnv, func(...string) string)
		commit   []string
		inherits bool
	}{
		{name: "staged", prepare: stageAll, commit: commitStaged, inherits: true},
		{name: "all", commit: commitAll, inherits: true},
		{name: "partial", commit: commitPath, inherits: true},
		{name: "amend-all", commit: []string{"commit", "--no-gpg-sign", "--amend", "-a", "--no-edit"}, inherits: true},
		{name: "all-rewritten", prepare: stageAllThenRewriteA, commit: commitAll},
		{name: "partial-rewritten", prepare: stageAllThenRewriteA, commit: commitPath},
		// The dropped commit changed b.txt, and b.txt is staged as deleted.
		// Git pairs that deletion with c.txt as a rename, which must not hide it.
		{name: "renamed-deletion", prepare: func(env *TestEnv, git func(...string) string) {
			git("add", "a.txt")
			git("rm", "-qf", "b.txt")
			env.WriteFile("c.txt", body)
			git("add", "c.txt")
		}, commit: commitStaged},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := NewFeatureBranchEnv(t)
			git := gitWithEntireCommitHooks(t, env)
			env.WriteFile("a.txt", "base\n")
			env.WriteFile("b.txt", body)
			env.WriteFile("d.txt", "doomed\n")
			git("add", "a.txt", "b.txt", "d.txt")
			git("commit", "--no-gpg-sign", "-m", "base")
			env.WriteFile("a.txt", "agent\n")
			env.WriteFile("b.txt", body+"agent\n")
			git("add", "a.txt", "b.txt")
			git("rm", "-q", "d.txt")
			env.GitCommitWithCheckpointID("agent: edit a and b, delete d", advForeignCheckpoint)

			git("reset", "-q", "HEAD~1")
			if tc.prepare != nil {
				tc.prepare(env, git)
			}
			git(tc.commit...)

			require.Equal(t, env.ReadFile("a.txt"), git("show", "HEAD:a.txt"), "the commit records a.txt as the worktree has it")
			ids := trailers.ParseAllCheckpoints(git("log", "-1", "--format=%B"))
			var got []string
			for _, cpID := range ids {
				got = append(got, cpID.String())
			}
			if !tc.inherits {
				require.NotContains(t, got, advForeignCheckpoint, "rewritten content is new work, not a redo")
				return
			}
			require.Contains(t, got, advForeignCheckpoint, "recommitting the dropped commit's content is a redo")
		})
	}
}

// gitWithEntireCommitHooks runs real git with Entire's commit hooks, so the
// hooks see the environment git gives them, including a temporary index. Each
// hook passes the arguments the installed hook passes (strategy/hooks.go).
func gitWithEntireCommitHooks(t *testing.T, env *TestEnv) func(...string) string {
	t.Helper()
	git := gitWithEntireCommitHooksEnv(t, env)
	return func(args ...string) string {
		t.Helper()
		out, err := git(nil, args...)
		require.NoError(t, err, "git %v: %s", args, out)
		return out
	}
}

// gitWithEntireCommitHooksEnv is gitWithEntireCommitHooks for a git command
// that may fail, run with extra environment such as a scripted GIT_EDITOR.
func gitWithEntireCommitHooksEnv(t *testing.T, env *TestEnv) func(extraEnv []string, args ...string) (string, error) {
	t.Helper()
	hooksDir := t.TempDir()
	binary := shellQuote(filepath.ToSlash(getTestBinary()))
	scripts := make(map[string]string)
	for hook, args := range map[string]string{"prepare-commit-msg": `"$1" "$2"`, "commit-msg": `"$1"`, "post-commit": "", "post-rewrite": `"$1"`} {
		scripts[hook] = fmt.Sprintf("#!/bin/sh\nexec %s hooks git %s %s\n", binary, hook, args)
	}
	writeScripts(t, hooksDir, scripts)
	return func(extraEnv []string, args ...string) (string, error) {
		t.Helper()
		args = append([]string{"-c", "core.hooksPath=" + hooksDir}, args...)
		cmd := execx.NonInteractive(t.Context(), "git", args...)
		cmd.Dir = env.RepoDir
		cmd.Env = append(env.cliEnv(), extraEnv...)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
}

// writeScripts writes executable shell scripts into dir, by file name.
func writeScripts(t *testing.T, dir string, scripts map[string]string) {
	t.Helper()
	// A parallel fork inherits an open write descriptor until it execs, and
	// Linux refuses to execute the script meanwhile (ETXTBSY).
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()
	for name, script := range scripts {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755))
	}
}

// shellQuote quotes s as one POSIX shell word.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}
