package cli

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	checkpointid "github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/remote"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
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

// runAttributionGit runs a git command in dir with the isolated test env.
func runAttributionGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", args...)
	cmd.Dir = dir
	cmd.Env = testutil.GitIsolatedEnv()
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "git %v: %s", args, out)
}

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

// A refresh that succeeds but still reports the checkpoint as NOT FOUND means
// the remote genuinely doesn't have it: the reason must say "not pushed", and
// must NOT suggest a git fetch (the user just did the equivalent).
func TestReadCheckpointContextRefreshedButAbsentSaysNotPushed(t *testing.T) {
	cpID := checkpointid.MustCheckpointID("ccb2c3d4e5f6")
	// summary nil + readErr nil → Read returns (nil, nil) → the store reports
	// the genuine-absence sentinel (checkpoint.ErrCheckpointNotFound).
	missing := &attributionCheckpointReaderStub{}
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

// A refresh that succeeds while the summary read keeps failing for a
// NON-absence reason (storage error, cancellation) proves nothing about the
// remote: the reason must not claim "not pushed".
func TestReadCheckpointContextRefreshedNonAbsenceErrorMakesNoRemoteClaim(t *testing.T) {
	cpID := checkpointid.MustCheckpointID("ceb2c3d4e5f6")
	broken := &attributionCheckpointReaderStub{readErr: errors.New("packfile corrupt")}
	overrideAttributionRefresh(t,
		func(context.Context) error { return nil },
		func(context.Context) (attributionCheckpointReader, *git.Repository, error) {
			return broken, nil, nil
		})

	resolver := newStubAttributionResolver(broken)
	resolver.fetchOnMiss = true
	ctx := resolver.readCheckpointContext(cpID, "auth.py")

	require.True(t, ctx.MetadataMissing)
	require.Contains(t, ctx.MetadataMissingReason, "packfile corrupt")
	require.NotContains(t, ctx.MetadataMissingReason, "may not have been pushed",
		"a read error is not evidence the remote lacks the checkpoint")
}

// Multi-line fetch errors (git output embeds newlines) must not break the
// one-line reason rendering.
func TestMissReasonCollapsesMultilineErrors(t *testing.T) {
	overrideAttributionRefresh(t,
		func(context.Context) error {
			return errors.New("fatal: could not read\nUsername for 'https://github.com'")
		},
		func(context.Context) (attributionCheckpointReader, *git.Repository, error) {
			t.Fatal("open must not run when the fetch fails")
			return nil, nil, nil
		})

	resolver := newStubAttributionResolver(&attributionCheckpointReaderStub{readErr: errors.New("object not found")})
	resolver.fetchOnMiss = true
	ctx := resolver.readCheckpointContext(checkpointid.MustCheckpointID("cfb2c3d4e5f6"), "auth.py")

	require.True(t, ctx.MetadataMissing)
	require.Contains(t, ctx.MetadataMissingReason, "could not read")
	require.NotContains(t, ctx.MetadataMissingReason, "\n", "reason must render as a single line")
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
// local data rather than adopting the emptier remote view, AND the reason must
// use what the retry proved: the remote lacks the checkpoint entirely, so it
// must not speculate that "the metadata branch may have been pushed without"
// the session records.
func TestReadCheckpointContextKeepsLocalWhenRefreshReadsWorse(t *testing.T) {
	cpID := checkpointid.MustCheckpointID("ffb2c3d4e5f6")
	// Local: summary readable, sessions unreadable (partial). Remote: reports
	// genuine absence (nil,nil stub → checkpoint.ErrCheckpointNotFound).
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
			return &attributionCheckpointReaderStub{}, nil, nil
		})

	resolver := newStubAttributionResolver(local)
	resolver.fetchOnMiss = true
	ctx := resolver.readCheckpointContext(cpID, "auth.py")

	require.True(t, ctx.MetadataMissing, "session records are still unreadable")
	require.Equal(t, []string{"auth.py"}, ctx.FilesTouched, "local summary data must survive a worse remote read")
	require.Contains(t, ctx.MetadataMissingReason, "not on the remote",
		"the retry proved the remote lacks the checkpoint — say so")
	require.NotContains(t, ctx.MetadataMissingReason, "pushed without them",
		"must not speculate about remote-side truncation the retry disproved")
}

