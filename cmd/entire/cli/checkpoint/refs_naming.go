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
// last two characters of the ID for both legacy hex and ULID formats). The full
// ID is always the leaf, so the ref round-trips through ParseRef.
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

// ParseLsRemoteRefs extracts the checkpoint refs from `git ls-remote` output,
// keyed by name with the advertised hash. Each line is "<hash>\t<refname>";
// only refs under CheckpointRefPrefix are kept, so HEAD, branches and tags drop
// out here. Checkpoint refs point at commits, so no peeled (`^{}`) lines appear
// for them; an anomalous refs/entire/checkpoints/...^{} name is left to ParseRef
// downstream (the "{}" shard never matches ShardFor). A line whose first field
// is not a well-formed object hash is skipped.
func ParseLsRemoteRefs(output []byte) map[plumbing.ReferenceName]plumbing.Hash {
	refs := make(map[plumbing.ReferenceName]plumbing.Hash)
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || !strings.HasPrefix(fields[1], CheckpointRefPrefix) {
			continue
		}
		if !plumbing.IsHash(fields[0]) {
			continue
		}
		refs[plumbing.ReferenceName(fields[1])] = plumbing.NewHash(fields[0])
	}
	return refs
}

// ParseCheckpointRefNames extracts the checkpoint ref names from `git
// ls-remote` output, in the order listed. Same filter as ParseLsRemoteRefs
// minus the hash; the store re-validates each name via ParseRef.
func ParseCheckpointRefNames(output []byte) []plumbing.ReferenceName {
	var names []plumbing.ReferenceName
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || !strings.HasPrefix(fields[1], CheckpointRefPrefix) {
			continue
		}
		names = append(names, plumbing.ReferenceName(fields[1]))
	}
	return names
}
