package strategy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildCherryPickCommit_ReturnsUnsignedCommit(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "f.txt", "init")
	testutil.GitAdd(t, dir, "f.txt")
	testutil.GitCommit(t, dir, "init")

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	head, err := repo.Head()
	require.NoError(t, err)
	headCommit, err := repo.CommitObject(head.Hash())
	require.NoError(t, err)

	built, err := buildCherryPickCommit(context.Background(), repo, headCommit.TreeHash, head.Hash(), headCommit)
	require.NoError(t, err)
	assert.Equal(t, headCommit.Message, built.Message)
	assert.Equal(t, headCommit.TreeHash, built.TreeHash)
	assert.Empty(t, built.Signature, "build must not sign")
	assert.Equal(t, []plumbing.Hash{head.Hash()}, built.ParentHashes)
}

func TestPersistCherryPickCommit_StoresAndReturnsHash(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "f.txt", "init")
	testutil.GitAdd(t, dir, "f.txt")
	testutil.GitCommit(t, dir, "init")

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	head, err := repo.Head()
	require.NoError(t, err)
	headCommit, err := repo.CommitObject(head.Hash())
	require.NoError(t, err)

	built, err := buildCherryPickCommit(context.Background(), repo, headCommit.TreeHash, head.Hash(), headCommit)
	require.NoError(t, err)

	hash, err := persistCherryPickCommit(repo, built)
	require.NoError(t, err)
	assert.NotEqual(t, plumbing.ZeroHash, hash)

	stored, err := repo.CommitObject(hash)
	require.NoError(t, err)
	assert.Equal(t, headCommit.Message, stored.Message)
	assert.Equal(t, headCommit.TreeHash, stored.TreeHash)
}

// Tests for signingProgressMessage formatting.
func TestSigningProgressMessage_FormatsSubjectAndCount(t *testing.T) {
	t.Parallel()

	got := signingProgressMessage(3, 12, "entire-checkpoint: step 4 post-tool")
	assert.Equal(t, "Signing commit 3/12: entire-checkpoint: step 4 post-tool", got)
}

func TestSigningProgressMessage_TruncatesLongSubject(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("a", 200)
	got := signingProgressMessage(1, 1, long)
	// Subject is truncated to 80 visible chars (79 'a's + "…"). "…" is 3 bytes.
	// Prefix is "Signing commit 1/1: " (20 chars). So byte length = 20 + 79 + 3 = 102.
	assert.True(t, strings.HasSuffix(got, "…"))
	assert.True(t, strings.HasPrefix(got, "Signing commit 1/1: "))
	// Subject portion (after prefix) should be 79 'a's + "…"
	subjectPortion := strings.TrimPrefix(got, "Signing commit 1/1: ")
	assert.Equal(t, 79, strings.Count(subjectPortion, "a"))
}

func TestSigningProgressMessage_StripsBodyOfMessage(t *testing.T) {
	t.Parallel()

	got := signingProgressMessage(1, 1, "subject line\n\nlong body line 1\nlong body line 2\n")
	assert.Equal(t, "Signing commit 1/1: subject line", got)
}

const testFakeSig = "fake-sig"

// Tests for signAndPersistCommits behavior.
// These tests override package-level function vars and cannot be parallelized.
func TestSignAndPersistCommits_AllSucceed(t *testing.T) {
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	repo, base := setupChainOfUnsignedCommits(t, dir, 3)

	prev := signCommitForPush
	signCommitForPush = func(_ context.Context, c *object.Commit) error {
		c.Signature = testFakeSig
		return nil
	}
	t.Cleanup(func() { signCommitForPush = prev })

	var stderr bytes.Buffer
	commits := listLocalCommits(t, repo, base)
	tip, err := signAndPersistCommits(context.Background(), repo, base, commits, &stderr)
	require.NoError(t, err)

	assert.NotEqual(t, plumbing.ZeroHash, tip)
	out := stderr.String()
	assert.Contains(t, out, "Signing commit 1/3:")
	assert.Contains(t, out, "Signing commit 2/3:")
	assert.Contains(t, out, "Signing commit 3/3:")

	walkAndAssertAllSigned(t, repo, tip, base)
}

