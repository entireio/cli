package checkpoint

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
	"github.com/entireio/cli/redact"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var batchTestTime = time.Date(2026, time.August, 25, 12, 34, 56, 0, time.UTC)

func batchTestSession(checkpointID id.CheckpointID, sessionID string) ReservedSession {
	return ReservedSession(WriteOptions{
		CheckpointID:     checkpointID,
		SessionID:        sessionID,
		CreatedAt:        batchTestTime,
		Strategy:         "manual-commit",
		Branch:           "feature",
		CheckpointsCount: 1,
		AuthorName:       "nested author is ignored",
		AuthorEmail:      "nested@example.com",
	})
}

func batchTestRequest(checkpointID id.CheckpointID, sessions ...ReservedSession) BatchSessions {
	return BatchSessions{
		CheckpointID: checkpointID,
		Sessions:     sessions,
		CommitTime:   batchTestTime,
		AuthorName:   "Batch Author",
		AuthorEmail:  "batch@example.com",
	}
}

func branchCheckpointTree(t *testing.T, store *GitStore, checkpointID id.CheckpointID) *object.Tree {
	t.Helper()
	ref, err := store.repo.Reference(store.refs.Primary, true)
	require.NoError(t, err)
	commit, err := store.repo.CommitObject(ref.Hash())
	require.NoError(t, err)
	root, err := commit.Tree()
	require.NoError(t, err)
	tree, err := root.Tree(checkpointID.Path())
	require.NoError(t, err)
	return tree
}

func refsCheckpointTree(t *testing.T, store *gitRefsStore, checkpointID id.CheckpointID) *object.Tree {
	t.Helper()
	ref, err := store.repo.Reference(mustRefName(t, checkpointID), true)
	require.NoError(t, err)
	commit, err := store.repo.CommitObject(ref.Hash())
	require.NoError(t, err)
	tree, err := commit.Tree()
	require.NoError(t, err)
	return tree
}

func treeEntryHash(t *testing.T, tree *object.Tree, name string) plumbing.Hash {
	t.Helper()
	for _, entry := range tree.Entries {
		if entry.Name == name {
			return entry.Hash
		}
	}
	t.Fatalf("tree entry %q not found", name)
	return plumbing.ZeroHash
}

func TestBatchSessions_ReverseOrderProducesSameTreeSummaryAndMessage(t *testing.T) {
	t.Parallel()
	checkpointID := id.MustCheckpointID("a1b2c3d4e5f6")
	ascending := []ReservedSession{
		batchTestSession(checkpointID, "session-a"),
		batchTestSession(checkpointID, "session-b"),
		batchTestSession(checkpointID, "session-c"),
	}
	reversed := []ReservedSession{ascending[2], ascending[1], ascending[0]}

	repoA, _ := setupBranchTestRepo(t)
	storeA := NewGitStore(repoA, DefaultV1Refs())
	reqA := batchTestRequest(checkpointID, ascending...)
	reqA.CommitSubject = "canonical batch"
	require.NoError(t, storeA.Write(t.Context(), reqA))

	repoB, _ := setupBranchTestRepo(t)
	storeB := NewGitStore(repoB, DefaultV1Refs())
	reqB := batchTestRequest(checkpointID, reversed...)
	reqB.CommitSubject = reqA.CommitSubject
	require.NoError(t, storeB.Write(t.Context(), reqB))

	assert.Equal(t, branchCheckpointTree(t, storeA, checkpointID).Hash, branchCheckpointTree(t, storeB, checkpointID).Hash)
	summaryA, err := storeA.Read(t.Context(), checkpointID)
	require.NoError(t, err)
	summaryB, err := storeB.Read(t.Context(), checkpointID)
	require.NoError(t, err)
	assert.Equal(t, summaryA, summaryB)

	commitA := branchTipCommit(t, storeA)
	commitB := branchTipCommit(t, storeB)
	assert.Equal(t, commitA.Message, commitB.Message)
	assert.True(t, commitA.Author.When.Equal(batchTestTime))
	assert.True(t, commitA.Committer.When.Equal(batchTestTime))
	assert.Less(t,
		strings.Index(commitA.Message, trailers.SessionTrailerKey+": session-a"),
		strings.Index(commitA.Message, trailers.SessionTrailerKey+": session-c"),
	)
	assert.Equal(t, 1, strings.Count(commitA.Message, trailers.StrategyTrailerKey+":"))

	for index := range ascending {
		metadata, err := storeA.ReadSessionMetadata(t.Context(), checkpointID, index)
		require.NoError(t, err)
		assert.Equal(t, WriteOptions(ascending[index]).SessionID, metadata.SessionID)
	}
}

