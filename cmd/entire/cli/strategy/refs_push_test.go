package strategy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
)

// mustRefName builds a checkpoint ref for a known-valid ID in tests.
func mustRefName(t *testing.T, cid id.CheckpointID) plumbing.ReferenceName {
	t.Helper()
	ref, err := checkpoint.RefName(cid)
	require.NoError(t, err)
	return ref
}

// setupRepoWithCheckpointRefs creates a work repo with two per-checkpoint refs
// pointing at HEAD, plus a fresh bare remote. Returns (workDir, bareDir, refs).
func setupRepoWithCheckpointRefs(t *testing.T) (string, string, []plumbing.ReferenceName) {
	t.Helper()
	return setupRepoWithNCheckpointRefs(t, 2)
}

func TestPartitionLocalRefs(t *testing.T) {
	t.Parallel()
	workDir, _, refs := setupRepoWithCheckpointRefs(t)
	repo, err := git.PlainOpen(workDir)
	require.NoError(t, err)

	stale := mustRefName(t, id.MustCheckpointID("ffffffffffff"))
	existing, missing, _ := partitionLocalRefs(t.Context(), repo, append([]plumbing.ReferenceName{stale}, refs...))

	assert.ElementsMatch(t, refs, existing, "local refs are pushable")
	assert.Equal(t, []plumbing.ReferenceName{stale}, missing, "absent ref is stale")
}

// partitionScenario is a repository and a queue to split. listed names every
// ref the namespace listing must report, by its on-disk name. tips maps refs to
// the revision localTip must resolve them to.
type partitionScenario struct {
	workDir                         string
	queued, existing, stale, listed []plumbing.ReferenceName
	tips                            map[plumbing.ReferenceName]string
	// gitFallback marks a repo whose go-git walk fails, so the listing only
	// decides existence and exposes no tips.
	gitFallback bool
}

// writeRefFile writes content as ref's loose file, bypassing git's checks.
func writeRefFile(t *testing.T, workDir string, ref plumbing.ReferenceName, content string) {
	t.Helper()
	path := filepath.Join(workDir, ".git", filepath.FromSlash(ref.String()))
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
}

// foldedShardFixture stores a checkpoint ref under the other spelling of its
// shard, the way a case-insensitive filesystem does when that directory
// already exists, and returns the stored name and the name the queue holds.
// It plants the stored name instead of relying on the filesystem, so the
// result is the same on a case-sensitive one.
func foldedShardFixture(t *testing.T, workDir, tip string) (stored, queued plumbing.ReferenceName) {
	t.Helper()
	stored = plumbing.ReferenceName(checkpoint.CheckpointRefPrefix + "AB/0000000000ab")
	queued = mustRefName(t, id.MustCheckpointID("0000000000ab"))
	require.NotEqual(t, stored, queued)
	testutil.RunGit(t, workDir, "update-ref", stored.String(), tip)
	return stored, queued
}

func TestPartitionLocalRefsUsing(t *testing.T) {
	t.Parallel()
	stale := mustRefName(t, id.MustCheckpointID("ffffffffffff"))
	scenarios := []struct {
		name  string
		build func(t *testing.T) partitionScenario
	}{
		{"keeps queue order", func(t *testing.T) partitionScenario {
			t.Helper()
			workDir, _, refs := setupRepoWithCheckpointRefs(t)
			return partitionScenario{
				workDir:  workDir,
				queued:   []plumbing.ReferenceName{refs[1], stale, refs[0]},
				existing: []plumbing.ReferenceName{refs[1], refs[0]},
				stale:    []plumbing.ReferenceName{stale},
				listed:   refs,
			}
		}},
		{"packed refs", func(t *testing.T) partitionScenario {
			t.Helper()
			workDir, _, refs := setupRepoWithCheckpointRefs(t)
			testutil.RunGit(t, workDir, "pack-refs", "--all")
			return partitionScenario{
				workDir:  workDir,
				queued:   append([]plumbing.ReferenceName{stale}, refs...),
				existing: refs,
				stale:    []plumbing.ReferenceName{stale},
				listed:   refs,
			}
		}},
		// The empty file aborts the go-git walk, and git for-each-ref skips both
		// broken files. go-git reads an empty ref file as absent and an
		// unparsable one as an error, which keeps it queued.
		{"broken ref files", func(t *testing.T) partitionScenario {
			t.Helper()
			workDir, _, refs := setupRepoWithCheckpointRefs(t)
			empty := mustRefName(t, id.MustCheckpointID("eeeeeeeeeee1"))
			garbage := mustRefName(t, id.MustCheckpointID("eeeeeeeeeee2"))
			writeRefFile(t, workDir, empty, "")
			writeRefFile(t, workDir, garbage, "not a hash\n")
			return partitionScenario{
				workDir:     workDir,
				queued:      append([]plumbing.ReferenceName{empty, garbage}, refs...),
				existing:    append([]plumbing.ReferenceName{garbage}, refs...),
				stale:       []plumbing.ReferenceName{empty},
				gitFallback: true,
			}
		}},
		// The listing reports the on-disk shard case. The queued name is the
		// pushable one whichever case the ref is stored under, and its tip is
		// the stored ref's.
		{"case-folded shard", func(t *testing.T) partitionScenario {
			t.Helper()
			workDir, _, refs := setupRepoWithCheckpointRefs(t)
			stored, queued := foldedShardFixture(t, workDir, "HEAD")
			return partitionScenario{
				workDir:  workDir,
				queued:   []plumbing.ReferenceName{queued},
				existing: []plumbing.ReferenceName{queued},
				listed:   append([]plumbing.ReferenceName{stored}, refs...),
				tips:     map[plumbing.ReferenceName]string{queued: stored.String()},
			}
		}},
		// Packing ends the filesystem's case-insensitive lookup: packed-refs
		// matches names exactly, so only the stored name resolves. The split
		// must not change when git packs the ref.
		{"packed case-folded shard", func(t *testing.T) partitionScenario {
			t.Helper()
			workDir, _, refs := setupRepoWithCheckpointRefs(t)
			stored, queued := foldedShardFixture(t, workDir, "HEAD")
			testutil.RunGit(t, workDir, "pack-refs", "--all")
			return partitionScenario{
				workDir:  workDir,
				queued:   []plumbing.ReferenceName{queued},
				existing: []plumbing.ReferenceName{queued},
				listed:   append([]plumbing.ReferenceName{stored}, refs...),
				tips:     map[plumbing.ReferenceName]string{queued: stored.String()},
			}
		}},
		// The go-git listing leaves a symbolic ref to the resolving lookup.
		{"symbolic ref", func(t *testing.T) partitionScenario {
			t.Helper()
			workDir, _, refs := setupRepoWithCheckpointRefs(t)
			symbolic := mustRefName(t, id.MustCheckpointID("0000000000aa"))
			testutil.RunGit(t, workDir, "commit", "--allow-empty", "-m", "symbolic target")
			testutil.RunGit(t, workDir, "update-ref", "refs/heads/target", "HEAD")
			testutil.RunGit(t, workDir, "symbolic-ref", symbolic.String(), "refs/heads/target")
			return partitionScenario{
				workDir:  workDir,
				queued:   []plumbing.ReferenceName{symbolic, refs[0]},
				existing: []plumbing.ReferenceName{symbolic, refs[0]},
				listed:   refs,
				tips:     map[plumbing.ReferenceName]string{symbolic: "refs/heads/target", refs[0]: refs[0].String()},
			}
		}},
		{"SHA-256 repository", func(t *testing.T) partitionScenario {
			t.Helper()
			workDir := t.TempDir()
			testutil.RunGit(t, workDir, "init", "--object-format=sha256")
			testutil.RunGit(t, workDir, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", "init")
			ref := mustRefName(t, id.MustCheckpointID("000000000001"))
			testutil.RunGit(t, workDir, "update-ref", ref.String(), "HEAD")
			return partitionScenario{
				workDir:  workDir,
				queued:   []plumbing.ReferenceName{stale, ref},
				existing: []plumbing.ReferenceName{ref},
				stale:    []plumbing.ReferenceName{stale},
				listed:   []plumbing.ReferenceName{ref},
			}
		}},
	}
	paths := []struct {
		name       string
		useListing bool
	}{
		{"per-ref lookups", false},
		{"namespace listing", true},
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			t.Parallel()
			s := sc.build(t)
			wantListed := make(map[plumbing.ReferenceName]plumbing.Hash, len(s.listed))
			for _, ref := range s.listed {
				wantListed[ref] = plumbing.NewHash(revParse(t, s.workDir, ref.String()))
			}
			for _, p := range paths {
				t.Run(p.name, func(t *testing.T) {
					t.Parallel()
					repo, err := gitrepo.OpenPath(s.workDir)
					require.NoError(t, err)
					defer repo.Close()

					existing, missing, listed := partitionLocalRefsUsing(t.Context(), repo, s.queued, p.useListing)

					assert.Equal(t, s.existing, existing, "pushable refs, in queue order")
					assert.Equal(t, s.stale, missing, "stale refs, in queue order")
					if p.useListing && !s.gitFallback {
						assert.Equal(t, wantListed, listed, "every listed ref maps to its tip")
					} else {
						assert.Nil(t, listed)
					}
					for ref, rev := range s.tips {
						tip, err := localTip(repo, listed, ref)
						require.NoError(t, err)
						assert.Equal(t, revParse(t, s.workDir, rev), tip.String(), "tip of %s", ref)
					}
				})
			}
		})
	}
}