func TestSignAndPersistCommits_SkipKeepsChainConnectedWithUnsignedMiddle(t *testing.T) {
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	repo, base := setupChainOfUnsignedCommits(t, dir, 3)

	prevSign := signCommitForPush
	calls := 0
	signCommitForPush = func(_ context.Context, c *object.Commit) error {
		calls++
		if calls == 2 {
			return errors.New("agent busy")
		}
		c.Signature = testFakeSig
		return nil
	}
	t.Cleanup(func() { signCommitForPush = prevSign })

	prevPrompt := promptOnSigningFailure
	promptOnSigningFailure = func(_ context.Context, _ string, _ error, _ io.Writer) signingAction {
		return signingActionSkip
	}
	t.Cleanup(func() { promptOnSigningFailure = prevPrompt })

	var stderr bytes.Buffer
	commits := listLocalCommits(t, repo, base)
	tip, err := signAndPersistCommits(context.Background(), repo, base, commits, &stderr)
	require.NoError(t, err)

	signedCount, unsignedCount := countSignedAndUnsigned(t, repo, tip, base)
	assert.Equal(t, 2, signedCount)
	assert.Equal(t, 1, unsignedCount)
}

func TestSignAndPersistCommits_AbortReturnsErrorWithoutAdvancing(t *testing.T) {
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	repo, base := setupChainOfUnsignedCommits(t, dir, 3)

	prevSign := signCommitForPush
	signCommitForPush = func(_ context.Context, _ *object.Commit) error {
		return errors.New("agent down")
	}
	t.Cleanup(func() { signCommitForPush = prevSign })

	prevPrompt := promptOnSigningFailure
	promptOnSigningFailure = func(_ context.Context, _ string, _ error, _ io.Writer) signingAction {
		return signingActionAbort
	}
	t.Cleanup(func() { promptOnSigningFailure = prevPrompt })

	var stderr bytes.Buffer
	commits := listLocalCommits(t, repo, base)
	_, err := signAndPersistCommits(context.Background(), repo, base, commits, &stderr)
	require.Error(t, err)
	assert.ErrorIs(t, err, errSigningAborted)
}

func TestSignAndPersistCommits_RetrySucceedsOnSecondAttempt(t *testing.T) {
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	repo, base := setupChainOfUnsignedCommits(t, dir, 1)

	prevSign := signCommitForPush
	attempt := 0
	signCommitForPush = func(_ context.Context, c *object.Commit) error {
		attempt++
		if attempt == 1 {
			return errors.New("transient")
		}
		c.Signature = testFakeSig
		return nil
	}
	t.Cleanup(func() { signCommitForPush = prevSign })

	prevPrompt := promptOnSigningFailure
	promptOnSigningFailure = func(_ context.Context, _ string, _ error, _ io.Writer) signingAction {
		return signingActionRetry
	}
	t.Cleanup(func() { promptOnSigningFailure = prevPrompt })

	var stderr bytes.Buffer
	commits := listLocalCommits(t, repo, base)
	tip, err := signAndPersistCommits(context.Background(), repo, base, commits, &stderr)
	require.NoError(t, err)
	assert.Equal(t, 2, attempt)
	walkAndAssertAllSigned(t, repo, tip, base)
}

// setupChainOfUnsignedCommits commits a base file to give the repo a valid
// HEAD, then creates n unsigned checkpoint commits as an orphan-rooted chain
// (matching production: entire/checkpoints/v1 is initialized as an orphan via
// strategy/common.go orphan-init). Points the local entire/checkpoints/v1 ref
// at the chain tip. Returns the repo and ZeroHash as the base, because the
// checkpoint chain does NOT descend from HEAD.
func setupChainOfUnsignedCommits(t *testing.T, dir string, n int) (*git.Repository, plumbing.Hash) {
	t.Helper()

	// Production rationale: entire/checkpoints/v1 is initialized as an orphan
	// (see strategy/common.go orphan-init and checkpoint/temporary.go
	// getOrCreateShadowBranch). The test must mirror that so first-push code
	// paths exercise the real chain shape.
	//
	// We still need an initial user commit so the repo has a valid HEAD for
	// OpenRepository, but the checkpoint chain does NOT descend from it.
	testutil.WriteFile(t, dir, "base.txt", "base")
	testutil.GitAdd(t, dir, "base.txt")
	testutil.GitCommit(t, dir, "base")

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	parent := plumbing.ZeroHash
	for i := range n {
		treeHash := makeUniqueTree(t, repo, i)
		hash, err := checkpoint.CreateCommit(context.Background(), repo, treeHash, parent, fmt.Sprintf("entire-checkpoint: step %d", i+1), "u", "u@e")
		require.NoError(t, err)
		parent = hash
	}

	tip := parent
	refs := checkpoint.DefaultV1Refs()
	require.NoError(t, AdvanceLocalRef(context.Background(), repo, refs, refs.Primary, tip))

	// base is ZeroHash because the chain is orphan-rooted — there is no
	// anchor commit to exclude when walking.
	return repo, plumbing.ZeroHash
}

