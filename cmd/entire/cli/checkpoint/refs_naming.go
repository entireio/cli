package checkpoint

import (
	"fmt"
	"strings"

	"github.com/go-git/go-git/v6/plumbing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
)

// CheckpointRefPrefix is the namespace under which the git-refs backend stores
// one ref per checkpoint: refs/entire/checkpoints/<shard>/<id>. Each ref points
// at a checkpoint commit whose tree root is that checkpoint's contents. This is
// distinct from the git-branch backend's single entire/checkpoints/v1 branch.
const CheckpointRefPrefix = "refs/entire/checkpoints/"

// RefName returns the per-checkpoint git ref for a checkpoint ID:
// refs/entire/checkpoints/<shard>/<id>, where <shard> is id.ShardFor() (the
// first two chars for legacy hex IDs, the last two for ULIDs). The full ID is
// always the leaf, so the ref round-trips through ParseRef.
//
// It errors on an empty or unrecognized checkpoint ID rather than returning a
// malformed ref (e.g. "refs/entire/checkpoints//"), so callers at trust
// boundaries — and future ones — can't silently push, fetch, or look up a bad
// ref.
func RefName(cid id.CheckpointID) (plumbing.ReferenceName, error) {
	if cid.Kind() == id.KindUnknown {
		return "", fmt.Errorf("cannot build checkpoint ref: invalid checkpoint ID %q", cid)
	}
	return plumbing.ReferenceName(CheckpointRefPrefix + cid.ShardFor() + "/" + cid.String()), nil
}

// ParseRef extracts the checkpoint ID from a per-checkpoint ref name,
// reporting whether name is a well-formed checkpoint ref. A ref is well-formed
// when it has the CheckpointRefPrefix, exactly a <shard>/<id> tail, and the
// shard matches the ID's own ShardFor — so refs the resolver did not write
// (mismatched shard, extra path segments) are rejected rather than silently
// resolved to the wrong bucket. It does not require the ID to be a recognized
// kind, so a future ID format still parses as long as it shards consistently.
func ParseRef(name plumbing.ReferenceName) (id.CheckpointID, bool) {
	s := name.String()
	tail, ok := strings.CutPrefix(s, CheckpointRefPrefix)
	if !ok {
		return id.EmptyCheckpointID, false
	}
	shard, rest, ok := strings.Cut(tail, "/")
	if !ok || shard == "" || rest == "" {
		return id.EmptyCheckpointID, false
	}
	// Reject extra path segments: the tail must be exactly <shard>/<id>.
	if strings.Contains(rest, "/") {
		return id.EmptyCheckpointID, false
	}
	cid := id.CheckpointID(rest)
	if cid.ShardFor() != shard {
		return id.EmptyCheckpointID, false
	}
	return cid, true
}