// revParsedTips maps each ref to its tip as git resolves it.
func revParsedTips(t *testing.T, workDir string, refs []plumbing.ReferenceName) map[plumbing.ReferenceName]plumbing.Hash {
	t.Helper()
	tips := make(map[plumbing.ReferenceName]plumbing.Hash, len(refs))
	for _, ref := range refs {
		tips[ref] = plumbing.NewHash(revParse(t, workDir, ref.String()))
	}
	return tips
}

// Both listers must agree with git on every storage shape, including a loose
// ref that shadows a stale packed entry for the same name.
func TestCheckpointRefListers(t *testing.T) {
	t.Parallel()
	scenarios := []struct {
		name  string
		build func(t *testing.T) (string, []plumbing.ReferenceName)
	}{
		{"loose", func(t *testing.T) (string, []plumbing.ReferenceName) {
			t.Helper()
			workDir, _, refs := setupRepoWithCheckpointRefs(t)
			return workDir, refs
		}},
		{"packed", func(t *testing.T) (string, []plumbing.ReferenceName) {
			t.Helper()
			workDir, _, refs := setupRepoWithCheckpointRefs(t)
			testutil.RunGit(t, workDir, "pack-refs", "--all")
			return workDir, refs
		}},
		{"loose over packed", func(t *testing.T) (string, []plumbing.ReferenceName) {
			t.Helper()
			workDir, _, refs := setupRepoWithCheckpointRefs(t)
			testutil.RunGit(t, workDir, "pack-refs", "--all")
			testutil.RunGit(t, workDir, "commit", "--allow-empty", "-m", "newer checkpoint version")
			testutil.RunGit(t, workDir, "update-ref", refs[1].String(), "HEAD")
			return workDir, refs
		}},
		{"SHA-256 repository", func(t *testing.T) (string, []plumbing.ReferenceName) {
			t.Helper()
			workDir := t.TempDir()
			testutil.RunGit(t, workDir, "init", "--object-format=sha256")
			testutil.RunGit(t, workDir, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", "init")
			ref := mustRefName(t, id.MustCheckpointID("000000000001"))
			testutil.RunGit(t, workDir, "update-ref", ref.String(), "HEAD")
			return workDir, []plumbing.ReferenceName{ref}
		}},
	}
	listers := []struct {
		name string
		list func(context.Context, *git.Repository) (map[plumbing.ReferenceName]plumbing.Hash, error)
	}{
		{"go-git", listCheckpointRefsWithGoGit},
		{"git for-each-ref", listCheckpointRefsWithGit},
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			t.Parallel()
			workDir, refs := sc.build(t)
			want := revParsedTips(t, workDir, refs)
			for _, l := range listers {
				t.Run(l.name, func(t *testing.T) {
					t.Parallel()
					repo, err := gitrepo.OpenPath(workDir)
					require.NoError(t, err)
					defer repo.Close()

					listed, err := l.list(t.Context(), repo)

					require.NoError(t, err)
					assert.Equal(t, want, listed)
				})
			}
		})
	}
}

// One empty loose ref anywhere under refs/ aborts go-git's walk; git
// for-each-ref skips it, so the namespace is still listed in full.
func TestListLocalCheckpointRefs_FallsBackToGit(t *testing.T) {
	t.Parallel()
	workDir, _, refs := setupRepoWithCheckpointRefs(t)
	writeRefFile(t, workDir, "refs/heads/broken", "")
	repo, err := gitrepo.OpenPath(workDir)
	require.NoError(t, err)
	defer repo.Close()

	_, goGitErr := listCheckpointRefsWithGoGit(t.Context(), repo)
	listed, tipsMatchLookup, err := listLocalCheckpointRefs(t.Context(), repo)

	require.Error(t, goGitErr, "go-git aborts on the empty ref file")
	require.NoError(t, err)
	assert.False(t, tipsMatchLookup, "git's tips only show which refs exist")
	assert.Equal(t, revParsedTips(t, workDir, refs), listed)
}

// git for-each-ref reads a ref file with trailing data as its hash, where
// go-git reads it as the zero hash. Tips must stay the go-git ones whichever
// listing ran, so the fallback listing only decides existence.
func TestPartitionLocalRefsUsing_TrailingDataTipMatchesLookup(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name            string
		breakGoGitWalk  bool
		wantListedNames bool
	}{
		{name: "go-git listing", wantListedNames: true},
		{name: "for-each-ref fallback", breakGoGitWalk: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			workDir, _, refs := setupRepoWithCheckpointRefs(t)
			junk := mustRefName(t, id.MustCheckpointID("aaaaaaaaaaa3"))
			writeRefFile(t, workDir, junk, revParse(t, workDir, "HEAD")+" junk\n")
			if tc.breakGoGitWalk {
				writeRefFile(t, workDir, "refs/heads/broken", "")
			}
			repo, err := gitrepo.OpenPath(workDir)
			require.NoError(t, err)
			defer repo.Close()
			lookup, err := repo.Reference(junk, true)
			require.NoError(t, err)

			queued := append([]plumbing.ReferenceName{junk}, refs...)
			existing, missing, listed := partitionLocalRefsUsing(t.Context(), repo, queued, true)
			tip, err := localTip(repo, listed, junk)

			assert.Equal(t, queued, existing)
			assert.Empty(t, missing)
			require.NoError(t, err)
			assert.Equal(t, lookup.Hash(), tip)
			if tc.wantListedNames {
				assert.Equal(t, revParsedTips(t, workDir, refs), listed)
			} else {
				assert.Nil(t, listed)
			}
		})
	}
}

// With both listings failing, the split is the per-ref one and nothing is listed.
func TestPartitionLocalRefsUsing_NoListing(t *testing.T) {
	t.Parallel()
	workDir, _, refs := setupRepoWithCheckpointRefs(t)
	writeRefFile(t, workDir, "refs/heads/broken", "")
	repo, err := gitrepo.OpenPath(workDir)
	require.NoError(t, err)
	defer repo.Close()
	stale := mustRefName(t, id.MustCheckpointID("ffffffffffff"))
	queued := []plumbing.ReferenceName{refs[1], stale, refs[0]}
	wantExisting, wantStale, _ := partitionLocalRefsUsing(t.Context(), repo, queued, false)
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()

	_, _, listErr := listLocalCheckpointRefs(cancelled, repo)
	existing, missing, listed := partitionLocalRefsUsing(cancelled, repo, queued, true)

	require.Error(t, listErr, "both listings fail")
	assert.Equal(t, wantExisting, existing)
	assert.Equal(t, wantStale, missing)
	assert.Nil(t, listed)
}

// remoteCheckpointRefs maps every ref the remote holds for cid to its tip, by
// exact name, so two spellings of one checkpoint show up as two entries.
func remoteCheckpointRefs(t *testing.T, bareDir string, cid id.CheckpointID) map[plumbing.ReferenceName]string {
	t.Helper()
	out := testutil.RunGit(t, bareDir, "for-each-ref", "--format=%(refname) %(objectname)", checkpoint.CheckpointRefPrefix)
	refs := make(map[plumbing.ReferenceName]string)
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		name, hash, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		if got, parsed := checkpoint.ParseRef(plumbing.ReferenceName(name)); parsed && got == cid {
			refs[plumbing.ReferenceName(name)] = hash
		}
	}
	return refs
}