func listLocalCommits(t *testing.T, repo *git.Repository, base plumbing.Hash) []*object.Commit {
	t.Helper()
	refs := checkpoint.DefaultV1Refs()
	tipRef, err := repo.Reference(refs.Primary, true)
	require.NoError(t, err)

	var out []*object.Commit
	for h := tipRef.Hash(); h != base; {
		c, err := repo.CommitObject(h)
		require.NoError(t, err)
		out = append([]*object.Commit{c}, out...)
		if len(c.ParentHashes) == 0 {
			break
		}
		h = c.ParentHashes[0]
	}
	return out
}

func walkAndAssertAllSigned(t *testing.T, repo *git.Repository, tip, base plumbing.Hash) {
	t.Helper()
	for h := tip; h != base; {
		c, err := repo.CommitObject(h)
		require.NoError(t, err)
		assert.NotEmpty(t, c.Signature, "expected commit %s signed", h)
		if len(c.ParentHashes) == 0 {
			break
		}
		h = c.ParentHashes[0]
	}
}

func countSignedAndUnsigned(t *testing.T, repo *git.Repository, tip, base plumbing.Hash) (signed, unsigned int) {
	t.Helper()
	for h := tip; h != base; {
		c, err := repo.CommitObject(h)
		require.NoError(t, err)
		if c.Signature != "" {
			signed++
		} else {
			unsigned++
		}
		if len(c.ParentHashes) == 0 {
			break
		}
		h = c.ParentHashes[0]
	}
	return signed, unsigned
}

func makeUniqueTree(t *testing.T, repo *git.Repository, salt int) plumbing.Hash {
	t.Helper()
	content := []byte(fmt.Sprintf("contents-%d", salt))
	blob, err := checkpoint.CreateBlobFromContent(repo, content)
	require.NoError(t, err)
	tree := &object.Tree{Entries: []object.TreeEntry{
		{Name: fmt.Sprintf("f-%d.txt", salt), Mode: filemode.Regular, Hash: blob},
	}}
	obj := repo.Storer.NewEncodedObject()
	require.NoError(t, tree.Encode(obj))
	hash, err := repo.Storer.SetEncodedObject(obj)
	require.NoError(t, err)
	return hash
}

func TestPrePush_SignsCommitsAboveRemoteTipAndAdvancesLocal(t *testing.T) { //nolint:paralleltest // t.Chdir + signCommitForPush global
	dir := t.TempDir()
	bareRemote := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.InitBareRepo(t, bareRemote)
	testutil.AddRemote(t, dir, "origin", bareRemote)
	_, base := setupChainOfUnsignedCommits(t, dir, 4)

	prev := signCommitForPush
	signCommitForPush = func(_ context.Context, c *object.Commit) error {
		c.Signature = testFakeSig
		return nil
	}
	t.Cleanup(func() { signCommitForPush = prev })

	t.Chdir(dir)
	s := &ManualCommitStrategy{}
	require.NoError(t, s.PrePush(context.Background(), "origin"))

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)
	refs := checkpoint.DefaultV1Refs()
	tip, err := repo.Reference(refs.Primary, true)
	require.NoError(t, err)
	// All checkpoint commits in the orphan-rooted chain must be signed.
	walkAndAssertAllSigned(t, repo, tip.Hash(), base)

	bare, err := git.PlainOpen(bareRemote)
	require.NoError(t, err)
	remoteTip, err := bare.Reference(refs.Primary, true)
	require.NoError(t, err)
	assert.Equal(t, tip.Hash(), remoteTip.Hash(), "remote should mirror local signed tip")

	// Walk to root and confirm the chain is orphan-rooted (matches production).
	rootHash := tip.Hash()
	for {
		c, err := repo.CommitObject(rootHash)
		require.NoError(t, err)
		if len(c.ParentHashes) == 0 {
			break
		}
		rootHash = c.ParentHashes[0]
	}
	root, err := repo.CommitObject(rootHash)
	require.NoError(t, err)
	assert.Empty(t, root.ParentHashes, "signed chain root must be orphan-rooted")
}

func TestPrePush_IdempotentReRunDoesNothing(t *testing.T) { //nolint:paralleltest // t.Chdir + signCommitForPush global
	dir := t.TempDir()
	bareRemote := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.InitBareRepo(t, bareRemote)
	testutil.AddRemote(t, dir, "origin", bareRemote)
	_, _ = setupChainOfUnsignedCommits(t, dir, 2)

	signCalls := 0
	prev := signCommitForPush
	signCommitForPush = func(_ context.Context, c *object.Commit) error {
		signCalls++
		c.Signature = testFakeSig
		return nil
	}
	t.Cleanup(func() { signCommitForPush = prev })

	t.Chdir(dir)
	s := &ManualCommitStrategy{}
	require.NoError(t, s.PrePush(context.Background(), "origin"))
	require.NoError(t, s.PrePush(context.Background(), "origin"))

	assert.Equal(t, 2, signCalls, "second push should sign nothing")
}

