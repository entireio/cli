package cli

import (
	"context"
	"errors"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	checkpointid "github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/go-git/go-git/v6"
	"github.com/stretchr/testify/require"
)

// Tests for issue #1551: `entire why` must resolve checkpoint metadata
// deterministically across equivalent clones — refreshing from the remote when
// the local read is incomplete, at most once per resolver, and reporting a
// precise reason (fetch failed vs not pushed vs run-the-fetch-yourself) when it
// still can't.
//
// These override the package-level fetch/reopen seams, so they must not run in
// parallel with each other.

// overrideAttributionRefresh swaps the refresh seams for the duration of a test.
func overrideAttributionRefresh(t *testing.T, fetch func(context.Context) error, open func(context.Context) (attributionCheckpointReader, *git.Repository, error)) {
	t.Helper()
	oldFetch, oldOpen := fetchAttributionMetadata, openAttributionStore
	fetchAttributionMetadata, openAttributionStore = fetch, open
	t.Cleanup(func() {
		fetchAttributionMetadata, openAttributionStore = oldFetch, oldOpen
	})
}

func fullyReadableStub(sessionID string) *attributionCheckpointReaderStub {
	return &attributionCheckpointReaderStub{
		summary: &checkpoint.CheckpointSummary{
			FilesTouched: []string{"auth.py"},
			Sessions:     []checkpoint.SessionFilePaths{{Metadata: "metadata.json"}},
		},
		content: &checkpoint.SessionContent{
			Metadata: checkpoint.Metadata{
				SessionID:    sessionID,
				FilesTouched: []string{"auth.py"},
				Agent:        agent.AgentTypeClaudeCode,
				Model:        "claude-test",
			},
			Prompts: "Fix the authentication bug.",
		},
	}
}

// The treeless-clone / gc-pruned case: the checkpoint summary resolves but every
// per-session record is unreadable. Previously this set MetadataMissing with NO
// reason and never attempted a remote refresh — the reason must now be present
// even on the no-fetch (blame) path.
func TestReadCheckpointContextSessionsUnreadableGetsReason(t *testing.T) {
	t.Parallel()
	cpID := checkpointid.MustCheckpointID("aab2c3d4e5f6")
	reader := &attributionCheckpointReaderStub{
		summary: &checkpoint.CheckpointSummary{
			Sessions: []checkpoint.SessionFilePaths{{Metadata: "metadata.json"}},
		},
		sessionErr: errors.New("object not found"),
	}

	// fetchOnMiss=false (the blame path): no refresh, but the reason must still
	// name the cause and the recovery command.
	ctx := newStubAttributionResolver(reader).readCheckpointContext(cpID, "auth.py")
	require.True(t, ctx.MetadataMissing)
	require.Contains(t, ctx.MetadataMissingReason, "session record")
	require.Contains(t, ctx.MetadataMissingReason, "object not found")
	require.Contains(t, ctx.MetadataMissingReason, "git fetch ")
}

// With fetchOnMiss, unreadable session records must trigger the remote refresh,
// and a refresh that restores the blobs must yield full metadata — the
// cross-clone convergence at the heart of #1551.
func TestReadCheckpointContextRefreshRestoresUnreadableSessions(t *testing.T) {
	cpID := checkpointid.MustCheckpointID("bbb2c3d4e5f6")
	broken := &attributionCheckpointReaderStub{
		summary: &checkpoint.CheckpointSummary{
			Sessions: []checkpoint.SessionFilePaths{{Metadata: "metadata.json"}},
		},
		sessionErr: errors.New("object not found"),
	}
	overrideAttributionRefresh(t,
		func(context.Context) error { return nil },
		func(context.Context) (attributionCheckpointReader, *git.Repository, error) {
			return fullyReadableStub("session-refreshed"), nil, nil
		})

	resolver := newStubAttributionResolver(broken)
	resolver.fetchOnMiss = true
	ctx := resolver.readCheckpointContext(cpID, "auth.py")

	require.False(t, ctx.MetadataMissing, "refresh restored the session records")
	require.Empty(t, ctx.MetadataMissingReason)
	require.Equal(t, "session-refreshed", ctx.SessionID)
	require.Equal(t, "Claude Code", ctx.Agent)
}