// TestPushQueuedCheckpointRefs_PushesFoldedShardRef covers a queue entry whose
// ref is stored under its case-folded shard spelling: the queued name no longer
// resolves once git has packed the ref, so partitionCheckpointRefPushes used to
// classify it stale, prune it, and leave the checkpoint unpushed forever. The
// remote receives the canonical name: the stored spelling is this clone's
// filesystem accident, and other clones read the canonical one.
func TestPushQueuedCheckpointRefs_PushesFoldedShardRef(t *testing.T) {
	workDir, bareDir, _ := setupRepoWithCheckpointRefs(t)
	t.Chdir(workDir)
	paths.ClearWorktreeRootCache()

	repo, err := git.PlainOpen(workDir)
	require.NoError(t, err)
	cid := id.MustCheckpointID("01M2DCHJCHTR9T9MZTSB7WV76B")
	canonical := mustRefName(t, cid)
	folded, ok := checkpoint.FoldedRefName(cid)
	require.True(t, ok)
	head, err := repo.Head()
	require.NoError(t, err)
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(folded, head.Hash())))
	// Packing is what removes the filesystem's case-insensitive rescue, so the
	// canonical name stops resolving on every platform rather than only on a
	// case-sensitive one.
	testutil.RunGit(t, workDir, "pack-refs", "--all")

	// Reopen after packing: this models a later pre-push process holding only
	// the canonical spelling in its queue.
	repo, err = git.PlainOpen(workDir)
	require.NoError(t, err)
	queue := enqueueRefs(t, repo, []plumbing.ReferenceName{canonical})

	pushed, pushDisabled, err := PushQueuedCheckpointRefs(context.Background(), repo, bareDir)
	require.NoError(t, err)
	assert.False(t, pushDisabled)
	assert.Equal(t, 1, pushed)
	assert.Equal(t, map[plumbing.ReferenceName]string{canonical: head.Hash().String()}, remoteCheckpointRefs(t, bareDir, cid),
		"the checkpoint reaches the remote under its canonical name only")

	remaining, err := queue.Drain()
	require.NoError(t, err)
	assert.Empty(t, remaining, "a pushed entry is pruned by its token, whatever spelling resolved")
}

// A checkpoint pushed while its ref is loose and again after git packs it must
// land on one remote ref. Pushing the stored spelling sent it as one name
// before packing and another after, leaving two refs with different tips on a
// case-sensitive remote.
func TestPushQueuedCheckpointRefs_FoldedShardRefKeepsItsRemoteNameAcrossPacking(t *testing.T) {
	workDir, bareDir, _ := setupRepoWithCheckpointRefs(t)
	t.Chdir(workDir)
	paths.ClearWorktreeRootCache()
	cid := id.MustCheckpointID("0000000000ab")
	stored, queued := foldedShardFixture(t, workDir, "HEAD")
	push := func() {
		t.Helper()
		repo, err := gitrepo.OpenPath(workDir)
		require.NoError(t, err)
		defer repo.Close()
		enqueueRefs(t, repo, []plumbing.ReferenceName{queued})
		pushed, _, err := PushQueuedCheckpointRefs(context.Background(), repo, bareDir)
		require.NoError(t, err)
		require.Equal(t, 1, pushed)
	}

	push()
	require.Equal(t, map[plumbing.ReferenceName]string{queued: revParse(t, workDir, "HEAD")}, remoteCheckpointRefs(t, bareDir, cid))

	testutil.RunGit(t, workDir, "commit", "--allow-empty", "-m", "newer checkpoint version")
	newer := revParse(t, workDir, "HEAD")
	testutil.RunGit(t, workDir, "update-ref", stored.String(), newer)
	testutil.RunGit(t, workDir, "pack-refs", "--all")
	push()

	require.Equal(t, map[plumbing.ReferenceName]string{queued: newer}, remoteCheckpointRefs(t, bareDir, cid),
		"the newer version must advance the same remote ref")
}

// Recovery fetches the canonical name from the remote and installs the replayed
// tip on the ref this clone stores, whatever its spelling.
func TestPushQueuedCheckpointRefs_FoldedShardRefRecoversOntoDivergedRemote(t *testing.T) {
	workDir, bareDir, _ := setupRepoWithCheckpointRefs(t)
	t.Chdir(workDir)
	paths.ClearWorktreeRootCache()
	cid := id.MustCheckpointID("0000000000ab")
	c1 := revParse(t, workDir, "HEAD")
	queued := mustRefName(t, cid)

	// The remote holds the canonical name at C2, a child of C1 adding b.txt.
	testutil.WriteFile(t, workDir, "b.txt", "b")
	testutil.GitAdd(t, workDir, "b.txt")
	testutil.GitCommit(t, workDir, "add b")
	testutil.RunGit(t, workDir, "push", bareDir, "HEAD:"+queued.String())

	// This clone holds C3, a sibling of C2 adding c.txt, stored folded and packed.
	testutil.GitReset(t, workDir, c1)
	testutil.WriteFile(t, workDir, "c.txt", "c")
	testutil.GitAdd(t, workDir, "c.txt")
	testutil.GitCommit(t, workDir, "add c")
	stored, _ := foldedShardFixture(t, workDir, "HEAD")
	testutil.RunGit(t, workDir, "pack-refs", "--all")

	repo, err := gitrepo.OpenPath(workDir)
	require.NoError(t, err)
	defer repo.Close()
	queue := enqueueRefs(t, repo, []plumbing.ReferenceName{queued})
	pushed, pushErr := flushCheckpointRefsQueue(context.Background(), repo, pushSettings{remote: bareDir}, batchWithinFlushBudget)
	require.NoError(t, pushErr)
	require.Equal(t, 1, pushed)
	remaining, err := queue.Drain()
	require.NoError(t, err)
	assert.Empty(t, remaining)

	files := remoteRefFiles(t, bareDir, queued)
	assert.Contains(t, files, "b.txt", "the remote's change is kept")
	assert.Contains(t, files, "c.txt", "the local change is replayed on top")
	remoteRefs := remoteCheckpointRefs(t, bareDir, cid)
	require.Len(t, remoteRefs, 1, "recovery must not add a second remote ref")
	assert.Equal(t, map[plumbing.ReferenceName]string{stored: remoteRefs[queued]}, remoteCheckpointRefs(t, workDir, cid),
		"the replayed tip is installed on the stored ref, and no second local ref appears")
}

// A remote that holds the folded name gains the canonical ref at the current
// tip, and the folded one is left where it was: readers ask for the canonical
// name first, and removing a remote ref is not the push's to do.
func TestPushQueuedCheckpointRefs_RemoteHoldingFoldedNameGainsCanonical(t *testing.T) {
	workDir, bareDir, _ := setupRepoWithCheckpointRefs(t)
	t.Chdir(workDir)
	paths.ClearWorktreeRootCache()
	cid := id.MustCheckpointID("0000000000ab")
	older := revParse(t, workDir, "HEAD")
	stored, queued := foldedShardFixture(t, workDir, older)
	testutil.RunGit(t, workDir, "push", bareDir, older+":"+stored.String())
	// Packed, so a case-insensitive filesystem under the remote cannot answer
	// to the canonical name with the folded ref's file.
	testutil.RunGit(t, bareDir, "pack-refs", "--all")

	testutil.RunGit(t, workDir, "commit", "--allow-empty", "-m", "newer checkpoint version")
	newer := revParse(t, workDir, "HEAD")
	testutil.RunGit(t, workDir, "update-ref", stored.String(), newer)
	testutil.RunGit(t, workDir, "pack-refs", "--all")

	repo, err := gitrepo.OpenPath(workDir)
	require.NoError(t, err)
	defer repo.Close()
	enqueueRefs(t, repo, []plumbing.ReferenceName{queued})
	pushed, _, err := PushQueuedCheckpointRefs(context.Background(), repo, bareDir)
	require.NoError(t, err)
	require.Equal(t, 1, pushed)

	assert.Equal(t, map[plumbing.ReferenceName]string{queued: newer, stored: older}, remoteCheckpointRefs(t, bareDir, cid))
}

// A queue can hold one checkpoint under both spellings of its shard, because
// the spelling a write enqueues follows how the ref is stored at that moment.
// Both entries name one destination, and git refuses a push that names a
// destination twice, taking the other refs of the batch down with it.
func TestPartitionCheckpointRefPushes_BothShardSpellingsQueuedMakeOnePush(t *testing.T) {
	workDir, bareDir, refs := setupRepoWithCheckpointRefs(t)
	t.Chdir(workDir)
	paths.ClearWorktreeRootCache()
	stored, queued := foldedShardFixture(t, workDir, "HEAD")
	testutil.RunGit(t, workDir, "pack-refs", "--all")

	repo, err := gitrepo.OpenPath(workDir)
	require.NoError(t, err)
	defer repo.Close()
	entries := []checkpoint.PushQueueEntry{{Ref: refs[0]}, {Ref: queued}, {Ref: stored}}
	part := partitionCheckpointRefPushes(repo, entries, checkpointRefSelection{})

	require.NoError(t, batchPushCheckpointRefs(context.Background(), bareDir, part.ready),
		"one batch must carry every queued checkpoint")
	assert.ElementsMatch(t, entries, checkpointRefPushTokens(part.ready),
		"both queue entries are delivered by the one push, so both must be dequeued")
}

