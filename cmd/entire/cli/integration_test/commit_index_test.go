//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/execx"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/stretchr/testify/require"
)

func TestCommitIndex_LinksNewSessionFile(t *testing.T) {
	t.Parallel()
	ForEachBackend(t, func(t *testing.T, backend string) {
		for _, mode := range []string{"staged", "partial", "all"} {
			for _, carry := range []bool{false, true} {
				for _, linked := range []bool{false, true} {
					for _, replaced := range []bool{false, true} {
						name := fmt.Sprintf("%s/carry=%t/linked=%t/replaced=%t", mode, carry, linked, replaced)
						t.Run(name, func(t *testing.T) {
							t.Parallel()
							testCommitIndex(t, backend, mode, carry, linked, replaced)
						})
					}
				}
			}
		}
	})
}

func testCommitIndex(t *testing.T, backend, mode string, carry, linked, replaced bool) {
	t.Helper()
	env := NewFeatureBranchEnv(t)
	env.CheckpointStore = backend
	mainEnv := env
	git := gitWithEntireCommitHooks(t, env)
	if linked {
		linkedDir := filepath.Join(t.TempDir(), "linked")
		git("worktree", "add", "-b", "linked", linkedDir)
		linkedEnv := *env
		linkedEnv.RepoDir = linkedDir
		env = &linkedEnv
		env.InitEntire()
		git = gitWithEntireCommitHooks(t, env)
	}
	sess := env.NewSession()
	require.NoError(t, env.SimulateUserPromptSubmitWithPromptAndTranscriptPath(sess.ID, "Create files", sess.TranscriptPath))

	const content = "a new file written by the agent\n"
	changes := []FileChange{{Path: "agent.txt", Content: content}}
	env.WriteFile("agent.txt", content)
	if carry {
		env.WriteFile("first.txt", "first contribution\n")
		changes = append(changes, FileChange{Path: "first.txt", Content: "first contribution\n"})
	}
	sess.CreateTranscript("Create files", changes)
	require.NoError(t, env.SimulateStop(sess.ID, sess.TranscriptPath))
	if carry {
		git("add", "first.txt")
		git("commit", "--no-gpg-sign", "-m", "Commit first contribution")
		require.NotEmpty(t, env.GetCheckpointIDFromCommitMessage(env.GetHeadHash()))
	}
	state, err := mainEnv.GetSessionState(sess.ID)
	require.NoError(t, err)
	require.Equal(t, session.PhaseIdle, state.Phase)
	require.Empty(t, state.TaskRecords, "content detection must run instead of the agent fast path")
	require.Equal(t, []string{"agent.txt"}, state.FilesTouched)

	wantContent := content
	if replaced {
		git("add", "agent.txt")
		wantContent = "completely unrelated human replacement\n"
		env.WriteFile("agent.txt", wantContent)
	} else {
		git("add", "-N", "agent.txt")
	}
	switch mode {
	case "staged":
		git("add", "agent.txt")
		git("commit", "--no-gpg-sign", "-m", "Commit new file")
	case "partial":
		env.WriteFile("unrelated.txt", "keep this staged\n")
		git("add", "unrelated.txt")
		git("commit", "--no-gpg-sign", "-m", "Commit new file", "--", "agent.txt")
		require.Equal(t, "unrelated.txt\n", git("diff", "--cached", "--name-only"))
	case "all":
		git("commit", "--no-gpg-sign", "-am", "Commit new file")
	}
	require.Equal(t, wantContent, git("show", "HEAD:agent.txt"))
	cp := env.GetCheckpointIDFromCommitMessage(env.GetHeadHash())
	if replaced {
		require.Empty(t, cp, "replacing the agent's new file must not claim a checkpoint")
		return
	}
	require.NotEmpty(t, cp, "the commit contains the agent's file and must link its idle session")
	ref := "entire/checkpoints/v1"
	if backend == StoreGitRefs {
		ref = checkpointRefName(cp)
	}
	metadataPath := SessionMetadataPath(cp)
	if backend == StoreGitRefs {
		metadataPath = "0/metadata.json"
	}
	metadata := git("show", ref+":"+metadataPath)
	var meta checkpoint.Metadata
	require.NoError(t, json.Unmarshal([]byte(metadata), &meta))
	require.Equal(t, sess.ID, meta.SessionID)
}

func gitWithEntireCommitHooks(t *testing.T, env *TestEnv) func(...string) string {
	t.Helper()
	hooksDir := t.TempDir()
	for _, hook := range []string{"prepare-commit-msg", "post-commit"} {
		binary := "'" + strings.ReplaceAll(filepath.ToSlash(getTestBinary()), "'", "'\"'\"'") + "'"
		script := fmt.Sprintf("#!/bin/sh\nexec %s hooks git %s \"$@\"\n", binary, hook)
		require.NoError(t, os.WriteFile(filepath.Join(hooksDir, hook), []byte(script), 0o755))
	}
	return func(args ...string) string {
		t.Helper()
		args = append([]string{"-c", "core.hooksPath=" + hooksDir}, args...)
		cmd := execx.NonInteractive(t.Context(), "git", args...)
		cmd.Dir = env.RepoDir
		cmd.Env = env.cliEnv()
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, out)
		return string(out)
	}
}
