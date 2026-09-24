//go:build integration

package integration

import (
	"slices"
	"strings"
	"testing"
)

// TestSkippedHookCommit_FilesAlreadyInHeadAreNotCarriedForward drives the
// sequence from #2577: a session's turn is committed with every Git hook
// skipped (LEFTHOOK=0, a hooks-less GUI client), then the next turn is
// committed normally. The first turn's file reached HEAD through a commit
// Entire never saw, so the hooked commit's diff does not contain it. It used
// to be carried forward onto a fresh shadow branch after every later commit,
// forever; it must now be dropped — unless it was edited again on disk, in
// which case those newer edits are still the agent's and must be kept.
func TestSkippedHookCommit_FilesAlreadyInHeadAreNotCarriedForward(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		editOneTxt  bool // edit one.txt on disk after the skipped commit
		wantTouched []string
	}{
		{name: "working tree clean", wantTouched: nil},
		{name: "edited again on disk", editOneTxt: true, wantTouched: []string{"one.txt"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := NewTestEnv(t)
			defer env.Cleanup()

			env.InitRepo()
			env.WriteFile("README.md", "# Test Repository")
			env.GitAdd("README.md")
			env.GitCommit("Initial commit")
			env.GitCheckoutNewBranch("feature/skipped-hook-commit")
			env.InitEntire()

			session := env.NewSession()

			// Turn 1 creates one.txt; it is committed with no hooks at all.
			if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
				t.Fatalf("SimulateUserPromptSubmit turn 1: %v", err)
			}
			env.WriteFile("one.txt", "one")
			session.CreateTranscript("Create one.txt", []FileChange{{Path: "one.txt", Content: "one"}})
			if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
				t.Fatalf("SimulateStop turn 1: %v", err)
			}
			env.GitAdd("one.txt")
			env.GitCommit("one.txt, hooks skipped")
			if tc.editOneTxt {
				env.WriteFile("one.txt", "one, edited again")
			}

			// Turn 2 creates two.txt; it is committed with the hooks.
			if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
				t.Fatalf("SimulateUserPromptSubmit turn 2: %v", err)
			}
			env.WriteFile("two.txt", "two")
			session.CreateTranscript("Create two.txt", []FileChange{{Path: "two.txt", Content: "two"}})
			if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
				t.Fatalf("SimulateStop turn 2: %v", err)
			}
			env.GitAdd("two.txt")
			env.GitCommitWithShadowHooks("two.txt", "two.txt")

			state, err := env.GetSessionState(session.ID)
			if err != nil {
				t.Fatalf("GetSessionState: %v", err)
			}
			if !slices.Equal(state.FilesTouched, tc.wantTouched) {
				t.Errorf("FilesTouched after the hooked commit = %v, want %v", state.FilesTouched, tc.wantTouched)
			}

			var shadows []string
			for _, b := range env.ListBranchesWithPrefix("entire/") {
				if !strings.HasPrefix(b, "entire/checkpoints") {
					shadows = append(shadows, b)
				}
			}
			if wantShadow := len(tc.wantTouched) > 0; (len(shadows) > 0) != wantShadow {
				t.Errorf("shadow branches after the hooked commit = %v, want carry-forward: %v", shadows, wantShadow)
			}
		})
	}
}