func TestPushQueuedCheckpointRefs_BothShardSpellingsQueuedPushOnce(t *testing.T) {
	workDir, bareDir, refs := setupRepoWithCheckpointRefs(t)
	t.Chdir(workDir)
	paths.ClearWorktreeRootCache()
	cid := id.MustCheckpointID("0000000000ab")
	stored, queued := foldedShardFixture(t, workDir, "HEAD")
	testutil.RunGit(t, workDir, "pack-refs", "--all")

	repo, err := gitrepo.OpenPath(workDir)
	require.NoError(t, err)
	defer repo.Close()
	queue := enqueueRefs(t, repo, []plumbing.ReferenceName{refs[0], queued, stored})
	pushed, _, err := PushQueuedCheckpointRefs(context.Background(), repo, bareDir)
	require.NoError(t, err)

	assert.Equal(t, 2, pushed, "two checkpoints are queued, one of them twice")
	assert.Equal(t, map[plumbing.ReferenceName]string{queued: revParse(t, workDir, "HEAD")}, remoteCheckpointRefs(t, bareDir, cid))
	remaining, err := queue.Drain()
	require.NoError(t, err)
	assert.Empty(t, remaining, "both spellings leave the queue")
}

// git can leave the canonical name at an older commit than the folded one
// (see checkpoint.SupersededBy). The push sends the newer version.
func TestPushQueuedCheckpointRefs_PushesTheNewerRefWhenBothShardSpellingsExist(t *testing.T) {
	workDir, bareDir, _ := setupRepoWithCheckpointRefs(t)
	t.Chdir(workDir)
	paths.ClearWorktreeRootCache()
	cid := id.MustCheckpointID("0000000000ab")
	stored := plumbing.ReferenceName(checkpoint.CheckpointRefPrefix + "AB/0000000000ab")
	queued := mustRefName(t, cid)
	testutil.RunGit(t, workDir, "update-ref", queued.String(), "HEAD")
	testutil.RunGit(t, workDir, "pack-refs", "--all")
	testutil.RunGit(t, workDir, "commit", "--allow-empty", "-m", "newer checkpoint version")
	newer := revParse(t, workDir, "HEAD")
	testutil.RunGit(t, workDir, "update-ref", stored.String(), newer)
	testutil.RunGit(t, workDir, "pack-refs", "--all")

	repo, err := gitrepo.OpenPath(workDir)
	require.NoError(t, err)
	defer repo.Close()
	queue := enqueueRefs(t, repo, []plumbing.ReferenceName{queued})
	pushed, _, err := PushQueuedCheckpointRefs(context.Background(), repo, bareDir)
	require.NoError(t, err)

	assert.Equal(t, 1, pushed)
	assert.Equal(t, map[plumbing.ReferenceName]string{queued: newer}, remoteCheckpointRefs(t, bareDir, cid))
	remaining, err := queue.Drain()
	require.NoError(t, err)
	assert.Empty(t, remaining)
}

// Both spellings can hold refs whose histories diverged, so neither is a later
// version of the other. Only one can take the destination in an upload; the
// other stays queued and the next push replays it onto the first.
func TestPushQueuedCheckpointRefs_DivergedShardSpellingsPushInTurn(t *testing.T) {
	workDir, bareDir, _ := setupRepoWithCheckpointRefs(t)
	t.Chdir(workDir)
	paths.ClearWorktreeRootCache()
	cid := id.MustCheckpointID("0000000000ab")
	base := revParse(t, workDir, "HEAD")
	testutil.WriteFile(t, workDir, "b.txt", "b")
	testutil.GitAdd(t, workDir, "b.txt")
	testutil.GitCommit(t, workDir, "add b")
	first := revParse(t, workDir, "HEAD")
	stored, queued := foldedShardFixture(t, workDir, first)
	// Packed between the two writes, so the second name is a ref of its own on
	// a case-insensitive filesystem too.
	testutil.RunGit(t, workDir, "pack-refs", "--all")
	testutil.GitReset(t, workDir, base)
	testutil.WriteFile(t, workDir, "c.txt", "c")
	testutil.GitAdd(t, workDir, "c.txt")
	testutil.GitCommit(t, workDir, "add c")
	testutil.RunGit(t, workDir, "update-ref", queued.String(), "HEAD")
	testutil.RunGit(t, workDir, "pack-refs", "--all")

	repo, err := gitrepo.OpenPath(workDir)
	require.NoError(t, err)
	defer repo.Close()
	queue := enqueueRefs(t, repo, []plumbing.ReferenceName{stored, queued})

	pushed, _, err := PushQueuedCheckpointRefs(context.Background(), repo, bareDir)
	require.NoError(t, err)
	assert.Equal(t, 1, pushed)
	assert.Equal(t, map[plumbing.ReferenceName]string{queued: first}, remoteCheckpointRefs(t, bareDir, cid))
	remaining, err := queue.Drain()
	require.NoError(t, err)
	assert.Equal(t, []plumbing.ReferenceName{queued}, remaining, "the ref that did not go stays queued")

	pushed, _, err = PushQueuedCheckpointRefs(context.Background(), repo, bareDir)
	require.NoError(t, err)
	assert.Equal(t, 1, pushed)
	files := remoteRefFiles(t, bareDir, queued)
	assert.Contains(t, files, "b.txt", "the first upload is kept")
	assert.Contains(t, files, "c.txt", "the second is replayed on top")
	remaining, err = queue.Drain()
	require.NoError(t, err)
	assert.Empty(t, remaining)
}

func TestBatchPushCheckpointRefs(t *testing.T) {
	workDir, bareDir, refs := setupRepoWithCheckpointRefs(t)
	t.Chdir(workDir)
	repo, err := git.PlainOpen(workDir)
	require.NoError(t, err)

	require.NoError(t, pushCheckpointRefsAtTips(t, repo, bareDir, refs))

	// All refs now exist on the bare remote.
	lsCmd := exec.CommandContext(context.Background(), "git", "ls-remote", bareDir)
	lsCmd.Env = testutil.GitIsolatedEnv()
	out, err := lsCmd.CombinedOutput()
	require.NoError(t, err, "ls-remote failed: %s", out)
	remoteRefs := string(out)
	for _, ref := range refs {
		assert.Contains(t, remoteRefs, ref.String(), "ref should be present on the remote after batch push")
	}
}

func TestBatchPushCheckpointRefs_PinsVerifiedHash(t *testing.T) {
	workDir, bareDir, refs := setupRepoWithCheckpointRefs(t)
	t.Chdir(workDir)
	repo, err := git.PlainOpen(workDir)
	require.NoError(t, err)
	refName := refs[0]
	verified, err := repo.Reference(refName, true)
	require.NoError(t, err)

	testutil.WriteFile(t, workDir, "later.txt", "later")
	testutil.GitAdd(t, workDir, "later.txt")
	testutil.GitCommit(t, workDir, "later")
	later, err := repo.Head()
	require.NoError(t, err)
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(refName, later.Hash())))

	candidate := checkpointRefPush{
		token: checkpoint.PushQueueEntry{Ref: refName, Hash: verified.Hash()},
		name:  refName,
		hash:  verified.Hash(),
	}
	require.NoError(t, batchPushCheckpointRefs(t.Context(), bareDir, []checkpointRefPush{candidate}))

	assert.Equal(t, verified.Hash().String(), remoteRefHash(t, bareDir, refName),
		"the remote must receive the captured hash, not the ref's later tip")
	current, err := repo.Reference(refName, true)
	require.NoError(t, err)
	assert.Equal(t, later.Hash(), current.Hash(), "pushing the captured hash must not rewind the local ref")
}

func TestBatchPushCheckpointRefs_Empty(t *testing.T) {
	t.Parallel()
	// No refs → no git invocation, no error.
	require.NoError(t, batchPushCheckpointRefs(context.Background(), "unused-target", nil))
}

// TestBatchPushCheckpointRefs_AllowsFastForward: advancing a checkpoint ref to a
// descendant commit (the normal case) pushes fine without force.
func TestBatchPushCheckpointRefs_AllowsFastForward(t *testing.T) {
	workDir, bareDir, refs := setupRepoWithCheckpointRefs(t)
	t.Chdir(workDir)
	repo, err := git.PlainOpen(workDir)
	require.NoError(t, err)

	require.NoError(t, pushCheckpointRefsAtTips(t, repo, bareDir, refs))

	// Advance refs[0] to a child commit (fast-forward).
	testutil.WriteFile(t, workDir, "two.txt", "second")
	testutil.GitAdd(t, workDir, "two.txt")
	testutil.GitCommit(t, workDir, "second")
	head2, err := repo.Head()
	require.NoError(t, err)
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(refs[0], head2.Hash())))

	require.NoError(t, pushCheckpointRefsAtTips(t, repo, bareDir, refs[:1]), "fast-forward update should push without force")
	assert.Equal(t, head2.Hash().String(), remoteRefHash(t, bareDir, refs[0]),
		"remote ref should advance to the descendant commit")
}

