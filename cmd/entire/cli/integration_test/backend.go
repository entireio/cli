//go:build integration

package integration

import (
	"os/exec"
	"sort"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// Checkpoint storage backends the integration suite can run against. Selected
// per-subtest by ForEachBackend, which sets TestEnv.CheckpointStore so every
// spawned CLI/hook inherits ENTIRE_CHECKPOINTS_PRIMARY. The backend-aware
// assertion helpers below mirror e2e/testutil/backend.go so the same test asserts
// against either the single v1 branch (git-branch) or the per-checkpoint refs
// (git-refs).
const (
	StoreGitBranch = "git-branch"
	StoreGitRefs   = "git-refs"

	// checkpointRefPrefix is the git-refs namespace for per-checkpoint refs.
	checkpointRefPrefix = "refs/entire/checkpoints/"
)

// ForEachBackend runs fn as a subtest for each checkpoint backend ("git-branch"
// and "git-refs"). Each subtest runs in parallel and receives the backend name;
// the closure constructs its TestEnv and assigns env.CheckpointStore = backend
// before any checkpoint-creating operation. Prefer the backend-aware assertion
// helpers (CheckpointsPresentLocally, CheckpointsPresentOnRemote, …) inside fn so
// the assertions hold for both topologies.
func ForEachBackend(t *testing.T, fn func(t *testing.T, backend string)) {
	t.Helper()
	for _, backend := range []string{StoreGitBranch, StoreGitRefs} {
		t.Run(backend, func(t *testing.T) {
			t.Parallel()
			fn(t, backend)
		})
	}
}

// usingGitRefs reports whether the env's selected backend is the per-checkpoint
// git-refs store. An empty CheckpointStore is the CLI default (git-branch).
func (env *TestEnv) usingGitRefs() bool {
	return env.CheckpointStore == StoreGitRefs
}

// LatestCheckpointID returns the most recent checkpoint ID in a backend-aware
// way: from the v1 branch commit message (git-branch) or from the code commit's
// Entire-Checkpoint trailer (git-refs, where there is no v1 commit to parse).
// The trailer is written for both backends, so the git-refs path also works for
// git-branch — the split keeps each backend on its established reader.
func (env *TestEnv) LatestCheckpointID() string {
	env.T.Helper()
	if env.usingGitRefs() {
		return env.GetLatestCheckpointIDFromHistory()
	}
	return env.GetLatestCheckpointID()
}

// checkpointRefName returns refs/entire/checkpoints/<shard>/<id> for a checkpoint.
func checkpointRefName(checkpointID string) string {
	return checkpointRefPrefix + id.CheckpointID(checkpointID).ShardFor() + "/" + checkpointID
}

// CheckpointsPresentLocally reports whether any committed checkpoint exists in the
// repo: the v1 branch (git-branch) or at least one per-checkpoint ref (git-refs).
func (env *TestEnv) CheckpointsPresentLocally() bool {
	env.T.Helper()
	if env.usingGitRefs() {
		return anyRefUnderPrefix(env.T, env.RepoDir, checkpointRefPrefix)
	}
	return env.BranchExists(paths.MetadataBranchName)
}

// CheckpointsPresentOnRemote reports whether any committed checkpoint landed on
// the bare remote: the v1 branch (git-branch) or at least one per-checkpoint ref
// (git-refs).
func (env *TestEnv) CheckpointsPresentOnRemote(bareDir string) bool {
	env.T.Helper()
	if env.usingGitRefs() {
		return anyRefUnderPrefix(env.T, bareDir, checkpointRefPrefix)
	}
	return env.BranchExistsOnRemote(bareDir, paths.MetadataBranchName)
}

// CheckpointExistsOnRemote reports whether a specific checkpoint landed on the
// bare remote: its metadata blob in the v1 tree (git-branch) or its per-checkpoint
// ref (git-refs).
func (env *TestEnv) CheckpointExistsOnRemote(bareDir, checkpointID string) bool {
	env.T.Helper()
	if env.usingGitRefs() {
		return refExists(env.T, bareDir, checkpointRefName(checkpointID))
	}
	return fileExistsOnRemoteBranch(env.T, bareDir, CheckpointSummaryPath(checkpointID))
}

// RemoteCheckpointState returns a digest of the committed checkpoint state on the
// bare remote that changes whenever a checkpoint is pushed. Backend-aware: the v1
// branch tip (git-branch) or the sorted set of per-checkpoint refs and their
// objects (git-refs). Used to assert idempotent pushes leave the remote unchanged.
func (env *TestEnv) RemoteCheckpointState(bareDir string) string {
	env.T.Helper()
	prefix := "refs/heads/" + paths.MetadataBranchName
	if env.usingGitRefs() {
		prefix = checkpointRefPrefix
	}
	cmd := exec.CommandContext(env.T.Context(), "git", "for-each-ref", "--format=%(refname) %(objectname)", prefix)
	cmd.Dir = bareDir
	cmd.Env = testutil.GitIsolatedEnv()
	out, err := cmd.Output()
	if err != nil {
		// Fail rather than return "": two broken invocations comparing equal
		// would make an idempotence assertion pass vacuously.
		env.T.Fatalf("RemoteCheckpointState: git for-each-ref %s in %s failed: %v", prefix, bareDir, err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// anyRefUnderPrefix reports whether the repo at dir has any ref under prefix.
func anyRefUnderPrefix(t *testing.T, dir, prefix string) bool {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", "for-each-ref", "--format=%(refname)", prefix)
	cmd.Dir = dir
	cmd.Env = testutil.GitIsolatedEnv()
	out, err := cmd.Output()
	if err != nil {
		// Fail rather than return false: a broken invocation reported as
		// "no refs" would make a "should NOT exist" assertion pass vacuously
		// (same reasoning as RemoteCheckpointState). An absent prefix is not
		// an error — for-each-ref exits 0 with empty output.
		t.Fatalf("anyRefUnderPrefix: git for-each-ref %s in %s failed: %v", prefix, dir, err)
	}
	return strings.TrimSpace(string(out)) != ""
}

// refExists reports whether the exact ref exists in the repo at dir.
func refExists(t *testing.T, dir, ref string) bool {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", "show-ref", "--verify", "--quiet", ref)
	cmd.Dir = dir
	cmd.Env = testutil.GitIsolatedEnv()
	return cmd.Run() == nil
}
