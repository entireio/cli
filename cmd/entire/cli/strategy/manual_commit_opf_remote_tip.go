package strategy

import (
	"context"
	"log/slog"
	"slices"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/remote"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/trailers"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
)

// opfDestinationTipsTimeout bounds the read of the destination's tips. It runs
// inside the user's git push, and an unanswered read only means checking whole
// histories, so it gets the background fetch budget rather than the push's.
const opfDestinationTipsTimeout = checkpointRemoteFetchTimeout

// opfDelivery is the OPF delivery requirement for one push of checkpoint refs.
// A nil *opfDelivery ships every queued ref as it is: OPF is off, or skipped
// for this push.
//
// Otherwise a ref ships only when every commit the destination does not
// already hold carries the trailer. The tip's trailer alone says nothing about
// the history below it: recovery replays this clone's commits onto whatever
// the remote holds, so a trailered commit can sit on another clone's unscanned
// one, and pushing that ref to a destination that never received the
// unscanned commit would send it. The destination's own tip is the line: what
// it already holds is not this push's to scan or rewrite.
type opfDelivery struct {
	// remoteTips is the destination's tip per checkpoint ref, read once per
	// push. nil when it could not be read: every ref's whole history then
	// counts as unsent, which can only rescan more, never ship less scanned.
	remoteTips map[plumbing.ReferenceName]plumbing.Hash
}

// remoteTip is the destination's tip for ref, or zero when the destination
// does not have the ref or its tips are unknown.
func (d *opfDelivery) remoteTip(ref plumbing.ReferenceName) plumbing.Hash {
	if d == nil {
		return plumbing.ZeroHash
	}
	return d.remoteTips[ref]
}

// verified reports whether every commit tip would send to the destination
// carries the trailer.
func (d *opfDelivery) verified(repo *git.Repository, ref plumbing.ReferenceName, tip plumbing.Hash) bool {
	_, deepest, err := opfUnsentCommits(repo, tip, d.remoteTip(ref))
	return err == nil && deepest < 0
}

// newOPFDelivery returns the delivery requirement for decision, reading the
// destination's tips only when the requirement applies and refs are queued,
// so a push with OPF off or nothing to deliver costs no network round trip.
func newOPFDelivery(ctx context.Context, repo *git.Repository, ps pushSettings, decision OPFDecision) *opfDelivery {
	if !deliveryRequiresOPFTrailer(decision) {
		return nil
	}
	if queuedCheckpointRefCount(ctx, repo) == 0 {
		return &opfDelivery{}
	}
	return &opfDelivery{remoteTips: opfDestinationTips(ctx, ps)}
}

// opfDestinationTips reads the destination's checkpoint ref tips with one
// ls-remote, or returns nil when it cannot or it does not answer within
// opfDestinationTipsTimeout. It asks the repository a push to ps writes to,
// which for a remote with a pushurl is not the one its name reads from.
func opfDestinationTips(ctx context.Context, ps pushSettings) map[plumbing.ReferenceName]plumbing.Hash {
	target := pushedRepoTarget(ctx, resolveRefsPushDestination(ctx, ps).target)
	lsCtx, cancel := context.WithTimeout(ctx, opfDestinationTipsTimeout)
	defer cancel()
	out, err := remote.LsRemoteInDir(lsCtx, "", target, checkpoint.CheckpointRefPrefix+"*")
	if err != nil {
		logging.Warn(ctx, "opf: could not read the destination's checkpoint refs; checking whole histories",
			slog.String("error", err.Error()))
		return nil
	}
	tips := make(map[plumbing.ReferenceName]plumbing.Hash)
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || !plumbing.IsHash(fields[0]) {
			continue
		}
		tips[plumbing.ReferenceName(fields[1])] = plumbing.NewHash(fields[0])
	}
	return tips
}

// opfUnsentCommits walks first parents from tip down to the first commit the
// destination already holds — one on the first-parent history of its tip,
// remoteTip — or the root: the commits a push of tip sends. Checkpoint refs
// are a single-parent chain, so the first parent is the whole history. It
// returns them tip-first, with the index of the deepest one lacking the
// trailer, or -1 when every one carries it. A remoteTip this clone does not
// have tells nothing about which commits the destination holds, so the whole
// history counts.
func opfUnsentCommits(repo *git.Repository, tip, remoteTip plumbing.Hash) ([]*object.Commit, int, error) {
	held := remoteHistory(repo, remoteTip)
	var commits []*object.Commit
	deepest := -1
	for h := tip; !h.IsZero(); {
		if _, ok := held[h]; ok {
			break
		}
		c, err := repo.CommitObject(h)
		if err != nil {
			return nil, -1, err //nolint:wrapcheck // callers name the ref
		}
		commits = append(commits, c)
		if !trailers.HasOPFApplied(c.Message) {
			deepest = len(commits) - 1
		}
		if len(c.ParentHashes) == 0 {
			break
		}
		h = c.ParentHashes[0]
	}
	return commits, deepest, nil
}

// remoteHistory is the first-parent history of remoteTip, the commits a
// destination at that tip holds, or nil when the tip is zero or not present
// locally.
func remoteHistory(repo *git.Repository, remoteTip plumbing.Hash) map[plumbing.Hash]struct{} {
	held := make(map[plumbing.Hash]struct{})
	for h := remoteTip; !h.IsZero(); {
		c, err := repo.CommitObject(h)
		if err != nil {
			// Unknown below this point: what was walked is still held.
			break
		}
		held[h] = struct{}{}
		if len(c.ParentHashes) == 0 {
			break
		}
		h = c.ParentHashes[0]
	}
	return held
}

// opfRewriteChain returns the commits a push of tip to a destination at
// remoteTip must rewrite, ancestor-first, and the parent the deepest one keeps.
// It starts at the deepest unsent commit without the trailer and rewrites
// everything above it too, trailered or not: a commit above an unscanned one
// carries that content in its tree, whatever its message says.
func opfRewriteChain(repo *git.Repository, tip, remoteTip plumbing.Hash) ([]*object.Commit, plumbing.Hash, error) {
	commits, deepest, err := opfUnsentCommits(repo, tip, remoteTip)
	if err != nil || deepest < 0 {
		return nil, plumbing.ZeroHash, err
	}
	chain := slices.Clone(commits[:deepest+1])
	base := plumbing.ZeroHash
	if parents := chain[deepest].ParentHashes; len(parents) > 0 {
		base = parents[0]
	}
	slices.Reverse(chain)
	return chain, base, nil
}
