package checkpoint

import (
	"context"
	"testing"
	"time"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/redact"
)

var (
	taskRecordStarted   = time.Date(2026, 8, 19, 10, 0, 0, 0, time.UTC)
	taskRecordCompleted = taskRecordStarted.Add(2 * time.Minute)
)

// taskRecordFixtures returns two task payloads carrying every field the
// report consumes. The second is still in flight (zero CompletedAt) so the
// read path is shown to pass the omitzero'd timestamp through as zero rather
// than inventing one.
func taskRecordFixtures() []TaskPayload {
	return []TaskPayload{
		{
			ToolUseID:       "toolu_explore",
			AgentID:         "agent-a",
			SubagentType:    "Explore",
			TaskDescription: "look around",
			Transcript:      redact.AlreadyRedacted([]byte(`{"msg":"explore"}` + "\n")),
			TokenUsage:      &types.TokenUsage{InputTokens: 1000, CacheReadTokens: 500, OutputTokens: 200, APICallCount: 3},
			StartedAt:       taskRecordStarted,
			CompletedAt:     taskRecordCompleted,
		},
		{
			ToolUseID:    "toolu_plan",
			AgentID:      "agent-b",
			SubagentType: "Plan",
			Transcript:   redact.AlreadyRedacted([]byte(`{"msg":"plan"}` + "\n")),
			TokenUsage:   &types.TokenUsage{InputTokens: 40, OutputTokens: 7, APICallCount: 1},
			StartedAt:    taskRecordStarted.Add(30 * time.Second),
		},
	}
}

// writeTaskRecordCheckpoint writes a one-session checkpoint carrying tasks.
func writeTaskRecordCheckpoint(t *testing.T, store PersistentStore, cid id.CheckpointID, tasks []TaskPayload) {
	t.Helper()
	require.NoError(t, store.Write(context.Background(), Session{
		CheckpointID: cid,
		SessionID:    "task-records-session",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte(`{"msg":"session"}` + "\n")),
		AuthorName:   "Test Author",
		AuthorEmail:  "test@example.com",
		Tasks:        tasks,
	}))
}

// injectMalformedTaskJSON commits a tasks/<toolUseID>/task.json holding
// non-JSON bytes into the checkpoint tree reachable from ref, at
// checkpointPath (the sharded path for the v1 branch, empty for a per-checkpoint
// ref), and moves ref to the new commit.
func injectMalformedTaskJSON(t *testing.T, repo *git.Repository, ref plumbing.ReferenceName, checkpointPath []string, toolUseID string) {
	t.Helper()
	tip, err := repo.Reference(ref, true)
	require.NoError(t, err)
	commit, err := repo.CommitObject(tip.Hash())
	require.NoError(t, err)

	blob, err := CreateBlobFromContent(repo, []byte("{not json"))
	require.NoError(t, err)
	segments := append(append([]string{}, checkpointPath...), "tasks", toolUseID)
	newTree, err := UpdateSubtree(repo, commit.TreeHash, segments, []object.TreeEntry{
		{Name: "task.json", Mode: filemode.Regular, Hash: blob},
	}, UpdateSubtreeOptions{MergeMode: MergeKeepExisting})
	require.NoError(t, err)

	newCommit, err := CreateCommit(context.Background(), repo, newTree, tip.Hash(), "inject malformed task.json", "Test", "test@example.com")
	require.NoError(t, err)
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(ref, newCommit)))
}

// assertTaskRecordFixtures checks that got is exactly the two fixture records,
// with every report-facing field carried through the write -> read round trip.
func assertTaskRecordFixtures(t *testing.T, got []StoredTaskRecord) {
	t.Helper()
	require.Len(t, got, 2)
	byID := make(map[string]StoredTaskRecord, len(got))
	for _, r := range got {
		byID[r.ToolUseID] = r
	}

	explore, ok := byID["toolu_explore"]
	require.True(t, ok, "toolu_explore record missing")
	assert.Equal(t, "agent-a", explore.AgentID)
	assert.Equal(t, "Explore", explore.SubagentType)
	assert.Equal(t, "look around", explore.TaskDescription)
	require.NotNil(t, explore.TokenUsage)
	assert.Equal(t, types.TokenUsage{InputTokens: 1000, CacheReadTokens: 500, OutputTokens: 200, APICallCount: 3}, *explore.TokenUsage)
	assert.True(t, explore.StartedAt.Equal(taskRecordStarted), "StartedAt = %v", explore.StartedAt)
	assert.True(t, explore.CompletedAt.Equal(taskRecordCompleted), "CompletedAt = %v", explore.CompletedAt)

	plan, ok := byID["toolu_plan"]
	require.True(t, ok, "toolu_plan record missing")
	assert.Equal(t, "Plan", plan.SubagentType)
	require.NotNil(t, plan.TokenUsage)
	assert.Equal(t, types.TokenUsage{InputTokens: 40, OutputTokens: 7, APICallCount: 1}, *plan.TokenUsage)
	assert.True(t, plan.StartedAt.Equal(taskRecordStarted.Add(30*time.Second)), "StartedAt = %v", plan.StartedAt)
	assert.True(t, plan.CompletedAt.IsZero(), "in-flight task must read back with a zero CompletedAt, got %v", plan.CompletedAt)
}

