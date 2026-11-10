//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/review"
	"github.com/entireio/cli/cmd/entire/cli/settings/usersettings"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
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

// The whole design: with `global_enable: true` in the user settings file,
// a repository nobody ran `entire enable` in is tracked like an enabled one —
// the commit is linked to a checkpoint carrying the session. No project
// settings or project hooks are written, and the push holds the checkpoint
// until the user agrees (see TestGlobalEnable_PushesHeldUntilAgreed).
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
	if refs := remoteCheckpointRefNames(t, bare); refs != "" {
		t.Errorf("push carried checkpoint refs the user never agreed to push:\n%s", refs)
	}
}

// remoteCheckpointRefNames lists the checkpoint refs on bare, from either
// backend: the v1 branch and per-checkpoint refs.
func remoteCheckpointRefNames(t *testing.T, bare string) string {
	t.Helper()
	return gitOutput(t, bare, "for-each-ref", "--format=%(refname)", "refs/heads/entire/", "refs/entire/")
}

// userTierFile is the private user settings file userTierEnv points at.
func userTierFile(t *testing.T, extra []string) string {
	t.Helper()
	for _, kv := range extra {
		if dir, ok := strings.CutPrefix(kv, "ENTIRE_CONFIG_DIR="); ok {
			return filepath.Join(dir, usersettings.FileName)
		}
	}
	t.Fatal("no ENTIRE_CONFIG_DIR in the env")
	return ""
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

// Checkpoints from a repository on only through global enable are held on
// both backends until the user agrees for that repository; then the held
// checkpoints go with the next push, and a push from another worktree of the
// clone carries new ones without asking again.
func TestGlobalEnable_PushesHeldUntilAgreed(t *testing.T) {
	t.Parallel()
	for name, store := range map[string]string{"git-branch": "", "git-refs": StoreGitRefs} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env := newNeverEnabledRepo(t)
			env.CheckpointStore = store
			bare := env.SetupBareRemote()
			extra, _ := userTierEnv(t, `{"global_enable": true}`)
			env.ExtraEnv = append(extra, agentFreeEnv()...)

			runClaudeTurn(t, env, "a.go", "package a\n")
			held := entireLines(gitPushWithHooksIn(t, env, env.RepoDir))
			// git-branch counts unpushed v1 commits, so the number is approximate.
			if !strings.Contains(held, "Entire is holding ") || !strings.Contains(held, "session checkpoint") {
				t.Errorf("held push does not say so:\n%s", held)
			}
			if strings.Contains(held, bare) {
				t.Errorf("the hold line names the remote's path:\n%s", held)
			}
			if refs := remoteCheckpointRefNames(t, bare); refs != "" {
				t.Fatalf("held push carried checkpoint refs:\n%s", refs)
			}

			// What a yes at the push prompt writes. The bare remote's path
			// names no forge, so the entry is keyed by the worktree's path.
			agreed := `{"global_enable": true, "repos": {` + jsonQuote(env.RepoDir) + `: {"push_checkpoints": true}}}`
			if err := os.WriteFile(userTierFile(t, extra), []byte(agreed), 0o600); err != nil {
				t.Fatal(err)
			}
			if out := gitPushWithHooksIn(t, env, env.RepoDir); strings.Contains(out, "holding") {
				t.Errorf("push after agreeing still holds:\n%s", out)
			}
			first := remoteCheckpointRefNames(t, bare)
			if first == "" {
				t.Fatal("push after agreeing carried no checkpoint refs")
			}

			linked := filepath.Join(t.TempDir(), "linked")
			testutil.RunGit(t, env.RepoDir, "worktree", "add", "-b", "linked-branch", linked)
			runClaudeTurn(t, env, "b.go", "package b\n")
			if out := gitPushWithHooksIn(t, env, linked); strings.Contains(out, "holding") {
				t.Errorf("push from another worktree holds:\n%s", out)
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

// entireLines keeps Entire's own lines of a push's output, dropping git's
// (which name the remote's path).
func entireLines(out string) string {
	var kept []string
	for line := range strings.Lines(out) {
		if strings.HasPrefix(line, "[entire]") {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "")
}

func jsonQuote(s string) string {
	data, err := json.Marshal(filepath.ToSlash(s))
	if err != nil {
		panic(err)
	}
	return string(data)
}

// A terminal push from a person asks (here the test override logs the
// question and reads no answer, which holds); the same push from an agent
// session never asks, because an agent's shell can pass the terminal probe
// and its yes is not the user's. Neither push asks the first-push policy
// question on top.
func TestGlobalEnable_HeldPushAsksOnlyAPerson(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		agentEnv []string
		asked    int
	}{
		"person": {asked: 1},
		"agent":  {agentEnv: []string{"CLAUDECODE=1"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env := newNeverEnabledRepo(t)
			env.CheckpointStore = StoreGitRefs
			bare := env.SetupBareRemote()
			extra, _ := userTierEnv(t, `{"global_enable": true}`)
			env.ExtraEnv = append(append(extra, agentFreeEnv()...), tc.agentEnv...)

			runClaudeTurn(t, env, "a.go", "package a\n")
			out := gitPushAsTerminal(t, env, "1")
			if !strings.Contains(out, "holding") {
				t.Errorf("push did not hold:\n%s", out)
			}
			if refs := remoteCheckpointRefNames(t, bare); refs != "" {
				t.Errorf("push carried checkpoint refs:\n%s", refs)
			}
			log := entireLog(t, env)
			if got := strings.Count(log, strategy.GlobalEnablePushConsentLogMsg); got != tc.asked {
				t.Errorf("consent question asked %d times, want %d", got, tc.asked)
			}
			if strings.Contains(log, strategy.CheckpointPushQuestionLogMsg) {
				t.Error("the policy question was asked on a held push")
			}
		})
	}
}

func entireLog(t *testing.T, env *TestEnv) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(env.RepoDir, ".entire", "logs", "entire.log"))
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// `entire checkpoint push` never asks: in a held repository it reports the
// hold and sends nothing.
func TestGlobalEnable_CheckpointPushReportsTheHold(t *testing.T) {
	t.Parallel()
	env := newNeverEnabledRepo(t)
	env.CheckpointStore = StoreGitRefs
	bare := env.SetupBareRemote()
	extra, _ := userTierEnv(t, `{"global_enable": true}`)
	env.ExtraEnv = append(extra, agentFreeEnv()...)
	runClaudeTurn(t, env, "a.go", "package a\n")
	cpID := env.GetCheckpointIDFromCommitMessage(env.GetHeadHash())
	if cpID == "" {
		t.Fatal("commit not linked to a checkpoint")
	}

	_, stderr, err := runCheckpointPush(t, env, true, cpID)
	if err == nil {
		t.Fatal("entire checkpoint push succeeded in a held repository")
	}
	if !strings.Contains(stderr, "on only through global enable") {
		t.Errorf("error does not name the hold:\n%s", stderr)
	}
	if refs := remoteCheckpointRefNames(t, bare); refs != "" {
		t.Errorf("checkpoint push sent refs:\n%s", refs)
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

// A repository the developer claimed pushes its checkpoints with no hold and
// no global-enable notice: the one-time notice speaks only for a push the
// owner list let through without asking, which a local bare remote (no forge
// owner) can never be.
func TestGlobalEnable_ClaimedRepoPushesWithoutNotice(t *testing.T) {
	t.Parallel()
	env := newNeverEnabledRepo(t)
	env.CheckpointStore = StoreGitRefs
	bare := env.SetupBareRemote()
	extra, _ := userTierEnv(t, `{"global_enable": true}`)
	env.ExtraEnv = append(extra, agentFreeEnv()...)
	env.WriteFile(".entire/settings.local.json", `{"enabled": true}`)

	runClaudeTurn(t, env, "a.go", "package a\n")
	out := env.RunPrePushOutput("origin")
	if strings.Contains(out, "holding") || strings.Contains(out, "without asking") {
		t.Errorf("claimed repository's push mentions global enable:\n%s", out)
	}
	if refs := remoteCheckpointRefNames(t, bare); refs == "" {
		t.Error("claimed repository's push carried no checkpoint refs")
	}
	if _, err := os.Stat(filepath.Join(env.RepoDir, ".git", "entire-global-enable-push-notice")); err == nil {
		t.Error("notice marker written for a claimed repository")
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

// Attaching a session to a pushed commit pushes its checkpoint at once, so in
// a held repository attach reports the hold instead: the checkpoint is
// written locally and nothing is sent.
func TestGlobalEnable_AttachReportsTheHold(t *testing.T) {
	t.Parallel()
	env := newNeverEnabledRepo(t)
	bare := env.SetupBareRemote()
	extra, _ := userTierEnv(t, `{"global_enable": true}`)
	env.ExtraEnv = append(extra, agentFreeEnv()...)

	const sessionID = "held-attach"
	tb := NewTranscriptBuilder()
	tb.AddUserMessage("explain the readme")
	tb.AddAssistantMessage("It names the project.")
	if err := tb.WriteToFile(filepath.Join(env.ClaudeProjectDir, sessionID+".jsonl")); err != nil {
		t.Fatal(err)
	}
	out, err := env.RunCLIWithError("session", "attach", sessionID, "-a", agentClaudeCode, "-f", "--commit", "HEAD")
	if err == nil {
		t.Fatalf("attach to a pushed commit succeeded in a held repository:\n%s", out)
	}
	if !strings.Contains(out, "written locally but not pushed") || !strings.Contains(out, "on only through global enable") {
		t.Errorf("attach does not report the hold:\n%s", out)
	}
	if refs := remoteCheckpointRefNames(t, bare); refs != "" {
		t.Errorf("attach sent checkpoint refs:\n%s", refs)
	}
}
