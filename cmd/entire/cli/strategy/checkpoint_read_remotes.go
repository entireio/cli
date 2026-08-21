package strategy

import (
	"context"
	"log/slog"

	"github.com/entireio/cli/cmd/entire/cli/logging"
)

// CheckpointReadRemotes returns the ordered, deduped remotes that checkpoint
// READS consult: the elected sync remote first, then "origin" as the legacy
// tier (pre-single-remote-sync checkpoints live there, and a fresh clone
// may lack the local settings that elected a non-origin remote).
//
// Unlike the write-side election, this fails OPEN: on an election error
// (misconfigured checkpoint_push_remote, unreadable settings) the chain is
// ["origin"] when configured. Writes fail closed to prevent leaking data;
// failing reads closed would only prevent FINDING data, with no privacy
// benefit. Callers must uphold the read-only rule for every candidate after
// the first: only the elected remote may seed or advance local refs.
//
// An empty result means no candidates — callers needing the "checkpoint
// absent" classification must gather their own positive evidence (successful
// remote listing, readable settings); an empty chain alone is not proof.
//
// The election itself is deliberately un-memoized: every call re-runs it, so a
// settings change or a captured election written mid-invocation is observed
// immediately. What IS memoized, per invocation, are the two .git/config reads
// underneath it — which remotes exist, and does remote X exist (see
// git_remote_cache.go). That split is load-bearing: caching the election result
// would make #1991's capture invisible to the gate that runs right after it.
func CheckpointReadRemotes(ctx context.Context) []string {
	return CheckpointReadRemotesWithElection(ctx).Candidates
}

// CheckpointReadResolution is the read-candidate chain bundled with the
// election result it was derived from.
type CheckpointReadResolution struct {
	// Candidates is the ordered read-candidate chain (see
	// CheckpointReadRemotes).
	Candidates []string
	// ElectedName is the elected checkpoint sync remote; "" when the election
	// failed or elected nothing. Only this remote may seed or advance local
	// refs.
	ElectedName string
	// ElectionErr records a failed election. It is advisory for readers (the
	// chain fail-opens to ["origin"] when configured); write-adjacent callers
	// may surface it when they need a push target.
	ElectionErr error
}

// CheckpointReadRemotesWithElection returns the read-candidate chain together
// with the election result it was derived from, so callers that need both —
// e.g. to confine local-ref advancement to the elected remote while iterating
// the chain — make ONE election call instead of two that could disagree if
// settings or remotes change mid-operation. See CheckpointReadRemotes for the
// read-side fail-open rationale and cost note.
func CheckpointReadRemotesWithElection(ctx context.Context) CheckpointReadResolution {
	var res CheckpointReadResolution
	elected, err := ResolveCheckpointSyncRemote(ctx)
	switch {
	case err != nil:
		res.ElectionErr = err
		logging.Debug(ctx, "checkpoint reads: election failed, falling back to origin only",
			slog.String("error", err.Error()))
	case elected.Name != "":
		res.ElectedName = elected.Name
		res.Candidates = append(res.Candidates, elected.Name)
	}
	if isConfiguredRemote(ctx, "origin") && (len(res.Candidates) == 0 || res.Candidates[0] != "origin") {
		res.Candidates = append(res.Candidates, "origin")
	}
	return res
}