// taskRecordStoreCase describes one persistent backend under test: how to build
// it, which checkpoint ID kind it serves, and where its checkpoint tree lives so
// a test can splice a malformed task.json into it.
type taskRecordStoreCase struct {
	name     string
	cid      id.CheckpointID
	newStore func(t *testing.T) (PersistentStore, *git.Repository)
	// ref is the reference whose commit tree contains the checkpoint.
	ref func(t *testing.T, cid id.CheckpointID) plumbing.ReferenceName
	// checkpointPath is the checkpoint's directory inside that tree.
	checkpointPath func(cid id.CheckpointID) []string
}

func taskRecordStoreCases() []taskRecordStoreCase {
	return []taskRecordStoreCase{
		{
			name: "git-branch",
			cid:  id.MustCheckpointID("a1b2c3d4e5f6"),
			newStore: func(t *testing.T) (PersistentStore, *git.Repository) {
				t.Helper()
				_, repo, _ := newTestRepo(t)
				return NewGitStore(repo, DefaultV1Refs()), repo
			},
			ref: func(t *testing.T, _ id.CheckpointID) plumbing.ReferenceName {
				t.Helper()
				return plumbing.NewBranchReferenceName(paths.MetadataBranchName)
			},
			checkpointPath: func(cid id.CheckpointID) []string {
				return []string{string(cid[:2]), string(cid[2:])}
			},
		},
		{
			name: "git-refs",
			cid:  id.MustCheckpointID(routingSampleULID),
			newStore: func(t *testing.T) (PersistentStore, *git.Repository) {
				t.Helper()
				_, repo, _ := newTestRepo(t)
				return newGitRefsStore(repo), repo
			},
			ref: func(t *testing.T, cid id.CheckpointID) plumbing.ReferenceName {
				t.Helper()
				return mustRefName(t, cid)
			},
			checkpointPath: func(id.CheckpointID) []string { return nil },
		},
	}
}

func TestReadTaskRecords(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	for _, tc := range taskRecordStoreCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			t.Run("reads every record with token usage and timing", func(t *testing.T) {
				t.Parallel()
				store, _ := tc.newStore(t)
				writeTaskRecordCheckpoint(t, store, tc.cid, taskRecordFixtures())

				got, err := store.ReadTaskRecords(ctx, tc.cid)
				require.NoError(t, err)
				assertTaskRecordFixtures(t, got)
			})

			t.Run("unknown checkpoint is ErrCheckpointNotFound", func(t *testing.T) {
				t.Parallel()
				store, _ := tc.newStore(t)

				_, err := store.ReadTaskRecords(ctx, tc.cid)
				require.ErrorIs(t, err, ErrCheckpointNotFound)
			})

			t.Run("checkpoint without tasks yields no records and no error", func(t *testing.T) {
				t.Parallel()
				store, _ := tc.newStore(t)
				writeTaskRecordCheckpoint(t, store, tc.cid, nil)

				got, err := store.ReadTaskRecords(ctx, tc.cid)
				require.NoError(t, err)
				assert.Nil(t, got)
			})

			t.Run("malformed task.json is skipped, siblings still returned", func(t *testing.T) {
				t.Parallel()
				store, repo := tc.newStore(t)
				writeTaskRecordCheckpoint(t, store, tc.cid, taskRecordFixtures())
				injectMalformedTaskJSON(t, repo, tc.ref(t, tc.cid), tc.checkpointPath(tc.cid), "toolu_broken")

				got, err := store.ReadTaskRecords(ctx, tc.cid)
				require.NoError(t, err)
				assertTaskRecordFixtures(t, got)
				for _, r := range got {
					assert.NotEqual(t, "toolu_broken", r.ToolUseID)
				}
			})
		})
	}
}

func TestKindRoutingStore_ReadTaskRecords(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	hexID := id.MustCheckpointID("a1b2c3d4e5f6")
	ulidID := id.MustCheckpointID(routingSampleULID)

	t.Run("git-refs primary falls through to the branch for a pre-migration hex checkpoint", func(t *testing.T) {
		t.Parallel()
		_, repo, _ := newTestRepo(t)
		branch := NewGitStore(repo, DefaultV1Refs())
		refs := newGitRefsStore(repo)
		writeTaskRecordCheckpoint(t, branch, hexID, taskRecordFixtures())

		router := newKindRoutingStore(refs, branch, refs, BackendTypeGitRefs)

		got, err := router.ReadTaskRecords(ctx, hexID)
		require.NoError(t, err)
		assertTaskRecordFixtures(t, got)
	})

	t.Run("ULID resolves from refs under any primary", func(t *testing.T) {
		t.Parallel()
		_, repo, _ := newTestRepo(t)
		branch := NewGitStore(repo, DefaultV1Refs())
		refs := newGitRefsStore(repo)
		writeTaskRecordCheckpoint(t, refs, ulidID, taskRecordFixtures())

		router := newKindRoutingStore(branch, branch, refs, BackendTypeGitBranch)

		got, err := router.ReadTaskRecords(ctx, ulidID)
		require.NoError(t, err)
		assertTaskRecordFixtures(t, got)
	})

	t.Run("absent everywhere is ErrCheckpointNotFound", func(t *testing.T) {
		t.Parallel()
		_, repo, _ := newTestRepo(t)
		branch := NewGitStore(repo, DefaultV1Refs())
		refs := newGitRefsStore(repo)

		router := newKindRoutingStore(refs, branch, refs, BackendTypeGitRefs)

		_, err := router.ReadTaskRecords(ctx, hexID)
		require.ErrorIs(t, err, ErrCheckpointNotFound)
	})
}
