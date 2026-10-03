package cli

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// A copy of the baseline that cannot be read must not end the search: a later
// worktree may hold the real one.
func TestBaselineSearch_SkipsAnUnreadableCopy(t *testing.T) {
	setupTestRepo(t)
	const name = "pre-task-toolu_unreadable.json"
	// A directory where the file should be: present, but not readable as one.
	require.NoError(t, os.MkdirAll(filepath.Join(".entire", "tmp", name), 0o750))
	other := t.TempDir()
	testutil.InitRepo(t, other)
	require.NoError(t, os.MkdirAll(filepath.Join(other, ".entire", "tmp"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(other, ".entire", "tmp", name), []byte(`{"tool_use_id":"toolu_unreadable"}`), 0o600))

	data, from, err := baselineSearch{"", other}.read(context.Background(), name)
	require.NoError(t, err)
	require.Equal(t, other, from)
	require.JSONEq(t, `{"tool_use_id":"toolu_unreadable"}`, string(data))
}

// A baseline that exists but cannot be read degrades new-file detection
// instead of leaving the caller with no baseline, which would claim every
// untracked file as the task's.
func TestLoadPreTaskState_UnreadableBaselineDegradesDetection(t *testing.T) {
	setupTestRepo(t)
	const toolUseID = "toolu_unreadable"
	require.NoError(t, os.MkdirAll(filepath.Join(".entire", "tmp", "pre-task-"+toolUseID+".json"), 0o750))

	state, err := LoadPreTaskState(context.Background(), toolUseID)
	require.Error(t, err)
	require.True(t, state.NewFilesUndetectable(), "an unreadable baseline must disable status-based new-file detection")
}

func TestLoadPrePromptState_UnreadableBaselineDegradesDetection(t *testing.T) {
	setupTestRepo(t)
	const sessionID = "sess-unreadable"
	require.NoError(t, os.MkdirAll(filepath.Join(".entire", "tmp"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(".entire", "tmp", "pre-prompt-"+sessionID+".json"), []byte("{not json"), 0o600))

	state, err := LoadPrePromptState(context.Background(), sessionID)
	require.Error(t, err)
	require.True(t, state.NewFilesUndetectable(), "a corrupt baseline must disable status-based new-file detection")
}

// The turn-start offset marks this turn's prompt only while it still lands on
// a boundary; a file cleared and refilled past it must not be split mid-prompt.
func TestPromptBoundary(t *testing.T) {
	t.Parallel()
	prompts := []byte("first" + promptSeparator + "second")
	cases := map[string]struct {
		offset int
		want   bool
	}{
		"start":                    {offset: 0, want: true},
		"on the separator":         {offset: len("first"), want: true},
		"end, nothing appended":    {offset: len(prompts), want: true},
		"mid-prompt after refill":  {offset: 2, want: false},
		"past the end after clear": {offset: len(prompts) + 1, want: false},
		"negative":                 {offset: -1, want: false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, promptBoundary(prompts, tc.offset))
		})
	}
}