func branchTipCommit(t *testing.T, store *GitStore) *object.Commit {
	t.Helper()
	ref, err := store.repo.Reference(store.refs.Primary, true)
	require.NoError(t, err)
	commit, err := store.repo.CommitObject(ref.Hash())
	require.NoError(t, err)
	return commit
}

func TestBatchSessions_UpsertReusesIndexAndUntouchedSubtreeHashes(t *testing.T) {
	t.Parallel()
	repo, _ := setupBranchTestRepo(t)
	store := NewGitStore(repo, DefaultV1Refs())
	checkpointID := id.MustCheckpointID("b1b2c3d4e5f6")

	require.NoError(t, store.Write(t.Context(), batchTestRequest(checkpointID,
		batchTestSession(checkpointID, "session-a"),
		batchTestSession(checkpointID, "session-b"),
		batchTestSession(checkpointID, "session-c"),
	)))
	before := branchCheckpointTree(t, store, checkpointID)
	beforeA := treeEntryHash(t, before, "0")
	beforeB := treeEntryHash(t, before, "1")
	beforeC := treeEntryHash(t, before, "2")

	upsert := batchTestSession(checkpointID, "session-b")
	upsertOpts := WriteOptions(upsert)
	upsertOpts.Transcript = redact.AlreadyRedacted([]byte("replacement"))
	upsert = ReservedSession(upsertOpts)
	require.NoError(t, store.Write(t.Context(), batchTestRequest(checkpointID,
		batchTestSession(checkpointID, "session-e"),
		upsert,
		batchTestSession(checkpointID, "session-d"),
	)))

	after := branchCheckpointTree(t, store, checkpointID)
	assert.Equal(t, beforeA, treeEntryHash(t, after, "0"), "untouched session-a subtree changed")
	assert.NotEqual(t, beforeB, treeEntryHash(t, after, "1"), "upserted session-b subtree did not change")
	assert.Equal(t, beforeC, treeEntryHash(t, after, "2"), "untouched session-c subtree changed")

	wantIDs := []string{"session-a", "session-b", "session-c", "session-d", "session-e"}
	for index, wantID := range wantIDs {
		metadata, err := store.ReadSessionMetadata(t.Context(), checkpointID, index)
		require.NoError(t, err)
		assert.Equal(t, wantID, metadata.SessionID, "session index %d", index)
	}
}

