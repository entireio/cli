package strategy

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
)

// TestSaveStep_ConcurrentSessionsSameShadowBranch reproduces the parallel-agents
// scenario behind the Stop-hook error
//
//	failed to write temporary checkpoint: failed to build tree:
//	  failed to apply changes in .entire: failed to read tree: object not found
//
// Multiple sessions in the same worktree on the same base commit all hash to the
// same shadow branch name. SaveStep is serialized per-session-ID via
// acquireSessionGate but there is no shadow-branch-wide lock, so the ref update
// at the end of WriteTemporary races.
//
// Each goroutine writes to a unique agent-XX.txt file with unique content per
// step, so every checkpoint produces a distinct tree hash — i.e. the dedup
// short-circuit in WriteTemporary never fires. That invariant is what lets us
// assert per-session StepCount == checkpointsPerWorker below; if a future
// change ever lands two identical checkpoints in a row, the StepCount
// assertion (not just the commit-count check) will catch it.
//
// Assertions:
//   - no SaveStep returns an error
//   - every session's persisted StepCount equals checkpointsPerWorker (no
//     checkpoint was skipped or lost)
//   - the resulting shadow branch is internally consistent: every commit
//     reachable from the ref has a tree where every directory entry resolves
//     (no "object not found" anywhere in the chain)
//   - the shadow branch commit count equals numSessions * checkpointsPerWorker
func TestSaveStep_ConcurrentSessionsSameShadowBranch(t *testing.T) {
	const (
		numSessions          = 8
		checkpointsPerWorker = 4
	)

	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "seed.txt", "seed\n")
	testutil.GitAdd(t, dir, "seed.txt")
	testutil.GitCommit(t, dir, "initial commit")

	t.Chdir(dir)
	paths.ClearWorktreeRootCache()

	type session struct {
		id             string
		metadataDir    string
		metadataDirAbs string
		file           string
	}
	sessions := make([]session, numSessions)
	for i := range sessions {
		id := fmt.Sprintf("2026-05-14-concurrent-%02d", i)
		md := paths.EntireMetadataDir + "/" + id
		sessions[i] = session{
			id:             id,
			metadataDir:    md,
			metadataDirAbs: filepath.Join(dir, md),
			file:           fmt.Sprintf("agent-%02d.txt", i),
		}
		testutil.WriteFile(t, dir, md+"/"+paths.TranscriptFileName,
			"{\"type\":\"human\",\"message\":{\"content\":\"start\"}}\n")
	}

	type goroutineErr struct {
		session string
		step    int
		err     error
	}
	errCh := make(chan goroutineErr, numSessions*(checkpointsPerWorker+1))
	start := make(chan struct{})

	var wg sync.WaitGroup
	for i := range sessions {
		sess := sessions[i]
		wg.Go(func() {
			ctx := context.Background()
			// Each goroutine owns its own strategy + repo handle, mirroring the
			// production case where every hook invocation is a fresh process.
			s := NewManualCommitStrategy()

			if err := s.InitializeSession(ctx, sess.id, "Claude Code", "", "", ""); err != nil {
				errCh <- goroutineErr{session: sess.id, step: -1, err: fmt.Errorf("InitializeSession: %w", err)}
				return
			}

			// Wait for all goroutines to be ready, then start together to widen
			// the race window for the SetReference contention.
			<-start

			for step := range checkpointsPerWorker {
				content := fmt.Sprintf("session=%s step=%d\n", sess.id, step)
				if err := writeFileForRaceTest(filepath.Join(dir, sess.file), content); err != nil {
					errCh <- goroutineErr{session: sess.id, step: step, err: fmt.Errorf("write worker file: %w", err)}
					return
				}
				transcriptLine := fmt.Sprintf("{\"type\":\"assistant\",\"step\":%d}\n", step)
				transcriptPath := filepath.Join(sess.metadataDirAbs, paths.TranscriptFileName)
				if err := writeFileForRaceTest(transcriptPath, transcriptLine); err != nil {
					errCh <- goroutineErr{session: sess.id, step: step, err: fmt.Errorf("write transcript: %w", err)}
					return
				}

				var modified, newFiles []string
				if step == 0 {
					newFiles = []string{sess.file}
				} else {
					modified = []string{sess.file}
				}

				err := s.SaveStep(ctx, StepContext{
					SessionID:      sess.id,
					ModifiedFiles:  modified,
					NewFiles:       newFiles,
					MetadataDir:    sess.metadataDir,
					MetadataDirAbs: sess.metadataDirAbs,
					CommitMessage:  fmt.Sprintf("Checkpoint %d for %s", step, sess.id),
					AuthorName:     "Test",
					AuthorEmail:    "test@example.com",
				})
				if err != nil {
					errCh <- goroutineErr{session: sess.id, step: step, err: fmt.Errorf("SaveStep: %w", err)}
					return
				}
			}
		})
	}

	close(start)
	wg.Wait()
	close(errCh)

	for ge := range errCh {
		t.Errorf("session %s step %d: %v", ge.session, ge.step, ge.err)
	}
	if t.Failed() {
		return
	}

	// Per-session invariant: every SaveStep call should have landed a
	// checkpoint (no skips from the dedup short-circuit). StepCount is
	// incremented in SaveStep only when WriteTemporary returns Skipped=false,
	// so this catches a future test change that accidentally writes
	// duplicate-content checkpoints — which would surface as a misleading
	// "commits were lost" message in the commit-count check below.
	stateStrategy := NewManualCommitStrategy()
	for _, sess := range sessions {
		state, err := stateStrategy.loadSessionState(context.Background(), sess.id)
		if err != nil {
			t.Errorf("load state for %s: %v", sess.id, err)
			continue
		}
		if state == nil {
			t.Errorf("missing state for %s", sess.id)
			continue
		}
		if state.StepCount != checkpointsPerWorker {
			t.Errorf("session %s StepCount = %d, want %d", sess.id, state.StepCount, checkpointsPerWorker)
		}
	}

	// Verify the shadow branch is internally consistent.
	repo, err := git.PlainOpen(dir)
	if err != nil {
		t.Fatalf("open repo: %v", err)
	}

	shadowBranches := listShadowBranches(t, repo)
	if len(shadowBranches) == 0 {
		t.Fatal("expected at least one shadow branch after SaveStep, found none")
	}
	if len(shadowBranches) > 1 {
		names := make([]string, 0, len(shadowBranches))
		for _, ref := range shadowBranches {
			names = append(names, ref.Name().Short())
		}
		t.Fatalf("expected sessions to share a single shadow branch, got %d: %v", len(shadowBranches), names)
	}

	commits := walkShadowBranchAssertConsistent(t, repo, shadowBranches[0])

	// Commit-count check: every distinct checkpoint we issued should have
	// landed on the shadow branch. See the test-level comment for why dedup
	// can't quietly defeat this assertion.
	expected := numSessions * checkpointsPerWorker
	switch {
	case commits > expected:
		t.Errorf("walked %d commits, more than the %d SaveStep calls — accounting bug", commits, expected)
	case commits < expected:
		t.Errorf("walked %d commits but issued %d SaveStep calls — %d commits were lost",
			commits, expected, expected-commits)
	default:
		t.Logf("walked %d commits matching %d SaveStep calls — no checkpoints lost", commits, expected)
	}
}

