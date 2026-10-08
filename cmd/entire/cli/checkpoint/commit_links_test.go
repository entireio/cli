package checkpoint

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/redact"
)

func TestCheckpointCommitLinks_Routing(t *testing.T) {
	t.Parallel()
	for _, primary := range []string{BackendTypeGitBranch, BackendTypeGitRefs} {
		t.Run(primary, func(t *testing.T) {
			t.Parallel()
			_, repo, _ := newTestRepo(t)
			branch := NewGitStore(repo, DefaultV1Refs())
			refs := newGitRefsStore(repo)
			var target, other PersistentStore = refs, branch
			cid := id.MustCheckpointID(routingSampleULID)
			if primary == BackendTypeGitRefs {
				target, other = branch, refs
				cid = id.MustCheckpointID("a1b2c3d4e5f6")
			}
			writeRoutingCheckpoint(t, target, cid, "original backend")
			mirror := &fakeMirror{}
			router := newKindRoutingStore(newFanoutStore(other, []Writer{mirror}), branch, refs, primary)
			links := []LinkedCommit{{SHA: strings.Repeat("a", 40)}}
			require.NoError(t, router.Write(t.Context(), CheckpointCommitLinks{CheckpointID: cid, Links: links}))
			summary, err := router.Read(t.Context(), cid)
			require.NoError(t, err)
			require.Equal(t, links, summary.LinkedCommits)
			require.Empty(t, mirror.writes, "fallback writes must not reach primary mirrors")
		})
	}
}

func TestCheckpointCommitLinks_Backends(t *testing.T) {
	t.Parallel()
	for _, backend := range []string{"git-branch", "git-refs"} {
		t.Run(backend, func(t *testing.T) {
			t.Parallel()
			var store PersistentStore
			if backend == "git-branch" {
				repo, _ := setupBranchTestRepo(t)
				t.Cleanup(func() { _ = repo.Close() })
				store = NewGitStore(repo, DefaultV1Refs())
			} else {
				refs := newRefsStore(t)
				t.Cleanup(func() { _ = refs.repo.Close() })
				store = refs
			}
			ctx := context.Background()
			cid := id.MustCheckpointID("a1b2c3d4e5f6")
			links := []LinkedCommit{{SHA: strings.Repeat("a", 40), Repo: "gh/owner/repo"}, {SHA: strings.Repeat("b", 40)}}
			req := CheckpointCommitLinks{CheckpointID: cid, Links: links}
			require.ErrorIs(t, store.Write(ctx, req), ErrCheckpointNotFound)
			write := Session{CheckpointID: cid, SessionID: "s1", Strategy: "manual-commit", Transcript: redact.AlreadyRedacted([]byte("original\n")), AuthorName: "Test", AuthorEmail: "test@example.com"}
			require.NoError(t, store.Write(ctx, write))
			require.NoError(t, store.Write(ctx, req))
			require.NoError(t, store.Write(ctx, req), "repeated repair must not duplicate associations")
			require.Error(t, store.Write(ctx, CheckpointCommitLinks{CheckpointID: cid, Links: []LinkedCommit{{SHA: "bad"}}}))
			require.NoError(t, store.Write(ctx, write), "session rewrite must preserve root links")
			require.NoError(t, store.Write(ctx, CheckpointAttribution{CheckpointID: cid, Attribution: &Attribution{AgentLines: 3}}))
			summary, err := store.Read(ctx, cid)
			require.NoError(t, err)
			require.Equal(t, links, summary.LinkedCommits)
			require.Len(t, summary.Sessions, 1)
			content, err := store.ReadSessionContent(ctx, cid, 0)
			require.NoError(t, err)
			require.Equal(t, "original\n", string(content.Transcript))
			infos, err := store.List(ctx)
			require.NoError(t, err)
			require.Equal(t, []id.CheckpointID{cid}, CheckpointsForCommit(infos, links[0].SHA, nil))
			if refs, ok := store.(*gitRefsStore); ok {
				queue, err := PushQueueForRepo(ctx, refs.repo)
				require.NoError(t, err)
				pending, err := queue.Drain()
				require.NoError(t, err)
				require.Contains(t, pending, mustRefName(t, cid))
			}
		})
	}
}
