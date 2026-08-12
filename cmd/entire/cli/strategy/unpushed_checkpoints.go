package strategy

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/settings"
)

// CountUnpushedCheckpoints approximates how many checkpoints exist locally
// but not on the checkpoint sync remote. Fully local — no network.
// git-refs primary: push-queue length. git-branch primary: v1 commits ahead
// of refs/remotes/<remote>/<v1>; an absent tracking ref with a local v1
// branch counts every v1 commit (correct for the deferred-publish case; a
// stale tracking ref overcounts — acceptable for a local, no-network
// heuristic).
func CountUnpushedCheckpoints(ctx context.Context, remoteName string) (int, error) {
	if cpCfg, _ := settings.LoadCheckpointsConfig(ctx); checkpoint.PrimaryIsRefs(cpCfg) { //nolint:errcheck // fail-soft like prePush: a bad checkpoints block defaults to the git-branch backend
		return countQueuedCheckpointRefs(ctx)
	}
	return countUnpushedV1Commits(ctx, remoteName)
}

// countQueuedCheckpointRefs returns the push-discovery queue length (git-refs
// backend): every queued ref is a checkpoint written locally but not yet
// confirmed pushed.
func countQueuedCheckpointRefs(ctx context.Context) (int, error) {
	repo, err := OpenRepository(ctx)
	if err != nil {
		return 0, fmt.Errorf("open repository: %w", err)
	}
	defer repo.Close()

	queue, err := checkpoint.PushQueueForRepo(ctx, repo)
	if err != nil {
		return 0, fmt.Errorf("resolve push queue: %w", err)
	}
	refs, err := queue.Peek()
	if err != nil {
		return 0, fmt.Errorf("read push queue: %w", err)
	}
	return len(refs), nil
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