func listShadowBranches(t *testing.T, repo *git.Repository) []plumbing.Reference {
	t.Helper()
	refs, err := repo.References()
	if err != nil {
		t.Fatalf("list refs: %v", err)
	}
	var out []plumbing.Reference
	err = refs.ForEach(func(ref *plumbing.Reference) error {
		if ref.Name().IsBranch() && strings.HasPrefix(ref.Name().Short(), checkpoint.ShadowBranchPrefix) &&
			ref.Name().Short() != paths.MetadataBranchName {
			out = append(out, *ref)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("iterate refs: %v", err)
	}
	return out
}

// walkShadowBranchAssertConsistent walks every commit reachable from the shadow
// branch ref and asserts every tree (and recursively every subtree) is in the
// object database. Returns the number of commits visited.
func walkShadowBranchAssertConsistent(t *testing.T, repo *git.Repository, ref plumbing.Reference) int {
	t.Helper()
	visited := make(map[plumbing.Hash]bool)
	count := 0
	hash := ref.Hash()
	for hash != plumbing.ZeroHash {
		if visited[hash] {
			t.Fatalf("shadow branch %s: cycle at commit %s", ref.Name().Short(), hash)
		}
		visited[hash] = true
		count++

		commit, err := repo.CommitObject(hash)
		if err != nil {
			t.Fatalf("shadow branch %s: commit %s unreadable: %v", ref.Name().Short(), hash, err)
		}
		walkTreeAssertConsistent(t, repo, commit.TreeHash, "/")

		if len(commit.ParentHashes) == 0 {
			break
		}
		hash = commit.ParentHashes[0]
	}
	return count
}

func walkTreeAssertConsistent(t *testing.T, repo *git.Repository, hash plumbing.Hash, path string) {
	t.Helper()
	tree, err := repo.TreeObject(hash)
	if err != nil {
		t.Fatalf("tree %s at %s unreadable: %v", hash, path, err)
	}
	for _, entry := range tree.Entries {
		if entry.Mode == filemode.Dir {
			walkTreeAssertConsistent(t, repo, entry.Hash, path+entry.Name+"/")
		}
	}
}

// writeFileForRaceTest is a goroutine-safe alternative to testutil.WriteFile,
// scoped to this test file. testutil.WriteFile calls t.Fatalf, which doesn't
// fail the test cleanly from a sub-goroutine. Name kept long and specific so
// it can't accidentally shadow a more general helper added to the package
// later.
func writeFileForRaceTest(absPath, content string) error {
	if err := os.MkdirAll(filepath.Dir(absPath), 0o755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	return os.WriteFile(absPath, []byte(content), 0o644)
}
