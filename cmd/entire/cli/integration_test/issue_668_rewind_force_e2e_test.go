//go:build integration && unix

package integration

import (
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestIssue668_RewindCheckout_DiscardsUncommittedChanges is a full-flow
// reproduction of #668: `entire rewind` → "Checkout commit (detached HEAD)"
// warns "Any uncommitted changes will be lost!" and then ran a plain
// `git checkout`, which aborts when a tracked file has local modifications. The
// warning promised the changes would be discarded, but the checkout failed
// instead. The fix routes that checkout through CheckoutBranchForce
// (git checkout --force).
//
// It drives the real interactive rewind binary end-to-end (accessible mode over
// a pty): two committed checkpoints, a dirty tracked file, then rewind to the
// earlier checkpoint via the checkout option. With the fix the checkout succeeds
// and the dirty change is discarded, leaving a detached HEAD at the target.
func TestIssue668_RewindCheckout_DiscardsUncommittedChanges(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)

	// Checkpoint A (the rewind target): tracked.txt = "v1".
	sessionA := env.NewSession()
	if err := env.SimulateUserPromptSubmitWithPrompt(sessionA.ID, "add tracked file"); err != nil {
		t.Fatalf("user-prompt-submit A: %v", err)
	}
	env.WriteFile("tracked.txt", "v1\n")
	sessionA.CreateTranscript("add tracked file", []FileChange{{Path: "tracked.txt", Content: "v1\n"}})
	if err := env.SimulateStop(sessionA.ID, sessionA.TranscriptPath); err != nil {
		t.Fatalf("stop A: %v", err)
	}
	env.GitCommitWithShadowHooksAsAgent("add tracked", "tracked.txt")
	commitA := env.GetHeadHash()
	if env.GetCheckpointIDFromCommitMessage(commitA) == "" {
		t.Fatal("checkpoint A was not created")
	}

	// Checkpoint B advances HEAD so rewinding to A is a real branch switch:
	// tracked.txt = "v2".
	sessionB := env.NewSession()
	if err := env.SimulateUserPromptSubmitWithPrompt(sessionB.ID, "edit tracked file"); err != nil {
		t.Fatalf("user-prompt-submit B: %v", err)
	}
	env.WriteFile("tracked.txt", "v2\n")
	sessionB.CreateTranscript("edit tracked file", []FileChange{{Path: "tracked.txt", Content: "v2\n"}})
	if err := env.SimulateStop(sessionB.ID, sessionB.TranscriptPath); err != nil {
		t.Fatalf("stop B: %v", err)
	}
	env.GitCommitWithShadowHooksAsAgent("edit tracked", "tracked.txt")

	// Uncommitted modification to the tracked file — a plain `git checkout`
	// of another commit aborts on this; only --force discards it.
	env.WriteFile("tracked.txt", "dirty uncommitted change\n")

	// Locate checkpoint A in the rewind menu (points are listed newest-first, so
	// the index isn't fixed). The menu is 1-indexed and shares its order with
	// GetRewindPoints.
	points := env.GetRewindPoints()
	selectNum := 0
	for i, p := range points {
		if p.ID == commitA {
			if !p.IsLogsOnly {
				t.Fatalf("checkpoint A point is not a logs-only (committed) point; interactive checkout menu won't appear")
			}
			selectNum = i + 1
			break
		}
	}
	if selectNum == 0 {
		t.Fatalf("checkpoint A (%s) not found among rewind points: %+v", commitA[:7], points)
	}

	// Drive the interactive rewind over a pty in accessible mode:
	//   1. select checkpoint A
	//   2. choose "Checkout commit (detached HEAD)" (option 2)
	//   3. confirm "Create detached HEAD?" with y
	out, err := env.RunCommandInteractive([]string{"checkpoint", "rewind"}, func(ptyFile *os.File) string {
		var sb strings.Builder
		respond := func(promptSubstr, response string) {
			got, waitErr := WaitForPromptAndRespond(ptyFile, promptSubstr, response, 8*time.Second)
			sb.WriteString(got)
			if waitErr != nil {
				sb.WriteString("\n[wait error for " + promptSubstr + ": " + waitErr.Error() + "]\n")
			}
		}
		respond("Select a checkpoint to restore", strconv.Itoa(selectNum)+"\n")
		respond("Logs-only point", "2\n")
		respond("Create detached HEAD", "y\n")
		return sb.String()
	})
	if err != nil {
		t.Fatalf("interactive rewind failed: %v\nOutput:\n%s", err, out)
	}

	// With the fix the forced checkout discarded the dirty change and restored
	// checkpoint A's content.
	if got := env.ReadFile("tracked.txt"); got != "v1\n" {
		t.Fatalf("rewind checkout did not discard uncommitted changes (#668): tracked.txt = %q, want %q\nOutput:\n%s", got, "v1\n", out)
	}

	// The checkout must have landed on checkpoint A in a detached HEAD.
	if head := env.GetHeadHash(); head != commitA {
		t.Fatalf("HEAD = %s, want checkpoint A %s after rewind checkout\nOutput:\n%s", head, commitA, out)
	}
	if branch := env.GetCurrentBranch(); branch != "" {
		t.Fatalf("expected detached HEAD after checkout, but on branch %q\nOutput:\n%s", branch, out)
	}
}