func TestBatchSessions_ReducesCompleteFinalSetCanonically(t *testing.T) {
	t.Parallel()
	repo, _ := setupBranchTestRepo(t)
	store := NewGitStore(repo, DefaultV1Refs())
	checkpointID := id.MustCheckpointID("c1b2c3d4e5f6")

	legacy := WriteOptions(batchTestSession(checkpointID, "session-m"))
	legacy.CommitSHA = "legacy-root"
	legacy.HasReview = true
	legacy.HasInvestigation = true
	legacy.Kind = "imported"
	legacy.CombinedAttribution = &Attribution{AgentLines: 1}
	require.NoError(t, store.Write(t.Context(), batchTestRequest(checkpointID, ReservedSession(legacy))))
	initial, err := store.Read(t.Context(), checkpointID)
	require.NoError(t, err)
	assert.Nil(t, initial.CombinedAttribution, "BatchSessions must ignore nested combined attribution")
	require.NoError(t, store.Write(t.Context(), CheckpointAttribution{
		CheckpointID: checkpointID,
		Attribution:  &Attribution{AgentLines: 99},
	}))

	a := WriteOptions(batchTestSession(checkpointID, "session-a"))
	a.Strategy = "strategy-a"
	a.Branch = "branch-a"
	a.CommitSHA = "commit-a"
	a.FilesTouched = []string{"b.go", "a.go", "a.go"}
	a.TokenUsage = &types.TokenUsage{InputTokens: 1}
	a.CheckpointsCount = 1

	m := WriteOptions(batchTestSession(checkpointID, "session-m"))
	m.Strategy = "strategy-m"
	m.Branch = "branch-m"
	m.CommitSHA = "commit-m"
	m.FilesTouched = []string{"b.go"}
	m.TokenUsage = &types.TokenUsage{InputTokens: 2}
	m.CheckpointsCount = 2
	m.Kind = ""
	m.HasReview = false
	m.HasInvestigation = false

	z := WriteOptions(batchTestSession(checkpointID, "session-z"))
	z.Strategy = ""
	z.Branch = ""
	z.CommitSHA = ""
	z.FilesTouched = []string{"c.go", "a.go"}
	z.TokenUsage = &types.TokenUsage{InputTokens: 3}
	z.CheckpointsCount = 3

	require.NoError(t, store.Write(t.Context(), batchTestRequest(checkpointID,
		ReservedSession(z), ReservedSession(m), ReservedSession(a),
	)))
	summary, err := store.Read(t.Context(), checkpointID)
	require.NoError(t, err)
	require.NotNil(t, summary)
	assert.Empty(t, summary.Strategy, "greatest SessionID must be able to clear strategy")
	assert.Empty(t, summary.Branch, "greatest SessionID must be able to clear branch")
	assert.Equal(t, "commit-m", summary.CommitSHA, "greatest non-empty CommitSHA must win")
	assert.Equal(t, 6, summary.CheckpointsCount)
	assert.Equal(t, []string{"a.go", "b.go", "c.go"}, summary.FilesTouched)
	require.NotNil(t, summary.TokenUsage)
	assert.Equal(t, 6, summary.TokenUsage.InputTokens)
	require.NotNil(t, summary.CombinedAttribution)
	assert.Equal(t, 99, summary.CombinedAttribution.AgentLines)
	assert.True(t, summary.HasReview, "legacy root-level true must survive when metadata cannot rederive it")
	assert.True(t, summary.HasInvestigation, "legacy root-level true must survive when metadata cannot rederive it")
	assert.True(t, summary.Imported, "legacy root-level true must survive when metadata cannot rederive it")

	// A smaller one-session recovery upsert cannot override the untouched
	// lexicographically greatest sibling.
	a.Strategy = "recovery-override"
	a.Branch = "recovery-branch"
	require.NoError(t, store.Write(t.Context(), BatchSessions{
		CheckpointID: checkpointID,
		Sessions:     []ReservedSession{ReservedSession(a)},
		CommitTime:   batchTestTime.Add(time.Minute),
		AuthorName:   "Batch Author",
		AuthorEmail:  "batch@example.com",
	}))
	summary, err = store.Read(t.Context(), checkpointID)
	require.NoError(t, err)
	assert.Empty(t, summary.Strategy)
	assert.Empty(t, summary.Branch)
	assert.Equal(t, "commit-m", summary.CommitSHA)
}

func TestBatchSessions_ValidationFailsBeforePublication(t *testing.T) {
	t.Parallel()
	validID := id.MustCheckpointID("d1b2c3d4e5f6")
	otherID := id.MustCheckpointID("e1b2c3d4e5f6")
	task := TaskPayload{ToolUseID: "task-1", AgentID: "agent-1"}

	tests := map[string]BatchSessions{
		"missing top-level checkpoint ID": {
			Sessions: []ReservedSession{batchTestSession(validID, "session-a")},
		},
		"empty sessions": {CheckpointID: validID},
		"missing nested checkpoint ID": {
			CheckpointID: validID,
			Sessions:     []ReservedSession{ReservedSession(WriteOptions{SessionID: "session-a"})},
		},
		"mismatched nested checkpoint ID": {
			CheckpointID: validID,
			Sessions:     []ReservedSession{batchTestSession(otherID, "session-a")},
		},
		"invalid session ID": {
			CheckpointID: validID,
			Sessions:     []ReservedSession{batchTestSession(validID, "../session")},
		},
		"duplicate session ID": {
			CheckpointID: validID,
			Sessions: []ReservedSession{
				batchTestSession(validID, "session-a"),
				batchTestSession(validID, "session-a"),
			},
		},
		"duplicate task within session": {
			CheckpointID: validID,
			Sessions:     []ReservedSession{withBatchTasks(batchTestSession(validID, "session-a"), task, task)},
		},
		"duplicate task across sessions": {
			CheckpointID: validID,
			Sessions: []ReservedSession{
				withBatchTasks(batchTestSession(validID, "session-a"), task),
				withBatchTasks(batchTestSession(validID, "session-b"), task),
			},
		},
	}

	for name, req := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			for _, backend := range []string{"git-branch", "git-refs"} {
				t.Run(backend, func(t *testing.T) {
					t.Parallel()
					repo, _ := setupBranchTestRepo(t)
					var writer Writer
					if backend == "git-branch" {
						writer = NewGitStore(repo, DefaultV1Refs())
					} else {
						writer = newGitRefsStore(repo)
					}
					require.Error(t, writer.Write(t.Context(), req))
					assert.Empty(t, entireCheckpointReferences(t, repo), "invalid batch published a persistent ref")
				})
			}
		})
	}
}

