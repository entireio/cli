//go:build integration

package integration

import (
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// TestEnableDefault_GitRefsRoundTrip pins the shipped first-run default end
// to end WITHOUT the ENTIRE_CHECKPOINTS_PRIMARY override every other
// backend-aware suite uses: a fresh `enable` writes the git-refs primary
// into settings.json, and that settings block alone drives checkpoint
// storage — condensation lands on per-checkpoint refs, not the v1 branch.
// Without this, a regression in the enable→settings→git-refs chain would
// pass the entire env-driven integration and e2e suites.
func TestEnableDefault_GitRefsRoundTrip(t *testing.T) {
	t.Parallel()

	env := NewTestEnv(t)
	testutil.InitRepo(t, env.RepoDir)
	env.WriteFile("README.md", "# fresh repo\n")
	gitOutput(t, env.RepoDir, "add", "README.md")
	gitOutput(t, env.RepoDir, "commit", "-m", "initial commit")

	env.RunCLI("enable", "--agent", "claude-code", "--telemetry=false")

	settings := env.ReadFile(".entire/settings.json")
	if !strings.Contains(settings, `"git-refs"`) {
		t.Fatalf("first-run enable should write the git-refs primary into settings.json, got:\n%s", settings)
	}

	initialHead := gitOutput(t, env.RepoDir, "rev-parse", "HEAD")
	sess := env.NewSession()
	prompt := "Create a file"
	if err := env.SimulateUserPromptSubmitWithPromptAndTranscriptPath(sess.ID, prompt, sess.TranscriptPath); err != nil {
		t.Fatalf("user-prompt-submit failed: %v", err)
	}
	const content = "package main\n\nfunc main() {}\n"
	env.WriteFile("main.go", content)
	sess.CreateTranscript(prompt, []FileChange{{Path: "main.go", Content: content}})
	if err := env.SimulateStop(sess.ID, sess.TranscriptPath); err != nil {
		t.Fatalf("stop hook failed: %v", err)
	}
	_ = initialHead
	env.GitCommitWithShadowHooks("Add main", "main.go")

	refs := gitOutput(t, env.RepoDir, "for-each-ref", "refs/entire/checkpoints")
	if refs == "" {
		t.Fatal("expected the condensed checkpoint as a per-checkpoint ref under refs/entire/checkpoints (settings-driven git-refs)")
	}
}