// TestBatchPushCheckpointRefs_RejectsNonFastForward: a divergent (non-descendant)
// update is rejected, and the remote ref is left untouched — the safety property
// that distinguishes this from a force push (we have no server-side ref
// protection).
func TestBatchPushCheckpointRefs_RejectsNonFastForward(t *testing.T) {
	workDir, bareDir, refs := setupRepoWithCheckpointRefs(t)
	t.Chdir(workDir)
	repo, err := git.PlainOpen(workDir)
	require.NoError(t, err)

	require.NoError(t, pushCheckpointRefsAtTips(t, repo, bareDir, refs))
	original := remoteRefHash(t, bareDir, refs[0])

	// Point refs[0] at an orphan commit (no parent) — not a descendant of what was
	// pushed, so the update is non-fast-forward.
	runGit := func(args ...string) string {
		return strings.TrimSpace(testutil.RunGit(t, workDir, args...))
	}
	tree := runGit("rev-parse", "HEAD^{tree}")
	orphan := runGit("commit-tree", tree, "-m", "divergent")
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(refs[0], plumbing.NewHash(orphan))))

	err = pushCheckpointRefsAtTips(t, repo, bareDir, refs[:1])
	require.Error(t, err, "a non-fast-forward update must be rejected, not force-pushed")
	assert.Equal(t, original, remoteRefHash(t, bareDir, refs[0]),
		"remote ref must be unchanged after a rejected non-fast-forward push")
}

// TestPushCheckpointRefWithRecovery_MergesDivergedRef: when a checkpoint ref has
// diverged on the remote (the same checkpoint advanced differently elsewhere), the
// recovery fetches the remote tip and replays the local-only commit on top, so the
// retry is a fast-forward — preserving the remote's change instead of overwriting
// it. Non-overlapping changes merge.
func TestPushCheckpointRefWithRecovery_MergesDivergedRef(t *testing.T) {
	workDir, bareDir, refs := setupRepoWithCheckpointRefs(t)
	t.Chdir(workDir)
	ctx := context.Background()
	ref := refs[0]

	repo, err := git.PlainOpen(workDir)
	require.NoError(t, err)
	head := func() plumbing.Hash {
		h, e := repo.Head()
		require.NoError(t, e)
		return h.Hash()
	}
	setRef := func(h plumbing.Hash) {
		require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(ref, h)))
	}

	c1 := head()
	require.NoError(t, pushCheckpointRefsAtTips(t, repo, bareDir, []plumbing.ReferenceName{ref})) // remote ref = C1

	// Remote advances: C2 (child of C1) adds b.txt; point the ref at it and push.
	testutil.WriteFile(t, workDir, "b.txt", "b")
	testutil.GitAdd(t, workDir, "b.txt")
	testutil.GitCommit(t, workDir, "add b")
	setRef(head())
	require.NoError(t, pushCheckpointRefsAtTips(t, repo, bareDir, []plumbing.ReferenceName{ref})) // remote ref = C2

	// Local diverges: reset to C1 and make C3 (sibling of C2) adding c.txt.
	testutil.GitReset(t, workDir, c1.String())
	testutil.WriteFile(t, workDir, "c.txt", "c")
	testutil.GitAdd(t, workDir, "c.txt")
	testutil.GitCommit(t, workDir, "add c")
	setRef(head())

	// C3 is not a descendant of the remote's C2 → the batch and individual
	// pushes are rejected, then recovery replays C3's delta onto C2.
	queue := enqueueRefs(t, repo, []plumbing.ReferenceName{ref})
	restore := captureStderr(t)
	pushed, pushErr := flushCheckpointRefsQueue(ctx, repo, pushSettings{remote: bareDir}, batchWithinFlushBudget)
	output := restore()
	require.NoError(t, pushErr, "diverged ref should be recovered by fetch+replay, not rejected")
	assert.Equal(t, 1, pushed)
	assert.NotContains(t, output, "Warning:", "plain divergence should recover quietly")
	remaining, err := queue.Drain()
	require.NoError(t, err)
	assert.Empty(t, remaining, "recovered ref landed and must leave the queue")

	files := remoteRefFiles(t, bareDir, ref)
	assert.Contains(t, files, "b.txt", "remote-only change must be preserved (not overwritten)")
	assert.Contains(t, files, "c.txt", "local-only change must be replayed on top")
}

func TestPushCheckpointRefWithRecovery_PreservesConcurrentGenerationOnCASConflict(t *testing.T) {
	workDir, bareDir, refs := setupRepoWithCheckpointRefs(t)
	t.Chdir(workDir)
	ctx := context.Background()
	ref := refs[0]

	repo, err := git.PlainOpen(workDir)
	require.NoError(t, err)
	head, err := repo.Head()
	require.NoError(t, err)
	base := head.Hash()
	require.NoError(t, pushCheckpointRefsAtTips(t, repo, bareDir, []plumbing.ReferenceName{ref}))

	// Advance the remote from base.
	testutil.WriteFile(t, workDir, "remote.txt", "remote")
	testutil.GitAdd(t, workDir, "remote.txt")
	testutil.GitCommit(t, workDir, "remote generation")
	head, err = repo.Head()
	require.NoError(t, err)
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(ref, head.Hash())))
	require.NoError(t, pushCheckpointRefsAtTips(t, repo, bareDir, []plumbing.ReferenceName{ref}))
	remoteTip := head.Hash()

	// Create a disconnected queued generation that recovery will try to replay.
	// This covers the former ReconcileDisconnectedMetadataRef path, whose final
	// update was unconditional before checkpoint recovery became generation-safe.
	testutil.GitReset(t, workDir, base.String())
	testutil.WriteFile(t, workDir, "local.txt", "local")
	testutil.GitAdd(t, workDir, "local.txt")
	tree := strings.TrimSpace(testutil.RunGit(t, workDir, "write-tree"))
	candidate := plumbing.NewHash(strings.TrimSpace(testutil.RunGit(t, workDir, "commit-tree", tree, "-m", "local generation")))
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(ref, candidate)))
	queue := enqueueRefs(t, repo, []plumbing.ReferenceName{ref})

	// At the recovery install boundary, simulate a checkpoint writer advancing
	// the same ref and queueing that newer generation. The real CAS must reject
	// the stale recovery rather than overwrite it.
	oldCAS := checkpointRefRecoveryCAS
	var concurrent plumbing.Hash
	checkpointRefRecoveryCAS = func(
		casCtx context.Context,
		casRepo *git.Repository,
		refName plumbing.ReferenceName,
		newHash, expectedOld plumbing.Hash,
	) error {
		require.Equal(t, expectedOld, candidate)
		runGit := func(args ...string) string {
			return strings.TrimSpace(testutil.RunGit(t, workDir, args...))
		}
		testutil.WriteFile(t, workDir, "concurrent.txt", "concurrent")
		testutil.GitAdd(t, workDir, "concurrent.txt")
		concurrentTree := runGit("write-tree")
		concurrent = plumbing.NewHash(runGit("commit-tree", concurrentTree, "-p", candidate.String(), "-m", "concurrent generation"))
		require.NoError(t, casRepo.Storer.SetReference(plumbing.NewHashReference(refName, concurrent)))
		require.NoError(t, queue.EnqueueEntry(checkpoint.PushQueueEntry{Ref: refName, Hash: concurrent}))
		return checkpoint.CASPersistentRef(casCtx, casRepo, refName, newHash, expectedOld)
	}
	t.Cleanup(func() { checkpointRefRecoveryCAS = oldCAS })

	restore := captureStderr(t)
	pushed, pushErr := flushCheckpointRefsQueue(ctx, repo, pushSettings{remote: bareDir}, batchUnbounded)
	restore()
	require.NoError(t, pushErr, "losing the CAS to a concurrent write is superseded, not a failed push")
	assert.Zero(t, pushed)

	localRef, err := repo.Reference(ref, true)
	require.NoError(t, err)
	assert.Equal(t, concurrent, localRef.Hash(), "recovery must not overwrite the concurrent checkpoint generation")
	assert.Equal(t, remoteTip.String(), remoteRefHash(t, bareDir, ref), "a CAS conflict must not push a stale recovered tip")
	queued, err := queue.PeekEntries()
	require.NoError(t, err)
	require.Len(t, queued, 1)
	assert.Equal(t, concurrent, queued[0].Hash, "the concurrent generation must remain queued")

	// The losing recovered commit is only a derived intermediate: the next
	// delivery replays the queued winner onto the remote and preserves all three
	// sources of data without keeping that stale intermediate under another ref.
	checkpointRefRecoveryCAS = oldCAS
	restore = captureStderr(t)
	pushed, pushErr = flushCheckpointRefsQueue(ctx, repo, pushSettings{remote: bareDir}, batchUnbounded)
	restore()
	require.NoError(t, pushErr)
	assert.Equal(t, 1, pushed)
	queued, err = queue.PeekEntries()
	require.NoError(t, err)
	assert.Empty(t, queued)
	files := remoteRefFiles(t, bareDir, ref)
	assert.Contains(t, files, "remote.txt")
	assert.Contains(t, files, "local.txt")
	assert.Contains(t, files, "concurrent.txt")
}