func withBatchTasks(session ReservedSession, tasks ...TaskPayload) ReservedSession {
	opts := WriteOptions(session)
	opts.Tasks = tasks
	return ReservedSession(opts)
}

func entireCheckpointReferences(t *testing.T, repo *git.Repository) []string {
	t.Helper()
	iter, err := repo.References()
	require.NoError(t, err)
	defer iter.Close()
	var refs []string
	require.NoError(t, iter.ForEach(func(ref *plumbing.Reference) error {
		if strings.Contains(ref.Name().String(), "entire/checkpoints") {
			refs = append(refs, ref.Name().String())
		}
		return nil
	}))
	return refs
}

func TestBatchSessions_BackendsProduceEquivalentCheckpointContent(t *testing.T) {
	t.Parallel()
	repo, _ := setupBranchTestRepo(t)
	branch := NewGitStore(repo, DefaultV1Refs())
	refs := newGitRefsStore(repo)
	checkpointID := id.MustCheckpointID("f1b2c3d4e5f6")
	task := TaskPayload{ToolUseID: "task-1", AgentID: "agent-1", Transcript: redact.AlreadyRedacted([]byte("task transcript"))}
	sessions := []ReservedSession{
		withBatchTasks(batchTestSession(checkpointID, "session-a"), task),
		batchTestSession(checkpointID, "session-b"),
	}
	req := batchTestRequest(checkpointID, sessions...)
	require.NoError(t, branch.Write(t.Context(), req))
	require.NoError(t, refs.Write(t.Context(), req))

	branchTree := branchCheckpointTree(t, branch, checkpointID)
	refsTree := refsCheckpointTree(t, refs, checkpointID)
	for _, name := range []string{"0", "1", "tasks"} {
		assert.Equal(t, treeEntryHash(t, branchTree, name), treeEntryHash(t, refsTree, name), "%s subtree differs", name)
	}

	branchSummary, err := branch.Read(t.Context(), checkpointID)
	require.NoError(t, err)
	refsSummary, err := refs.Read(t.Context(), checkpointID)
	require.NoError(t, err)
	normalizeBatchSummaryPaths(branchSummary, checkpointID.Path())
	assert.Equal(t, branchSummary, refsSummary)
}

func normalizeBatchSummaryPaths(summary *CheckpointSummary, prefix string) {
	strip := func(value string) string {
		return strings.TrimPrefix(value, "/"+prefix)
	}
	for i := range summary.Sessions {
		session := &summary.Sessions[i]
		session.Metadata = strip(session.Metadata)
		session.Transcript = strip(session.Transcript)
		session.CompactTranscript = strip(session.CompactTranscript)
		session.ContentHash = strip(session.ContentHash)
		session.Prompt = strip(session.Prompt)
		session.AssetsManifest = strip(session.AssetsManifest)
	}
}

func TestBatchSessions_CheckpointContentVisitationIsLinear(t *testing.T) {
	t.Parallel()
	for _, sessionCount := range []int{100, 200, 500} {
		t.Run(fmt.Sprintf("%d sessions", sessionCount), func(t *testing.T) {
			t.Parallel()
			repo, _ := setupBranchTestRepo(t)
			store := NewGitStore(repo, DefaultV1Refs())
			checkpointID := id.MustCheckpointID("0123456789ab")
			sessions := make([]ReservedSession, sessionCount)
			for i := range sessionCount {
				sessions[i] = batchTestSession(checkpointID, fmt.Sprintf("session-%04d", i))
			}
			require.NoError(t, store.Write(t.Context(), batchTestRequest(checkpointID, sessions...)))

			prepared, err := store.prepareBatchSessions(t.Context(), batchTestRequest(checkpointID, sessions...))
			require.NoError(t, err)
			_, stats, err := store.applyPreparedBatch(t.Context(), prepared, branchCheckpointTree(t, store, checkpointID), checkpointID.Path()+"/")
			require.NoError(t, err)

			assert.Equal(t, sessionCount+1, stats.RootEntriesVisited)
			assert.Equal(t, sessionCount, stats.SessionEntriesVisited)
			assert.Equal(t, sessionCount, stats.ExistingMetadataRead)
			assert.Zero(t, stats.TaskEntriesVisited)
			assert.Equal(t, sessionCount, stats.FinalSessionContributions)
			assert.Equal(t, 4*sessionCount+1, stats.checkpointContentVisits())
		})
	}
}

