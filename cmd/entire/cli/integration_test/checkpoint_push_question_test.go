//go:build integration

package integration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/execx"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/stretchr/testify/require"
)

const opfDeprecationNotice = "OPF is deprecated and will be removed. To upload only the checkpoints you choose, run `entire configure --checkpoint-push-policy manual`."

// gitPushAsTerminal pushes HEAD through the real pre-push hook with
// ENTIRE_TEST_TTY set to tty ("1" is a person at a terminal). Under the test
// override the question is logged but not read, and counts as "not now".
func gitPushAsTerminal(t *testing.T, env *TestEnv, tty string) string {
	t.Helper()
	env.InstallRealPrePushHook()
	cmd := execx.NonInteractive(t.Context(), "git", "push", "origin", "HEAD")
	cmd.Dir = env.RepoDir
	cmd.Env = append(env.cliEnv(), "ENTIRE_TEST_TTY="+tty)
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "the code push must succeed: %s", output)
	return string(output)
}

// checkpointPushQuestionsAsked counts the hook's log lines for the question.
func checkpointPushQuestionsAsked(t *testing.T, env *TestEnv) int {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(env.RepoDir, ".entire", "logs", "entire.log"))
	if os.IsNotExist(err) {
		return 0
	}
	require.NoError(t, err)
	return strings.Count(string(data), strategy.CheckpointPushQuestionLogMsg)
}

func TestCheckpointPushQuestion_AskedOnTerminalPushWithCheckpointsWaiting(t *testing.T) {
	t.Parallel()
	env := newOutgoingEnv(t)
	bareDir := env.SetupBareRemote()
	work := createCheckpointedCommit(t, env, "Some work", "work.go", "package work", "Some work")

	gitPushAsTerminal(t, env, "1")

	require.Equal(t, 1, checkpointPushQuestionsAsked(t, env))
	// "Not now" saves nothing and behaves as today.
	require.NoFileExists(t, gitCommonDirFile(t, env, settings.CheckpointPushPolicyFile))
	require.NoFileExists(t, gitCommonDirFile(t, env, settings.ClonePreferencesFile))
	require.Equal(t, []string{work}, remoteCheckpointIDs(t, bareDir))
}

func TestCheckpointPushQuestion_NotAsked(t *testing.T) {
	t.Parallel()
	inFuture := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	for _, tc := range []struct {
		name string
		tty  string
		// policy is the stored checkpoint push policy, if any.
		policy string
		// prefs is the clone preferences file content, if any.
		prefs string
		// noCheckpoint pushes a plain commit, leaving nothing waiting.
		noCheckpoint bool
	}{
		{name: "no terminal", tty: "0"},
		{name: "automatic chosen", tty: "1", policy: "automatic"},
		{name: "manual chosen", tty: "1", policy: "manual"},
		{name: "unreadable policy", tty: "1", policy: "stricter"},
		{name: "backing off", tty: "1", prefs: `{"checkpoint_push_question_backoff_until":"` + inFuture + `"}`},
		{name: "nothing waiting", tty: "1", noCheckpoint: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := newOutgoingEnv(t)
			env.SetupBareRemote()
			if tc.policy != "" {
				setCheckpointPushPolicy(t, env, tc.policy)
			}
			if tc.prefs != "" {
				writeGitCommonDirFile(t, env, settings.ClonePreferencesFile, tc.prefs)
			}
			if tc.noCheckpoint {
				env.WriteFile("plain.go", "package plain")
				env.GitAdd("plain.go")
				env.GitCommit("Plain commit")
			} else {
				createCheckpointedCommit(t, env, "Some work", "work.go", "package work", "Some work")
			}

			gitPushAsTerminal(t, env, tc.tty)

			require.Zero(t, checkpointPushQuestionsAsked(t, env))
		})
	}
}

func TestCheckpointPushQuestion_BackoffOverAsksAgain(t *testing.T) {
	t.Parallel()
	env := newOutgoingEnv(t)
	env.SetupBareRemote()
	inPast := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	writeGitCommonDirFile(t, env, settings.ClonePreferencesFile, `{"checkpoint_push_question_backoff_until":"`+inPast+`"}`)
	createCheckpointedCommit(t, env, "Some work", "work.go", "package work", "Some work")

	gitPushAsTerminal(t, env, "1")

	require.Equal(t, 1, checkpointPushQuestionsAsked(t, env))
}

// OPF can ask its own question on a push, so OPF users get a one-time
// deprecation line instead of the policy question.
func TestCheckpointPushQuestion_OPFUsersGetDeprecationLineOnce(t *testing.T) {
	t.Parallel()
	env := newOutgoingEnv(t)
	bareDir := env.SetupBareRemote()
	local, err := jsonutil.MarshalIndentWithNewline(map[string]any{
		"redaction": map[string]any{
			"openai_privacy_filter": map[string]any{
				"enabled":        true,
				"prompt_default": "never",
				"categories":     map[string]any{"private_person": true},
			},
		},
	}, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(env.RepoDir, ".entire", "settings.local.json"), local, 0o644))

	first := createCheckpointedCommit(t, env, "First", "first.go", "package first", "First")
	output := gitPushAsTerminal(t, env, "1")
	require.Contains(t, output, opfDeprecationNotice)
	require.Zero(t, checkpointPushQuestionsAsked(t, env))
	require.Equal(t, []string{first}, remoteCheckpointIDs(t, bareDir), "the line changes nothing about the upload")

	createCheckpointedCommit(t, env, "Second", "second.go", "package second", "Second")
	output = gitPushAsTerminal(t, env, "1")
	require.NotContains(t, output, "OPF is deprecated")
	require.Zero(t, checkpointPushQuestionsAsked(t, env))
}