// A refresh that succeeds but still lacks the checkpoint means the remote
// genuinely doesn't have it: the reason must say "not pushed", and must NOT
// suggest a git fetch (the user just did the equivalent).
func TestReadCheckpointContextRefreshedButAbsentSaysNotPushed(t *testing.T) {
	cpID := checkpointid.MustCheckpointID("ccb2c3d4e5f6")
	missing := &attributionCheckpointReaderStub{readErr: errors.New("object not found")}
	overrideAttributionRefresh(t,
		func(context.Context) error { return nil },
		func(context.Context) (attributionCheckpointReader, *git.Repository, error) {
			return missing, nil, nil
		})

	resolver := newStubAttributionResolver(missing)
	resolver.fetchOnMiss = true
	ctx := resolver.readCheckpointContext(cpID, "auth.py")

	require.True(t, ctx.MetadataMissing)
	require.Contains(t, ctx.MetadataMissingReason, "may not have been pushed")
	require.NotContains(t, ctx.MetadataMissingReason, "git fetch", "fetching again cannot help when the remote lacks the checkpoint")
}

// The refresh must run at most once per resolver, however many checkpoints
// miss: a file whose lines reference many unfetched checkpoints must not pay
// one network attempt per checkpoint.
func TestRefreshMetadataStoreRunsAtMostOnce(t *testing.T) {
	fetchCalls := 0
	overrideAttributionRefresh(t,
		func(context.Context) error { fetchCalls++; return errors.New("network unreachable") },
		func(context.Context) (attributionCheckpointReader, *git.Repository, error) {
			t.Fatal("openAttributionStore must not be called when the fetch fails")
			return nil, nil, nil
		})

	resolver := newStubAttributionResolver(&attributionCheckpointReaderStub{readErr: errors.New("object not found")})
	resolver.fetchOnMiss = true
	first := resolver.readCheckpointContext(checkpointid.MustCheckpointID("ddb2c3d4e5f6"), "auth.py")
	second := resolver.readCheckpointContext(checkpointid.MustCheckpointID("eeb2c3d4e5f6"), "auth.py")

	require.Equal(t, 1, fetchCalls, "one refresh serves (or fails) the whole resolver run")
	require.True(t, first.MetadataMissing)
	require.True(t, second.MetadataMissing)
	require.Contains(t, first.MetadataMissingReason, "remote refresh failed")
	require.Contains(t, first.MetadataMissingReason, "network unreachable")
	require.Contains(t, second.MetadataMissingReason, "remote refresh failed",
		"the memoized refresh outcome must be reflected in every subsequent miss")
}

// A checkpoint that reads (partially) from the LOCAL store but is absent from
// the refreshed store — the author's own unpushed checkpoint — must keep the
// local data rather than adopting the emptier remote view.
func TestReadCheckpointContextKeepsLocalWhenRefreshReadsWorse(t *testing.T) {
	cpID := checkpointid.MustCheckpointID("ffb2c3d4e5f6")
	// Local: summary readable, sessions unreadable (partial). Remote: nothing.
	local := &attributionCheckpointReaderStub{
		summary: &checkpoint.CheckpointSummary{
			FilesTouched: []string{"auth.py"},
			Sessions:     []checkpoint.SessionFilePaths{{Metadata: "metadata.json"}},
		},
		sessionErr: errors.New("object not found"),
	}
	overrideAttributionRefresh(t,
		func(context.Context) error { return nil },
		func(context.Context) (attributionCheckpointReader, *git.Repository, error) {
			return &attributionCheckpointReaderStub{readErr: errors.New("not on remote")}, nil, nil
		})

	resolver := newStubAttributionResolver(local)
	resolver.fetchOnMiss = true
	ctx := resolver.readCheckpointContext(cpID, "auth.py")

	require.True(t, ctx.MetadataMissing, "session records are still unreadable")
	require.Equal(t, []string{"auth.py"}, ctx.FilesTouched, "local summary data must survive a worse remote read")
	require.Contains(t, ctx.MetadataMissingReason, "session record")
}

// Two resolver runs against the same stores must produce identical contexts for
// the same checkpoint — the determinism contract of #1551 at the unit level.
func TestReadCheckpointContextDeterministicAcrossResolvers(t *testing.T) {
	cpID := checkpointid.MustCheckpointID("abb2c3d4e5f6")
	overrideAttributionRefresh(t,
		func(context.Context) error { return nil },
		func(context.Context) (attributionCheckpointReader, *git.Repository, error) {
			return fullyReadableStub("session-same"), nil, nil
		})

	makeCtx := func() attributionCheckpointContext {
		resolver := newStubAttributionResolver(&attributionCheckpointReaderStub{readErr: errors.New("object not found")})
		resolver.fetchOnMiss = true
		return resolver.readCheckpointContext(cpID, "auth.py")
	}
	first := makeCtx()
	second := makeCtx()
	require.Equal(t, first, second)
}