// recoveryCAS is the signature of the checkpointRefRecoveryCAS seam.
type recoveryCAS func(ctx context.Context, repo *git.Repository, refName plumbing.ReferenceName, newHash, expectedOld plumbing.Hash) error

// stubRecoveryCAS replaces the recovery install for one test.
func stubRecoveryCAS(t *testing.T, cas recoveryCAS) {
	t.Helper()
	old := checkpointRefRecoveryCAS
	checkpointRefRecoveryCAS = cas
	t.Cleanup(func() { checkpointRefRecoveryCAS = old })
}

// loseRecoveryCAS moves the ref to base just before the install, as a concurrent
// session write would, so the real CAS reports a conflict.
func loseRecoveryCAS(t *testing.T, base plumbing.Hash) recoveryCAS {
	t.Helper()
	return func(ctx context.Context, repo *git.Repository, refName plumbing.ReferenceName, newHash, expectedOld plumbing.Hash) error {
		require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(refName, base)))
		return checkpoint.CASPersistentRef(ctx, repo, refName, newHash, expectedOld)
	}
}

// divergeCheckpointRefs pushes n checkpoint refs at a remote generation, points
// them locally at a sibling generation, and queues them, so each one needs
// recovery. It returns the work repo, the remote, the refs, their common base,
// and the queue.
func divergeCheckpointRefs(t *testing.T, n int) (*git.Repository, string, []plumbing.ReferenceName, plumbing.Hash, *checkpoint.PushQueue) {
	t.Helper()
	workDir, bareDir, refs := setupRepoWithNCheckpointRefs(t, n)
	t.Chdir(workDir)
	repo, err := git.PlainOpen(workDir)
	require.NoError(t, err)
	head, err := repo.Head()
	require.NoError(t, err)
	base := head.Hash()
	setRefs := func(h plumbing.Hash) {
		for _, ref := range refs {
			require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(ref, h)))
		}
	}

	testutil.WriteFile(t, workDir, "remote.txt", "remote")
	testutil.GitAdd(t, workDir, "remote.txt")
	testutil.GitCommit(t, workDir, "remote generation")
	head, err = repo.Head()
	require.NoError(t, err)
	setRefs(head.Hash())
	require.NoError(t, pushCheckpointRefsAtTips(t, repo, bareDir, refs))

	testutil.GitReset(t, workDir, base.String())
	testutil.WriteFile(t, workDir, "local.txt", "local")
	testutil.GitAdd(t, workDir, "local.txt")
	testutil.GitCommit(t, workDir, "local generation")
	head, err = repo.Head()
	require.NoError(t, err)
	setRefs(head.Hash())
	return repo, bareDir, refs, base, enqueueRefs(t, repo, refs)
}

// A recovery that loses its CAS to a concurrent write leaves the newer
// generation local and queued. That is not a failed push: it must not count
// toward maxConsecutiveRefPushFailures, so a burst of concurrent writes cannot
// stop the retry before the remaining refs are attempted.
func TestFlushCheckpointRefsQueue_SupersededRecoveriesDoNotStopTheRetry(t *testing.T) {
	const n = maxConsecutiveRefPushFailures + 1
	repo, bareDir, _, base, queue := divergeCheckpointRefs(t, n)
	lose := loseRecoveryCAS(t, base)
	installs := 0
	stubRecoveryCAS(t, func(ctx context.Context, casRepo *git.Repository, refName plumbing.ReferenceName, newHash, expectedOld plumbing.Hash) error {
		installs++
		return lose(ctx, casRepo, refName, newHash, expectedOld)
	})

	restore := captureStderr(t)
	pushed, pushErr := flushCheckpointRefsQueue(context.Background(), repo, pushSettings{remote: bareDir}, batchUnbounded)
	output := restore()
	require.NoError(t, pushErr, output)
	assert.Zero(t, pushed)
	assert.Equal(t, n, installs, "every superseded ref must still be attempted")
	assert.NotContains(t, output, "Stopped retrying")
	queued, err := queue.PeekEntries()
	require.NoError(t, err)
	assert.Len(t, queued, n, "superseded refs stay queued for the next push")

	// With no concurrent write, the next push delivers every superseded ref.
	stubRecoveryCAS(t, checkpoint.CASPersistentRef)
	restore = captureStderr(t)
	pushed, pushErr = flushCheckpointRefsQueue(context.Background(), repo, pushSettings{remote: bareDir}, batchUnbounded)
	output = restore()
	require.NoError(t, pushErr, output)
	assert.Equal(t, n, pushed)
	queued, err = queue.PeekEntries()
	require.NoError(t, err)
	assert.Empty(t, queued)
}

// A selected ref superseded during recovery is reported as changed with the
// commit now waiting, so a caller has the version to pin next.
func TestFlushCheckpointRefs_SupersededSelectedRefReportsItsNewTip(t *testing.T) {
	repo, bareDir, refs, base, _ := divergeCheckpointRefs(t, 1)
	stubRecoveryCAS(t, loseRecoveryCAS(t, base))
	selection := checkpointRefSelection{refs: map[plumbing.ReferenceName]plumbing.Hash{refs[0]: plumbing.ZeroHash}}

	restore := captureStderr(t)
	result, err := flushCheckpointRefs(context.Background(), repo, pushSettings{remote: bareDir}, selection)
	restore()
	require.NoError(t, err)
	require.Len(t, result.changed, 1)
	assert.Equal(t, refs[0], result.changed[0].name)
	assert.Equal(t, base, result.changed[0].hash, "the commit the concurrent write left waiting")
}

// Superseded refs were attempted but did not fail, so the flush error counts
// only the real failures among the attempts.
func TestFlushCheckpointRefsQueue_ReportsOnlyRealFailures(t *testing.T) {
	repo, bareDir, refs, base, _ := divergeCheckpointRefs(t, 3)
	lose := loseRecoveryCAS(t, base)
	stubRecoveryCAS(t, func(ctx context.Context, casRepo *git.Repository, refName plumbing.ReferenceName, newHash, expectedOld plumbing.Hash) error {
		if refName == refs[0] {
			return errors.New("simulated install failure")
		}
		return lose(ctx, casRepo, refName, newHash, expectedOld)
	})

	restore := captureStderr(t)
	pushed, pushErr := flushCheckpointRefsQueue(context.Background(), repo, pushSettings{remote: bareDir}, batchUnbounded)
	restore()
	assert.Zero(t, pushed)
	require.ErrorContains(t, pushErr, "1 of 3 attempted checkpoint refs failed to push")
}

// go-git loads a repository handle's pack index once and does not reload it on
// a miss, so recovery must replay from a handle that can see objects its fetch
// wrote into a new pack. With two diverged refs, the first recovery loads the
// index before the second fetch lands.
func TestFlushCheckpointRefsQueue_RecoversARefFetchedIntoANewPack(t *testing.T) {
	workDir, bareDir, refs := setupRepoWithCheckpointRefs(t)
	t.Chdir(workDir)
	ctx := context.Background()
	// Keep every fetch as a pack, as git does once a fetch brings at least
	// fetch.unpackLimit objects.
	testutil.RunGit(t, workDir, "config", "fetch.unpackLimit", "1")

	repo, err := git.PlainOpen(workDir)
	require.NoError(t, err)
	head, err := repo.Head()
	require.NoError(t, err)
	base := head.Hash()
	require.NoError(t, pushCheckpointRefsAtTips(t, repo, bareDir, refs))

	// Advance each ref on the remote from another clone, so its objects reach
	// the work repo only through recovery's fetch.
	otherDir := t.TempDir()
	testutil.InitRepo(t, otherDir)
	for i, ref := range refs {
		testutil.RunGit(t, otherDir, "fetch", "-q", bareDir, ref.String())
		testutil.RunGit(t, otherDir, "checkout", "-q", "--detach", "FETCH_HEAD")
		name := fmt.Sprintf("remote-%d.txt", i)
		testutil.WriteFile(t, otherDir, name, name)
		testutil.GitAdd(t, otherDir, name)
		testutil.GitCommit(t, otherDir, "remote generation "+name)
		testutil.RunGit(t, otherDir, "push", "-q", bareDir, "HEAD:"+ref.String())
	}

	for i, ref := range refs {
		testutil.GitReset(t, workDir, base.String())
		name := fmt.Sprintf("local-%d.txt", i)
		testutil.WriteFile(t, workDir, name, name)
		testutil.GitAdd(t, workDir, name)
		testutil.GitCommit(t, workDir, "local generation "+name)
		head, err = repo.Head()
		require.NoError(t, err)
		require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(ref, head.Hash())))
	}
	queue := enqueueRefs(t, repo, refs)

	restore := captureStderr(t)
	pushed, pushErr := flushCheckpointRefsQueue(ctx, repo, pushSettings{remote: bareDir}, batchUnbounded)
	output := restore()
	require.NoError(t, pushErr, output)
	assert.Equal(t, 2, pushed)
	remaining, err := queue.Drain()
	require.NoError(t, err)
	assert.Empty(t, remaining, "both recovered refs landed and must leave the queue")
	for i, ref := range refs {
		files := remoteRefFiles(t, bareDir, ref)
		assert.Contains(t, files, fmt.Sprintf("remote-%d.txt", i), "remote-only change must be preserved")
		assert.Contains(t, files, fmt.Sprintf("local-%d.txt", i), "local-only change must be replayed on top")
	}
}

