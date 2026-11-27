//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/review"
	"github.com/entireio/cli/cmd/entire/cli/settings/usersettings"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// userTierEnv points the spawned binary at a private user settings file and a
// private Claude config dir, and returns the env to set plus the Claude dir.
// The suite-wide ENTIRE_CONFIG_DIR is shared, so each test gets its own. Codex,
// Pi and OpenCode homes point at directories that do not exist, so global
// enable never reaches the developer's real agent config.
func userTierEnv(t *testing.T, userSettings string) (extraEnv []string, claudeDir string) {
	t.Helper()
	entireDir := t.TempDir()
	claudeDir = t.TempDir()
	if err := os.WriteFile(filepath.Join(entireDir, usersettings.FileName), []byte(userSettings), 0o600); err != nil {
		t.Fatal(err)
	}
	absent := filepath.Join(t.TempDir(), "absent")
	return []string{
		"ENTIRE_CONFIG_DIR=" + entireDir, "CLAUDE_CONFIG_DIR=" + claudeDir,
		"CODEX_HOME=" + filepath.Join(absent, "codex"),
		"PI_CODING_AGENT_DIR=" + filepath.Join(absent, "pi"),
		"XDG_CONFIG_HOME=" + filepath.Join(absent, "xdg"),
	}, claudeDir
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

// The whole design: with `global_enable: true` in the user settings file,
// a repository nobody ran `entire enable` in is tracked like an enabled one —
// the commit is linked to a checkpoint carrying the session. No project
// settings or project hooks are written, and the push carries the checkpoint
// like any enabled repository's.
func TestGlobalEnable_NeverEnabledRepoIsTracked(t *testing.T) {
	t.Parallel()
	env := newNeverEnabledRepo(t)
	bare := env.SetupBareRemote()
	extra, claudeDir := userTierEnv(t, `{"global_enable": true}`)
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
	if refs := remoteCheckpointRefNames(t, bare); refs == "" {
		t.Error("push carried no checkpoint refs")
	}
}

// remoteCheckpointRefNames lists the checkpoint refs on bare, from either
// backend: the v1 branch and per-checkpoint refs.
func remoteCheckpointRefNames(t *testing.T, bare string) string {
	t.Helper()
	return gitOutput(t, bare, "for-each-ref", "--format=%(refname)", "refs/heads/entire/", "refs/entire/")
}

// agentFreeEnv clears every variable that marks an agent caller, so the
// developer's own session (the suite usually runs inside one) cannot leak in.
func agentFreeEnv() []string {
	var env []string
	for _, name := range review.AgentCallerEnvVars() {
		env = append(env, name+"=")
	}
	return env
}

// Checkpoints from a repository on only through global enable push on both
// backends like an enabled repository's, with no question, and a push from
// another worktree of the clone carries new ones. Only the first push from the
// clone says it went through global enable.
func TestGlobalEnable_PushesLikeAnEnabledRepo(t *testing.T) {
	t.Parallel()
	for name, store := range map[string]string{"git-branch": "", "git-refs": StoreGitRefs} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env := newNeverEnabledRepo(t)
			env.CheckpointStore = store
			bare := env.SetupBareRemote()
			extra, _ := userTierEnv(t, `{"global_enable": true}`)
			env.ExtraEnv = slices.Concat(extra, agentFreeEnv())

			runClaudeTurn(t, env, "a.go", "package a\n")
			const notice = "This repository is on through global enable"
			if out := gitPushWithHooksIn(t, env, env.RepoDir); !strings.Contains(out, notice) {
				t.Errorf("first push printed no global enable notice:\n%s", out)
			}
			first := remoteCheckpointRefNames(t, bare)
			if first == "" {
				t.Fatal("push carried no checkpoint refs")
			}

			linked := filepath.Join(t.TempDir(), "linked")
			testutil.RunGit(t, env.RepoDir, "worktree", "add", "-b", "linked-branch", linked)
			runClaudeTurn(t, env, "b.go", "package b\n")
			if out := gitPushWithHooksIn(t, env, linked); strings.Contains(out, notice) {
				t.Errorf("second push repeated the global enable notice:\n%s", out)
			}
			if after := remoteCheckpointRefNames(t, bare); after == first && store == StoreGitRefs {
				t.Errorf("push from another worktree carried no new checkpoint ref:\n%s", after)
			}
			if store == "" {
				local := gitOutput(t, env.RepoDir, "rev-parse", "refs/heads/entire/checkpoints/v1")
				if remote := gitOutput(t, bare, "rev-parse", "refs/heads/entire/checkpoints/v1"); remote != local {
					t.Errorf("remote v1 %s, local %s: the second checkpoint did not go", remote, local)
				}
			}
		})
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
	extra, _ := userTierEnv(t, `{"global_enable": true}`)
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

