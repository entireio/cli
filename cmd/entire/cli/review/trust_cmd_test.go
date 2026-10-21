package review_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/review"
	reviewtypes "github.com/entireio/cli/cmd/entire/cli/review/types"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// clearAgentCallerEnv removes every variable the trust gate reads as "an agent
// is running this", so the developer's own agent session cannot leak in.
func clearAgentCallerEnv(t *testing.T) {
	t.Helper()
	for _, name := range review.AgentCallerEnvVars() {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
}

// setupForeignBranchRepo creates a repo on branch "feature" whose one commit
// above main was authored by someone else, and a claude-code review profile.
func setupForeignBranchRepo(t *testing.T) (reviewer *captureRunConfigReviewer, deps review.Deps, head string) {
	t.Helper()
	testutil.IsolateGitConfigEnv(t)
	clearAgentCallerEnv(t)
	setupCmdTestRepo(t)
	runGitCmd(t, "branch", "-M", "main")
	runGitCmd(t, "checkout", "-b", "feature")
	testutil.WriteFile(t, ".", "g.txt", "y")
	testutil.GitAdd(t, ".", "g.txt")
	runGitCmd(t, "commit", "-m", "theirs", "--author", "Mallory <mallory@example.com>")
	head = strings.TrimSpace(runGitCmd(t, "rev-parse", "HEAD"))

	if err := seedReviewConfig(context.Background(), map[string]settings.ReviewConfig{
		"claude-code": {Skills: []string{"/review"}},
	}); err != nil {
		t.Fatal(err)
	}
	reviewer = &captureRunConfigReviewer{name: "claude-code"}
	deps = review.Deps{
		GetAgentsWithHooksInstalled: func(context.Context) []types.AgentName {
			return []types.AgentName{"claude-code"}
		},
		NewSilentError:          func(err error) error { return err },
		HeadHasReviewCheckpoint: func(context.Context) (bool, string) { return false, "" },
		ReviewerFor: func(name string) reviewtypes.AgentReviewer {
			if name == "claude-code" {
				return reviewer
			}
			return nil
		},
		InspectTrust: func(_ context.Context, source review.TrustSource, agents []review.TrustAgent) (review.TrustInventory, error) {
			if source.WorktreeRoot == "" || len(agents) != 1 || agents[0].Name != "claude-code" {
				t.Errorf("InspectTrust(%+v, %v): want the current worktree and the profile's agent", source, agents)
			}
			return review.TrustInventory{Entries: []review.TrustEntry{
				{Agent: "claude-code", Kind: review.TrustKindHook, Name: "Stop", Command: "npm test", Source: ".claude/settings.json"},
			}}, nil
		},
	}
	return reviewer, deps, head
}

// The re-run inside a target worktree carries the caller's worktree in its
// environment. That variable alone must not skip the gate.
func TestRunReview_TargetChildEnvAloneDoesNotSkipGate(t *testing.T) {
	reviewer, deps, _ := setupForeignBranchRepo(t)
	t.Setenv("ENTIRE_REVIEW_FINDINGS_WORKTREE", t.TempDir())

	cmd := review.NewCommand(deps)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"general"})
	if err := cmd.Execute(); err == nil || reviewer.called {
		t.Fatalf("env var alone skipped the gate (err=%v, called=%v)", err, reviewer.called)
	}
}

// The env var plus a matching --trust-target is how the target re-run skips
// a second gate. An agent can set both itself, so for an agent caller the
// approval must still be announced rather than skipped silently.
func TestRunReview_TargetChildBypassStillAnnouncesAgentApproval(t *testing.T) {
	reviewer, deps, head := setupForeignBranchRepo(t)
	t.Setenv("ENTIRE_REVIEW_FINDINGS_WORKTREE", t.TempDir())
	t.Setenv("CLAUDE_CODE_SESSION_ID", "11111111-2222-4333-8444-555555555555")

	var stderr bytes.Buffer
	cmd := review.NewCommand(deps)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"general", "--trust-target", head})
	if err := cmd.Execute(); err != nil || !reviewer.called {
		t.Fatalf("approved review did not run (err=%v, called=%v)", err, reviewer.called)
	}
	if !strings.Contains(stderr.String(), "Approved with --trust-target from") {
		t.Errorf("agent approval was not announced; stderr:\n%s", stderr.String())
	}
}

func runGitCmd(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// --base sets the review scope only. Pointing it at the head must not make
// someone else's commits look like an empty, "yours" range.
func TestRunReview_BaseAtHeadDoesNotSkipGate(t *testing.T) {
	reviewer, deps, head := setupForeignBranchRepo(t)

	cmd := review.NewCommand(deps)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"general", "--base", head})
	if err := cmd.Execute(); err == nil || reviewer.called {
		t.Fatalf("--base at head skipped the gate (err=%v, called=%v)", err, reviewer.called)
	}
}

