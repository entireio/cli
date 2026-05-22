package strategy

import (
	"context"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/testutil"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
)

const safelyAdvanceTestRef plumbing.ReferenceName = "refs/heads/safely-advance-test"

// newSafelyAdvanceTestRepo opens an empty git repository for ref-manipulation
// tests. It uses testutil.InitRepo so author/GPG config matches the rest of
// the suite, but the tests themselves operate purely via plumbing — no
// worktree state needed.
func newSafelyAdvanceTestRepo(t *testing.T) *git.Repository {
	t.Helper()
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	repo, err := git.PlainOpen(dir)
	if err != nil {
		t.Fatalf("PlainOpen: %v", err)
	}
	return repo
}

// makeEmptyTreeCommit writes a commit with an empty tree and the given parents
// and message. Different messages produce different hashes, so callers can
// create distinct commits (including divergent siblings) without touching
// files.
func makeEmptyTreeCommit(t *testing.T, repo *git.Repository, parents []plumbing.Hash, msg string) plumbing.Hash {
	t.Helper()
	emptyTree := object.Tree{}
	emptyTreeObj := repo.Storer.NewEncodedObject()
	if err := emptyTree.Encode(emptyTreeObj); err != nil {
		t.Fatalf("encode empty tree: %v", err)
	}
	treeHash, err := repo.Storer.SetEncodedObject(emptyTreeObj)
	if err != nil {
		t.Fatalf("store empty tree: %v", err)
	}
	sig := object.Signature{Name: "T", Email: "t@example.com", When: time.Unix(0, 0).UTC()}
	commit := &object.Commit{
		TreeHash:     treeHash,
		Message:      msg,
		Author:       sig,
		Committer:    sig,
		ParentHashes: parents,
	}
	obj := repo.Storer.NewEncodedObject()
	if err := commit.Encode(obj); err != nil {
		t.Fatalf("encode commit %q: %v", msg, err)
	}
	hash, err := repo.Storer.SetEncodedObject(obj)
	if err != nil {
		t.Fatalf("store commit %q: %v", msg, err)
	}
	return hash
}

// forceSetTestRef points safelyAdvanceTestRef at hash unconditionally —
// tests deliberately rewind/diverge the ref to set up the scenarios under
// test.
func forceSetTestRef(t *testing.T, repo *git.Repository, hash plumbing.Hash) {
	t.Helper()
	if err := repo.Storer.SetReference(plumbing.NewHashReference(safelyAdvanceTestRef, hash)); err != nil {
		t.Fatalf("SetReference %s: %v", safelyAdvanceTestRef, err)
	}
}

// readTestRef reads safelyAdvanceTestRef and fatals if it is missing. Used
// after a call to SafelyAdvanceLocalRef to inspect the resulting ref state.
func readTestRef(t *testing.T, repo *git.Repository) plumbing.Hash {
	t.Helper()
	ref, err := repo.Reference(safelyAdvanceTestRef, true)
	if err != nil {
		t.Fatalf("read %s: %v", safelyAdvanceTestRef, err)
	}
	return ref.Hash()
}

func TestSafelyAdvanceLocalRef_LocalMissing_SetsToTarget(t *testing.T) {
	t.Parallel()
	repo := newSafelyAdvanceTestRepo(t)
	a := makeEmptyTreeCommit(t, repo, nil, "A")

	if err := SafelyAdvanceLocalRef(context.Background(), repo, safelyAdvanceTestRef, a); err != nil {
		t.Fatalf("SafelyAdvanceLocalRef: %v", err)
	}
	if got := readTestRef(t, repo); got != a {
		t.Errorf("local ref = %s, want %s", got, a)
	}
}

func TestSafelyAdvanceLocalRef_LocalEqualsTarget_NoOp(t *testing.T) {
	t.Parallel()
	repo := newSafelyAdvanceTestRepo(t)
	a := makeEmptyTreeCommit(t, repo, nil, "A")
	forceSetTestRef(t, repo, a)

	if err := SafelyAdvanceLocalRef(context.Background(), repo, safelyAdvanceTestRef, a); err != nil {
		t.Fatalf("SafelyAdvanceLocalRef: %v", err)
	}
	if got := readTestRef(t, repo); got != a {
		t.Errorf("local ref = %s, want %s (unchanged)", got, a)
	}
}

