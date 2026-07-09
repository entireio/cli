package testutil

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"sort"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
)

// Checkpoint storage backends the e2e suite can run against. Selected per-run by
// E2E_CHECKPOINT_STORE (which TestMain maps to the ENTIRE_CHECKPOINTS_PRIMARY
// override the spawned binary honors). The testutil helpers below resolve the
// committed checkpoint record differently per backend so the same tests assert
// against either store.
const (
	storeModeGitBranch  = "git-branch"
	storeModeGitRefs    = "git-refs"
	checkpointRefPrefix = "refs/entire/checkpoints/"
)

// checkpointStoreMode reports the backend the suite is running against, defaulting
// to the git-branch store.
func checkpointStoreMode() string {
	if envIsGitRefs("E2E_CHECKPOINT_STORE") || envIsGitRefs("ENTIRE_CHECKPOINTS_PRIMARY") {
		return storeModeGitRefs
	}
	return storeModeGitBranch
}

func envIsGitRefs(key string) bool {
	return os.Getenv(key) == storeModeGitRefs
}

// UsingGitRefs reports whether the suite is running against the per-checkpoint
// git-refs store. Tests that are inherently specific to the v1 branch topology
// guard on this to skip under git-refs.
func UsingGitRefs() bool { return checkpointStoreMode() == storeModeGitRefs }

// checkpointRefName returns refs/entire/checkpoints/<shard>/<id> for the git-refs
// store, using the production shard computation (id.ShardFor).
func checkpointRefName(checkpointID string) string {
	return checkpointRefPrefix + id.CheckpointID(checkpointID).ShardFor() + "/" + checkpointID
}

// checkpointBlobSpec returns the `git show` spec (ref:path) for a path relative
// to a checkpoint's directory root, resolved for the active backend:
//
//	git-branch: entire/checkpoints/v1:<id[:2]>/<id[2:]>/<rel>
//	git-refs:   refs/entire/checkpoints/<shard>/<id>:<rel>
func checkpointBlobSpec(id, rel string) string {
	if UsingGitRefs() {
		return checkpointRefName(id) + ":" + rel
	}
	return checkpointReadRef() + ":" + CheckpointPath(id) + "/" + rel
}

// CheckpointState returns a backend-aware digest of the committed checkpoint
// state that changes whenever any checkpoint is created or advanced. It replaces
// a bare `rev-parse entire/checkpoints/v1` so advance detection works for both
// the single-branch and per-checkpoint-ref topologies. It never fails: an absent
// store yields a stable empty-state value.
func CheckpointState(dir string) string {
	if UsingGitRefs() {
		out := gitOutputSafe(dir, "for-each-ref", "--format=%(refname) %(objectname)", checkpointRefPrefix)
		lines := strings.Split(strings.TrimSpace(out), "\n")
		sort.Strings(lines)
		sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
		return hex.EncodeToString(sum[:])
	}
	return strings.TrimSpace(gitOutputSafe(dir, "rev-parse", checkpointReadRef()))
}

// CheckpointsPresent reports whether any committed checkpoint exists locally —
// the git-branch v1 ref, or at least one per-checkpoint ref under git-refs.
func CheckpointsPresent(dir string) bool {
	if UsingGitRefs() {
		return strings.TrimSpace(gitOutputSafe(dir, "for-each-ref", "--format=%(refname)", checkpointRefPrefix)) != ""
	}
	return strings.TrimSpace(gitOutputSafe(dir, "rev-parse", "--verify", "refs/heads/"+checkpointRefV1)) != ""
}
