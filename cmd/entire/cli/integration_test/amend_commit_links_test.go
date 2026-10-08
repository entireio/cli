//go:build integration

package integration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/execx"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

func TestAmendCommitLinks_RealGitHooks(t *testing.T) {
	t.Parallel()
	ForEachBackend(t, func(t *testing.T, backend string) {
		for _, mode := range []string{"message", "file", "new work", "soft reset fold"} {
			t.Run(mode, func(t *testing.T) {
				t.Parallel()
				env := NewFeatureBranchEnv(t)
				env.CheckpointStore = backend
				sess := env.NewSession()
				turn := func(prompt, file, content string) {
					require.NoError(t, env.SimulateUserPromptSubmitWithPromptAndTranscriptPath(sess.ID, prompt, sess.TranscriptPath))
					env.WriteFile(file, content)
					sess.CreateTranscript(prompt, []FileChange{{Path: file, Content: content}})
					require.NoError(t, env.SimulateStop(sess.ID, sess.TranscriptPath))
				}
				turn("first", "f1.txt", "one\n")
				env.GitCommitWithShadowHooks("first", "f1.txt")
				first := env.LatestCheckpointID()
				before := env.RunCLI("checkpoint", "explain", "--checkpoint", first, "--raw-transcript")
				ids := []string{first}
				if mode == "new work" || mode == "soft reset fold" {
					turn("second", "f2.txt", "two\n")
					if mode == "soft reset fold" {
						env.GitCommitWithShadowHooks("second", "f2.txt")
						ids = append(ids, env.LatestCheckpointID())
						testutil.RunGit(t, env.RepoDir, "reset", "--soft", "HEAD~1")
					} else {
						env.GitAdd("f2.txt")
					}
				}

				// Pin the shared test binary in real Git hooks; never use an installed
				// entire from PATH. Git supplies the actual -m/-F source and rewrite pair.
				binary := "'" + strings.ReplaceAll(filepath.ToSlash(getTestBinary()), "'", "'\\''") + "'"
				for _, hook := range []string{"prepare-commit-msg", "post-commit", "post-rewrite"} {
					name := filepath.Join(".git", "hooks", hook)
					env.WriteFile(name, "#!/bin/sh\nexec "+binary+" hooks git "+hook+" \"$@\"\n")
					require.NoError(t, os.Chmod(filepath.Join(env.RepoDir, name), 0o755))
				}
				args := []string{"commit", "--amend", "-m", "renamed"}
				if mode == "file" {
					env.WriteFile("message.txt", "renamed from file\n")
					args = []string{"commit", "--amend", "-F", "message.txt"}
				}
				cmd := execx.NonInteractive(t.Context(), "git", args...)
				cmd.Dir = env.RepoDir
				cmd.Env = env.gitHookEnv()
				out, err := cmd.CombinedOutput()
				require.NoError(t, err, "%s", out)
				head := env.GetHeadHash()
				if mode == "new work" {
					fresh := env.GetCheckpointIDFromCommitMessage(head)
					require.NotEmpty(t, fresh)
					require.NotEqual(t, first, fresh)
					ids = append(ids, fresh)
				}
				explained := env.RunCLI("checkpoint", "explain", "--commit", head)
				listed := env.RunCLI("checkpoint", "list", "--json")
				for _, cid := range ids {
					require.Contains(t, explained, cid, "explain must union trailers and recorded links")
					require.Contains(t, listed, cid, "branch listing must retain amended checkpoints")
				}
				require.Equal(t, before, env.RunCLI("checkpoint", "explain", "--checkpoint", first, "--raw-transcript"), "repair must not re-condense the old transcript")
				ref, path := "entire/checkpoints/v1", CheckpointSummaryPath(first)
				if env.usingGitRefs() {
					ref, path = checkpointRefName(first), "metadata.json"
				}
				var summary checkpoint.CheckpointSummary
				require.NoError(t, json.Unmarshal([]byte(testutil.RunGit(t, env.RepoDir, "show", ref+":"+path)), &summary))
				require.Contains(t, summary.LinkedCommits, checkpoint.LinkedCommit{SHA: head})
				require.Equal(t, head, env.GetHeadHash(), "readers and repair must not rewrite HEAD")
			})
		}
	})
}