// A rejected-worse retry whose failure is NOT genuine absence (corrupt fetched
// pack, cancellation) proves nothing about the remote: the reason must stay
// neutral and surface the retry's error rather than speculating the metadata
// branch "may have been pushed without" the session records.
func TestReadCheckpointContextRejectedRetryNonAbsenceStaysNeutral(t *testing.T) {
	cpID := checkpointid.MustCheckpointID("fdb2c3d4e5f6")
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
			return &attributionCheckpointReaderStub{readErr: errors.New("packfile corrupt")}, nil, nil
		})

	resolver := newStubAttributionResolver(local)
	resolver.fetchOnMiss = true
	ctx := resolver.readCheckpointContext(cpID, "auth.py")

	require.True(t, ctx.MetadataMissing)
	require.Contains(t, ctx.MetadataMissingReason, "packfile corrupt",
		"the rejected retry's error must be surfaced, not discarded")
	require.NotContains(t, ctx.MetadataMissingReason, "pushed without them",
		"a failed re-read is not evidence about the remote's contents")
	require.NotContains(t, ctx.MetadataMissingReason, "not on the remote",
		"a non-absence failure is not proof the remote lacks the checkpoint")
}

// The determinism contract of #1551 at the unit level: two "clones" with
// DIFFERENT local damage (one can't read the summary, the other can't read the
// session records) must converge on the identical complete context once both
// refresh from the same remote.
func TestReadCheckpointContextDeterministicAcrossResolvers(t *testing.T) {
	cpID := checkpointid.MustCheckpointID("abb2c3d4e5f6")
	overrideAttributionRefresh(t,
		func(context.Context) error { return nil },
		func(context.Context) (attributionCheckpointReader, *git.Repository, error) {
			return fullyReadableStub("session-same"), nil, nil
		})

	cloneA := newStubAttributionResolver(&attributionCheckpointReaderStub{readErr: errors.New("object not found")})
	cloneA.fetchOnMiss = true
	cloneB := newStubAttributionResolver(&attributionCheckpointReaderStub{
		summary: &checkpoint.CheckpointSummary{
			Sessions: []checkpoint.SessionFilePaths{{Metadata: "metadata.json"}},
		},
		sessionErr: errors.New("object not found"),
	})
	cloneB.fetchOnMiss = true

	first := cloneA.readCheckpointContext(cpID, "auth.py")
	second := cloneB.readCheckpointContext(cpID, "auth.py")
	require.False(t, first.MetadataMissing)
	require.Equal(t, first, second, "differently-damaged clones must converge on the remote's answer")
}

// A multi-session checkpoint with only SOME session records readable locally is
// incomplete: the fallback session would otherwise depend on which blobs a
// clone happens to have. The refresh must run and the retry must resolve the
// file-matching session.
func TestReadCheckpointContextPartialSessionsTriggerRefresh(t *testing.T) {
	cpID := checkpointid.MustCheckpointID("adb2c3d4e5f6")
	twoSessions := []checkpoint.SessionFilePaths{{Metadata: "0/metadata.json"}, {Metadata: "1/metadata.json"}}
	matching := &checkpoint.Metadata{
		SessionID:    "session-matching",
		FilesTouched: []string{"auth.py"},
		Agent:        agent.AgentTypeClaudeCode,
	}
	other := &checkpoint.Metadata{
		SessionID:    "session-other",
		FilesTouched: []string{"unrelated.go"},
		Agent:        agent.AgentTypeClaudeCode,
	}
	// Locally the file-matching session's record is unreadable; only the
	// non-matching one reads, so a no-refresh resolver would silently pick it.
	local := &attributionCheckpointReaderStub{
		summary: &checkpoint.CheckpointSummary{Sessions: twoSessions},
		sessions: []attributionSessionStub{
			{err: errors.New("object not found")},
			{meta: other, prompts: "Refactor helpers."},
		},
	}
	remote := &attributionCheckpointReaderStub{
		summary: &checkpoint.CheckpointSummary{Sessions: twoSessions},
		sessions: []attributionSessionStub{
			{meta: matching, prompts: "Fix the authentication bug."},
			{meta: other, prompts: "Refactor helpers."},
		},
	}
	overrideAttributionRefresh(t,
		func(context.Context) error { return nil },
		func(context.Context) (attributionCheckpointReader, *git.Repository, error) {
			return remote, nil, nil
		})

	resolver := newStubAttributionResolver(local)
	resolver.fetchOnMiss = true
	ctx := resolver.readCheckpointContext(cpID, "auth.py")

	require.False(t, ctx.MetadataMissing)
	require.Equal(t, "session-matching", ctx.SessionID, "the refresh must restore the file-matching session")
	require.False(t, ctx.SessionFallback, "a matched file must not be flagged as a fallback guess")
}

