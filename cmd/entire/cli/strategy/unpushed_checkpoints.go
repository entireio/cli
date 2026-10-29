package strategy

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
)

// CountUnpushedCheckpoints approximates how many checkpoints exist locally
// but not on the checkpoint sync remote. Fully local — no network.
// git-refs primary: the queued checkpoint refs that still exist locally, which
// is what the next push sends and what ListOutgoingCheckpoints lists.
// git-branch primary: v1 commits ahead of refs/remotes/<remote>/<v1>; an
// absent tracking ref with a local v1 branch counts every v1 commit (correct
// for the deferred-publish case; a stale tracking ref overcounts — acceptable
// for a local, no-network heuristic).
func CountUnpushedCheckpoints(ctx context.Context, remoteName string) (int, error) {
	if cpCfg, _ := settings.LoadCheckpointsConfig(ctx); checkpoint.PrimaryIsRefs(cpCfg) { //nolint:errcheck // fail-soft like prePush: a bad checkpoints block defaults to the git-branch backend
		outgoing, err := ListOutgoingCheckpoints(ctx)
		if err != nil {
			return 0, err
		}
		return len(outgoing.Checkpoints), nil
	}
	return countUnpushedV1Commits(ctx, remoteName)
}

// ErrOutgoingNeedsGitRefs reports an outgoing listing asked of the git-branch
// backend. Its push sends entire/checkpoints/v1 as one branch, so there is no
// per-checkpoint set to report, and none a later step could hold back.
var ErrOutgoingNeedsGitRefs = errors.New("outgoing checkpoints are listed only for the git-refs checkpoint backend")

// OutgoingCheckpoint is one queued checkpoint ref the next push sends.
type OutgoingCheckpoint struct {
	// CheckpointID is empty for a queued ref that exists locally but does not
	// name a checkpoint. The push sends it anyway.
	CheckpointID id.CheckpointID
	Ref          plumbing.ReferenceName
}

// OutgoingCheckpoints is what the next push to the checkpoint sync remote
// sends on the git-refs backend.
type OutgoingCheckpoints struct {
	Checkpoints []OutgoingCheckpoint
	// StaleQueueEntries counts queued refs missing locally. The next push
	// drops them from the queue instead of sending them.
	StaleQueueEntries int
}

// ListOutgoingCheckpoints lists the push queue the way pre-push consumes it:
// refs missing locally are pruned rather than sent, and every other queued
// ref is sent whether or not its name parses. It reads local state only, and
// returns ErrOutgoingNeedsGitRefs on the git-branch backend.
func ListOutgoingCheckpoints(ctx context.Context) (OutgoingCheckpoints, error) {
	if !primaryIsGitRefs(ctx) {
		return OutgoingCheckpoints{}, ErrOutgoingNeedsGitRefs
	}
	repo, err := OpenRepository(ctx)
	if err != nil {
		return OutgoingCheckpoints{}, fmt.Errorf("open repository: %w", err)
	}
	defer repo.Close()

	queued, err := peekCheckpointQueue(ctx, repo)
	if err != nil {
		return OutgoingCheckpoints{}, err
	}
	existing, stale := partitionLocalRefs(repo, queued)
	out := OutgoingCheckpoints{
		Checkpoints:       make([]OutgoingCheckpoint, 0, len(existing)),
		StaleQueueEntries: len(stale),
	}
	for _, ref := range existing {
		cid, _ := checkpoint.ParseRef(ref)
		out.Checkpoints = append(out.Checkpoints, OutgoingCheckpoint{CheckpointID: cid, Ref: ref})
	}
	return out, nil
}

// peekCheckpointQueue reads the push-discovery queue without compacting or
// pruning it; the push owns both.
func peekCheckpointQueue(ctx context.Context, repo *git.Repository) ([]plumbing.ReferenceName, error) {
	queue, err := checkpoint.PushQueueForRepo(ctx, repo)
	if err != nil {
		return nil, fmt.Errorf("resolve push queue: %w", err)
	}
	refs, err := queue.Peek()
	if err != nil {
		return nil, fmt.Errorf("read push queue: %w", err)
	}
	return refs, nil
}

// countUnpushedV1Commits counts v1-branch commits not on the remote-tracking
// ref (git-branch backend). No local v1 branch means nothing to push (0).
func countUnpushedV1Commits(ctx context.Context, remoteName string) (int, error) {
	local := checkpoint.ResolveRefs(ctx).Primary
	if !gitCommitRefExists(ctx, local.String()) {
		return 0, nil
	}
	rangeSpec := local.String()
	if remoteName != "" {
		tracking := "refs/remotes/" + remoteName + "/" + local.Short()
		if gitCommitRefExists(ctx, tracking) {
			rangeSpec = tracking + ".." + local.String()
		}
	}
	out, err := exec.CommandContext(ctx, "git", "rev-list", "--count", rangeSpec).Output()
	if err != nil {
		return 0, fmt.Errorf("count unpushed v1 commits: %w", err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return 0, fmt.Errorf("parse rev-list count: %w", err)
	}
	return n, nil
}

// gitCommitRefExists reports whether ref resolves to a commit in the current
// repo. Local and best-effort: any error reads as "absent".
func gitCommitRefExists(ctx context.Context, ref string) bool {
	return exec.CommandContext(ctx, "git", "rev-parse", "--verify", "--quiet", ref+"^{commit}").Run() == nil
}