// pushCheckpointRefsAtTips pins each ref to its current local tip, as the flush
// does, and pushes them in one batch. Tests use it to seed or advance the remote.
func pushCheckpointRefsAtTips(t *testing.T, repo *git.Repository, target string, refs []plumbing.ReferenceName) error {
	t.Helper()
	pushes := make([]checkpointRefPush, 0, len(refs))
	for _, name := range refs {
		ref, err := repo.Reference(name, true)
		require.NoError(t, err)
		pushes = append(pushes, checkpointRefPush{
			token: checkpoint.PushQueueEntry{Ref: name, Hash: ref.Hash()},
			name:  name,
			hash:  ref.Hash(),
		})
	}
	return batchPushCheckpointRefs(t.Context(), target, pushes)
}

// enqueueRefs seeds the repo's push queue with the given refs.
func enqueueRefs(t *testing.T, repo *git.Repository, refs []plumbing.ReferenceName) *checkpoint.PushQueue {
	t.Helper()
	queue, err := checkpoint.PushQueueForRepo(context.Background(), repo)
	require.NoError(t, err)
	for _, ref := range refs {
		require.NoError(t, queue.EnqueueEntry(checkpoint.PushQueueEntry{Ref: ref}))
	}
	return queue
}

func TestPushQueuedCheckpointRefs(t *testing.T) {
	workDir, bareDir, refs := setupRepoWithCheckpointRefs(t)
	t.Chdir(workDir)
	paths.ClearWorktreeRootCache()

	repo, err := git.PlainOpen(workDir)
	require.NoError(t, err)
	queue := enqueueRefs(t, repo, refs)

	pushed, pushDisabled, err := PushQueuedCheckpointRefs(context.Background(), repo, bareDir)
	require.NoError(t, err)
	assert.False(t, pushDisabled)
	assert.Equal(t, len(refs), pushed)

	for _, ref := range refs {
		assert.NotEmpty(t, remoteRefHash(t, bareDir, ref), "ref should be on the remote")
	}
	remaining, err := queue.Drain()
	require.NoError(t, err)
	assert.Empty(t, remaining, "pushed refs are removed from the queue")
}