func TestSafelyAdvanceLocalRef_LocalAhead_NoOp(t *testing.T) {
	t.Parallel()
	repo := newSafelyAdvanceTestRepo(t)
	a := makeEmptyTreeCommit(t, repo, nil, "A")
	b := makeEmptyTreeCommit(t, repo, []plumbing.Hash{a}, "B")
	forceSetTestRef(t, repo, b)

	if err := SafelyAdvanceLocalRef(context.Background(), repo, safelyAdvanceTestRef, a); err != nil {
		t.Fatalf("SafelyAdvanceLocalRef: %v", err)
	}
	if got := readTestRef(t, repo); got != b {
		t.Errorf("locally-ahead ref must not rewind: got %s, want %s", got, b)
	}
}

func TestSafelyAdvanceLocalRef_LocalBehind_FastForwards(t *testing.T) {
	t.Parallel()
	repo := newSafelyAdvanceTestRepo(t)
	a := makeEmptyTreeCommit(t, repo, nil, "A")
	b := makeEmptyTreeCommit(t, repo, []plumbing.Hash{a}, "B")
	forceSetTestRef(t, repo, a)

	if err := SafelyAdvanceLocalRef(context.Background(), repo, safelyAdvanceTestRef, b); err != nil {
		t.Fatalf("SafelyAdvanceLocalRef: %v", err)
	}
	if got := readTestRef(t, repo); got != b {
		t.Errorf("local ref should have fast-forwarded: got %s, want %s", got, b)
	}
}

// Before this fix, SafelyAdvanceLocalRef overwrote any local ref whose history
// the target couldn't reach. That destroyed unpushed local commits on orphan
// refs the CLI maintains (entire/checkpoints/v1, the V2 main ref) whenever a
// fetch landed sibling commits — e.g. checkpoint metadata produced on another
// machine. The objects survived in the loose-objects pool until git gc, but
// the branch ref no longer pointed at them.
func TestSafelyAdvanceLocalRef_Diverged_PreservesLocal(t *testing.T) {
	t.Parallel()
	repo := newSafelyAdvanceTestRepo(t)
	base := makeEmptyTreeCommit(t, repo, nil, "base")
	localTip := makeEmptyTreeCommit(t, repo, []plumbing.Hash{base}, "local-only-work")
	targetTip := makeEmptyTreeCommit(t, repo, []plumbing.Hash{base}, "remote-only-work")
	forceSetTestRef(t, repo, localTip)

	if err := SafelyAdvanceLocalRef(context.Background(), repo, safelyAdvanceTestRef, targetTip); err != nil {
		t.Fatalf("SafelyAdvanceLocalRef: %v", err)
	}
	if got := readTestRef(t, repo); got != localTip {
		t.Errorf("diverged local ref must be preserved: got %s, want %s (overwrote to target %s?)",
			got, localTip, targetTip)
	}
}

// Unrelated histories (no common ancestor at all) are a strict subset of
// "diverged" — neither ref is an ancestor of the other. Confirms the
// protection extends to that case.
func TestSafelyAdvanceLocalRef_UnrelatedHistory_PreservesLocal(t *testing.T) {
	t.Parallel()
	repo := newSafelyAdvanceTestRepo(t)
	localOnly := makeEmptyTreeCommit(t, repo, nil, "local-orphan")
	targetOnly := makeEmptyTreeCommit(t, repo, nil, "target-orphan")
	forceSetTestRef(t, repo, localOnly)

	if err := SafelyAdvanceLocalRef(context.Background(), repo, safelyAdvanceTestRef, targetOnly); err != nil {
		t.Fatalf("SafelyAdvanceLocalRef: %v", err)
	}
	if got := readTestRef(t, repo); got != localOnly {
		t.Errorf("unrelated-history local ref must be preserved: got %s, want %s", got, localOnly)
	}
}