// Gate flags are validated before any mode runs, so they are never silently
// ignored by --list, --configure, and the other modes.
func TestRunReview_GateFlagsValidatedForEveryMode(t *testing.T) {
	_, deps, _ := setupForeignBranchRepo(t)
	for _, args := range [][]string{{"--list", "--json"}, {"--list", "--trust-target", "abc"}} {
		cmd := review.NewCommand(deps)
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs(args)
		if err := cmd.Execute(); err == nil {
			t.Errorf("review %v succeeded; want a flag error", args)
		}
	}
}

// --set-config validates the file and saves to clone-local preferences,
// never the committed settings file.
func TestConfigure_SetConfigSavesToClonePreferences(t *testing.T) {
	_, deps, _ := setupForeignBranchRepo(t)
	cfgFile := filepath.Join(t.TempDir(), "review-config.json")
	if err := os.WriteFile(cfgFile, []byte(`{"settings":{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"/usr/bin/true"}]}]}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) error {
		cmd := review.NewCommand(deps)
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs(args)
		return cmd.Execute()
	}
	if err := run("--configure", "general", "--set-config", "claude-code="+cfgFile); err != nil {
		t.Fatalf("--set-config: %v", err)
	}
	s, err := settings.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cfg := s.ReviewProfiles["general"].Agents["claude-code"].Config
	if cfg == nil || !strings.Contains(string(cfg.Settings), "/usr/bin/true") {
		t.Fatalf("config not applied: %+v", cfg)
	}
	if data, err := os.ReadFile(filepath.Join(".entire", "settings.json")); err == nil && strings.Contains(string(data), `"config"`) {
		t.Fatalf("config written to the committed settings file:\n%s", data)
	}
	// Only the config is stored clone-locally, so a later change to the
	// profile in its own layer still takes effect.
	if err := settings.ModifyClonePreferences(context.Background(), func(p *settings.ClonePreferences) error {
		worker := p.ReviewProfiles["general"].Agents["claude-code"]
		if worker.Config != nil {
			t.Error("the config was copied into the stored profile")
		}
		worker.Model = "sonnet"
		p.ReviewProfiles["general"].Agents["claude-code"] = worker
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	s, err = settings.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := s.ReviewProfiles["general"].Agents["claude-code"]; got.Model != "sonnet" || got.Config == nil {
		t.Fatalf("after editing the profile: model %q, config %v; want sonnet with the config kept", got.Model, got.Config)
	}

	// A command naming a file in this checkout is saved with a warning.
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	inCheckout := filepath.Join(t.TempDir(), "in-checkout.json")
	hook, err := json.Marshal(filepath.Join(wd, "hook.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inCheckout, []byte(`{"settings":{"hooks":{"Stop":[{"hooks":[{"type":"command","command":`+string(hook)+`}]}]}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd := review.NewCommand(deps)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"--configure", "general", "--set-config", "claude-code=" + inCheckout})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("--set-config with a checkout path: %v", err)
	}
	if !strings.Contains(stderr.String(), "inside the checkout") {
		t.Errorf("no warning for a checkout path; stderr:\n%s", stderr.String())
	}

	// none removes the reviewer's config, so it uses the checkout's again.
	if err := run("--configure", "general", "--set-config", "claude-code=none"); err != nil {
		t.Fatalf("--set-config none: %v", err)
	}
	if s, err = settings.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if cfg := s.ReviewProfiles["general"].Agents["claude-code"].Config; cfg != nil {
		t.Fatalf("config still set after none: %+v", cfg)
	}
}

// A tag or local branch named like the default branch (fetched along with
// someone's commits) must not make their branch count as the default branch.
func TestRunReview_RefNamedLikeDefaultBranchDoesNotSkipGate(t *testing.T) {
	for _, spoof := range [][]string{
		{"tag", "origin/HEAD", "HEAD"},
		{"tag", "origin/main", "HEAD"},
		{"branch", "origin/main", "HEAD"},
	} {
		t.Run(spoof[0]+" "+spoof[1], func(t *testing.T) {
			reviewer, deps, _ := setupForeignBranchRepo(t)
			runGitCmd(t, spoof...)

			cmd := review.NewCommand(deps)
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&bytes.Buffer{})
			cmd.SetArgs([]string{"general"})
			if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "review needs approval") || reviewer.called {
				t.Fatalf("%s named %s skipped the gate (err=%v, called=%v)", spoof[0], spoof[1], err, reviewer.called)
			}
		})
	}
}