// A successful refresh must also run at most once: two different missing
// checkpoints share one fetch and one store reopen.
func TestRefreshMetadataStoreSuccessRunsAtMostOnce(t *testing.T) {
	fetchCalls, openCalls := 0, 0
	overrideAttributionRefresh(t,
		func(context.Context) error { fetchCalls++; return nil },
		func(context.Context) (attributionCheckpointReader, *git.Repository, error) {
			openCalls++
			return fullyReadableStub("session-once"), nil, nil
		})

	resolver := newStubAttributionResolver(&attributionCheckpointReaderStub{readErr: errors.New("object not found")})
	resolver.fetchOnMiss = true
	first := resolver.readCheckpointContext(checkpointid.MustCheckpointID("aeb2c3d4e5f6"), "auth.py")
	second := resolver.readCheckpointContext(checkpointid.MustCheckpointID("afb2c3d4e5f6"), "auth.py")

	require.Equal(t, 1, fetchCalls)
	require.Equal(t, 1, openCalls)
	require.False(t, first.MetadataMissing)
	require.False(t, second.MetadataMissing)
}

// Sessions unreadable even after a successful refresh: the reason must say the
// records are unavailable — not claim the checkpoint "may not have been pushed"
// or "predates checkpointing", both of which the just-read summary disproves.
func TestReadCheckpointContextSessionsUnreadableAfterRefreshWording(t *testing.T) {
	cpID := checkpointid.MustCheckpointID("bcb2c3d4e5f6")
	broken := &attributionCheckpointReaderStub{
		summary: &checkpoint.CheckpointSummary{
			Sessions: []checkpoint.SessionFilePaths{{Metadata: "metadata.json"}},
		},
		sessionErr: errors.New("object not found"),
	}
	overrideAttributionRefresh(t,
		func(context.Context) error { return nil },
		func(context.Context) (attributionCheckpointReader, *git.Repository, error) {
			return broken, nil, nil // remote is equally damaged
		})

	resolver := newStubAttributionResolver(broken)
	resolver.fetchOnMiss = true
	ctx := resolver.readCheckpointContext(cpID, "auth.py")

	require.True(t, ctx.MetadataMissing)
	require.Contains(t, ctx.MetadataMissingReason, "still unavailable after refreshing")
	require.NotContains(t, ctx.MetadataMissingReason, "may not have been pushed",
		"the summary resolved, so the checkpoint IS on the metadata branch")
	require.NotContains(t, ctx.MetadataMissingReason, "predates checkpointing")
}

// The authoritative-remote rule, pinned on the real seam: when a checkpoint
// remote IS configured, its fetch failure must surface as the refresh outcome —
// wrapped as a checkpoint-remote failure, with no origin fallback masking it
// and no "may not have been pushed" claim about a remote never reached.
func TestRefreshConfiguredCheckpointRemoteFailureIsAuthoritative(t *testing.T) {
	repoRoot := newAttributionRepo(t)
	// Hermeticity: with ENTIRE_CHECKPOINT_TOKEN set, FetchURL derives a real
	// github.com URL without any git remote and the test would hit the network
	// with the environment's token. Clear it so resolution fails fast locally.
	t.Setenv(remote.CheckpointTokenEnvVar, "")
	// A configured checkpoint remote in a repo with no origin: URL resolution
	// fails fast (no network), exercising the authoritative-failure path.
	require.NoError(t, os.MkdirAll(filepath.Join(repoRoot, ".entire"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, ".entire", "settings.json"),
		[]byte(`{"enabled":true,"strategy_options":{"checkpoint_remote":{"provider":"github","repo":"org/checkpoints"}}}`), 0o644))

	resolver := newStubAttributionResolver(&attributionCheckpointReaderStub{readErr: errors.New("object not found")})
	resolver.fetchOnMiss = true
	ctx := resolver.readCheckpointContext(checkpointid.MustCheckpointID("dcb2c3d4e5f6"), "auth.py")

	require.True(t, ctx.MetadataMissing)
	require.Contains(t, ctx.MetadataMissingReason, "remote refresh failed")
	require.Contains(t, ctx.MetadataMissingReason, "checkpoint remote fetch failed",
		"the configured checkpoint remote's failure must be the surfaced outcome, not masked by an origin fallback")
	require.NotContains(t, ctx.MetadataMissingReason, "may not have been pushed",
		"we never reached the checkpoint remote, so nothing is known about what it contains")
}