// Global enable turns on repositories nobody configured, not ones turned off:
// a committed enabled:false stays a floor. Nothing is tracked, no git hooks
// are installed, and no .entire/.gitignore is written.
func TestGlobalEnable_DisabledRepoStaysUntracked(t *testing.T) {
	t.Parallel()
	env := newNeverEnabledRepo(t)
	extra, _ := userTierEnv(t, `{"global_enable": true}`)
	env.ExtraEnv = extra
	env.WriteFile(".entire/settings.json", `{"enabled": false}`)
	env.GitAdd(".entire/settings.json")
	env.GitCommit("Turn Entire off for this repository")

	runClaudeTurn(t, env, "feature.go", "package feature\n")

	if cpID := env.GetCheckpointIDFromCommitMessage(env.GetHeadHash()); cpID != "" {
		t.Errorf("commit linked to checkpoint %s in a disabled repository", cpID)
	}
	if state := strategy.CheckGitHookStateInDir(t.Context(), env.RepoDir); state != strategy.GitHooksAbsent {
		t.Errorf("Entire git hooks installed in a disabled repository (state = %v)", state)
	}
	if env.FileExists(".entire/.gitignore") {
		t.Error(".entire/.gitignore written in a disabled repository")
	}
}

// A repository on only through global enable gets Entire's git hooks on the
// first prompt, without anyone running `entire enable` there. The SessionStart
// banner says so (Claude shows it; it hides a successful hook's stderr), and
// the prompt that installs them says so on stderr for agents that show it.
// Neither appears once the hooks are in, nor in a repository the developer
// enabled.
func TestGlobalEnable_SaysWhenItInstallsGitHooks(t *testing.T) {
	t.Parallel()
	const banner = "through global enable (you have not run `entire enable` here)"
	const installLine = "Installed Entire's git hooks in"
	for name, tc := range map[string]struct {
		claim bool
		want  bool
	}{
		"global only": {want: true},
		"claimed":     {claim: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env := newNeverEnabledRepo(t)
			extra, _ := userTierEnv(t, `{"global_enable": true}`)
			env.ExtraEnv = extra
			if tc.claim {
				env.WriteFile(".entire/settings.local.json", `{"enabled": true}`)
			}
			runner := env.newHookRunner()
			session := env.NewSession()

			start := env.SimulateSessionStartWithOutput(session.ID)
			if start.Err != nil {
				t.Fatalf("session-start: %v\n%s", start.Err, start.Stderr)
			}
			if got := strings.Contains(string(start.Stdout), banner); got != tc.want {
				t.Errorf("session-start banner mentions global enable = %v, want %v:\n%s", got, tc.want, start.Stdout)
			}

			prompt := func() HookOutput {
				t.Helper()
				input, err := json.Marshal(map[string]string{"session_id": session.ID, "transcript_path": "", "prompt": "hi"})
				if err != nil {
					t.Fatal(err)
				}
				out := runner.runHookWithOutput("user-prompt-submit", input)
				if out.Err != nil {
					t.Fatalf("user-prompt-submit: %v\n%s", out.Err, out.Stderr)
				}
				if len(bytes.TrimSpace(out.Stdout)) > 0 && !json.Valid(out.Stdout) {
					t.Errorf("user-prompt-submit stdout is not valid JSON:\n%s", out.Stdout)
				}
				return out
			}
			first := prompt()
			if got := strings.Contains(string(first.Stderr), installLine); got != tc.want {
				t.Errorf("first prompt reports the install = %v, want %v:\n%s", got, tc.want, first.Stderr)
			}
			if second := prompt(); strings.Contains(string(second.Stderr), installLine) {
				t.Errorf("second prompt repeated the install line:\n%s", second.Stderr)
			}
			if again := env.SimulateSessionStartWithOutput(env.NewSession().ID); strings.Contains(string(again.Stdout), banner) {
				t.Errorf("a later session repeated the banner once the hooks are in:\n%s", again.Stdout)
			}
		})
	}
}