func TestPushQueuedCheckpointRefs_PushDisabled(t *testing.T) {
	workDir, bareDir, refs := setupRepoWithCheckpointRefs(t)
	t.Chdir(workDir)
	paths.ClearWorktreeRootCache()

	// push_sessions disabled: the push is a no-op, and the caller must be able
	// to tell that apart from an empty queue (pushed==0 with pushing enabled).
	require.NoError(t, os.MkdirAll(filepath.Join(workDir, ".entire"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(workDir, ".entire", "settings.json"),
		[]byte(`{"enabled": true, "strategy_options": {"push_sessions": false}}`),
		0o600,
	))

	repo, err := git.PlainOpen(workDir)
	require.NoError(t, err)
	queue := enqueueRefs(t, repo, refs)

	pushed, pushDisabled, err := PushQueuedCheckpointRefs(context.Background(), repo, bareDir)
	require.NoError(t, err)
	assert.True(t, pushDisabled, "push_sessions=false must be reported as disabled")
	assert.Equal(t, 0, pushed)

	remaining, err := queue.Drain()
	require.NoError(t, err)
	assert.ElementsMatch(t, refs, remaining, "disabled push leaves refs queued")
}

// Under manual, the queue is held, not empty: the caller must be able to tell
// the two apart to report why nothing was pushed.
func TestPushQueuedCheckpointRefs_ManualPolicyHoldsRefs(t *testing.T) {
	workDir, bareDir, refs := setupRepoWithCheckpointRefs(t)
	t.Chdir(workDir)
	paths.ClearWorktreeRootCache()
	policyFile := filepath.Join(workDir, ".git", filepath.FromSlash(settings.CheckpointPushPolicyFile))
	require.NoError(t, os.MkdirAll(filepath.Dir(policyFile), 0o755))
	require.NoError(t, os.WriteFile(policyFile, []byte("manual\n"), 0o600))

	repo, err := git.PlainOpen(workDir)
	require.NoError(t, err)
	queue := enqueueRefs(t, repo, refs)

	pushed, pushDisabled, err := PushQueuedCheckpointRefs(context.Background(), repo, bareDir)
	require.ErrorIs(t, err, ErrCheckpointPushHeldByPolicy)
	assert.False(t, pushDisabled)
	assert.Equal(t, 0, pushed)

	assert.Empty(t, strings.TrimSpace(testutil.RunGit(t, bareDir, "for-each-ref", "refs/entire/")), "manual must not push")
	remaining, err := queue.Drain()
	require.NoError(t, err)
	assert.ElementsMatch(t, refs, remaining, "held refs stay queued")
}

// flushCheckpointRefsQueue checks the policy itself, so a new caller that
// forgets to cannot upload under manual.
func TestFlushCheckpointRefsQueue_ManualPolicySendsNothing(t *testing.T) {
	workDir, bareDir, refs := setupRepoWithCheckpointRefs(t)
	t.Chdir(workDir)
	paths.ClearWorktreeRootCache()
	policyFile := filepath.Join(workDir, ".git", filepath.FromSlash(settings.CheckpointPushPolicyFile))
	require.NoError(t, os.MkdirAll(filepath.Dir(policyFile), 0o755))
	require.NoError(t, os.WriteFile(policyFile, []byte("manual\n"), 0o600))

	repo, err := git.PlainOpen(workDir)
	require.NoError(t, err)
	queue := enqueueRefs(t, repo, refs)

	pushed, err := flushCheckpointRefsQueue(context.Background(), repo, pushSettings{remote: bareDir}, batchUnbounded)
	require.NoError(t, err)
	assert.Equal(t, 0, pushed)

	assert.Empty(t, strings.TrimSpace(testutil.RunGit(t, bareDir, "for-each-ref", "refs/entire/")), "manual must not push")
	remaining, err := queue.Drain()
	require.NoError(t, err)
	assert.ElementsMatch(t, refs, remaining, "held refs stay queued")
}

func TestPushQueuedCheckpointRefs_FailureLeavesRefsQueued(t *testing.T) {
	workDir, _, refs := setupRepoWithCheckpointRefs(t)
	t.Chdir(workDir)
	paths.ClearWorktreeRootCache()

	repo, err := git.PlainOpen(workDir)
	require.NoError(t, err)
	queue := enqueueRefs(t, repo, refs)

	badTarget := filepath.Join(t.TempDir(), "missing.git")
	pushed, _, err := PushQueuedCheckpointRefs(context.Background(), repo, badTarget)
	require.ErrorContains(t, err, "failed to push")
	assert.Equal(t, 0, pushed)

	remaining, err := queue.Drain()
	require.NoError(t, err)
	assert.ElementsMatch(t, refs, remaining, "failed push leaves refs queued")
}

// remoteRefFiles lists the files in the tree a ref points at on the bare remote.
func remoteRefFiles(t *testing.T, bareDir string, ref plumbing.ReferenceName) string {
	t.Helper()
	c := exec.CommandContext(context.Background(), "git", "-C", bareDir, "ls-tree", "-r", "--name-only", ref.String())
	c.Env = testutil.GitIsolatedEnv()
	out, err := c.CombinedOutput()
	require.NoError(t, err, "ls-tree failed: %s", out)
	return string(out)
}

// assertRefsAbsentFromRemote fails if any of refs reached the bare remote.
func assertRefsAbsentFromRemote(t *testing.T, bareDir string, refs []plumbing.ReferenceName, msg string) {
	t.Helper()
	out := testutil.RunGit(t, bareDir, "ls-remote", bareDir)
	for _, ref := range refs {
		assert.NotContains(t, out, ref.String(), msg)
	}
}

// remoteRefHash returns the object hash a ref points at on the bare remote.
func remoteRefHash(t *testing.T, bareDir string, ref plumbing.ReferenceName) string {
	t.Helper()
	lsCmd := exec.CommandContext(context.Background(), "git", "ls-remote", bareDir, ref.String())
	lsCmd.Env = testutil.GitIsolatedEnv()
	out, err := lsCmd.CombinedOutput()
	require.NoError(t, err, "ls-remote failed: %s", out)
	fields := strings.Fields(strings.TrimSpace(string(out)))
	require.NotEmpty(t, fields, "ref %s not found on remote", ref)
	return fields[0]
}

// TestPushQueuedCheckpointRefs_OPFAppliedBeforePush pins the gate this path was
// missing: with OPF enabled, the migration command's opt-in push must re-redact
// the queued refs before they leave the machine, so what lands on the remote is
// the rewritten, OPF-applied commit rather than the 8-layer one.
func TestPushQueuedCheckpointRefs_OPFAppliedBeforePush(t *testing.T) {
	configureFakeOPF(t, &fakeOPFForRewrite{})
	bareDir, repo, refs := setupGitRefsOPFRepo(t, "a1b2c3d4e5f6", "b2c3d4e5f6a1")
	before := refHashes(t, repo, refs)

	pushed, pushDisabled, err := PushQueuedCheckpointRefs(t.Context(), repo, bareDir)
	require.NoError(t, err)
	assert.False(t, pushDisabled)
	assert.Equal(t, len(refs), pushed)

	after := refHashes(t, repo, refs)
	for i, ref := range refs {
		require.NotEqual(t, before[i], after[i], "ref %s must be rewritten before it is pushed", ref)
		commit, commitErr := repo.CommitObject(after[i])
		require.NoError(t, commitErr)
		assert.True(t, trailers.HasOPFApplied(commit.Message), "pushed commit must carry the OPF trailer")
		assert.NotContains(t, treeContents(t, repo, after[i]), "PERSONABC", "the sentinel must be scrubbed")
		assert.Equal(t, after[i].String(), remoteRefHash(t, bareDir, ref),
			"the rewritten commit is what reached the remote, not the 8-layer one")
	}
	assert.Empty(t, queuedRefs(t, repo), "pushed refs leave the queue")
}

// TestPushQueuedCheckpointRefs_OPFFailureWithholdsPush is the fail-closed half:
// when OPF is enabled but the rewrite cannot run, nothing may reach the remote.
func TestPushQueuedCheckpointRefs_OPFFailureWithholdsPush(t *testing.T) {
	configureFakeOPF(t, &fakeRuntimeAlwaysFails{})
	bareDir, repo, refs := setupGitRefsOPFRepo(t, "a1b2c3d4e5f6", "b2c3d4e5f6a1")
	before := refHashes(t, repo, refs)

	pushed, pushDisabled, err := PushQueuedCheckpointRefs(t.Context(), repo, bareDir)
	require.Error(t, err)
	assert.False(t, pushDisabled)
	assert.Equal(t, 0, pushed)

	assert.Equal(t, before, refHashes(t, repo, refs), "a withheld push must leave every ref unmoved")
	assert.ElementsMatch(t, refs, queuedRefs(t, repo), "withheld refs stay queued")
	assertRefsAbsentFromRemote(t, bareDir, refs, "withheld push must not reach the remote")
}

// A push of named refs must not read, warn about or fail on refs it was not
// asked to send.
func TestPartitionCheckpointRefPushes_UnnamedUnreadableRefWaits(t *testing.T) {
	workDir, _, refs := setupRepoWithCheckpointRefs(t)
	t.Chdir(workDir)
	repo, err := git.PlainOpen(workDir)
	require.NoError(t, err)
	loop := plumbing.ReferenceName("refs/entire/loop")
	require.NoError(t, repo.Storer.SetReference(plumbing.NewSymbolicReference(refs[0], loop)))
	require.NoError(t, repo.Storer.SetReference(plumbing.NewSymbolicReference(loop, refs[0])))
	entries := []checkpoint.PushQueueEntry{{Ref: refs[0]}, {Ref: refs[1]}}

	part := partitionCheckpointRefPushes(repo, entries,
		checkpointRefSelection{refs: map[plumbing.ReferenceName]plumbing.Hash{refs[1]: plumbing.ZeroHash}})

	assert.Empty(t, part.unreadable, "a ref nobody named is not read")
	require.Len(t, part.waiting, 1)
	assert.Equal(t, refs[0], part.waiting[0].Ref)
	require.Len(t, part.ready, 1)
	assert.Equal(t, refs[1], part.ready[0].name)
}

// A queued ref that cannot be read (here a symbolic-ref loop, which fails with
// something other than not-found) has no tip to pin. It must still stay queued
// and be reported, not be withheld silently or pruned as stale.
func TestFlushCheckpointRefsQueue_UnreadableRefStaysQueuedAndIsReported(t *testing.T) {
	workDir, bareDir, refs := setupRepoWithCheckpointRefs(t)
	t.Chdir(workDir)
	ctx := context.Background()
	repo, err := git.PlainOpen(workDir)
	require.NoError(t, err)

	loop := plumbing.ReferenceName("refs/entire/loop")
	require.NoError(t, repo.Storer.SetReference(plumbing.NewSymbolicReference(refs[0], loop)))
	require.NoError(t, repo.Storer.SetReference(plumbing.NewSymbolicReference(loop, refs[0])))
	queue := enqueueRefs(t, repo, refs)

	restore := captureStderr(t)
	pushed, pushErr := flushCheckpointRefsQueue(ctx, repo, pushSettings{remote: bareDir}, batchUnbounded)
	output := restore()

	require.NoError(t, pushErr)
	assert.Equal(t, 1, pushed, "the readable ref still ships")
	assert.Contains(t, output, "1 checkpoint ref(s) could not be read and stay queued for the next push (for example "+refs[0].String()+")",
		"the warning names a ref so the user can find it")
	remaining, err := queue.Drain()
	require.NoError(t, err)
	assert.Equal(t, []plumbing.ReferenceName{refs[0]}, remaining, "the unreadable ref stays queued")

	// Once the ref is readable again, the next push delivers it.
	head, err := repo.Head()
	require.NoError(t, err)
	require.NoError(t, repo.Storer.RemoveReference(loop))
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(refs[0], head.Hash())))
	restore = captureStderr(t)
	pushed, pushErr = flushCheckpointRefsQueue(ctx, repo, pushSettings{remote: bareDir}, batchUnbounded)
	restore()
	require.NoError(t, pushErr)
	assert.Equal(t, 1, pushed)
	assert.Equal(t, head.Hash().String(), remoteRefHash(t, bareDir, refs[0]))
	remaining, err = queue.Drain()
	require.NoError(t, err)
	assert.Empty(t, remaining)
}

// TestCheckpointWaitingAndRefExists_FoldedShardRef pins the two reads that
// drive `entire session attach`'s report. A checkpoint ref stored under its
// case-folded shard and then packed does not answer to its canonical name, so
// both reads said the checkpoint did not exist and attach told the user to run
// a command that could never find it.
func TestCheckpointWaitingAndRefExists_FoldedShardRef(t *testing.T) {
	workDir, _, _ := setupRepoWithCheckpointRefs(t)
	t.Chdir(workDir)
	paths.ClearWorktreeRootCache()

	repo, err := git.PlainOpen(workDir)
	require.NoError(t, err)
	cid := id.MustCheckpointID("01M2DCHJCHTR9T9MZTSB7WV76B")
	canonical := mustRefName(t, cid)
	folded, ok := checkpoint.FoldedRefName(cid)
	require.True(t, ok)
	head, err := repo.Head()
	require.NoError(t, err)
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(folded, head.Hash())))
	// Packing removes the filesystem's case-insensitive rescue, so the canonical
	// name stops resolving on a case-insensitive platform too.
	testutil.RunGit(t, workDir, "pack-refs", "--all")

	repo, err = git.PlainOpen(workDir)
	require.NoError(t, err)
	_ = enqueueRefs(t, repo, []plumbing.ReferenceName{canonical})

	ctx := context.Background()
	exists, err := CheckpointRefExists(ctx, cid)
	require.NoError(t, err)
	assert.True(t, exists, "a checkpoint stored under its folded shard still exists")

	waiting, err := CheckpointWaiting(ctx, cid)
	require.NoError(t, err)
	assert.True(t, waiting, "its queue entry is still waiting to be pushed")
}