// The token-path evasion, pinned on the real seam: with ENTIRE_CHECKPOINT_TOKEN
// set and a checkpoint_remote whose provider can't be resolved, FetchURL falls
// back to a token-rewritten origin URL that no string comparison would catch.
// The explicit source signal must treat that as a refresh failure — never a
// "successful" refresh that would fabricate not-pushed evidence about a
// checkpoint remote that was never contacted. Hermetic: the guard rejects
// before any fetch runs.
func TestRefreshTokenPathOriginFallbackIsNotSuccess(t *testing.T) {
	repoRoot := newAttributionRepo(t)
	runAttributionGit(t, repoRoot, "remote", "add", "origin", "git@github.com:org/app.git")
	require.NoError(t, os.MkdirAll(filepath.Join(repoRoot, ".entire"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, ".entire", "settings.json"),
		[]byte(`{"enabled":true,"strategy_options":{"checkpoint_remote":{"provider":"bitbucket","repo":"org/checkpoints"}}}`), 0o644))
	t.Setenv(remote.CheckpointTokenEnvVar, "test-token")

	resolver := newStubAttributionResolver(&attributionCheckpointReaderStub{readErr: errors.New("object not found")})
	resolver.fetchOnMiss = true
	ctx := resolver.readCheckpointContext(checkpointid.MustCheckpointID("ecb2c3d4e5f6"), "auth.py")

	require.True(t, ctx.MetadataMissing)
	require.Contains(t, ctx.MetadataMissingReason, "remote refresh failed")
	require.Contains(t, ctx.MetadataMissingReason, "fell back to origin",
		"the token-path origin fallback must surface as a failure, not fake a checkpoint-remote fetch")
	require.NotContains(t, ctx.MetadataMissingReason, "may not have been pushed",
		"we never reached the checkpoint remote, so nothing is known about what it contains")
	// The appended recovery suggestion must not contradict the failure by
	// telling the user to fetch from the very origin URL the refresh refused.
	require.NotContains(t, ctx.MetadataMissingReason, "git fetch https://github.com/org/app.git",
		"suggesting a fetch from origin would contradict the authoritative-remote policy")
	require.Contains(t, ctx.MetadataMissingReason, "checkpoint_remote",
		"the recovery guidance should point at the configuration that failed to resolve")
}

// Regression pin for the unsound-refresh bug: in a repo with a LOCAL metadata
// branch but no reachable remote, the refresh must report failure (surfacing
// the fetch error and the git fetch suggestion) — not silently fall back to the
// stale local branch and then claim the checkpoint "may not have been pushed".
// Uses the real fetchAttributionMetadata seam.
func TestRefreshWithLocalBranchButNoRemoteReportsFetchFailure(t *testing.T) {
	repoRoot := newAttributionRepo(t)
	// Hermeticity: ensure a developer/CI ENTIRE_CHECKPOINT_TOKEN can't turn
	// this into a network-dependent test.
	t.Setenv(remote.CheckpointTokenEnvVar, "")
	// A local entire/checkpoints/v1 exists (the old getMetadataTree fallback
	// would "succeed" from it); the queried checkpoint is not in it.
	writeAttributionCheckpoint(t, repoRoot, "99b2c3d4e5f6", checkpoint.WriteOptions{
		SessionID:    "session-local-only",
		Prompts:      []string{"unrelated"},
		FilesTouched: []string{"auth.py"},
		Agent:        agent.AgentTypeClaudeCode,
	})

	resolver := newStubAttributionResolver(&attributionCheckpointReaderStub{readErr: errors.New("object not found")})
	resolver.fetchOnMiss = true
	ctx := resolver.readCheckpointContext(checkpointid.MustCheckpointID("cdb2c3d4e5f6"), "auth.py")

	require.True(t, ctx.MetadataMissing)
	require.Contains(t, ctx.MetadataMissingReason, "remote refresh failed",
		"no remote is configured, so the refresh must report failure, not local-fallback success")
	require.Contains(t, ctx.MetadataMissingReason, "git fetch ")
	require.NotContains(t, ctx.MetadataMissingReason, "may not have been pushed",
		"we never reached a remote, so nothing is known about what it contains")
}
