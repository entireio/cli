package checkpoint

import (
	"context"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/redact"
	"github.com/go-git/go-git/v6"
	"github.com/stretchr/testify/require"
)

// taskReaderStore is the surface these tests drive: the write that
// materializes task records and the reader that brings them back.
type taskReaderStore interface {
	Writer
	TaskReader
}

var taskReaderStarted = time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)

func writeTaskReaderCheckpoint(t *testing.T, store Writer, cid id.CheckpointID) {
	t.Helper()
	err := store.Write(context.Background(), Session{
		CheckpointID:     cid,
		SessionID:        "task-reader-session",
		Strategy:         "manual-commit",
		Transcript:       redact.AlreadyRedacted([]byte(`{"msg":"parent"}` + "\n")),
		CheckpointsCount: 1,
		AuthorName:       "Test Author",
		AuthorEmail:      "test@example.com",
		Tasks: []TaskPayload{
			{
				ToolUseID:       "toolu_late",
				AgentID:         "agentlate",
				SubagentType:    "Explore",
				TaskDescription: "second task",
				Transcript:      redact.AlreadyRedacted([]byte("late child line\n")),
				Files:           []string{"b.go"},
				TokenUsage:      &types.TokenUsage{InputTokens: 11, OutputTokens: 2, APICallCount: 1},
				StartedAt:       taskReaderStarted.Add(time.Minute),
				CompletedAt:     taskReaderStarted.Add(2 * time.Minute),
			},
			{
				ToolUseID:                   "toolu_early",
				AgentID:                     "agentearly",
				SubagentType:                "general-purpose",
				StartedAt:                   taskReaderStarted,
				TranscriptUnavailableReason: "transcript unreadable",
			},
		},
	})
	require.NoError(t, err)
}

// TestTaskReader_ListsAndReadsTaskRecords pins the read half of the subagent
// task records: the write path has materialized tasks/<tool_use_id>/ since
// #2058, but nothing read them back. Both git backends share one tree reader,
// so this runs it through each of them and through the wrappers that must
// forward it (routing and mirror fan-out).
func TestTaskReader_ListsAndReadsTaskRecords(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		build func(repo *git.Repository) taskReaderStore
	}{
		{
			name:  "git-branch store",
			build: func(repo *git.Repository) taskReaderStore { return NewGitStore(repo, DefaultV1Refs()) },
		},
		{
			name:  "git-refs store",
			build: func(repo *git.Repository) taskReaderStore { return newGitRefsStore(repo) },
		},
		{
			name: "kind routing store, git-refs primary",
			build: func(repo *git.Repository) taskReaderStore {
				refs := newGitRefsStore(repo)
				return newKindRoutingStore(refs, NewGitStore(repo, DefaultV1Refs()), refs, BackendTypeGitRefs)
			},
		},
		{
			name: "mirror fan-out over git-branch",
			build: func(repo *git.Repository) taskReaderStore {
				return newFanoutStore(NewGitStore(repo, DefaultV1Refs()), []Writer{&fakeMirror{}})
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			repo, _ := setupBranchTestRepo(t)
			store := tt.build(repo)
			cid := id.MustCheckpointID("aabbccdd0101")
			writeTaskReaderCheckpoint(t, store, cid)

			entries, err := store.ListTasks(ctx, cid)
			require.NoError(t, err)
			require.Len(t, entries, 2)

			// Ordered by launch time, not by directory name.
			early, late := entries[0], entries[1]
			require.Equal(t, "toolu_early", early.ToolUseID)
			require.NoError(t, early.Err)
			require.False(t, early.TranscriptStored)
			require.Equal(t, "agentearly", early.Record.AgentID)
			require.Equal(t, "transcript unreadable", early.Record.TranscriptUnavailableReason)
			require.True(t, early.Record.CompletedAt.IsZero())

			require.Equal(t, "toolu_late", late.ToolUseID)
			require.NoError(t, late.Err)
			require.True(t, late.TranscriptStored)
			require.Equal(t, "agentlate", late.Record.AgentID)
			require.Equal(t, "Explore", late.Record.SubagentType)
			require.Equal(t, "second task", late.Record.TaskDescription)
			require.Equal(t, []string{"b.go"}, late.Record.Files)
			require.NotNil(t, late.Record.TokenUsage)
			require.Equal(t, 11, late.Record.TokenUsage.InputTokens)
			require.True(t, late.Record.StartedAt.Equal(taskReaderStarted.Add(time.Minute)))
			require.True(t, late.Record.CompletedAt.Equal(taskReaderStarted.Add(2*time.Minute)))

			transcript, err := store.ReadTaskTranscript(ctx, cid, "toolu_late")
			require.NoError(t, err)
			require.Equal(t, "late child line\n", string(transcript))

			_, err = store.ReadTaskTranscript(ctx, cid, "toolu_early")
			require.ErrorIs(t, err, ErrNoTranscript)
			require.ErrorContains(t, err, "transcript unreadable")

			_, err = store.ReadTaskTranscript(ctx, cid, "toolu_absent")
			require.ErrorIs(t, err, ErrTaskNotFound)

			_, err = store.ReadTaskTranscript(ctx, cid, "../escape")
			require.Error(t, err)
			require.NotErrorIs(t, err, ErrTaskNotFound, "a path-unsafe ID is rejected before any tree lookup")

			_, err = store.ListTasks(ctx, id.MustCheckpointID("aabbccdd0bad"))
			require.ErrorIs(t, err, ErrCheckpointNotFound)
		})
	}
}

// TestTaskReader_NoTasksIsEmptyNotError: every checkpoint written before the
// task-record writer, and every session without subagents, has no tasks/
// subtree. That is an empty list, not a failure.
func TestTaskReader_NoTasksIsEmptyNotError(t *testing.T) {
	t.Parallel()
	for _, build := range []func(*git.Repository) taskReaderStore{
		func(repo *git.Repository) taskReaderStore { return NewGitStore(repo, DefaultV1Refs()) },
		func(repo *git.Repository) taskReaderStore { return newGitRefsStore(repo) },
	} {
		repo, _ := setupBranchTestRepo(t)
		store := build(repo)
		cid := id.MustCheckpointID("aabbccdd0202")
		require.NoError(t, store.Write(context.Background(), Session{
			CheckpointID:     cid,
			SessionID:        "no-task-session",
			Strategy:         "manual-commit",
			Transcript:       redact.AlreadyRedacted([]byte(`{"msg":"parent"}` + "\n")),
			CheckpointsCount: 1,
			AuthorName:       "Test Author",
			AuthorEmail:      "test@example.com",
		}))

		entries, err := store.ListTasks(context.Background(), cid)
		require.NoError(t, err)
		require.Empty(t, entries)

		_, err = store.ReadTaskTranscript(context.Background(), cid, "toolu_any")
		require.ErrorIs(t, err, ErrTaskNotFound)
	}
}