// Condensation clears a session's prompt.txt under the session lock, possibly
// in the very worktree a turn's prompt is being carried out of. The carry
// reads, writes and trims both copies, so it must hold the same lock, or a
// commit condensing mid-carry has its cleared prompts written back.
//
// Not parallel: setupTestRepo changes the process directory.
func TestCarryTurnPrompt_WaitsForTheSessionLock(t *testing.T) {
	dst := setupTestRepo(t)
	const sessionID = "sess-carry-lock"
	ctx := context.Background()
	require.NoError(t, strategy.SaveSessionState(ctx, &strategy.SessionState{SessionID: sessionID, BaseCommit: "abc123", StartedAt: time.Now(), WorktreePath: dst}))
	src := t.TempDir()
	testutil.InitRepo(t, src)
	srcPrompt := filepath.Join(src, ".entire", "metadata", sessionID, "prompt.txt")
	require.NoError(t, os.MkdirAll(filepath.Dir(srcPrompt), 0o750))
	before := "earlier turn"
	require.NoError(t, os.WriteFile(srcPrompt, []byte(before+promptSeparator+"this turn"), 0o600))
	dstPrompt := filepath.Join(dst, ".entire", "metadata", sessionID, "prompt.txt")

	locked, release := make(chan struct{}), make(chan struct{})
	holder := make(chan error, 1)
	go func() {
		holder <- strategy.MutateSessionState(ctx, sessionID, func(*strategy.SessionState) error {
			close(locked)
			<-release
			return strategy.ErrMutationSkip
		})
	}()
	select {
	case <-locked:
	case err := <-holder:
		t.Fatalf("precondition: the holder never took the session lock: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("precondition: the holder never took the session lock")
	}
	carried := make(chan error, 1)
	go func() { carried <- carryTurnPrompt(ctx, src, sessionID, len(before)) }()

	time.Sleep(300 * time.Millisecond)
	_, statErr := os.Stat(dstPrompt)
	require.ErrorIs(t, statErr, os.ErrNotExist, "the carry must not write while another holder has the session lock")
	still, err := os.ReadFile(srcPrompt)
	require.NoError(t, err)
	require.Contains(t, string(still), "this turn", "nor trim the source")

	close(release)
	require.NoError(t, <-holder)
	require.NoError(t, <-carried)
	got, err := os.ReadFile(dstPrompt)
	require.NoError(t, err)
	require.Equal(t, "this turn", string(got))
}

// A hook that followed its agent into a tree another session also works in
// must not take that session's newer task as its own.
//
// Not parallel: setupTestRepo changes the process directory.
func TestFindActivePreTaskFile_IgnoresAnotherSessionsTask(t *testing.T) {
	setupTestRepo(t)
	ctx := context.Background()
	require.NoError(t, CapturePreTaskState(ctx, "sess-mine", "toolu_mine"))
	time.Sleep(20 * time.Millisecond)
	require.NoError(t, CapturePreTaskState(ctx, "sess-other", "toolu_other"))

	got, found := FindActivePreTaskFile(ctx, "sess-mine")
	require.True(t, found)
	require.Equal(t, "toolu_mine", got)

	got, found = FindActivePreTaskFile(ctx, "")
	require.True(t, found)
	require.Equal(t, "toolu_other", got, "without a session the newest task wins, as before")
}

// The carry writes this tree's copy, then trims the source. If the trim fails
// the prompt would sit in both trees and be condensed twice, so the write is
// undone: the prompt stays where it was, once.
//
// Not parallel: setupTestRepo changes the process directory.
func TestCarryTurnPrompt_UndoesTheCopyWhenTheSourceCannotBeTrimmed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("read-only directory permissions")
	}
	dst := setupTestRepo(t)
	const sessionID = "sess-carry-undo"
	src := t.TempDir()
	testutil.InitRepo(t, src)
	srcDir := filepath.Join(src, ".entire", "metadata", sessionID)
	require.NoError(t, os.MkdirAll(srcDir, 0o750))
	before := "earlier turn"
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "prompt.txt"), []byte(before+promptSeparator+"this turn"), 0o600))
	dstPrompt := filepath.Join(dst, ".entire", "metadata", sessionID, "prompt.txt")
	require.NoError(t, os.MkdirAll(filepath.Dir(dstPrompt), 0o750))
	require.NoError(t, os.WriteFile(dstPrompt, []byte("this tree's earlier turn"), 0o600))
	require.NoError(t, os.Chmod(srcDir, 0o500)) // the trim's atomic write cannot create its temp file
	t.Cleanup(func() {
		if err := os.Chmod(srcDir, 0o750); err != nil {
			t.Logf("restore permissions for cleanup: %v", err)
		}
	})

	require.Error(t, carryTurnPrompt(context.Background(), src, sessionID, len(before)))

	got, err := os.ReadFile(dstPrompt)
	require.NoError(t, err)
	require.Equal(t, "this tree's earlier turn", string(got), "the copy is undone")
	still, err := os.ReadFile(filepath.Join(srcDir, "prompt.txt"))
	require.NoError(t, err)
	require.Contains(t, string(still), "this turn")
}

// A prompt.txt that exists but cannot be read at turn start leaves the turn's
// starting offset unknown. Carrying from offset 0 would move every earlier
// turn's prompt and delete it from the tree whose saved steps it belongs to,
// so an unknown offset carries nothing.
//
// Not parallel: setupTestRepo changes the process directory.
func TestPromptOffset_UnreadablePromptFileSkipsTheCarry(t *testing.T) {
	setupTestRepo(t)
	ctx := context.Background()
	const sessionID = "sess-unreadable-prompt"
	// A directory where the file should be: present, but not readable as one.
	require.NoError(t, os.MkdirAll(filepath.Join(".entire", "metadata", sessionID, "prompt.txt"), 0o750))
	require.NoError(t, CapturePrePromptState(ctx, nil, sessionID, ""))
	state, err := LoadPrePromptState(ctx, sessionID)
	require.NoError(t, err)
	require.Equal(t, unknownPromptOffset, state.PromptOffset)

	src := t.TempDir()
	testutil.InitRepo(t, src)
	srcPrompt := filepath.Join(src, ".entire", "metadata", sessionID, "prompt.txt")
	require.NoError(t, os.MkdirAll(filepath.Dir(srcPrompt), 0o750))
	require.NoError(t, os.WriteFile(srcPrompt, []byte("earlier turns"), 0o600))
	require.NoError(t, carryTurnPrompt(ctx, src, sessionID, unknownPromptOffset))
	got, err := os.ReadFile(srcPrompt)
	require.NoError(t, err)
	require.Equal(t, "earlier turns", string(got), "nothing is carried out of the source")
}
