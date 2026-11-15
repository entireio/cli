//go:build integration

package integration

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// These tests cover a transcript that spells a tracked path in another case
// than git does, as an agent on a case-insensitive filesystem (macOS,
// Windows) can: writing readme.md there changes README.md. Recorded as
// readme.md, the path never matches a commit of README.md, and the session
// stays pending for good. They run where the filesystem ignores case.

// transcriptCaseEnv returns a repository that tracks README.md, and skips the
// test unless its filesystem ignores case.
func transcriptCaseEnv(t *testing.T) *TestEnv {
	t.Helper()
	env := NewFeatureBranchEnv(t)
	env.WriteFile("README.md", "readme\n")
	env.GitAdd("README.md")
	env.GitCommit("Add README.md")
	tracked, err := os.Lstat(filepath.Join(env.RepoDir, "README.md"))
	if err != nil {
		t.Fatalf("lstat README.md: %v", err)
	}
	if other, err := os.Lstat(filepath.Join(env.RepoDir, "readme.md")); err != nil || !os.SameFile(tracked, other) {
		t.Skip("needs a case-insensitive filesystem; strategy's TestTrackedSpelling covers the rule here")
	}
	return env
}

func sessionFilesTouched(t *testing.T, env *TestEnv, sess *Session) []string {
	t.Helper()
	state, err := env.GetSessionState(sess.ID)
	if err != nil || state == nil {
		t.Fatalf("GetSessionState(%s) failed: %v (state=%v)", sess.ID, err, state)
	}
	files := slices.Clone(state.FilesTouched)
	slices.Sort(files)
	return files
}

// The turn end merges git status's README.md with the transcript's readme.md.
// Once README.md is committed nothing may stay pending, so the ended session
// is fully condensed by the next commit.
func TestTranscriptPathCase_DirtyFileIsRecordedOnce(t *testing.T) {
	t.Parallel()
	env := transcriptCaseEnv(t)
	sess := env.NewSession()
	if err := env.SimulateUserPromptSubmitWithTranscriptPath(sess.ID, sess.TranscriptPath); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}
	env.WriteFile("README.md", "readme, edited\n")
	sess.CreateTranscript("edit the readme", []FileChange{{Path: "readme.md", Content: "readme, edited\n"}})
	if err := env.SimulateStop(sess.ID, sess.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop failed: %v", err)
	}
	if got := sessionFilesTouched(t, env, sess); !slices.Equal(got, []string{"README.md"}) {
		t.Fatalf("FilesTouched = %v, want [README.md], in git's spelling only", got)
	}

	env.GitCommitWithHooks("Commit README.md", "README.md")
	if got := sessionFilesTouched(t, env, sess); len(got) != 0 {
		t.Fatalf("FilesTouched = %v after README.md was committed, want none", got)
	}
	if err := env.SimulateSessionEnd(sess.ID); err != nil {
		t.Fatalf("SimulateSessionEnd failed: %v", err)
	}
	env.WriteFile("other.txt", "other\n")
	env.GitCommitWithHooks("Commit other.txt", "other.txt")
	state, err := env.GetSessionState(sess.ID)
	if err != nil || state == nil {
		t.Fatalf("GetSessionState failed: %v", err)
	}
	if !state.FullyCondensed {
		t.Errorf("ended session is not fully condensed; FilesTouched = %v", state.FilesTouched)
	}
}

// A turn that commits README.md mid-turn leaves nothing pending: the
// transcript's readme.md is the committed file, not an uncommitted one.
func TestTranscriptPathCase_FileCommittedMidTurnIsNotPending(t *testing.T) {
	t.Parallel()
	env := transcriptCaseEnv(t)
	sess := env.NewSession()
	if err := env.SimulateUserPromptSubmitWithTranscriptPath(sess.ID, sess.TranscriptPath); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}
	env.WriteFile("README.md", "readme, edited\n")
	sess.CreateTranscript("edit the readme and commit it", []FileChange{{Path: "readme.md", Content: "readme, edited\n"}})
	env.GitCommitWithHooksAsAgent("Commit README.md", "README.md")
	if got := sessionFilesTouched(t, env, sess); len(got) != 0 {
		t.Fatalf("FilesTouched = %v after the mid-turn commit, want none", got)
	}
	if err := env.SimulateStop(sess.ID, sess.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop failed: %v", err)
	}
	if got := sessionFilesTouched(t, env, sess); len(got) != 0 {
		t.Fatalf("FilesTouched = %v after the turn committed README.md, want none", got)
	}
}
