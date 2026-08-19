package review

import (
	"bytes"
	"context"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestReviewTargetChildArgsUsesParsedCommandFlags(t *testing.T) {
	t.Parallel()

	cmd := &cobra.Command{}
	cmd.Flags().String("target", "", "")
	cmd.Flags().Bool("cleanup-worktree", false, "")
	cmd.Flags().String("base", "", "")
	cmd.Flags().String("prompt", "", "")
	if err := cmd.Flags().Parse([]string{"--target=feature/x", "--cleanup-worktree", "--base=main", "--prompt=focus here"}); err != nil {
		t.Fatalf("parse flags: %v", err)
	}
	want := []string{"review", "general", "--base=main", "--prompt=focus here"}
	if got := reviewTargetChildArgs(cmd, []string{"general"}); !reflect.DeepEqual(got, want) {
		t.Fatalf("reviewTargetChildArgs() = %q, want %q", got, want)
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