func TestPrePush_SigningDisabled_PushesUnsigned(t *testing.T) { //nolint:paralleltest // t.Chdir + setupSigningEnv global
	dir := t.TempDir()
	bareRemote := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.InitBareRepo(t, bareRemote)
	testutil.AddRemote(t, dir, "origin", bareRemote)
	_, base := setupChainOfUnsignedCommits(t, dir, 2)

	// Write a settings file that disables signing, then chdir.
	writeDisabledSigningSettings(t, dir)
	t.Chdir(dir)
	s := &ManualCommitStrategy{}
	require.NoError(t, s.PrePush(context.Background(), "origin"))

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)
	refs := checkpoint.DefaultV1Refs()
	tip, err := repo.Reference(refs.Primary, true)
	require.NoError(t, err)

	// All checkpoint commits in the orphan-rooted chain should be unsigned.
	signed, unsigned := countSignedAndUnsigned(t, repo, tip.Hash(), base)
	assert.Equal(t, 0, signed)
	assert.Equal(t, 2, unsigned)
}

func TestPrePush_SigningFailureSkipNonTTY_KeepsChainConnected(t *testing.T) { //nolint:paralleltest // t.Chdir + signCommitForPush global
	dir := t.TempDir()
	bareRemote := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.InitBareRepo(t, bareRemote)
	testutil.AddRemote(t, dir, "origin", bareRemote)
	_, base := setupChainOfUnsignedCommits(t, dir, 3)

	prev := signCommitForPush
	calls := 0
	signCommitForPush = func(_ context.Context, c *object.Commit) error {
		calls++
		if calls == 2 {
			return errors.New("agent busy")
		}
		c.Signature = testFakeSig
		return nil
	}
	t.Cleanup(func() { signCommitForPush = prev })

	// Default prompt under `go test` is non-interactive, so it skips silently.

	t.Chdir(dir)
	s := &ManualCommitStrategy{}
	require.NoError(t, s.PrePush(context.Background(), "origin"))

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)
	refs := checkpoint.DefaultV1Refs()
	tip, err := repo.Reference(refs.Primary, true)
	require.NoError(t, err)
	// Count across all checkpoint commits in the orphan-rooted chain.
	signed, unsigned := countSignedAndUnsigned(t, repo, tip.Hash(), base)
	assert.Equal(t, 2, signed)
	assert.Equal(t, 1, unsigned)
}

func TestPrePush_SigningAbort_LeavesLocalUnchanged(t *testing.T) { //nolint:paralleltest // t.Chdir + signCommitForPush global
	dir := t.TempDir()
	bareRemote := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.InitBareRepo(t, bareRemote)
	testutil.AddRemote(t, dir, "origin", bareRemote)
	_, _ = setupChainOfUnsignedCommits(t, dir, 2)

	prevSign := signCommitForPush
	signCommitForPush = func(_ context.Context, _ *object.Commit) error {
		return errors.New("agent down")
	}
	t.Cleanup(func() { signCommitForPush = prevSign })

	prevPrompt := promptOnSigningFailure
	promptOnSigningFailure = func(_ context.Context, _ string, _ error, _ io.Writer) signingAction {
		return signingActionAbort
	}
	t.Cleanup(func() { promptOnSigningFailure = prevPrompt })

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)
	refs := checkpoint.DefaultV1Refs()
	tipBefore, err := repo.Reference(refs.Primary, true)
	require.NoError(t, err)

	t.Chdir(dir)
	s := &ManualCommitStrategy{}
	require.Error(t, s.PrePush(context.Background(), "origin"))

	tipAfter, err := repo.Reference(refs.Primary, true)
	require.NoError(t, err)
	assert.Equal(t, tipBefore.Hash(), tipAfter.Hash(), "local ref must not advance on abort")
}

// writeDisabledSigningSettings writes a settings file disabling signing into dir/.entire/settings.json.
func writeDisabledSigningSettings(t *testing.T, dir string) {
	t.Helper()
	entireDir := filepath.Join(dir, ".entire")
	require.NoError(t, os.MkdirAll(entireDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(entireDir, "settings.json"), []byte(`{"sign_checkpoint_commits": false}`), 0o644))
}
