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
// shard case-insensitively matches the ID's own ShardFor — so refs the
// resolver did not write (mismatched shard, extra path segments) are rejected
// rather than silently resolved to the wrong bucket. It does not require the
// ID to be a recognized kind, so a future ID format still parses as long as it
// shards consistently.
//
// The shard match is case-insensitive (strings.EqualFold) rather than exact,
// because the on-disk directory name is not always byte-identical to a fresh
// ShardFor() computation: on a case-insensitive-but-case-preserving filesystem
// (macOS APFS, Windows NTFS defaults), git resolves a new shard directory
// against existing ones case-insensitively, so a ULID's uppercase shard (e.g.
// "6B") can land inside an already-present differently-cased directory (e.g.
// a legacy hex checkpoint's lowercase "6b") instead of a distinct one. An
// exact comparison then rejects that ref as malformed even though the
// checkpoint object it names is intact — see the ULID/legacy shard-collision
// bug this guards against.
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
	if !strings.EqualFold(cid.ShardFor(), shard) {
		return id.EmptyCheckpointID, false
	}
	return cid, true
}

// FoldedRefName returns the one alternate-cased spelling of cid's checkpoint
// ref, reporting whether such a spelling exists.
//
// A shard bucket on disk has exactly two possible spellings, because each ID
// format uses one case exclusively: a legacy ID is lowercase hex, a ULID is
// uppercase Crockford base32. On a case-insensitive-but-case-preserving
// filesystem (macOS APFS, Windows NTFS defaults) the bucket is named after
// whichever format created it first and every later ref folds into it, so a
// checkpoint's ref can live at its own ShardFor spelling OR at the other
// format's. There is no third possibility, which is what makes a single
// fallback lookup exhaustive rather than a heuristic — see resolveLocalRef.
//
// A shard of two digits has no alternate spelling (digits have no case) and
// reports false: the two formats name that bucket identically, so nothing can
// diverge.
func FoldedRefName(cid id.CheckpointID) (plumbing.ReferenceName, bool) {
	shard, ok := foldedShard(cid.ShardFor())
	if !ok {
		return "", false
	}
	return plumbing.ReferenceName(CheckpointRefPrefix + shard + "/" + cid.String()), true
}

// foldedShard returns shard in the opposite case, reporting whether that
// differs from shard itself.
func foldedShard(shard string) (string, bool) {
	if lower := strings.ToLower(shard); lower != shard {
		return lower, true
	}
	if upper := strings.ToUpper(shard); upper != shard {
		return upper, true
	}
	return "", false
}

// isCanonicalRefName reports whether name is cid's canonical RefName spelling,
// as opposed to the case-folded one FoldedRefName describes. An ID whose kind
// RefName rejects has no canonical spelling and reports false.
func isCanonicalRefName(cid id.CheckpointID, name plumbing.ReferenceName) bool {
	canonical, err := RefName(cid)
	return err == nil && canonical == name
}