func TestBatchSessions_CommitTrailersOnlyEmitCommonValues(t *testing.T) {
	t.Parallel()
	checkpointID := id.MustCheckpointID("1123456789ab")
	a := WriteOptions(batchTestSession(checkpointID, "session-a"))
	a.Agent = agent.AgentTypeClaudeCode
	a.EphemeralBranch = "entire/a"
	b := WriteOptions(batchTestSession(checkpointID, "session-b"))
	b.Agent = agent.AgentTypeCodex
	b.EphemeralBranch = "entire/b"

	canonical, err := CanonicalizeBatchSessions(batchTestRequest(checkpointID, ReservedSession(b), ReservedSession(a)))
	require.NoError(t, err)
	message := buildBatchCommitMessage(canonical)
	assert.Contains(t, message, trailers.StrategyTrailerKey+": manual-commit")
	assert.NotContains(t, message, trailers.AgentTrailerKey+":")
	assert.NotContains(t, message, trailers.EphemeralBranchTrailerKey+":")
	assert.Less(t, strings.Index(message, "session-a"), strings.Index(message, "session-b"))
}

func TestBatchSessions_ZeroTimesShareOneCapturedUTCInstant(t *testing.T) {
	t.Parallel()
	repo, _ := setupBranchTestRepo(t)
	store := NewGitStore(repo, DefaultV1Refs())
	checkpointID := id.MustCheckpointID("2123456789ab")
	a := WriteOptions(batchTestSession(checkpointID, "session-a"))
	a.CreatedAt = time.Time{}
	b := WriteOptions(batchTestSession(checkpointID, "session-b"))
	b.CreatedAt = time.Time{}

	before := time.Now().UTC()
	require.NoError(t, store.Write(t.Context(), BatchSessions{
		CheckpointID: checkpointID,
		Sessions:     []ReservedSession{ReservedSession(b), ReservedSession(a)},
	}))
	after := time.Now().UTC()
	commit := branchTipCommit(t, store)
	assert.False(t, commit.Author.When.Before(before.Truncate(time.Second)))
	assert.False(t, commit.Author.When.After(after))
	for index := range 2 {
		metadata, err := store.ReadSessionMetadata(t.Context(), checkpointID, index)
		require.NoError(t, err)
		assert.True(t, commit.Author.When.Equal(metadata.CreatedAt))
		assert.Equal(t, time.UTC, metadata.CreatedAt.Location())
	}
}

func TestBatchSessions_CrossSessionTaskCollisionLeavesExistingTasksUntouched(t *testing.T) {
	t.Parallel()
	repo, _ := setupBranchTestRepo(t)
	store := NewGitStore(repo, DefaultV1Refs())
	checkpointID := id.MustCheckpointID("3123456789ab")
	task := TaskPayload{ToolUseID: "task-existing", AgentID: "agent-1", Transcript: redact.AlreadyRedacted([]byte("existing"))}
	require.NoError(t, store.Write(t.Context(), batchTestRequest(checkpointID,
		withBatchTasks(batchTestSession(checkpointID, "session-a"), task),
	)))
	before := treeEntryHash(t, branchCheckpointTree(t, store, checkpointID), "tasks")

	duplicate := TaskPayload{ToolUseID: "task-duplicate", AgentID: "agent-2"}
	err := store.Write(context.Background(), batchTestRequest(checkpointID,
		withBatchTasks(batchTestSession(checkpointID, "session-b"), duplicate),
		withBatchTasks(batchTestSession(checkpointID, "session-c"), duplicate),
	))
	require.Error(t, err)
	after := treeEntryHash(t, branchCheckpointTree(t, store, checkpointID), "tasks")
	assert.Equal(t, before, after)
}

func TestBatchSessions_SingleSessionAdapterPreservesCombinedAttribution(t *testing.T) {
	t.Parallel()
	repo, _ := setupBranchTestRepo(t)
	store := NewGitStore(repo, DefaultV1Refs())
	checkpointID := id.MustCheckpointID("4123456789ab")
	opts := WriteOptions(batchTestSession(checkpointID, "session-a"))
	opts.CombinedAttribution = &Attribution{AgentLines: 17}
	require.NoError(t, store.Write(t.Context(), Session(opts)))

	summary, err := store.Read(t.Context(), checkpointID)
	require.NoError(t, err)
	require.NotNil(t, summary.CombinedAttribution)
	assert.Equal(t, 17, summary.CombinedAttribution.AgentLines)
}
