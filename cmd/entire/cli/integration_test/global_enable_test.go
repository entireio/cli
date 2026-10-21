//go:build integration

package integration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/settings/usersettings"
)

// userTierEnv points the spawned binary at a private user settings file and a
// private Claude config dir, and returns the env to set plus the Claude dir.
// The suite-wide ENTIRE_CONFIG_DIR is shared, so each test gets its own.
func userTierEnv(t *testing.T, userSettings string) (extraEnv []string, claudeDir string) {
	t.Helper()
	entireDir := t.TempDir()
	claudeDir = t.TempDir()
	if err := os.WriteFile(filepath.Join(entireDir, usersettings.FileName), []byte(userSettings), 0o600); err != nil {
		t.Fatal(err)
	}
	return []string{"ENTIRE_CONFIG_DIR=" + entireDir, "CLAUDE_CONFIG_DIR=" + claudeDir}, claudeDir
}

// newNeverEnabledRepo is a feature-branch repo with a commit and no .entire
// settings and no project agent hooks — what `entire enable` never touched.
func newNeverEnabledRepo(t *testing.T) *TestEnv {
	t.Helper()
	env := NewTestEnv(t)
	env.InitRepo()
	env.WriteFile("README.md", "# never enabled")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")
	env.GitCheckoutNewBranch("feature/global")
	return env
}

// runClaudeTurn drives one simulated Claude turn that writes path, then
// commits it through the git hooks.
func runClaudeTurn(t *testing.T, env *TestEnv, path, content string) string {
	t.Helper()
	session := env.NewSession()
	if err := env.SimulateUserPromptSubmitWithPrompt(session.ID, "write "+path); err != nil {
		t.Fatalf("user-prompt-submit: %v", err)
	}
	env.WriteFile(path, content)
	session.CreateTranscript("write "+path, []FileChange{{Path: path, Content: content}})
	if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("stop: %v", err)
	}
	env.GitCommitWithHooks("Add "+path, path)
	return session.ID
}

// The whole design: with `preferences.enabled: true` in the user settings file,
// a repository nobody ran `entire enable` in is tracked like an enabled one —
// the commit is linked to a checkpoint carrying the session, and a push
// carries the checkpoint. No project settings or project hooks are written.
func TestGlobalEnable_NeverEnabledRepoIsTrackedAndPushed(t *testing.T) {
	t.Parallel()
	env := newNeverEnabledRepo(t)
	bare := env.SetupBareRemote()
	extra, claudeDir := userTierEnv(t, `{"preferences": {"enabled": true}}`)
	env.ExtraEnv = extra

	// Any foreground command installs the user-level hooks; hook calls don't.
	env.RunCLI("version")
	userHooks, err := os.ReadFile(filepath.Join(claudeDir, "settings.json"))
	if err != nil || !strings.Contains(string(userHooks), "entire hooks claude-code stop") {
		t.Errorf("entire version did not install user-level Claude hooks (err=%v):\n%s", err, userHooks)
	}

	sessionID := runClaudeTurn(t, env, "feature.go", "package feature\n")

	cpID := env.GetCheckpointIDFromCommitMessage(env.GetHeadHash())
	if cpID == "" {
		t.Fatalf("commit not linked to a checkpoint:\n%s", env.GetCommitMessage(env.GetHeadHash()))
	}
	infos := assertCheckpointCount(t, env, 1, "one turn, one commit")
	if infos[0].CheckpointID.String() != cpID {
		t.Errorf("stored checkpoint %s, trailer %s", infos[0].CheckpointID, cpID)
	}
	state, err := env.GetSessionState(sessionID)
	if err != nil || state == nil {
		t.Fatalf("session %s not recorded (err=%v)", sessionID, err)
	}

	for _, rel := range []string{".entire/settings.json", ".entire/settings.local.json", ".claude/settings.json"} {
		if env.FileExists(rel) {
			t.Errorf("%s written into a never-enabled repo", rel)
		}
	}
	env.GitPushWithHooks("origin", "feature/global")
	out := gitOutput(t, bare, "for-each-ref", "--format=%(refname)")
	t.Logf("remote refs after push:\n%s", out)
	if !strings.Contains(out, "refs/heads/entire/checkpoints/v1") && !strings.Contains(out, "refs/entire/") {
		t.Errorf("push carried no checkpoint refs; remote has:\n%s", out)
	}
}

// Control: without the flag the same repository stays untracked.
func TestGlobalEnable_FlagOffLeavesNeverEnabledRepoUntracked(t *testing.T) {
	t.Parallel()
	env := newNeverEnabledRepo(t)
	extra, claudeDir := userTierEnv(t, `{"preferences": {"telemetry": false}}`)
	env.ExtraEnv = extra

	runClaudeTurn(t, env, "feature.go", "package feature\n")

	if cpID := env.GetCheckpointIDFromCommitMessage(env.GetHeadHash()); cpID != "" {
		t.Errorf("commit linked to checkpoint %s with the flag off", cpID)
	}
	if _, err := os.Stat(filepath.Join(claudeDir, "settings.json")); !os.IsNotExist(err) {
		t.Errorf("user-level Claude settings touched with the flag off (err=%v)", err)
	}
}

// An enabled repository with the flag also on has the same hooks in the
// project and the user file. Claude Code runs an identical command once, but
// if both copies ever did fire, Entire must still record one session and one
// checkpoint: every hook below is delivered twice.
func TestGlobalEnable_EnabledRepoToleratesEveryHookTwice(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	extra, _ := userTierEnv(t, `{"preferences": {"enabled": true}}`)
	env.ExtraEnv = extra

	session := env.NewSession()
	twice := func(name string, fn func() error) {
		t.Helper()
		for i := range 2 {
			if err := fn(); err != nil {
				t.Fatalf("%s #%d: %v", name, i+1, err)
			}
		}
	}
	twice("session-start", func() error { return env.SimulateSessionStartWithOutput(session.ID).Err })
	twice("user-prompt-submit", func() error { return env.SimulateUserPromptSubmitWithPrompt(session.ID, "write a.go") })
	env.WriteFile("a.go", "package a\n")
	session.CreateTranscript("write a.go", []FileChange{{Path: "a.go", Content: "package a\n"}})
	twice("stop", func() error { return env.SimulateStop(session.ID, session.TranscriptPath) })
	env.GitCommitWithHooks("Add a.go", "a.go")

	infos := assertCheckpointCount(t, env, 1, "duplicate hooks")
	summary := readCheckpointSummary(t, env, infos[0].CheckpointID.String())
	if len(summary.Sessions) != 1 {
		t.Errorf("checkpoint has %d sessions, want 1", len(summary.Sessions))
	}
	env.AssertCheckpointContainsSession(t, summary, session.ID)
}
