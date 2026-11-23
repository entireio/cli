package review

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/spf13/cobra"
)

func TestReviewTargetChildArgsUsesParsedCommandFlags(t *testing.T) {
	t.Parallel()

	cmd := &cobra.Command{}
	cmd.Flags().String("target", "", "")
	cmd.Flags().Bool("cleanup-worktree", false, "")
	cmd.Flags().String("base", "", "")
	cmd.Flags().String("prompt", "", "")
	cmd.Flags().String("trust-target", "", "")
	cmd.Flags().Bool("show-config", false, "")
	if err := cmd.Flags().Parse([]string{"--target=feature/x", "--cleanup-worktree", "--base=main", "--prompt=focus here", "--trust-target=abc1234"}); err != nil {
		t.Fatalf("parse flags: %v", err)
	}
	head := strings.Repeat("a", 40)
	// --prompt travels in the child's environment, not its argv.
	want := []string{"review", "general", "--base=main", "--trust-target=" + head}
	if got := reviewTargetChildArgs(cmd, []string{"general"}, head, ""); !reflect.DeepEqual(got, want) {
		t.Fatalf("reviewTargetChildArgs() = %q, want %q", got, want)
	}
	wantEnv := []string{envReviewFindingsWorktree + "=/caller", envReviewTargetPrompt + "=focus here"}
	if got := reviewTargetChildEnv(cmd, "/caller", reviewProfileSelection{}); !reflect.DeepEqual(got, wantEnv) {
		t.Fatalf("reviewTargetChildEnv() = %q, want %q", got, wantEnv)
	}
}

func TestReviewTargetChildEnvOmitsUnsetPrompt(t *testing.T) {
	t.Parallel()

	cmd := &cobra.Command{}
	cmd.Flags().String("prompt", "", "")
	want := []string{envReviewFindingsWorktree + "=/caller"}
	if got := reviewTargetChildEnv(cmd, "/caller", reviewProfileSelection{}); !reflect.DeepEqual(got, want) {
		t.Fatalf("reviewTargetChildEnv() = %q, want %q", got, want)
	}
}

// A profile the parent used unsaved travels to the re-run as JSON; a saved
// one is not passed, since the re-run reads it from the branch's settings.
func TestReviewTargetChildEnvPassesTheUnsavedProfile(t *testing.T) {
	t.Parallel()

	cmd := &cobra.Command{}
	cmd.Flags().String("prompt", "", "")
	profile := settings.ReviewProfileConfig{Agents: map[string]settings.ReviewConfig{"claude-code": {Prompt: "review"}}}
	raw, err := json.Marshal(targetChildProfile{Name: "general", Profile: profile})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{envReviewFindingsWorktree + "=/caller", envReviewTargetProfile + "=" + string(raw)}
	if got := reviewTargetChildEnv(cmd, "/caller", reviewProfileSelection{name: "general", profile: profile, unsaved: &profile}); !reflect.DeepEqual(got, want) {
		t.Fatalf("reviewTargetChildEnv() = %q, want %q", got, want)
	}
	saved := []string{envReviewFindingsWorktree + "=/caller"}
	if got := reviewTargetChildEnv(cmd, "/caller", reviewProfileSelection{name: "general", profile: profile}); !reflect.DeepEqual(got, saved) {
		t.Fatalf("reviewTargetChildEnv() with a saved profile = %q, want %q", got, saved)
	}
}

func TestTakeTargetChildPrompt(t *testing.T) {
	t.Setenv(envReviewFindingsWorktree, "/caller")
	t.Setenv(envReviewTargetPrompt, "focus here")
	if got := takeTargetChildPrompt(); got != "focus here" {
		t.Fatalf("takeTargetChildPrompt() = %q, want %q", got, "focus here")
	}
	// Unset so the reviewer agents the child starts don't inherit it.
	if _, ok := os.LookupEnv(envReviewTargetPrompt); ok {
		t.Fatalf("%s still set after takeTargetChildPrompt", envReviewTargetPrompt)
	}
}

func TestTakeTargetChildPromptIgnoredOutsideTargetChild(t *testing.T) {
	t.Setenv(envReviewFindingsWorktree, "")
	t.Setenv(envReviewTargetPrompt, "injected")
	if got := takeTargetChildPrompt(); got != "" {
		t.Fatalf("takeTargetChildPrompt() = %q outside a target child, want empty", got)
	}
}

func TestReviewManifestWorktreePathUsesCallerWorktree(t *testing.T) {
	t.Setenv(envReviewFindingsWorktree, "/caller")
	if got := reviewManifestWorktreePath("/target"); got != "/caller" {
		t.Fatalf("reviewManifestWorktreePath() = %q, want /caller", got)
	}
}

func TestFinishTargetReviewExplicitCleanup(t *testing.T) {
	t.Parallel()

	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	removed := ""
	err := finishTargetReview(t.Context(), cmd, TargetWorktree{Path: "/tmp/review", Created: true}, true, func(_ context.Context, path string) error {
		removed = path
		return nil
	})
	if err != nil {
		t.Fatalf("finishTargetReview: %v", err)
	}
	if removed != "/tmp/review" {
		t.Fatalf("removed = %q, want /tmp/review", removed)
	}
	if !strings.Contains(out.String(), "Removed temporary review worktree") {
		t.Fatalf("output = %q", out.String())
	}
}

func TestFinishTargetReviewNeverRemovesReusedWorktree(t *testing.T) {
	t.Parallel()

	cmd := &cobra.Command{}
	cmd.SetOut(io.Discard)
	called := false
	err := finishTargetReview(t.Context(), cmd, TargetWorktree{Path: "/tmp/reused", Created: false}, true, func(context.Context, string) error {
		called = true
		return nil
	})
	if err != nil {
		t.Fatalf("finishTargetReview: %v", err)
	}
	if called {
		t.Fatal("remove callback called for reused worktree")
	}
}

func TestRunReviewInWorktreeUsesInjectedRunner(t *testing.T) {
	t.Parallel()

	var gotRoot string
	var gotArgs []string
	var gotEnv []string
	runner := func(_ context.Context, root string, args, env []string, _ io.Reader, _, _ io.Writer) error {
		gotRoot = root
		gotArgs = append([]string(nil), args...)
		gotEnv = append([]string(nil), env...)
		return nil
	}
	wantEnv := []string{envReviewFindingsWorktree + "=/caller"}
	if err := runReviewInWorktree(t.Context(), runner, "/tmp/review", []string{"review", "general"}, wantEnv, bytes.NewReader(nil), io.Discard, io.Discard); err != nil {
		t.Fatalf("runReviewInWorktree: %v", err)
	}
	if gotRoot != "/tmp/review" || !reflect.DeepEqual(gotArgs, []string{"review", "general"}) || !reflect.DeepEqual(gotEnv, wantEnv) {
		t.Fatalf("runner got root=%q args=%q env=%q", gotRoot, gotArgs, gotEnv)
	}
}
